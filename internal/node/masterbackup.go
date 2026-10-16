package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/doctor"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/promfile"
	"github.com/mbvlabs/eru/internal/steps"
)

// The master backup job (Phase 5, D3): build the bundle when a bundled file
// changed, keep it locally, upload it with its sidecar when there is a
// bucket, prune, and write eru-master-backup.prom.

const (
	// masterBackupLockPath keeps the path unit's run, the daily run and
	// master-backup.now apart. It isn't the node lock: a long apply must
	// never delay a bundle.
	masterBackupLockPath = "/run/lock/eru-master-backup.lock"
	// debounceQuiet is how long the dirty flag must be untouched before the
	// job snapshots, so a burst of changes makes one bundle.
	debounceQuiet = 5 * time.Second
	// debounceMax bounds the wait for a quiet moment.
	debounceMax = 60 * time.Second
	// newestKept is how many bundles retention always keeps (D4).
	newestKept = 3
	// verifyEvery is how often a run checks that the bucket's newest bundle
	// is this node's (the daily safety net).
	verifyEvery = 20 * time.Hour
	// pruneEvery is how often the bucket is pruned.
	pruneEvery = 24 * time.Hour
	// incompleteAge is how old a bundle without a sidecar (or a sidecar
	// without a bundle) must be before pruning removes it.
	incompleteAge = 24 * time.Hour
	// promMasterBackup is the master backup's textfile.
	promMasterBackup = "eru-master-backup.prom"
)

// MasterLast is /var/lib/eru/master-backup/last.json: the last bundle and
// how its upload went.
type MasterLast struct {
	Name           string `json:"name,omitempty"`
	ContentHash    string `json:"content_hash,omitempty"`
	CreatedAt      string `json:"created_at,omitempty"`
	Size           int64  `json:"size,omitempty"`
	Epoch          int64  `json:"epoch,omitempty"`
	UploadedName   string `json:"uploaded_name,omitempty"`
	UploadedAt     string `json:"uploaded_at,omitempty"`
	LastFailureAt  string `json:"last_failure_at,omitempty"`
	LastFailure    string `json:"last_failure,omitempty"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
	LastPrunedAt   string `json:"last_pruned_at,omitempty"`
	LastExportedAt string `json:"last_exported_at,omitempty"`
	// PendingSince is when a change was first left unbundled or unuploaded,
	// cleared once a bundle reaches its destination.
	PendingSince string `json:"pending_since,omitempty"`
}

func loadMasterLast() MasterLast {
	var l MasterLast
	if raw, err := os.ReadFile(masterbackup.LastPath); err == nil {
		_ = json.Unmarshal(raw, &l)
	}
	return l
}

func saveMasterLast(l MasterLast) error {
	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(masterbackup.LastPath, append(raw, '\n'), 0o600)
}

// MasterBackupResult is what a run did.
type MasterBackupResult struct {
	Name     string `json:"name,omitempty"`
	Built    bool   `json:"built"`
	Uploaded bool   `json:"uploaded"`
	Skipped  string `json:"skipped,omitempty"`
	Epoch    int64  `json:"epoch"`
	Size     int64  `json:"size,omitempty"`
	Bucket   bool   `json:"bucket"`
	Pruned   int    `json:"pruned,omitempty"`
	Verified bool   `json:"verified,omitempty"`
}

// masterBackupOptions tune a run: debounce waits for a quiet dirty flag (the
// path unit's run), force builds even when nothing changed, verify checks
// the bucket for the newest bundle however recently it last did
// (master-backup.now).
type masterBackupOptions struct {
	debounce bool
	force    bool
	verify   bool
}

// runMasterBackup runs the job under its own lock.
func runMasterBackup(ctx context.Context, cfg config.Cluster, report engine.Reporter, opts masterBackupOptions) (res MasterBackupResult, err error) {
	if _, statErr := os.Stat(masterbackup.Dir); statErr != nil {
		return res, output.NewError(output.CodePrecondition, masterbackup.Dir+" does not exist", output.ExitPrecondition,
			"Run eru apply: master-backup-v1 prepares the node for the master backup.").WithDetails(map[string]any{"reason": "node_outdated"})
	}
	err = withMasterLock(func() error {
		var runErr error
		res, runErr = masterBackupLocked(ctx, cfg, report, opts)
		return runErr
	})
	writeMasterMetrics(cfg)
	return res, err
}

// withMasterLock runs fn under the master backup's own lock.
func withMasterLock(fn func() error) error { return engine.WithFileLock(masterBackupLockPath, fn) }

func masterBackupLocked(ctx context.Context, cfg config.Cluster, report engine.Reporter, opts masterBackupOptions) (MasterBackupResult, error) {
	res := MasterBackupResult{Bucket: cfg.BackupConfigured()}
	if masterbackup.IsFenced() {
		return res, fencedError()
	}
	if opts.debounce {
		waitQuiet(ctx)
	}
	if err := masterbackup.ClearDirty(""); err != nil {
		return res, err
	}
	last := loadMasterLast()
	now := time.Now().UTC()
	files, err := masterbackup.Snapshot("")
	if err != nil {
		return res, recordMasterFailure(last, now, err)
	}
	hash := masterbackup.ContentHash(files)
	// With a bucket, the fence and the first epoch come before the build,
	// so the bundle is named for the epoch it belongs to: the first one of a
	// fresh server is -e1-, not -e0-. A bucket that can't be reached still
	// gets a local bundle; the failure is recorded once it is built.
	var target backup.Target
	var bucketErr error
	if cfg.BackupConfigured() {
		target, bucketErr = settleEpoch(ctx, cfg, report)
		if masterbackup.IsFenced() {
			return res, recordMasterFailure(last, now, fencedError())
		}
	}
	local, _ := masterbackup.ReadLocalEpoch("")
	res.Epoch = local.Epoch
	if needsBuild(last, hash, local.Epoch, localBundleExists(last.Name)) || opts.force {
		recipient := steps.ReadRecipient()
		if recipient == "" {
			return res, recordMasterFailure(last, now, output.NewError(output.CodeInputRequired,
				"the recovery recipient is not on this node", output.ExitInput,
				"Run eru apply: master-backup-v1 writes "+masterbackup.RecipientPath+" from the recovery kit.").
				WithDetails(map[string]any{"path": masterbackup.RecipientPath}))
		}
		name, size, err := buildLocalBundle(cfg, files, local.Epoch, recipient, now)
		if err != nil {
			return res, recordMasterFailure(last, now, err)
		}
		logLine(report, "master-backup", "built "+name)
		last.Name, last.ContentHash, last.CreatedAt, last.Size, last.Epoch = name, hash, now.Format(time.RFC3339), size, local.Epoch
		if last.PendingSince == "" {
			last.PendingSince = now.Format(time.RFC3339)
		}
		res.Built = true
		pruneLocalBundles(cfg.MasterBackup.LocalKeep)
	} else {
		res.Skipped = "unchanged"
	}
	res.Name, res.Size = last.Name, last.Size
	if !cfg.BackupConfigured() {
		last.PendingSince = ""
		last.LastFailure, last.LastFailureAt = "", ""
		return res, saveMasterLast(last)
	}
	if bucketErr != nil {
		return res, recordMasterFailure(last, now, bucketErr)
	}
	uploaded, verified, pruned, err := syncBucket(ctx, cfg, target, report, &last, now, opts.verify)
	res.Uploaded, res.Verified, res.Pruned = uploaded, verified, pruned
	if err != nil {
		return res, recordMasterFailure(last, now, err)
	}
	last.PendingSince = ""
	last.LastFailure, last.LastFailureAt = "", ""
	return res, saveMasterLast(last)
}

// needsBuild reports whether a run builds a new bundle: the content changed,
// there is none yet, the last one is gone from the node, or it was built
// under another epoch than the node's current one (a bucket's first epoch
// written since, or a reclaim), so every bundle's name carries the epoch it
// belongs to.
func needsBuild(last MasterLast, hash string, epoch int64, exists bool) bool {
	return hash != last.ContentHash || last.Name == "" || !exists || last.Epoch != epoch
}

// waitQuiet waits until the dirty flag has been untouched for debounceQuiet,
// at most debounceMax.
func waitQuiet(ctx context.Context) {
	deadline := time.Now().Add(debounceMax)
	for time.Now().Before(deadline) {
		dirty, touched := masterbackup.Dirty("")
		if !dirty || masterbackup.Settled(touched, time.Now(), debounceQuiet) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func recordMasterFailure(last MasterLast, now time.Time, cause error) error {
	last.LastFailureAt, last.LastFailure = now.Format(time.RFC3339), cause.Error()
	if last.PendingSince == "" {
		last.PendingSince = now.Format(time.RFC3339)
	}
	_ = saveMasterLast(last)
	return cause
}

func localBundleExists(name string) bool {
	_, err := os.Stat(filepath.Join(masterbackup.Dir, name+masterbackup.BundleSuffix))
	return err == nil
}

// buildLocalBundle builds, seals and writes a bundle and its sidecar to the
// local directory.
func buildLocalBundle(cfg config.Cluster, files []masterbackup.File, epoch int64, recipient string, now time.Time) (string, int64, error) {
	nodeName := ""
	if n, err := config.LoadNode(config.NodePath); err == nil {
		nodeName = n.NodeName
	}
	meta := masterbackup.Meta{Server: cfg.Name(), Node: nodeName, Epoch: epoch, CreatedAt: now, EruVersion: buildinfo.Version}
	tarball, manifest, err := masterbackup.Build(meta, files)
	if err != nil {
		return "", 0, err
	}
	sealed, err := masterbackup.Seal(tarball, recipient)
	if err != nil {
		return "", 0, err
	}
	name := masterbackup.Name(now, epoch, manifest.Revision)
	side := masterbackup.NewSidecar(name, manifest, sealed)
	sideRaw, err := json.MarshalIndent(side, "", "  ")
	if err != nil {
		return "", 0, err
	}
	if err := engine.WriteFileAtomic(filepath.Join(masterbackup.Dir, name+masterbackup.BundleSuffix), sealed, 0o600); err != nil {
		return "", 0, err
	}
	if err := engine.WriteFileAtomic(filepath.Join(masterbackup.Dir, name+masterbackup.SidecarSuffix), append(sideRaw, '\n'), 0o600); err != nil {
		return "", 0, err
	}
	return name, int64(len(sealed)), nil
}

// LocalBundle is one bundle kept on the node.
type LocalBundle struct {
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Epoch     int64  `json:"epoch"`
	Size      int64  `json:"size"`
}

// localBundles lists the bundles in the local directory, newest first.
func localBundles() []LocalBundle {
	entries, _ := os.ReadDir(masterbackup.Dir)
	var out []LocalBundle
	for _, e := range entries {
		name, isBundle, ok := masterbackup.SplitKey(e.Name())
		if !ok || !isBundle {
			continue
		}
		p, _ := masterbackup.ParseName(name)
		var size int64
		if info, err := e.Info(); err == nil {
			size = info.Size()
		}
		out = append(out, LocalBundle{Name: name, CreatedAt: p.CreatedAt.Format(time.RFC3339), Epoch: p.Epoch, Size: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out
}

// pruneLocalBundles keeps the newest keep bundles.
func pruneLocalBundles(keep int) {
	if keep < 1 {
		keep = 1
	}
	for i, b := range localBundles() {
		if i < keep {
			continue
		}
		_ = os.Remove(filepath.Join(masterbackup.Dir, b.Name+masterbackup.BundleSuffix))
		_ = os.Remove(filepath.Join(masterbackup.Dir, b.Name+masterbackup.SidecarSuffix))
	}
}

// readLocalBundle reads a local bundle and its sidecar.
func readLocalBundle(name string) ([]byte, masterbackup.Sidecar, error) {
	var side masterbackup.Sidecar
	if _, ok := masterbackup.ParseName(name); !ok {
		return nil, side, output.NewError(output.CodeUsage, name+" is not a bundle name", output.ExitUsage, "Use a name from eru server backup list.")
	}
	raw, err := os.ReadFile(filepath.Join(masterbackup.Dir, name+masterbackup.BundleSuffix)) //nolint:gosec // G304: a parsed bundle name in the fixed directory.
	if errors.Is(err, fs.ErrNotExist) {
		return nil, side, output.NewError(output.CodeBundleNotFound, "bundle "+name+" is not on the server", output.ExitPrecondition,
			"List the bundles with eru server backup list.").WithDetails(map[string]any{"bundle": name})
	}
	if err != nil {
		return nil, side, err
	}
	if sideRaw, err := os.ReadFile(filepath.Join(masterbackup.Dir, name+masterbackup.SidecarSuffix)); err == nil { //nolint:gosec // G304: as above.
		_ = json.Unmarshal(sideRaw, &side)
	}
	return raw, side, nil
}

// settleEpoch checks the fence and writes the first epoch when the bucket
// has none, so the node's local epoch is the bucket's before a bundle is
// named. It returns the bucket target for the upload.
func settleEpoch(ctx context.Context, cfg config.Cluster, report engine.Reporter) (backup.Target, error) {
	t, _, err := steps.BackupTarget(cfg)
	if err != nil {
		return t, err
	}
	state, bucketEpoch, err := checkFence(ctx, cfg, t, report)
	if err != nil {
		return t, err
	}
	switch {
	case state == masterbackup.FenceUnknown && bucketEpoch != nil:
		// An epoch that isn't ours but no proof of a newer master either:
		// don't upload or prune beside whoever wrote it.
		return t, epochConflict()
	case state == masterbackup.FenceAbsent:
		if err := writeFirstEpoch(ctx, cfg, t); err != nil {
			return t, err
		}
		logLine(report, "master-backup", "wrote epoch 1 to the bucket")
	case state == masterbackup.FenceOK && bucketEpoch != nil:
		_ = saveLocalEpoch(*bucketEpoch)
	}
	return t, nil
}

// syncBucket uploads the newest local bundle when the bucket lacks it,
// confirms it once a day (every run with verify) and prunes. settleEpoch
// has checked the fence.
func syncBucket(ctx context.Context, cfg config.Cluster, t backup.Target, report engine.Reporter, last *MasterLast, now time.Time, verify bool) (uploaded, verified bool, pruned int, err error) {
	if last.Name == "" {
		return false, false, 0, nil
	}
	needUpload := last.UploadedName != last.Name
	if bucketCheckDue(*last, now, verify) {
		ok, err := backup.MasterHasSidecar(ctx, t, last.Name)
		if err != nil {
			return false, false, 0, err
		}
		needUpload = !ok
		verified = ok
	}
	if needUpload {
		sealed, side, err := readLocalBundle(last.Name)
		if err != nil {
			return false, false, 0, err
		}
		if err := backup.MasterPut(ctx, t, last.Name, sealed, side); err != nil {
			return false, false, 0, err
		}
		last.UploadedName, last.UploadedAt = last.Name, now.Format(time.RFC3339)
		uploaded, verified = true, true
		logLine(report, "master-backup", "uploaded "+last.Name)
	}
	if verified {
		last.LastVerifiedAt = now.Format(time.RFC3339)
	}
	if sinceRFC3339(last.LastPrunedAt, now) >= pruneEvery {
		n, err := pruneBucket(ctx, cfg, t, now)
		if err != nil {
			logLine(report, "master-backup", "pruning the bucket failed: "+err.Error())
		} else {
			pruned = n
			last.LastPrunedAt = now.Format(time.RFC3339)
		}
	}
	return uploaded, verified, pruned, nil
}

// bucketCheckDue reports whether a run checks the bucket for the bundle
// last.json says was uploaded: daily, or on every run with verify, since
// the bucket can lose it without the node knowing.
func bucketCheckDue(last MasterLast, now time.Time, verify bool) bool {
	if last.UploadedName != last.Name {
		return false // it's uploaded anyway
	}
	return verify || sinceRFC3339(last.LastVerifiedAt, now) >= verifyEvery
}

// sinceRFC3339 is how long ago ts was; forever when it doesn't parse.
func sinceRFC3339(ts string, now time.Time) time.Duration {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 1<<63 - 1
	}
	return now.Sub(t)
}

// pruneBucket applies the retention (D4) to the bucket's bundles and removes
// incomplete uploads older than a day.
func pruneBucket(ctx context.Context, cfg config.Cluster, t backup.Target, now time.Time) (int, error) {
	list, err := backup.MasterList(ctx, t)
	if err != nil {
		return 0, err
	}
	return pruneBundles(ctx, list, now, cfg.MasterBackup, func(name string) error { return backup.MasterDelete(ctx, t, name) })
}

// pruneBundles deletes what retention doesn't keep, through del.
func pruneBundles(_ context.Context, list []backup.MasterBundle, now time.Time, mb config.MasterBackup, del func(string) error) (int, error) {
	var complete []string
	for _, b := range list {
		if b.Complete {
			complete = append(complete, b.Name)
		}
	}
	keep := masterbackup.Keep(complete, now, mb.RetentionDays, mb.RetentionWeeks, newestKept)
	n := 0
	for _, b := range list {
		drop := false
		if b.Complete {
			drop = !keep[b.Name]
		} else if p, ok := masterbackup.ParseName(b.Name); ok && now.Sub(p.CreatedAt) > incompleteAge {
			drop = true
		}
		if !drop {
			continue
		}
		if err := del(b.Name); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// --- epoch and fence (D5) ---

func saveLocalEpoch(e masterbackup.Epoch) error {
	raw, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(masterbackup.Dir, 0o700); err != nil {
		return err
	}
	return engine.WriteFileAtomic(masterbackup.EpochPath, append(raw, '\n'), 0o600)
}

// writeFirstEpoch writes epoch 1 for this instance, only while the bucket
// has none: read, write, read back.
func writeFirstEpoch(ctx context.Context, cfg config.Cluster, t backup.Target) error {
	instance, err := masterbackup.ReadInstance("")
	if err != nil || instance == "" {
		return output.NewError(output.CodePrecondition, "this node has no instance id", output.ExitPrecondition,
			"Run eru apply: master-backup-v1 makes one.").WithDetails(map[string]any{"reason": "node_outdated"})
	}
	if existing, err := backup.ReadEpoch(ctx, t); err != nil {
		return err
	} else if existing != nil {
		// Written since checkFence read it: decide again on what's there.
		switch masterbackup.Decide(existing, nil, fenceLocal(cfg)) {
		case masterbackup.FenceOK:
			return saveLocalEpoch(*existing)
		case masterbackup.FenceFenced:
			return applyFence(ctx, *existing)
		default:
			return epochConflict()
		}
	}
	e := masterbackup.NewEpoch(cfg.Name(), 1, instance, "node", masterbackup.ReasonFirst)
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

// epochConflict is the bucket's epoch naming another writer that isn't
// proof of a newer master: the node doesn't fence itself, it stops.
func epochConflict() error {
	return output.NewError(output.CodePrecondition, "the bucket's master epoch names another instance", output.ExitPrecondition,
		"Check that no other server uses this bucket and prefix.").WithDetails(map[string]any{"reason": "epoch_conflict"})
}

// checkFence reads the bucket's epoch and fences this node when it names
// another instance. Unknown (the bucket can't be read) never fences.
func checkFence(ctx context.Context, cfg config.Cluster, t backup.Target, report engine.Reporter) (masterbackup.FenceState, *masterbackup.Epoch, error) {
	e, readErr := backup.ReadEpoch(ctx, t)
	state := masterbackup.Decide(e, readErr, fenceLocal(cfg))
	switch state {
	case masterbackup.FenceFenced:
		logLine(report, "fence", fmt.Sprintf("the bucket's epoch %d names instance %s, not this one: fencing this node", e.Epoch, e.InstanceID))
		return state, e, applyFence(ctx, *e)
	case masterbackup.FenceUnknown:
		if readErr != nil {
			logLine(report, "fence", "the bucket's epoch can't be read: "+readErr.Error())
		} else if e != nil {
			logLine(report, "fence", fmt.Sprintf("the bucket's epoch (server %q, epoch %d, instance %q) is no proof of a newer master: not fencing", e.Server, e.Epoch, e.InstanceID))
		}
	}
	return state, e, nil
}

// fenceLocal is this node as the fence decision sees it.
func fenceLocal(cfg config.Cluster) masterbackup.Local {
	instance, _ := masterbackup.ReadInstance("")
	local, _ := masterbackup.ReadLocalEpoch("")
	return masterbackup.Local{Server: cfg.Name(), Instance: instance, Epoch: local.Epoch}
}

// fencedError is node_fenced.
func fencedError() error {
	f, _ := masterbackup.ReadFenced("")
	return output.NewError(output.CodeNodeFenced,
		"this node is fenced: a newer master took over its bucket", output.ExitPrecondition,
		"This server was replaced by eru server init --restore. Destroy it; nothing here writes to the bucket any more.").
		WithDetails(map[string]any{"reason": "node_fenced", "epoch": f.Epoch, "instance_id": f.InstanceID, "seen_at": f.SeenAt})
}

// fenceUnits are what a fenced node stops: the poolers, so clients that
// still reach the old address get "connection refused".
var fenceUnits = []string{"pgbouncer.service", steps.SessionPoolerService + ".service", steps.MasterBackupPath}

// applyFence records the fence, stops and disables every eru timer but the
// fence check, and stops the poolers. Clusters keep running on their sockets
// for break-glass inspection; archive-push refuses (unit 5's wrapper).
func applyFence(ctx context.Context, e masterbackup.Epoch) error {
	if _, already := masterbackup.ReadFenced(""); !already {
		raw, _ := json.Marshal(masterbackup.Fenced{Epoch: e.Epoch, InstanceID: e.InstanceID, SeenAt: time.Now().UTC().Format(time.RFC3339)})
		if err := engine.WriteFileAtomic(masterbackup.FencedPath, append(raw, '\n'), 0o644); err != nil {
			return err
		}
	}
	timers, _ := filepath.Glob(filepath.Join(steps.SystemdSystemDir, "eru-*.timer"))
	var units []string
	for _, p := range timers {
		if name := filepath.Base(p); name != steps.FenceTimer {
			units = append(units, name)
		}
	}
	units = append(units, fenceUnits...)
	for _, u := range units {
		_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", u).Run() //nolint:gosec // G204: fixed unit names from the systemd directory.
	}
	return fencedError()
}

// requireNotFenced is the check every job that writes to or deletes from the
// bucket runs first (D5): a node already fenced refuses at once; otherwise
// the bucket's epoch is read, and a newer instance fences the node.
func requireNotFenced(ctx context.Context, cfg config.Cluster, report engine.Reporter) error {
	if masterbackup.IsFenced() {
		return fencedError()
	}
	if !cfg.BackupConfigured() {
		return nil
	}
	t, _, err := steps.BackupTarget(cfg)
	if err != nil {
		return nil //nolint:nilerr // no keys on the node: the job reports that itself.
	}
	_, _, err = checkFence(ctx, cfg, t, report)
	return err
}

// runFenceCheck is the fence-check job (boot and every five minutes).
func runFenceCheck(ctx context.Context, cfg config.Cluster, report engine.Reporter) (any, error) {
	if masterbackup.IsFenced() {
		f, _ := masterbackup.ReadFenced("")
		// Keep the poolers and timers down after a reboot.
		_ = applyFence(ctx, masterbackup.Epoch{Epoch: f.Epoch, InstanceID: f.InstanceID})
		writeMasterMetrics(cfg)
		return map[string]any{"job": "fence-check", "state": string(masterbackup.FenceFenced)}, nil
	}
	if !cfg.BackupConfigured() {
		return map[string]any{"job": "fence-check", "state": "no_bucket"}, nil
	}
	t, _, err := steps.BackupTarget(cfg)
	if err != nil {
		return map[string]any{"job": "fence-check", "state": string(masterbackup.FenceUnknown)}, nil //nolint:nilerr // no keys: nothing to check.
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	state, e, err := checkFence(ctx, cfg, t, report)
	if state == masterbackup.FenceOK && e != nil {
		_ = saveLocalEpoch(*e)
	}
	writeMasterMetrics(cfg)
	if err != nil && state == masterbackup.FenceFenced {
		// Fencing is the job doing its work, not a failure of the job.
		return map[string]any{"job": "fence-check", "state": string(state)}, nil
	}
	return map[string]any{"job": "fence-check", "state": string(state)}, err
}

// --- metrics (Phase 4 hook) ---

// writeMasterMetrics writes eru-master-backup.prom.
func writeMasterMetrics(cfg config.Cluster) {
	_ = promfile.Write(promMasterBackup, masterFamilies(cfg, loadMasterLast(), time.Now()))
}

func unixOf(ts string) float64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	return float64(t.Unix())
}

func masterFamilies(cfg config.Cluster, last MasterLast, _ time.Time) []*promfile.Family {
	success := promfile.Gauge("eru_master_backup_last_success_timestamp_seconds", "When the last master bundle reached its destination (the bucket, or the node without one).")
	failure := promfile.Gauge("eru_master_backup_last_failure_timestamp_seconds", "When the master backup last failed.")
	verified := promfile.Gauge("eru_master_backup_last_verified_timestamp_seconds", "When the bucket's newest bundle was last confirmed to be this node's.")
	size := promfile.Gauge("eru_master_backup_size_bytes", "Size of the last master bundle.")
	dirty := promfile.Gauge("eru_master_backup_dirty", "1 while a change waits to be bundled or uploaded.")
	local := promfile.Gauge("eru_master_backup_local_bundles", "Master bundles kept on the node.")
	epoch := promfile.Gauge("eru_master_epoch", "The master's epoch.")
	fenced := promfile.Gauge("eru_master_fenced", "1 when this node was fenced by a newer master.")
	awaiting := promfile.Gauge("eru_database_awaiting_recovery", "1 for a database a rebuilt master hasn't recovered yet.")

	at := last.CreatedAt
	if cfg.BackupConfigured() {
		at = last.UploadedAt
	}
	if v := unixOf(at); v > 0 {
		success.Add(v)
	}
	if v := unixOf(last.LastFailureAt); v > 0 {
		failure.Add(v)
	}
	if v := unixOf(last.LastVerifiedAt); v > 0 {
		verified.Add(v)
	}
	if last.Size > 0 {
		size.Add(float64(last.Size))
	}
	isDirty, _ := masterbackup.Dirty("")
	if last.PendingSince != "" {
		isDirty = true
	}
	dirty.Add(boolFloat(isDirty))
	local.Add(float64(len(localBundles())))
	e, _ := masterbackup.ReadLocalEpoch("")
	epoch.Add(float64(e.Epoch))
	fenced.Add(boolFloat(masterbackup.IsFenced()))
	if recs, err := backup.LoadRecords(); err == nil {
		for _, db := range promfile.SortedKeys(recs.AwaitingRecovery) {
			awaiting.Add(1, promfile.L("db", db)...)
		}
	}
	return []*promfile.Family{success, failure, verified, size, dirty, local, epoch, fenced, awaiting}
}

func boolFloat(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// masterFacts is the master backup for doctor (Phase 5, unit 8).
func masterFacts(cfg config.Cluster) doctor.MasterFacts {
	if _, err := os.Stat(masterbackup.Dir); err != nil {
		return doctor.MasterFacts{}
	}
	last := loadMasterLast()
	f := doctor.MasterFacts{
		Known: true, Bucket: cfg.BackupConfigured(), PendingSince: last.PendingSince, LastFailure: last.LastFailure,
		LastVerifiedAt: last.LastVerifiedAt, LastExportedAt: last.LastExportedAt,
	}
	if fe, ok := masterbackup.ReadFenced(""); ok {
		f.Fenced, f.FencedSeen = true, fe.SeenAt
	}
	if local := localBundles(); len(local) > 0 {
		f.NewestLocal = local[0].CreatedAt
	}
	f.Dirty, _ = masterbackup.Dirty("")
	return f
}

// masterStatus is the master backup as status, doctor and server backup
// list see it.
type masterStatus struct {
	Last       MasterLast           `json:"last"`
	Local      []LocalBundle        `json:"local"`
	Epoch      masterbackup.Epoch   `json:"epoch"`
	InstanceID string               `json:"instance_id,omitempty"`
	Fenced     *masterbackup.Fenced `json:"fenced,omitempty"`
	Dirty      bool                 `json:"dirty"`
	Bucket     bool                 `json:"bucket"`
	// ContentHash is the live content hash, to compare with a bundle's.
	ContentHash string `json:"content_hash,omitempty"`
}

func readMasterStatus(cfg config.Cluster, withHash bool) masterStatus {
	st := masterStatus{Last: loadMasterLast(), Local: localBundles(), Bucket: cfg.BackupConfigured()}
	st.Epoch, _ = masterbackup.ReadLocalEpoch("")
	st.InstanceID, _ = masterbackup.ReadInstance("")
	if f, ok := masterbackup.ReadFenced(""); ok {
		st.Fenced = &f
	}
	st.Dirty, _ = masterbackup.Dirty("")
	if withHash {
		if files, err := masterbackup.Snapshot(""); err == nil {
			st.ContentHash = masterbackup.ContentHash(files)
		}
	}
	return st
}

// trimName normalises a bundle name given with a suffix.
func trimName(name string) string {
	name = strings.TrimSuffix(name, masterbackup.BundleSuffix)
	return strings.TrimSuffix(name, masterbackup.SidecarSuffix)
}
