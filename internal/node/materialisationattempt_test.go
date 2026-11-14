package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func attemptFixture(t *testing.T) (*jobEnv, string) {
	t.Helper()
	_, dir := materialisationFixture(t)
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	ports := filepath.Join(materialisationAncestorBase, "ports")
	if err := os.Mkdir(ports, 0o700); err != nil {
		t.Fatal(err)
	}
	oldRecords, oldInstance := backup.RecordsPath, followHostInstance
	oldNew, oldOwned, oldRestore := callerNewMaterialised, callerNewRestoreOwned, callerRestoreMaterialised
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	followHostInstance = func() (string, error) { return "install-test", nil }
	t.Cleanup(func() {
		backup.RecordsPath, followHostInstance = oldRecords, oldInstance
		callerNewMaterialised, callerNewRestoreOwned, callerRestoreMaterialised = oldNew, oldOwned, oldRestore
	})
	cfg := config.Cluster{
		Server:   config.Server{DataRoot: dir},
		Clusters: []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-stanza"}},
	}
	env := &jobEnv{ID: "job-1", OperationID: "operation-1", installID: "install-test", Job: "restore", DB: "app", Cluster: cfg,
		Params: json.RawMessage(`{"database":"app","kind":"latest"}`)}
	env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(env.lease.finish)
	callerNewMaterialised = func(root, name, source string, sourcePort int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
		return &backup.Materialised{Name: name, Source: source, Data: filepath.Join(root, name), Port: 6001, SourcePort: sourcePort, Shell: sh, StepID: step, Report: report}, nil
	}
	callerNewRestoreOwned = func(root, name, source string, sourcePort int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, sourcePort, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: ports, ConfigRoot: materialisationConfigRoot, UID: materialisationRecordUID,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
		})
	}
	return env, dir
}

func attemptFailure(reason string) error {
	code, exit := output.CodePrecondition, output.ExitPrecondition
	if reason == "recovery_parameter_mismatch" {
		code, exit = output.CodeExternalCommand, output.ExitExternal
	}
	return output.NewError(code, "safe recovery failure", exit, "").WithDetails(map[string]any{"reason": reason})
}

func drillAttemptFixture(t *testing.T) (*jobEnv, string, *backup.Materialised) {
	t.Helper()
	env, dir := attemptFixture(t)
	env.lease.finish()
	env.Job = "restore-drill"
	env.Params = nil
	env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__drill", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(env.lease.finish)
	m, err := callerNewRestoreOwned(dir, backup.DrillName("app"), "app", 5432, engine.Shell{}, env.Job, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.ReleaseForkReservationAuthority)
	t.Cleanup(func() {
		if birth, ok := m.Ownership.(*restoreBirth); ok {
			birth.close()
		}
	})
	return env, dir, m
}

func TestMaterialiseRestoreRejectsReplacedInvocationBeforeConstructor(t *testing.T) {
	for _, change := range []string{"operation", "install", "database", "kind"} {
		t.Run(change, func(t *testing.T) {
			env, _ := attemptFixture(t)
			r := backup.Restore{ID: env.ID, Database: "app", Kind: backup.TargetLatest,
				InstanceID: env.installID, OperationID: env.OperationID, Phase: backup.PhaseRestoring}
			switch change {
			case "operation":
				r.OperationID = "replacement"
			case "install":
				r.InstanceID = "replacement"
			case "database":
				r.Database = "other"
			case "kind":
				r.Recover = true
			}
			if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = r; return nil }); err != nil {
				t.Fatal(err)
			}
			called := false
			callerNewMaterialised = func(string, string, string, int, engine.Shell, string, engine.Reporter) (*backup.Materialised, error) {
				called = true
				return nil, errors.New("unexpected constructor")
			}
			_, _, _, err := materialiseRestore(context.Background(), env, restoreParams{Database: "app", Kind: backup.TargetLatest})
			if err == nil || called {
				t.Fatalf("replaced invocation: err=%v constructor called=%v", err, called)
			}
			records, err := backup.LoadRecords()
			if err != nil || !reflect.DeepEqual(records.Restores["app"], r) {
				t.Fatalf("replaced record changed: %+v, %v", records.Restores["app"], err)
			}
		})
	}
}

func TestRestoreRetainsTypedFailureAndBlocksCleanup(t *testing.T) {
	for _, reason := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			env, dir := attemptFixture(t)
			env.Cluster.Clusters[0].Size = "medium"
			if err := backup.UpdateRecords(func(s *backup.Records) error {
				s.Restores["app"] = backup.Restore{ID: env.ID, Database: "app", Kind: backup.TargetLatest, InstanceID: env.installID, OperationID: env.OperationID,
					Phase: backup.PhaseRestoring, MaterialisationPending: true}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			failure := attemptFailure(reason)
			callerRestoreMaterialised = func(_ context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				if m.DestinationConfig == nil || !strings.Contains(m.DestinationConfig.Tuning, "max_connections = 50") {
					t.Fatalf("intended destination was not rendered: %+v", m.DestinationConfig)
				}
				recs, err := backup.LoadRecords()
				if err != nil || recs.Restores["app"].Attempt == nil || recs.Restores["app"].Attempt.JobID != env.ID {
					t.Fatalf("attempt missing before restore: %+v %v", recs.Restores["app"], err)
				}
				if err := os.Mkdir(m.Data, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(m.Data, "sentinel"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
				return failure
			}
			_, err := jobRestore(context.Background(), env)
			if !errors.Is(err, failure) || !backup.RetainRecoveryFailure(err) {
				t.Fatalf("job error lost type: %v", err)
			}
			recs, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}
			r := recs.Restores["app"]
			if r.Phase != backup.PhaseFailed || r.Attempt == nil || r.Attempt.Reason != reason ||
				r.Attempt.InstanceID != "install-test" || r.Attempt.OperationID != "operation-1" ||
				r.Attempt.Target != "app__restoring" || r.Attempt.Stanza != "app-stanza" || r.Attempt.TemporaryPort != config.TemporaryPortMin {
				t.Fatalf("failed restore record: %+v", r)
			}
			if err := clearFailedRestore(env.Cluster, "app"); err == nil {
				t.Fatal("retry silently cleaned retained target")
			}
			req := rpc.Request{Params: json.RawMessage(`{"database":"app"}`), Confirm: "app/restore"}
			nodeEnv := &Env{Req: req, Cluster: env.Cluster}
			if _, err := handleRestoreAbort(context.Background(), nodeEnv); err == nil {
				t.Fatal("abort deleted without ownership proof")
			}
			if _, err := handleRestorePromote(context.Background(), nodeEnv); err == nil {
				t.Fatal("failed restore promoted")
			}
			if _, err := os.Stat(filepath.Join(dir, "app__restoring", "sentinel")); err != nil {
				t.Fatalf("retained target removed: %v", err)
			}
		})
	}
}

func TestDrillAttemptRetainsTypedFailureAndBlocksNextCleanup(t *testing.T) {
	for _, reason := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			env, _, m := drillAttemptFixture(t)
			failure := attemptFailure(reason)
			callerRestoreMaterialised = func(_ context.Context, _ *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				recs, err := backup.LoadRecords()
				if err != nil || recs.DrillAttempts["app"].JobID != env.ID || recs.Drilling["app"] != m.Name {
					t.Fatalf("attempt missing before drill: %+v %v", recs, err)
				}
				if err := m.Ownership.CreateData(context.Background(), m.OwnershipPaths()); err != nil {
					t.Fatal(err)
				}
				return failure
			}
			_, started, retained, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
			if !started || !retained || !errors.Is(err, failure) || !backup.RetainRecoveryFailure(err) {
				t.Fatalf("drill result: %v %v %v", started, retained, err)
			}
			recs, err := backup.LoadRecords()
			if err != nil || recs.DrillAttempts["app"].Reason != reason {
				t.Fatalf("drill record: %+v %v", recs.DrillAttempts, err)
			}
			if err := dropKilledDrill(context.Background(), env, recs, "app"); err == nil {
				t.Fatal("next drill deleted retained target")
			}
			if _, err := os.Stat(m.Data); err != nil {
				t.Fatalf("retained drill removed: %v", err)
			}
		})
	}
}

func TestAttemptMetadataFailureDoesNotStartDrill(t *testing.T) {
	env, dir, m := drillAttemptFixture(t)
	backup.RecordsPath = filepath.Join(dir, "records-directory")
	if err := os.Mkdir(backup.RecordsPath, 0o700); err != nil {
		t.Fatal(err)
	}
	called := false
	callerRestoreMaterialised = func(context.Context, *backup.Materialised, backup.Driver, backup.RestoreOptions) error {
		called = true
		return nil
	}
	_, started, retain, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
	if err == nil || started || retain || called {
		t.Fatalf("metadata failure started restore: %v %v %v %v", started, retain, called, err)
	}
}

func TestPendingRestoreBlocksUnverifiedAbortAndRetry(t *testing.T) {
	env, _ := attemptFixture(t)
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{ID: env.ID, Database: "app", Phase: backup.PhaseFailed, MaterialisationPending: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := clearFailedRestore(env.Cluster, "app"); err == nil {
		t.Fatal("pending unknown attempt cleared")
	}
	nodeEnv := &Env{Cluster: env.Cluster, Req: rpc.Request{Params: json.RawMessage(`{"database":"app"}`)}}
	if _, err := handleRestoreAbort(context.Background(), nodeEnv); err == nil || !strings.Contains(err.Error(), "ownership") {
		t.Fatalf("unverified abort: %v", err)
	}
}

func TestRecoverFailureKeepsMatchingAttempt(t *testing.T) {
	for _, reason := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch"} {
		t.Run(reason, func(t *testing.T) {
			env, _ := attemptFixture(t)
			env.Job = "recover"
			env.Params = json.RawMessage(`{"database":"app"}`)
			env.lease.finish()
			env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
			t.Cleanup(env.lease.finish)
			expected := backup.MaterialisationAttempt{JobID: env.ID, Kind: "recover"}
			if err := backup.UpdateRecords(func(s *backup.Records) error {
				s.Restores["app"] = backup.Restore{ID: env.ID, Database: "app", Recover: true, InstanceID: env.installID, OperationID: env.OperationID,
					Phase: backup.PhaseRestoring, MaterialisationPending: true,
					Attempt: &expected}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := failRestoreChecked(env, attemptFailure(reason), &expected); err != nil {
				t.Fatal(err)
			}
			recs, err := backup.LoadRecords()
			if err != nil || recs.Restores["app"].Phase != backup.PhaseFailed ||
				recs.Restores["app"].Attempt == nil || recs.Restores["app"].Attempt.Reason != reason {
				t.Fatalf("recover state: %+v %v", recs.Restores["app"], err)
			}
			if err := clearFailedRestore(env.Cluster, "app"); err == nil {
				t.Fatal("recover retry cleared retained attempt")
			}
		})
	}
}

func TestFailRestoreRefusesDifferentAttempt(t *testing.T) {
	env, _ := attemptFixture(t)
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{ID: "other-job", Phase: backup.PhaseRestoring}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := failRestoreChecked(env, attemptFailure("recovery_requirements_unmet"), nil); err == nil {
		t.Fatal("another job's record was changed")
	}
	recs, err := backup.LoadRecords()
	if err != nil || recs.Restores["app"].Phase != backup.PhaseRestoring || recs.Restores["app"].ID != "other-job" {
		t.Fatalf("foreign record changed: %+v %v", recs.Restores["app"], err)
	}
}

func TestTypedFailureRecordWriteFailureRemainsBlocked(t *testing.T) {
	env, dir := attemptFixture(t)
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{ID: env.ID, Database: "app", Kind: backup.TargetLatest,
			InstanceID: env.installID, OperationID: env.OperationID, Phase: backup.PhaseRestoring, MaterialisationPending: true}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	realPath := backup.RecordsPath
	badPath := filepath.Join(dir, "unwritable-record")
	if err := os.Mkdir(badPath, 0o700); err != nil {
		t.Fatal(err)
	}
	failure := attemptFailure("recovery_parameter_mismatch")
	callerRestoreMaterialised = func(_ context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
		if err := os.Mkdir(m.Data, 0o700); err != nil {
			t.Fatal(err)
		}
		backup.RecordsPath = badPath
		return failure
	}
	_, err := jobRestore(context.Background(), env)
	backup.RecordsPath = realPath
	if !errors.Is(err, failure) || !backup.RetainRecoveryFailure(err) {
		t.Fatalf("lost failure identity: %v", err)
	}
	recs, err := backup.LoadRecords()
	if err != nil || recs.Restores["app"].Phase != backup.PhaseRestoring ||
		!recs.Restores["app"].MaterialisationPending || recs.Restores["app"].Attempt == nil {
		t.Fatalf("intent not preserved: %+v %v", recs.Restores["app"], err)
	}
	if err := clearFailedRestore(env.Cluster, "app"); err == nil {
		t.Fatal("unwritten failure record permitted automatic cleanup")
	}
}

func TestRestoreForeignSameJobAttemptIsNotChangedOrDropped(t *testing.T) {
	for _, field := range []string{"operation", "instance", "target"} {
		for _, outcome := range []string{"typed", "other", "success"} {
			t.Run(field+"/"+outcome, func(t *testing.T) {
				env, dir := attemptFixture(t)
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					s.Restores["app"] = backup.Restore{ID: env.ID, Database: "app", Kind: backup.TargetLatest, InstanceID: env.installID, OperationID: env.OperationID, Phase: backup.PhaseRestoring, MaterialisationPending: true}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				callerRestoreMaterialised = func(_ context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
					if err := os.Mkdir(m.Data, 0o700); err != nil {
						t.Fatal(err)
					}
					if err := backup.UpdateRecords(func(s *backup.Records) error {
						r := s.Restores["app"]
						switch field {
						case "operation":
							r.Attempt.OperationID = "foreign-op"
						case "instance":
							r.Attempt.InstanceID = "foreign-install"
						case "target":
							r.Attempt.Target = "foreign-target"
						}
						s.Restores["app"] = r
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					switch outcome {
					case "typed":
						return attemptFailure("recovery_parameter_mismatch")
					case "other":
						return errors.New("ordinary tool failure")
					default:
						return nil
					}
				}
				_, err := jobRestore(context.Background(), env)
				if err == nil {
					t.Fatal("foreign attempt accepted")
				}
				recs, err := backup.LoadRecords()
				if err != nil || recs.Restores["app"].Phase != backup.PhaseRestoring ||
					recs.Restores["app"].Attempt == nil || recs.Restores["app"].Attempt.Reason != "" {
					t.Fatalf("foreign attempt changed: %+v %v", recs.Restores["app"], err)
				}
				if _, err := os.Stat(filepath.Join(dir, "app__restoring")); err != nil {
					t.Fatalf("foreign target deleted: %v", err)
				}
			})
		}
	}
}

func TestDrillForeignSameJobAttemptIsNotChanged(t *testing.T) {
	for _, field := range []string{"operation", "instance", "target"} {
		t.Run(field, func(t *testing.T) {
			env, _, m := drillAttemptFixture(t)
			callerRestoreMaterialised = func(_ context.Context, _ *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					a := s.DrillAttempts["app"]
					switch field {
					case "operation":
						a.OperationID = "foreign-op"
					case "instance":
						a.InstanceID = "foreign-install"
					case "target":
						a.Target = "foreign-target"
					}
					s.DrillAttempts["app"] = a
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				return attemptFailure("recovery_parameter_mismatch")
			}
			_, started, retained, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
			if !started || !retained || err == nil || !backup.RetainRecoveryFailure(err) {
				t.Fatalf("foreign drill: %v %v %v", started, retained, err)
			}
			recs, err := backup.LoadRecords()
			if err != nil || recs.DrillAttempts["app"].Reason != "" {
				t.Fatalf("foreign reason changed: %+v %v", recs.DrillAttempts["app"], err)
			}
		})
	}
}

func TestAttemptIdentityCoversEveryImmutableField(t *testing.T) {
	base := backup.MaterialisationAttempt{JobID: "job", OperationID: "operation", InstanceID: "install",
		Kind: "restore", Source: "source", Target: "target", Stanza: "stanza",
		SourcePort: 5432, TemporaryPort: 6001, DataPath: "/data/target", ConfigPath: "/config/target"}
	mutations := []func(*backup.MaterialisationAttempt){
		func(a *backup.MaterialisationAttempt) { a.JobID = "other" },
		func(a *backup.MaterialisationAttempt) { a.OperationID = "other" },
		func(a *backup.MaterialisationAttempt) { a.InstanceID = "other" },
		func(a *backup.MaterialisationAttempt) { a.Kind = "other" },
		func(a *backup.MaterialisationAttempt) { a.Source = "other" },
		func(a *backup.MaterialisationAttempt) { a.Target = "other" },
		func(a *backup.MaterialisationAttempt) { a.Stanza = "other" },
		func(a *backup.MaterialisationAttempt) { a.SourcePort++ },
		func(a *backup.MaterialisationAttempt) { a.TemporaryPort++ },
		func(a *backup.MaterialisationAttempt) { a.DataPath = "/other" },
		func(a *backup.MaterialisationAttempt) { a.ConfigPath = "/other" },
	}
	for i, mutate := range mutations {
		changed := base
		mutate(&changed)
		if sameAttemptIdentity(changed, base) {
			t.Fatalf("identity mutation %d accepted", i)
		}
	}
	changed := base
	changed.Reason = "recovery_parameter_mismatch"
	if !sameAttemptIdentity(changed, base) {
		t.Fatal("outcome reason was treated as immutable identity")
	}
}
