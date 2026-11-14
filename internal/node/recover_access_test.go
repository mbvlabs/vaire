package node

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

func TestRecoveredAccessRequiresExactOwnedRestore(t *testing.T) {
	old := recoverProvisionOwned
	t.Cleanup(func() { recoverProvisionOwned = old })
	calls := 0
	fail := errors.New("access transaction failed")
	recoverProvisionOwned = func(_ context.Context, _ config.Cluster, _ engine.Runtime, name, recovering string, _ engine.Reporter) error {
		calls++
		if name != "app" || recovering != "app" {
			t.Fatalf("authorization crossed target: %q %q", name, recovering)
		}
		return fail
	}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "standby", Follows: "app"}}}
	r := backup.Restore{ID: "restore", Database: "app", Phase: backup.PhaseReady, Recover: true}
	err := provisionRecoveredAccess(context.Background(), cfg, "app", r, engine.Runtime{}, nil)
	if !errors.Is(err, fail) || calls != 1 {
		t.Fatalf("owned access failure: %v, calls %d", err, calls)
	}
	for _, invalid := range []backup.Restore{
		{ID: "restore", Database: "other", Phase: backup.PhaseReady, Recover: true},
		{Database: "app", Phase: backup.PhaseReady, Recover: true},
		{ID: "restore", Database: "app", Phase: backup.PhaseFailed, Recover: true},
	} {
		if err := provisionRecoveredAccess(context.Background(), cfg, "app", invalid, engine.Runtime{}, nil); err == nil {
			t.Fatalf("invalid restore authorized: %+v", invalid)
		}
	}
	r.Recover = false
	if err := provisionRecoveredAccess(context.Background(), cfg, "app", r, engine.Runtime{}, nil); err != nil || calls != 1 {
		t.Fatalf("ordinary restore widened access: %v, calls %d", err, calls)
	}
	r.Database, r.Recover = "standby", true
	if err := provisionRecoveredAccess(context.Background(), cfg, "standby", r, engine.Runtime{}, nil); err == nil || calls != 1 {
		t.Fatalf("physical follower authorized: %v, calls %d", err, calls)
	}
}

func TestRecoverPromotionAccessFailureKeepsPublicationFenced(t *testing.T) {
	oldPath := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	oldFence, oldPause, oldSwap := promotePoolerFence, promotePause, promoteSwap
	oldStart, oldPromote, oldSettle := promoteStartCluster, promoteDatabase, promoteSettle
	oldOwned, oldPoolers, oldFull := recoverProvisionOwned, promoteApplyPoolers, promoteStartFull
	t.Cleanup(func() {
		backup.RecordsPath = oldPath
		promotePoolerFence, promotePause, promoteSwap = oldFence, oldPause, oldSwap
		promoteStartCluster, promoteDatabase, promoteSettle = oldStart, oldPromote, oldSettle
		recoverProvisionOwned, promoteApplyPoolers, promoteStartFull = oldOwned, oldPoolers, oldFull
	})
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abcdef", Since: "2026-01-01T00:00:00Z"}
		s.Restores["app"] = backup.Restore{ID: "owned", Database: "app", Phase: backup.PhaseReady, Kind: backup.TargetLatest, Port: 6433, Recover: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var calls []string
	promotePoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		calls = append(calls, "fence")
		return nil
	}
	promotePause = func(context.Context, string) func() {
		calls = append(calls, "pause")
		return func() { calls = append(calls, "resume") }
	}
	promoteSwap = func(context.Context, engine.Shell, string, engine.Reporter, string, string, string, string, int) error {
		calls = append(calls, "swap")
		return nil
	}
	promoteStartCluster = func(context.Context, engine.Shell, string, engine.Reporter, string, string, int, string) error {
		calls = append(calls, "start")
		return nil
	}
	promoteDatabase = func(context.Context, pg.Local, string) ([]string, error) {
		calls = append(calls, "writable")
		return nil, nil
	}
	promoteSettle = func(context.Context, engine.Shell, string, engine.Reporter, pg.Local, string, string, int, []string) error {
		calls = append(calls, "settle")
		return nil
	}
	recoverProvisionOwned = func(_ context.Context, _ config.Cluster, _ engine.Runtime, name, recovering string, _ engine.Reporter) error {
		calls = append(calls, "access")
		if name != "app" || recovering != "app" {
			t.Fatalf("wrong target: %s %s", name, recovering)
		}
		return errors.New("grant failed")
	}
	promoteApplyPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		calls = append(calls, "publish")
		return nil
	}
	promoteStartFull = func(context.Context, config.Cluster, engine.Runtime, string, string, engine.Reporter) error {
		calls = append(calls, "backup")
		return nil
	}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abcdef"}}}
	cfg.Server.DataRoot = t.TempDir()
	result, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {})
	if err == nil || result != nil || !slices.Equal(calls, []string{"fence", "pause", "swap", "start", "writable", "settle", "access", "resume"}) {
		t.Fatalf("unsafe promotion sequence %v, result %v, error %v", calls, result, err)
	}
	recs, err := backup.LoadRecords()
	if err != nil || !recs.IsAwaiting("app") || !recs.Restores["app"].Recover {
		t.Fatalf("failure lost durable recovery fence: %+v, %v", recs, err)
	}
}

func TestRecoverInspectionReadyDoesNotClaimApplicationReadiness(t *testing.T) {
	old := recoverInspectionPoolers
	t.Cleanup(func() { recoverInspectionPoolers = old })
	calls := 0
	recoverInspectionPoolers = func(_ context.Context, _ config.Cluster, _ engine.Runtime, recovering string, _ engine.Reporter) error {
		calls++
		if recovering != "app" {
			t.Fatalf("inspection did not retain exact recovery target: %q", recovering)
		}
		return nil
	}
	got := recoverInspectionReady(context.Background(), config.Cluster{}, engine.Runtime{}, "app", nil, map[string]any{})
	if calls != 1 || got["phase"] != backup.PhaseReady || got["application_ready"] != false || got["access_coverage"] != "unknown" {
		t.Fatalf("inspection state: %+v", got)
	}
}

func TestAwaitingPromoteRequiresReadyRecoverRecord(t *testing.T) {
	recs := backup.Records{
		AwaitingRecovery: map[string]backup.Awaiting{"app": {}},
		Restores: map[string]backup.Restore{
			"app": {ID: "id", Database: "app", Phase: backup.PhaseReady, Recover: true},
		},
	}
	if !awaitingRecoverPromotable("app", recs) || awaitingRecoverPromotable("other", recs) {
		t.Fatal("ready recover ownership not scoped to its target")
	}
	r := recs.Restores["app"]
	r.Recover = false
	recs.Restores["app"] = r
	if awaitingRecoverPromotable("app", recs) {
		t.Fatal("ordinary restore authorized recovery bypass")
	}
	r.Recover, r.Phase = true, backup.PhaseFailed
	recs.Restores["app"] = r
	if awaitingRecoverPromotable("app", recs) {
		t.Fatal("failed recovery authorized promotion")
	}
}
