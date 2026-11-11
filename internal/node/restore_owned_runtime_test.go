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
			f := newRestoreOwnedAbort(t, mode)
			err := f.restore()
			if f.assertEarlyOutcome(t, err) {
				return
			}
			f.requireRetainedBirth(t, err)
			f.stageAbort(t)
			f.abort(t)
		})
	}
}

// restoreOwnedAbort is one materialised restore record driven through the
// abort, with the host-facing effects replaced.
type restoreOwnedAbort struct {
	mode     string
	dataRoot string
	ports    string
	cfg      config.Cluster
	env      *jobEnv
	record   backup.Restore
	reason   string
	failure  error
	released bool
	drops    int
}

// newRestoreOwnedAbort lays down the fixture, installs the restore driver and
// leaves the record in its materialising state.
func newRestoreOwnedAbort(t *testing.T, mode string) *restoreOwnedAbort {
	useExtensionFileFixture(t)
	t.Helper()
	f := &restoreOwnedAbort{
		mode: mode,
		cfg:  config.DefaultCluster(),
		record: backup.Restore{ID: "job", Database: "app", Kind: backup.TargetLatest, InstanceID: "install", OperationID: "operation",
			Phase: backup.PhaseRestoring, MaterialisationPending: true},
	}
	if mode == "history-pruned" {
		oldRelease := releaseMaterialisationJob
		releaseMaterialisationJob = func(id string) { f.released = id == "job" }
		t.Cleanup(func() { releaseMaterialisationJob = oldRelease })
	}
	_, dataRoot := materialisationFixture(t)
	f.dataRoot = dataRoot
	f.ports = filepath.Join(materialisationAncestorBase, "ports")
	for _, dir := range []string{dataRoot, f.ports} {
		mustNoErr(t, os.Mkdir(dir, 0o700))
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
	f.cfg.Server.DataRoot = dataRoot
	f.cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5432, Stanza: "app-abcdef", Size: "small"}}
	noExtensions := []string{}
	f.cfg.Clusters[0].RecoveryExtensions = &noExtensions
	f.env = &jobEnv{ID: "job", OperationID: "operation", installID: "install", Job: "restore", DB: "app", Cluster: f.cfg,
		Params: json.RawMessage(`{"database":"app","kind":"latest"}`)}
	f.env.lease = newLockedJobLease(f.env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(f.env.lease.finish)
	if mode == "recover-success" {
		f.env.Job, f.env.Params = "recover", json.RawMessage(`{"database":"app"}`)
		f.record.Recover, f.record.Promote = true, true
		f.env.lease.finish()
		f.env.lease = newLockedJobLease(f.env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
		t.Cleanup(f.env.lease.finish)
	}
	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = f.record; return nil }))
	materialisationHostInfo = func(string) (hostinfo.Info, error) { return hostinfo.Info{RAMMiB: 8192, VCPU: 4}, nil }
	callerRecoveredTo = func(context.Context, *backup.Materialised) string { return "" }
	restoreApplyPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error { return nil }
	restoreWriteMetrics = func(config.Cluster) {}
	jobRuntimeHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	callerNewRestoreOwned = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: f.ports, ConfigRoot: materialisationConfigRoot, UID: materialisationRecordUID,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
		})
	}
	f.reason = "recovery_requirements_unmet"
	if mode == "mismatch-success" {
		f.reason = "recovery_parameter_mismatch"
	}
	f.failure = attemptFailure(f.reason)
	callerRestoreMaterialised = ownedRestoreMaterialisation(mode, f.failure)
	return f
}

// ownedRestoreMaterialisation is the restore driver body: it creates the owned
// data and config layout, applies the mode's record change or early return,
// and reaches the typed inspection failure.
func ownedRestoreMaterialisation(mode string, failure error) func(context.Context, *backup.Materialised, backup.Driver, backup.RestoreOptions) error {
	return func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
		p := m.OwnershipPaths()
		if err := m.Ownership.CreateData(ctx, p); err != nil {
			return err
		}
		if err := restoreOwnedData(ctx, m, p, mode); err != nil {
			return err
		}
		if err := m.Ownership.CreateConfig(ctx, p); err != nil {
			return err
		}
		if err := registerOwnedConfig(ctx, m, p); err != nil {
			return err
		}
		if mode == "no-promote-ready" {
			return passOwnedRegistration(ctx, m, p)
		}
		if mode == "stale-invocation" || mode == "stale-record" {
			if err := overwriteOwnedRecord(mode); err != nil {
				return err
			}
		}
		return m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error { return failure })
	}
}

// restoreOwnedData writes the data sentinel inside the ownership guard, and
// fails the restore ordinarily in that mode.
func restoreOwnedData(ctx context.Context, m *backup.Materialised, p backup.MaterialisationPaths, mode string) error {
	return m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error {
		if err := os.WriteFile(filepath.Join(p.DataDir, "sentinel"), []byte("keep"), 0o600); err != nil {
			return err
		}
		if mode == "ordinary-failure" {
			return errors.New("ordinary restore failure")
		}
		return nil
	})
}

// registerOwnedConfig writes the config that registration later verifies.
func registerOwnedConfig(ctx context.Context, m *backup.Materialised, p backup.MaterialisationPaths) error {
	return m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
		return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte(fmt.Sprintf("%s %d", p.DataDir, p.Port)), 0o600)
	})
}

// passOwnedRegistration runs the prepare, inspect and start actions without
// promoting, so the record stays ready but uninspected.
func passOwnedRegistration(ctx context.Context, m *backup.Materialised, p backup.MaterialisationPaths) error {
	for _, action := range []backup.MaterialisationAction{backup.MaterialisationPrepare, backup.MaterialisationInspect, backup.MaterialisationStart} {
		if err := m.Ownership.Guard(ctx, p, action, func() error { return nil }); err != nil {
			return err
		}
	}
	return nil
}

// overwriteOwnedRecord replaces the invocation's identity mid-restore.
func overwriteOwnedRecord(mode string) error {
	return backup.UpdateRecords(func(s *backup.Records) error {
		changed := s.Restores["app"]
		if mode == "stale-invocation" {
			changed.OperationID = "replacement"
		} else {
			changed.Target = "replacement target"
		}
		s.Restores["app"] = changed
		return nil
	})
}

// restore runs the restore job, or the recovery follow-up in recover mode.
func (f *restoreOwnedAbort) restore() error {
	if f.mode == "stale-lease" {
		f.env.lease.finish()
	}
	if f.mode == "recover-success" {
		_, _, attempt, cause := materialiseRestore(context.Background(), f.env, (recoverParams{Database: "app"}).restore())
		return errors.Join(cause, failRestoreChecked(f.env, cause, attempt))
	}
	_, err := jobRestore(context.Background(), f.env)
	return err
}

// assertEarlyOutcome reports whether the mode ends before the retained-birth
// abort, and checks its terminal state.
func (f *restoreOwnedAbort) assertEarlyOutcome(t *testing.T, err error) bool {
	t.Helper()
	switch f.mode {
	case "no-promote-ready":
		f.assertReadyWithoutInspection(t, err)
		return true
	case "stale-lease":
		f.assertStaleLease(t, err)
		return true
	case "stale-invocation", "stale-record":
		f.assertStaleRecord(t, err)
		return true
	case "ordinary-failure":
		f.assertOrdinaryFailure(t, err)
		return true
	}
	return false
}

// assertReadyWithoutInspection checks the inspection-only restore commits a
// ready record with no attempt or pending materialisation.
func (f *restoreOwnedAbort) assertReadyWithoutInspection(t *testing.T, err error) {
	t.Helper()
	mustNoErr(t, err)
	got, err := backup.LoadRecords()
	if err != nil || got.Restores["app"].Phase != backup.PhaseReady || got.Restores["app"].Attempt != nil ||
		got.Restores["app"].MaterialisationPending || got.Restores["app"].Port != config.TemporaryPortMin {
		t.Fatalf("inspection restore not ready: %+v %v", got.Restores["app"], err)
	}
}

// assertStaleLease checks a finished lease is refused without allocating.
func (f *restoreOwnedAbort) assertStaleLease(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("stale lease accepted")
	}
	if _, err := os.Lstat(filepath.Join(f.ports, "app__restoring")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale lease allocated a reservation: %v", err)
	}
	got, err := backup.LoadRecords()
	rejectf(t, err != nil || got.Restores["app"].Phase != backup.PhaseRestoring || got.Restores["app"].Attempt != nil, "stale lease changed record: %+v %v", got.Restores["app"], err)
}

// assertStaleRecord checks the mid-restore replacement is refused and kept.
func (f *restoreOwnedAbort) assertStaleRecord(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("stale invocation accepted")
	}
	got, _ := backup.LoadRecords()
	if (f.mode == "stale-invocation" && got.Restores["app"].OperationID != "replacement") ||
		(f.mode == "stale-record" && got.Restores["app"].Target != "replacement target") || got.Restores["app"].Phase != backup.PhaseRestoring {
		t.Fatalf("new record changed: %+v", got.Restores["app"])
	}
}

// assertOrdinaryFailure checks an untyped restore failure cleans every owned
// artifact and leaves the record failed without an attempt.
func (f *restoreOwnedAbort) assertOrdinaryFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "ordinary restore failure") {
		t.Fatalf("ordinary failure lost: %v", err)
	}
	got, err := backup.LoadRecords()
	rejectf(t, err != nil || got.Restores["app"].Phase != backup.PhaseFailed || got.Restores["app"].Attempt != nil || !got.Restores["app"].MaterialisationCleaned, "ordinary cleanup state: %+v %v", got.Restores["app"], err)
	for _, path := range []string{filepath.Join(f.dataRoot, "app__restoring"), filepath.Join(f.ports, "app__restoring")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ordinary cleanup left %s: %v", path, err)
		}
	}
}

// requireRetainedBirth checks the typed failure retained its birth binding and
// releases the invocation lease before the abort.
func (f *restoreOwnedAbort) requireRetainedBirth(t *testing.T, err error) {
	t.Helper()
	rejectf(t, !errors.Is(err, f.failure), "typed failure lost: %v", err)
	got, err := backup.LoadRecords()
	reject(t, err != nil, err)
	attempt := got.Restores["app"].Attempt
	if got.Restores["app"].Phase != backup.PhaseFailed || attempt == nil ||
		attempt.ArtifactStage != materialisationRegistered || attempt.DataIdentity.Inode == 0 ||
		attempt.ConfigIdentity.Inode == 0 || attempt.ReservationIdentity.Inode == 0 || attempt.Reason != f.reason {
		t.Fatalf("missing retained birth binding: %+v", got.Restores["app"])
	}
	f.env.lease.finish()
}

// stageAbort installs the abort's state, reservation and drop reads, and
// leaves the mode's artifact mutation in place.
func (f *restoreOwnedAbort) stageAbort(t *testing.T) {
	t.Helper()
	restoreAbsenceState = f.abortState
	restoreCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
		return backup.CleanupRestoreReservationWithHost(ctx, f.ports, materialisationRecordUID, name, port, id, effect)
	}
	restoreCleanupAbsence = f.absence
	calls := 0
	restoreCleanupRegistration = func(name, _, root string, _, port int) error {
		calls++
		raw, err := os.ReadFile(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"))
		if err != nil || string(raw) != fmt.Sprintf("%s %d", filepath.Join(root, name), port) {
			return errors.New("wrong registration layout")
		}
		if f.mode == "record-changed" && calls == 2 {
			return backup.UpdateRecords(func(s *backup.Records) error {
				changed := s.Restores["app"]
				changed.OperationID = "newer"
				s.Restores["app"] = changed
				return nil
			})
		}
		return nil
	}
	restoreCleanupDrop = f.drop
	restoreAbortReload = func(context.Context, *Env, config.Cluster) error { return nil }
	restoreWriteMetrics = func(config.Cluster) {}
	f.mutateArtifacts(t)
}

// abortState serves the job state the abort's absence check reads.
func (f *restoreOwnedAbort) abortState() (engine.State, error) {
	db := "app"
	if f.mode == "foreign-job" {
		db = "other"
	}
	job := engine.JobRecord{ID: "job", Job: f.env.Job, DB: db, State: engine.JobFailed}
	if f.mode == "history-pruned" {
		return rotatedMaterialisationJob(job), nil
	}
	return engine.State{Jobs: []engine.JobRecord{job}}, nil
}

// absence takes the real reservation lock and applies the record change the
// mode needs while it is held.
func (f *restoreOwnedAbort) absence(ctx context.Context, name string, port int, present bool) (func(), error) {
	unlock, err := backup.LockForkReservationWithHost(ctx, f.ports, materialisationRecordUID, name, port, present)
	if err == nil && f.mode == "record-before-retire" {
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

// drop records the mode's database drop or its failure.
func (f *restoreOwnedAbort) drop(context.Context, *Env, string) error {
	f.drops++
	if f.mode == "drop-failure" {
		return errors.New("drop failed")
	}
	if f.mode == "record-during-drop" {
		return backup.UpdateRecords(func(s *backup.Records) error {
			changed := s.Restores["app"]
			changed.OperationID = "newer"
			s.Restores["app"] = changed
			return nil
		})
	}
	return nil
}

// mutateArtifacts replaces or removes the owned artifact the mode retires.
func (f *restoreOwnedAbort) mutateArtifacts(t *testing.T) {
	t.Helper()
	name := backup.RestoringName("app")
	switch f.mode {
	case "replace-data", "replace-config":
		root := f.dataRoot
		if f.mode == "replace-config" {
			root = materialisationConfigRoot
		}
		path := filepath.Join(root, name)
		mustNoErr(t, os.Rename(path, path+"-old"))
		mustNoErr(t, os.Mkdir(path, 0o700))
	case "replace-reservation":
		path := filepath.Join(f.ports, name)
		raw, err := os.ReadFile(path)
		reject(t, err != nil, err)
		mustNoErr(t, os.Rename(path, path+"-old"))
		mustNoErr(t, os.WriteFile(path, raw, 0o600))
	case "missing-reservation":
		mustNoErr(t, os.Remove(filepath.Join(f.ports, name)))
	case "layout":
		mustNoErr(t, os.WriteFile(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"), []byte("foreign"), 0o600))
	case "unbound":
		mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
			changed := s.Restores["app"]
			changed.Attempt.DataIdentity = backup.ArtifactIdentity{}
			s.Restores["app"] = changed
			return nil
		}))
	}
}

// abort runs the abort handler and checks the mode's terminal state.
func (f *restoreOwnedAbort) abort(t *testing.T) {
	t.Helper()
	name := backup.RestoringName("app")
	params, _ := json.Marshal(restoreDBParams{Database: "app"})
	_, err := handleRestoreAbort(context.Background(), &Env{Cluster: f.cfg, Req: rpc.Request{Params: params}})
	after, readErr := backup.LoadRecords()
	reject(t, readErr != nil, readErr)
	if f.mode == "success" || f.mode == "history-pruned" || f.mode == "mismatch-success" || f.mode == "recover-success" {
		f.assertAbortSucceeded(t, err, after, name)
		return
	}
	f.assertAbortRefused(t, err, after)
}

// assertAbortSucceeded checks an accepted abort dropped the database, cleared
// the record and removed every owned artifact.
func (f *restoreOwnedAbort) assertAbortSucceeded(t *testing.T, err error, after backup.Records, name string) {
	t.Helper()
	if err != nil || f.drops != 1 {
		t.Fatalf("abort: %v, drops %d", err, f.drops)
	}
	reject(t, f.mode == "history-pruned" && !f.released, "retained job was not released")
	if _, ok := after.Restores["app"]; ok {
		t.Fatal("record remains")
	}
	for _, path := range []string{filepath.Join(f.dataRoot, name), filepath.Join(materialisationConfigRoot, name), filepath.Join(f.ports, name)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact remains %s: %v", path, err)
		}
	}
}

// assertAbortRefused checks a refused abort kept its evidence and did not drop
// the database or overwrite a newer record.
func (f *restoreOwnedAbort) assertAbortRefused(t *testing.T, err error, after backup.Records) {
	t.Helper()
	rejectf(t, err == nil, "%s accepted", f.mode)
	if _, ok := after.Restores["app"]; !ok {
		t.Fatal("refusal lost evidence")
	}
	rejectf(t, f.mode != "drop-failure" && f.mode != "record-during-drop" && f.mode != "record-before-retire" && f.drops != 0, "unexpected drop: %d", f.drops)
	reject(t, (f.mode == "record-changed" || f.mode == "record-during-drop" || f.mode == "record-before-retire") && !strings.Contains(after.Restores["app"].OperationID, "newer"), "newer record overwritten")
}
