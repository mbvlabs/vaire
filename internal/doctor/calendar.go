package doctor

import (
	"errors"
	"strings"
	"time"
)

// ParseCalendarIterations reads the elapses of systemd-analyze calendar
// --iterations=N. systemd prints each elapse in local time ("Next elapse",
// then "Iter. #n" before systemd 256 and "Iteration #n" since), and adds a
// "(in UTC)" line under each only when the local zone isn't UTC:
//
//	Normalized form: Sun *-*-* 01:00:00
//	    Next elapse: Sun 2026-10-18 01:00:00 CEST
//	       (in UTC): Sat 2026-10-17 23:00:00 UTC
//	       From now: 3 days left
//	   Iteration #2: Sun 2026-10-25 01:00:00 CET
//	       (in UTC): Sun 2026-10-25 00:00:00 UTC
//
// The "(in UTC)" lines are used when there are any; otherwise the local
// lines are, which are UTC on a UTC server (and with TZ=UTC, as the node
// runs it). The zone abbreviation is not parsed: only the gaps matter.
func ParseCalendarIterations(out string) []time.Time {
	var utc, local []time.Time
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		var into *[]time.Time
		switch {
		case key == "(in UTC)":
			into = &utc
		case key == "Next elapse", strings.HasPrefix(key, "Iter"):
			into = &local
		default:
			continue
		}
		// "Sun 2026-10-18 01:00:00 UTC"
		fields := strings.Fields(strings.TrimSpace(value))
		if len(fields) < 3 {
			continue
		}
		if t, err := time.Parse("2006-01-02 15:04:05", fields[1]+" "+fields[2]); err == nil {
			*into = append(*into, t.UTC())
		}
	}
	if len(utc) > 0 {
		return utc
	}
	return local
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
