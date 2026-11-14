package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
)

func TestDrillOwnedBirthAndCleanup(t *testing.T) {
	for _, mode := range []string{"success", "typed", "replace-data", "replace-config", "replace-reservation", "replace-record", "drop-failure"} {
		t.Run(mode, func(t *testing.T) {
			env, root, m := drillAttemptFixture(t)
			oldRegistration, oldDrop, oldAbsence := restoreCleanupRegistration, birthDropCluster, drillReservationAbsence
			t.Cleanup(func() {
				restoreCleanupRegistration, birthDropCluster, drillReservationAbsence = oldRegistration, oldDrop, oldAbsence
			})
			ports := filepath.Join(materialisationAncestorBase, "ports")
			restoreCleanupRegistration = func(string, string, string, int, int) error { return nil }
			drops := 0
			birthDropCluster = func(context.Context, *restoreBirth) error {
				drops++
				if mode == "drop-failure" {
					return errors.New("drop failed")
				}
				return nil
			}
			drillReservationAbsence = func(ctx context.Context, name string) (func(), error) {
				return backup.LockTemporaryReservationAbsenceWithHost(ctx, ports, materialisationRecordUID, name)
			}
			failure := attemptFailure("recovery_requirements_unmet")
			callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
				p := m.OwnershipPaths()
				before, err := backup.LoadRecords()
				if err != nil || before.DrillAttempts["app"].ReservationIdentity.Inode == 0 {
					t.Fatalf("reservation not bound before creation: %+v %v", before, err)
				}
				if err := m.Ownership.CreateData(ctx, p); err != nil {
					return err
				}
				data, err := backup.LoadRecords()
				if err != nil || data.DrillAttempts["app"].DataIdentity.Inode == 0 {
					t.Fatalf("data not bound at creation: %+v %v", data, err)
				}
				if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error { return nil }); err != nil {
					return err
				}
				if err := m.Ownership.CreateConfig(ctx, p); err != nil {
					return err
				}
				bound, err := backup.LoadRecords()
				if err != nil || bound.DrillAttempts["app"].ConfigIdentity.Inode == 0 {
					t.Fatalf("config not bound at creation: %+v %v", bound, err)
				}
				if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
					return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte("test"), 0o600)
				}); err != nil {
					return err
				}
				if mode == "typed" {
					return m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error { return failure })
				}
				return nil
			}
			attempt, started, retained, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
			if !started || attempt == nil {
				t.Fatalf("not started: %v", err)
			}
			birth := m.Ownership.(*restoreBirth)
			if mode == "typed" {
				if !retained || !errors.Is(err, failure) || attempt.Reason != "recovery_requirements_unmet" || drops != 0 {
					t.Fatalf("typed failure not retained: %+v %v", attempt, err)
				}
				return
			}
			if err != nil || retained {
				t.Fatalf("materialisation: %v", err)
			}
			switch mode {
			case "replace-data", "replace-config", "replace-reservation":
				path := filepath.Join(root, m.Name)
				switch mode {
				case "replace-config":
					path = filepath.Join(materialisationConfigRoot, m.Name)
				case "replace-reservation":
					path = filepath.Join(ports, m.Name)
				}
				if err := os.Rename(path, path+"-old"); err != nil {
					t.Fatal(err)
				}
				if mode == "replace-reservation" {
					err = os.WriteFile(path, []byte("6001\n"), 0o600)
				} else {
					err = os.Mkdir(path, 0o700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "replace-record":
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					a := s.DrillAttempts["app"]
					a.OperationID = "other"
					s.DrillAttempts["app"] = a
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			err = finishDrill(context.Background(), birth, attempt, "app", m.Name)
			if mode == "success" {
				if err != nil || drops != 1 {
					t.Fatalf("owned cleanup: drops=%d err=%v", drops, err)
				}
				records, _ := backup.LoadRecords()
				if _, exists := records.DrillAttempts["app"]; exists || records.Drilling["app"] != "" {
					t.Fatal("cleaned attempt remains")
				}
			} else {
				if err == nil || (mode != "drop-failure" && drops != 0) {
					t.Fatalf("unsafe cleanup: drops=%d err=%v", drops, err)
				}
				records, _ := backup.LoadRecords()
				if _, exists := records.DrillAttempts["app"]; !exists {
					t.Fatal("failed cleanup lost evidence")
				}
			}
			env.lease.finish()
			if err := withLockedJobTarget(env, m.Name, func() error { return nil }); err == nil {
				t.Fatal("released lease remained usable")
			}
		})
	}
}
