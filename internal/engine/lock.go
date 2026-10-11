package engine

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// LockPath is the node's global lock (§9.5).
const LockPath = "/run/lock/eru.lock"

// LockInfoPath names the most recent holder.
const LockInfoPath = LockPath + ".info"

// LockMode is shared or exclusive.
type LockMode string

// Lock modes. LockNone means the method takes no lock.
const (
	LockNone      LockMode = ""
	LockShared    LockMode = "shared"
	LockExclusive LockMode = "exclusive"
)

const lockPoll = 200 * time.Millisecond

// Holder is the content of eru.lock.info.
type Holder struct {
	PID       int    `json:"pid"`
	Method    string `json:"method"`
	RequestID string `json:"request_id"`
	Mode      string `json:"mode"`
	StartedAt string `json:"started_at"`
}

// Release drops a lock taken by Acquire.
type Release func()

// Acquire takes the global lock in mode, polling every 200 ms until timeout.
// A timeout of 0 tries once. On timeout it returns precondition_failed with
// reason lock_held and the holder, or a null holder when its PID is gone.
func Acquire(ctx context.Context, mode LockMode, method, requestID string, timeout time.Duration) (Release, error) {
	if mode == LockNone {
		return func() {}, nil
	}
	how := syscall.LOCK_EX
	if mode == LockShared {
		how = syscall.LOCK_SH
	}
	f, err := os.OpenFile(LockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(fd, how|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if timeout <= 0 || !time.Now().Before(deadline) {
			_ = f.Close()
			return nil, lockHeld()
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	info := Holder{
		PID:       os.Getpid(),
		Method:    method,
		RequestID: requestID,
		Mode:      string(mode),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if raw, err := json.Marshal(info); err == nil {
		_ = WriteFileAtomic(LockInfoPath, append(raw, '\n'), 0o644)
	}
	return func() {
		if mode == LockExclusive {
			_ = os.Remove(LockInfoPath)
		}
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func lockHeld() *output.CLIError {
	var holder *Holder
	if raw, err := os.ReadFile(LockInfoPath); err == nil {
		var h Holder
		if json.Unmarshal(raw, &h) == nil && processAlive(h.PID) {
			holder = &h
		}
	}
	return output.NewError(
		output.CodePrecondition,
		"another eru operation holds the node lock",
		output.ExitPrecondition,
		"Wait for it to finish, or retry with a longer lock timeout.",
	).WithDetails(map[string]any{"reason": "lock_held", "holder": holder})
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
