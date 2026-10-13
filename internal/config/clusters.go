package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/mbvlabs/eru/internal/catalog"
)

// Cluster ports (RFC 8.2). A cluster listens on its Unix socket only, and
// the port names the socket. Temporary clusters (restores, forks, drills)
// take the range above.
const (
	ClusterPortMin     = 5433
	ClusterPortMax     = 5479
	TemporaryPortMin   = 5480
	TemporaryPortMax   = 5499
	stanzaSuffixLength = 6
)

// ClusterSpec is one [[clusters]] entry: one database in its own PostgreSQL
// cluster, with the same name. node, port and stanza are assigned at create;
// port and stanza never change while the cluster exists.
type ClusterSpec struct {
	Name             string            `toml:"name"`
	Node             string            `toml:"node"`
	Port             int               `toml:"port"`
	Stanza           string            `toml:"stanza"`
	Size             string            `toml:"size"`
	Memory           string            `toml:"memory"`
	MemoryMax        string            `toml:"memory_max"`
	CPUWeight        int               `toml:"cpu_weight"`
	CPUQuota         string            `toml:"cpu_quota"`
	IOWeight         int               `toml:"io_weight"`
	Profile          string            `toml:"profile,omitempty"`
	StatementTimeout string            `toml:"statement_timeout"`
	Extensions       []string          `toml:"extensions"`
	PoolSize         int               `toml:"pool_size"`
	SessionPoolSize  int               `toml:"session_pool_size"`
	Settings         map[string]string `toml:"settings,omitempty"`
	Backup           ClusterBackup     `toml:"backup,omitempty"`
}

// ClusterBackup is [clusters.backup]: the cluster's restore window and
// schedules, each defaulting to [backup.database], and its scheduled
// pg_dump archives kept longer than the window.
type ClusterBackup struct {
	FullSchedule   string `toml:"full_schedule,omitempty"`
	DiffSchedule   string `toml:"diff_schedule,omitempty"`
	IncrSchedule   string `toml:"incr_schedule,omitempty"`
	VerifySchedule string `toml:"verify_schedule,omitempty"`
	DrillSchedule  string `toml:"drill_schedule,omitempty"`
	RetentionFull  int    `toml:"retention_full,omitempty"`
	// DumpSchedule is a systemd OnCalendar expression; empty means no
	// scheduled dumps.
	DumpSchedule string `toml:"dump_schedule,omitempty"`
	// DumpRetention is how many dumps are kept.
	DumpRetention int `toml:"dump_retention,omitempty"`
}

var (
	stanzaPattern   = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}-[0-9a-f]{6}$`)
	cpuQuotaPattern = regexp.MustCompile(`^[1-9][0-9]{0,4}%$`)
)

// ClusterByName returns the [[clusters]] entry called name.
func (c Cluster) ClusterByName(name string) (ClusterSpec, bool) {
	for _, s := range c.Clusters {
		if s.Name == name {
			return s, true
		}
	}
	return ClusterSpec{}, false
}

// Resources resolves spec's limits against [postgres] default_size.
func (c Cluster) Resources(spec ClusterSpec) (Resources, error) {
	return ResolveResources(spec, c.Postgres.DefaultSize)
}

// FreeClusterPort is the lowest port in 5433-5479 no cluster holds.
func (c Cluster) FreeClusterPort() (int, error) {
	used := map[int]bool{}
	for _, s := range c.Clusters {
		used[s.Port] = true
	}
	for port := ClusterPortMin; port <= ClusterPortMax; port++ {
		if !used[port] {
			return port, nil
		}
	}
	return 0, fmt.Errorf("every cluster port from %d to %d is taken", ClusterPortMin, ClusterPortMax)
}

// NewStanza returns a stanza name for a new cluster: <name>-<6 hex>. A
// database dropped and created again gets a new one, because the old stanza
// still holds the dropped database's restore window.
func NewStanza(name string) (string, error) {
	raw := make([]byte, stanzaSuffixLength/2)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return name + "-" + hex.EncodeToString(raw), nil
}

// validateClusters checks every [[clusters]] entry.
func (c Cluster) validateClusters(add func(string, ...any)) {
	master, _ := c.Master()
	names, ports, stanzas := map[string]bool{}, map[int]string{}, map[string]bool{}
	for i, s := range c.Clusters {
		at := fmt.Sprintf("clusters[%d]", i)
		if err := CheckDatabaseName(s.Name); err != nil {
			add("%s.name %q %v", at, s.Name, err)
		}
		if names[s.Name] {
			add("%s.name %q is a duplicate", at, s.Name)
		}
		names[s.Name] = true
		if s.Node != master.Name {
			add("%s.node %q must be the master %q: workers arrive in Phase 10", at, s.Node, master.Name)
		}
		if s.Port < ClusterPortMin || s.Port > ClusterPortMax {
			add("%s.port %d must be %d-%d", at, s.Port, ClusterPortMin, ClusterPortMax)
		} else if other, taken := ports[s.Port]; taken {
			add("%s.port %d is already used by %s", at, s.Port, other)
		}
		ports[s.Port] = s.Name
		if !stanzaPattern.MatchString(s.Stanza) || !strings.HasPrefix(s.Stanza, s.Name+"-") {
			add("%s.stanza %q must be %s-<6 hex>", at, s.Stanza, s.Name)
		}
		if stanzas[s.Stanza] {
			add("%s.stanza %q is a duplicate", at, s.Stanza)
		}
		stanzas[s.Stanza] = true
		c.validateClusterResources(add, at, s)
		if s.Profile != "" {
			if err := checkProfile(s.Profile); err != nil {
				add("%s.profile %q %v", at, s.Profile, err)
			}
		}
		if !durationPattern.MatchString(s.StatementTimeout) {
			add("%s.statement_timeout %q must be a PostgreSQL duration such as 60s, 500ms or 0", at, s.StatementTimeout)
		}
		for _, ext := range s.Extensions {
			if _, ok := catalog.Lookup(ext); !ok {
				add("%s.extensions %q is not in the catalog (it arrives with eru ext in Phase 4)", at, ext)
			}
		}
		for key := range s.Settings {
			if !SettingKeyPattern.MatchString(key) {
				add("%s.settings key %q must match %s", at, key, SettingKeyPattern)
			}
			if isReservedSetting(key) {
				add("%s.settings key %q is managed by eru and cannot be overridden", at, key)
			}
		}
		c.validateClusterBackup(add, at, s.Backup)
	}
}

// validateClusterResources checks the size and limit keys and that both
// pools fit in max_connections.
func (c Cluster) validateClusterResources(add func(string, ...any), at string, s ClusterSpec) {
	if s.CPUWeight < 0 || s.CPUWeight > 10000 {
		add("%s.cpu_weight %d must be 0 (from the size) or 1-10000", at, s.CPUWeight)
	}
	if s.IOWeight < 0 || s.IOWeight > 10000 {
		add("%s.io_weight %d must be 0 (the default 100) or 1-10000", at, s.IOWeight)
	}
	if s.CPUQuota != "" && !cpuQuotaPattern.MatchString(s.CPUQuota) {
		add("%s.cpu_quota %q must be a percentage such as 150%%", at, s.CPUQuota)
	}
	if s.PoolSize < 0 {
		add("%s.pool_size must not be negative", at)
	}
	if s.SessionPoolSize < 0 {
		add("%s.session_pool_size must not be negative", at)
	}
	r, err := c.Resources(s)
	if err != nil {
		add("%s.%v", at, err)
		return
	}
	if r.MemoryMaxMiB != 0 && r.MemoryMaxMiB < r.MemoryMiB {
		add("%s.memory_max %q is below the cluster's memory (%d MiB)", at, s.MemoryMax, r.MemoryMiB)
	}
	if r.PoolSize < 1 || r.PoolSize+r.SessionPoolSize+ReservedConnections > r.MaxConnections {
		add("%s: pool_size %d + session_pool_size %d + %d reserved must fit in max_connections %d", at,
			r.PoolSize, r.SessionPoolSize, ReservedConnections, r.MaxConnections)
	}
}

// validateClusterBackup checks [clusters.backup].
func (c Cluster) validateClusterBackup(add func(string, ...any), at string, b ClusterBackup) {
	if b.RetentionFull < 0 {
		add("%s.backup.retention_full must not be negative", at)
	}
	if b.DumpSchedule != "" {
		if c.Backup.Provider == "" {
			add("%s.backup.dump_schedule needs a bucket: run eru backup setup first", at)
		}
		if b.DumpRetention < 1 {
			add("%s.backup.dump_retention must be at least 1 when dump_schedule is set", at)
		}
	}
	if b.DumpRetention < 0 {
		add("%s.backup.dump_retention must not be negative", at)
	}
}

// checkProfile checks a workload profile name.
func checkProfile(v string) error {
	switch v {
	case "oltp", "mixed", "analytics":
		return nil
	}
	return errors.New("must be oltp, mixed or analytics")
}

// RetentionFull is a cluster's restore window in weeks: its own
// backup.retention_full, or [backup.database]'s.
func (c Cluster) RetentionFull(spec ClusterSpec) int {
	if spec.Backup.RetentionFull > 0 {
		return spec.Backup.RetentionFull
	}
	return c.Backup.Database.RetentionFull
}
