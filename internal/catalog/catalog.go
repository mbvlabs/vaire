// Package catalog is the curated PostgreSQL extension list (§17). Phase 2
// carries the contrib-only entries; extensions that need a package install
// arrive with eru ext in Phase 7.
package catalog

// Repos a catalog entry may come from.
const (
	RepoContrib = "contrib"
	RepoPGDG    = "pgdg"
)

// Extension is one catalog entry.
type Extension struct {
	Name       string            `json:"name"`
	ExtName    string            `json:"ext_name"` // CREATE EXTENSION name; "" when it is preload-only
	Package    string            `json:"package,omitempty"`
	Repo       string            `json:"repo,omitempty"`
	Preload    string            `json:"preload,omitempty"`
	Settings   map[string]string `json:"settings,omitempty"`
	Restart    bool              `json:"restart"`
	PerCluster bool              `json:"per_cluster,omitempty"`
	Default    bool              `json:"default,omitempty"`
	Note       string            `json:"note,omitempty"`
}

// entries is the Phase 2 catalogue (D9). Package is empty: contrib ships with
// postgresql-contrib, installed by step 8.
var entries = []Extension{
	{
		Name: "pg_stat_statements", ExtName: "pg_stat_statements", Repo: RepoContrib, Preload: "pg_stat_statements",
		Settings: map[string]string{"pg_stat_statements.max": "10000", "pg_stat_statements.track": "top"},
		Restart:  true, Default: true,
	},
	{
		Name: "auto_explain", Repo: RepoContrib, Preload: "auto_explain",
		Restart: true, Default: true,
		Note: "not a CREATE EXTENSION; configured through auto_explain.* settings",
	},
	{Name: "pg_trgm", ExtName: "pg_trgm", Repo: RepoContrib},
	{Name: "citext", ExtName: "citext", Repo: RepoContrib},
	{Name: "btree_gin", ExtName: "btree_gin", Repo: RepoContrib},
	{Name: "btree_gist", ExtName: "btree_gist", Repo: RepoContrib},
	{Name: "pgstattuple", ExtName: "pgstattuple", Repo: RepoContrib},
	{Name: "pg_buffercache", ExtName: "pg_buffercache", Repo: RepoContrib},
	{Name: "amcheck", ExtName: "amcheck", Repo: RepoContrib},
}

// List returns every catalog entry in catalogue order.
func List() []Extension { return append([]Extension(nil), entries...) }

// Lookup returns the entry called name.
func Lookup(name string) (Extension, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	return Extension{}, false
}

// Defaults returns the names of the entries enabled on init.
func Defaults() []string {
	var out []string
	for _, e := range entries {
		if e.Default {
			out = append(out, e.Name)
		}
	}
	return out
}

// Preloads returns the shared_preload_libraries values for the enabled
// extensions. The default extensions are always preloaded.
func Preloads(enabled []string) []string {
	want := map[string]bool{}
	for _, name := range enabled {
		want[name] = true
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Preload == "" || (!e.Default && !want[e.Name]) || seen[e.Preload] {
			continue
		}
		seen[e.Preload] = true
		out = append(out, e.Preload)
	}
	return out
}
