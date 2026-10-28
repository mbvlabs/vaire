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

func TestHealthHealthyStore(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	mustAppend(t, s, baseEvent())
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Exists || !health.IsDir {
		t.Fatalf("health = %+v", health)
	}
	if health.DirMode != "0700" {
		t.Fatalf("dir mode = %q", health.DirMode)
	}
	if health.SegmentMode != "0600" {
		t.Fatalf("segment mode = %q", health.SegmentMode)
	}
	if !health.Readable || !health.Writable {
		t.Fatalf("access = readable %v writable %v", health.Readable, health.Writable)
	}
	if !health.OwnerOnly || health.NonOwnerOnlySegments != 0 {
		t.Fatalf("owner-only = %v segments = %d", health.OwnerOnly, health.NonOwnerOnlySegments)
	}
	if health.MaxBytes != s.MaxBytes || health.SegmentBytes != s.SegmentBytes {
		t.Fatalf("budgets = %+v", health)
	}
	if health.RetentionS != int64(s.Retention/time.Second) {
		t.Fatalf("retention = %d", health.RetentionS)
	}
	if health.UsedBytes <= 0 || health.Segments != 1 {
		t.Fatalf("usage = %d bytes, %d segments", health.UsedBytes, health.Segments)
	}
	if health.FirstRetainedAt == "" || health.LastRetainedAt == "" {
		t.Fatalf("retained bounds = %+v", health)
	}
	if health.LastWriteAt == "" {
		t.Fatal("last write missing")
	}
	if health.OverBudget {
		t.Fatal("fresh store over budget")
	}
	if health.LossDetected || health.MalformedEvents != 0 || health.MalformedSegments != 0 ||
		health.DroppedEvents != 0 || health.SequenceGaps != 0 {
		t.Fatalf("fresh store reports loss: %+v", health)
	}
	if health.Dir != s.Dir {
		t.Fatalf("dir = %q", health.Dir)
	}
}

func TestHealthMissingStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	s := &Store{Dir: dir, MaxBytes: 64 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Exists || health.IsDir || health.Readable || health.Writable || health.OwnerOnly {
		t.Fatalf("missing store health = %+v", health)
	}
	if health.Dir != dir || health.MaxBytes != s.MaxBytes || health.SegmentBytes != s.SegmentBytes {
		t.Fatalf("missing store config = %+v", health)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Health created the store: %v", err)
	}
}

func TestHealthUnsafeModesAndSymlinks(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	if err := os.Chmod(s.Dir, 0o755); err != nil { //nolint:gosec // G302: the test needs an unsafe directory mode.
		t.Fatal(err)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.OwnerOnly || health.DirMode != "0755" {
		t.Fatalf("health = %+v", health)
	}
	if !health.LossDetected {
		t.Fatal("unsafe directory not reported as loss")
	}
	if _, err := s.Query(context.Background(), Query{}); !errors.Is(err, errUnsafeDir) {
		t.Fatalf("Query error = %v, want errUnsafeDir", err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeDir) {
		t.Fatalf("Append error = %v, want errUnsafeDir", err)
	}
	if err := os.Chmod(s.Dir, dirMode); err != nil {
		t.Fatal(err)
	}
	segs := segmentsOf(t, s)
	if err := os.Chmod(segs[0].path, 0o644); err != nil { //nolint:gosec // G302: the test needs an unsafe segment mode.
		t.Fatal(err)
	}
	health, err = s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.NonOwnerOnlySegments != 1 || health.OwnerOnly || health.SegmentMode != "0644" {
		t.Fatalf("segment mode health = %+v", health)
	}
	if _, err := s.Query(context.Background(), Query{}); !errors.Is(err, errUnsafeSegment) {
		t.Fatalf("Query error = %v, want errUnsafeSegment", err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeSegment) {
		t.Fatalf("Append error = %v, want errUnsafeSegment", err)
	}
}

func TestHealthSymlinkedStore(t *testing.T) {
	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "activity-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatal(err)
	}
	s := &Store{Dir: link, MaxBytes: 64 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.Exists || health.IsDir || health.OwnerOnly {
		t.Fatalf("symlinked store health = %+v", health)
	}
	if !health.LossDetected {
		t.Fatalf("symlinked store not reported as loss: %+v", health)
	}
	if _, err := s.Query(context.Background(), Query{}); !errors.Is(err, errUnsafeDir) {
		t.Fatalf("Query error = %v, want errUnsafeDir", err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeDir) {
		t.Fatalf("Append error = %v, want errUnsafeDir", err)
	}
}

func TestHealthSymlinkedSegment(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte{}, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(segs[0].path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, segs[0].path); err != nil {
		t.Fatal(err)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.NonOwnerOnlySegments != 1 || health.OwnerOnly {
		t.Fatalf("symlink health = %+v", health)
	}
	if _, err := s.Query(context.Background(), Query{}); !errors.Is(err, errUnsafeSegment) {
		t.Fatalf("Query error = %v, want errUnsafeSegment", err)
	}
}

func TestHealthOverBudget(t *testing.T) {
	s := testStore(t)
	label := strings.Repeat("a", 100)
	for i := 0; i < 40; i++ {
		mustAppend(t, s, labelledEvent(label))
	}
	small := &Store{Dir: s.Dir, MaxBytes: maxEventBytes, SegmentBytes: maxEventBytes, Retention: time.Hour}
	health, err := small.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.OverBudget {
		t.Fatalf("store not over budget: used %d, max %d", health.UsedBytes, health.MaxBytes)
	}
}

func TestRetainedBoundsWithOutOfOrderTimes(t *testing.T) {
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	for _, at := range []time.Time{now.Add(-2 * time.Hour), now, now.Add(-time.Hour)} {
		mustAppend(t, s, Event{
			Source:     SourceRPC,
			Action:     "backup.full",
			Outcome:    OutcomeSucceeded,
			OccurredAt: at.Format(time.RFC3339),
		})
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	first, err := time.Parse(time.RFC3339Nano, page.Coverage.FirstRetainedAt)
	if err != nil {
		t.Fatal(err)
	}
	last, err := time.Parse(time.RFC3339Nano, page.Coverage.LastRetainedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Equal(now.Add(-2*time.Hour)) || !last.Equal(now) {
		t.Fatalf("retained bounds = %v to %v", first, last)
	}
	if first.After(last) {
		t.Fatal("retained bounds are inverted")
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.FirstRetainedAt != page.Coverage.FirstRetainedAt ||
		health.LastRetainedAt != page.Coverage.LastRetainedAt {
		t.Fatalf("health bounds = %s to %s, page = %s to %s",
			health.FirstRetainedAt, health.LastRetainedAt,
			page.Coverage.FirstRetainedAt, page.Coverage.LastRetainedAt)
	}
}

func TestHealthCountsMalformedAndGaps(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	segs := segmentsOf(t, s)
	f, err := os.OpenFile(segs[0].path, os.O_WRONLY|os.O_APPEND, fileMode)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{broken\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.MalformedEvents != 1 || !health.LossDetected {
		t.Fatalf("health = %+v", health)
	}
}
