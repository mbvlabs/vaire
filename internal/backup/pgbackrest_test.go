package backup

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/engine"
)

func TestMaterialiseTimelineOptions(t *testing.T) {
	for _, kind := range []string{TargetLatest, "", TargetTime, TargetSet} {
		for _, pause := range []bool{false, true} {
			opts := RestoreOptions{Kind: kind, Pause: pause, Path: "/data/copy", Set: "20261117-155723F", Timeline: "1"}
			args, err := materialiseArgs(opts)
			if err != nil {
				t.Fatal(err)
			}
			wantTimeline := kind != TargetSet
			if got := slices.Contains(args, "--target-timeline=1"); got != wantTimeline {
				t.Errorf("kind=%q pause=%v: timeline=%v, want %v: %v", kind, pause, got, wantTimeline, args)
			}
			if kind == TargetSet && (!slices.Contains(args, "--type=immediate") || !slices.Contains(args, "--set="+opts.Set)) {
				t.Errorf("selected-set restore lost its immediate target: %v", args)
			}
		}
	}
}

// A time restore passes the target's microseconds into pgBackRest's
// --target. PostgreSQL stores timestamps at microsecond resolution, so a
// nanosecond target is reduced there and no further.
func TestMaterialiseArgsCarryRecoveryTargetPrecision(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"microseconds", "2026-11-17T18:44:53.983364Z", "2026-11-17 18:44:53.983364+00"},
		{"nanoseconds reduce to microseconds", "2026-11-17T18:44:53.983364123Z", "2026-11-17 18:44:53.983364+00"},
		{"whole second", "2026-11-17T18:44:53Z", "2026-11-17 18:44:53.000000+00"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339Nano, c.input)
			if err != nil {
				t.Fatal(err)
			}
			args, err := materialiseArgs(RestoreOptions{Kind: TargetTime, Time: at, Pause: true, Path: "/data/copy"})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(args, "--target="+c.want) {
				t.Errorf("args %v, want --target=%s", args, c.want)
			}
			if !slices.Contains(args, "--type=time") || !slices.Contains(args, "--target-action=pause") {
				t.Errorf("args %v, want a paused time restore", args)
			}
		})
	}
}

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
