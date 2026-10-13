package config

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/pelletier/go-toml/v2"
)

// ClusterPath is the desired state on the central command.
const ClusterPath = "/etc/eru/cluster.toml"

// ClusterSchema is the cluster.toml format version.
const ClusterSchema = 1

// Addressing defaults (§7.1). Nodes use 10.42.0.0/24, operators 10.42.255.0/24.
const (
	DefaultSubnet          = "10.42.0.0/16"
	DefaultListenPort      = 51820
	DefaultCentralWG       = "10.42.0.1"
	DefaultFirstOperatorWG = "10.42.255.1"
	DefaultAdminUser       = "eru"
	DefaultSwapMB          = 1024
	DefaultTimezone        = "UTC"
	DefaultDataRoot        = "/var/lib/postgresql/18"
	DefaultNodeName        = "db1"
	RoleMaster             = "master"
)

// Scope selects how much of cluster.toml ValidateFor checks. A Phase 1 host
// bootstrap allows the fields that only matter once the database runs.
type Scope string

// Validation scopes.
const (
	ScopeHost Scope = "host"
	ScopeFull Scope = "full"
)

// Cluster is cluster.toml (§6.1), the full schema. Every table has defaults so
// a Phase 1 file still decodes; unknown keys are rejected (D8).
type Cluster struct {
	Schema      int           `toml:"schema"`
	ClusterName string        `toml:"name"`
	Domain      string        `toml:"domain,omitempty"`
	ACMEEmail   string        `toml:"acme_email,omitempty"`
	Timezone    string        `toml:"timezone"`
	TLS         TLS           `toml:"tls"`
	Transport   Transport     `toml:"transport"`
	Nodes       []NodeSpec    `toml:"nodes"`
	Server      Server        `toml:"server"`
	Postgres    Postgres      `toml:"postgres"`
	Network     Network       `toml:"network"`
	Clusters    []ClusterSpec `toml:"clusters"`
	Backup      Backup        `toml:"backup"`
	// MasterBackup is decoded and validated now; the master backup that
	// uses it arrives in Phase 7.
	MasterBackup MasterBackup `toml:"master_backup"`
	Maintenance  Maintenance  `toml:"maintenance"`
	Metrics      Metrics      `toml:"metrics"`
}

// TLS is [tls].
type TLS struct {
	Mode   string `toml:"mode"`
	ACMECA string `toml:"acme_ca,omitempty"`
}

// Transport is [transport].
type Transport struct {
	Kind       string `toml:"kind"`
	Subnet     string `toml:"subnet"`
	ListenPort int    `toml:"listen_port"`
}

// NodeSpec is one [[nodes]] entry.
type NodeSpec struct {
	Name          string `toml:"name"`
	Role          string `toml:"role"`
	PublicAddress string `toml:"public_address"`
	WGAddress     string `toml:"wg_address"`
}

// Server is [server]. DataRoot holds one directory per PostgreSQL cluster.
// MemoryReserve is what the OS, the poolers, nginx and eru keep; empty means
// max(512MiB, 25% of RAM).
type Server struct {
	AdminUser     string `toml:"admin_user"`
	SwapMB        int    `toml:"swap_mb"`
	DataRoot      string `toml:"data_root"`
	MemoryReserve string `toml:"memory_reserve"`
}

// DataDirectory is cluster's data directory, <data_root>/<cluster>.
func (s Server) DataDirectory(cluster string) string {
	return path.Join(s.DataRoot, cluster)
}

// Postgres is [postgres]: the setup-wide defaults for every cluster, with
// the [postgres.settings] override table. ClientConnections is the
// transaction pooler's max_client_conn, SessionClientConnections the
// session pooler's.
type Postgres struct {
	Version                  int               `toml:"version"`
	Profile                  string            `toml:"profile"`
	ClientConnections        int               `toml:"client_connections"`
	SessionClientConnections int               `toml:"session_client_connections"`
	LocaleProvider           string            `toml:"locale_provider"`
	Locale                   string            `toml:"locale"`
	DefaultSize              string            `toml:"default_size"`
	Settings                 map[string]string `toml:"settings,omitempty"`
}

// Network is [network].
type Network struct {
	ClientAllowlist []string `toml:"client_allowlist"`
	ExposePgbouncer bool     `toml:"expose_pgbouncer"`
	ExposePostgres  bool     `toml:"expose_postgres"`
}

// MasterBackup is [master_backup] (RFC 6.5): the retention of the master's
// state bundles. The bucket and prefix come from [backup].
type MasterBackup struct {
	RetentionDays  int `toml:"retention_days"`
	RetentionWeeks int `toml:"retention_weeks"`
	LocalKeep      int `toml:"local_keep"`
}

// Backup is [backup].
type Backup struct {
	Provider string         `toml:"provider"`
	Endpoint string         `toml:"endpoint"`
	Region   string         `toml:"region"`
	Bucket   string         `toml:"bucket"`
	Prefix   string         `toml:"prefix"`
	URIStyle string         `toml:"uri_style"`
	Database BackupDatabase `toml:"database"`
	VPS      BackupVPS      `toml:"vps"`
}

// BackupDatabase is [backup.database].
type BackupDatabase struct {
	FullSchedule   string `toml:"full_schedule"`
	DiffSchedule   string `toml:"diff_schedule"`
	IncrSchedule   string `toml:"incr_schedule"`
	RetentionFull  int    `toml:"retention_full"`
	LocalRepo      bool   `toml:"local_repo"`
	VerifySchedule string `toml:"verify_schedule"`
	DrillSchedule  string `toml:"drill_schedule"`
}

// BackupVPS is [backup.vps].
type BackupVPS struct {
	Enabled       bool     `toml:"enabled"`
	Schedule      string   `toml:"schedule"`
	KeepWeekly    int      `toml:"keep_weekly"`
	KeepMonthly   int      `toml:"keep_monthly"`
	CheckSchedule string   `toml:"check_schedule"`
	ExtraPaths    []string `toml:"extra_paths"`
}

// Maintenance is [maintenance].
type Maintenance struct {
	Window               string  `toml:"window"`
	AutoRepack           bool    `toml:"auto_repack"`
	RepackBloatThreshold float64 `toml:"repack_bloat_threshold"`
}

// Metrics is [metrics].
type Metrics struct {
	Enabled    bool   `toml:"enabled"`
	PathPrefix string `toml:"path_prefix"`
}

// Name returns the cluster name.
func (c Cluster) Name() string { return c.ClusterName }

// DefaultCluster returns a cluster.toml with every default set (§6.1). It has
// no databases: init fills them from the interview.
func DefaultCluster() Cluster {
	return Cluster{
		Schema:    ClusterSchema,
		Timezone:  DefaultTimezone,
		TLS:       TLS{Mode: "acme"},
		Transport: Transport{Kind: "wireguard", Subnet: DefaultSubnet, ListenPort: DefaultListenPort},
		Server:    Server{AdminUser: DefaultAdminUser, SwapMB: DefaultSwapMB, DataRoot: DefaultDataRoot},
		Postgres: Postgres{
			Version:                  18,
			Profile:                  "oltp",
			ClientConnections:        1000,
			SessionClientConnections: 100,
			LocaleProvider:           "builtin",
			Locale:                   "C.UTF-8",
			DefaultSize:              DefaultSize,
			Settings:                 map[string]string{},
		},
		Network: Network{ClientAllowlist: []string{}, ExposePgbouncer: true},
		Backup: Backup{
			Database: BackupDatabase{
				FullSchedule:   "Sun *-*-* 01:00:00",
				DiffSchedule:   "Mon..Sat *-*-* 01:00:00",
				IncrSchedule:   "",
				RetentionFull:  4,
				VerifySchedule: "Wed *-*-* 04:00:00",
				DrillSchedule:  "*-*-01 05:00:00",
			},
			VPS: BackupVPS{
				Enabled:       true,
				Schedule:      "Sun *-*-* 03:30:00",
				KeepWeekly:    8,
				KeepMonthly:   6,
				CheckSchedule: "*-*-15 04:30:00",
				ExtraPaths:    []string{},
			},
		},
		MasterBackup: MasterBackup{RetentionDays: 30, RetentionWeeks: 52, LocalKeep: 10},
		Maintenance:  Maintenance{Window: "02:00-05:00", RepackBloatThreshold: 0.40},
		Metrics:      Metrics{Enabled: true, PathPrefix: "/metrics"},
	}
}

// Master returns the node with role master: the first server, which holds
// the desired state.
func (c Cluster) Master() (NodeSpec, bool) {
	for _, n := range c.Nodes {
		if n.Role == RoleMaster {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// Hostname is a node's one hostname, <node>.<domain>. It is empty until the
// domain is set.
func (c Cluster) Hostname(node string) string {
	if c.Domain == "" {
		return ""
	}
	return node + "." + c.Domain
}

// MasterHostname is the master's hostname: what clients, the certificate
// and the operator's context name.
func (c Cluster) MasterHostname() string {
	n, ok := c.Master()
	if !ok {
		return ""
	}
	return c.Hostname(n.Name)
}

// Node returns the node called name.
func (c Cluster) Node(name string) (NodeSpec, bool) {
	for _, n := range c.Nodes {
		if n.Name == name {
			return n, true
		}
	}
	return NodeSpec{}, false
}

// WGPrefixLen is the prefix length of [transport] subnet, used for the wg0
// interface address.
func (c Cluster) WGPrefixLen() int {
	p, err := netip.ParsePrefix(c.Transport.Subnet)
	if err != nil {
		return 16
	}
	return p.Bits()
}

// LoadCluster reads path. A missing file returns cluster_not_initialized. A
// file that does not parse or validate returns config_error.
func LoadCluster(path string) (Cluster, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed cluster config path.
	if err != nil {
		if IsNotExist(err) {
			return Cluster{}, output.NewError(
				output.CodeClusterNotInitialized,
				"this node has no cluster.toml",
				output.ExitContext,
				"Initialise the cluster first with eru server init.",
			).WithDetails(map[string]any{"path": path})
		}
		return Cluster{}, configError(path, err)
	}
	cfg, err := ParseCluster(raw)
	if err != nil {
		return Cluster{}, configError(path, err)
	}
	if err := cfg.Validate(); err != nil {
		return Cluster{}, configError(path, err)
	}
	return cfg, nil
}

// ParseCluster decodes raw over the defaults with a strict decoder (§6.1,
// D8). An unknown key is reported by name.
func ParseCluster(raw []byte) (Cluster, error) {
	cfg := DefaultCluster()
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		var strict *toml.StrictMissingError
		if errors.As(err, &strict) {
			return Cluster{}, unknownKeys(strict)
		}
		return Cluster{}, err
	}
	if cfg.Network.ClientAllowlist == nil {
		cfg.Network.ClientAllowlist = []string{}
	}
	if cfg.Postgres.Settings == nil {
		cfg.Postgres.Settings = map[string]string{}
	}
	if cfg.Backup.VPS.ExtraPaths == nil {
		cfg.Backup.VPS.ExtraPaths = []string{}
	}
	return cfg, nil
}

// unknownKeys turns a strict-decode failure into a message naming the keys.
func unknownKeys(strict *toml.StrictMissingError) error {
	keys := make([]string, 0, len(strict.Errors))
	for _, e := range strict.Errors {
		keys = append(keys, strings.Join(e.Key(), "."))
	}
	return fmt.Errorf("unknown keys: %s", strings.Join(keys, ", "))
}

// ParseAnswers decodes an answers file (§6.1): cluster.toml plus the
// [bootstrap] and [backup.credentials] tables, which are not part of the
// desired state and are stripped before the strict decode.
func ParseAnswers(raw []byte) (Cluster, error) {
	return ParseCluster(stripTables(raw, "bootstrap", "backup.credentials"))
}

// stripTables removes the named TOML tables and their keys from raw. Each
// table runs from its "[name]" header to the next line starting with "[".
func stripTables(raw []byte, names ...string) []byte {
	drop := map[string]bool{}
	for _, name := range names {
		drop["["+name+"]"] = true
	}
	lines := strings.Split(string(raw), "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			header := trimmed
			if idx := strings.Index(header, "]"); idx >= 0 {
				header = header[:idx+1]
			}
			skipping = drop[header]
		}
		if skipping {
			continue
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

// Encode returns cluster.toml bytes.
func (c Cluster) Encode() ([]byte, error) {
	raw, err := toml.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte("# managed by eru: change it with eru config set or eru config edit\n"), raw...), nil
}

var (
	// NamePattern is the cluster name format; it is also the pgBackRest stanza.
	NamePattern     = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)
	NodeNamePattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	userPattern     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	timezonePattern = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)
	fqdnPattern     = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
	// DatabasePattern is the database name format.
	DatabasePattern = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	localePattern   = regexp.MustCompile(`^[A-Za-z0-9_.@-]+$`)
	// SettingKeyPattern is the [postgres.settings] key format.
	SettingKeyPattern = regexp.MustCompile(`^[a-z_][a-z0-9_.]*$`)
	durationPattern   = regexp.MustCompile(`^(\d+(ms|s|min|h)|0)$`)
	// schedulePattern is a coarse operator-side filter for OnCalendar
	// expressions: printable, no quotes or shell metacharacters.
	schedulePattern = regexp.MustCompile(`^[A-Za-z0-9 *:,./~+-]{1,120}$`)
)

// reservedSettings are the settings eru owns; a user override is rejected.
var reservedSettings = []string{
	"allow_alter_system", "listen_addresses", "port", "password_encryption",
	"shared_preload_libraries", "data_directory", "hba_file", "ident_file",
}

// reservedSettingPrefixes are the setting families eru owns.
var reservedSettingPrefixes = []string{"ssl", "archive_", "include"}

// isReservedSetting reports whether key is one eru writes itself.
func isReservedSetting(key string) bool {
	for _, name := range reservedSettings {
		if key == name {
			return true
		}
	}
	for _, prefix := range reservedSettingPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

// Validate checks the full schema (scope full).
func (c Cluster) Validate() error { return c.ValidateFor(ScopeFull) }

// ValidateFor checks cluster.toml for scope. Scope host skips the fields that
// only matter once the database runs, so a Phase 1 file still validates.
func (c Cluster) ValidateFor(scope Scope) error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if c.Schema != ClusterSchema {
		add("schema must be %d", ClusterSchema)
	}
	if !NamePattern.MatchString(c.ClusterName) {
		add("name %q must match %s", c.ClusterName, NamePattern)
	}
	if c.Domain != "" {
		if err := CheckDomain(c.Domain); err != nil {
			add("domain %q %v", c.Domain, err)
		}
	}
	if c.ACMEEmail != "" {
		if err := CheckACMEEmail(c.ACMEEmail); err != nil {
			add("acme_email %q %v", c.ACMEEmail, err)
		}
	}
	if err := CheckTimezone(c.Timezone); err != nil {
		add("timezone %q %v", c.Timezone, err)
	}
	if c.Transport.Kind != "wireguard" {
		add("transport.kind must be wireguard until Phase 10")
	}
	subnet, err := netip.ParsePrefix(c.Transport.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		add("transport.subnet %q is not an IPv4 prefix", c.Transport.Subnet)
	}
	if c.Transport.ListenPort < 1 || c.Transport.ListenPort > 65535 {
		add("transport.listen_port must be 1-65535")
	}
	if err := CheckAdminUser(c.Server.AdminUser); err != nil {
		add("server.admin_user %q %v", c.Server.AdminUser, err)
	}
	if c.Server.SwapMB < 0 || c.Server.SwapMB > 65536 {
		add("server.swap_mb must be 0-65536")
	}
	if err := CheckDataRoot(c.Server.DataRoot); err != nil {
		add("server.data_root %v", err)
	}
	for _, cidr := range c.Network.ClientAllowlist {
		if err := CheckAllowlistEntry(cidr); err != nil {
			add("network.client_allowlist entry %q %v", cidr, err)
		}
	}
	masters := 0
	names := map[string]bool{}
	for i, n := range c.Nodes {
		if !NodeNamePattern.MatchString(n.Name) {
			add("nodes[%d].name %q must match %s", i, n.Name, NodeNamePattern)
		}
		if names[n.Name] {
			add("nodes[%d].name %q is a duplicate", i, n.Name)
		}
		names[n.Name] = true
		if n.Role == RoleMaster {
			masters++
		} else {
			add("nodes[%d].role %q: only master exists until Phase 10", i, n.Role)
		}
		if _, err := netip.ParseAddr(n.PublicAddress); err != nil {
			add("nodes[%d].public_address %q is not an IP address", i, n.PublicAddress)
		}
		wg, err := netip.ParseAddr(n.WGAddress)
		if err != nil || (subnet.IsValid() && !subnet.Contains(wg)) {
			add("nodes[%d].wg_address %q is not inside %s", i, n.WGAddress, c.Transport.Subnet)
		}
	}
	if masters != 1 {
		add("exactly one node with role master is required (workers arrive in Phase 10)")
	}
	c.validatePhase2(add, scope)
	if len(problems) == 0 {
		return nil
	}
	return errors.New(strings.Join(problems, "; "))
}

// validatePhase2 checks the tables that only matter once the database runs.
func (c Cluster) validatePhase2(add func(string, ...any), scope Scope) {
	if scope == ScopeHost {
		return
	}
	if c.Domain == "" {
		add("domain is required (set it with eru config set domain <fqdn>)")
	}
	switch c.TLS.Mode {
	case "acme", "self-signed":
	default:
		add("tls.mode %q must be acme or self-signed", c.TLS.Mode)
	}
	if c.TLS.ACMECA != "" && !strings.HasPrefix(c.TLS.ACMECA, "https://") {
		add("tls.acme_ca %q must be an https:// URL", c.TLS.ACMECA)
	}
	if c.Postgres.Version != 18 {
		add("postgres.version must be 18")
	}
	if err := checkProfile(c.Postgres.Profile); err != nil {
		add("postgres.profile %q %v", c.Postgres.Profile, err)
	}
	if err := CheckClientConnections(c.Postgres.ClientConnections); err != nil {
		add("postgres.client_connections %v", err)
	}
	if err := CheckClientConnections(c.Postgres.SessionClientConnections); err != nil {
		add("postgres.session_client_connections %v", err)
	}
	if _, ok := LookupSize(c.Postgres.DefaultSize); !ok {
		add("postgres.default_size %q must be one of nano, small, medium, large, xlarge", c.Postgres.DefaultSize)
	}
	if c.Server.MemoryReserve != "" {
		if _, err := ParseMemoryMiB(c.Server.MemoryReserve); err != nil {
			add("server.memory_reserve %q %v", c.Server.MemoryReserve, err)
		}
	}
	if c.MasterBackup.RetentionDays < 1 || c.MasterBackup.RetentionWeeks < 0 || c.MasterBackup.LocalKeep < 1 {
		add("master_backup needs retention_days and local_keep of at least 1, and retention_weeks of at least 0")
	}
	switch c.Postgres.LocaleProvider {
	case "builtin", "icu", "libc":
	default:
		add("postgres.locale_provider %q must be builtin, icu or libc", c.Postgres.LocaleProvider)
	}
	if !localePattern.MatchString(c.Postgres.Locale) {
		add("postgres.locale %q is not a locale name", c.Postgres.Locale)
	}
	for key := range c.Postgres.Settings {
		if !SettingKeyPattern.MatchString(key) {
			add("postgres.settings key %q must match %s", key, SettingKeyPattern)
		}
		if isReservedSetting(key) {
			add("postgres.settings key %q is managed by eru and cannot be overridden", key)
		}
	}
	c.validateClusters(add)
	if c.Backup.Provider != "" {
		if err := CheckProvider(c.Backup.Provider); err != nil {
			add("backup.provider %q %v", c.Backup.Provider, err)
		}
		if err := CheckEndpoint(c.Backup.Endpoint); err != nil {
			add("backup.endpoint %q %v", c.Backup.Endpoint, err)
		}
		if err := CheckBucket(c.Backup.Bucket); err != nil {
			add("backup.bucket %q %v", c.Backup.Bucket, err)
		}
		if err := CheckPrefix(c.Backup.Prefix); err != nil {
			add("backup.prefix %q %v", c.Backup.Prefix, err)
		}
		if c.Backup.Region == "" {
			add("backup.region is required")
		}
		switch c.Backup.URIStyle {
		case "path", "host":
		default:
			add("backup.uri_style %q must be path or host", c.Backup.URIStyle)
		}
		if c.Backup.Database.FullSchedule == "" {
			add("backup.database.full_schedule is required")
		}
		for key, value := range c.ScheduleValues() {
			if !schedulePattern.MatchString(value) {
				add("%s %q is not a systemd OnCalendar expression", key, value)
			}
		}
		if c.Backup.Database.RetentionFull < 1 {
			add("backup.database.retention_full must be at least 1")
		}
		if c.Backup.VPS.KeepWeekly < 1 {
			add("backup.vps.keep_weekly must be at least 1")
		}
	}
}

// ScheduleValues returns every non-empty database backup schedule by its
// dotted key, for validation. The node checks each with systemd-analyze
// calendar, which has the final say.
func (c Cluster) ScheduleValues() map[string]string {
	out := map[string]string{}
	add := func(key, value string) {
		if value != "" {
			out[key] = value
		}
	}
	add("backup.database.full_schedule", c.Backup.Database.FullSchedule)
	add("backup.database.diff_schedule", c.Backup.Database.DiffSchedule)
	add("backup.database.incr_schedule", c.Backup.Database.IncrSchedule)
	add("backup.database.verify_schedule", c.Backup.Database.VerifySchedule)
	add("backup.database.drill_schedule", c.Backup.Database.DrillSchedule)
	for _, cl := range c.Clusters {
		at := "clusters." + cl.Name + ".backup."
		add(at+"full_schedule", cl.Backup.FullSchedule)
		add(at+"diff_schedule", cl.Backup.DiffSchedule)
		add(at+"incr_schedule", cl.Backup.IncrSchedule)
		add(at+"verify_schedule", cl.Backup.VerifySchedule)
		add(at+"drill_schedule", cl.Backup.DrillSchedule)
		add(at+"dump_schedule", cl.Backup.DumpSchedule)
	}
	return out
}

// BackupConfigured reports whether [backup] names a bucket.
func (c Cluster) BackupConfigured() bool { return c.Backup.Provider != "" }

// ValidationError wraps a Validate failure as validation_failed.
func ValidationError(err error) *output.CLIError {
	return output.NewError(
		output.CodeValidation,
		"cluster settings are invalid: "+err.Error(),
		output.ExitConfig,
		"Fix the values named in the message.",
	)
}
