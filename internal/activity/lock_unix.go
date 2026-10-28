//go:build unix

package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// fileLock is one held append/rotation lock. It is never the operation lock.
type fileLock struct {
	f *os.File
}

// acquireLock takes the store's short append lock with non-blocking flock
// attempts, polling until ctx expires or lockWait passes.
func acquireLock(ctx context.Context, path string) (*fileLock, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|noFollowFlag, fileMode) //nolint:gosec // G304: the store's own lock file.
	if err != nil {
		return nil, fmt.Errorf("activity: open lock file: %w", err)
	}
	if err := verifyLockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	deadline := time.NewTimer(lockWait)
	defer deadline.Stop()
	ticker := time.NewTicker(lockPoll)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) //nolint:gosec // G115: file descriptors fit in int.
		if err == nil {
			return &fileLock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, fmt.Errorf("activity: lock store: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("activity: lock store: %w", ctx.Err())
		case <-deadline.C:
			_ = f.Close()
			return nil, errLockTimeout
		case <-ticker.C:
		}
	}
}

// verifyLockFile refuses a symlink, device or foreign lock file.
func verifyLockFile(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != fileMode || !ownerMatches(info) {
		return errUnsafeLock
	}
	return nil
}

// release unlocks and closes the lock file.
func (l *fileLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: file descriptors fit in int.
	_ = l.f.Close()
}

// appendLockHeld reports whether a writer holds the store's append lock right
// now. It opens an existing lock file read-only without following a symlink and
// never creates or changes anything; a missing or unusable lock reports false,
// so a reader never mistakes it for an active writer.
func appendLockHeld(dir string) bool {
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDONLY|noFollowFlag, 0) //nolint:gosec // G304: the store's own lock file.
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	if err := verifyLockFile(f); err != nil {
		return false
	}
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) //nolint:gosec // G115: file descriptors fit in int.
	if err != nil {
		return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EINTR)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: file descriptors fit in int.
	return false
}
