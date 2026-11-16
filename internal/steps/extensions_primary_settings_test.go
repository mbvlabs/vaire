package steps

import (
	"context"
	"maps"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/tune"
)

func primarySettingsTestSpec() (config.Cluster, config.ClusterSpec) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	return cfg, cfg.Clusters[0]
}

func resolvePrimarySettingsExtension(t *testing.T, cfg config.Cluster, name string) catalog.Extension {
	t.Helper()
	ext, err := cfg.ExtensionResolver().Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	return ext
}

func renderedExtensionConf(t *testing.T, cfg config.Cluster, spec config.ClusterSpec) string {
	t.Helper()
	rendered, err := render.ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	return string(rendered)
}

func TestPrimaryExtensionExpectedSettingsFollowRenderedSelection(t *testing.T) {
	t.Run("implicit defaults render no descriptor settings", testPrimaryImplicitDefaults)
	t.Run("custom-only selection renders only its own settings", testPrimaryCustomOnly)
	t.Run("built-in alias selects the canonical descriptor", testPrimaryBuiltinAlias)
	t.Run("implicit extension still expects actually rendered overrides", testPrimaryImplicitOverrides)
}

func testPrimaryImplicitDefaults(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	rendered := renderedExtensionConf(t, cfg, spec)
	if !strings.Contains(rendered, "shared_preload_libraries = 'pg_stat_statements,auto_explain'") {
		t.Fatalf("implicit preloads are missing from the rendered configuration:\n%s", rendered)
	}
	if strings.Contains(rendered, "pg_stat_statements.") {
		t.Fatalf("the renderer wrote implicit descriptor settings:\n%s", rendered)
	}
	ext := resolvePrimarySettingsExtension(t, cfg, "pg_stat_statements")
	if got := PrimaryExtensionExpectedSettings(cfg, spec, ext); len(got) != 0 {
		t.Fatalf("implicit pg_stat_statements acquired unrendered expectations: %+v", got)
	}
}

func testPrimaryCustomOnly(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings: map[string]string{"custom.worker": "on"},
		},
	}
	spec.Extensions = []string{"custom"}
	cfg.Clusters[0] = spec
	rendered := renderedExtensionConf(t, cfg, spec)
	if !strings.Contains(rendered, "custom.worker = on") {
		t.Fatalf("the explicit custom setting is not rendered:\n%s", rendered)
	}
	if strings.Contains(rendered, "pg_stat_statements.") {
		t.Fatalf("the renderer wrote implicit descriptor settings:\n%s", rendered)
	}
	stats := resolvePrimarySettingsExtension(t, cfg, "pg_stat_statements")
	if got := PrimaryExtensionExpectedSettings(cfg, spec, stats); len(got) != 0 {
		t.Fatalf("custom-only intent acquired implicit expectations: %+v", got)
	}
	custom := resolvePrimarySettingsExtension(t, cfg, "custom")
	if got := PrimaryExtensionExpectedSettings(cfg, spec, custom); !maps.Equal(got, map[string]string{"custom.worker": "on"}) {
		t.Fatalf("custom expectations = %+v, want its rendered setting", got)
	}
}

func testPrimaryBuiltinAlias(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"vector": {Settings: map[string]string{"vector.mode": "on"}},
	}
	spec.Extensions = []string{"pgvector"}
	ext := resolvePrimarySettingsExtension(t, cfg, "pgvector")
	if ext.Name != "vector" {
		t.Fatalf("alias resolved to %q", ext.Name)
	}
	if got := PrimaryExtensionExpectedSettings(cfg, spec, ext); !maps.Equal(got, map[string]string{"vector.mode": "on"}) {
		t.Fatalf("aliased explicit expectations = %+v, want the rendered setting", got)
	}
	if rendered := renderedExtensionConf(t, cfg, spec); !strings.Contains(rendered, "vector.mode = on") {
		t.Fatalf("the aliased setting is not rendered:\n%s", rendered)
	}
}

func testPrimaryImplicitOverrides(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	cfg.Postgres.Settings = map[string]string{
		"pg_stat_statements.max": "12000",
		"random_page_cost":       "1.10",
	}
	spec.Settings = map[string]string{
		"pg_stat_statements.max":   "13000",
		"pg_stat_statements.track": "all",
		"work_mem":                 "8MB",
	}
	ext := resolvePrimarySettingsExtension(t, cfg, "pg_stat_statements")
	want := map[string]string{
		"pg_stat_statements.max":   "13000",
		"pg_stat_statements.track": "all",
	}
	if got := PrimaryExtensionExpectedSettings(cfg, spec, ext); !maps.Equal(got, want) {
		t.Fatalf("implicit override expectations = %+v, want %+v", got, want)
	}
	rendered := string(render.SettingsConf(cfg, spec))
	for _, line := range []string{
		"pg_stat_statements.max = '13000'",
		"pg_stat_statements.track = 'all'",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("override %q is not rendered:\n%s", line, rendered)
		}
	}
}

func TestPostgresRolesFreshBootstrapImplicitStatementsNativeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name   string
		custom bool
	}{
		{name: "empty selection"},
		{name: "custom-only selection", custom: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testPostgresRolesFreshBootstrapImplicitStatements(t, tc.custom)
		})
	}
}

func testPostgresRolesFreshBootstrapImplicitStatements(t *testing.T, custom bool) {
	cfg, spec := primarySettingsTestSpec()
	if custom {
		cfg.Extensions = map[string]config.ExtensionDefinition{
			"custom": {
				Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
				Settings: map[string]string{"custom.worker": "on"},
			},
		}
		spec.Extensions = []string{"custom"}
		cfg.Clusters[0] = spec
	}
	// Rendering is the authority: implicit pg_stat_statements keeps
	// its preload but writes no descriptor settings, so PostgreSQL's
	// native defaults (max = 5000) are the effective values.
	rendered := renderedExtensionConf(t, cfg, spec)
	if !strings.Contains(rendered, "shared_preload_libraries = 'pg_stat_statements,auto_explain") {
		t.Fatalf("implicit preloads are missing from the rendered configuration:\n%s", rendered)
	}
	if strings.Contains(rendered, "pg_stat_statements.") {
		t.Fatalf("the renderer wrote implicit descriptor settings:\n%s", rendered)
	}
	stats := resolvePrimarySettingsExtension(t, cfg, "pg_stat_statements")
	if got := PrimaryExtensionExpectedSettings(cfg, spec, stats); len(got) != 0 {
		t.Fatalf("implicit pg_stat_statements expectations = %+v", got)
	}

	fake := completeRolesPG()
	fake.databases = []string{"postgres", "template0", "template1"}
	fake.extensions = nil
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
	fake.settings = map[string]string{
		"pg_stat_statements.max":   "5000",
		"pg_stat_statements.track": "top",
	}
	if custom {
		fake.settings["custom.worker"] = "on"
	}
	useFakeRolesPG(t, fake)

	res, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete {
		t.Fatalf("fresh bootstrap deferred on unrendered native defaults: %+v", res)
	}
	if !containsString(fake.databases, "app") {
		t.Fatal("the application database was not created")
	}
	var targetSQL strings.Builder
	for _, exec := range fake.execs {
		if exec.db == "app" {
			targetSQL.WriteString(exec.sql)
		}
	}
	sql := targetSQL.String()
	if !strings.Contains(sql, `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) {
		t.Fatalf("implicit SQL was not created:\n%s", sql)
	}
	if custom && !strings.Contains(sql, `CREATE EXTENSION IF NOT EXISTS "custom_ext"`) {
		t.Fatalf("explicit custom SQL was not created:\n%s", sql)
	}
}

func TestPostgresRolesExplicitStatementsSettingMismatchDefers(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	spec.Extensions = []string{"pg_stat_statements"}
	cfg.Clusters[0] = spec
	stats := resolvePrimarySettingsExtension(t, cfg, "pg_stat_statements")
	want := map[string]string{
		"pg_stat_statements.max":   "10000",
		"pg_stat_statements.track": "top",
	}
	if got := PrimaryExtensionExpectedSettings(cfg, spec, stats); !maps.Equal(got, want) {
		t.Fatalf("explicit expectations = %+v, want %+v", got, want)
	}
	rendered := renderedExtensionConf(t, cfg, spec)
	for _, line := range []string{"pg_stat_statements.max = 10000", "pg_stat_statements.track = top"} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("explicit descriptor setting %q is not rendered:\n%s", line, rendered)
		}
	}

	fake := completeRolesPG()
	fake.databases = []string{"postgres", "template0", "template1"}
	fake.extensions = nil
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	fake.settings = map[string]string{
		"pg_stat_statements.max":   "5000",
		"pg_stat_statements.track": "top",
	}
	useFakeRolesPG(t, fake)
	step := (postgresRoles{}).Instance("app")

	res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || !containsWarning(res.Warnings, "extension_settings_pending") {
		t.Fatalf("explicit descriptor mismatch did not defer: %+v", res)
	}
	if strings.Contains(execsForDatabase(fake.execs, "app"), `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) {
		t.Fatal("dependent SQL ran with the wrong explicit setting")
	}

	fake.settings["pg_stat_statements.max"] = "10000"
	res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete {
		t.Fatalf("corrected explicit setting did not converge: %+v", res)
	}
	if !strings.Contains(execsForDatabase(fake.execs, "app"), `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) {
		t.Fatal("dependent SQL did not run after the setting became effective")
	}
}

func TestPostgresRolesExplicitStatementsMissingSettingRunsNoDependentSQL(t *testing.T) {
	cfg, spec := primarySettingsTestSpec()
	spec.Extensions = []string{"pg_stat_statements"}
	cfg.Clusters[0] = spec
	fake := completeRolesPG()
	fake.databases = []string{"postgres", "template0", "template1"}
	fake.extensions = nil
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	fake.settings = map[string]string{"pg_stat_statements.track": "top"}
	useFakeRolesPG(t, fake)

	_, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "pg_stat_statements.max") {
		t.Fatalf("missing explicit setting = %v", err)
	}
	if sql := execsForDatabase(fake.execs, "app"); sql != "" {
		t.Fatalf("dependent SQL ran without an explicit setting fact:\n%s", sql)
	}
}

func execsForDatabase(execs []fakeRolesExec, db string) string {
	var sql strings.Builder
	for _, exec := range execs {
		if exec.db == db {
			sql.WriteString(exec.sql)
		}
	}
	return sql.String()
}
