package activity

import (
	"context"
	"sync"
	"testing"
)

func TestConcurrentAppends(t *testing.T) {
	s := testStore(t)

	const (
		writers = 8
		each    = 15
	)

	var wg sync.WaitGroup

	errs := make(chan error, writers*each)
	for range writers {
		wg.Go(func() {
			for range each {
				if _, err := s.Append(context.Background(), Event{
					Source:      SourceRPC,
					Action:      "backup.full",
					Outcome:     OutcomeStarted,
					OperationID: "op-concurrent",
					RequestID:   "req-concurrent",
				}); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Append: %v", err)
	}

	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if got, want := len(page.Events), writers*each; got != want {
		t.Fatalf("events = %d, want %d", got, want)
	}

	seenIDs := map[string]bool{}

	for i, ev := range page.Events {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("event %d has sequence %d", i, ev.Sequence)
		}

		if seenIDs[ev.EventID] {
			t.Fatalf("duplicate event id %q", ev.EventID)
		}

		seenIDs[ev.EventID] = true
	}

	if page.Coverage.SequenceGaps != 0 || page.Coverage.MalformedEvents != 0 {
		t.Fatalf("coverage = %+v", page.Coverage)
	}
}

func TestConcurrentAppendAndQuery(t *testing.T) {
	s := testStore(t)
	done, errs := startAppendWriters(s, 4, 20)

	for {
		select {
		case <-done:
			requireNoWriterError(t, errs)
			return
		default:
			assertConcurrentPage(t, s)
		}
	}
}

// startAppendWriters runs appenders of baseEvent and closes the returned done
// channel when all of them finish. errs carries one error per writer.
func startAppendWriters(s *Store, writers, each int) (<-chan struct{}, <-chan error) {
	var wg sync.WaitGroup

	done := make(chan struct{})

	errs := make(chan error, writers)
	for range writers {
		wg.Go(func() {
			for range each {
				if _, err := s.Append(context.Background(), baseEvent()); err != nil {
					errs <- err
					return
				}
			}
		})
	}

	go func() {
		wg.Wait()
		close(done)
	}()

	return done, errs
}

// requireNoWriterError fails the test when a finished writer reported an error.
func requireNoWriterError(t *testing.T, errs <-chan error) {
	t.Helper()

	select {
	case err := <-errs:
		t.Fatalf("writer error: %v", err)
	default:
	}
}

// assertConcurrentPage checks one page for ascending order and a cursor page
// that never repeats a sequence.
func assertConcurrentPage(t *testing.T, s *Store) {
	t.Helper()
	page := mustQuery(t, s, Query{Limit: 5})

	var prev uint64
	for _, ev := range page.Events {
		if ev.Sequence <= prev {
			t.Fatalf("page is not ascending: %d after %d", ev.Sequence, prev)
		}

		prev = ev.Sequence
	}

	if page.NextCursor == "" {
		return
	}

	next := mustQuery(t, s, Query{Limit: 5, Cursor: page.NextCursor})
	if len(page.Events) == 0 {
		return
	}

	last := page.Events[len(page.Events)-1].Sequence
	for _, ev := range next.Events {
		if ev.Sequence <= last {
			t.Fatalf("cursor page repeated sequence %d", ev.Sequence)
		}
	}
}
