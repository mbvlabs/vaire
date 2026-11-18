package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/engine"
)

type testForkGuard struct{}

func (testForkGuard) CreateData(context.Context, MaterialisationPaths) error   { return nil }
func (testForkGuard) CreateConfig(context.Context, MaterialisationPaths) error { return nil }
func (testForkGuard) Guard(_ context.Context, _ MaterialisationPaths, _ MaterialisationAction, effect func() error) error {
	return effect()
}

type lockingForkGuard struct {
	dir string
}

func (g lockingForkGuard) CreateData(context.Context, MaterialisationPaths) error   { return nil }
func (g lockingForkGuard) CreateConfig(context.Context, MaterialisationPaths) error { return nil }
func (g lockingForkGuard) Guard(ctx context.Context, _ MaterialisationPaths, _ MaterialisationAction, effect func() error) error {
	for i := range 2 {
		lock, err := openForkAllocator(ctx, g.dir, uint32(os.Getuid()))
		if err != nil {
			return err
		}

		lock.close()

		if i == 0 {
			if err := effect(); err != nil {
				return err
			}
		}
	}

	return nil
}

func forkReservationFixture(t *testing.T) (*Materialised, string) {
	t.Helper()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	m := &Materialised{Name: "fork", Source: "source", Data: filepath.Join(t.TempDir(), "fork"), Ownership: testForkGuard{}}

	r, err := reserveForkAt(context.Background(), dir, uint32(os.Getuid()), m, func() map[int]bool { return map[int]bool{} }, func(int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}

	m.reservation, m.Port = r, r.port

	t.Cleanup(func() {
		if r.file != nil {
			_ = r.file.Close()
		}
	})

	return m, dir
}

func TestForkReservationRefusesExistingOwner(t *testing.T) {
	m, dir := forkReservationFixture(t)

	_, err := reserveForkAt(context.Background(), dir, uint32(os.Getuid()), &Materialised{Name: m.Name}, func() map[int]bool { return map[int]bool{} }, func(int) bool { return false })
	if err == nil {
		t.Fatal("existing owner accepted")
	}

	raw, err := os.ReadFile(filepath.Join(dir, m.Name))
	if err != nil || strings.TrimSpace(string(raw)) != fmt.Sprint(m.Port) {
		t.Fatalf("reservation changed: %q %v", raw, err)
	}
}

// damageForkReservation applies one reservation change and returns the
// materialisation whose capability the test then checks.
func damageForkReservation(t *testing.T, damage, dir, path string, m *Materialised) *Materialised {
	t.Helper()

	switch damage {
	case "replacement", "symlink":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}

		if damage == "replacement" {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
			t.Fatal(err)
		}
	case "foreign-mode":
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	case "port":
		m.Port++
	case "file-port":
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port+1)), 0o600); err != nil {
			t.Fatal(err)
		}
	case "duplicate-port":
		if err := os.WriteFile(filepath.Join(dir, "foreign"), []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
			t.Fatal(err)
		}
	case "copied":
		copied := *m
		return &copied
	}

	return m
}

func TestForkReservationRefusesChangedCapability(t *testing.T) {
	for _, damage := range []string{"replacement", "symlink", "foreign-mode", "port", "file-port", "duplicate-port", "copied"} {
		t.Run(damage, func(t *testing.T) {
			m, dir := forkReservationFixture(t)
			m = damageForkReservation(t, damage, dir, filepath.Join(dir, m.Name), m)
			called := false

			err := m.reservation.perform(context.Background(), m, func() error { called = true; return nil }, nil)
			if err == nil || called {
				t.Fatalf("accepted %s: %v, effect=%v", damage, err, called)
			}
		})
	}
}

func TestNewForkMaterialisedReservationAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	oldStore, oldPorts, oldSocket, oldConf := forkReservationStore, forkClusterPorts, forkSocketTaken, pgConfRoot
	forkReservationStore = func() (string, uint32, error) { return dir, uint32(os.Getuid()), nil }
	forkClusterPorts = func() map[int]bool { return map[int]bool{} }
	forkSocketTaken = func(int) bool { return false }
	pgConfRoot = t.TempDir()
	t.Cleanup(func() {
		forkReservationStore, forkClusterPorts, forkSocketTaken, pgConfRoot = oldStore, oldPorts, oldSocket, oldConf
	})

	m, err := NewForkMaterialised(t.TempDir(), "fork", "source", 5432, engine.Shell{}, "", nil)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Drop(); err == nil {
		t.Fatal("missing ownership guard accepted")
	}

	m.ReleaseForkReservationAuthority()

	m.Ownership = testForkGuard{}
	if err := m.Drop(); err == nil {
		t.Fatal("released capability accepted")
	}

	if _, err := os.Stat(filepath.Join(dir, m.Name)); err != nil {
		t.Fatalf("relinquishing removed reservation: %v", err)
	}
}

func TestForkReservationLocksAndRemovesExactly(t *testing.T) {
	m, dir := forkReservationFixture(t)

	err := m.reservation.perform(context.Background(), m, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		_, err := openForkAllocator(ctx, dir, uint32(os.Getuid()))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("allocator not held: %v", err)
		}

		return nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := os.Lstat(filepath.Join(dir, m.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation remains: %v", err)
	}

	if err := m.reservation.perform(context.Background(), m, func() error { t.Fatal("stale effect"); return nil }, nil); err == nil {
		t.Fatal("stale authority accepted")
	}
}

// damageFollowerReservation applies one reservation change to a follower
// cleanup fixture.
func damageFollowerReservation(t *testing.T, mode, path string, m *Materialised) {
	t.Helper()

	switch mode {
	case "missing", "replaced":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}

		if mode == "replaced" {
			if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	case "changed":
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// requireAllocatorHeld requires the allocator flock of dir to be held: a
// second allocator must wait until its context ends.
func requireAllocatorHeld(t *testing.T, dir string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	if _, err := openForkAllocator(ctx, dir, uint32(os.Getuid())); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("allocator lock not held: %v", err)
	}
}

// requireFollowerCleanupResult requires the outcome and the reservation state
// a cleanup mode implies.
func requireFollowerCleanupResult(t *testing.T, mode string, err error, calls int, path string) {
	t.Helper()

	wantErr := mode == "replaced" || mode == "changed" || mode == "effect-error"

	wantCalls := map[bool]int{true: 0, false: 1}[mode == "replaced" || mode == "changed"]
	if (err != nil) != wantErr || calls != wantCalls {
		t.Fatalf("mode=%s calls=%d err=%v", mode, calls, err)
	}

	_, statErr := os.Lstat(path)
	if mode == "present" || mode == "missing" {
		if !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("reservation remains: %v", statErr)
		}
	} else if statErr != nil {
		t.Fatalf("reservation lost: %v", statErr)
	}
}

func TestFollowerCleanupReservationOwnershipAndLock(t *testing.T) {
	for _, mode := range []string{"present", "missing", "replaced", "changed", "effect-error"} {
		t.Run(mode, func(t *testing.T) {
			m, dir := forkReservationFixture(t)

			id, err := m.ForkReservationIdentity()
			if err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(dir, m.Name)
			damageFollowerReservation(t, mode, path, m)

			calls := 0
			err = CleanupFollowerReservationWithHost(context.Background(), dir, uint32(os.Getuid()), m.Name, m.Port, id, func() error {
				calls++

				requireAllocatorHeld(t, dir)

				if mode == "effect-error" {
					return errors.New("effect failed")
				}

				return nil
			})
			requireFollowerCleanupResult(t, mode, err, calls, path)
		})
	}
}

// isolateForkLifecycleProbes isolates the registration writes and lifecycle
// probes a fork adopt or drop makes.
func isolateForkLifecycleProbes(t *testing.T) {
	t.Helper()

	oldConf, oldRunning, oldWrite := pgConfRoot, materialisedDataRunning, writeClusterFile
	pgConfRoot = t.TempDir()
	materialisedDataRunning = func(context.Context, string) bool { return false }
	writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
		return os.WriteFile(path, data, perm)
	}

	t.Cleanup(func() { pgConfRoot, materialisedDataRunning, writeClusterFile = oldConf, oldRunning, oldWrite })
}

// adoptFork adopts a fork whose source registration is in place.
func adoptFork(t *testing.T, m *Materialised) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(m.ConfDir(), "pg_ctl.conf"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(pgConfRoot, m.Source), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(pgConfRoot, m.Source, "postgresql.conf"), []byte("port = 5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := m.Adopt(context.Background(), 5439); err != nil {
		t.Fatal(err)
	}

	if m.Port != 5439 {
		t.Fatal("final port not recorded")
	}
}

// dropFork drops a fork whose data directory exists.
func dropFork(t *testing.T, m *Materialised) {
	t.Helper()

	if err := os.MkdirAll(m.Data, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := m.Drop(); err != nil {
		t.Fatal(err)
	}
}

// legacyReservationFixture writes a bare legacy reservation into a fresh
// allocator directory. No capability or other descriptor of the test's own
// refers to it, so only the cleanup can keep its inode alive.
func legacyReservationFixture(t *testing.T) (dir, name string, port int, info os.FileInfo) {
	t.Helper()

	dir = t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	name, port = "app__restoring", 5480

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", port)), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}

	return dir, name, port, info
}

// reservationHeldOpen reports whether this process holds an open descriptor for
// info. It scans procfs because the descriptor the cleanup pins the inode with
// is internal to the package.
func reservationHeldOpen(t *testing.T, info os.FileInfo) bool {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("procfs is unavailable: %v", err)
	}

	for _, entry := range entries {
		held, err := os.Stat(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue // the descriptor closed while scanning
		}

		if os.SameFile(info, held) {
			return true
		}
	}

	return false
}

// The cleanup must keep the legacy reservation's inode referenced through its
// effect: an open descriptor stops the kernel from recycling the inode number,
// so a replacement created while the callback runs cannot pass the identity
// check and be removed as if it were the original mapping.
func TestLegacyCleanupPinsReservationThroughEffect(t *testing.T) {
	dir, name, port, info := legacyReservationFixture(t)
	path := filepath.Join(dir, name)
	pinned := false
	err := CleanupCurrentLegacyReservationWithHost(context.Background(), dir, uint32(os.Getuid()), name, port, func() error {
		pinned = reservationHeldOpen(t, info)

		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		if err := os.Remove(path); err != nil {
			return err
		}

		return os.WriteFile(path, raw, 0o600)
	})

	if !pinned {
		t.Fatal("the legacy reservation was not pinned by an open descriptor through the effect")
	}

	if err == nil || !strings.Contains(err.Error(), "changed during cleanup") {
		t.Fatalf("a reservation replaced during the effect was not refused: %v", err)
	}

	if _, statErr := os.Lstat(path); statErr != nil {
		t.Fatalf("the replacement was removed: %v", statErr)
	}
}

func TestForkReservationAdoptAndDrop(t *testing.T) {
	for _, action := range []string{"adopt", "drop"} {
		t.Run(action, func(t *testing.T) {
			isolateForkLifecycleProbes(t)
			m, dir := forkReservationFixture(t)

			m.Ownership = lockingForkGuard{dir: dir}
			if err := os.MkdirAll(filepath.Join(m.ConfDir(), "conf.d"), 0o700); err != nil {
				t.Fatal(err)
			}

			if action == "adopt" {
				adoptFork(t, m)
			} else {
				dropFork(t, m)
			}

			if _, err := os.Lstat(filepath.Join(dir, m.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reservation remains: %v", err)
			}
		})
	}
}

// replacedForkReservation returns a fork whose reservation file was replaced by
// a different inode, and the counter of the lifecycle probes it runs.
func replacedForkReservation(t *testing.T) (*Materialised, *int) {
	t.Helper()

	oldConf, oldRunning := pgConfRoot, materialisedDataRunning
	pgConfRoot = t.TempDir()
	runningCalls := 0
	materialisedDataRunning = func(context.Context, string) bool { runningCalls++; return false }

	t.Cleanup(func() { pgConfRoot, materialisedDataRunning = oldConf, oldRunning })

	m, dir := forkReservationFixture(t)
	if err := os.MkdirAll(m.Data, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, m.Name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
		t.Fatal(err)
	}

	return m, &runningCalls
}

func TestForkLifecycleRefusesReplacedReservationBeforeEffects(t *testing.T) {
	for _, action := range []string{"adopt", "drop"} {
		t.Run(action, func(t *testing.T) {
			m, runningCalls := replacedForkReservation(t)

			var err error
			if action == "adopt" {
				err = m.Adopt(context.Background(), 5439)
			} else {
				err = m.Drop()
			}

			if err == nil || *runningCalls != 0 {
				t.Fatalf("replaced reservation accepted: %v, probes %d", err, *runningCalls)
			}

			if _, err := os.Stat(m.Data); err != nil {
				t.Fatalf("data touched: %v", err)
			}
		})
	}
}
