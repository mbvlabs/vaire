package node

import (
	"context"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/tune"
)

// clusterReading is one cluster's cgroup at refresh time; ok is false for a
// cluster that isn't running.
type clusterReading struct {
	Name           string
	Cgroup         hostinfo.Cgroup
	OK             bool
	RestartPending bool
}

// hostReadings is everything vaire-host.prom is made of.
type hostReadings struct {
	Clusters []clusterReading
	Sheet    tune.Sheet
	Host     tune.Host
	// TLSNotAfter is the certificate's expiry, zero when unreadable.
	TLSNotAfter time.Time
	Deferred    map[string]string
}

// hostFamilies are vaire-host.prom's families (Phase 4): each cluster's
// cgroup memory against its limits, memory events, PSI, CPU and I/O
// counters and a pending restart; the server's memory budget, commit limit,
// certificate expiry and deferred restarts.
func hostFamilies(r hostReadings) []*promfile.Family {
	memCur := promfile.Gauge("vaire_cluster_memory_current_bytes", "memory.current of each database's cluster cgroup.")
	memHigh := promfile.Gauge("vaire_cluster_memory_high_bytes", "MemoryHigh of each database's cluster (the throttling point).")
	memMax := promfile.Gauge("vaire_cluster_memory_max_bytes", "MemoryMax of each database's cluster, when one is set.")
	events := promfile.Counter("vaire_cluster_memory_events_total", "memory.events of each database's cluster: high (throttled), max, oom, oom_kill.")
	pressure := promfile.Counter("vaire_cluster_pressure_seconds_total", "Pressure stall time of each database's cluster, by resource and kind.")
	cpu := promfile.Counter("vaire_cluster_cpu_seconds_total", "CPU time used by each database's cluster.")
	io := promfile.Counter("vaire_cluster_io_bytes_total", "Bytes read and written by each database's cluster.")
	up := promfile.Gauge("vaire_cluster_running", "Whether each database's cluster has a running cgroup.")
	pending := promfile.Gauge("vaire_cluster_restart_pending", "Whether each database's cluster waits for a restart to apply settings.")
	for _, c := range r.Clusters {
		db := promfile.L("db", c.Name)
		running := 0.0
		if c.OK {
			running = 1
		}
		up.Add(running, db...)
		restart := 0.0
		if c.RestartPending {
			restart = 1
		}
		pending.Add(restart, db...)
		if !c.OK {
			continue
		}
		g := c.Cgroup
		memCur.Add(float64(g.MemoryCurrent), db...)
		if g.MemoryHigh > 0 {
			memHigh.Add(float64(g.MemoryHigh), db...)
		}
		if g.MemoryMax > 0 {
			memMax.Add(float64(g.MemoryMax), db...)
		}
		for _, ev := range []string{"high", "max", "oom", "oom_kill"} {
			events.Add(float64(g.MemoryEvents[ev]), promfile.L("db", c.Name, "event", ev)...)
		}
		for _, res := range []string{"cpu", "memory", "io"} {
			p, ok := g.Pressure[res]
			if !ok {
				continue
			}
			pressure.Add(p.Some.TotalSec, promfile.L("db", c.Name, "resource", res, "kind", "some")...)
			if p.HasFull {
				pressure.Add(p.Full.TotalSec, promfile.L("db", c.Name, "resource", res, "kind", "full")...)
			}
		}
		cpu.Add(g.CPUUsageSec, db...)
		io.Add(float64(g.IOReadBytes), promfile.L("db", c.Name, "direction", "read")...)
		io.Add(float64(g.IOWriteByte), promfile.L("db", c.Name, "direction", "write")...)
	}

	const mib = 1 << 20
	budget := promfile.Gauge("vaire_memory_budget_bytes", "Memory the database clusters share: RAM minus the server reserve.")
	budget.Add(float64(r.Sheet.BudgetMiB) * mib)
	allocated := promfile.Gauge("vaire_memory_allocated_bytes", "Memory the database clusters' sizes add up to.")
	allocated.Add(float64(r.Sheet.AllocatedMiB) * mib)
	reserve := promfile.Gauge("vaire_memory_reserve_bytes", "Memory kept for the OS, the poolers, nginx, the exporters and vaire.")
	reserve.Add(float64(r.Sheet.ReserveMiB) * mib)
	commit := promfile.Gauge("vaire_commit_limit_bytes", "CommitLimit from /proc/meminfo.")
	committed := promfile.Gauge("vaire_committed_bytes", "Committed_AS from /proc/meminfo.")
	if r.Host.MemoryKnown {
		commit.Add(float64(r.Host.CommitLimitMiB) * mib)
		committed.Add(float64(r.Host.CommittedMiB) * mib)
	}
	tls := promfile.Gauge("vaire_tls_cert_expiry_timestamp_seconds", "Expiry of the node's certificate.")
	if !r.TLSNotAfter.IsZero() {
		tls.Add(float64(r.TLSNotAfter.Unix()))
	}
	deferred := promfile.Gauge("vaire_deferred_restart_requested_timestamp_seconds", "Databases whose restart waits for the maintenance window, by when it was asked.")
	for _, db := range promfile.SortedKeys(r.Deferred) {
		if t, err := time.Parse(time.RFC3339, r.Deferred[db]); err == nil {
			deferred.Add(float64(t.Unix()), promfile.L("db", db)...)
		}
	}
	return []*promfile.Family{up, memCur, memHigh, memMax, events, pressure, cpu, io, pending,
		budget, allocated, reserve, commit, committed, tls, deferred}
}

// readHost gathers hostReadings without touching PostgreSQL: cgroup files,
// /proc/meminfo, the journal's pending restarts, the certificate and
// state.json.
func readHost(cfg config.Cluster, host hostinfo.Info) hostReadings {
	h, sheet := budgetSheet(cfg, host)
	r := hostReadings{Sheet: sheet, Host: h}
	pending := pendingRestarts()
	for _, spec := range cfg.Clusters {
		g, ok := hostinfo.ReadCgroup(hostinfo.ServiceCgroup(steps.PGServiceName(spec.Name)))
		r.Clusters = append(r.Clusters, clusterReading{Name: spec.Name, Cgroup: g, OK: ok, RestartPending: pendingFor(pending, spec.Name)})
	}
	if cert := readCert(steps.TLSCertPath); cert != nil {
		r.TLSNotAfter = cert.NotAfter
	}
	if st, err := engine.LoadState(engine.StatePath); err == nil {
		r.Deferred = st.DeferredRestarts
	}
	return r
}

// pendingRestarts are the clusters with a step instance whose restart the
// journal still holds. A server-level step's pending restart is keyed "*"
// and counts for every cluster (pendingFor).
func pendingRestarts() map[string]bool {
	j, err := engine.LoadJournal(engine.JournalPath)
	if err != nil {
		return map[string]bool{}
	}
	return pendingFromIDs(j.RestartPending)
}

func pendingFromIDs(ids []string) map[string]bool {
	out := map[string]bool{}
	for _, id := range ids {
		_, cluster := engine.SplitID(id)
		if cluster == "" {
			cluster = "*"
		}
		out[cluster] = true
	}
	return out
}

// pendingFor reports whether db has a pending restart in pending.
func pendingFor(pending map[string]bool, db string) bool { return pending[db] || pending["*"] }

// refreshMetrics rewrites vaire.prom and vaire-host.prom. It is the
// metrics-refresh job (every minute) and runs after the backup jobs too.
func refreshMetrics(_ context.Context, cfg config.Cluster) error {
	steps.WriteMetrics(cfg)
	host, _ := hostinfo.Detect(cfg.Server.DataRoot)
	return promfile.Write(promfile.Host, hostFamilies(readHost(cfg, host)))
}
