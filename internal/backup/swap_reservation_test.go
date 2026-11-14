package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSwapReservationMoveAndRetry(t *testing.T) {
	m, dir := forkReservationFixture(t)
	uid := uint32(os.Getuid()) //nolint:gosec // test UID
	var saved ReservationIdentity
	id, err := moveSwapReservation(context.Background(), dir, uid, m.Name, "aside", m.Port, saved, func(id ReservationIdentity) error {
		saved = id
		return nil
	})
	if err != nil || id != saved || id.Inode == 0 {
		t.Fatalf("move: id=%+v saved=%+v err=%v", id, saved, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, m.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source remains: %v", err)
	}
	if _, err := moveSwapReservation(context.Background(), dir, uid, m.Name, "aside", m.Port, saved, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
}

func TestSwapReservationInterruptedLink(t *testing.T) {
	m, dir := forkReservationFixture(t)
	id, err := m.ForkReservationIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Link(filepath.Join(dir, m.Name), filepath.Join(dir, "aside")); err != nil {
		t.Fatal(err)
	}
	if _, err := moveSwapReservation(context.Background(), dir, uint32(os.Getuid()), m.Name, "aside", m.Port, id, nil); err != nil { //nolint:gosec // test UID
		t.Fatal(err)
	}
}

func TestSwapReservationRefusesCollisionAndUnboundRetry(t *testing.T) {
	m, dir := forkReservationFixture(t)
	uid := uint32(os.Getuid()) //nolint:gosec // test UID
	path := filepath.Join(dir, "aside")
	if err := os.WriteFile(path, []byte("other"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := moveSwapReservation(context.Background(), dir, uid, m.Name, "aside", m.Port, ReservationIdentity{}, func(ReservationIdentity) error { return nil }); err == nil {
		t.Fatal("accepted collision")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // test fixture
	if err != nil || string(raw) != "other" {
		t.Fatalf("destination changed: %q %v", raw, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := moveSwapReservation(context.Background(), dir, uid, m.Name, "aside", m.Port, ReservationIdentity{}, nil); err == nil {
		t.Fatal("accepted unbound retry")
	}
}

func TestSwapReservationPrepareFailureLeavesSource(t *testing.T) {
	m, dir := forkReservationFixture(t)
	_, err := moveSwapReservation(context.Background(), dir, uint32(os.Getuid()), m.Name, "aside", m.Port, ReservationIdentity{}, func(ReservationIdentity) error { return errors.New("persist failed") }) //nolint:gosec // test UID
	if err == nil {
		t.Fatal("accepted failed preparation")
	}
	if _, err := os.Lstat(filepath.Join(dir, m.Name)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "aside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination created: %v", err)
	}
}

func TestSwapReservationRefusesReplacementDuringPrepare(t *testing.T) {
	m, dir := forkReservationFixture(t)
	path := filepath.Join(dir, m.Name)
	_, err := moveSwapReservation(context.Background(), dir, uint32(os.Getuid()), m.Name, "aside", m.Port, ReservationIdentity{}, func(ReservationIdentity) error { //nolint:gosec // test UID
		if err := os.Remove(path); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("20000\n"), 0o600)
	})
	if err == nil {
		t.Fatal("accepted replacement")
	}
	if _, err := os.Lstat(filepath.Join(dir, "aside")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("destination created: %v", err)
	}
}
