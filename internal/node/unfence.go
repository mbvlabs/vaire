package node

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
)

// UnfenceResult is what vaire node unfence did.
type UnfenceResult struct {
	WasFenced bool   `json:"was_fenced"`
	DryRun    bool   `json:"dry_run,omitempty"`
	Bucket    string `json:"bucket_epoch,omitempty"`
	Reclaimed bool   `json:"reclaimed,omitempty"`
	Epoch     int64  `json:"epoch,omitempty"`
}

// ReasonReclaim is the epoch reason of vaire node unfence --reclaim.
const ReasonReclaim = "reclaim"

// Unfence lifts the fence on this node (Phase 5): run on the node as root,
// over break-glass SSH, since a fenced node refuses mutating RPCs. With a
// bucket it reads the epoch first. When the epoch still names another
// instance (a rebuild elsewhere), it refuses unless reclaim is set: reclaim
// writes a newer epoch naming this node, which fences that other instance,
// so it is only for a rebuild that was abandoned and whose VPS is gone. It
// then removes the fence marker; vaire apply brings the timers and poolers
// back.
func Unfence(ctx context.Context, confirm string, reclaim, dryRun bool) (UnfenceResult, error) {
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return UnfenceResult{}, err
	}
	token := cfg.Name() + "/unfence"
	if confirm != token {
		return UnfenceResult{}, output.NewError(output.CodeUnsafeAction, "lifting the fence needs --confirm "+token, output.ExitUnsafe,
			"Only when the rebuild that fenced this server was abandoned or was a mistake. Destroy a replaced server instead.").
			WithDetails(map[string]any{"confirm": token})
	}
	_, fenced := masterbackup.ReadFenced("")
	res := UnfenceResult{WasFenced: fenced, DryRun: dryRun}
	if !fenced {
		return res, nil
	}
	if !dryRun && os.Geteuid() != 0 {
		return res, rootRequired("vaire node unfence")
	}
	if cfg.BackupConfigured() {
		if err := unfenceBucket(ctx, cfg, reclaim, dryRun, &res); err != nil {
			return res, err
		}
	}
	if dryRun {
		return res, nil
	}
	if err := os.Remove(masterbackup.FencedPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	writeMasterMetrics(cfg)
	return res, nil
}

// unfenceBucket checks (and with reclaim takes back) the bucket's epoch.
func unfenceBucket(ctx context.Context, cfg config.Cluster, reclaim, dryRun bool, res *UnfenceResult) error {
	t, _, err := steps.BackupTarget(cfg)
	if err != nil {
		return err
	}
	instance, err := masterbackup.ReadInstance("")
	if err != nil || instance == "" {
		return output.NewError(output.CodePrecondition, "this node has no instance id", output.ExitPrecondition,
			"A node without "+masterbackup.InstancePath+" can't be fenced; remove "+masterbackup.FencedPath+" by hand.")
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cur, err := backup.ReadEpoch(ctx, t)
	if err != nil {
		return err
	}
	if cur == nil || cur.InstanceID == instance {
		if cur != nil {
			res.Bucket, res.Epoch = fmt.Sprintf("epoch %d names this node", cur.Epoch), cur.Epoch
			if !dryRun {
				return saveLocalEpoch(*cur)
			}
		}
		return nil
	}
	res.Bucket, res.Epoch = fmt.Sprintf("epoch %d names instance %s", cur.Epoch, cur.InstanceID), cur.Epoch
	if !reclaim {
		return output.NewError(output.CodePrecondition,
			fmt.Sprintf("the bucket's epoch %d still names instance %s, not this node", cur.Epoch, cur.InstanceID), output.ExitPrecondition,
			"A rebuild took over this server. Destroy this one. Only if that rebuild was abandoned and its VPS is gone, pass --reclaim: it writes a newer epoch naming this node and fences the other.").
			WithDetails(map[string]any{"reason": "node_fenced", "epoch": cur.Epoch, "instance_id": cur.InstanceID})
	}
	e := masterbackup.NewEpoch(cfg.Name(), cur.Epoch+1, instance, "node", ReasonReclaim)
	res.Reclaimed, res.Epoch = true, e.Epoch
	if dryRun {
		return nil
	}
	if err := backup.WriteEpoch(ctx, t, e); err != nil {
		return err
	}
	back, err := backup.ReadEpoch(ctx, t)
	if err != nil {
		return err
	}
	if back == nil || back.InstanceID != instance {
		return epochConflict()
	}
	return saveLocalEpoch(e)
}
