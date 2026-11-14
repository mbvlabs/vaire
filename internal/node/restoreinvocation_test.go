package node

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestRestoreInvocationFence(t *testing.T) {
	env, _ := attemptFixture(t)
	for _, job := range []string{"restore", "recover"} {
		t.Run(job, func(t *testing.T) {
			env.Job = job
			env.Params = json.RawMessage(`{"database":"app","kind":"latest"}`)
			base := backup.Restore{ID: env.ID, Database: "app", InstanceID: env.installID,
				OperationID: env.OperationID, Phase: backup.PhaseRestoring, Kind: backup.TargetLatest, Recover: job == "recover", Promote: job == "recover"}
			cases := []struct {
				name string
				edit func(*backup.Restore)
			}{
				{"matching", func(*backup.Restore) {}},
				{"job", func(r *backup.Restore) { r.ID = "new" }},
				{"database", func(r *backup.Restore) { r.Database = "other" }},
				{"phase", func(r *backup.Restore) { r.Phase = backup.PhaseFailed }},
				{"install", func(r *backup.Restore) { r.InstanceID = "other" }},
				{"operation", func(r *backup.Restore) { r.OperationID = "other" }},
				{"legacy", func(r *backup.Restore) { r.InstanceID = "" }},
				{"type", func(r *backup.Restore) { r.Recover = !r.Recover }},
				{"source kind", func(r *backup.Restore) { r.Kind = targetDump }},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					r := base
					tc.edit(&r)
					if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = r; return nil }); err != nil {
						t.Fatal(err)
					}
					err := validateRestoreInvocation(env)
					if (err == nil) != (tc.name == "matching") {
						t.Fatalf("validation: %v", err)
					}
					got, err := backup.LoadRecords()
					if err != nil || !reflect.DeepEqual(got.Restores["app"], r) {
						t.Fatalf("record changed: %+v %v", got.Restores["app"], err)
					}
				})
			}
			if err := backup.UpdateRecords(func(s *backup.Records) error { delete(s.Restores, "app"); return nil }); err != nil {
				t.Fatal(err)
			}
			if validateRestoreInvocation(env) == nil {
				t.Fatal("missing record accepted")
			}
		})
	}
}

func TestRestorePreparationLockOrderAndRelease(t *testing.T) {
	oldLocks, oldLoad := nodeLockers, loadGuardCluster
	t.Cleanup(func() { nodeLockers, loadGuardCluster = oldLocks, oldLoad })
	var events []string
	nodeLockers.node = func(_ context.Context, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
		events = append(events, "node")
		if mode != engine.LockShared {
			t.Fatal("node lock not shared")
		}
		return func() { events = append(events, "-node") }, nil
	}
	nodeLockers.cluster = func(_ context.Context, name string, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
		events = append(events, name)
		if (name == "app" && mode != engine.LockShared) || (name != "app" && mode != engine.LockExclusive) {
			t.Fatalf("wrong lock mode for %s", name)
		}
		return func() { events = append(events, "-"+name) }, nil
	}
	loadGuardCluster = func() (config.Cluster, error) { events = append(events, "load"); return config.Cluster{}, nil }
	env := &Env{Req: rpc.Request{RequestID: "job"}}
	err := withRestorePreparation(context.Background(), env, "app", "restore.start", func() error {
		events = append(events, "publish")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events = append(events, "launch")
	want := []string{"node", "app", "app__restoring", "load", "publish", "-app__restoring", "-app", "-node", "launch"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events %v, want %v", events, want)
	}
}
