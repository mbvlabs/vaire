package node

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestRunJobDumpRetainedGuardAndRelease(t *testing.T) {
	o, root := materialisationFixture(t)
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: o.Target}, {Name: "ordinary"}}}
	cfg.Server.DataRoot = root
	oldLocks, oldStat, oldUID, oldLoad := nodeLockers, jobNodeStat, jobEffectiveUID, jobLoadCluster
	oldUpdate, oldFence, oldActivity, oldSettle := jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord
	t.Cleanup(func() {
		nodeLockers, jobNodeStat, jobEffectiveUID, jobLoadCluster = oldLocks, oldStat, oldUID, oldLoad
		jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord = oldUpdate, oldFence, oldActivity, oldSettle
	})
	acquired, released, fences := 0, 0, 0
	take := func() engine.Release { acquired++; return func() { released++ } }
	nodeLockers.node = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return take(), nil
	}
	nodeLockers.cluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return take(), nil
	}
	nodeLockers.heavyWait = func(context.Context, string, string, time.Duration) (engine.Release, error) { return take(), nil }
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(materialisationStateDir) }
	jobEffectiveUID = func() int { return 0 }
	jobLoadCluster = func(string) (config.Cluster, error) { return cfg, nil }
	var state engine.State
	jobUpdateState = func(fn func(*engine.State) error) error { return fn(&state) }
	jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { fences++; return nil }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
	settleJobRecord = func(string, bool) error { return nil }
	run := func(name string, wantFence int) error {
		t.Helper()
		_, _, err := RunJob(context.Background(), "db-dump", JobOptions{DB: name}, nil)
		if acquired != released || fences != wantFence {
			t.Fatalf("locks %d/%d fence %d", acquired, released, fences)
		}
		return err
	}
	if err := run(o.Target, 0); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("retained: %v", err)
	}
	// No bucket configured: reaching the ordinary dispatch returns its normal
	// backup precondition, without invoking any host or network operation.
	if err := run("ordinary", 1); err == nil {
		t.Fatal("expected missing backup precondition")
	}
	path, _ := materialisationPath(o.Target)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(o.Target, 1); output.ExitCode(err) != output.ExitPrecondition {
		t.Fatalf("corrupt: %v", err)
	}
}
