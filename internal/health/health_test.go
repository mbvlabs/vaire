package health

import (
	"testing"
	"time"
)

func TestParseSample(t *testing.T) {
	s := ParseSample(map[string]string{"uptime_s": "3600", "max_connections": "30", "connections": "6", "blks_hit": "99", "blks_read": "1",
		"last_archived_age_s": "-1", "ssl": "on", "max_xid_age": "", "bogus": "x"})
	if s.UptimeS != 3600 || s.MaxConnections != 30 || s.Connections != 6 || s.LastArchivedAgeS != -1 || s.SSL != "on" || s.MaxXIDAge != 0 {
		t.Errorf("%+v", s)
	}
	if use := ConnectionUse(s); use != 0.2 {
		t.Errorf("use %v", use)
	}
}

func TestCacheHitAndTPS(t *testing.T) {
	if _, ok := CacheHit(0, 0); ok {
		t.Error("no reads yet, but a ratio")
	}
	if r, _ := CacheHit(99, 1); r != 0.99 {
		t.Errorf("ratio %v", r)
	}
	if got := TPS(100, 150, 500*time.Millisecond); got != 100 {
		t.Errorf("tps %v", got)
	}
	if got := TPS(150, 10, time.Second); got != 0 {
		t.Errorf("a counter reset gave %v", got)
	}
	if got := TPS(1, 2, 0); got != 0 {
		t.Errorf("zero interval gave %v", got)
	}
}

func TestSumPools(t *testing.T) {
	p := SumPools([]map[string]string{
		{"database": "app", "cl_active": "3", "cl_waiting": "1", "sv_active": "2", "sv_idle": "4", "maxwait": "2"},
		{"database": "app__auth", "cl_active": "9"},
		{"database": "pgbouncer", "cl_active": "9"},
		{"database": "other", "cl_active": "1", "maxwait": "5"},
	})
	if p.ClientsActive != 4 || p.ClientsWaiting != 1 || p.ServersActive != 2 || p.ServersIdle != 4 || p.MaxWaitS != 5 {
		t.Errorf("%+v", p)
	}
}

func TestUptime(t *testing.T) {
	for in, want := range map[int64]string{0: "-", 59: "0m", 3720: "1h2m", 90000: "1d1h"} {
		if got := Uptime(in); got != want {
			t.Errorf("Uptime(%d) = %s, want %s", in, got, want)
		}
	}
}
