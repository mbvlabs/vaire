package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseWindow(t *testing.T) {
	start, end, err := ParseWindow("02:00-05:00")
	if err != nil || start != 2*time.Hour || end != 5*time.Hour {
		t.Fatalf("%v %v %v", start, end, err)
	}
	if WindowLength(start, end) != 3*time.Hour {
		t.Error("length")
	}
	s, e, err := ParseWindow("23:30-01:00")
	if err != nil || WindowLength(s, e) != 90*time.Minute {
		t.Errorf("across midnight: %v %v", WindowLength(s, e), err)
	}
	for _, bad := range []string{"", "02:00", "2-5", "02:00-02:00", "25:00-01:00"} {
		if _, _, err := ParseWindow(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestInWindow(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 10, 14, h, m, 0, 0, time.UTC) }
	start, end, _ := ParseWindow("02:00-05:00")
	if in, left := InWindow(at(3, 0), start, end); !in || left != 2*time.Hour {
		t.Errorf("03:00: %v %v", in, left)
	}
	if in, _ := InWindow(at(5, 0), start, end); in {
		t.Error("05:00 is the end")
	}
	s, e, _ := ParseWindow("23:00-01:00")
	if in, left := InWindow(at(0, 30), s, e); !in || left != 30*time.Minute {
		t.Errorf("00:30 across midnight: %v %v", in, left)
	}
	if in, _ := InWindow(at(12, 0), s, e); in {
		t.Error("noon")
	}
}

func TestMetricsValidation(t *testing.T) {
	cfg := DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []NodeSpec{{Name: "db1", Role: RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Metrics.Allowlist = []string{"not-a-cidr"}
	cfg.Metrics.PathPrefix = "/"
	cfg.Maintenance.Window = "nope"
	err := cfg.ValidateFor(ScopeFull)
	if err == nil {
		t.Fatal("bad metrics config passed")
	}
	for _, want := range []string{"metrics.allowlist", "metrics.path_prefix", "maintenance.window"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %s: %v", want, err)
		}
	}
}

func TestPoolsLeaveTheReservedSlots(t *testing.T) {
	for _, size := range Sizes {
		cfg := DefaultCluster()
		r, err := cfg.Resources(ClusterSpec{Name: "app", Size: size.Name})
		if err != nil {
			t.Fatal(err)
		}
		if r.PoolSize+r.SessionPoolSize+3 != r.MaxConnections {
			t.Errorf("%s: pool %d + session %d + 3 != %d", size.Name, r.PoolSize, r.SessionPoolSize, r.MaxConnections)
		}
	}
}
