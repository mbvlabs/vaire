// Package doctor holds eru doctor's checks (Phase 4). The node gathers Facts
// (files, systemd, PostgreSQL, the records); every check is a pure function
// of them, so thresholds and exemptions are testable without a server.
package doctor

import (
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/health"
)

// Status is a check's outcome.
type Status string

// Statuses. skip is a check that doesn't apply (no bucket for the archiving
// check, metrics off, a database too young for its first full).
const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Categories, for --only. security is the CIS-style subset across them.
const (
	CatHost     = "host"
	CatPostgres = "postgres"
	CatBackups  = "backups"
	CatTLS      = "tls"
	CatMetrics  = "metrics"
	CatSecurity = "security"
)

// Categories lists the --only values.
var Categories = []string{CatSecurity, CatBackups, CatHost, CatPostgres, CatTLS, CatMetrics}

// Result is one check's outcome, for one database when DB is set.
type Result struct {
	ID       string `json:"id"`
	DB       string `json:"db,omitempty"`
	Status   Status `json:"status"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
	Category string `json:"category"`
	Security bool   `json:"security,omitempty"`
}

// Check is one doctor check.
type Check struct {
	ID       string
	Category string
	// Security puts the check in --only security.
	Security bool
	// AtInit is false for a check that can't pass during server init (the
	// secrets aren't confirmed yet, no backup has run): the init gate skips
	// it (Phase 4 D10).
	AtInit bool
	Run    func(f Facts) []Result
}

// Facts is everything the checks read, gathered by the node.
type Facts struct {
	Now time.Time
	// Init is set by the server init gate.
	Init bool

	OSSupported    bool
	OSName         string
	RebootRequired bool
	DiskTotal      uint64
	DiskFree       uint64
	DiskKnown      bool
	DataRoot       string
	// DiskPath is the path whose filesystem was measured: the data root,
	// or its nearest existing parent while the data root doesn't exist yet
	// (no database has been created, or the last one was dropped).
	DiskPath string
	// UnattendedActive is unattended-upgrades' unit state.
	UnattendedActive bool
	TimeSynced       bool
	TimeKnown        bool

	LockedDownAt    string
	LockdownPending bool
	BreakGlassOpen  bool
	// SSHConfigDrift, FirewallDrift: the step's check detail, empty when
	// the host matches.
	SSHConfigDrift string
	UFWActive      bool
	FirewallDrift  string
	// OpenAllowlists are 0.0.0.0/0 or ::/0 entries, by setting.
	OpenAllowlists []string

	TLSMode       string
	CertNotAfter  time.Time
	CertKnown     bool
	CertSelfSign  bool
	DNSChecked    bool
	DNSMatches    bool
	DNSDetail     string
	Hostname      string
	SecretsAtTime string

	MetricsEnabled bool
	// Exporters is each exporter's error, nil when it answers.
	Exporters map[string]error
	// EndpointStatus is nginx's answer to /metrics/node without a
	// credential (401 expected), 0 when unreachable.
	EndpointStatus int
	EndpointError  string
	// PGProbes is each database's postgres_exporter probe, by name, and
	// PGExporterLog postgres_exporter's last errors when one failed.
	PGProbes      map[string]PGProbe
	PGExporterLog []string

	NginxActive   bool
	PoolersActive map[string]bool
	HasDatabases  bool

	BackupConfigured bool
	BudgetWarnings   []Warning
	Records          backup.Records
	Jobs             []engine.JobRecord
	Clusters         []ClusterFacts
	// Master is the master backup (Phase 5).
	Master MasterFacts
}

// Warning is an oversubscription warning (memory_overcommitted, cpu_crowded,
// disk_crowded).
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// ClusterFacts is one database's cluster.
type ClusterFacts struct {
	Name   string
	Stanza string
	// State is running, stopped or unreachable.
	State  string
	Error  string
	Sample health.Sample
	// ConfigDrift is postgres-config-v1's check detail, empty when the
	// rendered configuration is on disk.
	ConfigDrift string
	// Overrides are the settings 90-overrides.conf sets.
	Overrides []string
	// HBAWeak are pg_hba lines with trust, password or md5.
	HBAWeak []string
	// Memory pressure (PSI some avg300, percent) and memory.events high now
	// and at the last doctor run.
	PressureKnown   bool
	PressureAvg300  float64
	HighEvents      int64
	PrevHighEvents  int64
	PrevKnown       bool
	WouldNotStart   string
	CreatedAt       time.Time
	FullInterval    time.Duration
	FullIntervalErr string
	// AwaitingRecovery is a database a rebuilt master hasn't recovered yet
	// (Phase 5): its PostgreSQL and backup checks are skipped.
	AwaitingRecovery bool
}

// Thresholds.
const (
	DiskWarnFreePct = 20
	DiskFailFreePct = 10
	TLSWarnDays     = 21
	TLSFailDays     = 7
	WraparoundWarn  = 1_000_000_000
	WraparoundFail  = 1_600_000_000
	ConnectionsWarn = 0.8
	// PressureWarnPct is memory.pressure "some avg300" above which a cluster
	// is said to be starved of memory.
	PressureWarnPct  = 10.0
	VerifyMaxAge     = 14 * 24 * time.Hour
	DrillMaxAge      = 40 * 24 * time.Hour
	SkipsInARowWarns = 3
)

// Filter picks the checks for --only (empty is every check).
func Filter(checks []Check, only string) []Check {
	if only == "" {
		return checks
	}
	var out []Check
	for _, c := range checks {
		if c.Category == only || (only == CatSecurity && c.Security) {
			out = append(out, c)
		}
	}
	return out
}

// Run runs checks against f. With f.Init, checks that can't pass yet are
// left out. db narrows per-database results.
func Run(checks []Check, f Facts, db string) []Result {
	var out []Result
	for _, c := range checks {
		if f.Init && !c.AtInit {
			continue
		}
		for _, r := range c.Run(f) {
			if db != "" && r.DB != "" && r.DB != db {
				continue
			}
			r.ID, r.Category, r.Security = c.ID, c.Category, c.Security
			out = append(out, r)
		}
	}
	return out
}

// Summary counts results by status.
func Summary(results []Result) map[Status]int {
	out := map[Status]int{Pass: 0, Warn: 0, Fail: 0, Skip: 0}
	for _, r := range results {
		out[r.Status]++
	}
	return out
}

// Failed returns the failing results.
func Failed(results []Result) []Result {
	var out []Result
	for _, r := range results {
		if r.Status == Fail {
			out = append(out, r)
		}
	}
	return out
}

// ValidOnly reports whether only is a known --only value.
func ValidOnly(only string) bool { return only == "" || slices.Contains(Categories, only) }

// one is a single result.
func one(status Status, detail, fix string) []Result {
	return []Result{{Status: status, Detail: detail, Fix: fix}}
}

// joinSorted joins a sorted copy of items.
func joinSorted(items []string) string {
	out := append([]string(nil), items...)
	sort.Strings(out)
	return strings.Join(out, ", ")
}
