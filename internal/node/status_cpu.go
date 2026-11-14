package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/steps"
)

type statusCPURequest struct {
	Mode  string `json:"mode"`
	Quota string `json:"quota"`
}

type statusCPUEffective struct {
	Idle            *bool   `json:"idle,omitempty"`
	QuotaPeriodUsec *uint64 `json:"quota_period_usec,omitempty"`
	QuotaUsec       *uint64 `json:"quota_usec,omitempty"`
	Unlimited       *bool   `json:"unlimited,omitempty"`
	Weight          *uint64 `json:"weight,omitempty"`
}

type statusCPU struct {
	Requested         statusCPURequest    `json:"requested"`
	EffectiveKnown    bool                `json:"effective_known"`
	Effective         *statusCPUEffective `json:"effective,omitempty"`
	UsageUsec         *uint64             `json:"usage_usec,omitempty"`
	ObservedAt        *time.Time          `json:"observed_at,omitempty"`
	Reason            string              `json:"reason,omitempty"`
	SampleGeneration  string              `json:"sample_generation,omitempty"`
	SampleMonotonicNS *int64              `json:"sample_monotonic_ns,omitempty"`
}

var statusInvocationPattern = regexp.MustCompile(`^[a-fA-F0-9]{32}$`)
var statusCPURead = statusCPUProbe

// statusCPUProbe sandwiches the cgroup observation between systemd invocation
// reads. The opaque generation binds that invocation to the cgroup inode.
// Its monotonic timestamp is meaningful only with that generation.
func statusCPUProbe(ctx context.Context, spec config.ClusterSpec) (hostinfo.CPUObservation, string, *int64) {
	if config.CheckDatabaseName(spec.Name) != nil {
		return hostinfo.CPUObservation{Reason: "invalid_service"}, "", nil
	}
	unit := steps.PGServiceName(spec.Name) + ".service"
	invocation := func() (string, error) {
		return steps.Probe(ctx, "systemctl", "show", "--property=InvocationID", "--value", "--", unit)
	}
	return statusCPUProbeWith(invocation, func() hostinfo.CPUObservation {
		return hostinfo.ObserveCPUService(ctx, unit)
	}, statusMonotonicNS, time.Since)
}

func statusCPUProbeWith(invocation func() (string, error), observe func() hostinfo.CPUObservation,
	monotonic func() (int64, bool), since func(time.Time) time.Duration,
) (hostinfo.CPUObservation, string, *int64) {
	before, err := invocation()
	obs := observe()
	clock, clockOK := monotonic()
	elapsed := since(obs.ObservedAt)
	after, err2 := invocation()
	if err != nil || err2 != nil || before != after || !statusInvocationPattern.MatchString(before) ||
		obs.Reason == "service_identity_unverified" || obs.Identity == nil || obs.UsageUsec == nil || obs.ObservedAt.IsZero() || !clockOK {
		return obs, "", nil
	}
	st, ok := obs.Identity.Sys().(*syscall.Stat_t)
	if !ok {
		return obs, "", nil
	}
	if elapsed < 0 || int64(elapsed) >= clock {
		return obs, "", nil
	}
	stamp := clock - int64(elapsed)
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d:%d", strings.ToLower(before), st.Dev, st.Ino)))
	return obs, hex.EncodeToString(sum[:]), &stamp
}

func cpuStatus(res config.Resources, obs hostinfo.CPUObservation, generation string, monotonic *int64) *statusCPU {
	cpu := &statusCPU{Requested: statusCPURequest{Mode: res.CPUMode, Quota: res.CPUQuota}, Reason: obs.Reason}
	if cpu.Requested.Mode == "" {
		cpu.Requested.Mode = "normal"
	}
	if obs.Reason == "service_identity_unverified" {
		return cpu
	}
	cpu.EffectiveKnown = obs.Screen.Capability == hostinfo.CPUCapable && obs.Idle != nil && obs.Quota != nil && obs.Weight != nil
	if obs.Idle != nil || obs.Quota != nil || obs.Weight != nil {
		eff := &statusCPUEffective{Idle: obs.Idle, Weight: obs.Weight}
		if obs.Quota != nil {
			period, unlimited := obs.Quota.PeriodUsec, obs.Quota.Unlimited
			eff.QuotaPeriodUsec, eff.Unlimited = &period, &unlimited
			if !unlimited {
				quota := obs.Quota.QuotaUsec
				eff.QuotaUsec = &quota
			}
		}
		cpu.Effective = eff
	}
	if obs.UsageUsec != nil && !obs.ObservedAt.IsZero() {
		cpu.UsageUsec = obs.UsageUsec
		at := obs.ObservedAt.UTC()
		cpu.ObservedAt = &at
		if generation != "" && monotonic != nil && *monotonic > 0 {
			cpu.SampleGeneration, cpu.SampleMonotonicNS = generation, monotonic
		}
	}
	if !cpu.EffectiveKnown && cpu.Reason == "" {
		cpu.Reason = obs.Screen.Reason
		if cpu.Reason == "" {
			cpu.Reason = "effective_policy_unknown"
		}
	}
	return cpu
}
