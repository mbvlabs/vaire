package cli

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

func TestWaitLabel(t *testing.T) {
	cases := []struct {
		method string
		params any
		want   string
	}{
		{"restore.start", map[string]any{"database": "app2", "promote": true}, "restoring app2"},
		{"restore.undo", map[string]string{"database": "app2"}, "undoing the restore of app2"},
		{"db.create", map[string]any{"name": "app3"}, "creating app3"},
		{"db.fork", map[string]string{"source": "app", "name": "app-copy"}, "forking app"},
		{"job.follow", map[string]string{"id": "01ABC"}, "following job 01ABC"},
		{"tune.apply", nil, "applying the tuning"},
		{"something.new", nil, "waiting for something.new"},
	}
	for _, c := range cases {
		if got := waitLabel(c.method, c.params); got != c.want {
			t.Errorf("waitLabel(%s) = %q, want %q", c.method, got, c.want)
		}
	}
}

// syncBuffer is a strings.Builder safe for the heartbeat goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func TestPlainWaitHeartbeat(t *testing.T) {
	out := &syncBuffer{}
	p := &progress{opts: output.Options{Mode: output.ModeHuman}, stderr: out}
	done := p.wait("restoring app2")
	p.beat.mu.Lock()
	p.beat.every = 50 * time.Millisecond
	p.beat.mu.Unlock()
	if inner := p.wait("nested"); p.beat.label != "restoring app2" {
		t.Fatalf("a nested wait replaced the outer one")
	} else {
		inner()
	}
	p.track(output.Event{Type: "step", Step: "pgbackrest-v1", State: "started"})
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), "still pgbackrest-v1") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	done()
	if !strings.Contains(out.String(), "… still pgbackrest-v1 (") {
		t.Fatalf("no heartbeat in %q", out.String())
	}
	if p.beat != nil || p.stderr != out {
		t.Errorf("stop did not restore stderr")
	}
}

func TestHeartbeatLine(t *testing.T) {
	start := time.Now()
	h := &heartbeat{label: "restoring app2", started: start, phaseStarted: start}
	if got := h.line(start.Add(130 * time.Second)); got != "… still restoring app2 (2m10s)" {
		t.Errorf("label line = %q", got)
	}
	h.phase, h.phaseStarted = "pgbackrest-v1", start.Add(100*time.Second)
	if got := h.line(start.Add(130 * time.Second)); got != "… still pgbackrest-v1 (30s, 2m10s total)" {
		t.Errorf("phase line = %q", got)
	}
}

func TestStructuredWaitIsSilent(t *testing.T) {
	out := &syncBuffer{}
	p := &progress{opts: output.Options{Mode: output.ModeJSON}, stderr: out}
	done := p.wait("restoring app2")
	if p.beat != nil || p.status != nil {
		t.Errorf("structured mode started a waiting line")
	}
	done()
}
