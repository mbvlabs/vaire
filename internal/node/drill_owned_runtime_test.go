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
			runDrillOwnedBirth(t, mode)
		})
	}
}

type drillBirthState struct {
	mode    string
	ports   string
	drops   int
	failure error
}

func runDrillOwnedBirth(t *testing.T, mode string) {
	t.Helper()
	s, env, root, m := stubDrillBirth(t, mode)
	attempt, started, retained, err := materialiseDrill(context.Background(), env, m, nil, backup.RestoreOptions{}, "app-stanza")
	rejectf(t, !started || attempt == nil, "not started: %v", err)
	birth := mustAs[*restoreBirth](t, m.Ownership)
	if mode == "typed" {
		if !retained || !errors.Is(err, s.failure) || attempt.Reason != "recovery_requirements_unmet" || s.drops != 0 {
			t.Fatalf("typed failure not retained: %+v %v", attempt, err)
		}
		return
	}
	rejectf(t, err != nil || retained, "materialisation: %v", err)
	if err := replaceDrillOwnedPath(t, mode, root, s.ports, m.Name); err != nil {
		t.Fatal(err)
	}
	err = finishDrill(context.Background(), birth, attempt, "app", m.Name)
	assertDrillBirthCleanup(t, mode, s.drops, err)
	env.lease.finish()
	if err := withLockedJobTarget(env, m.Name, func() error { return nil }); err == nil {
		t.Fatal("released lease remained usable")
	}
}

func stubDrillBirth(t *testing.T, mode string) (*drillBirthState, *jobEnv, string, *backup.Materialised) {
	t.Helper()
	env, root, m := drillAttemptFixture(t)
	s := &drillBirthState{mode: mode, ports: filepath.Join(materialisationAncestorBase, "ports"), failure: attemptFailure("recovery_requirements_unmet")}
	oldRegistration, oldDrop, oldAbsence := restoreCleanupRegistration, birthDropCluster, drillReservationAbsence
	t.Cleanup(func() {
		restoreCleanupRegistration, birthDropCluster, drillReservationAbsence = oldRegistration, oldDrop, oldAbsence
	})
	restoreCleanupRegistration = func(string, string, string, int, int) error { return nil }
	birthDropCluster = func(context.Context, *restoreBirth) error {
		s.drops++
		if mode == "drop-failure" {
			return errors.New("drop failed")
		}
		return nil
	}
	drillReservationAbsence = func(ctx context.Context, name string) (func(), error) {
		return backup.LockTemporaryReservationAbsenceWithHost(ctx, s.ports, materialisationRecordUID, name)
	}
	callerRestoreMaterialised = func(ctx context.Context, m *backup.Materialised, _ backup.Driver, _ backup.RestoreOptions) error {
		return populateDrillBirth(ctx, t, s, m)
	}
	return s, env, root, m
}

// populateDrillBirth records each owned artifact as it is created and checks
// that the reservation, data and config identities are bound in that order.
func populateDrillBirth(ctx context.Context, t *testing.T, s *drillBirthState, m *backup.Materialised) error {
	p := m.OwnershipPaths()
	before, err := backup.LoadRecords()
	rejectf(t, err != nil || before.DrillAttempts["app"].ReservationIdentity.Inode == 0, "reservation not bound before creation: %+v %v", before, err)
	if err := m.Ownership.CreateData(ctx, p); err != nil {
		return err
	}
	data, err := backup.LoadRecords()
	rejectf(t, err != nil || data.DrillAttempts["app"].DataIdentity.Inode == 0, "data not bound at creation: %+v %v", data, err)
	if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error { return nil }); err != nil {
		return err
	}
	if err := m.Ownership.CreateConfig(ctx, p); err != nil {
		return err
	}
	bound, err := backup.LoadRecords()
	rejectf(t, err != nil || bound.DrillAttempts["app"].ConfigIdentity.Inode == 0, "config not bound at creation: %+v %v", bound, err)
	if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
		return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte("test"), 0o600)
	}); err != nil {
		return err
	}
	if s.mode == "typed" {
		return m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error { return s.failure })
	}
	return nil
}

// replaceDrillOwnedPath swaps the owned artifact a mode expects cleanup to
// refuse, or rebinds the drill record to another operation.
func replaceDrillOwnedPath(t *testing.T, mode, root, ports, name string) error {
	t.Helper()
	switch mode {
	case "replace-data", "replace-config", "replace-reservation":
		path := filepath.Join(root, name)
		switch mode {
		case "replace-config":
			path = filepath.Join(materialisationConfigRoot, name)
		case "replace-reservation":
			path = filepath.Join(ports, name)
		}
		if err := os.Rename(path, path+"-old"); err != nil {
			return err
		}
		if mode == "replace-reservation" {
			return os.WriteFile(path, []byte("6001\n"), 0o600)
		}
		return os.Mkdir(path, 0o700)
	case "replace-record":
		return backup.UpdateRecords(func(s *backup.Records) error {
			a := s.DrillAttempts["app"]
			a.OperationID = "other"
			s.DrillAttempts["app"] = a
			return nil
		})
	}
	return nil
}

func assertDrillBirthCleanup(t *testing.T, mode string, drops int, err error) {
	t.Helper()
	if mode == "success" {
		if err != nil || drops != 1 {
			t.Fatalf("owned cleanup: drops=%d err=%v", drops, err)
		}
		records, _ := backup.LoadRecords()
		if _, exists := records.DrillAttempts["app"]; exists || records.Drilling["app"] != "" {
			t.Fatal("cleaned attempt remains")
		}
		return
	}
	rejectf(t, err == nil || (mode != "drop-failure" && drops != 0), "unsafe cleanup: drops=%d err=%v", drops, err)
	records, _ := backup.LoadRecords()
	if _, exists := records.DrillAttempts["app"]; !exists {
		t.Fatal("failed cleanup lost evidence")
	}
}
