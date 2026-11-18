package activity

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

	f, err := os.OpenFile(segs[0].path, os.O_WRONLY|os.O_APPEND, fileMode)
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
	line := halfWrittenEventLine(t)
	half := len(line) / 2
	appendSegmentBytes(t, segs[0].path, line[:half])

	old := afterListSegmentsHook
	afterListSegmentsHook = func() {
		afterListSegmentsHook = nil

		appendSegmentBytes(t, segs[0].path, line[half:])
	}

	t.Cleanup(func() { afterListSegmentsHook = old })

	return s, func() {
		if afterListSegmentsHook != nil {
			t.Fatalf("the in-flight record was never completed")
		}
	}
}

// halfWrittenEventLine is the encoded record a paused writer leaves
// incomplete.
func halfWrittenEventLine(t *testing.T) []byte {
	t.Helper()

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

	return line
}

// appendSegmentBytes appends raw bytes to one segment without touching state.
func appendSegmentBytes(t *testing.T, path string, b []byte) {
	t.Helper()

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, fileMode)
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

// walkListedSegment walks a whole listed segment with the newest-segment flag.
func walkListedSegment(t *testing.T, seg segmentInfo, newest bool) segmentWalk {
	t.Helper()

	f, err := openSegmentRead(seg)
	if err != nil {
		t.Fatal(err)
	}

	defer func() { _ = f.Close() }()

	walk, err := walkSegment(context.Background(), f, seg.size, false, newest, 0, func(Event) bool { return true })
	if err != nil {
		t.Fatalf("walkSegment: %v", err)
	}

	return walk
}

// TestConcurrentReadsCountNoMalformedEvents tightens the concurrent regression:
// a healthy append can never make a reader see a malformed or lost record.
func TestConcurrentReadsCountNoMalformedEvents(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const writes = 120

	done := make(chan error, 1)
	go appendLabelledEvents(ctx, s, writes, done)

	assertHealthyConcurrentReads(t, s, done)

	page := mustQuery(t, s, Query{Limit: MaxLimit})
	if len(page.Events) != writes || page.Coverage.LossDetected {
		t.Fatalf("final page = %d events, coverage %+v", len(page.Events), page.Coverage)
	}
}

// appendLabelledEvents appends writes labelled events and closes done when it
// finishes, carrying one error at most.
func appendLabelledEvents(ctx context.Context, s *Store, writes int, done chan<- error) {
	defer close(done)

	for i := range writes {
		if _, err := s.Append(ctx, labelledEvent(fmt.Sprintf("e%03d", i))); err != nil {
			done <- err
			return
		}
	}
}

// assertHealthyConcurrentReads reads until the writer finishes and fails on any
// fabricated loss while it runs.
func assertHealthyConcurrentReads(t *testing.T, s *Store, done <-chan error) {
	t.Helper()

	for {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Append during reads: %v", err)
			}

			return
		default:
			assertReadsSeeNoLoss(t, s)
		}
	}
}

// assertReadsSeeNoLoss checks one concurrent Query and Health report no
// fabricated loss.
func assertReadsSeeNoLoss(t *testing.T, s *Store) {
	t.Helper()

	page := mustQuery(t, s, Query{Limit: 20})
	if cov := page.Coverage; cov.MalformedEvents != 0 || cov.MalformedSegments != 0 || cov.LossDetected {
		t.Fatalf("healthy concurrent append reported loss: %+v", cov)
	}

	health := mustHealth(t, s)
	if health.MalformedEvents != 0 || health.MalformedSegments != 0 || health.LossDetected {
		t.Fatalf("healthy concurrent append reported loss by Health: %+v", health)
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
	line := halfWrittenEventLine(t)
	appendSegmentBytes(t, segs[0].path, line[:len(line)/2])

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
	if walk := walkListedSegment(t, segs[0], true); !walk.inFlightTail || walk.malformedEvents != 0 {
		t.Fatalf("paused writer reported as corruption: %+v", walk)
	}
	// Only the newest segment can be appended to: an older unterminated tail is
	// corruption even while some writer holds the lock.
	if walk := walkListedSegment(t, segs[0], false); walk.inFlightTail || walk.malformedEvents != 1 {
		t.Fatalf("older torn tail was excused: %+v", walk)
	}

	assertPausedWriterPage(t, s)
	assertPausedWriterHealth(t, s)

	lock.release()

	released = true

	torn := mustQuery(t, s, Query{})
	if !torn.Coverage.LossDetected || torn.Coverage.MalformedEvents != 1 {
		t.Fatalf("torn tail no longer reported: %+v", torn.Coverage)
	}
}

// assertPausedWriterPage checks a cut snapshot from a live writer is truncated,
// not loss.
func assertPausedWriterPage(t *testing.T, s *Store) {
	t.Helper()

	page := mustQuery(t, s, Query{})
	if page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("paused writer reported as loss: %+v", page.Coverage)
	}

	if !page.Coverage.Truncated {
		t.Fatalf("a cut snapshot claimed a complete scan: %+v", page.Coverage)
	}
}

// assertPausedWriterHealth checks Health does not call a live writer's cut
// snapshot corruption.
func assertPausedWriterHealth(t *testing.T, s *Store) {
	t.Helper()

	health := mustHealth(t, s)
	if health.LossDetected {
		t.Fatalf("paused writer reported as loss by Health: %+v", health)
	}
}

// TestCancelledTailRecoveryDoesNotReuseSequence reproduces a stale-state append
// whose tail recovery must fall back to a bounded scan, cancelled before the
// scan can prove the last sequence. The append must abort before writing:
// treating the interrupted scan as an absent tail would reuse a sequence that
// is already durable.
func TestCancelledTailRecoveryDoesNotReuseSequence(t *testing.T) {
	s, segPath := staleStateWithMalformedTail(t)
	before := fileSize(t, segPath)

	oldHook := beforeSegmentLastEventScanHook

	t.Cleanup(func() { beforeSegmentLastEventScanHook = oldHook })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	beforeSegmentLastEventScanHook = func() {
		beforeSegmentLastEventScanHook = nil

		cancel()
	}

	if _, err := s.Append(ctx, baseEvent()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tail recovery append = %v, want context.Canceled", err)
	}

	if after := fileSize(t, segPath); after != before {
		t.Fatalf("cancelled append wrote %d bytes to the active segment", after-before)
	}

	state, ok, err := readState(s.Dir)
	if err != nil || !ok {
		t.Fatalf("readState: %v ok=%v", err, ok)
	}

	if state.NextSequence != 3 || state.LastSequence != 2 {
		t.Fatalf("cancelled append advanced the counters: %+v", state)
	}

	next := mustAppend(t, s, baseEvent())
	if next.Sequence != 4 {
		t.Fatalf("append after cancelled recovery reused sequence %d, want 4", next.Sequence)
	}

	page := mustQuery(t, s, Query{Limit: MaxLimit})
	if got := sequences(page.Events); !equalSeq(got, []uint64{1, 2, 3, 4}) {
		t.Fatalf("sequences after recovery = %v, want 1..4", got)
	}
}

// staleStateWithMalformedTail builds a store whose active segment holds three
// durable events while the state counts two, and whose tail window is a run of
// malformed lines. Only the bounded fallback scan can find the last event, so
// an interrupted recovery has no tail it can trust.
func staleStateWithMalformedTail(t *testing.T) (*Store, string) {
	t.Helper()

	s := &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     256 << 10,
		SegmentBytes: 64 << 10,
		Retention:    24 * time.Hour,
	}
	for range 3 {
		mustAppend(t, s, baseEvent())
	}

	state, ok, err := readState(s.Dir)
	if err != nil || !ok {
		t.Fatalf("readState: %v ok=%v", err, ok)
	}
	// Crash image: event 3 is durable in its segment but the state write did
	// not land.
	state.LastSequence = 2
	state.NextSequence = 3

	state.ActiveBytes = 1
	if err := writeState(s.Dir, state); err != nil {
		t.Fatal(err)
	}

	segments := segmentsOf(t, s)
	active := segments[len(segments)-1]
	// More malformed bytes than the tail window, so the newest valid event is
	// only reachable through the bounded fallback scan.
	garbage := strings.Repeat("{broken\n", 2*maxEventBytes/len("{broken\n")+1)
	appendSegmentBytes(t, active.path, []byte(garbage))

	return s, active.path
}

// fileSize is the current size of path.
func fileSize(t *testing.T, path string) int64 {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	return info.Size()
}
