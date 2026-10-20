// Package tune is the pure tuning calculator (§12): a function of one
// cluster's memory, its share of the hardware, its workload profile and
// connection limit that returns PostgreSQL settings with a reason for each,
// and the server's memory budget (RFC 7.3). It does no I/O.
package tune

import "fmt"

// Profile is the workload shape.
const (
	ProfileOLTP      = "oltp"
	ProfileMixed     = "mixed"
	ProfileAnalytics = "analytics"
)

// Input is the calculator's inputs for one cluster. RAMMiB is the cluster's
// memory, not the server's (RFC 7.3), and DataDiskGiB its share of the data
// disk (RFC 7.7). VCPU is the server's; Clusters caps the CPU-driven
// settings at the cluster's share of it. MaxConnections comes from the
// cluster's size; zero falls back to 4 x vCPU.
type Input struct {
	RAMMiB            int
	VCPU              int
	Clusters          int
	DiskRotational    bool
	DataDiskGiB       int
	Profile           string
	MaxConnections    int
	ClientConnections int
	PGMajor           int
	Extensions        []string
}

// HugePagesMinMiB is the cluster memory from which huge_pages is try. A
// smaller cluster uses off, so a fragmented pool can't leave its start-up at
// the mercy of another cluster's (RFC 7.3).
const HugePagesMinMiB = 2048

// Server reserve (RFC 7.3): what the OS, both poolers, nginx, vaire's jobs and
// the page cache keep before the clusters get the rest.
const (
	ReserveMinMiB  = 512
	ReservePercent = 25
)

// ServerReserve is max(512 MiB, 25% of RAM), or overrideMiB when it is set.
func ServerReserve(ramMiB, overrideMiB int) int {
	if overrideMiB > 0 {
		return overrideMiB
	}
	return maxInt(ReserveMinMiB, ramMiB*ReservePercent/100)
}

// ClusterBudget is the memory left for clusters after the server reserve.
func ClusterBudget(ramMiB, reserveMiB int) int {
	return maxInt(0, ramMiB-reserveMiB)
}

// CPUShare is the vCPU a cluster plans for: vCPU / clusters, at least 2, and
// never more than the server has.
func CPUShare(vcpu, clusters int) int {
	if clusters < 1 {
		clusters = 1
	}
	return minInt(vcpu, maxInt(2, vcpu/clusters))
}

// Setting is one PostgreSQL setting with its reason and restart context.
type Setting struct {
	Name    string `json:"name"`
	Value   string `json:"value"`
	Unit    string `json:"unit,omitempty"`
	Reason  string `json:"reason"`
	Context string `json:"context"` // postmaster | sighup | user
}

// Result is the calculator's output.
type Result struct {
	Settings         []Setting `json:"settings"`
	SharedBuffersMiB int       `json:"shared_buffers_mib"`
	HugePages        int       `json:"huge_pages"`
}

// Calculate returns the settings for in (§12.1).
func Calculate(in Input) Result {
	r := &calc{in: in}
	r.add()
	return r.res
}

type calc struct {
	res   Result
	in    Input
	items []Setting
}

// set appends a setting.
func (c *calc) set(name, value, unit, reason, context string) {
	c.items = append(c.items, Setting{Name: name, Value: value, Unit: unit, Reason: reason, Context: context})
}

func (c *calc) add() {
	in := c.in
	ram := in.RAMMiB
	cpu := CPUShare(in.VCPU, in.Clusters)
	cpuReason := fmt.Sprintf("%d vCPU", cpu)
	if cpu != in.VCPU {
		cpuReason = fmt.Sprintf("%d vCPU share of %d over %d clusters", cpu, in.VCPU, in.Clusters)
	}
	analytics := in.Profile == ProfileAnalytics
	ssd := !in.DiskRotational

	// shared_buffers.
	var sharedBuffers int
	if ram < 2048 {
		sharedBuffers = maxInt(128, int(float64(ram)*0.20))
	} else {
		sharedBuffers = minInt(int(float64(ram)*0.25), 65536)
	}
	c.res.SharedBuffersMiB = sharedBuffers
	c.set("shared_buffers", fmt.Sprintf("%dMB", sharedBuffers), "MB",
		fmt.Sprintf("%s of the cluster's %d MiB", percentReason(ram, sharedBuffers), ram), "postmaster")

	// effective_cache_size.
	ratio := 0.70
	if analytics {
		ratio = 0.75
	}
	c.set("effective_cache_size", fmt.Sprintf("%dMB", int(float64(ram)*ratio)), "MB",
		fmt.Sprintf("%.0f%% of the cluster's %d MiB", ratio*100, ram), "user")

	// Connections.
	maxConn := clampInt(4*cpu, 50, 400)
	connReason := fmt.Sprintf("4 x %d vCPU, PgBouncer fans in", cpu)
	if in.MaxConnections > 0 {
		maxConn = in.MaxConnections
		connReason = "from the cluster's size; both poolers fan in"
	}
	c.set("max_connections", itoa(maxConn), "", connReason, "postmaster")
	// The two sum to config.ReservedConnections, the slots both pools leave free.
	// Superusers may take reserved_connections' slots too, so no separate
	// superuser set is kept.
	c.set("superuser_reserved_connections", "0", "", "postgres shares reserved_connections", "postmaster")
	c.set("reserved_connections", "3", "", "shared by vaire_admin, vaire_monitor (pg_use_reserved_connections) and postgres", "postmaster")

	// work_mem.
	factor := map[string]int{ProfileOLTP: 3, ProfileMixed: 2, ProfileAnalytics: 1}[in.Profile]
	if factor == 0 {
		factor = 2
	}
	ceiling := 256
	if analytics {
		ceiling = 1024
	}
	remaining := ram - sharedBuffers
	if remaining < 0 {
		remaining = 0
	}
	workMem := remaining / (maxConn * factor)
	workMem = clampInt(workMem, 4, ceiling)
	c.set("work_mem", fmt.Sprintf("%dMB", workMem), "MB",
		fmt.Sprintf("(%d - %d) MiB / (%d connections x %d)", ram, sharedBuffers, maxConn, factor), "user")

	c.set("hash_mem_multiplier", "2.0", "", "allow hash operations to use more than work_mem", "user")

	maint := clampInt(ram/16, 64, 2048)
	c.set("maintenance_work_mem", fmt.Sprintf("%dMB", maint), "MB", fmt.Sprintf("the cluster's %d MiB / 16", ram), "user")
	c.set("autovacuum_work_mem", fmt.Sprintf("%dMB", minInt(maint, 1024)), "MB", "capped at 1 GiB", "sighup")

	// Huge pages.
	hugePages := "off"
	if ram >= HugePagesMinMiB {
		hugePages = "try"
	}
	c.set("huge_pages", hugePages, "", hugePagesReason(ram), "postmaster")
	if hugePages == "try" {
		c.res.HugePages = HugePages(sharedBuffers)
	}

	// WAL and checkpoints.
	c.set("wal_level", "logical", "", "archiving, replication and logical-replication followers (Phases 3 and 6)", "postmaster")
	c.set("wal_compression", "zstd", "", "cheap CPU for less WAL traffic", "user")
	c.set("wal_buffers", "-1", "", "let PostgreSQL size WAL buffers from shared_buffers", "postmaster")
	c.set("max_wal_senders", "10", "", "headroom for replication and pgBackRest (Phase 3)", "postmaster")
	c.set("min_wal_size", gib(clampFloat(float64(in.DataDiskGiB)*0.02, 1, 8)), "GiB",
		fmt.Sprintf("2%% of the cluster's %d GiB share of the data disk", in.DataDiskGiB), "sighup")
	c.set("max_wal_size", gib(clampFloat(float64(in.DataDiskGiB)*0.10, 2, 64)), "GiB",
		fmt.Sprintf("10%% of the cluster's %d GiB share of the data disk", in.DataDiskGiB), "sighup")
	c.set("checkpoint_timeout", "15min", "", "spread checkpoints over a quarter hour", "sighup")
	c.set("checkpoint_completion_target", "0.9", "", "smooth checkpoint I/O", "sighup")

	// I/O.
	if ssd {
		c.set("random_page_cost", "1.1", "", "SSD: random reads are nearly sequential", "user")
		c.set("effective_io_concurrency", "200", "", "SSD parallelism", "user")
		c.set("maintenance_io_concurrency", "200", "", "SSD parallelism for maintenance", "user")
	} else {
		c.set("random_page_cost", "4.0", "", "HDD: random reads cost more", "user")
		c.set("effective_io_concurrency", "2", "", "HDD: little seek parallelism", "user")
		c.set("maintenance_io_concurrency", "10", "", "HDD maintenance parallelism", "user")
	}
	c.set("io_method", "worker", "", "PostgreSQL 18 default; io_uring is an explicit opt-in", "postmaster")
	c.set("io_workers", itoa(maxInt(3, cpu/4)), "", "one I/O worker per 4 vCPU, at least 3", "sighup")

	// Parallelism.
	c.set("max_worker_processes", itoa(maxInt(cpu+8, 8)), "", cpuReason+" + 8 background workers", "postmaster")
	c.set("max_parallel_workers", itoa(cpu), "", "one parallel worker per vCPU: "+cpuReason, "postmaster")
	c.set("max_parallel_workers_per_gather", itoa(parallelGather(in.Profile, cpu)), "", profileReason(in.Profile, "parallelism"), "user")
	c.set("max_parallel_maintenance_workers", itoa(minInt(4, maxInt(1, cpu/2))), "", "parallel maintenance workers", "user")
	jit := "off"
	if analytics {
		jit = "on"
	}
	c.set("jit", jit, "", jitReason(analytics), "user")

	// Statistics and autovacuum.
	target := 100
	if analytics {
		target = 500
	}
	c.set("default_statistics_target", itoa(target), "", profileReason(in.Profile, "statistics detail"), "user")
	c.set("autovacuum_worker_slots", "16", "", "room for concurrent autovacuum workers", "postmaster")
	c.set("autovacuum_max_workers", itoa(clampInt(cpu/2, 3, 8)), "", fmt.Sprintf("%d vCPU / 2, 3-8 workers", cpu), "sighup")
	c.set("autovacuum_naptime", "30s", "", "check for dead tuples twice a minute", "sighup")
	c.set("autovacuum_vacuum_scale_factor", "0.05", "", "vacuum earlier than the 0.2 default", "sighup")
	c.set("autovacuum_vacuum_insert_scale_factor", "0.05", "", "vacuum insert-only tables too", "sighup")
	c.set("autovacuum_analyze_scale_factor", "0.02", "", "analyze earlier than the 0.1 default", "sighup")
	if ssd {
		c.set("autovacuum_vacuum_cost_limit", "2000", "", "SSD: allow more vacuum I/O", "sighup")
	} else {
		c.set("autovacuum_vacuum_cost_limit", "400", "", "HDD: throttle vacuum I/O", "sighup")
	}
	c.set("track_io_timing", "on", "", "measure I/O for pg_stat_statements and reports", "user")

	// Guardrails and logging.
	c.set("idle_in_transaction_session_timeout", "10min", "", "drop sessions that idle in a transaction", "user")
	c.set("log_min_duration_statement", profileDuration(in.Profile), "", profileReason(in.Profile, "slow-statement logging"), "user")

	c.res.Settings = c.items
}

// HugePages estimates the huge pages needed for sharedBuffersMiB (§12.3).
func HugePages(sharedBuffersMiB int) int {
	if sharedBuffersMiB <= 0 {
		return 0
	}
	return (sharedBuffersMiB*105+199)/200 + 64
}

func parallelGather(profile string, cpu int) int {
	switch profile {
	case ProfileOLTP:
		return minInt(2, cpu/2)
	case ProfileAnalytics:
		return minInt(8, cpu/2)
	default:
		return minInt(4, cpu/2)
	}
}

func profileDuration(profile string) string {
	switch profile {
	case ProfileOLTP:
		return "500ms"
	case ProfileAnalytics:
		return "5s"
	default:
		return "1s"
	}
}

func hugePagesReason(ram int) string {
	if ram >= HugePagesMinMiB {
		return "try: the cluster's memory is large enough to reserve huge pages"
	}
	return fmt.Sprintf("off: the cluster has under %d MiB", HugePagesMinMiB)
}

func jitReason(analytics bool) string {
	if analytics {
		return "analytics: JIT pays off on long scans"
	}
	return "off: JIT costs more than it saves on short queries"
}

func profileReason(profile, what string) string {
	return profile + " profile " + what
}

func percentReason(ram, value int) string {
	if ram <= 0 {
		return "20%"
	}
	return fmt.Sprintf("%d%%", value*100/ram)
}

func gib(v float64) string {
	return fmt.Sprintf("%.0fGB", v)
}

func itoa(v int) string { return fmt.Sprintf("%d", v) }

func clampInt(v, lo, hi int) int { return minInt(maxInt(v, lo), hi) }

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
