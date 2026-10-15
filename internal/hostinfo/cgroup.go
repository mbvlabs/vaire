package hostinfo

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// PostgresSlice is where systemd puts every postgresql@ instance.
const PostgresSlice = "/sys/fs/cgroup/system.slice/system-postgresql.slice"

// ServiceCgroup is the cgroup directory of a postgresql@ unit, for example
// postgresql@18-app.
func ServiceCgroup(unit string) string {
	return filepath.Join(PostgresSlice, unit+".service")
}

// Cgroup is what eru reads about one cluster's cgroup (Phase 4): its memory
// against its limits, the throttling and OOM events, pressure stall
// information and CPU and I/O counters.
type Cgroup struct {
	MemoryCurrent int64
	// MemoryHigh and MemoryMax are 0 for "max" (no limit).
	MemoryHigh   int64
	MemoryMax    int64
	MemoryEvents map[string]int64
	// Pressure is keyed by resource (cpu, memory, io).
	Pressure    map[string]Pressure
	CPUUsageSec float64
	IOReadBytes int64
	IOWriteByte int64
}

// Pressure is one resource's PSI: the 10 s, 60 s and 300 s averages in
// percent and the total stall time in seconds, for "some" and "full".
type Pressure struct {
	Some PressureLine
	Full PressureLine
	// HasFull is false for cpu on kernels without a full line.
	HasFull bool
}

// PressureLine is one line of a pressure file.
type PressureLine struct {
	Avg10, Avg60, Avg300 float64
	TotalSec             float64
}

// ReadCgroup reads dir's cgroup files. ok is false when the cgroup doesn't
// exist (a stopped cluster, or not Linux).
func ReadCgroup(dir string) (Cgroup, bool) {
	cur, err := readInt(filepath.Join(dir, "memory.current"))
	if err != nil {
		return Cgroup{}, false
	}
	c := Cgroup{MemoryCurrent: cur, MemoryEvents: map[string]int64{}, Pressure: map[string]Pressure{}}
	c.MemoryHigh, _ = readInt(filepath.Join(dir, "memory.high"))
	c.MemoryMax, _ = readInt(filepath.Join(dir, "memory.max"))
	if raw, err := os.ReadFile(filepath.Join(dir, "memory.events")); err == nil { //nolint:gosec // G304: a cgroup file.
		c.MemoryEvents = ParseKeyValues(string(raw))
	}
	for _, res := range []string{"cpu", "memory", "io"} {
		if raw, err := os.ReadFile(filepath.Join(dir, res+".pressure")); err == nil { //nolint:gosec // G304: a cgroup file.
			c.Pressure[res] = ParsePressure(string(raw))
		}
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "cpu.stat")); err == nil { //nolint:gosec // G304: a cgroup file.
		c.CPUUsageSec = float64(ParseKeyValues(string(raw))["usage_usec"]) / 1e6
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "io.stat")); err == nil { //nolint:gosec // G304: a cgroup file.
		c.IOReadBytes, c.IOWriteByte = ParseIOStat(string(raw))
	}
	return c, true
}

// readInt reads a cgroup file holding one integer; "max" reads as 0.
func readInt(path string) (int64, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a cgroup file.
	if err != nil {
		return 0, err
	}
	s := strings.TrimSpace(string(raw))
	if s == "max" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

// ParseKeyValues parses "key value" lines (memory.events, cpu.stat).
func ParseKeyValues(raw string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if n, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
			out[fields[0]] = n
		}
	}
	return out
}

// ParsePressure parses a PSI file:
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=12345
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=6789
//
// total is in microseconds.
func ParsePressure(raw string) Pressure {
	var p Pressure
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var pl PressureLine
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			switch k {
			case "avg10":
				pl.Avg10 = n
			case "avg60":
				pl.Avg60 = n
			case "avg300":
				pl.Avg300 = n
			case "total":
				pl.TotalSec = n / 1e6
			}
		}
		switch fields[0] {
		case "some":
			p.Some = pl
		case "full":
			p.Full, p.HasFull = pl, true
		}
	}
	return p
}

// ParseIOStat sums rbytes and wbytes over every device in io.stat:
//
//	253:0 rbytes=1 wbytes=2 rios=3 wios=4 dbytes=0 dios=0
func ParseIOStat(raw string) (read, write int64) {
	for _, line := range strings.Split(raw, "\n") {
		for _, f := range strings.Fields(line) {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				continue
			}
			switch k {
			case "rbytes":
				read += n
			case "wbytes":
				write += n
			}
		}
	}
	return read, write
}
