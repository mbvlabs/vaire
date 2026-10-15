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

// ClusterLockPath is one cluster's lock (RFC 11.2): its step instances and
// per-database jobs take it after the global lock, so work on different
// clusters runs side by side. Locks are always taken global first, then
// clusters in name order.
func ClusterLockPath(cluster string) string { return "/run/lock/eru-" + cluster + ".lock" }

// HeavyIOLockPath is the server's heavy-I/O lock (RFC 9.3): the backup jobs
// of every cluster take it exclusive after the global lock and before their
// cluster's, so at most one full, diff, verify, drill or dump reads the disk
// at a time. Database names have no hyphen, so no cluster lock shares it.
const HeavyIOLockPath = "/run/lock/eru-heavy-io.lock"

// LockMode is shared or exclusive.
type LockMode string

// Lock modes. LockNone means the method takes no lock.
const (
	LockNone      LockMode = ""
	LockShared    LockMode = "shared"
	LockExclusive LockMode = "exclusive"
)

const lockPoll = 200 * time.Millisecond

// Holder is the content of eru.lock.info and eru-<cluster>.lock.info.
type Holder struct {
	PID       int    `json:"pid"`
	Cluster   string `json:"cluster,omitempty"`
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
	return acquire(ctx, LockPath, "", "", mode, method, requestID, timeout)
}

// AcquireCluster takes cluster's lock in mode, as Acquire does the global
// one. The caller holds the global lock already.
func AcquireCluster(ctx context.Context, cluster string, mode LockMode, method, requestID string, timeout time.Duration) (Release, error) {
	return acquire(ctx, ClusterLockPath(cluster), cluster, "", mode, method, requestID, timeout)
}

// AcquireHeavyIO takes the heavy-I/O lock exclusive, once: a busy lock is
// lock_held with details.lock heavy_io. The caller holds the global lock.
func AcquireHeavyIO(ctx context.Context, method, requestID string) (Release, error) {
	return acquire(ctx, HeavyIOLockPath, "", "heavy_io", LockExclusive, method, requestID, 0)
}

// AcquireHeavyIOWait takes the heavy-I/O lock exclusive, waiting up to
// timeout for it: db recover waits for a backup that holds it instead of
// failing (Phase 5).
func AcquireHeavyIOWait(ctx context.Context, method, requestID string, timeout time.Duration) (Release, error) {
	return acquire(ctx, HeavyIOLockPath, "", "heavy_io", LockExclusive, method, requestID, timeout)
}

func acquire(ctx context.Context, path, cluster, lock string, mode LockMode, method, requestID string, timeout time.Duration) (Release, error) {
	if mode == LockNone {
		return func() {}, nil
	}
	how := syscall.LOCK_EX
	if mode == LockShared {
		how = syscall.LOCK_SH
	}
	infoPath := path + ".info"
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the fixed global lock or a cluster's, named from config.
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
			return nil, lockHeld(infoPath, cluster, lock)
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
		Cluster:   cluster,
		Method:    method,
		RequestID: requestID,
		Mode:      string(mode),
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if raw, err := json.Marshal(info); err == nil {
		_ = WriteFileAtomic(infoPath, append(raw, '\n'), 0o644)
	}
	return func() {
		if mode == LockExclusive {
			_ = os.Remove(infoPath)
		}
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

func lockHeld(infoPath, cluster, lock string) *output.CLIError {
	var holder *Holder
	if raw, err := os.ReadFile(infoPath); err == nil { //nolint:gosec // G304: the lock's own info file.
		var h Holder
		if json.Unmarshal(raw, &h) == nil && processAlive(h.PID) {
			holder = &h
		}
	}
	message := "another eru operation holds the node lock"
	details := map[string]any{"reason": "lock_held", "holder": holder}
	if cluster != "" {
		message = "another eru operation holds the lock of " + cluster
		details["cluster"] = cluster
	}
	if lock == "heavy_io" {
		message = "another backup job holds the heavy-I/O lock"
		details["lock"] = lock
	}
	return output.NewError(
		output.CodePrecondition,
		message,
		output.ExitPrecondition,
		"Wait for it to finish, or retry with a longer lock timeout.",
	).WithDetails(details)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// IsLockHeld reports whether err is Acquire's lock_held timeout, as opposed
// to a failure to open or lock LockPath at all.
func IsLockHeld(err error) bool {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	details, ok := cliErr.Details.(map[string]any)
	return ok && details["reason"] == "lock_held"
}
