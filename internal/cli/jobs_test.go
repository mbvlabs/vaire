package cli

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
)

func streamLostError() error {
	return output.NewError(output.CodeTransportFailed, "lost the connection", output.ExitTransport, "read the run log").
		WithDetails(map[string]any{"reason": "stream_lost"})
}

func logEvents(msgs ...string) []output.Event {
	evs := make([]output.Event, 0, len(msgs))
	for _, m := range msgs {
		evs = append(evs, output.Event{Type: "log", Message: m})
	}
	return evs
}

func TestReattachJobSkipsReplayedEvents(t *testing.T) {
	journal := logEvents("a", "b", "c", "d")
	var shown []string
	seen := 2 // a and b were shown before the stream died
	calls := 0
	follow := func(_ context.Context, onEvent func(output.Event)) (remote.Reply, error) {
		calls++
		if calls == 1 {
			// The first re-attach dies too, after replaying a, b and c.
			for _, ev := range journal[:3] {
				onEvent(ev)
			}
			return remote.Reply{}, streamLostError()
		}
		for _, ev := range journal {
			onEvent(ev)
		}
		return remote.Reply{Data: []byte(`{"job_id":"J"}`)}, nil
	}
	reconnects := 0
	reply, err := reattachJob(context.Background(), "J", streamLostError(), &seen,
		func(context.Context) error { reconnects++; return nil }, follow,
		func(ev output.Event) { shown = append(shown, ev.Message) }, func(string) {},
		[]time.Duration{0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	if string(reply.Data) != `{"job_id":"J"}` {
		t.Fatalf("reply = %s", reply.Data)
	}
	if want := []string{"c", "d"}; !slices.Equal(shown, want) {
		t.Fatalf("shown = %v, want %v", shown, want)
	}
	if reconnects != 2 || seen != 4 {
		t.Fatalf("reconnects = %d, seen = %d", reconnects, seen)
	}
}

func TestReattachJobGivesUpWithTheOriginalError(t *testing.T) {
	lost := streamLostError()
	seen := 0
	attempts := 0
	_, err := reattachJob(context.Background(), "J", lost, &seen,
		func(context.Context) error { attempts++; return errors.New("dial failed") },
		func(context.Context, func(output.Event)) (remote.Reply, error) {
			t.Fatal("followed without a connection")
			return remote.Reply{}, nil
		},
		func(output.Event) {}, func(string) {}, []time.Duration{0, 0, 0})
	if !errors.Is(err, lost) {
		t.Fatalf("err = %v, want the original stream_lost error", err)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
}

func TestReattachJobReturnsTheJobFailure(t *testing.T) {
	failed := output.NewError(output.CodeError, "job J failed", output.ExitGeneric, "")
	seen := 0
	_, err := reattachJob(context.Background(), "J", streamLostError(), &seen,
		func(context.Context) error { return nil },
		func(context.Context, func(output.Event)) (remote.Reply, error) { return remote.Reply{}, failed },
		func(output.Event) {}, func(string) {}, []time.Duration{0, 0})
	if !errors.Is(err, failed) {
		t.Fatalf("err = %v, want the job's failure", err)
	}
}

func TestReattachJobStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	seen := 0
	_, err := reattachJob(ctx, "J", streamLostError(), &seen,
		func(context.Context) error { return nil },
		func(context.Context, func(output.Event)) (remote.Reply, error) { return remote.Reply{}, nil },
		func(output.Event) {}, func(string) {}, []time.Duration{time.Hour})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestIsStreamLost(t *testing.T) {
	if !isStreamLost(streamLostError()) {
		t.Fatal("a stream_lost error was not recognised")
	}
	if isStreamLost(output.NewError(output.CodeTransportFailed, "x", output.ExitTransport, "")) || isStreamLost(errors.New("x")) {
		t.Fatal("another error was taken for stream_lost")
	}
}
