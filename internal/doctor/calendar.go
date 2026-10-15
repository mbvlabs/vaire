package doctor

import (
	"errors"
	"strings"
	"time"
)

// ParseCalendarIterations reads the "Next elapse" and "Iter. #n" lines of
// systemd-analyze calendar --iterations=N, in UTC:
//
//	  Original form: Sun *-*-* 01:00:00
//	Normalized form: Sun *-*-* 01:00:00
//	    Next elapse: Sun 2026-10-18 01:00:00 UTC
//	       (in UTC): Sun 2026-10-18 01:00:00 UTC
//	       From now: 3 days left
//	       Iter. #2: Sun 2026-10-25 01:00:00 UTC
//	       (in UTC): Sun 2026-10-25 01:00:00 UTC
func ParseCalendarIterations(out string) []time.Time {
	var times []time.Time
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || key != "(in UTC)" {
			continue
		}
		fields := strings.Fields(strings.TrimSpace(value))
		// "Sun 2026-10-18 01:00:00 UTC"
		if len(fields) < 3 {
			continue
		}
		t, err := time.Parse("2006-01-02 15:04:05", fields[1]+" "+fields[2])
		if err == nil {
			times = append(times, t.UTC())
		}
	}
	return times
}

// ScheduleInterval is the gap between the first two elapses of a schedule,
// from ParseCalendarIterations' output with --iterations=2 or more. A
// schedule with uneven gaps (Mon..Sat) gets its largest.
func ScheduleInterval(times []time.Time) (time.Duration, error) {
	if len(times) < 2 {
		return 0, errors.New("the schedule has fewer than two elapses")
	}
	var longest time.Duration
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d > longest {
			longest = d
		}
	}
	return longest, nil
}
