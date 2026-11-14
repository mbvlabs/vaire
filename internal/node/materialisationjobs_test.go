package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
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
	if !errors.Is(err, failure) {
		t.Fatalf("state failure lost: %v", err)
	}
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

func rotatedMaterialisationJob(job engine.JobRecord) engine.State {
	var st engine.State
	st.PutJob(job)
	st.RetainJob(job.ID)
	for i := range 60 {
		st.PutJob(engine.JobRecord{ID: fmt.Sprintf("other-%d", i), Job: "backup-full", State: engine.JobSucceeded})
	}
	return st
}
