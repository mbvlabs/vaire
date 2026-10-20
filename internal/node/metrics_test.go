package node

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/tune"
)

func TestHostFamilies(t *testing.T) {
	r := hostReadings{
		Clusters: []clusterReading{
			{Name: "app", OK: true, RestartPending: true, Cgroup: hostinfo.Cgroup{
				MemoryCurrent: 100, MemoryHigh: 640 << 20,
				MemoryEvents: map[string]int64{"high": 4},
				Pressure: map[string]hostinfo.Pressure{
					"memory": {Some: hostinfo.PressureLine{TotalSec: 1.5}, Full: hostinfo.PressureLine{TotalSec: 0.5}, HasFull: true},
					"cpu":    {Some: hostinfo.PressureLine{TotalSec: 2}},
				},
				CPUUsageSec: 12.5, IOReadBytes: 7, IOWriteByte: 9,
			}},
			{Name: "other"},
		},
		Sheet:       tune.Sheet{BudgetMiB: 1024, AllocatedMiB: 512, ReserveMiB: 512},
		Host:        tune.Host{MemoryKnown: true, CommitLimitMiB: 2048, CommittedMiB: 1000},
		TLSNotAfter: time.Unix(1800000000, 0),
		Deferred:    map[string]string{"other": "2026-10-14T00:00:00Z"},
	}
	out := string(promfile.Render(hostFamilies(r)))
	for _, line := range []string{
		`vaire_cluster_running{db="app"} 1`,
		`vaire_cluster_running{db="other"} 0`,
		`vaire_cluster_memory_current_bytes{db="app"} 100`,
		`vaire_cluster_memory_high_bytes{db="app"} 671088640`,
		`vaire_cluster_memory_events_total{db="app",event="high"} 4`,
		`vaire_cluster_pressure_seconds_total{db="app",resource="memory",kind="full"} 0.5`,
		`vaire_cluster_pressure_seconds_total{db="app",resource="cpu",kind="some"} 2`,
		`vaire_cluster_cpu_seconds_total{db="app"} 12.5`,
		`vaire_cluster_io_bytes_total{db="app",direction="write"} 9`,
		`vaire_cluster_restart_pending{db="app"} 1`,
		`vaire_memory_budget_bytes 1073741824`,
		`vaire_commit_limit_bytes 2147483648`,
		`vaire_tls_cert_expiry_timestamp_seconds 1800000000`,
		`vaire_deferred_restart_requested_timestamp_seconds{db="other"} 1791936000`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %s in\n%s", line, out)
		}
	}
	if strings.Contains(out, `resource="cpu",kind="full"`) {
		t.Error("cpu has no full line")
	}
	if strings.Contains(out, `vaire_cluster_memory_max_bytes`) {
		t.Error("no MemoryMax set, but a max line")
	}
}

func TestPendingFromIDs(t *testing.T) {
	p := pendingFromIDs([]string{"postgres-config-v1@app"})
	if !pendingFor(p, "app") || pendingFor(p, "other") {
		t.Errorf("instance pending %v", p)
	}
	p = pendingFromIDs([]string{"kernel-tuning-v1"})
	if !pendingFor(p, "other") {
		t.Error("a server-level restart counts for every cluster")
	}
}
