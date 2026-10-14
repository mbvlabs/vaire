package ui

import (
	"fmt"
	"time"
)

// idleAfter is how long the live area stays quiet before the status line
// shows, so quick calls never flash it.
const idleAfter = 300 * time.Millisecond

// Status is the single line the live area shows while a command waits for
// the node and no task runs: a spinner, what it waits for, the latest output
// and how long the phase and the whole wait have taken. It is drawn only
// after nothing was printed for idleAfter.
type Status struct {
	l            *Live
	label        string
	phase        string
	detail       string
	started      time.Time
	phaseStarted time.Time
}

// Wait shows label in the status line until the returned func is called.
// A wait already showing is kept, and the returned func then does nothing.
func (l *Live) Wait(label string) (*Status, func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.status != nil {
		return l.status, func() {}
	}
	now := time.Now()
	st := &Status{l: l, label: label, started: now, phaseStarted: now}
	l.status = st
	l.lastOut = now
	l.startTicker()
	return st, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		if l.status != st {
			return
		}
		l.status = nil
		l.clear()
		if len(l.tasks) == 0 {
			l.stopTicker()
		}
		l.draw()
	}
}

// Phase names the current phase; an empty phase falls back to the label.
// A new phase restarts its clock and drops the detail.
func (s *Status) Phase(phase string) {
	s.l.mu.Lock()
	defer s.l.mu.Unlock()
	phase = sanitize(phase)
	if phase == s.phase {
		return
	}
	s.phase, s.detail, s.phaseStarted = phase, "", time.Now()
}

// Detail sets the dim text after the phase, such as the latest log line.
func (s *Status) Detail(detail string) {
	s.l.mu.Lock()
	s.detail = sanitize(detail)
	s.l.mu.Unlock()
}

// line renders the status line with the spinner frame. The caller holds
// l.mu.
func (s *Status) line(frame string, now time.Time) string {
	text := s.label
	if s.phase != "" {
		text = s.phase
	}
	out := frame + " " + text
	if s.detail != "" && s.detail != text {
		out += styleDim.Render(" · " + s.detail)
	}
	times := Clock(now.Sub(s.started))
	if s.phaseStarted.After(s.started) {
		times = Clock(now.Sub(s.phaseStarted)) + " · " + times + " total"
	}
	return out + "  " + styleDim.Render(times)
}

// Clock formats d as mm:ss, or h:mm:ss from an hour.
func Clock(d time.Duration) string {
	d = max(d, 0).Truncate(time.Second)
	h, m, sec := int(d.Hours()), int(d.Minutes())%60, int(d.Seconds())%60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, sec)
	}
	return fmt.Sprintf("%02d:%02d", m, sec)
}
