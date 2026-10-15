package hostinfo

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParsePressure(t *testing.T) {
	p := ParsePressure("some avg10=1.50 avg60=0.25 avg300=0.10 total=2500000\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1000000\n")
	if p.Some.Avg10 != 1.5 || p.Some.Avg300 != 0.1 || p.Some.TotalSec != 2.5 {
		t.Errorf("some %+v", p.Some)
	}
	if !p.HasFull || p.Full.TotalSec != 1 {
		t.Errorf("full %+v", p.Full)
	}
	cpu := ParsePressure("some avg10=0.00 avg60=0.00 avg300=0.00 total=10\n")
	if cpu.HasFull {
		t.Error("no full line, but HasFull")
	}
}

func TestParseKeyValuesAndIOStat(t *testing.T) {
	ev := ParseKeyValues("low 0\nhigh 12\nmax 1\noom 0\noom_kill 0\noom_group_kill 0\n")
	if ev["high"] != 12 || ev["max"] != 1 || len(ev) != 6 {
		t.Errorf("events %v", ev)
	}
	r, w := ParseIOStat("253:0 rbytes=100 wbytes=200 rios=1 wios=2 dbytes=0 dios=0\n8:0 rbytes=1 wbytes=2\n")
	if r != 101 || w != 202 {
		t.Errorf("io %d %d", r, w)
	}
	if r, w := ParseIOStat(""); r != 0 || w != 0 {
		t.Error("empty io.stat")
	}
}

func TestReadCgroup(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"memory.current":  "1048576\n",
		"memory.high":     "max\n",
		"memory.max":      "2097152\n",
		"memory.events":   "high 3\n",
		"memory.pressure": "some avg10=0.00 avg60=0.00 avg300=12.00 total=5\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=1\n",
		"cpu.stat":        "usage_usec 3000000\nuser_usec 1\n",
		"io.stat":         "253:0 rbytes=10 wbytes=20\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, ok := ReadCgroup(dir)
	if !ok {
		t.Fatal("not read")
	}
	if c.MemoryCurrent != 1<<20 || c.MemoryHigh != 0 || c.MemoryMax != 2<<20 || c.MemoryEvents["high"] != 3 {
		t.Errorf("memory %+v", c)
	}
	if c.Pressure["memory"].Some.Avg300 != 12 || c.CPUUsageSec != 3 || c.IOReadBytes != 10 || c.IOWriteByte != 20 {
		t.Errorf("counters %+v", c)
	}
	if _, ok := ReadCgroup(filepath.Join(dir, "missing")); ok {
		t.Error("a missing cgroup read as present")
	}
}
