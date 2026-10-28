package activity

import (
	"context"
	"sync"
	"testing"
)

func TestConcurrentAppends(t *testing.T) {
	s := testStore(t)
	const writers = 8
	const each = 15
	var wg sync.WaitGroup
	errs := make(chan error, writers*each)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
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
		}(w)
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
	var wg sync.WaitGroup
	done := make(chan struct{})
	errs := make(chan error, 256)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := s.Append(context.Background(), baseEvent()); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	go func() {
		wg.Wait()
		close(done)
	}()
	for {
		select {
		case <-done:
			close(errs)
			for err := range errs {
				t.Fatalf("writer error: %v", err)
			}
			return
		default:
			page, err := s.Query(context.Background(), Query{Limit: 5})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			var prev uint64
			for _, ev := range page.Events {
				if ev.Sequence <= prev {
					t.Fatalf("page is not ascending: %d after %d", ev.Sequence, prev)
				}
				prev = ev.Sequence
			}
			if page.NextCursor != "" {
				q := Query{Limit: 5, Cursor: page.NextCursor}
				next, err := s.Query(context.Background(), q)
				if err != nil {
					t.Fatalf("cursor Query: %v", err)
				}
				for _, ev := range next.Events {
					if ev.Sequence <= page.Events[len(page.Events)-1].Sequence {
						t.Fatalf("cursor page repeated sequence %d", ev.Sequence)
					}
				}
			}
		}
	}
}
