package config

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MetricsPathPattern is [metrics] path_prefix: an absolute path of lowercase
// segments, not the root.
var MetricsPathPattern = regexp.MustCompile(`^(/[a-z0-9_-]+)+$`)

// ParseWindow parses [maintenance] window, "HH:MM-HH:MM" in local time
// (per [timezone]). The end may be before the start, for a window across
// midnight. It returns the offsets of the start and end from midnight.
func ParseWindow(window string) (start, end time.Duration, err error) {
	from, to, ok := strings.Cut(window, "-")
	if !ok {
		return 0, 0, fmt.Errorf("must be HH:MM-HH:MM")
	}
	s, err1 := time.Parse("15:04", strings.TrimSpace(from))
	e, err2 := time.Parse("15:04", strings.TrimSpace(to))
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("must be HH:MM-HH:MM")
	}
	start = time.Duration(s.Hour())*time.Hour + time.Duration(s.Minute())*time.Minute
	end = time.Duration(e.Hour())*time.Hour + time.Duration(e.Minute())*time.Minute
	if start == end {
		return 0, 0, fmt.Errorf("must not start and end at the same time")
	}
	return start, end, nil
}

// WindowLength is the window's length; one across midnight wraps.
func WindowLength(start, end time.Duration) time.Duration {
	if end <= start {
		end += 24 * time.Hour
	}
	return end - start
}

// InWindow reports whether t (local time) falls inside the window, and how
// long is left of it.
func InWindow(t time.Time, start, end time.Duration) (bool, time.Duration) {
	midnight := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	now := t.Sub(midnight)
	length := WindowLength(start, end)
	since := now - start
	if since < 0 {
		since += 24 * time.Hour
	}
	if since < length {
		return true, length - since
	}
	return false, 0
}
