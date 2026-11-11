package activity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAppendRefusesWhenRecordedHistoryIsGone covers a store whose state still
// counts events while every segment holding them disappeared. Appending used to
// rebaseline the counters and hide the loss from later reads; it now refuses.
func TestAppendRefusesWhenRecordedHistoryIsGone(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	mustAppend(t, s, baseEvent())
	for _, seg := range segmentsOf(t, s) {
		if err := os.Remove(seg.path); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errHistoryMissing) {
		t.Fatalf("Append after losing every segment = %v, want errHistoryMissing", err)
	}
	state, exists, err := readState(s.Dir)
	if err != nil || !exists {
		t.Fatalf("state after refused append: exists=%v err=%v", exists, err)
	}
	if state.LastSequence != 2 || state.DroppedEvents != 0 || state.StoreID == "" {
		t.Fatalf("refused append rewrote state: %+v", state)
	}
	if segs := segmentsOf(t, s); len(segs) != 0 {
		t.Fatalf("refused append wrote a segment: %+v", segs)
	}

	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 0 || !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("query after loss no longer reports it: %+v", page.Coverage)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.LossDetected {
		t.Fatalf("health after loss no longer reports it: %+v", health)
	}
}

// TestWalkSegmentIgnoresAnInFlightTail cuts a real segment in the middle of its
// last record, which is what a reader sees when it snapshotted the size while a
// writer was appending: the file already holds the rest of the line.
func TestWalkSegmentIgnoresAnInFlightTail(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	raw, err := os.ReadFile(segs[0].path)
	if err != nil {
		t.Fatal(err)
	}
	end := len(raw) - 1
	start := bytes.LastIndexByte(raw[:end], '\n') + 1
	line := raw[start:end]
	cut := int64(end - len(line)/2)

	f, err := openSegmentRead(segs[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	walk, err := walkSegment(context.Background(), f, cut, false, true, 0, func(Event) bool { return true })
	if err != nil {
		t.Fatalf("walkSegment: %v", err)
	}
	if !walk.inFlightTail {
		t.Fatalf("grown tail not reported as in flight: %+v", walk)
	}
	if walk.malformedEvents != 0 {
		t.Fatalf("in-flight tail counted as malformed: %+v", walk)
	}
	if walk.events != 1 {
		t.Fatalf("events = %d, want the one complete record", walk.events)
	}
}

// TestWalkSegmentCountsATornTail keeps genuine corruption visible: the file
// ends mid-record and does not grow, so the writer stopped without finishing.
func TestWalkSegmentCountsATornTail(t *testing.T) {
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
	if walk.inFlightTail {
		t.Fatalf("torn tail taken for an in-flight append: %+v", walk)
	}
	if walk.malformedEvents != 1 {
		t.Fatalf("torn tail malformed count = %d, want 1", walk.malformedEvents)
	}
}

// TestReadersIgnoreAnInFlightTail reproduces the snapshot boundary through the
// public reads: the directory is listed while only half of the next record is
// on disk, and the other half lands before the scan.
func TestReadersIgnoreAnInFlightTail(t *testing.T) {
	s, complete := snapshotWithHalfWrittenRecord(t)
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	complete()
	if page.Coverage.MalformedEvents != 0 || page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("in-flight tail reported as loss: %+v", page.Coverage)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want the one record complete at snapshot time", len(page.Events))
	}

	s, complete = snapshotWithHalfWrittenRecord(t)
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	complete()
	if health.MalformedEvents != 0 || health.LossDetected {
		t.Fatalf("in-flight tail reported as loss by Health: %+v", health)
	}
}

// snapshotWithHalfWrittenRecord writes the first half of a valid next record
// and returns a hook that completes it after the directory listing. The
// returned func must be called before the read's Coverage is asserted.
func snapshotWithHalfWrittenRecord(t *testing.T) (*Store, func()) {
	t.Helper()
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	next := Event{
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
	}
	line, err := encodeEvent(next)
	if err != nil {
		t.Fatal(err)
	}
	half := len(line) / 2
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
	appendBytes(line[:half])

	old := afterListSegmentsHook
	afterListSegmentsHook = func() {
		afterListSegmentsHook = nil
		appendBytes(line[half:])
	}
	t.Cleanup(func() { afterListSegmentsHook = old })
	return s, func() {
		if afterListSegmentsHook != nil {
			t.Fatalf("the in-flight record was never completed")
		}
	}
}

// TestConcurrentReadsCountNoMalformedEvents tightens the concurrent regression:
// a healthy append can never make a reader see a malformed or lost record.
func TestConcurrentReadsCountNoMalformedEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const writes = 120
	done := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 0; i < writes; i++ {
			if _, err := s.Append(ctx, labelledEvent(fmt.Sprintf("e%03d", i))); err != nil {
				done <- err
				return
			}
		}
	}()
	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Append during reads: %v", err)
			}
		default:
			page, err := s.Query(ctx, Query{Limit: 20})
			if err != nil {
				t.Fatalf("Query during append: %v", err)
			}
			if cov := page.Coverage; cov.MalformedEvents != 0 || cov.MalformedSegments != 0 || cov.LossDetected {
				t.Fatalf("healthy concurrent append reported loss: %+v", cov)
			}
			health, err := s.Health(ctx)
			if err != nil {
				t.Fatalf("Health during append: %v", err)
			}
			if health.MalformedEvents != 0 || health.MalformedSegments != 0 || health.LossDetected {
				t.Fatalf("healthy concurrent append reported loss by Health: %+v", health)
			}
			continue
		}
		break
	}
	page, err := s.Query(ctx, Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("final Query: %v", err)
	}
	if len(page.Events) != writes || page.Coverage.LossDetected {
		t.Fatalf("final page = %d events, coverage %+v", len(page.Events), page.Coverage)
	}
}

// TestPausedWriterIsNotCorruption separates a paused but live append from a torn
// tail. A writer keeps the store's append lock for as long as its line is
// incomplete, so a reader that sees no growth can still prove the line is not
// corruption. Once the lock is free, the same bytes are a torn tail again.
func TestPausedWriterIsNotCorruption(t *testing.T) {
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
	half := len(line) / 2
	appendHalf := func(b []byte) {
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
	appendHalf(line[:half])

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
	// Re-list so the segment's size includes the incomplete line.
	segs = segmentsOf(t, s)

	walkNewest := func(newest bool) segmentWalk {
		t.Helper()
		f, err := openSegmentRead(segs[0])
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		walk, err := walkSegment(context.Background(), f, segs[0].size, false, newest, 0, func(Event) bool { return true })
		if err != nil {
			t.Fatalf("walkSegment: %v", err)
		}
		return walk
	}
	if walk := walkNewest(true); !walk.inFlightTail || walk.malformedEvents != 0 {
		t.Fatalf("paused writer reported as corruption: %+v", walk)
	}
	// Only the newest segment can be appended to: an older unterminated tail is
	// corruption even while some writer holds the lock.
	if walk := walkNewest(false); walk.inFlightTail || walk.malformedEvents != 1 {
		t.Fatalf("older torn tail was excused: %+v", walk)
	}

	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("paused writer reported as loss: %+v", page.Coverage)
	}
	if !page.Coverage.Truncated {
		t.Fatalf("a cut snapshot claimed a complete scan: %+v", page.Coverage)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.LossDetected {
		t.Fatalf("paused writer reported as loss by Health: %+v", health)
	}

	lock.release()
	released = true
	page, err = s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query after release: %v", err)
	}
	if !page.Coverage.LossDetected || page.Coverage.MalformedEvents != 1 {
		t.Fatalf("torn tail no longer reported: %+v", page.Coverage)
	}
}
