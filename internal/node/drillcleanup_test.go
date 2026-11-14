package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

func stoppedDrillFixture(t *testing.T, dataOnly, typed bool) (*jobEnv, string, *backup.MaterialisationAttempt) {
	t.Helper()
	env, root, m := drillAttemptFixture(t)
	oldState, oldReservation, oldAbsence := restoreAbsenceState, restoreCleanupReservation, restoreCleanupAbsence
	oldRegistration, oldDrop, oldStore, oldUpdate := restoreCleanupRegistration, restoreCleanupDrop, dbDropStore, drillCleanupUpdateRecords
	t.Cleanup(func() {
		restoreAbsenceState, restoreCleanupReservation, restoreCleanupAbsence = oldState, oldReservation, oldAbsence
		restoreCleanupRegistration, restoreCleanupDrop, dbDropStore = oldRegistration, oldDrop, oldStore
		drillCleanupUpdateRecords = oldUpdate
	})
	ports := filepath.Join(materialisationAncestorBase, "ports")
	restoreCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
		return backup.CleanupRestoreReservationWithHost(ctx, ports, materialisationRecordUID, name, port, id, effect)
	}
	restoreCleanupAbsence = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		return backup.LockForkReservationWithHost(ctx, ports, materialisationRecordUID, name, port, present)
	}
	restoreCleanupRegistration = func(string, string, string, int, int) error { return nil }
	restoreCleanupDrop = func(context.Context, *Env, string) error { return nil }
	oldJob := engine.JobRecord{ID: env.ID, Job: "restore-drill", DB: "app", State: engine.JobFailed}
	restoreAbsenceState = func() (engine.State, error) { return engine.State{Jobs: []engine.JobRecord{oldJob}}, nil }
	store := &followerDropStore{cfg: env.Cluster, fail: true}
	dbDropStore = func() state.Store { return store }
	callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
		p := m.OwnershipPaths()
		if err := m.Ownership.CreateData(ctx, p); err != nil {
			return err
		}
		if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error {
			if dataOnly && typed {
				return attemptFailure("recovery_requirements_unmet")
			}
			return nil
		}); err != nil {
			return err
		}
		if dataOnly {
			return nil
		}
		if err := m.Ownership.CreateConfig(ctx, p); err != nil {
			return err
		}
		if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
			return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte("test"), 0o600)
		}); err != nil {
			return err
		}
		if typed {
			return m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error {
				return attemptFailure("recovery_requirements_unmet")
			})
		}
		return nil
	}
	attempt, started, retained, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
	if !started || retained != typed || (err != nil) != typed {
		t.Fatalf("prepare drill: started=%v retained=%v err=%v", started, retained, err)
	}
	env.lease.finish()
	m.ReleaseForkReservationAuthority()
	if err := os.Mkdir(filepath.Join(root, "app"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "app", "sentinel"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	return env, root, attempt
}

func TestDrillDropExactOwnedTarget(t *testing.T) {
	for _, mode := range []string{"success", "history-pruned", "data-only", "data-only-unbound-config", "no-confirm", "wrong-confirm", "running", "queued", "foreign-job", "duplicate-job", "state-unreadable", "foreign-install",
		"replace-data", "replace-config", "replace-reservation", "record-during-drop", "record-before-retire", "drop-failure", "retire-failure", "record-save-failure", "registration-failure", "unbound", "legacy", "configured-target"} {
		t.Run(mode, func(t *testing.T) {
			env, root, attempt := stoppedDrillFixture(t, mode == "data-only" || mode == "data-only-unbound-config", true)
			released := false
			if mode == "history-pruned" {
				oldRelease := releaseMaterialisationJob
				releaseMaterialisationJob = func(id string) { released = id == attempt.JobID }
				t.Cleanup(func() { releaseMaterialisationJob = oldRelease })
			}
			cfg := env.Cluster
			if mode == "configured-target" {
				cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: attempt.Target})
				dbDropStore = func() state.Store { return &followerDropStore{cfg: cfg, fail: true} }
			}
			switch mode {
			case "history-pruned":
				restoreAbsenceState = func() (engine.State, error) {
					return rotatedMaterialisationJob(engine.JobRecord{ID: attempt.JobID, Job: "restore-drill", DB: "app", State: engine.JobFailed}), nil
				}
			case "data-only-unbound-config":
				if err := os.Mkdir(filepath.Join(materialisationConfigRoot, attempt.Target), 0o700); err != nil {
					t.Fatal(err)
				}
			case "running", "queued":
				status := engine.JobRunning
				if mode == "queued" {
					status = engine.JobQueued
				}
				restoreAbsenceState = func() (engine.State, error) {
					return engine.State{Jobs: []engine.JobRecord{{ID: attempt.JobID, Job: "restore-drill", DB: "app", State: status}}}, nil
				}
			case "foreign-job":
				restoreAbsenceState = func() (engine.State, error) {
					return engine.State{Jobs: []engine.JobRecord{{ID: attempt.JobID, Job: "restore", DB: "app", State: engine.JobFailed}}}, nil
				}
			case "duplicate-job":
				restoreAbsenceState = func() (engine.State, error) {
					job := engine.JobRecord{ID: attempt.JobID, Job: "restore-drill", DB: "app", State: engine.JobFailed}
					return engine.State{Jobs: []engine.JobRecord{job, job}}, nil
				}
			case "state-unreadable":
				restoreAbsenceState = func() (engine.State, error) { return engine.State{}, errors.New("unreadable") }
			case "foreign-install":
				followHostInstance = func() (string, error) { return "other", nil }
			case "registration-failure":
				restoreCleanupRegistration = func(string, string, string, int, int) error { return errors.New("foreign registration") }
			case "unbound":
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					a := s.DrillAttempts["app"]
					a.DataIdentity = backup.ArtifactIdentity{}
					s.DrillAttempts["app"] = a
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "replace-data", "replace-config", "replace-reservation":
				path := filepath.Join(root, attempt.Target)
				switch mode {
				case "replace-config":
					path = filepath.Join(materialisationConfigRoot, attempt.Target)
				case "replace-reservation":
					path = filepath.Join(materialisationAncestorBase, "ports", attempt.Target)
				}
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if mode == "replace-reservation" {
					if err := os.WriteFile(path, []byte("6001\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				if err := backup.UpdateRecords(func(s *backup.Records) error { delete(s.DrillAttempts, "app"); return nil }); err != nil {
					t.Fatal(err)
				}
			}
			drops := 0
			restoreCleanupDrop = func(_ context.Context, _ *Env, name string) error {
				drops++
				if name != attempt.Target {
					t.Fatalf("dropped %s", name)
				}
				switch mode {
				case "drop-failure":
					return errors.New("injected drop failure")
				case "record-during-drop":
					return backup.UpdateRecords(func(s *backup.Records) error {
						a := s.DrillAttempts["app"]
						a.OperationID = "replacement"
						s.DrillAttempts["app"] = a
						return nil
					})
				}
				return nil
			}
			if mode == "retire-failure" {
				restoreCleanupAbsence = func(context.Context, string, int, bool) (func(), error) {
					return nil, errors.New("injected retirement failure")
				}
			}
			if mode == "record-save-failure" {
				drillCleanupUpdateRecords = func(func(*backup.Records) error) error { return errors.New("injected record save failure") }
			}
			if mode == "record-before-retire" {
				drillCleanupUpdateRecords = func(fn func(*backup.Records) error) error {
					if err := backup.UpdateRecords(func(s *backup.Records) error {
						a := s.DrillAttempts["app"]
						a.OperationID = "replacement"
						s.DrillAttempts["app"] = a
						return nil
					}); err != nil {
						return err
					}
					return backup.UpdateRecords(fn)
				}
			}
			confirm := attempt.Target
			switch mode {
			case "no-confirm":
				confirm = ""
			case "wrong-confirm":
				confirm = "app"
			}
			req := rpc.Request{Confirm: confirm, Params: json.RawMessage(`{"name":"app__drill"}`)}
			res, err := handleDBDrop(context.Background(), &Env{Cluster: cfg, Req: req})
			success := mode == "success" || mode == "history-pruned" || mode == "data-only"
			if success != (err == nil) {
				t.Fatalf("result=%+v err=%v", res, err)
			}
			if success && drops != map[bool]int{true: 0, false: 1}[mode == "data-only"] {
				t.Fatalf("drop calls %d", drops)
			}
			if mode == "history-pruned" && !released {
				t.Fatal("retained job was not released")
			}
			if !success && mode != "drop-failure" && mode != "record-during-drop" && mode != "record-before-retire" && mode != "retire-failure" && mode != "record-save-failure" && drops != 0 {
				t.Fatalf("unexpected destructive callback: %d", drops)
			}
			records, readErr := backup.LoadRecords()
			if readErr != nil {
				t.Fatal(readErr)
			}
			_, exists := records.DrillAttempts["app"]
			if mode != "legacy" && exists == success {
				t.Fatalf("attempt presence %v after success=%v", exists, success)
			}
			if (records.Drilling["app"] == "") != success {
				t.Fatalf("drilling record: %+v", records.Drilling)
			}
			data, readErr := os.ReadFile(filepath.Join(root, "app", "sentinel")) //nolint:gosec // test fixture path
			if readErr != nil || string(data) != "source" || !reflect.DeepEqual(env.Cluster.Clusters, []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-stanza"}}) {
				t.Fatalf("source changed: %q %v", data, readErr)
			}
		})
	}
}

func TestKilledDrillRequiresCurrentTargetLease(t *testing.T) {
	env, _, _ := stoppedDrillFixture(t, false, false)
	records, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	drops := 0
	restoreCleanupDrop = func(context.Context, *Env, string) error { drops++; return nil }
	if err := dropKilledDrill(context.Background(), env, records, "app"); err == nil || drops != 0 {
		t.Fatalf("stale lease: drops=%d err=%v", drops, err)
	}
}

func TestJobDrillRetriesOnlyOrdinaryStoppedAttempt(t *testing.T) {
	for _, mode := range []string{"ordinary", "typed-retained", "stale-running", "live-running", "foreign-after-reap"} {
		t.Run(mode, func(t *testing.T) {
			typed := mode == "typed-retained"
			oldEnv, root, oldAttempt := stoppedDrillFixture(t, false, typed)
			oldInfo, oldSpace, oldMetrics, oldNew := drillReadInfo, drillCheckSpace, drillWriteMetrics, callerNewDrillOwned
			t.Cleanup(func() {
				drillReadInfo, drillCheckSpace, drillWriteMetrics, callerNewDrillOwned = oldInfo, oldSpace, oldMetrics, oldNew
			})
			if mode == "stale-running" || mode == "live-running" || mode == "foreign-after-reap" {
				oldReap, oldState, oldGone := reapDrillJob, reapJobState, reapJobGone
				t.Cleanup(func() { reapDrillJob, reapJobState, reapJobGone = oldReap, oldState, oldGone })
				st := rotatedMaterialisationJob(engine.JobRecord{ID: oldAttempt.JobID, Job: "restore-drill", DB: "app", State: engine.JobRunning, PID: 123})
				restoreAbsenceState = func() (engine.State, error) { return st, nil }
				reapDrillJob = reapJobs
				reapJobState = func(fn func(*engine.State) error) error { return fn(&st) }
				reapJobGone = func(_ context.Context, rec engine.JobRecord) bool {
					if rec.ID != oldAttempt.JobID {
						t.Fatalf("reaped unrelated job %s", rec.ID)
					}
					return mode == "stale-running"
				}
				if mode == "foreign-after-reap" {
					reapDrillJob = func(context.Context, string) error {
						rec := st.RetainedJobs[oldAttempt.JobID]
						rec.PID, rec.State = 456, engine.JobFailed
						st.PutJob(rec)
						return nil
					}
				}
			}
			drillReadInfo = func(context.Context, config.ClusterSpec, string, engine.Reporter) (backup.Info, error) {
				return backup.Info{Sets: []backup.Set{{Label: "backup"}}}, nil
			}
			drillCheckSpace = func(context.Context, config.Cluster, config.ClusterSpec) error { return nil }
			drillWriteMetrics = func(config.Cluster) {}
			callerNewDrillOwned = callerNewRestoreOwned
			drops, started := 0, 0
			restoreCleanupDrop = func(context.Context, *Env, string) error { drops++; return nil }
			callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				started++
				records, err := backup.LoadRecords()
				if err != nil || records.DrillAttempts["app"].JobID != "job-2" || records.Drilling["app"] != m.Name {
					t.Fatalf("new attempt not bound: %+v %v", records, err)
				}
				p := m.OwnershipPaths()
				if err := m.Ownership.CreateData(ctx, p); err != nil {
					return err
				}
				return m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error {
					return attemptFailure("recovery_requirements_unmet")
				})
			}
			cfg := oldEnv.Cluster
			cfg.Backup.Provider = "r2"
			cfg.Clusters[0].Size = "small"
			env := &jobEnv{ID: "job-2", OperationID: "operation-2", installID: oldEnv.installID, Job: "restore-drill", DB: "app", Cluster: cfg}
			env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__drill", mode: engine.LockExclusive}}, nil, func() {})
			t.Cleanup(env.lease.finish)
			_, err := jobDrill(context.Background(), env)
			if err == nil {
				t.Fatal("expected retained failure")
			}
			records, readErr := backup.LoadRecords()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if typed || mode == "live-running" || mode == "foreign-after-reap" {
				if drops != 0 || started != 0 || records.DrillAttempts["app"] != *oldAttempt {
					t.Fatalf("retained attempt changed: drops=%d started=%d record=%+v", drops, started, records.DrillAttempts["app"])
				}
			} else if drops != 1 || started != 1 || records.DrillAttempts["app"].JobID != env.ID ||
				records.DrillAttempts["app"].Reason != "recovery_requirements_unmet" {
				t.Fatalf("retry: drops=%d started=%d record=%+v", drops, started, records.DrillAttempts["app"])
			}
			if data, err := os.ReadFile(filepath.Join(root, "app", "sentinel")); err != nil || string(data) != "source" { //nolint:gosec // test fixture path
				t.Fatalf("source changed: %q %v", data, err)
			}
		})
	}
}
