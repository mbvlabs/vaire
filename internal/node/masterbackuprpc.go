package node

import (
	"context"
	"encoding/base64"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

// master-backup.now (Phase 5, unit 6): build and upload a bundle now.
type masterBackupNowParams struct {
	Force bool `json:"force,omitempty"`
}

func handleMasterBackupNow(ctx context.Context, env *Env) (Result, error) {
	var p masterBackupNowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		st := readMasterStatus(env.Cluster, true)
		return Result{Data: map[string]any{
			"dry_run": true, "bucket": env.Cluster.BackupConfigured(),
			"changed": st.ContentHash != st.Last.ContentHash, "last": st.Last.Name,
		}}, nil
	}
	// The operator asked: check the bucket rather than trust last.json.
	res, err := runMasterBackup(ctx, env.Cluster, env.Report, masterBackupOptions{force: p.Force, verify: true})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: res}, nil
}

// master-backup.list: the local bundles, last.json, the epoch and the live
// content hash.
func handleMasterBackupList(_ context.Context, env *Env) (Result, error) {
	return Result{Data: readMasterStatus(env.Cluster, true)}, nil
}

// master-backup.get: one local bundle, still encrypted, for server backup
// export and verify. It records the export.
type masterBackupGetParams struct {
	Name   string `json:"name,omitempty"`
	Export bool   `json:"export,omitempty"`
}

// MasterBundleData is master-backup.get's result.
type MasterBundleData struct {
	Name    string               `json:"name"`
	Bundle  string               `json:"bundle"`
	Sidecar masterbackup.Sidecar `json:"sidecar"`
}

func handleMasterBackupGet(_ context.Context, env *Env) (Result, error) {
	var p masterBackupGetParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	name := trimName(p.Name)
	if name == "" {
		local := localBundles()
		if len(local) == 0 {
			return Result{}, output.NewError(output.CodeBundleNotFound, "the server has no master bundle yet", output.ExitPrecondition,
				"Run vaire server backup now.")
		}
		name = local[0].Name
	}
	raw, side, err := readLocalBundle(name)
	if err != nil {
		return Result{}, err
	}
	if p.Export {
		_ = withMasterLast(func(l *MasterLast) { l.LastExportedAt = time.Now().UTC().Format(time.RFC3339) })
	}
	return Result{Data: MasterBundleData{Name: name, Bundle: base64.StdEncoding.EncodeToString(raw), Sidecar: side}}, nil
}

// withMasterLast updates last.json under the master backup's lock.
func withMasterLast(fn func(*MasterLast)) error {
	return withMasterLock(func() error {
		l := loadMasterLast()
		fn(&l)
		return saveMasterLast(l)
	})
}
