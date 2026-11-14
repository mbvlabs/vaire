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
	for i := 0; i < 2; i++ {
		lock, err := openForkAllocator(ctx, g.dir, uint32(os.Getuid())) //nolint:gosec // local test UID
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
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private temporary directory
		t.Fatal(err)
	}
	m := &Materialised{Name: "fork", Source: "source", Data: filepath.Join(t.TempDir(), "fork"), Ownership: testForkGuard{}}
	r, err := reserveForkAt(context.Background(), dir, uint32(os.Getuid()), m, func() map[int]bool { return map[int]bool{} }, func(int) bool { return false }) //nolint:gosec // local test UID
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
	_, err := reserveForkAt(context.Background(), dir, uint32(os.Getuid()), &Materialised{Name: m.Name}, func() map[int]bool { return map[int]bool{} }, func(int) bool { return false }) //nolint:gosec // local test UID
	if err == nil {
		t.Fatal("existing owner accepted")
	}
	raw, err := os.ReadFile(filepath.Join(dir, m.Name)) //nolint:gosec // test fixture
	if err != nil || strings.TrimSpace(string(raw)) != fmt.Sprint(m.Port) {
		t.Fatalf("reservation changed: %q %v", raw, err)
	}
}

func TestForkReservationRefusesChangedCapability(t *testing.T) {
	for _, tc := range []string{"replacement", "symlink", "foreign-mode", "port", "file-port", "duplicate-port", "copied"} {
		t.Run(tc, func(t *testing.T) {
			m, dir := forkReservationFixture(t)
			path := filepath.Join(dir, m.Name)
			switch tc {
			case "replacement", "symlink":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if tc == "replacement" {
					if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", m.Port)), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Symlink(filepath.Join(dir, "elsewhere"), path); err != nil {
					t.Fatal(err)
				}
			case "foreign-mode":
				if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // unsafe fixture
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
				copy := *m
				m = &copy
			}
			called := false
			err := m.reservation.perform(context.Background(), m, func() error { called = true; return nil }, nil)
			if err == nil || called {
				t.Fatalf("accepted %s: %v, effect=%v", tc, err, called)
			}
		})
	}
}

func TestNewForkMaterialisedReservationAuthority(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // private test directory
		t.Fatal(err)
	}
	oldStore, oldPorts, oldSocket, oldConf := forkReservationStore, forkClusterPorts, forkSocketTaken, pgConfRoot
	forkReservationStore = func() (string, uint32, error) { return dir, uint32(os.Getuid()), nil } //nolint:gosec // local test UID
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
		_, err := openForkAllocator(ctx, dir, uint32(os.Getuid())) //nolint:gosec // local test UID
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

func TestFollowerCleanupReservationOwnershipAndLock(t *testing.T) {
	for _, mode := range []string{"present", "missing", "replaced", "changed", "effect-error"} {
		t.Run(mode, func(t *testing.T) {
			m, dir := forkReservationFixture(t)
			id, err := m.ForkReservationIdentity()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, m.Name)
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
			calls := 0
			err = CleanupFollowerReservationWithHost(context.Background(), dir, uint32(os.Getuid()), m.Name, m.Port, id, func() error { //nolint:gosec // local test uid
				calls++
				ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
				defer cancel()
				if _, err := openForkAllocator(ctx, dir, uint32(os.Getuid())); !errors.Is(err, context.DeadlineExceeded) { //nolint:gosec // local test UID
					t.Fatalf("allocator lock not held: %v", err)
				} //nolint:gosec // local test uid
				if mode == "effect-error" {
					return errors.New("effect failed")
				}
				return nil
			})
			wantErr := mode == "replaced" || mode == "changed" || mode == "effect-error"
			if (err != nil) != wantErr || calls != map[bool]int{true: 0, false: 1}[mode == "replaced" || mode == "changed"] {
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
		})
	}
}

func TestForkReservationAdoptAndDrop(t *testing.T) {
	for _, action := range []string{"adopt", "drop"} {
		t.Run(action, func(t *testing.T) {
			oldConf, oldRunning, oldWrite := pgConfRoot, materialisedDataRunning, writeClusterFile
			pgConfRoot = t.TempDir()
			materialisedDataRunning = func(context.Context, string) bool { return false }
			writeClusterFile = func(path string, data []byte, perm os.FileMode, _, _ string) error {
				return os.WriteFile(path, data, perm) //nolint:gosec // temporary fixture paths
			}
			t.Cleanup(func() { pgConfRoot, materialisedDataRunning, writeClusterFile = oldConf, oldRunning, oldWrite })
			m, dir := forkReservationFixture(t)
			m.Ownership = lockingForkGuard{dir: dir}
			if err := os.MkdirAll(filepath.Join(m.ConfDir(), "conf.d"), 0o700); err != nil {
				t.Fatal(err)
			}
			if action == "adopt" {
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
			} else {
				if err := os.MkdirAll(m.Data, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := m.Drop(); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Lstat(filepath.Join(dir, m.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("reservation remains: %v", err)
			}
		})
	}
}

func TestForkLifecycleRefusesReplacedReservationBeforeEffects(t *testing.T) {
	for _, action := range []string{"adopt", "drop"} {
		t.Run(action, func(t *testing.T) {
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
			var err error
			if action == "adopt" {
				err = m.Adopt(context.Background(), 5439)
			} else {
				err = m.Drop()
			}
			if err == nil || runningCalls != 0 {
				t.Fatalf("replaced reservation accepted: %v, probes %d", err, runningCalls)
			}
			if _, err := os.Stat(m.Data); err != nil {
				t.Fatalf("data touched: %v", err)
			}
		})
	}
}
