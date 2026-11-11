package activity

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	return &Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     256 << 10,
		SegmentBytes: 16 << 10,
		Retention:    24 * time.Hour,
	}
}

func baseEvent() Event {
	return Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted}
}

func TestStoreFiltersUnderscoreDatabaseAcrossPages(t *testing.T) {
	s := testStore(t)
	ev := baseEvent()
	ev.Database = "_app"
	first := mustAppend(t, s, ev)
	ev.Database = "other"
	mustAppend(t, s, ev)
	ev.Database = "_app"
	last := mustAppend(t, s, ev)
	q := Query{Database: "_app", Limit: 1}
	page, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventID != first.EventID || page.NextCursor == "" {
		t.Fatalf("first filtered page = %+v", page)
	}
	q.Cursor = page.NextCursor
	page, err = s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].EventID != last.EventID || page.NextCursor != "" {
		t.Fatalf("last filtered page = %+v", page)
	}
}

func mustAppend(t *testing.T, s *Store, ev Event) Event {
	t.Helper()
	assigned, err := s.Append(context.Background(), ev)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	return assigned
}

func segmentsOf(t *testing.T, s *Store) []segmentInfo {
	t.Helper()
	segs, _, _, err := listSegments(s.Dir)
	if err != nil {
		t.Fatalf("listSegments: %v", err)
	}
	return segs
}

func TestDefaultStoreAndConfig(t *testing.T) {
	def := DefaultStore()
	if def.Dir != DefaultDir || def.MaxBytes != defaultMaxBytes ||
		def.SegmentBytes != defaultSegmentBytes || def.Retention != defaultRetention {
		t.Fatalf("DefaultStore = %+v", def)
	}
	cfg, err := (&Store{}).config()
	if err != nil {
		t.Fatalf("zero Store config: %v", err)
	}
	if cfg.dir != DefaultDir || cfg.maxBytes != defaultMaxBytes ||
		cfg.segmentBytes != defaultSegmentBytes || cfg.retention != defaultRetention {
		t.Fatalf("zero Store config = %+v", cfg)
	}
	for _, bad := range []*Store{
		{Dir: "relative/path"},
		{Dir: "/"},
		{Dir: t.TempDir(), MaxBytes: 1, SegmentBytes: maxEventBytes},
		{Dir: t.TempDir(), MaxBytes: maxEventBytes, SegmentBytes: maxEventBytes - 1},
		{Dir: t.TempDir(), Retention: -time.Second},
	} {
		if _, err := bad.config(); !errors.Is(err, errConfig) {
			t.Fatalf("config(%+v) error = %v, want errConfig", bad, err)
		}
	}
}

func TestAppendAssignsStoreFieldsAndPreservesTime(t *testing.T) {
	s := testStore(t)
	occurredAt := time.Now().UTC().Add(-2 * time.Minute).Truncate(time.Second)
	occurred := occurredAt.Format(time.RFC3339)
	assigned := mustAppend(t, s, Event{
		Source:      SourceRPC,
		Action:      "backup.full",
		Outcome:     OutcomeSucceeded,
		OccurredAt:  occurred,
		EventID:     "client-event-id",
		Sequence:    42,
		Schema:      Schema,
		RequestID:   "req-1",
		OperationID: "op-1",
	})
	if assigned.Schema != Schema {
		t.Fatalf("schema = %q", assigned.Schema)
	}
	if assigned.Sequence != 1 {
		t.Fatalf("sequence = %d, want 1", assigned.Sequence)
	}
	if !strings.HasPrefix(assigned.EventID, "evt-") || assigned.EventID == "client-event-id" {
		t.Fatalf("event id = %q", assigned.EventID)
	}
	got, err := time.Parse(time.RFC3339Nano, assigned.OccurredAt)
	if err != nil {
		t.Fatalf("occurred_at = %q: %v", assigned.OccurredAt, err)
	}
	if !got.Equal(occurredAt) {
		t.Fatalf("occurred_at changed: %v", got)
	}
	second := mustAppend(t, s, baseEvent())
	if second.Sequence != 2 {
		t.Fatalf("second sequence = %d, want 2", second.Sequence)
	}
	info, err := os.Stat(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != dirMode {
		t.Fatalf("dir mode = %04o, want %04o", info.Mode().Perm(), dirMode)
	}
	segs := segmentsOf(t, s)
	if len(segs) != 1 {
		t.Fatalf("segments = %d, want 1", len(segs))
	}
	for _, path := range []string{segs[0].path, filepath.Join(s.Dir, stateName), filepath.Join(s.Dir, lockName)} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if info.Mode().Perm() != fileMode {
			t.Fatalf("%s mode = %04o, want %04o", path, info.Mode().Perm(), fileMode)
		}
	}
	raw, err := os.ReadFile(segs[0].path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("segment has %d lines, want 2", len(lines))
	}
	var stored Event
	if err := json.Unmarshal([]byte(lines[0]), &stored); err != nil {
		t.Fatalf("stored event: %v", err)
	}
	if stored.Sequence != 1 || stored.OccurredAt != assigned.OccurredAt {
		t.Fatalf("stored event = %+v", stored)
	}
}

func TestAppendDefaultsOccurredAt(t *testing.T) {
	s := testStore(t)
	before := time.Now().UTC().Add(-time.Second)
	assigned := mustAppend(t, s, baseEvent())
	after := time.Now().UTC().Add(time.Second)
	got, err := time.Parse(time.RFC3339Nano, assigned.OccurredAt)
	if err != nil {
		t.Fatalf("occurred_at: %v", err)
	}
	if got.Before(before) || got.After(after) {
		t.Fatalf("occurred_at %v outside [%v,%v]", got, before, after)
	}
}

func TestAppendUniqueEventIDs(t *testing.T) {
	s := testStore(t)
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		ev := mustAppend(t, s, baseEvent())
		if seen[ev.EventID] {
			t.Fatalf("duplicate event id %q", ev.EventID)
		}
		seen[ev.EventID] = true
	}
}

func TestQueryFiltersAndOrder(t *testing.T) {
	s := testStore(t)
	events := []Event{
		{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, Database: "app", OperationID: "op-1", RequestID: "req-1"},
		{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeSucceeded, Database: "app", OperationID: "op-1", RequestID: "req-2"},
		{Source: SourceJob, Action: "backup.full", Outcome: OutcomeFailed, Database: "app", JobID: "job-1"},
		{Source: SourceOperator, Action: "cert.renew", Outcome: OutcomeSkipped},
		{Source: SourceService, Action: "postgres.reload", Outcome: OutcomeSucceeded},
		{Source: SourceStep, Action: "postgres.start", Outcome: OutcomeSucceeded, Database: "other"},
	}
	for _, ev := range events {
		mustAppend(t, s, ev)
	}
	tests := []struct {
		name  string
		query Query
		want  []uint64
	}{
		{"action", Query{Action: "backup.full"}, []uint64{1, 2, 3}},
		{"outcome", Query{Outcome: OutcomeSucceeded}, []uint64{2, 5, 6}},
		{"database", Query{Database: "app"}, []uint64{1, 2, 3}},
		{"operation", Query{OperationID: "op-1"}, []uint64{1, 2}},
		{"request", Query{RequestID: "req-1"}, []uint64{1}},
		{"job", Query{JobID: "job-1"}, []uint64{3}},
		{"limit", Query{Limit: 2}, []uint64{1, 2}},
		{"none", Query{Action: "nothing.here"}, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			page, err := s.Query(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if !page.Coverage.Available {
				t.Fatal("coverage unavailable")
			}
			var got []uint64
			for _, ev := range page.Events {
				got = append(got, ev.Sequence)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("sequences = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("sequences = %v, want %v", got, tc.want)
				}
			}
			if page.Coverage.EventsReturned != len(page.Events) {
				t.Fatalf("events_returned = %d", page.Coverage.EventsReturned)
			}
		})
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != len(events) {
		t.Fatalf("default query returned %d events", len(page.Events))
	}
	capped, err := s.Query(context.Background(), Query{Limit: MaxLimit + 1000})
	if err != nil {
		t.Fatalf("capped limit query: %v", err)
	}
	if len(capped.Events) != len(events) || capped.Coverage.LimitReached {
		t.Fatalf("capped limit page = %d events, limitReached %v", len(capped.Events), capped.Coverage.LimitReached)
	}
	if page.Schema != Schema {
		t.Fatalf("page schema = %q", page.Schema)
	}
}

func TestQueryTimeWindow(t *testing.T) {
	s := testStore(t)
	t0 := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	for _, at := range []time.Time{t0, t0.Add(time.Hour), t0.Add(2 * time.Hour)} {
		mustAppend(t, s, Event{
			Source:     SourceRPC,
			Action:     "backup.full",
			Outcome:    OutcomeSucceeded,
			OccurredAt: at.Format(time.RFC3339),
		})
	}
	page, err := s.Query(context.Background(), Query{Since: t0.Add(time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[0].Sequence != 2 {
		t.Fatalf("since query = %+v", page.Events)
	}
	page, err = s.Query(context.Background(), Query{Until: t0.Add(2 * time.Hour).Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 2 || page.Events[1].Sequence != 2 {
		t.Fatalf("until query = %+v", page.Events)
	}
	page, err = s.Query(context.Background(), Query{Since: "2h"})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 1 || page.Events[0].Sequence != 3 {
		t.Fatalf("duration query = %+v", page.Events)
	}
	if page.Coverage.FirstReturnedAt == "" || page.Coverage.LastReturnedAt == "" {
		t.Fatal("returned bounds missing")
	}
	if _, err := s.Query(context.Background(), Query{Since: "3h", Until: "1h"}); err != nil {
		t.Fatalf("valid duration window: %v", err)
	}
	if _, err := s.Query(context.Background(), Query{Since: "2026-10-21T00:00:00Z", Until: "2026-10-20T00:00:00Z"}); !errors.Is(err, errInvalidQuery) {
		t.Fatalf("inverted window error = %v", err)
	}
}

func TestQueryPaginationSnapshot(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 10; i++ {
		mustAppend(t, s, baseEvent())
	}
	q := Query{Limit: 3}
	first, err := s.Query(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got := sequences(first.Events); !equalSeq(got, []uint64{1, 2, 3}) {
		t.Fatalf("first page = %v", got)
	}
	if first.NextCursor == "" || !first.Coverage.LimitReached {
		t.Fatalf("first page cursor = %q limitReached = %v", first.NextCursor, first.Coverage.LimitReached)
	}
	for i := 0; i < 5; i++ {
		mustAppend(t, s, baseEvent())
	}
	var collected []uint64
	page := first
	for page.NextCursor != "" {
		q.Cursor = page.NextCursor
		next, err := s.Query(context.Background(), q)
		if err != nil {
			t.Fatalf("next page: %v", err)
		}
		for _, ev := range next.Events {
			collected = append(collected, ev.Sequence)
		}
		page = next
	}
	collected = append(sequences(first.Events), collected...)
	if !equalSeq(collected, []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}) {
		t.Fatalf("paged sequences = %v", collected)
	}
	all, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Events) != 15 {
		t.Fatalf("unpaged events = %d, want 15", len(all.Events))
	}
}

func TestCursorFilterAndTamperErrors(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 5; i++ {
		mustAppend(t, s, baseEvent())
	}
	first, err := s.Query(context.Background(), Query{Action: "backup.full", Limit: 2})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %v cursor=%q", err, first.NextCursor)
	}
	base := Query{Action: "backup.full", Limit: 2, Cursor: first.NextCursor}
	cases := []struct {
		name string
		q    Query
		want error
	}{
		{"action", Query{Action: "cert.renew", Limit: 2, Cursor: first.NextCursor}, errCursorMismatch},
		{"limit", Query{Action: "backup.full", Limit: 3, Cursor: first.NextCursor}, errCursorMismatch},
		{"since", Query{Action: "backup.full", Limit: 2, Cursor: first.NextCursor, Since: "1h"}, errCursorMismatch},
		{"outcome", Query{Action: "backup.full", Limit: 2, Cursor: first.NextCursor, Outcome: OutcomeFailed}, errCursorMismatch},
		{"tamper", Query{Action: "backup.full", Limit: 2, Cursor: first.NextCursor[:len(first.NextCursor)-2] + "AA"}, errInvalidCursor},
		{"garbage", Query{Action: "backup.full", Limit: 2, Cursor: "c1.bm90LWpzb24"}, errInvalidCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Query(context.Background(), tc.q); !errors.Is(err, tc.want) {
				t.Fatalf("Query error = %v, want %v", err, tc.want)
			}
		})
	}
	page, err := s.Query(context.Background(), base)
	if err != nil {
		t.Fatalf("bound cursor: %v", err)
	}
	if got := sequences(page.Events); !equalSeq(got, []uint64{3, 4}) {
		t.Fatalf("second page = %v", got)
	}
}

func TestAppendCreatesPrivateDirUnderSharedParent(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "var", "log", "vaire")
	dir := filepath.Join(parent, "activity")
	s := &Store{Dir: dir, MaxBytes: 64 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	mustAppend(t, s, baseEvent())
	parentInfo, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	if parentInfo.Mode().Perm() != parentMode {
		t.Fatalf("parent mode = %04o, want %04o", parentInfo.Mode().Perm(), parentMode)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("activity dir: %v", err)
	}
	if dirInfo.Mode().Perm() != dirMode {
		t.Fatalf("activity dir mode = %04o, want %04o", dirInfo.Mode().Perm(), dirMode)
	}
}

func TestQueryMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	s := &Store{Dir: dir, MaxBytes: 64 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.Available {
		t.Fatal("missing store reported available")
	}
	if !page.Coverage.Partial {
		t.Fatal("missing store did not report partial")
	}
	if page.Events == nil || len(page.Events) != 0 {
		t.Fatalf("events = %v", page.Events)
	}
	if page.NextCursor != "" {
		t.Fatalf("next cursor = %q", page.NextCursor)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Query created the store: %v", err)
	}
}

func TestQueryEmptyStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	if err := os.Mkdir(dir, dirMode); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: dir, MaxBytes: 64 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Coverage.Available || page.Coverage.Partial || page.Coverage.LossDetected {
		t.Fatalf("empty store coverage = %+v", page.Coverage)
	}
	if len(page.Events) != 0 {
		t.Fatalf("events = %+v", page.Events)
	}
	if page.Coverage.FirstRetainedAt != "" || page.Coverage.LastRetainedAt != "" {
		t.Fatalf("retained bounds on empty store = %+v", page.Coverage)
	}
}

func TestMalformedEvidenceReported(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 2; i++ {
		mustAppend(t, s, baseEvent())
	}
	segs := segmentsOf(t, s)
	active := segs[len(segs)-1]
	f, err := os.OpenFile(active.path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("not json\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":"vaire-activity/v1","event_id":"evt-1","occurred_at":"2026-01-01T00:00:00Z","sequence":7,"source":"rpc","action":"x","outcome":"started","extra":"x"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(strings.Repeat("x", 2*maxEventBytes) + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(active.path)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, s, baseEvent())
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.MalformedEvents != 3 {
		t.Fatalf("malformed events = %d, want 3", page.Coverage.MalformedEvents)
	}
	if !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("loss not reported: %+v", page.Coverage)
	}
	if len(page.Events) != 3 {
		t.Fatalf("valid events = %d, want 3", len(page.Events))
	}
	after, err := os.Stat(active.path)
	if err != nil {
		t.Fatal(err)
	}
	if before.Size() != after.Size() {
		t.Fatalf("read path truncated the segment: %d -> %d bytes", before.Size(), after.Size())
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.MalformedEvents != 3 || !health.LossDetected {
		t.Fatalf("health = %+v", health)
	}
}

func TestAppendAfterTornTailRotatesWithoutMerging(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	active := segs[len(segs)-1]
	f, err := os.OpenFile(active.path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"schema":"vaire-activity/v1","event_id":"torn`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	assigned := mustAppend(t, s, baseEvent())
	if assigned.Sequence != 2 {
		t.Fatalf("sequence = %d, want 2", assigned.Sequence)
	}
	after := segmentsOf(t, s)
	if len(after) != 2 {
		t.Fatalf("segments = %d, want a rotation after the torn tail", len(after))
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	if page.Coverage.MalformedEvents != 1 || !page.Coverage.LossDetected {
		t.Fatalf("torn tail coverage = %+v", page.Coverage)
	}
}

func TestCorruptStateRefusesAppendAndQueryReportsLoss(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	statePath := filepath.Join(s.Dir, stateName)
	if err := os.WriteFile(statePath, []byte("not json"), fileMode); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errCorruptState) {
		t.Fatalf("Append error = %v, want errCorruptState", err)
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("corrupt state coverage = %+v", page.Coverage)
	}
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !health.LossDetected {
		t.Fatalf("health did not report state loss: %+v", health)
	}
}

func TestUnsafeStateRefused(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	statePath := filepath.Join(s.Dir, stateName)
	target := filepath.Join(t.TempDir(), "state-target")
	raw, err := os.ReadFile(statePath) //nolint:gosec // G304: the test store's own state file.
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, raw, fileMode); err != nil { //nolint:gosec // G703: a path inside the test's own temporary directory.
		t.Fatal(err)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, statePath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeState) {
		t.Fatalf("Append error = %v, want errUnsafeState", err)
	}
	if _, err := s.Query(context.Background(), Query{}); !errors.Is(err, errUnsafeState) {
		t.Fatalf("Query error = %v, want errUnsafeState", err)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.LossDetected {
		t.Fatalf("unsafe state not reported: %+v", health)
	}
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o644); err != nil { //nolint:gosec // G306: the test needs an unsafe state mode.
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeState) {
		t.Fatalf("Append with unsafe state mode error = %v, want errUnsafeState", err)
	}
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	s := testStore(t)
	tests := []Event{
		{Source: SourceRPC, Action: "", Outcome: OutcomeStarted},
		{Source: "sql", Action: "backup.full", Outcome: OutcomeStarted},
		{Source: SourceRPC, Action: "SELECT 1", Outcome: OutcomeStarted},
		{Source: SourceRPC, Action: "backup.full", Outcome: "done"},
		{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted, ActorVerified: true, ActorKind: ActorOperator},
	}
	for _, ev := range tests {
		if _, err := s.Append(context.Background(), ev); !errors.Is(err, errInvalidEvent) {
			t.Fatalf("Append(%+v) error = %v", ev, err)
		}
	}
	if _, err := os.Stat(s.Dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("invalid append created the store: %v", err)
	}
}

func TestMissingStateRefusesAppendAndReportsLoss(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	if err := os.Remove(filepath.Join(s.Dir, stateName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errStateMissing) {
		t.Fatalf("Append error = %v, want errStateMissing", err)
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(page.Events) != 3 {
		t.Fatalf("events = %d, want 3", len(page.Events))
	}
	if !page.Coverage.Partial || !page.Coverage.LossDetected {
		t.Fatalf("missing state coverage = %+v", page.Coverage)
	}
	if page.Coverage.FirstRetainedAt == "" {
		t.Fatal("missing state reported no retained window")
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.LossDetected {
		t.Fatalf("health did not report missing state: %+v", health)
	}
}

func TestSequenceGapReported(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	state, ok, err := readState(s.Dir)
	if err != nil || !ok {
		t.Fatalf("readState: %v ok=%v", err, ok)
	}
	state.NextSequence = state.LastSequence + 7
	if err := writeState(s.Dir, state); err != nil {
		t.Fatal(err)
	}
	assigned := mustAppend(t, s, baseEvent())
	if assigned.Sequence != 8 {
		t.Fatalf("sequence = %d, want 8", assigned.Sequence)
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Coverage.SequenceGaps != 6 {
		t.Fatalf("sequence gaps = %d, want 6", page.Coverage.SequenceGaps)
	}
	if !page.Coverage.LossDetected || !page.Coverage.Partial {
		t.Fatalf("gap coverage = %+v", page.Coverage)
	}
}

func TestReadPathsDoNotMutate(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 3; i++ {
		mustAppend(t, s, baseEvent())
	}
	before := snapshotTree(t, s.Dir)
	if _, err := s.Query(context.Background(), Query{Limit: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := snapshotTree(t, s.Dir)
	if len(before) != len(after) {
		t.Fatalf("read path changed file count: %d -> %d", len(before), len(after))
	}
	for name, entry := range before {
		other, ok := after[name]
		if !ok {
			t.Fatalf("read path removed %s", name)
		}
		if entry != other {
			t.Fatalf("read path changed %s: %v -> %v", name, entry, other)
		}
	}
}

type treeEntry struct {
	mode os.FileMode
	size int64
}

func snapshotTree(t *testing.T, dir string) map[string]treeEntry {
	t.Helper()
	out := map[string]treeEntry{}
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = treeEntry{mode: info.Mode().Perm(), size: info.Size()}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sequences(events []Event) []uint64 {
	out := make([]uint64, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Sequence)
	}
	return out
}

func equalSeq(got, want []uint64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
