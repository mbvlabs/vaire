package backup

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/eru/internal/engine"
)

// A stanza-create that meets an async archive-push holding the stanza lock
// waits and retries; any other failure is returned at once.
func TestRetryStanzaLock(t *testing.T) {
	busy := &engine.ScriptError{Step: "pgbackrest stanza-create", Status: stanzaLockBusy}
	calls := 0
	err := retryStanzaLock(context.Background(), func(context.Context) error {
		calls++
		if calls < 3 {
			return busy
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("retry: err %v, calls %d, want nil after 3", err, calls)
	}

	other := &engine.ScriptError{Step: "pgbackrest stanza-create", Status: 68}
	calls = 0
	if err := retryStanzaLock(context.Background(), func(context.Context) error {
		calls++
		return other
	}); !errors.Is(err, other) || calls != 1 {
		t.Fatalf("exit 68: err %v, calls %d, want one attempt", err, calls)
	}

	// A lock that never clears gives up after the bounded retries.
	calls = 0
	if err := retryStanzaLock(context.Background(), func(context.Context) error {
		calls++
		return busy
	}); !errors.Is(err, busy) || calls != stanzaLockRetries+1 {
		t.Fatalf("persistent lock: err %v, calls %d, want %d", err, calls, stanzaLockRetries+1)
	}

	// A cancelled context stops waiting.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls = 0
	if err := retryStanzaLock(ctx, func(context.Context) error {
		calls++
		return busy
	}); !errors.Is(err, busy) || calls != 1 {
		t.Fatalf("cancelled: err %v, calls %d, want one attempt", err, calls)
	}
}
