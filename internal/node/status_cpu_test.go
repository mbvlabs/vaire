package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestStatusCPUProbeExcludesFinalInvocationLatency(t *testing.T) {
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	usage := uint64(10)
	obs := hostinfo.CPUObservation{Identity: info, UsageUsec: &usage, ObservedAt: time.Now()}
	elapsed := 10 * time.Millisecond
	finalDelay := 2 * time.Second
	calls := 0
	invocation := func() (string, error) {
		calls++
		if calls == 2 {
			elapsed = finalDelay
		}
		return "0123456789abcdef0123456789abcdef", nil
	}
	_, generation, stamp := statusCPUProbeWith(invocation, func() hostinfo.CPUObservation { return obs },
		func() (int64, bool) { return int64(5 * time.Second), true },
		func(time.Time) time.Duration { return elapsed })
	if generation == "" || stamp == nil || *stamp != int64(4990*time.Millisecond) {
		t.Fatalf("generation %q timestamp %v", generation, stamp)
	}
	first := *stamp
	calls, elapsed = 0, 10*time.Millisecond
	finalDelay = 4 * time.Second
	usage += 500000
	_, generation2, stamp2 := statusCPUProbeWith(invocation, func() hostinfo.CPUObservation { return obs },
		func() (int64, bool) { return int64(6 * time.Second), true },
		func(time.Time) time.Duration { return elapsed })
	if stamp2 == nil || generation != generation2 || float64(500000)*100000/float64(*stamp2-first) != 50 {
		t.Fatalf("generation %q timestamp %v", generation2, stamp2)
	}
}

func TestStatusCPUFacts(t *testing.T) {
	now := time.Now()
	idle, weight, usage, stamp := true, uint64(1), uint64(42), int64(1000)
	obs := hostinfo.CPUObservation{Screen: hostinfo.CPUScreen{Capability: hostinfo.CPUCapable},
		Idle: &idle, Weight: &weight, UsageUsec: &usage, ObservedAt: now,
		Quota: &hostinfo.CPUQuota{Unlimited: true, PeriodUsec: 100000}}
	cpu := cpuStatus(config.Resources{CPUMode: "spare", CPUQuota: ""}, obs, "generation", &stamp)
	if !cpu.EffectiveKnown || cpu.Effective == nil || cpu.Effective.QuotaUsec != nil || !*cpu.Effective.Unlimited || cpu.UsageUsec == nil {
		t.Fatalf("%+v", cpu)
	}
	raw, err := json.Marshal(cpu)
	if err != nil || !strings.Contains(string(raw), `"unlimited":true`) || strings.Contains(string(raw), `"quota_usec":`) {
		t.Fatalf("%s %v", raw, err)
	}
	obs.Reason = "service_identity_unverified"
	cpu = cpuStatus(config.Resources{}, obs, "generation", &stamp)
	if cpu.EffectiveKnown || cpu.UsageUsec != nil || cpu.SampleGeneration != "" || cpu.Effective != nil {
		t.Fatalf("%+v", cpu)
	}
	cpu = cpuStatus(config.Resources{}, hostinfo.CPUObservation{}, "", nil)
	if cpu.EffectiveKnown || cpu.Requested.Mode != "normal" || cpu.Reason == "" {
		t.Fatalf("%+v", cpu)
	}
}

func TestStatusDataFreeRequiresActualManagedPath(t *testing.T) {
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "follower", Follows: "source"}
	want := cfg.Server.DataDirectory(spec.Name)
	freeCalls := 0
	free := func(path string) (uint64, bool) {
		freeCalls++
		if path != want {
			t.Fatalf("unexpected path %s", path)
		}
		return 0, true
	}
	path := want
	query := func(context.Context, string, string, map[string]string) ([]map[string]string, error) {
		return []map[string]string{{"data_directory": path}}, nil
	}
	got := statusDataFree(context.Background(), cfg, spec, query, free)
	if got == nil || *got != 0 || freeCalls != 1 {
		t.Fatal(got, freeCalls)
	}
	path = "/"
	if statusDataFree(context.Background(), cfg, spec, query, free) != nil || freeCalls != 1 {
		t.Fatal("mismatched path accepted")
	}
	fail := func(context.Context, string, string, map[string]string) ([]map[string]string, error) {
		return nil, errors.New("unavailable")
	}
	if statusDataFree(context.Background(), cfg, spec, fail, free) != nil || freeCalls != 1 {
		t.Fatal("unavailable directory accepted")
	}
}
