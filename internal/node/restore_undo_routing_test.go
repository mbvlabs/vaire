package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestUndoRepublishesAndVerifiesBothPoolers(t *testing.T) {
	for _, scenario := range []string{"settled", "pending-promote", "publish-failed", "session-missing", "resume-failed"} {
		t.Run(scenario, func(t *testing.T) {
			useTempExtensionRecords(t)
			useTestInstance(t)
			cfg := planCfg(t)
			spec := cfg.Clusters[0]
			u := routingUndoFixture(t)
			if scenario == "pending-promote" {
				writeTestFence(t, cfg, "restore-promote", "restore-op")
				if err := backup.UpdateActivationFence(spec.Name, "restore-op", func(f *backup.ActivationFence) error {
					f.Phase = fencePhaseRouting
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			probe := stubUndoRouting(t, scenario, spec)
			warnings := finishUndo(context.Background(), cfg, spec.Name, u,
				engine.Runtime{}, &Env{Cluster: cfg}, func() { probe.released = true })
			requireUndoRoutingResult(t, scenario, probe, warnings)
		})
	}
}

func routingUndoFixture(t *testing.T) backup.Undo {
	t.Helper()
	u := backup.Undo{
		From:  backup.Leftover{Database: "app", Name: "app__pre_restore_20261117T160000", Stanza: "app-abc123", RestoreID: "restore-op"},
		Aside: "app__pre_restore_20261117T161000", Routing: true,
	}
	if err := backup.UpdateRecords(func(recs *backup.Records) error {
		recs.Undoing["app"] = u
		recs.Timelines["app"] = backup.TimelineSwitch{Stanza: "app-abc123", Timeline: "1"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return u
}

type undoRoutingProbe struct {
	published, reads  int
	resumed, released bool
}

func stubUndoRouting(t *testing.T, scenario string, spec config.ClusterSpec) *undoRoutingProbe {
	t.Helper()
	oldPublish, oldRoutes, oldResume := applyPoolerFence, poolerRuntimeRoutes, poolerResumeDatabase
	t.Cleanup(func() {
		applyPoolerFence, poolerRuntimeRoutes, poolerResumeDatabase = oldPublish, oldRoutes, oldResume
	})
	probe := &undoRoutingProbe{}
	applyPoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		probe.published++
		if scenario == "publish-failed" {
			return errors.New("pooler reload failed")
		}
		return nil
	}
	poolerResumeDatabase = func(context.Context, string) { probe.resumed = true }
	poolerRuntimeRoutes = func(_ context.Context, dir string, _ int) (map[string]steps.PoolerRoute, error) {
		probe.reads++
		if scenario == "session-missing" && dir == steps.SessionSocketDir {
			return map[string]steps.PoolerRoute{}, nil
		}
		return map[string]steps.PoolerRoute{
			spec.Name: {
				Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName(),
				Paused: !probe.resumed || scenario == "resume-failed",
			},
		}, nil
	}
	return probe
}

func requireUndoRoutingResult(t *testing.T, scenario string, probe *undoRoutingProbe, warnings []output.Warning) {
	t.Helper()
	if probe.published != 1 {
		t.Fatalf("pooler publications = %d, want 1", probe.published)
	}
	if scenario != "settled" && scenario != "pending-promote" {
		if len(warnings) != 1 || warnings[0].Code != "after_cutover_failed" || probe.released {
			t.Fatalf("routing failure not reported before backup: warnings=%v probe=%+v", warnings, probe)
		}
		recs, err := backup.LoadRecords()
		if err != nil || !recs.Undoing["app"].Routing {
			t.Fatalf("routing failure lost its finish-only continuation: %v %v", recs.Undoing, err)
		}
		return
	}
	if len(warnings) != 0 || probe.reads != 4 || !probe.resumed || !probe.released {
		t.Fatalf("routing not verified: warnings=%v probe=%+v", warnings, probe)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if _, pending := recs.ActivationFenceFor("app"); pending {
		t.Fatal("undo left the promoted routing fence unfinished")
	}
	if _, pending := recs.Undoing["app"]; pending {
		t.Fatal("verified undo still has an unfinished continuation")
	}
}

func TestUndoRoutingRetryDoesNotSwapAgain(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	u := routingUndoFixture(t)
	probe := stubUndoRouting(t, "session-missing", spec)
	env := &Env{Cluster: cfg, Req: request("restore.undo", map[string]any{"database": spec.Name})}
	warnings := finishUndo(context.Background(), cfg, spec.Name, u,
		engine.Runtime{}, env, func() { probe.released = true })
	requireUndoRoutingResult(t, "session-missing", probe, warnings)

	oldSwap := swapCluster
	t.Cleanup(func() { swapCluster = oldSwap })
	swapCluster = func(context.Context, engine.Shell, string, engine.Reporter, string, string, string, string, int) error {
		t.Fatal("a finish-only undo retried its data swap")
		return nil
	}
	probe = stubUndoRouting(t, "settled", spec)
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	result, err := executeUndo(context.Background(), env, cfg, spec, u, true, recs, func() { probe.released = true })
	if err != nil {
		t.Fatal(err)
	}
	requireUndoRoutingResult(t, "settled", probe, result.Warnings)
}

func TestRecordUndoKeepsFinishOnlyContinuation(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	u := routingUndoFixture(t)
	u.Routing = false
	if err := backup.UpdateRecords(func(recs *backup.Records) error {
		recs.Undoing[spec.Name] = u
		recs.Leftovers = []backup.Leftover{u.From}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := recordUndo(spec.Name, u, u.Aside, "2", "1", spec.Stanza); err != nil {
		t.Fatal(err)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	retry, resuming, err := planUndo(recs, spec.Name)
	if err != nil || !resuming || !retry.Routing || retry.From != u.From || retry.Aside != u.Aside {
		t.Fatalf("undo lost its finish-only identity: %+v %v %v", retry, resuming, err)
	}
	if len(recs.Leftovers) != 1 || recs.Leftovers[0].Name != u.Aside {
		t.Fatalf("undo leftovers = %+v", recs.Leftovers)
	}
}

func TestUndoRefusesCrossStanzaLeftover(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "restore-promote", "restore-op")
	if err := backup.UpdateRecords(func(recs *backup.Records) error {
		f := recs.Activation["app"]
		f.Phase = fencePhaseRouting
		recs.Activation["app"] = f
		recs.Leftovers = []backup.Leftover{{
			Database: "app", Name: "app__pre_restore_20261117T160000",
			RestoreID: "restore-op", Stanza: "app-old123",
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req := request("restore.undo", map[string]any{"database": "app"})
	req.DryRun = true
	if _, err := handleRestoreUndo(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || reasonOf(t, err) != "undo_identity_changed" {
		t.Fatalf("cross-stanza undo = %v", err)
	}
}

func TestUndoReloadsConfigurationUnderCutoverLock(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	u := routingUndoFixture(t)
	if err := backup.UpdateRecords(func(recs *backup.Records) error {
		recs.Undoing["app"] = u
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	env := &Env{Cluster: cfg.Clone(), Req: request("restore.undo", map[string]any{"database": "app"})}
	env.Req.Confirm = "app/restore"
	oldLock, oldLoad := lockForCutover, applyReloadCluster
	t.Cleanup(func() { lockForCutover, applyReloadCluster = oldLock, oldLoad })
	locked := false
	lockForCutover = func(context.Context, *Env, string, string) (func(), error) {
		locked = true
		return func() {}, nil
	}
	applyReloadCluster = func() (config.Cluster, error) {
		if !locked {
			t.Fatal("configuration reloaded before cutover lock")
		}
		return cfg, nil
	}
	probe := stubUndoRouting(t, "settled", cfg.Clusters[0])
	result, err := handleRestoreUndo(context.Background(), env)
	if err != nil || len(result.Warnings) != 0 || probe.published != 1 ||
		env.Cluster.Clusters[0].Stanza != cfg.Clusters[0].Stanza {
		t.Fatalf("locked reload: result=%+v error=%v probe=%+v", result, err, probe)
	}
}

func TestRestoreStartCannotOvertakeUnfinishedUndo(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	cfg.Backup.Provider = "s3"
	routingUndoFixture(t)
	oldLocks, oldLoad, oldTarget := nodeLockers, loadGuardCluster, restoreCheckTarget
	t.Cleanup(func() { nodeLockers, loadGuardCluster, restoreCheckTarget = oldLocks, oldLoad, oldTarget })
	locks := 0
	nodeLockers.node = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		locks++
		return func() { locks-- }, nil
	}
	nodeLockers.cluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		locks++
		return func() { locks-- }, nil
	}
	loadGuardCluster = func() (config.Cluster, error) { return cfg, nil }
	restoreCheckTarget = func(context.Context, config.Cluster, restoreParams) (string, error) {
		t.Fatal("new restore reached external preparation while undo was pending")
		return "", nil
	}
	env := &Env{Cluster: cfg, Req: request("restore.start", map[string]any{"database": "app", "kind": "latest"})}
	env.Req.Confirm = "app/restore"
	if _, err := handleRestoreStart(context.Background(), env); err == nil || reasonOf(t, err) != "undo_in_progress" {
		t.Fatalf("new restore with pending undo = %v", err)
	}
	if locks != 0 {
		t.Fatal("restore preparation did not release its locks")
	}
}

func TestUndoRoutingRefusesChangedContinuationBeforePublication(t *testing.T) {
	for _, changed := range []string{"missing-timeline", "different-stanza", "newer-fence"} {
		t.Run(changed, func(t *testing.T) {
			useTempExtensionRecords(t)
			cfg := planCfg(t)
			u := routingUndoFixture(t)
			if err := backup.UpdateRecords(func(recs *backup.Records) error {
				switch changed {
				case "missing-timeline":
					delete(recs.Timelines, "app")
				case "different-stanza":
					recs.Timelines["app"] = backup.TimelineSwitch{Stanza: "app-other123", Timeline: "1"}
				case "newer-fence":
					recs.Activation["app"] = backup.ActivationFence{
						OperationID: "newer-restore", Kind: "restore-promote", Phase: fencePhaseRouting,
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			probe := stubUndoRouting(t, "settled", cfg.Clusters[0])
			err := settleUndoRouting(context.Background(), cfg, "app", u, engine.Runtime{}, nil)
			if err == nil || reasonOf(t, err) != "undo_identity_changed" || probe.published != 0 {
				t.Fatalf("changed continuation was not refused before effects: %v probe=%+v", err, probe)
			}
		})
	}
}
