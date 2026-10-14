package ui

import (
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/spinner"
	"github.com/charmbracelet/x/ansi"
)

func TestClock(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                                     "00:00",
		-time.Second:                          "00:00",
		42*time.Second + 900*time.Millisecond: "00:42",
		2*time.Minute + 10*time.Second:        "02:10",
		time.Hour + 2*time.Minute + 3*time.Second: "1:02:03",
	} {
		if got := Clock(d); got != want {
			t.Errorf("Clock(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestStatusLine(t *testing.T) {
	l := &Live{}
	start := time.Now().Add(-3 * time.Minute)
	st := &Status{l: l, label: "restoring app2", started: start, phaseStarted: start}
	now := start.Add(130 * time.Second)
	if got := ansi.Strip(st.line("*", now)); got != "* restoring app2  02:10" {
		t.Errorf("label line = %q", got)
	}
	st.Phase("pgbackrest-v1")
	st.phaseStarted = start.Add(100 * time.Second)
	st.Detail("50%\rP00 INFO: backup start ")
	got := ansi.Strip(st.line("*", now))
	if want := "* pgbackrest-v1 · P00 INFO: backup start  00:30 · 02:10 total"; got != want {
		t.Errorf("phase line = %q, want %q", got, want)
	}
	st.Phase("")
	if got := ansi.Strip(st.line("*", time.Now())); !strings.HasPrefix(got, "* restoring app2  ") {
		t.Errorf("after the phase ended = %q", got)
	}
}

func TestWaitDrawsAfterIdle(t *testing.T) {
	var out strings.Builder
	l := &Live{out: &out, spinner: spinner.MiniDot, fd: -1}
	_, stop := l.Wait("restoring app2")
	l.mu.Lock()
	l.draw()
	quiet := out.Len()
	l.lastOut = time.Now().Add(-2 * idleAfter)
	l.draw()
	drawn := l.drawn
	l.mu.Unlock()
	stop()
	if quiet != 0 {
		t.Errorf("drew %q before a second without output", out.String())
	}
	if drawn != 1 || !strings.Contains(out.String(), "restoring app2") {
		t.Errorf("status line not drawn after idle: drawn=%d out=%q", drawn, out.String())
	}
	if l.status != nil || l.running || l.drawn != 0 {
		t.Errorf("stop left status=%v running=%v drawn=%d", l.status, l.running, l.drawn)
	}
}

func TestAnsweredLine(t *testing.T) {
	if got := ansi.Strip(answeredLine("Current code from the authenticator app", secretMask)); got != "? Current code from the authenticator app ••••••" {
		t.Errorf("answeredLine = %q", got)
	}
	if got := ansi.Strip(answeredLine("Push?", "")); got != "? Push?" {
		t.Errorf("answeredLine without an answer = %q", got)
	}
}
