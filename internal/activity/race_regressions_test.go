package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestStateReadBeforeSegmentListing reproduces the interleaving that used to
// fabricate loss: the reader has read state, then a healthy writer completes an
// append (segment first, state second), and only then does the reader list
// segments. State must be allowed to be behind the listing.
func TestStateReadBeforeSegmentListing(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	oldHook := afterStateReadHook
	t.Cleanup(func() { afterStateReadHook = oldHook })

	appended := false
	afterStateReadHook = func() {
		if appended {
			return
		}
		appended = true
		mustAppend(t, s, baseEvent())
	}
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	if page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("healthy interleaving reported loss: %+v", page.Coverage)
	}

	appended = false
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.LossDetected {
		t.Fatalf("healthy interleaving health reported loss: %+v", health)
	}
}

// TestFirstAppendRaceDoesNotInventMissingState covers the other interleaving:
// the reader observes no state, then the healthy first append publishes state
// and history before the listing. The read must recheck state instead of
// reporting lost history.
func TestFirstAppendRaceDoesNotInventMissingState(t *testing.T) {
	oldHook := afterStateReadHook
	t.Cleanup(func() { afterStateReadHook = oldHook })

	queryStore := testStore(t)
	if err := os.MkdirAll(queryStore.Dir, dirMode); err != nil {
		t.Fatal(err)
	}
	appended := false
	afterStateReadHook = func() {
		if appended {
			return
		}
		appended = true
		mustAppend(t, queryStore, baseEvent())
	}
	page, err := queryStore.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(page.Events))
	}
	if page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("first-append race invented loss: %+v", page.Coverage)
	}

	healthStore := testStore(t)
	if err := os.MkdirAll(healthStore.Dir, dirMode); err != nil {
		t.Fatal(err)
	}
	appended = false
	afterStateReadHook = func() {
		if appended {
			return
		}
		appended = true
		mustAppend(t, healthStore, baseEvent())
	}
	health, err := healthStore.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.LossDetected {
		t.Fatalf("first-append race invented health loss: %+v", health)
	}
}

// TestStateRecheckRelistsSegments covers a newer append landing between the
// first listing and the state recheck: the restored state must be paired with a
// fresh listing, not the stale one.
func TestStateRecheckRelistsSegments(t *testing.T) {
	oldAfter, oldBefore := afterStateReadHook, beforeStateRecheckHook
	t.Cleanup(func() {
		afterStateReadHook = oldAfter
		beforeStateRecheckHook = oldBefore
	})

	check := func(t *testing.T, s *Store, read func() (Coverage, int, error)) {
		if err := os.MkdirAll(s.Dir, dirMode); err != nil {
			t.Fatal(err)
		}
		first := true
		second := true
		afterStateReadHook = func() {
			if !first {
				return
			}
			first = false
			mustAppend(t, s, baseEvent())
		}
		beforeStateRecheckHook = func() {
			if !second {
				return
			}
			second = false
			mustAppend(t, s, Event{Source: SourceJob, Action: "backup.verify", Outcome: OutcomeSucceeded})
		}
		cov, events, err := read()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		if events != 2 {
			t.Fatalf("events = %d, want 2", events)
		}
		if cov.LossDetected || cov.Partial {
			t.Fatalf("recheck re-list invented loss: %+v", cov)
		}
	}

	t.Run("query", func(t *testing.T) {
		s := testStore(t)
		check(t, s, func() (Coverage, int, error) {
			page, err := s.Query(context.Background(), Query{})
			return page.Coverage, len(page.Events), err
		})
	})
	t.Run("health", func(t *testing.T) {
		s := testStore(t)
		check(t, s, func() (Coverage, int, error) {
			health, err := s.Health(context.Background())
			if err != nil {
				return Coverage{}, 0, err
			}
			if health.LossDetected {
				t.Fatalf("health invented loss: %+v", health)
			}
			return Coverage{Partial: health.LossDetected}, 2, nil
		})
	})
}

// TestConcurrentReadsNeverInventLoss runs real writers and readers with no
// retention. Any LossDetected verdict must be backed by a concrete counter.
func TestConcurrentReadsNeverInventLoss(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const writers = 2
	const each = 150
	var wg sync.WaitGroup
	writerErr := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if _, err := s.Append(ctx, baseEvent()); err != nil {
					writerErr <- err
					return
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
		close(writerErr)
	}()
	var readers sync.WaitGroup
	violations := make(chan string, 8)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				page, err := s.Query(ctx, Query{Limit: 40})
				if err != nil {
					violations <- fmt.Sprintf("reader %d Query: %v", r, err)
					return
				}
				cov := page.Coverage
				if cov.LossDetected && !cov.CursorExpired &&
					cov.MalformedSegments == 0 && cov.MalformedEvents == 0 &&
					cov.SequenceGaps == 0 && cov.DroppedEvents == 0 {
					violations <- fmt.Sprintf("reader %d false Query loss: %+v", r, cov)
					return
				}
				health, err := s.Health(ctx)
				if err != nil {
					violations <- fmt.Sprintf("reader %d Health: %v", r, err)
					return
				}
				if health.LossDetected && health.MalformedSegments == 0 &&
					health.MalformedEvents == 0 && health.SequenceGaps == 0 &&
					health.DroppedEvents == 0 && health.OwnerOnly && !health.OverBudget {
					violations <- fmt.Sprintf("reader %d false Health loss: %+v", r, health)
					return
				}
			}
		}(r)
	}
	readers.Wait()
	if err := <-writerErr; err != nil {
		t.Fatalf("writer: %v", err)
	}
	select {
	case violation := <-violations:
		t.Fatal(violation)
	default:
	}
	page, err := s.Query(ctx, Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("final Query: %v", err)
	}
	if len(page.Events) != writers*each {
		t.Fatalf("final events = %d, want %d", len(page.Events), writers*each)
	}
	if page.Coverage.LossDetected || page.Coverage.Partial {
		t.Fatalf("final coverage reports loss: %+v", page.Coverage)
	}
}

// TestOversizeSegmentPagesEventuallyProgress lowers the read configuration
// below the stored segment size and checks that pages still advance through a
// whole segment instead of repeating one cursor.
func TestOversizeSegmentPagesEventuallyProgress(t *testing.T) {
	writer := &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     256 << 10,
		SegmentBytes: 64 << 10,
		Retention:    24 * time.Hour,
	}
	const total = 150
	for i := 0; i < total; i++ {
		mustAppend(t, writer, labelledEvent(strings.Repeat("o", 100)))
	}
	segments := segmentsOf(t, writer)
	if len(segments) != 1 || segments[0].size <= 16<<10 {
		t.Fatalf("setup: segments=%d size=%d, want one oversize segment", len(segments), segments[0].size)
	}
	reader := &Store{Dir: writer.Dir, MaxBytes: maxEventBytes, SegmentBytes: maxEventBytes, Retention: 24 * time.Hour}
	// The old reader scanned only its 16 KiB budget, returned a cursor at the
	// cut, and then re-read the same budget prefix forever. One whole segment
	// per page must expose every event.
	page, err := reader.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != total {
		t.Fatalf("whole-segment page returned %d events, want %d", len(page.Events), total)
	}
	if page.NextCursor != "" || page.Coverage.Truncated {
		t.Fatalf("whole-segment page stalled: cursor=%q coverage=%+v", page.NextCursor, page.Coverage)
	}
	if page.Coverage.BytesScanned != segments[0].size {
		t.Fatalf("first page bytes scanned = %d, want whole segment %d", page.Coverage.BytesScanned, segments[0].size)
	}

	// A small limit must still page through the same oversize segment without
	// repeating a cursor.
	seen := map[uint64]bool{}
	cursors := map[string]bool{}
	page, err = reader.Query(context.Background(), Query{Limit: 20})
	if err != nil {
		t.Fatalf("paged Query: %v", err)
	}
	pages := 0
	for {
		pages++
		if pages > 2*total {
			t.Fatalf("pagination did not terminate after %d pages", pages)
		}
		for _, ev := range page.Events {
			if seen[ev.Sequence] {
				t.Fatalf("duplicate sequence %d", ev.Sequence)
			}
			seen[ev.Sequence] = true
		}
		if page.NextCursor == "" {
			break
		}
		if cursors[page.NextCursor] {
			t.Fatalf("cursor repeated: %q", page.NextCursor)
		}
		cursors[page.NextCursor] = true
		page, err = reader.Query(context.Background(), Query{Limit: 20, Cursor: page.NextCursor})
		if err != nil {
			t.Fatalf("cursor Query: %v", err)
		}
	}
	if len(seen) != total {
		t.Fatalf("pages exposed %d events, want %d", len(seen), total)
	}
	for seq := uint64(1); seq <= total; seq++ {
		if !seen[seq] {
			t.Fatalf("sequence %d unreachable", seq)
		}
	}
}

// TestOversizeSegmentBeyondCapRefusesContinuation checks the absolute page cap:
// when a single segment cannot be read far enough to advance the cursor, the
// page is explicitly truncated/partial with no continuation.
func TestOversizeSegmentBeyondCapRefusesContinuation(t *testing.T) {
	writer := &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     256 << 10,
		SegmentBytes: 32 << 10,
		Retention:    24 * time.Hour,
	}
	for i := 0; i < 60; i++ {
		mustAppend(t, writer, labelledEvent(strings.Repeat("p", 100)))
	}
	segments := segmentsOf(t, writer)
	if len(segments) != 1 || segments[0].size <= 4096 {
		t.Fatalf("setup: segments=%d size=%d", len(segments), segments[0].size)
	}
	oldCap := maxPageBytes
	maxPageBytes = 4096
	t.Cleanup(func() { maxPageBytes = oldCap })
	reader := &Store{Dir: writer.Dir, MaxBytes: maxEventBytes, SegmentBytes: maxEventBytes, Retention: 24 * time.Hour}
	first, err := reader.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !first.Coverage.Truncated || first.NextCursor == "" || len(first.Events) == 0 {
		t.Fatalf("first capped page = %d events, truncated=%v cursor=%q", len(first.Events), first.Coverage.Truncated, first.NextCursor)
	}
	second, err := reader.Query(context.Background(), Query{Limit: MaxLimit, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("cursor Query: %v", err)
	}
	if second.NextCursor != "" {
		t.Fatalf("capped scan repeated a cursor %q", second.NextCursor)
	}
	if len(second.Events) != 0 || !second.Coverage.Truncated || !second.Coverage.Partial {
		t.Fatalf("capped page did not refuse honestly: %d events, %+v", len(second.Events), second.Coverage)
	}
}

// TestListSegmentsSkipsVanishedEntry makes a retention delete race the
// enumeration deterministically.
func TestListSegmentsSkipsVanishedEntry(t *testing.T) {
	oldHook := dirEntryHook
	t.Cleanup(func() { dirEntryHook = oldHook })

	// Phase A: the unit listing skips the vanished entry and flags the race.
	a := testStore(t)
	mustAppend(t, a, baseEvent())
	aSegments := segmentsOf(t, a)
	dirEntryHook = func(name string) {
		if name == aSegments[0].name {
			_ = os.Remove(aSegments[0].path)
		}
	}
	listed, more, raced, err := listSegments(a.Dir)
	if err != nil {
		t.Fatalf("listSegments: %v", err)
	}
	if !raced || more || len(listed) != 0 {
		t.Fatalf("list result = %d segments, more=%v raced=%v", len(listed), more, raced)
	}

	// Phase B: the same race through Query must not become a read error.
	b := testStore(t)
	mustAppend(t, b, baseEvent())
	bSegments := segmentsOf(t, b)
	dirEntryHook = func(name string) {
		if name == bSegments[0].name {
			_ = os.Remove(bSegments[0].path)
		}
	}
	page, err := b.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query after raced delete: %v", err)
	}
	if !page.Coverage.Truncated {
		t.Fatalf("raced delete not reported as truncated: %+v", page.Coverage)
	}
	if page.Coverage.MalformedSegments != 0 {
		t.Fatalf("raced delete counted as malformed: %+v", page.Coverage)
	}

	// Phase C: Health stays inspectable rather than reporting the directory
	// unreadable.
	c := testStore(t)
	mustAppend(t, c, baseEvent())
	cSegments := segmentsOf(t, c)
	dirEntryHook = func(name string) {
		if name == cSegments[0].name {
			_ = os.Remove(cSegments[0].path)
		}
	}
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health after raced delete: %v", err)
	}
	if health.MalformedSegments != 0 {
		t.Fatalf("raced delete counted as malformed by Health: %+v", health)
	}
	if !health.Readable {
		t.Fatalf("raced delete made the store unreadable: %+v", health)
	}
	dirEntryHook = oldHook
	// Removing the only segment is not a retention race: retention never
	// deletes the segment it just wrote. The state still counts that event, so
	// the store refuses to append rather than rebaseline the counters.
	if _, err := c.Append(context.Background(), baseEvent()); !errors.Is(err, errHistoryMissing) {
		t.Fatalf("Append after losing the only segment = %v, want errHistoryMissing", err)
	}
}

func TestClassifyQueryError(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	if err := ValidateQuery(Query{Limit: -1}); !errors.Is(err, errQueryOutOfRange) {
		t.Fatalf("negative limit error = %v", err)
	}
	_, configErr := (&Store{Dir: "relative"}).Query(context.Background(), Query{})
	_, appendErr := s.Append(context.Background(), Event{Source: SourceRPC, Action: "", Outcome: OutcomeStarted})
	lost := testStore(t)
	mustAppend(t, lost, baseEvent())
	if err := os.Remove(filepath.Join(lost.Dir, stateName)); err != nil {
		t.Fatal(err)
	}
	_, missingStateErr := lost.Append(context.Background(), baseEvent())
	corrupt := testStore(t)
	mustAppend(t, corrupt, baseEvent())
	if err := os.WriteFile(filepath.Join(corrupt.Dir, stateName), []byte("not json"), fileMode); err != nil {
		t.Fatal(err)
	}
	_, corruptStateErr := corrupt.Append(context.Background(), baseEvent())
	cases := []struct {
		name   string
		err    error
		reason string
		ok     bool
	}{
		{"invalid query", ValidateQuery(Query{Action: "bad action"}), QueryReasonInvalid, true},
		{"out of range", ValidateQuery(Query{Limit: -1}), QueryReasonOutOfRange, true},
		{"tampered cursor", ValidateQuery(Query{Cursor: "c1.bm90LWpzb24"}), QueryReasonInvalidCursor, true},
		{"query invalid action", func() error { _, err := s.Query(context.Background(), Query{Action: "bad action"}); return err }(), QueryReasonInvalid, true},
		{"query negative limit", func() error { _, err := s.Query(context.Background(), Query{Limit: -1}); return err }(), QueryReasonOutOfRange, true},
		{"query tampered cursor", func() error { _, err := s.Query(context.Background(), Query{Cursor: "c1.bm90LWpzb24"}); return err }(), QueryReasonInvalidCursor, true},
		{"nil", nil, "", false},
		{"plain", errors.New("boom"), "", false},
		{"config", configErr, "", false},
		{"invalid append", appendErr, "", false},
		{"missing state", missingStateErr, "", false},
		{"corrupt state", corruptStateErr, "", false},
	}
	first, err := s.Query(context.Background(), Query{Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %v cursor=%q", err, first.NextCursor)
	}
	_, mismatch := s.Query(context.Background(), Query{Limit: 2, Action: "backup.full", Cursor: first.NextCursor})
	cases = append(cases, struct {
		name   string
		err    error
		reason string
		ok     bool
	}{"filter mismatch", mismatch, QueryReasonCursorMismatch, true})
	_, limitErr := s.Query(context.Background(), Query{Limit: 3, Cursor: first.NextCursor})
	cases = append(cases, struct {
		name   string
		err    error
		reason string
		ok     bool
	}{"limit mismatch", limitErr, QueryReasonCursorMismatch, true})
	// Unsafe store reads are not caller query errors.
	segments := segmentsOf(t, s)
	if err := os.Chmod(segments[0].path, 0o644); err != nil { //nolint:gosec // G302: the test needs an unsafe mode.
		t.Fatal(err)
	}
	_, unsafeErr := s.Query(context.Background(), Query{})
	if reason, ok := ClassifyQueryError(unsafeErr); ok || reason != "" {
		t.Fatalf("unsafe store classified as query error: %q %v", reason, ok)
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, ok := ClassifyQueryError(tc.err)
			if ok != tc.ok || reason != tc.reason {
				t.Fatalf("ClassifyQueryError = %q,%v want %q,%v (err %v)", reason, ok, tc.reason, tc.ok, tc.err)
			}
		})
	}
}
