package tune

import (
	"strconv"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

// The pools leave config.ReservedConnections free, so both reserved sets
// together must be exactly that many, or full pools would hit "remaining
// connection slots are reserved" before they reach their size.
func TestReservedSlotsMatchThePools(t *testing.T) {
	res := Calculate(Input{RAMMiB: 1024, VCPU: 2, Clusters: 1, Profile: ProfileMixed, MaxConnections: 50, PGMajor: 18})
	sum := 0
	for _, s := range res.Settings {
		if s.Name == "superuser_reserved_connections" || s.Name == "reserved_connections" {
			n, err := strconv.Atoi(s.Value)
			if err != nil {
				t.Fatalf("%s = %q", s.Name, s.Value)
			}
			sum += n
		}
	}
	if sum != config.ReservedConnections {
		t.Errorf("superuser_reserved_connections + reserved_connections = %d, want config.ReservedConnections %d", sum, config.ReservedConnections)
	}
}

// Every tuning result must start a PostgreSQL 18 server and reload cleanly:
// the cross-setting rules the server checks at start, and each value within
// its range. A reload's "contains errors; unaffected changes were applied"
// for a postmaster setting such as superuser_reserved_connections is
// PostgreSQL deferring it to the restart, not an invalid value.
func TestSettingsAreValidForPostgres18(t *testing.T) {
	for _, ram := range []int{256, 512, 1024, 2048, 4096, 16384, 65536, 262144} {
		for _, vcpu := range []int{1, 2, 4, 16, 64} {
			for _, profile := range []string{ProfileOLTP, ProfileMixed, ProfileAnalytics} {
				for _, maxConn := range []int{0, 20, 100, 400} {
					for _, rotational := range []bool{false, true} {
						in := Input{RAMMiB: ram, VCPU: vcpu, Clusters: 3, Profile: profile, MaxConnections: maxConn,
							DiskRotational: rotational, DataDiskGiB: 40, PGMajor: 18}
						checkSettings(t, in, Calculate(in))
					}
				}
			}
		}
	}
}

func checkSettings(t *testing.T, in Input, res Result) {
	t.Helper()
	v := map[string]string{}
	for _, s := range res.Settings {
		if _, dup := v[s.Name]; dup {
			t.Fatalf("%+v: %s set twice", in, s.Name)
		}
		v[s.Name] = s.Value
		switch s.Context {
		case "postmaster", "sighup", "user":
		default:
			t.Fatalf("%s: context %q", s.Name, s.Context)
		}
	}
	num := func(name string) int {
		n, err := strconv.Atoi(v[name])
		if err != nil {
			t.Fatalf("%+v: %s = %q is not an integer", in, name, v[name])
		}
		return n
	}
	mib := func(name string) int {
		raw := v[name]
		for suffix, factor := range map[string]int{"MB": 1, "GB": 1024} {
			if n, ok := strings.CutSuffix(raw, suffix); ok {
				x, err := strconv.Atoi(n)
				if err != nil {
					t.Fatalf("%+v: %s = %q", in, name, raw)
				}
				return x * factor
			}
		}
		t.Fatalf("%+v: %s = %q has no MB or GB unit", in, name, raw)
		return 0
	}
	maxConn := num("max_connections")
	// PostgreSQL refuses to start unless both reserved sets together are
	// less than max_connections.
	if r := num("superuser_reserved_connections") + num("reserved_connections"); r >= maxConn {
		t.Errorf("%+v: %d reserved slots of max_connections %d", in, r, maxConn)
	}
	if maxConn < 1 || maxConn > 262143 {
		t.Errorf("%+v: max_connections %d", in, maxConn)
	}
	if w := num("max_worker_processes"); w < num("max_parallel_workers") || w > 262143 {
		t.Errorf("%+v: max_worker_processes %d, max_parallel_workers %s", in, w, v["max_parallel_workers"])
	}
	if num("autovacuum_max_workers") > num("autovacuum_worker_slots") {
		t.Errorf("%+v: autovacuum_max_workers %s over autovacuum_worker_slots %s", in, v["autovacuum_max_workers"], v["autovacuum_worker_slots"])
	}
	if n := num("io_workers"); n < 1 || n > 32 {
		t.Errorf("%+v: io_workers %d is outside 1-32", in, n)
	}
	if n := num("effective_io_concurrency"); n < 0 || n > 1000 {
		t.Errorf("%+v: effective_io_concurrency %d", in, n)
	}
	if n := num("maintenance_io_concurrency"); n < 0 || n > 1000 {
		t.Errorf("%+v: maintenance_io_concurrency %d", in, n)
	}
	// min_wal_size is at least twice the 16 MB segment, and no larger than
	// max_wal_size.
	if lo, hi := mib("min_wal_size"), mib("max_wal_size"); lo < 32 || lo > hi {
		t.Errorf("%+v: min_wal_size %s, max_wal_size %s", in, v["min_wal_size"], v["max_wal_size"])
	}
	if mib("shared_buffers") < 1 || mib("work_mem") < 1 || mib("maintenance_work_mem") < 1 || mib("autovacuum_work_mem") < 1 {
		t.Errorf("%+v: a memory setting under 1 MB", in)
	}
	for name, allowed := range map[string][]string{
		"huge_pages":  {"off", "try"},
		"wal_level":   {"replica", "logical"},
		"io_method":   {"worker", "sync", "io_uring"},
		"jit":         {"on", "off"},
		"wal_buffers": {"-1"},
	} {
		if !contains(allowed, v[name]) {
			t.Errorf("%+v: %s = %q", in, name, v[name])
		}
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
