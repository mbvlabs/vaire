// Package catalog holds the built-in PostgreSQL extension descriptors (§17).
// It is a convenience list, not an allowlist: configuration resolves custom
// descriptors too, and only a config-aware resolver may decide what is valid.
package catalog

import (
	"slices"
	"strings"
)

// Sources an extension may come from. A custom descriptor must select one.
const (
	SourceAPT          = "apt"
	SourcePreinstalled = "preinstalled"
)

// Copy safety declarations. Unknown is the default for custom descriptors and
// blocks a copy or follower until the operator supplies a declaration.
const (
	CopySafetyUnknown  = "unknown"
	CopySafetySafe     = "safe"
	CopySafetyDisabled = "disabled"
)

// DBPlaceholder in a setting's value is the database name inside the cluster:
// the database's own name, or the source's for a follower.
const DBPlaceholder = "{db}"

// Extension is one resolved extension descriptor. The config package owns
// custom descriptors and returns them in this shape, so every path (render,
// validation, roles, operations and recovery) sees the same metadata.
type Extension struct {
	Name    string `json:"name"`
	ExtName string `json:"ext_name,omitempty"` // CREATE EXTENSION name; "" when it is preload-only
	// Aliases are accepted spellings; cluster.toml always stores Name.
	Aliases []string `json:"aliases,omitempty"`
	// Source is apt or preinstalled. A contrib built-in is preinstalled
	// because postgresql-contrib installs it.
	Source string `json:"source"`
	// Package is the APT package for source apt; empty for preinstalled.
	Package string `json:"package,omitempty"`
	// Preload is the shared_preload_libraries value; "" when none.
	Preload string `json:"preload,omitempty"`
	// Settings go into 30-vaire-extensions.conf as written. DBPlaceholder
	// becomes the database name inside the cluster.
	Settings map[string]string `json:"settings,omitempty"`
	// Schema is the schema the extension is created in, made first; ""
	// for the default (public).
	Schema string `json:"schema,omitempty"`
	// Restart is derived: Preload != "".
	Restart bool `json:"restart"`
	// PerCluster marks an extension that lives in one database of its
	// cluster (pg_cron's cron.database_name).
	PerCluster bool `json:"per_cluster,omitempty"`
	// Default entries are always preloaded and enabled on db create.
	Default bool `json:"default,omitempty"`
	// CopySafety says whether a copy may load the extension: safe,
	// disabled (CopySettings suppress its background effects), or unknown.
	CopySafety string `json:"copy_safety"`
	// CopySettings are applied before a copy or independent cluster starts.
	CopySettings map[string]string `json:"copy_settings,omitempty"`
	// StandbySafe says the extension may load on a physical standby. It is
	// false unless the descriptor explicitly declares it.
	StandbySafe bool `json:"standby_safe"`
	// BuiltIn marks a descriptor supplied by this package rather than by
	// configuration. An operator override is still a built-in entry.
	BuiltIn bool   `json:"built_in,omitempty"`
	Note    string `json:"note,omitempty"`
}

// entries is the built-in catalogue. Packages were checked against PGDG's
// PostgreSQL 18 indexes for Debian 13 (trixie-pgdg) and Ubuntu 26.04
// (resolute-pgdg), amd64 and arm64. Metadata availability is not install or
// runtime qualification.
var entries = []Extension{
	{
		Name: "pg_stat_statements", ExtName: "pg_stat_statements", Source: SourcePreinstalled, Preload: "pg_stat_statements",
		Settings: map[string]string{"pg_stat_statements.max": "10000", "pg_stat_statements.track": "top"},
		Restart:  true, Default: true, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
	},
	{
		Name: "auto_explain", Source: SourcePreinstalled, Preload: "auto_explain",
		Restart: true, Default: true, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
		Note: "not a CREATE EXTENSION; configured through auto_explain.* settings, always on",
	},
	{Name: "pg_trgm", ExtName: "pg_trgm", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "citext", ExtName: "citext", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "btree_gin", ExtName: "btree_gin", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "btree_gist", ExtName: "btree_gist", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "pgstattuple", ExtName: "pgstattuple", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "pg_buffercache", ExtName: "pg_buffercache", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{Name: "amcheck", ExtName: "amcheck", Source: SourcePreinstalled, CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{
		Name: "vector", ExtName: "vector", Aliases: []string{"pgvector"},
		Source: SourceAPT, Package: "postgresql-18-pgvector",
		CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
	},
	{
		Name: "postgis", ExtName: "postgis",
		Source: SourceAPT, Package: "postgresql-18-postgis-3",
		CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
		Note: "a large install (GEOS, PROJ, GDAL); postgis_topology and postgis_raster come with the package and can be created by hand",
	},
	{
		Name: "pg_cron", ExtName: "pg_cron", Source: SourceAPT, Package: "postgresql-18-cron", Preload: "pg_cron",
		Settings: map[string]string{
			"cron.database_name":          "'" + DBPlaceholder + "'",
			"cron.use_background_workers": "on",
			"cron.max_running_jobs":       "3",
		},
		Restart: true, PerCluster: true, BuiltIn: true,
		CopySafety: CopySafetyDisabled,
		CopySettings: map[string]string{
			"cron.launch_active_jobs": "off",
		},
		StandbySafe: true,
		Note:        "jobs run as background workers (the clusters listen on a socket only); a copy keeps the library loaded with launches off",
	},
	{
		Name: "pgaudit", ExtName: "pgaudit", Source: SourceAPT, Package: "postgresql-18-pgaudit", Preload: "pgaudit",
		Settings:   map[string]string{"pgaudit.log": "'ddl,role'"},
		Restart:    true,
		CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
		Note: "audit lines go to the cluster's log (vaire logs postgres --db)",
	},
	{
		Name: "pg_repack", ExtName: "pg_repack", Source: SourceAPT, Package: "postgresql-18-repack",
		CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
		Note: "vaire maint repack and auto-repack need it",
	},
	{
		Name: "pg_partman", ExtName: "pg_partman", Source: SourceAPT, Package: "postgresql-18-partman",
		Schema: "partman", CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true,
		Note: "in the partman schema; its background worker is not preloaded (run partman.run_maintenance_proc() from pg_cron)",
	},
	{Name: "hypopg", ExtName: "hypopg", Source: SourceAPT, Package: "postgresql-18-hypopg", CopySafety: CopySafetySafe, StandbySafe: true, BuiltIn: true},
	{
		Name: "timescaledb", ExtName: "timescaledb", Source: SourceAPT, Package: "postgresql-18-timescaledb", Preload: "timescaledb",
		Settings:   map[string]string{"timescaledb.max_background_workers": "2"},
		Restart:    true,
		CopySafety: CopySafetyDisabled,
		CopySettings: map[string]string{
			"timescaledb.max_background_workers": "0",
		},
		StandbySafe: true, BuiltIn: true,
		Note: "PGDG's Apache-2 build: no compression, continuous aggregates or other Timescale License features",
	},
}

// Clone returns a deep copy: nested slices and maps never alias the receiver.
func (e Extension) Clone() Extension {
	out := e
	out.Aliases = slices.Clone(e.Aliases)
	out.Settings = cloneMap(e.Settings)
	out.CopySettings = cloneMap(e.CopySettings)
	return out
}

// List returns every built-in entry in catalogue order, as deep copies.
func List() []Extension {
	out := make([]Extension, len(entries))
	for i, e := range entries {
		out[i] = e.Clone()
	}
	return out
}

// Lookup returns the built-in entry called name or one of its aliases.
func Lookup(name string) (Extension, bool) {
	for _, e := range entries {
		if e.Name == name || slices.Contains(e.Aliases, name) {
			return e.Clone(), true
		}
	}
	return Extension{}, false
}

// Defaults returns the names of the built-in entries enabled on init.
func Defaults() []string {
	var out []string
	for _, e := range entries {
		if e.Default {
			out = append(out, e.Name)
		}
	}
	return out
}

// Names are the built-in names in catalogue order.
func Names() []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Name)
	}
	return out
}

// Canonical is name's built-in catalogue name: an alias (pgvector) becomes
// the entry's Name (vector). An unknown name comes back unchanged with ok
// false.
func Canonical(name string) (string, bool) {
	e, ok := Lookup(name)
	if !ok {
		return name, false
	}
	return e.Name, true
}

// CanonicalList is names with aliases rewritten to their entry's Name and
// duplicates dropped, in order. Unknown names stay, for validation to refuse.
func CanonicalList(names []string) []string {
	var out []string
	for _, n := range names {
		c, _ := Canonical(strings.TrimSpace(n))
		if c != "" && !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// ByExtName returns the built-in entry whose CREATE EXTENSION name is extname.
func ByExtName(extname string) (Extension, bool) {
	for _, e := range entries {
		if e.ExtName != "" && e.ExtName == extname {
			return e.Clone(), true
		}
	}
	return Extension{}, false
}

// Managed reports whether vaire ext acts on e: auto_explain is always on and
// has nothing to create or drop.
func (e Extension) Managed() bool { return e.ExtName != "" }

// ResolvedSettings are e's settings for database db, placeholders filled. The
// returned map is a fresh copy; the descriptor is never changed.
func (e Extension) ResolvedSettings(db string) map[string]string {
	return resolvedMap(e.Settings, db)
}

// ResolvedCopySettings are e's copy settings for database db, placeholders
// filled. The returned map is a fresh copy; the descriptor is never changed.
// A copy setting is applied only while the descriptor's copy_safety is
// disabled; callers check that separately.
func (e Extension) ResolvedCopySettings(db string) map[string]string {
	return resolvedMap(e.CopySettings, db)
}

func resolvedMap(settings map[string]string, db string) map[string]string {
	if len(settings) == 0 {
		return nil
	}
	out := make(map[string]string, len(settings))
	for k, v := range settings {
		out[k] = strings.ReplaceAll(v, DBPlaceholder, db)
	}
	return out
}

// NormalizedSettingValue removes one PostgreSQL string-literal quoting layer
// for comparing a declared value with pg_settings' canonical value: on and
// 'on' are the same effective setting.
func NormalizedSettingValue(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

// SemanticIdentity is the runtime intent of one descriptor: install source,
// SQL name, schema, preload, settings and copy/standby safety. It deliberately
// excludes human notes, aliases and display-only flags. Plan and step inputs
// can serialize it without treating a note edit as a runtime change.
type SemanticIdentity struct {
	Name         string            `json:"name"`
	ExtName      string            `json:"ext_name,omitempty"`
	Source       string            `json:"source"`
	Package      string            `json:"package,omitempty"`
	Preload      string            `json:"preload,omitempty"`
	Schema       string            `json:"schema,omitempty"`
	Settings     map[string]string `json:"settings,omitempty"`
	CopySafety   string            `json:"copy_safety"`
	CopySettings map[string]string `json:"copy_settings,omitempty"`
	StandbySafe  bool              `json:"standby_safe"`
}

// SemanticIdentity returns a fresh identity value. Nil and empty maps are
// normalised to nil so an absent table and an empty table do not churn.
func (e Extension) SemanticIdentity() SemanticIdentity {
	return SemanticIdentity{
		Name:         e.Name,
		ExtName:      e.ExtName,
		Source:       e.Source,
		Package:      e.Package,
		Preload:      e.Preload,
		Schema:       e.Schema,
		Settings:     semanticMap(e.Settings),
		CopySafety:   e.CopySafety,
		CopySettings: semanticMap(e.CopySettings),
		StandbySafe:  e.StandbySafe,
	}
}

// Equal compares two semantic identities. Map order and nil/empty maps do not
// matter.
func (s SemanticIdentity) Equal(other SemanticIdentity) bool {
	return s.Name == other.Name &&
		s.ExtName == other.ExtName &&
		s.Source == other.Source &&
		s.Package == other.Package &&
		s.Schema == other.Schema &&
		s.Preload == other.Preload &&
		equalMap(s.Settings, other.Settings) &&
		s.CopySafety == other.CopySafety &&
		equalMap(s.CopySettings, other.CopySettings) &&
		s.StandbySafe == other.StandbySafe
}

// SemanticEqual compares the runtime intent of two descriptors. Human notes,
// aliases and derived display flags do not change runtime intent.
func (e Extension) SemanticEqual(other Extension) bool {
	return e.SemanticIdentity().Equal(other.SemanticIdentity())
}

func equalMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// semanticMap normalises an empty map to nil and returns a fresh copy.
func semanticMap(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	return cloneMap(m)
}
