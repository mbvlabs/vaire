package config

import (
	"strings"
	"testing"
)

func TestCPUModeRoundTripAndSet(t *testing.T) {
	cfg := followerCluster()
	if r, err := cfg.Resources(cfg.Clusters[1]); err != nil || r.CPUMode != "normal" {
		t.Fatalf("%+v %v", r, err)
	}
	if err := cfg.Set("clusters.app_follower.cpu_mode", "spare"); err != nil {
		t.Fatal(err)
	}
	cfg.Clusters[1].CPUWeight = 73
	cfg.Clusters[1].CPUQuota = "150%"
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseCluster(raw)
	if err != nil {
		t.Fatal(err)
	}
	got := decoded.Clusters[1]
	if got.CPUMode != "spare" || got.CPUWeight != 73 || got.CPUQuota != "150%" {
		t.Fatal(got)
	}
	r, err := decoded.Resources(got)
	if err != nil || r.CPUMode != "spare" || r.CPUWeight != 73 {
		t.Fatalf("%+v %v", r, err)
	}
	if err := decoded.Set("clusters.app_follower.cpu_mode", "normal"); err != nil {
		t.Fatal(err)
	}
	if decoded.Clusters[1].CPUWeight != 73 {
		t.Fatal("numeric weight changed")
	}
	if _, err := ParseCluster([]byte("[[clusters]]\nname='app'\ncpu_mod='spare'\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
}

func TestCPUResourceValidation(t *testing.T) {
	for _, mode := range []string{"", "normal", "spare"} {
		cfg := followerCluster()
		cfg.Clusters[1].CPUMode = mode
		if p := validationProblems(cfg); strings.Contains(p, "cpu_mode") {
			t.Fatalf("%q: %s", mode, p)
		}
	}
	for _, quota := range []string{"0%", "-1%", "NaN%", "+Inf%", "1.5%", "100000%"} {
		cfg := followerCluster()
		cfg.Clusters[1].CPUQuota = quota
		if p := validationProblems(cfg); !strings.Contains(p, "cpu_quota") {
			t.Fatalf("%q: %s", quota, p)
		}
	}
	cfg := followerCluster()
	cfg.Clusters[1].CPUMode = "idle"
	if p := validationProblems(cfg); !strings.Contains(p, "cpu_mode") {
		t.Fatal(p)
	}
}
