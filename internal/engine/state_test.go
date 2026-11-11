package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestWithFileLockContextBoundsLockWait proves a busy state lock cannot hold a
// caller past its context. Settlement uses this so diagnostic bookkeeping
// cannot hang a finished job behind another process's state write.
func TestWithFileLockContextBoundsLockWait(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.json.lock")
	held, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a test lock path.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	fd := int(held.Fd()) //nolint:gosec // G115: a file descriptor fits in int.
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	called := false
	started := time.Now()
	err = withFileLockContext(ctx, lockPath, func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("withFileLockContext = %v, want the context deadline", err)
	}
	if called {
		t.Fatal("locked work ran without the lock")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("busy lock held the caller for %s", elapsed)
	}
}

// TestWithFileLockContextRunsWhenUnlocked keeps the normal settlement path
// working after the bounded lock acquisition.
func TestWithFileLockContextRunsWhenUnlocked(t *testing.T) {
	lockPath := filepath.Join(t.TempDir(), "state.json.lock")
	called := false
	if err := withFileLockContext(context.Background(), lockPath, func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("withFileLockContext: %v", err)
	}
	if !called {
		t.Fatal("withFileLockContext did not run its work")
	}
}
