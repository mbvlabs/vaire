package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestRestoreAbortNeverStartedAbsence(t *testing.T) {
	for _, mode := range []string{"success", "history-pruned", "recover-success", "failed-cleanup", "missing-job", "duplicate-job", "unreadable-state", "queued", "running", "foreign-job", "foreign-install", "missing-operation", "data", "config", "reservation", "record-changed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRestoreAbsenceFixture(t, mode)
			f.stage(t)
			f.abort(t)
			f.assert(t)
		})
	}
}

// restoreAbsenceFixture is one never-started restore record, its unrelated
// neighbour and the paths the abort inspects.
type restoreAbsenceFixture struct {
	mode     string
	name     string
	root     string
	ports    string
	record   backup.Restore
	other    backup.Restore
	job      engine.JobRecord
	released bool
}

// newRestoreAbsenceFixture installs the records path and the mode-specific
// record fields the abort reads.
func newRestoreAbsenceFixture(t *testing.T, mode string) *restoreAbsenceFixture {
	t.Helper()
	_, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	ports := filepath.Join(materialisationAncestorBase, "ports")
	mustNoErr(t, os.Mkdir(ports, 0o700))
	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	oldState, oldReservation := restoreAbsenceState, restoreAbsenceReservation
	t.Cleanup(func() {
		backup.RecordsPath, restoreAbsenceState, restoreAbsenceReservation = oldRecords, oldState, oldReservation
	})
	f := &restoreAbsenceFixture{
		mode:   mode,
		name:   backup.RestoringName("app"),
		root:   root,
		ports:  ports,
		other:  backup.Restore{ID: "other", Database: "other", Phase: backup.PhaseReady},
		record: backup.Restore{ID: "job", Database: "app", InstanceID: "install", OperationID: "operation", Phase: backup.PhaseRestoring, MaterialisationPending: true},
	}
	if mode == "history-pruned" {
		oldRelease := releaseMaterialisationJob
		releaseMaterialisationJob = func(id string) { f.released = id == f.record.ID }
		t.Cleanup(func() { releaseMaterialisationJob = oldRelease })
	}
	if mode == "recover-success" {
		f.record.Recover = true
	}
	if mode == "failed-cleanup" {
		f.record.Phase = backup.PhaseFailed
	}
	if mode == "foreign-install" {
		f.record.InstanceID = "other"
	}
	if mode == "missing-operation" {
		f.record.OperationID = ""
	}
	return f
}

// stage publishes the record and job state, substitutes the state and
// reservation reads, and lays down the artifact the mode leaves behind.
func (f *restoreAbsenceFixture) stage(t *testing.T) {
	t.Helper()
	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"], s.Restores["other"] = f.record, f.other; return nil }))
	f.job = engine.JobRecord{ID: f.record.ID, Job: "restore", DB: "app", State: engine.JobFailed}
	if f.mode == "recover-success" {
		f.job.Job = "recover"
	}
	if f.mode == "queued" {
		f.job.State = engine.JobQueued
	}
	if f.mode == "running" {
		f.job.State = engine.JobRunning
	}
	if f.mode == "foreign-job" {
		f.job.DB = "other"
	}
	restoreAbsenceState = f.state
	switch f.mode {
	case "data":
		mustNoErr(t, os.Mkdir(filepath.Join(f.root, f.name), 0o700))
	case "config":
		mustNoErr(t, os.Mkdir(filepath.Join(materialisationConfigRoot, f.name), 0o700))
	case "reservation":
		mustNoErr(t, os.WriteFile(filepath.Join(f.ports, f.name), []byte("6500\n"), 0o600))
	}
	restoreAbsenceReservation = f.reservation
}

// state serves the job state the mode needs.
func (f *restoreAbsenceFixture) state() (engine.State, error) {
	switch f.mode {
	case "history-pruned":
		return rotatedMaterialisationJob(f.job), nil
	case "unreadable-state":
		return engine.State{}, errors.New("unreadable state")
	case "missing-job":
		return engine.State{}, nil
	case "duplicate-job":
		return engine.State{Jobs: []engine.JobRecord{f.job, f.job}}, nil
	}
	return engine.State{Jobs: []engine.JobRecord{f.job}}, nil
}

// reservation takes the real allocator lock and applies the mode's probe or
// record change while it is held.
func (f *restoreAbsenceFixture) reservation(ctx context.Context, owner string) (func(), error) {
	unlock, err := backup.LockTemporaryReservationAbsenceWithHost(ctx, f.ports, materialisationRecordUID, owner)
	if err != nil {
		return nil, err
	}
	if f.mode == "success" {
		probe, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		_, err := backup.LockTemporaryReservationAbsenceWithHost(probe, f.ports, materialisationRecordUID, owner)
		if !errors.Is(err, context.DeadlineExceeded) {
			unlock()
			return nil, errors.New("allocator lock was not held")
		}
	}
	if f.mode == "record-changed" {
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			cur := s.Restores["app"]
			cur.OperationID = "replacement"
			s.Restores["app"] = cur
			return nil
		}); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

// abort runs the cleanup under test for the mode.
func (f *restoreAbsenceFixture) abort(t *testing.T) {
	t.Helper()
	cfg := config.Cluster{Server: config.Server{DataRoot: f.root}}
	if f.mode == "failed-cleanup" {
		mustNoErr(t, clearFailedRestore(context.Background(), cfg, "app"))
		return
	}
	params, _ := json.Marshal(restoreDBParams{Database: "app"})
	_, err := handleRestoreAbort(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: params}})
	accepted := f.mode == "success" || f.mode == "history-pruned" || f.mode == "recover-success"
	reject(t, accepted && err != nil, err)
	rejectf(t, !accepted && err == nil, "%s accepted", f.mode)
}

// assert checks the records, the release hook and the artifact path after the
// abort.
func (f *restoreAbsenceFixture) assert(t *testing.T) {
	t.Helper()
	got, err := backup.LoadRecords()
	reject(t, err != nil, err)
	reject(t, !reflect.DeepEqual(got.Restores["other"], f.other), "unrelated restore changed")
	cur, exists := got.Restores["app"]
	if f.mode == "success" || f.mode == "history-pruned" || f.mode == "recover-success" || f.mode == "failed-cleanup" {
		if exists {
			t.Fatal("never-started record remains")
		}
		reject(t, f.mode == "history-pruned" && !f.released, "retained job was not released")
	} else if !exists || f.mode != "record-changed" && !reflect.DeepEqual(cur, f.record) || f.mode == "record-changed" && cur.OperationID != "replacement" {
		t.Fatalf("refused record modified: %+v", cur)
	}
	if _, err := os.Stat(filepath.Join(f.root, f.name)); f.mode == "data" && err != nil || f.mode != "data" && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("data path changed: %v", err)
	}
}

func TestKilledDrillWithoutBirthIsRetained(t *testing.T) {
	_, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
	name := backup.DrillName("app")
	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error { s.Drilling["app"] = name; return nil }); err != nil {
		t.Fatal(err)
	}
	rs, err := backup.LoadRecords()
	reject(t, err != nil, err)
	if err := dropKilledDrill(context.Background(), &jobEnv{Cluster: config.Cluster{Server: config.Server{DataRoot: root}}}, rs, "app"); err == nil {
		t.Fatal("name-only drill cleanup accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unbound drill removed: %v", err)
	}
	after, err := backup.LoadRecords()
	rejectf(t, err != nil || after.Drilling["app"] != name, "drill record lost: %v", err)
}
