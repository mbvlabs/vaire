package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
)

// Reading an absent store reports it as absent without creating it: doctor is
// read-only and the node's emitter creates the store on its first write.
func TestActivityHealthDoesNotCreateStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	store := &activity.Store{
		Dir:          dir,
		MaxBytes:     1 << 20,
		SegmentBytes: 64 << 10,
		Retention:    time.Hour,
	}
	h, ok := activityStoreHealth(context.Background(), store)
	if !ok {
		t.Fatal("health of an absent store is unknown, want an absent reading")
	}
	if h.Exists {
		t.Error("an absent store is reported as existing")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("Health created the store: %v", err)
	}
}

func TestActivityHealthNilStore(t *testing.T) {
	if _, ok := activityStoreHealth(context.Background(), nil); ok {
		t.Error("a nil store has known health")
	}
}

// A canceled context means doctor did not measure the store. The helper must
// report that as unknown rather than as a healthy or empty history, so the
// doctor check can warn instead of silently trusting it.
func TestActivityHealthCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	store := &activity.Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     1 << 20,
		SegmentBytes: 64 << 10,
		Retention:    time.Hour,
	}
	if _, ok := activityStoreHealth(ctx, store); ok {
		t.Error("a canceled read has known health")
	}
}

// An unsafe existing directory still yields typed Health so the doctor checks
// can fail its permissions; the helper must not turn it into an unknown read.
func TestActivityHealthUnsafeDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the test needs an unsafe mode for the check.
		t.Fatal(err)
	}
	store := &activity.Store{
		Dir:          dir,
		MaxBytes:     1 << 20,
		SegmentBytes: 64 << 10,
		Retention:    time.Hour,
	}
	h, ok := activityStoreHealth(context.Background(), store)
	if !ok {
		t.Fatal("health of an unsafe directory is unknown, want a typed reading")
	}
	if !h.Exists || !h.IsDir {
		t.Errorf("unsafe directory: exists %t, is_dir %t", h.Exists, h.IsDir)
	}
	if h.OwnerOnly {
		t.Error("a 0755 activity directory is reported as owner-only")
	}
}

// A store that fails the read is unknown, never healthy: the doctor check must
// warn instead of passing a history it could not inspect.
func TestActivityHealthFailureIsUnknown(t *testing.T) {
	store := &fakeActivityStore{healthErr: errors.New("the state file is unreadable")}
	if h, ok := activityStoreHealth(context.Background(), store); ok {
		t.Fatalf("a failed read reported known health: %+v", h)
	}
}

// A real health failure that the store reports as typed data stays visible:
// the bounded read only turns a broken *call* into unknown, not the losses,
// permissions and budget facts the doctor checks act on.
func TestActivityHealthKeepsTypedFailureVisible(t *testing.T) {
	store := &fakeActivityStore{health: activity.Health{
		Exists: true, IsDir: true, Readable: true, Writable: false,
		LossDetected: true, MalformedEvents: 1,
	}}
	h, ok := activityStoreHealth(context.Background(), store)
	if !ok {
		t.Fatal("a typed health reading was reported as unknown")
	}
	if !h.LossDetected || h.MalformedEvents != 1 || h.Writable {
		t.Fatalf("health = %+v", h)
	}
}

// panicHealthStore panics from Health, as a broken store implementation could.
type panicHealthStore struct {
	fakeActivityStore
}

func (s *panicHealthStore) Health(context.Context) (activity.Health, error) {
	panic("health")
}

// A store that panics is unknown as well: the read runs on its own goroutine,
// outside the request's recover, so it must not end the process.
func TestActivityHealthPanicIsUnknown(t *testing.T) {
	if _, ok := activityStoreHealth(context.Background(), &panicHealthStore{}); ok {
		t.Fatal("a panicking store reported known health")
	}
	if activityHealthInFlight.Load() {
		t.Fatal("a panicking store left the health read latched")
	}
	if _, ok := activityStoreHealth(context.Background(), &fakeActivityStore{health: activity.Health{Exists: true}}); !ok {
		t.Fatal("health stays unknown after a store panic")
	}
}

// stalledHealthStore blocks inside Health until the test releases it. It
// models a filesystem call that ignores its context: doctor's bounded wait
// cannot cancel it, only stop waiting on it.
type stalledHealthStore struct {
	fakeActivityStore

	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int64
}

func newStalledHealthStore() *stalledHealthStore {
	return &stalledHealthStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *stalledHealthStore) Health(context.Context) (activity.Health, error) {
	s.calls.Add(1)
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	return activity.Health{Exists: true, IsDir: true, Readable: true, Writable: true, OwnerOnly: true, Segments: 1}, nil
}

// releaseStalledHealth lets the blocked read return, once.
func (s *stalledHealthStore) releaseStalledHealth() {
	s.once.Do(func() { close(s.release) })
}

// stalledActivityHealth installs a store whose health read never returns and
// shortens doctor's wait so the test is fast and deterministic. Cleanup
// releases the read and waits until no health read is in flight, so one test
// cannot leak the process-wide latch into the next.
func stalledActivityHealth(t *testing.T) *stalledHealthStore {
	t.Helper()
	oldWait, oldFactory := activityHealthWait, activityStoreFactory
	activityHealthWait = 25 * time.Millisecond
	store := newStalledHealthStore()
	activityStoreFactory = func() (activityStore, error) { return store, nil }
	t.Cleanup(func() {
		store.releaseStalledHealth()
		waitActivityHealthReads(t)
		activityHealthWait = oldWait
		activityStoreFactory = oldFactory
	})
	return store
}

// waitActivityHealthReads waits until the background health read has returned.
func waitActivityHealthReads(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for activityHealthInFlight.Load() {
		if time.Now().After(deadline) {
			t.Fatal("an activity health read is still in flight")
		}
		time.Sleep(time.Millisecond)
	}
}

// waitStalledHealthEntered waits until the store is actually inside Health.
func waitStalledHealthEntered(t *testing.T, store *stalledHealthStore) {
	t.Helper()
	select {
	case <-store.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the health read never reached the store")
	}
}

// TestActivityHealthStallIsBounded is the caller's bound: a read that never
// returns past its context still lets doctor return, reporting the history as
// unknown rather than as healthy, and the next run reads the store again once
// it answers.
func TestActivityHealthStallIsBounded(t *testing.T) {
	store := stalledActivityHealth(t)

	started := time.Now()
	if h, ok := gatherActivityHealth(context.Background()); ok {
		t.Fatalf("a stalled read reported known health: %+v", h)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the doctor caller waited %s for a stalled read", elapsed)
	}
	waitStalledHealthEntered(t, store)

	store.releaseStalledHealth()
	waitActivityHealthReads(t)

	h, ok := gatherActivityHealth(context.Background())
	if !ok || !h.Exists || h.Segments != 1 {
		t.Fatalf("health after the stall = %+v, known %t", h, ok)
	}
}

// TestActivityHealthKeepsOneReadInFlight is the background bound: repeated
// stalled reads must not each pin another goroutine on the store. While the
// first read is stuck, later calls report unknown at once without reaching the
// store, and the read that lands afterwards is still reported.
func TestActivityHealthKeepsOneReadInFlight(t *testing.T) {
	store := stalledActivityHealth(t)

	if _, ok := gatherActivityHealth(context.Background()); ok {
		t.Fatal("a stalled read reported known health")
	}
	waitStalledHealthEntered(t, store)
	baseline := runtime.NumGoroutine()

	started := time.Now()
	for i := 0; i < 50; i++ {
		if _, ok := gatherActivityHealth(context.Background()); ok {
			t.Fatal("a latched read reported known health")
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("50 latched calls took %s", elapsed)
	}
	if calls := store.calls.Load(); calls != 1 {
		t.Fatalf("the stalled store was read %d times, want one in-flight read", calls)
	}
	if grown := runtime.NumGoroutine() - baseline; grown > 2 {
		t.Fatalf("repeated stalls added %d goroutines", grown)
	}

	store.releaseStalledHealth()
	waitActivityHealthReads(t)
	if _, ok := gatherActivityHealth(context.Background()); !ok {
		t.Fatal("health stays unknown after the stalled read returned")
	}
}

// TestActivityHealthStallReleasesTheNodeLock models the lock doctor runs
// under: the quiet doctor job takes the node lock shared and a full apply
// holds it exclusive through the end-of-apply gate, and both release it with a
// defer when the caller returns. A stalled read must therefore outlive the
// caller without outliving the lock. The node lock's path is fixed, so this
// uses a real flock on a temporary file.
func TestActivityHealthStallReleasesTheNodeLock(t *testing.T) {
	store := stalledActivityHealth(t)

	path := filepath.Join(t.TempDir(), "vaire.lock")
	lock, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a lock file in the test's temp directory.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }()
		if _, ok := gatherActivityHealth(context.Background()); ok {
			t.Error("a stalled read reported known health")
		}
	}()
	waitStalledHealthEntered(t, store)

	// The read is still blocked on the store, yet another operation can take
	// the lock exclusive: the caller returned and its deferred release ran.
	other, err := os.OpenFile(path, os.O_RDWR, 0o600) //nolint:gosec // G304: the test's own lock file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = other.Close() }()
	if err := syscall.Flock(int(other.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the node lock is still held while the health read stalls: %v", err)
	}
	_ = syscall.Flock(int(other.Fd()), syscall.LOCK_UN)
}
