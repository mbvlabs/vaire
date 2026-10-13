package node

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/tune"
)

// budgetHost is the server's side of the oversubscription arithmetic
// (RFC 7.5): RAM and the reserve, what /proc/meminfo says can still start,
// and the data disk.
func budgetHost(cfg config.Cluster, host hostinfo.Info) tune.Host {
	override := 0
	if cfg.Server.MemoryReserve != "" {
		override, _ = config.ParseMemoryMiB(cfg.Server.MemoryReserve)
	}
	h := tune.Host{
		RAMMiB:     host.RAMMiB,
		VCPU:       host.VCPU,
		ReserveMiB: tune.ServerReserve(host.RAMMiB, override),
		DiskGiB:    host.DataDisk.SizeGiB,
	}
	if m, ok := hostinfo.ReadMemory(); ok {
		h.MemoryKnown = true
		h.CommitLimitMiB, h.CommittedMiB, h.AvailableMiB = m.CommitLimitMiB, m.CommittedMiB, m.AvailableMiB
		h.HugePagesFree, h.HugePageSizeKiB = m.HugePagesFree, m.HugePageSizeKiB
	}
	if used, ok := hostinfo.UsedGiB(cfg.Server.DataRoot); ok {
		h.DiskUsedGiB = used
	}
	return h
}

// demandFor is spec's claim on the server: its resolved size, the shared
// memory and huge pages its tuning (with [clusters.settings] applied)
// allocates at start-up, and its max_wal_size.
func demandFor(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) tune.Demand {
	res, _ := cfg.Resources(spec)
	tuned := steps.TuningFor(cfg, spec, host)
	d := tune.Demand{
		Name:            spec.Name,
		Size:            res.Size,
		MemoryMiB:       res.MemoryMiB,
		CPUWeight:       res.CPUWeight,
		CPUQuotaPercent: quotaPercent(res.CPUQuota),
		HugePages:       tuned.HugePages,
		Running:         clusterRunning(spec),
	}
	sharedBuffers := tuned.SharedBuffersMiB
	if v, ok := spec.Settings["shared_buffers"]; ok {
		if mib, err := config.ParseMemoryMiB(v); err == nil {
			sharedBuffers = mib
		}
	}
	d.SharedMemoryMiB = tune.SharedMemoryMiB(sharedBuffers)
	switch strings.TrimSpace(spec.Settings["huge_pages"]) {
	case "on":
		d.HugePagesRequired = true
		d.HugePages = tune.HugePages(sharedBuffers)
	case "off":
		d.HugePages = 0
	}
	walSize := ""
	for _, s := range tuned.Settings {
		if s.Name == "max_wal_size" {
			walSize = s.Value
		}
	}
	if v, ok := spec.Settings["max_wal_size"]; ok {
		walSize = v
	}
	if mib, err := config.ParseMemoryMiB(walSize); err == nil {
		d.MaxWALGiB = mib / 1024
	}
	return d
}

// budgetSheet is every cluster's demand added up against the budget.
func budgetSheet(cfg config.Cluster, host hostinfo.Info) (tune.Host, tune.Sheet) {
	h := budgetHost(cfg, host)
	demands := make([]tune.Demand, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		demands = append(demands, demandFor(cfg, c, host))
	}
	return h, tune.NewSheet(h, demands)
}

// sizeCandidates is spec as each named size, smallest first, with its own
// memory key dropped: what db create steps down through and what the
// would_not_start hint picks from.
func sizeCandidates(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) []tune.Demand {
	out := make([]tune.Demand, 0, len(config.Sizes))
	for _, size := range config.Sizes {
		c := spec
		c.Size, c.Memory = size.Name, ""
		out = append(out, demandFor(cfg, c, host))
	}
	return out
}

// oversubscriptionWarnings are cfg's memory_overcommitted, cpu_crowded and
// disk_crowded warnings on this server (RFC 7.5). plan, apply, config set,
// tune show and the db commands carry them.
func oversubscriptionWarnings(cfg config.Cluster, host hostinfo.Info) []output.Warning {
	if len(cfg.Clusters) == 0 {
		return nil
	}
	h, sheet := budgetSheet(cfg, host)
	return toWarnings(sheet.Warnings(h))
}

func toWarnings(in []tune.Warning) []output.Warning {
	out := make([]output.Warning, 0, len(in))
	for _, w := range in {
		out = append(out, output.Warning{Code: w.Code, Message: w.Message})
	}
	return out
}

// checkStarts refuses spec when its postmaster could not start
// (would_not_start), with the arithmetic and the smallest size that would.
// freedMiB is the shared memory a restart gives back first.
func checkStarts(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info, freedMiB int) error {
	h := budgetHost(cfg, host)
	reason := tune.WouldNotStart(h, demandFor(cfg, spec, host), freedMiB)
	if reason == "" {
		return nil
	}
	details := map[string]any{"reason": tune.ReasonWouldNotStart, "database": spec.Name,
		"commit_limit_mib": h.CommitLimitMiB, "committed_mib": h.CommittedMiB, "huge_pages_free": h.HugePagesFree}
	hint := "No size fits what is left: drop a database, free memory on the server, or move one to its own node."
	if smallest := tune.SmallestThatStarts(h, sizeCandidates(cfg, spec, host), freedMiB); smallest != "" {
		details["smallest_size"] = smallest
		hint = "Size " + smallest + " would start: pass --size " + smallest + "."
	}
	return output.NewError(output.CodePrecondition, reason+", so PostgreSQL would not start", output.ExitPrecondition, hint).WithDetails(details)
}

// clusterRunning reports whether spec's postmaster holds its socket.
func clusterRunning(spec config.ClusterSpec) bool {
	_, err := os.Stat(filepath.Join(steps.PGSocketDir, fmt.Sprintf(".s.PGSQL.%d.lock", spec.Port)))
	return err == nil
}

// clusterMemoryCurrentMiB is memory.current of name's service cgroup.
func clusterMemoryCurrentMiB(name string) (int, bool) {
	raw, err := os.ReadFile(filepath.Join("/sys/fs/cgroup/system.slice/system-postgresql.slice", steps.PGServiceName(name)+".service", "memory.current"))
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil {
		return 0, false
	}
	return int(n / (1 << 20)), true
}

// quotaPercent turns cpu_quota ("150%") into 150; unset or malformed is 0.
func quotaPercent(v string) int {
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimSpace(v), "%"))
	if err != nil {
		return 0
	}
	return n
}
