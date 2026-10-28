package activity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestTailCompletedBetweenSizeAndLockChecks covers the transition where a
// writer finishes its line and releases the append lock after the reader's
// size check but before its lock probe. The file grew, so the tail is a
// completed append, not corruption; the recheck keeps a healthy write from
// being counted as loss.
func TestTailCompletedBetweenSizeAndLockChecks(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	line, err := encodeEvent(Event{
		Schema:     Schema,
		EventID:    "evt-01M4X0ME0NBQ95HNNDT213BGH9",
		OccurredAt: formatTime(time.Now().UTC()),
		Sequence:   2,
		Server:     "prod",
		Node:       "db1",
		Source:     SourceRPC,
		Action:     "backup.full",
		Outcome:    OutcomeStarted,
		ActorKind:  ActorOperator,
	})
	if err != nil {
		t.Fatal(err)
	}
	appendBytes := func(b []byte) {
		f, err := os.OpenFile(segs[0].path, os.O_WRONLY|os.O_APPEND, fileMode) //nolint:gosec // G304: a test store segment.
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	half := len(line) / 2
	appendBytes(line[:half])

	lock, err := acquireLock(context.Background(), filepath.Join(s.Dir, lockName))
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			lock.release()
		}
	}()
	// Re-list so the snapshot includes the incomplete line.
	segs = segmentsOf(t, s)

	oldHook := afterTailStatHook
	afterTailStatHook = func() {
		afterTailStatHook = nil
		appendBytes(line[half:])
		lock.release()
		released = true
	}
	t.Cleanup(func() { afterTailStatHook = oldHook })

	f, err := openSegmentRead(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	walk, err := walkSegment(context.Background(), f, segs[0].size, false, true, 0, func(Event) bool { return true })
	if err != nil {
		t.Fatalf("walkSegment: %v", err)
	}
	if !walk.inFlightTail || walk.malformedEvents != 0 {
		t.Fatalf("completed tail not recognized as an in-flight append: %+v", walk)
	}
	if walk.events != 1 {
		t.Fatalf("events = %d, want the one complete record", walk.events)
	}
}

// TestTailStillTornWhenNoWriterHoldsTheLock keeps genuine corruption visible:
// the same partial line, with no growth and no writer, is still counted.
func TestTailStillTornWhenNoWriterHoldsTheLock(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	f, err := os.OpenFile(segs[0].path, os.O_WRONLY|os.O_APPEND, fileMode) //nolint:gosec // G304: a test store segment.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(`{"schema":"vaire-activity/v1"`)); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	segs = segmentsOf(t, s)
	read, err := openSegmentRead(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = read.Close() }()
	walk, err := walkSegment(context.Background(), read, segs[0].size, false, true, 0, func(Event) bool { return true })
	if err != nil {
		t.Fatalf("walkSegment: %v", err)
	}
	if walk.inFlightTail || walk.malformedEvents != 1 {
		t.Fatalf("torn tail = %+v, want one malformed event", walk)
	}
}
