package activity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// appendRawLine appends one raw line to the newest segment without touching
// the state file. It is test-only hostility/truncation injection.
func appendRawLine(t *testing.T, dir, line string) {
	t.Helper()
	segments, _, _, err := listSegments(dir)
	if err != nil || len(segments) == 0 {
		t.Fatalf("listSegments: %v (%d segments)", err, len(segments))
	}
	f, err := os.OpenFile(segments[len(segments)-1].path, os.O_WRONLY|os.O_APPEND, fileMode) //nolint:gosec // G304: a test store segment.
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(line); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func segmentNames(segments []segmentInfo) string {
	names := make([]string, 0, len(segments))
	for _, seg := range segments {
		names = append(names, seg.name)
	}
	return strings.Join(names, ",")
}

func TestBudgetCutIsTruncationNotCorruption(t *testing.T) {
	writer := &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     512 << 10,
		SegmentBytes: 32 << 10,
		Retention:    24 * time.Hour,
	}
	for i := 0; i < 80; i++ {
		mustAppend(t, writer, labelledEvent(strings.Repeat("q", 100)))
	}
	segments := segmentsOf(t, writer)
	if len(segments) != 1 || segments[0].size <= 16<<10 {
		t.Fatalf("setup: segments=%d size=%d, want one segment over 16 KiB", len(segments), segments[0].size)
	}
	// A lower budget than the stored segment size is a supported read.
	reader := &Store{Dir: writer.Dir, MaxBytes: maxEventBytes, SegmentBytes: maxEventBytes, Retention: 24 * time.Hour}
	page, err := reader.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	// One whole segment is read to make progress even though the configured
	// budget is lower. That is a complete page, not corruption.
	if page.Coverage.Truncated || page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("oversize-segment page = %+v", page.Coverage)
	}
	if page.Coverage.MalformedEvents != 0 || page.Coverage.MalformedSegments != 0 {
		t.Fatalf("budget cutoff fabricated corruption: %+v", page.Coverage)
	}
	if len(page.Events) != 80 {
		t.Fatalf("oversize-segment page returned %d events, want 80", len(page.Events))
	}
	oldCap := maxPageBytes
	maxPageBytes = 4096
	capped, err := reader.Query(context.Background(), Query{Limit: MaxLimit})
	maxPageBytes = oldCap
	if err != nil {
		t.Fatalf("capped Query: %v", err)
	}
	if !capped.Coverage.Truncated {
		t.Fatalf("hard page cap not reported as truncated: %+v", capped.Coverage)
	}
	if capped.Coverage.MalformedEvents != 0 || capped.Coverage.LossDetected {
		t.Fatalf("hard page cap fabricated corruption/loss: %+v", capped.Coverage)
	}
	if len(capped.Events) == 0 {
		t.Fatal("hard page cap returned no evidence")
	}
	health, err := reader.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.MalformedEvents != 0 || health.MalformedSegments != 0 {
		t.Fatalf("health fabricated corruption: %+v", health)
	}
	if health.LossDetected {
		t.Fatalf("health fabricated loss: %+v", health)
	}

	// A genuine torn tail is still corruption and remains visible.
	appendRawLine(t, writer.Dir, `{"schema":"vaire-activity/v1","event_id":"torn`)
	page, err = writer.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query with torn tail: %v", err)
	}
	if page.Coverage.MalformedEvents != 1 {
		t.Fatalf("torn tail malformed events = %d, want 1", page.Coverage.MalformedEvents)
	}
	if !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("torn tail not reported: %+v", page.Coverage)
	}
	if len(page.Events) != 80 {
		t.Fatalf("torn tail lost events: %d, want 80", len(page.Events))
	}
}

func TestMissingStateWithUndecodableNewestReportsLoss(t *testing.T) {
	s := &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     32 * maxEventBytes,
		SegmentBytes: maxEventBytes,
		Retention:    24 * time.Hour,
	}
	for i := 0; i < 40; i++ {
		mustAppend(t, s, labelledEvent(strings.Repeat("u", 100)))
	}
	segments := segmentsOf(t, s)
	if len(segments) < 2 {
		t.Fatalf("setup needs at least two segments, got %d", len(segments))
	}
	if err := os.Remove(filepath.Join(s.Dir, stateName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(segments[len(segments)-1].path, []byte("not json\nnot json\nnot json\n"), fileMode); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) == 0 {
		t.Fatal("undecodable newest segment hid all older evidence")
	}
	if page.Coverage.MalformedEvents != 3 {
		t.Fatalf("malformed events = %d, want 3", page.Coverage.MalformedEvents)
	}
	if !page.Coverage.Partial || !page.Coverage.LossDetected {
		t.Fatalf("lossy store returned a clean page: %+v", page.Coverage)
	}
	if page.Coverage.FirstRetainedAt == "" || page.Coverage.LastRetainedAt == "" {
		t.Fatalf("retained bounds missing: %+v", page.Coverage)
	}
	if page.Coverage.EventsScanned < len(page.Events) {
		t.Fatalf("scan counters inconsistent: %+v", page.Coverage)
	}
}

func TestDeletedEvidenceIsNotCleanEmpty(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	for _, seg := range segmentsOf(t, s) {
		if err := os.Remove(seg.path); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("events = %d, want 0", len(page.Events))
	}
	if !page.Coverage.Partial || !page.Coverage.LossDetected {
		t.Fatalf("deleted evidence reported as clean: %+v", page.Coverage)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.LossDetected || health.UsedBytes != 0 {
		t.Fatalf("deleted evidence health = %+v", health)
	}
}

func TestMissingStatePageHasNoUnboundCursor(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 5; i++ {
		mustAppend(t, s, baseEvent())
	}
	if err := os.Remove(filepath.Join(s.Dir, stateName)); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 2 || !page.Coverage.LimitReached {
		t.Fatalf("page = %d events, limitReached=%v", len(page.Events), page.Coverage.LimitReached)
	}
	if page.NextCursor != "" {
		t.Fatalf("missing state offered an unbound cursor %q", page.NextCursor)
	}
	if !page.Coverage.Partial || !page.Coverage.LossDetected {
		t.Fatalf("missing state page is clean: %+v", page.Coverage)
	}
}

func TestHealthReportsUnusableLock(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	lockPath := filepath.Join(s.Dir, lockName)
	if err := os.Chmod(lockPath, 0o644); err != nil { //nolint:gosec // G302: the test needs an unsafe lock mode.
		t.Fatal(err)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.OwnerOnly || health.Writable || !health.LossDetected {
		t.Fatalf("unsafe lock health = %+v", health)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeLock) {
		t.Fatalf("Append error = %v, want errUnsafeLock", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "lock-target")
	if err := os.WriteFile(target, nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	health, err = s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.OwnerOnly || health.Writable || !health.LossDetected {
		t.Fatalf("symlinked lock health = %+v", health)
	}
	if _, err := s.Append(context.Background(), baseEvent()); err == nil {
		t.Fatal("Append accepted a symlinked lock")
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	health, err = s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OwnerOnly || !health.Writable || health.LossDetected {
		t.Fatalf("missing lock health = %+v", health)
	}
	if _, err := os.Lstat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Health created the lock: %v", err)
	}
}

func TestFailedAppendKeepsHistoryAndRealRetainedBounds(t *testing.T) {
	s := retentionStore(t, 24*time.Hour)
	for i := 0; i < 100; i++ {
		mustAppend(t, s, labelledEvent(strings.Repeat("r", 100)))
	}
	beforeSegments := segmentsOf(t, s)
	beforeNames := segmentNames(beforeSegments)
	beforePage, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if beforePage.Coverage.FirstRetainedAt == "" {
		t.Fatal("setup has no retained window")
	}
	oldWrite := writeSegmentLine
	writeSegmentLine = func(string, []byte, bool) error { return errors.New("injected failure") }
	_, appendErr := s.Append(context.Background(), labelledEvent(strings.Repeat("r", 100)))
	writeSegmentLine = oldWrite
	if appendErr == nil {
		t.Fatal("Append swallowed the injected failure")
	}
	afterSegments := segmentsOf(t, s)
	if segmentNames(afterSegments) != beforeNames {
		t.Fatalf("failed append changed segments:\n before %s\n after  %s", beforeNames, segmentNames(afterSegments))
	}
	afterPage, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterPage.Events) != len(beforePage.Events) {
		t.Fatalf("failed append discarded history: %d -> %d events", len(beforePage.Events), len(afterPage.Events))
	}

	// A stale state that claims an ancient first event must not widen
	// coverage beyond the actual oldest segment.
	state, ok, err := readState(s.Dir)
	if err != nil || !ok {
		t.Fatalf("readState: %v ok=%v", err, ok)
	}
	state.FirstSequence = 1
	state.FirstEventAt = "2000-01-01T00:00:00.000000000Z"
	if err := writeState(s.Dir, state); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Since: "2020-01-01T00:00:00Z", Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	first, err := time.Parse(time.RFC3339Nano, page.Coverage.FirstRetainedAt)
	if err != nil {
		t.Fatalf("first retained: %v", err)
	}
	if first.Year() < 2026 {
		t.Fatalf("stale state widened retained coverage: %s", page.Coverage.FirstRetainedAt)
	}
	if !page.Coverage.Partial {
		t.Fatalf("old window after retention is not partial: %+v", page.Coverage)
	}
	if page.Coverage.MalformedEvents != 0 || page.Coverage.SequenceGaps != 0 {
		t.Fatalf("retention loss misreported as malformed evidence: %+v", page.Coverage)
	}
	if len(page.Events) != 2 || !page.Coverage.LimitReached || page.NextCursor == "" {
		t.Fatalf("limit-2 page = %d events, limitReached=%v cursor=%q", len(page.Events), page.Coverage.LimitReached, page.NextCursor)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	healthFirst, err := time.Parse(time.RFC3339Nano, health.FirstRetainedAt)
	if err != nil || healthFirst.Year() < 2026 {
		t.Fatalf("health widened retained coverage: %q (%v)", health.FirstRetainedAt, err)
	}
}

func TestStateBehindSegmentsStillReadsDurableEvidence(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	state, ok, err := readState(s.Dir)
	if err != nil || !ok {
		t.Fatalf("readState: %v ok=%v", err, ok)
	}
	// Crash image: the third event is durable in its segment but the state
	// write did not land. Counters and active size are one event behind.
	state.LastSequence = 2
	state.NextSequence = 3
	state.ActiveBytes = 1
	if err := writeState(s.Dir, state); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sequences(page.Events); !equalSeq(got, []uint64{1, 2, 3}) {
		t.Fatalf("stale state hid durable evidence: %v", got)
	}
	next := mustAppend(t, s, baseEvent())
	if next.Sequence != 4 {
		t.Fatalf("sequence after stale-state recovery = %d, want 4", next.Sequence)
	}
}

func TestAppendRejectsFutureOccurrence(t *testing.T) {
	s := testStore(t)
	futureTime := time.Now().UTC().Add(365 * 24 * time.Hour)
	future := futureTime.Format(time.RFC3339)
	_, err := s.Append(context.Background(), Event{
		Source:     SourceRPC,
		Action:     "backup.full",
		Outcome:    OutcomeStarted,
		OccurredAt: future,
	})
	if !errors.Is(err, errInvalidEvent) {
		t.Fatalf("future append error = %v, want errInvalidEvent", err)
	}
	if strings.Contains(err.Error(), future) || strings.Contains(err.Error(), "2027") {
		t.Fatalf("future append error echoed the value: %v", err)
	}
	if _, statErr := os.Stat(s.Dir); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("future append created the store: %v", statErr)
	}
	// ValidateEvent is clock-free, so it accepts a future timestamp that Append
	// refuses; the policy belongs to the local write.
	if err := ValidateEvent(Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, OccurredAt: future}); err != nil {
		t.Fatalf("ValidateEvent rejected a parseable future time: %v", err)
	}
	if _, err := s.Append(context.Background(), Event{
		Source:     SourceRPC,
		Action:     "backup.full",
		Outcome:    OutcomeStarted,
		OccurredAt: time.Now().UTC().Add(time.Minute).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("append within skew: %v", err)
	}
}

func TestRetentionTimeIgnoresFutureTimestamp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	if err := os.Mkdir(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	ev := Event{
		Schema:     Schema,
		EventID:    "evt-0123456789abcdef0123456789abcdef",
		OccurredAt: formatTime(now.Add(365 * 24 * time.Hour)),
		Sequence:   1,
		Source:     SourceRPC,
		Action:     "backup.full",
		Outcome:    OutcomeStarted,
	}
	line, err := encodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, segmentFileName(1)), line, fileMode); err != nil {
		t.Fatal(err)
	}
	segments, _, _, err := listSegments(dir)
	if err != nil || len(segments) != 1 {
		t.Fatalf("listSegments: %v (%d)", err, len(segments))
	}
	got, ok := retentionTime(context.Background(), segments[0], maxEventBytes, now)
	if !ok {
		t.Fatal("retentionTime gave no time")
	}
	if got.After(now) {
		t.Fatalf("future timestamp pinned retention: %v", got)
	}
	if !got.Equal(segments[0].modTime) {
		t.Fatalf("retention time = %v, want segment file time %v", got, segments[0].modTime)
	}
}

func TestMalformedLineInsideLimitScanIsCounted(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	segments := segmentsOf(t, s)
	raw, err := os.ReadFile(segments[0].path) //nolint:gosec // G304: a test store segment.
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("setup lines = %d, want 3", len(lines))
	}
	corrupt := lines[0] + "\n{broken\n" + strings.Join(lines[1:], "\n") + "\n"
	if err := os.WriteFile(segments[0].path, []byte(corrupt), fileMode); err != nil { //nolint:gosec // G703: a path inside the test store.
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.MalformedEvents != 1 {
		t.Fatalf("malformed events in limit scan = %d, want 1", page.Coverage.MalformedEvents)
	}
	if len(page.Events) != 2 || !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("limit page = %d events, coverage %+v", len(page.Events), page.Coverage)
	}
	if page.Coverage.EventsScanned != 3 {
		t.Fatalf("events scanned = %d, want 3", page.Coverage.EventsScanned)
	}
}

func TestQueryLimitStopsScanningAndSkipsOldSegments(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 520; i++ {
		mustAppend(t, s, baseEvent())
	}
	segments := segmentsOf(t, s)
	if len(segments) < 4 {
		t.Fatalf("setup has %d segments, want several", len(segments))
	}
	page, err := s.Query(context.Background(), Query{Limit: 5})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 5 || page.NextCursor == "" || !page.Coverage.LimitReached {
		t.Fatalf("limit page = %d events cursor=%q limitReached=%v", len(page.Events), page.NextCursor, page.Coverage.LimitReached)
	}
	if page.Coverage.EventsScanned > 6 {
		t.Fatalf("limit 5 scanned %d events, want at most 6", page.Coverage.EventsScanned)
	}
	if page.Coverage.SegmentsScanned != 1 {
		t.Fatalf("limit 5 scanned %d segments, want 1", page.Coverage.SegmentsScanned)
	}
	if page.Coverage.BytesScanned <= 0 || page.Coverage.BytesScanned > 4096 {
		t.Fatalf("early-return bytes = %d, want >0 and small", page.Coverage.BytesScanned)
	}

	bigPage, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("big Query: %v", err)
	}
	if len(bigPage.Events) != MaxLimit || bigPage.NextCursor == "" {
		t.Fatalf("big page = %d events cursor=%q", len(bigPage.Events), bigPage.NextCursor)
	}
	next, err := s.Query(context.Background(), Query{Limit: MaxLimit, Cursor: bigPage.NextCursor})
	if err != nil {
		t.Fatalf("cursor Query: %v", err)
	}
	if len(next.Events) != 20 || next.Events[0].Sequence != 501 || next.Events[19].Sequence != 520 {
		t.Fatalf("tail page sequences = %v", sequences(next.Events))
	}
	if next.Coverage.EventsScanned > 100 {
		t.Fatalf("cursor page decoded %d events, want only the tail", next.Coverage.EventsScanned)
	}
	if next.Coverage.SegmentsScanned > 2 {
		t.Fatalf("cursor page scanned %d segments, want at most 2", next.Coverage.SegmentsScanned)
	}

	rare := mustAppend(t, s, Event{Source: SourceJob, Action: "rare.action", Outcome: OutcomeSucceeded})
	sparse, err := s.Query(context.Background(), Query{Action: "rare.action", Limit: 5})
	if err != nil {
		t.Fatalf("sparse Query: %v", err)
	}
	if len(sparse.Events) != 1 || sparse.Events[0].Sequence != rare.Sequence {
		t.Fatalf("sparse events = %v", sequences(sparse.Events))
	}
	if sparse.Coverage.EventsScanned != 521 {
		t.Fatalf("sparse filter scanned %d events, want 521", sparse.Coverage.EventsScanned)
	}
	totalBytes := int64(0)
	for _, seg := range segmentsOf(t, s) {
		totalBytes += seg.size
	}
	if sparse.Coverage.BytesScanned != totalBytes {
		t.Fatalf("sparse bytes scanned = %d, want %d", sparse.Coverage.BytesScanned, totalBytes)
	}
	if sparse.Coverage.LimitReached || sparse.Coverage.Truncated {
		t.Fatalf("sparse coverage = %+v", sparse.Coverage)
	}
}

func TestStoredMaxSequenceIsMalformed(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	appendRawLine(t, s.Dir, `{"schema":"vaire-activity/v1","event_id":"evt-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","occurred_at":"2026-01-01T00:00:00.000000000Z","sequence":18446744073709551615,"source":"rpc","action":"backup.full","outcome":"started","actor_verified":false}`+"\n")
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.MalformedEvents != 1 {
		t.Fatalf("max-sequence record malformed events = %d, want 1", page.Coverage.MalformedEvents)
	}
	if page.Coverage.SequenceGaps != 0 {
		t.Fatalf("max-sequence record fabricated %d gaps", page.Coverage.SequenceGaps)
	}
	if !page.Coverage.LossDetected || len(page.Events) != 1 {
		t.Fatalf("page = %d events, coverage %+v", len(page.Events), page.Coverage)
	}
	next := mustAppend(t, s, baseEvent())
	if next.Sequence != 2 {
		t.Fatalf("sequence after malformed max = %d, want 2", next.Sequence)
	}
	page, err = s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if got := sequences(page.Events); !equalSeq(got, []uint64{1, 2}) {
		t.Fatalf("sequences = %v", got)
	}
}

func TestWindowBeforeIntactHistoryIsNotPartial(t *testing.T) {
	s := testStore(t)
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	mustAppend(t, s, Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded, OccurredAt: t0.Format(time.RFC3339)})
	page, err := s.Query(context.Background(), Query{Until: t0.Add(-time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 || page.Coverage.Partial || page.Coverage.LossDetected {
		t.Fatalf("window before intact history = %+v", page.Coverage)
	}
	page, err = s.Query(context.Background(), Query{Since: t0.Add(-time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Coverage.Partial {
		t.Fatalf("since before intact history = %+v", page.Coverage)
	}
	// Remove the first segment through real retention, then the same window is
	// genuinely incomplete.
	retained := &Store{Dir: filepath.Join(t.TempDir(), "activity"), MaxBytes: 4 * maxEventBytes, SegmentBytes: maxEventBytes, Retention: time.Hour}
	old := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	mustAppend(t, retained, Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded, OccurredAt: old.Format(time.RFC3339)})
	mustAppend(t, retained, Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded})
	page, err = retained.Query(context.Background(), Query{Until: old.Add(time.Minute).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Coverage.Partial {
		t.Fatalf("window before removed evidence is not partial: %+v", page.Coverage)
	}
}

func TestCursorBoundToStoreInstance(t *testing.T) {
	a := testStore(t)
	b := testStore(t)
	for i := 0; i < 5; i++ {
		mustAppend(t, a, baseEvent())
		mustAppend(t, b, baseEvent())
	}
	first, err := a.Query(context.Background(), Query{Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %v cursor=%q", err, first.NextCursor)
	}
	if _, err := b.Query(context.Background(), Query{Limit: 2, Cursor: first.NextCursor}); !errors.Is(err, errCursorMismatch) {
		t.Fatalf("cross-store cursor error = %v, want errCursorMismatch", err)
	}
	next, err := a.Query(context.Background(), Query{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("same-store cursor: %v", err)
	}
	if got := sequences(next.Events); !equalSeq(got, []uint64{3, 4}) {
		t.Fatalf("same-store page = %v", got)
	}
}

func TestCursorWithMissingStateIsExplicitlyExpired(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 5; i++ {
		mustAppend(t, s, baseEvent())
	}
	first, err := s.Query(context.Background(), Query{Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %v cursor=%q", err, first.NextCursor)
	}
	if err := os.Remove(filepath.Join(s.Dir, stateName)); err != nil {
		t.Fatal(err)
	}
	page, err := s.Query(context.Background(), Query{Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("cursor Query: %v", err)
	}
	if !page.Coverage.CursorExpired || !page.Coverage.Partial || !page.Coverage.LossDetected {
		t.Fatalf("missing-state cursor coverage = %+v", page.Coverage)
	}
	if len(page.Events) != 0 || page.NextCursor != "" {
		t.Fatalf("missing-state cursor reset silently: %d events, cursor %q", len(page.Events), page.NextCursor)
	}
}
