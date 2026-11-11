package config

import (
	"math"
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

func TestRepackBloatThreshold(t *testing.T) {
	for _, valid := range []float64{0.0001, 0.4, 1} {
		if err := CheckRepackBloatThreshold(valid); err != nil {
			t.Errorf("%v: %v", valid, err)
		}
	}
	for _, invalid := range []float64{0, -0.1, 1.0001, math.NaN(), math.Inf(1), math.Inf(-1)} {
		if err := CheckRepackBloatThreshold(invalid); err == nil {
			t.Errorf("%v passed", invalid)
		}
	}
	if got := DefaultCluster().Maintenance.RepackBloatThreshold; got != 0.40 {
		t.Errorf("default threshold %v, want 0.40", got)
	}
	parsed, err := ParseCluster([]byte("[maintenance]\nwindow = '02:00-05:00'\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := parsed.Maintenance.RepackBloatThreshold; got != 0.40 {
		t.Errorf("omitted threshold %v, want 0.40", got)
	}
}

func TestRepackBloatThresholdValidation(t *testing.T) {
	validCfg := func() Cluster {
		cfg := DefaultCluster()
		cfg.ClusterName = "prod"
		cfg.Domain = "example.com"
		cfg.Nodes = []NodeSpec{{Name: "db1", Role: RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
		return cfg
	}
	if err := validCfg().ValidateFor(ScopeFull); err != nil {
		t.Fatalf("default config: %v", err)
	}
	for _, invalid := range []float64{0, -1, 1.01, math.NaN(), math.Inf(1)} {
		cfg := validCfg()
		cfg.Maintenance.RepackBloatThreshold = invalid
		err := cfg.ValidateFor(ScopeFull)
		if err == nil || !strings.Contains(err.Error(), "maintenance.repack_bloat_threshold") {
			t.Errorf("%v: %v", invalid, err)
		}
	}
	cfg := validCfg()
	cfg.Maintenance.RepackBloatThreshold = 1
	if err := cfg.ValidateFor(ScopeFull); err != nil {
		t.Errorf("threshold 1: %v", err)
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
