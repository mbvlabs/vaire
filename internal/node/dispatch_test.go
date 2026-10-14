package node

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
)

// blockingWriter never returns from Write, like a stalled SSH channel.
type blockingWriter struct{ block chan struct{} }

func (w blockingWriter) Write([]byte) (int, error) {
	<-w.block
	return 0, errors.New("unblocked")
}

// epipeWriter fails every write, like stdout after sshd closed the session.
type epipeWriter struct{ writes int }

func (w *epipeWriter) Write([]byte) (int, error) {
	w.writes++
	return 0, syscall.EPIPE
}

func newTestWriter(t *testing.T, w interface{ Write([]byte) (int, error) }) (*eventWriter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "run.log")
	log, err := engine.OpenRunLogFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := &eventWriter{w: w}
	out.setLog(log)
	return out, path
}

func readLog(t *testing.T, out *eventWriter, path string) string {
	t.Helper()
	if err := out.log.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: test temp file.
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// emitAll emits n log events plus a step and a warning, failing the test if
// emit blocks.
func emitAll(t *testing.T, out *eventWriter, n int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range n {
			out.emit(output.Event{Type: "log", Line: "line " + strings.Repeat("x", i%3)})
		}
		out.emit(output.Event{Type: "step", Step: "after-flood", State: "ok"})
		out.emit(output.Event{Type: "warning", Code: "w1", Message: "kept"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked on stdout")
	}
}

func TestEventWriterBlockedStdout(t *testing.T) {
	old := streamDrainTimeout
	streamDrainTimeout = 200 * time.Millisecond
	t.Cleanup(func() { streamDrainTimeout = old })

	w := blockingWriter{block: make(chan struct{})}
	t.Cleanup(func() { close(w.block) })
	out, path := newTestWriter(t, w)
	n := streamQueueMax + 500
	emitAll(t, out, n)

	start := time.Now()
	if code := out.ok("test", Result{Data: map[string]string{"k": "v"}}); code != output.ExitOK {
		t.Fatalf("ok = %d", code)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("ok took %s, want about the drain timeout", took)
	}

	out.qmu.Lock()
	queued := len(out.pending)
	out.qmu.Unlock()
	// One line is stuck in Write; the queue holds at most the cap plus the
	// kept step, warnings and result.
	if queued > streamQueueMax+4 {
		t.Fatalf("queue holds %d lines, want at most %d", queued, streamQueueMax+4)
	}

	log := readLog(t, out, path)
	if got := strings.Count(log, `"type":"log"`); got != n {
		t.Fatalf("run log has %d log events, want %d", got, n)
	}
	for _, want := range []string{`"step":"after-flood"`, `"code":"w1"`, `"code":"events_dropped"`, `"type":"result"`, "did not drain"} {
		if !strings.Contains(log, want) {
			t.Fatalf("run log lacks %s", want)
		}
	}
}

func TestEventWriterBrokenStdout(t *testing.T) {
	w := &epipeWriter{}
	out, path := newTestWriter(t, w)
	emitAll(t, out, 100)

	start := time.Now()
	code := out.fail("test", output.NewError(output.CodeError, "boom", output.ExitGeneric, ""))
	if code != output.ExitGeneric {
		t.Fatalf("fail = %d", code)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("fail took %s with a dead stdout", took)
	}
	if w.writes != 1 {
		t.Fatalf("stdout got %d writes after EPIPE, want 1", w.writes)
	}

	log := readLog(t, out, path)
	if got := strings.Count(log, `"type":"log"`); got != 100 {
		t.Fatalf("run log has %d log events, want 100", got)
	}
	for _, want := range []string{`"step":"after-flood"`, `"type":"result"`, `"message":"boom"`, "broken pipe"} {
		if !strings.Contains(log, want) {
			t.Fatalf("run log lacks %s", want)
		}
	}
}
