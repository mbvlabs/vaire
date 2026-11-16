package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
)

func TestRunDrillRequiresPersistedJobEvidence(t *testing.T) {
	_, root := materialisationFixture(t)
	oldStat, oldUID, oldLoad, oldUpdate, oldLocks := jobNodeStat, jobEffectiveUID, jobLoadCluster, jobUpdateState, nodeLockers
	oldActivity, oldSettle := activityRecorderFactory, settleJobRecord
	t.Cleanup(func() {
		jobNodeStat, jobEffectiveUID, jobLoadCluster, jobUpdateState, nodeLockers = oldStat, oldUID, oldLoad, oldUpdate, oldLocks
		activityRecorderFactory, settleJobRecord = oldActivity, oldSettle
	})
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(materialisationStateDir) }
	jobEffectiveUID = func() int { return 0 }
	jobLoadCluster = func(string) (config.Cluster, error) {
		return config.Cluster{Server: config.Server{DataRoot: root}, Clusters: []config.ClusterSpec{{Name: "app"}}}, nil
	}
	failure := errors.New("state write failed")
	jobUpdateState = func(func(*engine.State) error) error { return failure }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
	settleJobRecord = func(string, bool) error { return nil }
	nodeLockers.node = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		t.Fatal("job acquired a lock without persisted evidence")
		return nil, nil
	}
	_, _, err := RunJob(context.Background(), "restore-drill", JobOptions{DB: "app"}, nil)
	rejectf(t, !errors.Is(err, failure), "state failure lost: %v", err)
}

func TestMaterialisationJobRejectsConflictingHistory(t *testing.T) {
	job := engine.JobRecord{ID: "owner", Job: "restore-drill", DB: "app", State: engine.JobFailed}
	st := engine.State{Jobs: []engine.JobRecord{job}, RetainedJobs: map[string]engine.JobRecord{"owner": job}}
	if _, err := materialisationJob(st, job.ID); err != nil {
		t.Fatal(err)
	}
	foreign := job
	foreign.DB = "other"
	st.RetainedJobs[job.ID] = foreign
	if _, err := materialisationJob(st, job.ID); err == nil {
		t.Fatal("conflicting retained evidence accepted")
	}
	delete(st.RetainedJobs, job.ID)
	st.Jobs = append(st.Jobs, job)
	if _, err := materialisationJob(st, job.ID); err == nil {
		t.Fatal("duplicate history accepted")
	}
}

func TestForkJobRetentionTracksUnresolvedAuthority(t *testing.T) {
	useTempRecords(t)
	o, root := materialisationFixture(t)
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	oldFollower, oldRelease := followerState, releaseMaterialisationJob
	t.Cleanup(func() { followerState, releaseMaterialisationJob = oldFollower, oldRelease })
	followerState = func() state.Store { return ownedForkConfigStore{cfg: &cfg} }
	released := ""
	releaseMaterialisationJob = func(id string) { released = id }
	_, err := createMaterialisation(o, root)
	mustNoErr(t, err)
	mustNoErr(t, backup.SetActivationFence(backup.ActivationFence{
		OperationID: o.Job, Kind: "fork", Database: o.Target, Instance: o.Instance,
		TargetStanza: "target-abcdef", OldStanza: "source-abcdef", NewStanza: "target-abcdef",
		Registration: "target:5433:", Port: 5433, DataDir: o.DataPath, ConfDir: o.RegisteredPath,
		Phase: fencePhasePrepared, CreatedAt: "2026-11-01T00:00:00Z",
	}))
	releaseResolvedMaterialisationJob(o.Job, "db-fork")
	if released != "" {
		t.Fatalf("job released while its fence and marker exist: %q", released)
	}
	mustNoErr(t, backup.ClearActivationFence(o.Target, o.Job))
	releaseResolvedMaterialisationJob(o.Job, "db-fork")
	if released != "" {
		t.Fatalf("job released while its materialisation marker exists: %q", released)
	}
	p, err := materialisationPath(o.Target)
	mustNoErr(t, err)
	mustNoErr(t, os.Remove(p))
	releaseResolvedMaterialisationJob(o.Job, "db-fork")
	if released != o.Job {
		t.Fatalf("resolved job retained: %q", released)
	}
}

func rotatedMaterialisationJob(job engine.JobRecord) engine.State {
	var st engine.State
	st.PutJob(job)
	st.RetainJob(job.ID)
	for i := range 60 {
		st.PutJob(engine.JobRecord{ID: fmt.Sprintf("other-%d", i), Job: "backup-full", State: engine.JobSucceeded})
	}
	return st
}
