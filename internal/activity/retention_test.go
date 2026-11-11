package activity

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func retentionStore(t *testing.T, retention time.Duration) *Store {
	t.Helper()
	return &Store{
		Dir:          t.TempDir() + "/activity",
		MaxBytes:     2 * maxEventBytes,
		SegmentBytes: maxEventBytes,
		Retention:    retention,
	}
}

func labelledEvent(label string) Event {
	return Event{
		Source:     SourceJob,
		Action:     "backup.full",
		Outcome:    OutcomeSucceeded,
		ActorKind:  ActorSystem,
		ActorLabel: label,
	}
}

func TestRetentionByteBudget(t *testing.T) {
	s := retentionStore(t, 24*time.Hour)
	label := strings.Repeat("a", 100)
	const total = 80
	for i := 0; i < total; i++ {
		mustAppend(t, s, labelledEvent(label))
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.UsedBytes > health.MaxBytes {
		t.Fatalf("used bytes %d > budget %d", health.UsedBytes, health.MaxBytes)
	}
	if health.Segments > 3 {
		t.Fatalf("segments = %d, want at most 3", health.Segments)
	}
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) == 0 {
		t.Fatal("retention kept no events")
	}
	if !page.Coverage.Partial {
		t.Fatalf("retention loss not reported: %+v", page.Coverage)
	}
	if page.Coverage.FirstRetainedAt == "" {
		t.Fatal("first retained timestamp missing")
	}
	if page.Events[0].Sequence <= 1 {
		t.Fatalf("oldest sequence = %d, want later than 1", page.Events[0].Sequence)
	}
}

func TestCursorExpiredAfterRotation(t *testing.T) {
	s := retentionStore(t, time.Hour)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, labelledEvent(strings.Repeat("a", 100)))
	}
	first, err := s.Query(context.Background(), Query{Limit: 2})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page has no cursor")
	}
	expiring := &Store{Dir: s.Dir, MaxBytes: s.MaxBytes, SegmentBytes: s.SegmentBytes, Retention: time.Nanosecond}
	for i := 0; i < 80; i++ {
		mustAppend(t, expiring, labelledEvent(strings.Repeat("b", 100)))
	}
	q := Query{Limit: 2, Cursor: first.NextCursor}
	page, err := expiring.Query(context.Background(), q)
	if err != nil {
		t.Fatalf("expired cursor Query returned error: %v", err)
	}
	if !page.Coverage.CursorExpired {
		t.Fatalf("cursor did not expire: %+v", page.Coverage)
	}
	if !page.Coverage.Partial {
		t.Fatalf("expired cursor page not partial: %+v", page.Coverage)
	}
	if len(page.Events) != 0 {
		t.Fatalf("expired cursor silently returned %d events", len(page.Events))
	}
	if page.NextCursor != "" {
		t.Fatalf("expired cursor offered a next cursor: %q", page.NextCursor)
	}
	fresh, err := expiring.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("fresh Query: %v", err)
	}
	if !fresh.Coverage.Partial || fresh.Coverage.FirstRetainedAt == "" {
		t.Fatalf("fresh query after retention = %+v", fresh.Coverage)
	}
	if len(fresh.Events) == 0 || fresh.Events[0].Sequence <= 3 {
		t.Fatalf("retained events = %+v", fresh.Events)
	}
}

func TestRetentionByTime(t *testing.T) {
	s := retentionStore(t, time.Hour)
	old := time.Now().UTC().Add(-2 * time.Hour)
	label := strings.Repeat("c", 100)
	for i := 0; i < 40; i++ {
		mustAppend(t, s, Event{
			Source:     SourceJob,
			Action:     "backup.full",
			Outcome:    OutcomeSucceeded,
			ActorKind:  ActorSystem,
			ActorLabel: label,
			OccurredAt: old.Format(time.RFC3339),
		})
	}
	for i := 0; i < 40; i++ {
		mustAppend(t, s, labelledEvent(label))
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.FirstRetainedAt == "" {
		t.Fatal("first retained timestamp missing")
	}
	first, err := time.Parse(time.RFC3339Nano, health.FirstRetainedAt)
	if err != nil {
		t.Fatal(err)
	}
	if first.Before(time.Now().UTC().Add(-time.Hour)) {
		t.Fatalf("expired events retained: first = %v", first)
	}
	page, err := s.Query(context.Background(), Query{Since: "1h", Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) == 0 {
		t.Fatal("no recent events after time retention")
	}
	for _, ev := range page.Events {
		at, err := time.Parse(time.RFC3339Nano, ev.OccurredAt)
		if err != nil {
			t.Fatal(err)
		}
		if at.Before(time.Now().UTC().Add(-time.Hour)) {
			t.Fatalf("expired event returned: %v", at)
		}
	}
}

func TestRetentionRefusesToDeleteActiveContent(t *testing.T) {
	// A single event larger than any remaining room still fits a fresh
	// segment, and retention never removes the segment it is writing.
	s := retentionStore(t, time.Nanosecond)
	assigned := mustAppend(t, s, labelledEvent(strings.Repeat("d", 100)))
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 1 || page.Events[0].EventID != assigned.EventID {
		t.Fatalf("active event was lost: %+v", page.Events)
	}
}

func TestStoreFullIsRefusedNotSilentlyDropped(t *testing.T) {
	// A configuration that cannot hold a single encoded event is invalid
	// rather than an append that loses data.
	s := &Store{Dir: t.TempDir() + "/activity", MaxBytes: maxEventBytes / 2, SegmentBytes: maxEventBytes / 2, Retention: time.Hour}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errConfig) {
		t.Fatalf("Append error = %v, want errConfig", err)
	}
}

func TestRetentionKeepsFreshEventsBeforeBackdatedTail(t *testing.T) {
	s := retentionStore(t, time.Hour)
	s.MaxBytes = 256 << 10
	now := time.Now().UTC()
	ev := baseEvent()
	ev.OccurredAt = now.Format(time.RFC3339Nano)
	fresh := mustAppend(t, s, ev)
	ev.OccurredAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	mustAppend(t, s, ev)
	segments := segmentsOf(t, s)
	if len(segments) != 1 {
		t.Fatalf("setup has %d segments, want 1", len(segments))
	}
	if newest, ok := retentionTime(context.Background(), segments[0], s.SegmentBytes, now); !ok || !newest.Equal(now) {
		t.Errorf("retention time = %v, %t; want newest occurrence %v", newest, ok, now)
	}
	ev.OccurredAt = now.Format(time.RFC3339Nano)
	mustAppend(t, s, ev)
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 || page.Events[0].EventID != fresh.EventID {
		t.Fatalf("backdated tail expired fresh evidence: %+v", page.Events)
	}
	// Force a size-based rotation with an old occurrence at the segment tail.
	// Pruning the sealed segment must also use its newest occurrence.
	ev.OccurredAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	for range 40 {
		mustAppend(t, s, ev)
	}
	page, err = s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) == 0 || page.Events[0].EventID != fresh.EventID {
		t.Fatal("rotation expired a segment that still contained fresh evidence")
	}
}

func TestRetentionDoesNotExpireIncompleteScan(t *testing.T) {
	s := retentionStore(t, time.Hour)
	ev := baseEvent()
	now := time.Now().UTC()
	ev.OccurredAt = now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	mustAppend(t, s, ev)
	seg := segmentsOf(t, s)[0]
	if segmentExpired(context.Background(), seg, seg.size-1, now.Add(-time.Hour), now) {
		t.Fatal("a bounded partial scan authorized deletion")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if segmentExpired(ctx, seg, seg.size, now.Add(-time.Hour), now) {
		t.Fatal("a cancelled scan authorized deletion")
	}
}
