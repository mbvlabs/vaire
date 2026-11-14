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
			_, root := materialisationFixture(t)
			if err := os.Mkdir(root, 0o700); err != nil {
				t.Fatal(err)
			}
			ports := filepath.Join(materialisationAncestorBase, "ports")
			if err := os.Mkdir(ports, 0o700); err != nil {
				t.Fatal(err)
			}
			oldRecords := backup.RecordsPath
			backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
			oldState, oldReservation := restoreAbsenceState, restoreAbsenceReservation
			t.Cleanup(func() {
				backup.RecordsPath, restoreAbsenceState, restoreAbsenceReservation = oldRecords, oldState, oldReservation
			})
			r := backup.Restore{ID: "job", Database: "app", InstanceID: "install", OperationID: "operation", Phase: backup.PhaseRestoring, MaterialisationPending: true}
			released := false
			if mode == "history-pruned" {
				oldRelease := releaseMaterialisationJob
				releaseMaterialisationJob = func(id string) { released = id == r.ID }
				t.Cleanup(func() { releaseMaterialisationJob = oldRelease })
			}
			if mode == "recover-success" {
				r.Recover = true
			}
			if mode == "failed-cleanup" {
				r.Phase = backup.PhaseFailed
			}
			if mode == "foreign-install" {
				r.InstanceID = "other"
			}
			if mode == "missing-operation" {
				r.OperationID = ""
			}
			other := backup.Restore{ID: "other", Database: "other", Phase: backup.PhaseReady}
			if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"], s.Restores["other"] = r, other; return nil }); err != nil {
				t.Fatal(err)
			}
			job := engine.JobRecord{ID: r.ID, Job: "restore", DB: "app", State: engine.JobFailed}
			if mode == "recover-success" {
				job.Job = "recover"
			}
			if mode == "queued" {
				job.State = engine.JobQueued
			}
			if mode == "running" {
				job.State = engine.JobRunning
			}
			if mode == "foreign-job" {
				job.DB = "other"
			}
			restoreAbsenceState = func() (engine.State, error) {
				if mode == "history-pruned" {
					return rotatedMaterialisationJob(job), nil
				}
				if mode == "unreadable-state" {
					return engine.State{}, errors.New("unreadable state")
				}
				if mode == "missing-job" {
					return engine.State{}, nil
				}
				if mode == "duplicate-job" {
					return engine.State{Jobs: []engine.JobRecord{job, job}}, nil
				}
				return engine.State{Jobs: []engine.JobRecord{job}}, nil
			}
			name := backup.RestoringName("app")
			switch mode {
			case "data":
				if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
					t.Fatal(err)
				}
			case "config":
				if err := os.Mkdir(filepath.Join(materialisationConfigRoot, name), 0o700); err != nil {
					t.Fatal(err)
				}
			case "reservation":
				if err := os.WriteFile(filepath.Join(ports, name), []byte("6500\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			restoreAbsenceReservation = func(ctx context.Context, owner string) (func(), error) {
				unlock, err := backup.LockTemporaryReservationAbsenceWithHost(ctx, ports, materialisationRecordUID, owner)
				if err != nil {
					return nil, err
				}
				if mode == "success" {
					probe, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
					defer cancel()
					_, err := backup.LockTemporaryReservationAbsenceWithHost(probe, ports, materialisationRecordUID, owner)
					if !errors.Is(err, context.DeadlineExceeded) {
						unlock()
						return nil, errors.New("allocator lock was not held")
					}
				}
				if mode == "record-changed" {
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
			cfg := config.Cluster{Server: config.Server{DataRoot: root}}
			if mode == "failed-cleanup" {
				err := clearFailedRestore(cfg, "app")
				if err != nil {
					t.Fatal(err)
				}
			} else {
				params, _ := json.Marshal(restoreDBParams{Database: "app"})
				_, err := handleRestoreAbort(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: params}})
				if (mode == "success" || mode == "history-pruned" || mode == "recover-success") && err != nil {
					t.Fatal(err)
				}
				if mode != "success" && mode != "history-pruned" && mode != "recover-success" && err == nil {
					t.Fatalf("%s accepted", mode)
				}
			}
			got, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.Restores["other"], other) {
				t.Fatal("unrelated restore changed")
			}
			cur, exists := got.Restores["app"]
			if mode == "success" || mode == "history-pruned" || mode == "recover-success" || mode == "failed-cleanup" {
				if exists {
					t.Fatal("never-started record remains")
				}
				if mode == "history-pruned" && !released {
					t.Fatal("retained job was not released")
				}
			} else if !exists || mode != "record-changed" && !reflect.DeepEqual(cur, r) || mode == "record-changed" && cur.OperationID != "replacement" {
				t.Fatalf("refused record modified: %+v", cur)
			}
			if _, err := os.Stat(filepath.Join(root, name)); mode == "data" && err != nil || mode != "data" && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("data path changed: %v", err)
			}
		})
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
	if err != nil {
		t.Fatal(err)
	}
	if err := dropKilledDrill(context.Background(), &jobEnv{Cluster: config.Cluster{Server: config.Server{DataRoot: root}}}, rs, "app"); err == nil {
		t.Fatal("name-only drill cleanup accepted")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("unbound drill removed: %v", err)
	}
	after, err := backup.LoadRecords()
	if err != nil || after.Drilling["app"] != name {
		t.Fatalf("drill record lost: %v", err)
	}
}
