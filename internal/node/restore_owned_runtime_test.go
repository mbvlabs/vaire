package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestRestoreOwnedAbortLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "history-pruned", "mismatch-success", "recover-success", "no-promote-ready", "ordinary-failure", "replace-data", "replace-config", "replace-reservation", "missing-reservation", "layout", "record-changed", "record-during-drop", "record-before-retire", "unbound", "foreign-job", "drop-failure", "stale-invocation", "stale-record", "stale-lease"} {
		t.Run(mode, func(t *testing.T) {
			released := false
			if mode == "history-pruned" {
				oldRelease := releaseMaterialisationJob
				releaseMaterialisationJob = func(id string) { released = id == "job" }
				t.Cleanup(func() { releaseMaterialisationJob = oldRelease })
			}
			_, dataRoot := materialisationFixture(t)
			ports := filepath.Join(materialisationAncestorBase, "ports")
			for _, dir := range []string{dataRoot, ports} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			oldRecords, oldNew, oldRestore := backup.RecordsPath, callerNewRestoreOwned, callerRestoreMaterialised
			oldHost, oldState := materialisationHostInfo, restoreAbsenceState
			oldReservation, oldAbsent := restoreCleanupReservation, restoreCleanupAbsence
			oldRegistration, oldDrop, oldReload, oldMetrics := restoreCleanupRegistration, restoreCleanupDrop, restoreAbortReload, restoreWriteMetrics
			oldRecovered, oldPoolers, oldRuntimeHost := callerRecoveredTo, restoreApplyPoolers, jobRuntimeHostRead
			t.Cleanup(func() {
				backup.RecordsPath, callerNewRestoreOwned, callerRestoreMaterialised = oldRecords, oldNew, oldRestore
				materialisationHostInfo, restoreAbsenceState = oldHost, oldState
				restoreCleanupReservation, restoreCleanupAbsence = oldReservation, oldAbsent
				restoreCleanupRegistration, restoreCleanupDrop, restoreAbortReload, restoreWriteMetrics = oldRegistration, oldDrop, oldReload, oldMetrics
				callerRecoveredTo, restoreApplyPoolers, jobRuntimeHostRead = oldRecovered, oldPoolers, oldRuntimeHost
			})
			backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
			cfg := config.DefaultCluster()
			cfg.Server.DataRoot = dataRoot
			cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-abcdef", Size: "small"}}
			env := &jobEnv{ID: "job", OperationID: "operation", installID: "install", Job: "restore", DB: "app", Cluster: cfg,
				Params: json.RawMessage(`{"database":"app","kind":"latest"}`)}
			env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
			t.Cleanup(env.lease.finish)
			r := backup.Restore{ID: "job", Database: "app", Kind: backup.TargetLatest, InstanceID: "install", OperationID: "operation",
				Phase: backup.PhaseRestoring, MaterialisationPending: true}
			if mode == "recover-success" {
				env.Job, env.Params = "recover", json.RawMessage(`{"database":"app"}`)
				r.Recover, r.Promote = true, true
				env.lease.finish()
				env.lease = newLockedJobLease(env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
				t.Cleanup(env.lease.finish)
			}
			if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = r; return nil }); err != nil {
				t.Fatal(err)
			}
			materialisationHostInfo = func(string) (hostinfo.Info, error) { return hostinfo.Info{RAMMiB: 8192, VCPU: 4}, nil }
			callerRecoveredTo = func(context.Context, *backup.Materialised) string { return "" }
			restoreApplyPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error { return nil }
			restoreWriteMetrics = func(config.Cluster) {}
			jobRuntimeHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
			callerNewRestoreOwned = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
				return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
					ReservationDir: ports, ConfigRoot: materialisationConfigRoot, UID: materialisationRecordUID,
					ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
				})
			}
			reason := "recovery_requirements_unmet"
			if mode == "mismatch-success" {
				reason = "recovery_parameter_mismatch"
			}
			failure := attemptFailure(reason)
			callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				p := m.OwnershipPaths()
				if err := m.Ownership.CreateData(ctx, p); err != nil {
					return err
				}
				if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error {
					if err := os.WriteFile(filepath.Join(p.DataDir, "sentinel"), []byte("keep"), 0o600); err != nil {
						return err
					}
					if mode == "ordinary-failure" {
						return errors.New("ordinary restore failure")
					}
					return nil
				}); err != nil {
					return err
				}
				if err := m.Ownership.CreateConfig(ctx, p); err != nil {
					return err
				}
				if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
					return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte(fmt.Sprintf("%s %d", p.DataDir, p.Port)), 0o600)
				}); err != nil {
					return err
				}
				if mode == "no-promote-ready" {
					for _, action := range []backup.MaterialisationAction{backup.MaterialisationPrepare, backup.MaterialisationInspect, backup.MaterialisationStart} {
						if err := m.Ownership.Guard(ctx, p, action, func() error { return nil }); err != nil {
							return err
						}
					}
					return nil
				}
				if mode == "stale-invocation" || mode == "stale-record" {
					if err := backup.UpdateRecords(func(s *backup.Records) error {
						changed := s.Restores["app"]
						if mode == "stale-invocation" {
							changed.OperationID = "replacement"
						} else {
							changed.Target = "replacement target"
						}
						s.Restores["app"] = changed
						return nil
					}); err != nil {
						return err
					}
				}
				return m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error { return failure })
			}
			var err error
			if mode == "stale-lease" {
				env.lease.finish()
			}
			if mode == "recover-success" {
				_, _, attempt, cause := materialiseRestore(context.Background(), env, (recoverParams{Database: "app"}).restore())
				err = errors.Join(cause, failRestoreChecked(env, cause, attempt))
			} else {
				_, err = jobRestore(context.Background(), env)
			}
			if mode == "no-promote-ready" {
				if err != nil {
					t.Fatal(err)
				}
				got, err := backup.LoadRecords()
				if err != nil || got.Restores["app"].Phase != backup.PhaseReady || got.Restores["app"].Attempt != nil ||
					got.Restores["app"].MaterialisationPending || got.Restores["app"].Port != config.TemporaryPortMin {
					t.Fatalf("inspection restore not ready: %+v %v", got.Restores["app"], err)
				}
				return
			}
			if mode == "stale-lease" {
				if err == nil {
					t.Fatal("stale lease accepted")
				}
				if _, err := os.Lstat(filepath.Join(ports, "app__restoring")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("stale lease allocated a reservation: %v", err)
				}
				got, err := backup.LoadRecords()
				if err != nil || got.Restores["app"].Phase != backup.PhaseRestoring || got.Restores["app"].Attempt != nil {
					t.Fatalf("stale lease changed record: %+v %v", got.Restores["app"], err)
				}
				return
			}
			if mode == "stale-invocation" || mode == "stale-record" {
				if err == nil {
					t.Fatal("stale invocation accepted")
				}
				got, _ := backup.LoadRecords()
				if (mode == "stale-invocation" && got.Restores["app"].OperationID != "replacement") ||
					(mode == "stale-record" && got.Restores["app"].Target != "replacement target") || got.Restores["app"].Phase != backup.PhaseRestoring {
					t.Fatalf("new record changed: %+v", got.Restores["app"])
				}
				return
			}
			if mode == "ordinary-failure" {
				if err == nil || !strings.Contains(err.Error(), "ordinary restore failure") {
					t.Fatalf("ordinary failure lost: %v", err)
				}
				got, err := backup.LoadRecords()
				if err != nil || got.Restores["app"].Phase != backup.PhaseFailed || got.Restores["app"].Attempt != nil || !got.Restores["app"].MaterialisationCleaned {
					t.Fatalf("ordinary cleanup state: %+v %v", got.Restores["app"], err)
				}
				for _, path := range []string{filepath.Join(dataRoot, "app__restoring"), filepath.Join(ports, "app__restoring")} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("ordinary cleanup left %s: %v", path, err)
					}
				}
				return
			}
			if !errors.Is(err, failure) {
				t.Fatalf("typed failure lost: %v", err)
			}
			got, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}
			attempt := got.Restores["app"].Attempt
			if got.Restores["app"].Phase != backup.PhaseFailed || attempt == nil ||
				attempt.ArtifactStage != materialisationRegistered || attempt.DataIdentity.Inode == 0 ||
				attempt.ConfigIdentity.Inode == 0 || attempt.ReservationIdentity.Inode == 0 || attempt.Reason != reason {
				t.Fatalf("missing retained birth binding: %+v", got.Restores["app"])
			}
			env.lease.finish()
			restoreAbsenceState = func() (engine.State, error) {
				db := "app"
				if mode == "foreign-job" {
					db = "other"
				}
				job := engine.JobRecord{ID: "job", Job: env.Job, DB: db, State: engine.JobFailed}
				if mode == "history-pruned" {
					return rotatedMaterialisationJob(job), nil
				}
				return engine.State{Jobs: []engine.JobRecord{job}}, nil
			}
			restoreCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
				return backup.CleanupRestoreReservationWithHost(ctx, ports, materialisationRecordUID, name, port, id, effect)
			}
			restoreCleanupAbsence = func(ctx context.Context, name string, port int, present bool) (func(), error) {
				unlock, err := backup.LockForkReservationWithHost(ctx, ports, materialisationRecordUID, name, port, present)
				if err == nil && mode == "record-before-retire" {
					err = backup.UpdateRecords(func(s *backup.Records) error {
						changed := s.Restores["app"]
						changed.OperationID = "newer"
						s.Restores["app"] = changed
						return nil
					})
					if err != nil {
						unlock()
						return nil, err
					}
				}
				return unlock, err
			}
			calls := 0
			restoreCleanupRegistration = func(name, _, root string, _, port int) error {
				calls++
				raw, err := os.ReadFile(filepath.Join(materialisationConfigRoot, name, "postgresql.conf")) //nolint:gosec // temporary test fixture
				if err != nil || string(raw) != fmt.Sprintf("%s %d", filepath.Join(root, name), port) {
					return errors.New("wrong registration layout")
				}
				if mode == "record-changed" && calls == 2 {
					return backup.UpdateRecords(func(s *backup.Records) error {
						changed := s.Restores["app"]
						changed.OperationID = "newer"
						s.Restores["app"] = changed
						return nil
					})
				}
				return nil
			}
			dropped := 0
			restoreCleanupDrop = func(context.Context, *Env, string) error {
				dropped++
				if mode == "drop-failure" {
					return errors.New("drop failed")
				}
				if mode == "record-during-drop" {
					return backup.UpdateRecords(func(s *backup.Records) error {
						changed := s.Restores["app"]
						changed.OperationID = "newer"
						s.Restores["app"] = changed
						return nil
					})
				}
				return nil
			}
			restoreAbortReload = func(context.Context, *Env, config.Cluster) error { return nil }
			restoreWriteMetrics = func(config.Cluster) {}
			name := backup.RestoringName("app")
			switch mode {
			case "replace-data", "replace-config":
				root := dataRoot
				if mode == "replace-config" {
					root = materialisationConfigRoot
				}
				path := filepath.Join(root, name)
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "replace-reservation":
				path := filepath.Join(ports, name)
				raw, err := os.ReadFile(path) //nolint:gosec // temporary test fixture
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil { //nolint:gosec // temporary test fixture
					t.Fatal(err)
				}
			case "missing-reservation":
				if err := os.Remove(filepath.Join(ports, name)); err != nil {
					t.Fatal(err)
				}
			case "layout":
				if err := os.WriteFile(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"), []byte("foreign"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unbound":
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					changed := s.Restores["app"]
					changed.Attempt.DataIdentity = backup.ArtifactIdentity{}
					s.Restores["app"] = changed
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			params, _ := json.Marshal(restoreDBParams{Database: "app"})
			_, err = handleRestoreAbort(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: params}})
			after, readErr := backup.LoadRecords()
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "success" || mode == "history-pruned" || mode == "mismatch-success" || mode == "recover-success" {
				if err != nil || dropped != 1 {
					t.Fatalf("abort: %v, drops %d", err, dropped)
				}
				if mode == "history-pruned" && !released {
					t.Fatal("retained job was not released")
				}
				if _, ok := after.Restores["app"]; ok {
					t.Fatal("record remains")
				}
				for _, path := range []string{filepath.Join(dataRoot, name), filepath.Join(materialisationConfigRoot, name), filepath.Join(ports, name)} {
					if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("artifact remains %s: %v", path, err)
					}
				}
			} else {
				if err == nil {
					t.Fatalf("%s accepted", mode)
				}
				if _, ok := after.Restores["app"]; !ok {
					t.Fatal("refusal lost evidence")
				}
				if mode != "drop-failure" && mode != "record-during-drop" && mode != "record-before-retire" && dropped != 0 {
					t.Fatalf("unexpected drop: %d", dropped)
				}
				if (mode == "record-changed" || mode == "record-during-drop" || mode == "record-before-retire") && !strings.Contains(after.Restores["app"].OperationID, "newer") {
					t.Fatal("newer record overwritten")
				}
			}
		})
	}
}
