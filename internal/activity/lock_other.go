//go:build !unix

package activity

import (
	"context"
	"errors"
)

// errUnsupported reports a platform without flock. Failing is safer than
// writing without the append lock.
var errUnsupported = errors.New("activity: file locking is unsupported on this platform")

// fileLock is unused on platforms without flock.
type fileLock struct{}

// acquireLock refuses to run without the append lock rather than risking
// interleaved writes.
func acquireLock(context.Context, string) (*fileLock, error) {
	return nil, errUnsupported
}

func (l *fileLock) release() {}

// appendLockHeld reports no active writer on a platform without flock: the
// store cannot append there at all.
func appendLockHeld(string) bool { return false }
