package doctor

import (
	"strings"
	"testing"
)

func extensionFacts() ExtensionFacts {
	return ExtensionFacts{
		Checked:            true,
		Enabled:            []ExtensionInfo{{Name: "pg_stat_statements", ExtName: "pg_stat_statements", Preload: "pg_stat_statements"}, {Name: "vector", ExtName: "vector"}},
		Known:              map[string]ExtensionInfo{"pg_stat_statements": {Name: "pg_stat_statements", ExtName: "pg_stat_statements", Preload: "pg_stat_statements"}, "vector": {Name: "vector", ExtName: "vector"}},
		PackagesKnown:      true,
		PrerequisitesKnown: true,
		Available:          true,
		Installed: map[string]ExtensionVersion{
			"pg_stat_statements": {Version: "1.12", Default: "1.12"},
			"vector":             {Version: "0.9.0", Default: "0.9.0"},
		},
		LoadedKnown:  true,
		Loaded:       map[string]bool{"pg_stat_statements": true, "auto_explain": true},
		PendingKnown: true,
	}
}

func extensionFixture(x ExtensionFacts) Facts {
	f := healthy()
	f.Clusters = f.Clusters[:1]
	f.Clusters[0].Ext = x
	return f
}

func TestExtensionChecksHealthy(t *testing.T) {
	for _, r := range Run(ExtensionChecks(), extensionFixture(extensionFacts()), "") {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Detail)
		}
	}
}

func TestExtensionChecksUnknownFactsSkip(t *testing.T) {
	for _, r := range Run(ExtensionChecks(), extensionFixture(ExtensionFacts{}), "") {
		if r.Status != Skip {
			t.Errorf("%s: %s %s, want skip", r.ID, r.Status, r.Detail)
		}
	}
}

func TestExtensionChecksProblems(t *testing.T) {
	cases := []struct {
		name   string
		edit   func(*ExtensionFacts)
		id     string
		status Status
		detail string
		fix    string
	}{
		{
			name: "missing package",
			edit: func(x *ExtensionFacts) {
				x.MissingPackages = []string{"postgresql-18-pgvector"}
			},
			id: "ext.packages", status: Fail, detail: "postgresql-18-pgvector", fix: "vaire apply",
		},
		{
			name: "missing prerequisite",
			edit: func(x *ExtensionFacts) {
				x.MissingPrerequisites = "preinstalled extension \"custom\" is missing its control file"
			},
			id: "ext.packages", status: Fail, detail: "preinstalled extension", fix: "vaire apply",
		},
		{
			name: "package state unknown",
			edit: func(x *ExtensionFacts) {
				x.PackagesKnown, x.PackageError = false, "dpkg-query failed"
			},
			id: "ext.packages", status: Warn, detail: "dpkg-query failed", fix: "vaire apply",
		},
		{
			name: "pending restart is not missing",
			edit: func(x *ExtensionFacts) {
				x.Enabled = []ExtensionInfo{{Name: "pg_cron", ExtName: "pg_cron", Preload: "pg_cron"}}
				x.Known["pg_cron"] = ExtensionInfo{Name: "pg_cron", ExtName: "pg_cron", Preload: "pg_cron"}
				x.Installed = map[string]ExtensionVersion{"pg_cron": {Version: "1.6", Default: "1.6"}}
				x.Loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
				x.PendingKnown, x.PendingRestart = true, true
			},
			id: "ext.state", status: Warn, detail: "wait for the restart", fix: "--confirm app/restart",
		},
		{
			name: "missing SQL",
			edit: func(x *ExtensionFacts) {
				x.Enabled = []ExtensionInfo{{Name: "vector", ExtName: "vector"}}
				x.Installed = map[string]ExtensionVersion{}
			},
			id: "ext.state", status: Warn, detail: "doesn't have", fix: "vaire apply --db app",
		},
		{
			name: "installed but not enabled",
			edit: func(x *ExtensionFacts) {
				x.Enabled = []ExtensionInfo{{Name: "pg_stat_statements", ExtName: "pg_stat_statements", Preload: "pg_stat_statements"}}
				x.Known["timescaledb"] = ExtensionInfo{Name: "timescaledb", ExtName: "timescaledb", Preload: "timescaledb"}
				x.Installed = map[string]ExtensionVersion{
					"pg_stat_statements": {Version: "1.12", Default: "1.12"},
					"timescaledb":        {Version: "2.31.0", Default: "2.31.0"},
				}
			},
			id: "ext.state", status: Warn, detail: "doesn't enable", fix: "vaire ext add timescaledb --db app",
		},
		{
			name: "installed SQL not enabled",
			edit: func(x *ExtensionFacts) {
				x.Enabled = []ExtensionInfo{{Name: "pg_stat_statements", ExtName: "pg_stat_statements", Preload: "pg_stat_statements"}}
				x.Known["pg_trgm"] = ExtensionInfo{Name: "pg_trgm", ExtName: "pg_trgm"}
				x.Installed = map[string]ExtensionVersion{
					"pg_stat_statements": {Version: "1.12", Default: "1.12"},
					"pg_trgm":            {Version: "1.6", Default: "1.6"},
				}
			},
			id: "ext.state", status: Warn, detail: "doesn't enable", fix: "vaire ext add pg_trgm --db app",
		},
		{
			name: "unmanaged",
			edit: func(x *ExtensionFacts) {
				x.Installed["mystery"] = ExtensionVersion{Version: "2.0", Default: "2.0"}
			},
			id: "ext.state", status: Warn, detail: "unmanaged extensions", fix: "vaire ext list --db app",
		},
		{
			name: "unavailable is unknown",
			edit: func(x *ExtensionFacts) {
				x.Available, x.Error = false, "connection refused"
			},
			id: "ext.state", status: Warn, detail: "cannot read its extensions", fix: "vaire status --db app",
		},
		{
			name: "update",
			edit: func(x *ExtensionFacts) {
				x.Installed["vector"] = ExtensionVersion{Version: "0.8.0", Default: "0.9.0"}
			},
			id: "ext.updates", status: Warn, detail: "0.8.0 -> 0.9.0", fix: "vaire ext add vector --db app",
		},
		{
			name: "older default is not an update",
			edit: func(x *ExtensionFacts) {
				x.Installed["vector"] = ExtensionVersion{Version: "2.0", Default: "1.0"}
			},
			id: "ext.updates", status: Pass, detail: "every extension", fix: "",
		},
		{
			name: "unknown default version",
			edit: func(x *ExtensionFacts) {
				x.Installed["vector"] = ExtensionVersion{Version: "0.8.0", Default: ""}
			},
			id: "ext.updates", status: Warn, detail: "cannot read the default version", fix: "vaire apply --db app",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			x := extensionFacts()
			tc.edit(&x)
			r := byID(Run(ExtensionChecks(), extensionFixture(x), ""), tc.id, "app")
			if r.Status != tc.status || !strings.Contains(r.Detail, tc.detail) || !strings.Contains(r.Fix, tc.fix) {
				t.Fatalf("%s: %s %q fix %q", tc.name, r.Status, r.Detail, r.Fix)
			}
		})
	}
}

func TestExtensionChecksInNodeChecks(t *testing.T) {
	found := 0
	for _, c := range NodeChecks() {
		if strings.HasPrefix(c.ID, "ext.") {
			found++
		}
	}
	if found != 3 {
		t.Fatalf("NodeChecks has %d ext checks", found)
	}
}
