package activity

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// retentionRacePolicy creates a store where the next append after the injected
// change rotates and prunes the segment that the reader listed.
type retentionRacePolicy struct {
	name  string
	store func(t *testing.T) *Store
	seed  func(t *testing.T, s *Store)
	churn func(t *testing.T, s *Store)
}

// agedRaceEvent expires under the policy's one-hour retention. Appending it to
// a full or expired segment rotates and lets time-based pruning remove the
// previous segment.
func agedRaceEvent() Event {
	ev := labelledEvent("retention-race")
	ev.OccurredAt = time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	return ev
}

func timeRetentionRacePolicy() retentionRacePolicy {
	return retentionRacePolicy{
		name: "time",
		store: func(t *testing.T) *Store {
			return &Store{
				Dir:          filepath.Join(t.TempDir(), "activity"),
				MaxBytes:     256 << 10,
				SegmentBytes: maxEventBytes,
				Retention:    time.Hour,
			}
		},
		seed: func(t *testing.T, s *Store) {
			mustAppend(t, s, agedRaceEvent())
		},
		churn: func(t *testing.T, s *Store) {
			mustAppend(t, s, agedRaceEvent())
		},
	}
}

// encodedLineLen is the line length of ev with a fixed identity and sequence,
// so a test can fill a segment up to the point where the next append rotates
// without actually rotating it.
func encodedLineLen(t *testing.T, ev Event) int64 {
	t.Helper()
	ev.Schema = Schema
	ev.EventID = "evt-" + strings.Repeat("0", 32)
	ev.OccurredAt = formatTime(time.Now().UTC())
	ev.Sequence = 1
	line, err := encodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(line))
}

// fillActiveSegment appends ev until the next append of that shape exceeds the
// segment target and therefore rotates.
func fillActiveSegment(t *testing.T, s *Store, ev Event, line int64) {
	t.Helper()
	for {
		segments := segmentsOf(t, s)
		if len(segments) == 0 {
			t.Fatal("fillActiveSegment: the store has no segment")
		}
		active := segments[len(segments)-1]
		if active.size+line > s.SegmentBytes {
			return
		}
		mustAppend(t, s, ev)
	}
}

func byteRetentionRacePolicy(t *testing.T) retentionRacePolicy {
	ev := labelledEvent("retention-race")
	line := encodedLineLen(t, ev)
	return retentionRacePolicy{
		name: "bytes",
		store: func(t *testing.T) *Store {
			// MaxBytes equals one segment, so a rotation always prunes the
			// segment it rotated away from.
			return &Store{
				Dir:          filepath.Join(t.TempDir(), "activity"),
				MaxBytes:     maxEventBytes,
				SegmentBytes: maxEventBytes,
				Retention:    24 * time.Hour,
			}
		},
		seed: func(t *testing.T, s *Store) {
			mustAppend(t, s, ev)
			fillActiveSegment(t, s, ev, line)
		},
		churn: func(t *testing.T, s *Store) {
			fillActiveSegment(t, s, ev, line)
			mustAppend(t, s, ev)
		},
	}
}

// retentionChurn counts the injected rotations and reports whether the read
// observed every one of them.
type retentionChurn struct {
	remaining int
}

func (c *retentionChurn) exhausted() bool {
	return c.remaining == 0
}

// installRetentionChurn rotates the store after the reader lists segments,
// before it reads their retained bounds. A read that takes a fresh snapshot
// calls the hook again; the first races injections are therefore seen on
// successive attempts.
func installRetentionChurn(t *testing.T, s *Store, churn func(*testing.T, *Store), races int) *retentionChurn {
	t.Helper()
	old := afterListSegmentsHook
	c := &retentionChurn{remaining: races}
	afterListSegmentsHook = func() {
		if c.remaining == 0 {
			return
		}
		c.remaining--
		churn(t, s)
	}
	t.Cleanup(func() { afterListSegmentsHook = old })
	return c
}

// TestRetentionRotationDoesNotReportLoss injects time-based and byte-budget
// rotation between the segment listing and the retained-bound read. The listed
// segment vanishes and a newer one appears; that is normal retention, not lost
// evidence. Both a single race and repeated races on successive read attempts
// must produce a clean verdict, and the read must be a complete snapshot of the
// store that remains.
func TestRetentionRotationDoesNotReportLoss(t *testing.T) {
	policies := []retentionRacePolicy{
		timeRetentionRacePolicy(),
		byteRetentionRacePolicy(t),
	}
	for _, policy := range policies {
		for _, races := range []int{1, 3} {
			t.Run(policy.name+"/races="+strconv.Itoa(races), func(t *testing.T) {
				t.Run("query", func(t *testing.T) {
					s := policy.store(t)
					policy.seed(t, s)
					churn := installRetentionChurn(t, s, policy.churn, races)

					page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
					if err != nil {
						t.Fatalf("Query: %v", err)
					}
					if !churn.exhausted() {
						t.Fatalf("%d injected rotations never reached the read", churn.remaining)
					}
					if page.Coverage.LossDetected {
						t.Fatalf("retention rotation reported loss: %+v", page.Coverage)
					}
					if page.Coverage.Truncated {
						t.Fatalf("retention rotation left a truncated snapshot: %+v", page.Coverage)
					}
					if len(page.Events) != 1 {
						t.Fatalf("events = %d, want the single event retained by rotation", len(page.Events))
					}
					if page.Coverage.LastRetainedAt == "" {
						t.Fatalf("retained last timestamp missing: %+v", page.Coverage)
					}
				})
				t.Run("health", func(t *testing.T) {
					s := policy.store(t)
					policy.seed(t, s)
					churn := installRetentionChurn(t, s, policy.churn, races)

					health, err := s.Health(context.Background())
					if err != nil {
						t.Fatalf("Health: %v", err)
					}
					if !churn.exhausted() {
						t.Fatalf("%d injected rotations never reached the read", churn.remaining)
					}
					if health.LossDetected {
						t.Fatalf("retention rotation reported loss by Health: %+v", health)
					}
					if health.Segments != 1 {
						t.Fatalf("segments = %d, want the single segment retained by rotation", health.Segments)
					}
					if health.MalformedEvents != 0 || health.MalformedSegments != 0 || health.SequenceGaps != 0 {
						t.Fatalf("retention rotation fabricated corruption: %+v", health)
					}
					if health.FirstRetainedAt == "" || health.LastRetainedAt == "" {
						t.Fatalf("retained window missing: %+v", health)
					}
				})
			})
		}
	}
}

// TestRetentionRaceRetryIsBounded races a rotation against every observation
// attempt. The read must stop at the retry bound and report an incomplete
// snapshot rather than turning the mixed observations into loss.
func TestRetentionRaceRetryIsBounded(t *testing.T) {
	policy := timeRetentionRacePolicy()
	t.Run("query", func(t *testing.T) {
		s := policy.store(t)
		policy.seed(t, s)
		// Four successive rotations exhaust the read's bounded retry, so this
		// snapshot is still mixed when the read returns.
		churn := installRetentionChurn(t, s, policy.churn, 4)

		page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if !churn.exhausted() {
			t.Fatalf("%d injected rotations never reached the read", churn.remaining)
		}
		if page.Coverage.LossDetected {
			t.Fatalf("a bounded churn read reported loss: %+v", page.Coverage)
		}
		if !page.Coverage.Truncated || !page.Coverage.Partial {
			t.Fatalf("a bounded churn read claimed a complete snapshot: %+v", page.Coverage)
		}
	})
	t.Run("health", func(t *testing.T) {
		s := policy.store(t)
		policy.seed(t, s)
		churn := installRetentionChurn(t, s, policy.churn, 4)

		health, err := s.Health(context.Background())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if !churn.exhausted() {
			t.Fatalf("%d injected rotations never reached the read", churn.remaining)
		}
		if health.LossDetected {
			t.Fatalf("a bounded churn health read reported loss: %+v", health)
		}
	})
}

// TestRetentionRetryStillReportsMissingNewestSegment is the negative control
// for the retry: deleting the newest segment between listing and bounds also
// removes a listed segment, but the next snapshot is stable and state still
// counts the deleted event. The read must report loss, not excuse it as
// retention.
func TestRetentionRetryStillReportsMissingNewestSegment(t *testing.T) {
	setup := func(t *testing.T) (*Store, string) {
		t.Helper()
		s := &Store{
			Dir:          filepath.Join(t.TempDir(), "activity"),
			MaxBytes:     256 << 10,
			SegmentBytes: maxEventBytes,
			Retention:    24 * time.Hour,
		}
		ev := labelledEvent(strings.Repeat("g", 100))
		for i := 0; i < 200; i++ {
			mustAppend(t, s, ev)
			if len(segmentsOf(t, s)) >= 2 {
				break
			}
		}
		segments := segmentsOf(t, s)
		if len(segments) < 2 {
			t.Fatalf("setup segments = %d, want at least 2", len(segments))
		}
		return s, segments[len(segments)-1].path
	}
	installDeleteNewest := func(t *testing.T, newest string) *int {
		t.Helper()
		old := afterListSegmentsHook
		calls := 0
		afterListSegmentsHook = func() {
			calls++
			if calls > 1 {
				return
			}
			if err := os.Remove(newest); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { afterListSegmentsHook = old })
		return &calls
	}

	t.Run("query", func(t *testing.T) {
		s, newest := setup(t)
		calls := installDeleteNewest(t, newest)
		page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if *calls < 2 {
			t.Fatalf("the vanished segment did not trigger a fresh observation (%d calls)", *calls)
		}
		if !page.Coverage.LossDetected || !page.Coverage.Partial {
			t.Fatalf("missing newest segment reported as healthy: %+v", page.Coverage)
		}
	})
	t.Run("health", func(t *testing.T) {
		s, newest := setup(t)
		calls := installDeleteNewest(t, newest)
		health, err := s.Health(context.Background())
		if err != nil {
			t.Fatalf("Health: %v", err)
		}
		if *calls < 2 {
			t.Fatalf("the vanished segment did not trigger a fresh observation (%d calls)", *calls)
		}
		if !health.LossDetected {
			t.Fatalf("missing newest segment reported as healthy: %+v", health)
		}
	})
}
