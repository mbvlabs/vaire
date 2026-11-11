package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

type fakeRolesPG struct {
	roles      []string
	databases  []string
	extensions []string
	auth       map[string]string
	adminOwner string
	signalBack string
	maintain   string
	direct     []string
	loaded       map[string]bool
	pending      []string
	settings     map[string]string
	settingTypes map[string]string
	execs        []fakeRolesExec
}

type fakeRolesExec struct {
	db  string
	sql string
}

func (f *fakeRolesPG) query(_ context.Context, _ pg.Local, db, sql string) ([]string, error) {
	if db != "postgres" && !containsString(f.databases, db) {
		return nil, fmt.Errorf("database %q does not exist", db)
	}
	switch {
	case strings.Contains(sql, "FROM pg_roles"):
		return f.roles, nil
	case strings.Contains(sql, "FROM pg_database"):
		return f.databases, nil
	case strings.Contains(sql, "FROM pg_proc"):
		if strings.Contains(sql, "pgbouncer_get_auth_session") {
			return []string{f.auth["session"]}, nil
		}
		return []string{f.auth["auth"]}, nil
	case strings.Contains(sql, "pg_has_role('vaire_admin'"):
		switch {
		case strings.Contains(sql, "pg_signal_backend"):
			return []string{f.signalBack}, nil
		case strings.Contains(sql, "pg_maintain"):
			return []string{f.maintain}, nil
		default:
			return []string{f.adminOwner}, nil
		}
	case strings.Contains(sql, "FROM pg_auth_members"):
		return f.direct, nil
	case strings.Contains(sql, "FROM pg_extension"):
		return f.extensions, nil
	}
	return nil, fmt.Errorf("unexpected query on %s: %s", db, sql)
}

func (f *fakeRolesPG) exec(_ context.Context, _ pg.Local, db, sql string, _ map[string]string) error {
	if db != "postgres" && !containsString(f.databases, db) {
		return fmt.Errorf("database %q does not exist", db)
	}
	f.execs = append(f.execs, fakeRolesExec{db: db, sql: sql})
	if db == "postgres" && strings.Contains(sql, `CREATE DATABASE "app"`) && !containsString(f.databases, "app") {
		f.databases = append(f.databases, "app")
	}
	if db == "app" && strings.Contains(sql, `CREATE EXTENSION IF NOT EXISTS "custom_ext"`) {
		if !containsString(f.extensions, "custom_ext") {
			f.extensions = append(f.extensions, "custom_ext")
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func completeRolesPG() *fakeRolesPG {
	return &fakeRolesPG{
		roles: []string{
			"vaire_admin", "vaire_monitor", "pgbouncer_auth", RoleDirectOnly,
			"app_owner", "app_migrate", "app_app", "app_ro",
		},
		databases:  []string{"postgres", "app"},
		extensions: []string{"pg_stat_statements", "custom_ext"},
		auth: map[string]string{
			"auth":    "SELECT ... vaire_direct_only ...",
			"session": "SELECT ... operator connections ...",
		},
		adminOwner: "t",
		signalBack: "t",
		maintain:   "t",
		direct:     []string{"app_migrate"},
		loaded:     map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true},
	}
}

func rolesPipelineConfig() config.Cluster {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Extensions: []string{"custom"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib"},
	}
	return cfg
}

func useFakeRolesPG(t *testing.T, fake *fakeRolesPG) {
	t.Helper()
	extensionTestDirs(t)
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	oldQuery, oldExec := rolesQueryColumn, rolesExec
	oldLoaded, oldPending, oldSettings := rolesLoadedPreloads, rolesPendingRestart, rolesExtensionSettings
	oldRuntime, oldWrite := rolesRuntimeDir, rolesWriteRuntimePass
	rolesQueryColumn, rolesExec = fake.query, fake.exec
	rolesLoadedPreloads = func(context.Context, pg.Local) (map[string]bool, error) { return fake.loaded, nil }
	rolesPendingRestart = func(context.Context, pg.Local) ([]string, error) { return fake.pending, nil }
	rolesExtensionSettings = func(_ context.Context, _ pg.Local, _ string, keys []string) (map[string]ExtensionSettingFact, error) {
		out := map[string]ExtensionSettingFact{}
		for _, key := range keys {
			if value, ok := fake.settings[key]; ok {
				out[key] = ExtensionSettingFact{Value: value, Type: fake.settingTypes[key]}
			}
		}
		return out, nil
	}
	rolesRuntimeDir = t.TempDir()
	rolesWriteRuntimePass = func(engine.Runtime, string, string) error { return nil }
	t.Cleanup(func() {
		rolesQueryColumn, rolesExec = oldQuery, oldExec
		rolesLoadedPreloads, rolesPendingRestart, rolesExtensionSettings = oldLoaded, oldPending, oldSettings
		rolesRuntimeDir, rolesWriteRuntimePass = oldRuntime, oldWrite
	})
	for _, name := range []string{"pgbouncer_auth", "monitor"} {
		if err := os.WriteFile(filepath.Join(rolesRuntimeDir, name+".pass"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

type rolesRestartStep struct{ id string }

func (s rolesRestartStep) ID() string                     { return s.id }
func (s rolesRestartStep) Describe(config.Cluster) string { return s.id }
func (s rolesRestartStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s rolesRestartStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: true}, nil
}
func (s rolesRestartStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{Restart: engine.RestartRestart}, nil
}

type rolesRestartTemplate struct{}

func (rolesRestartTemplate) ID() string                     { return "config-v1" }
func (rolesRestartTemplate) Describe(config.Cluster) string { return "config-v1" }
func (rolesRestartTemplate) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte("config-v1"), nil
}
func (rolesRestartTemplate) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: true}, nil
}
func (rolesRestartTemplate) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, nil
}
func (rolesRestartTemplate) Instance(cluster string) engine.Step {
	return rolesRestartStep{id: engine.InstanceID("config-v1", cluster)}
}

type rolesRestarter struct{ restarted []string }

func (r *rolesRestarter) Restart(_ context.Context, cluster string) (time.Duration, error) {
	r.restarted = append(r.restarted, cluster)
	return 0, nil
}

func TestPostgresRolesPipelineNoTokenThenConfirmedRestart(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	fake.pending = []string{"shared_preload_libraries"}
	useFakeRolesPG(t, fake)
	journal := filepath.Join(t.TempDir(), "journal.json")
	restarter := &rolesRestarter{}
	catalog := []engine.Step{rolesRestartTemplate{}, postgresRoles{}}

	data, warnings, err := engine.Apply(context.Background(), catalog, cfg, engine.Runtime{}, engine.Options{
		DB: "app", JournalPath: journal, Restarter: restarter,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(restarter.restarted) != 0 {
		t.Fatalf("restarted without a token: %v", restarter.restarted)
	}
	if !containsWarning(warnings, "extension_pending_restart") {
		t.Fatalf("warnings = %+v", warnings)
	}
	if !containsString(data.RestartPending, "config-v1@app") {
		t.Fatalf("pending = %v", data.RestartPending)
	}
	j, err := engine.LoadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	rolesID := postgresRoles{}.Instance("app").ID()
	if got := j.Steps[rolesID]; got.Status != engine.StatusIncomplete {
		t.Fatalf("roles after no-token run = %+v", got)
	}

	// The confirmed run still puts roles before the end-of-run restart, so it
	// defers again and only the target cluster restarts.
	data, _, err = engine.Apply(context.Background(), catalog, cfg, engine.Runtime{}, engine.Options{
		DB: "app", JournalPath: journal, Restarter: restarter,
		Confirm: engine.Confirmation{Clusters: []string{"app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(restarter.restarted, "app") || len(restarter.restarted) != 1 {
		t.Fatalf("restarted %v, want app once", restarter.restarted)
	}
	if len(data.RestartPending) != 0 {
		t.Fatalf("pending after restart = %v", data.RestartPending)
	}
	j, err = engine.LoadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Steps[rolesID]; got.Status != engine.StatusIncomplete {
		t.Fatalf("roles after confirmed run = %+v", got)
	}

	// After the restart the same-hash retry sees the loaded library, creates
	// the deferred SQL and completes.
	fake.loaded["custom_lib"] = true
	fake.pending = nil
	if _, _, err := engine.Apply(context.Background(), catalog, cfg, engine.Runtime{}, engine.Options{
		DB: "app", JournalPath: journal, Restarter: restarter,
	}); err != nil {
		t.Fatal(err)
	}
	j, err = engine.LoadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Steps[rolesID]; got.Status != engine.StatusCompleted {
		t.Fatalf("roles after retry = %+v", got)
	}
	if !containsString(fake.extensions, "custom_ext") {
		t.Fatalf("deferred SQL was not created: %v", fake.extensions)
	}
	if res, err := (postgresRoles{}).Instance("app").Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("roles check after retry = %+v, %v", res, err)
	}
}

func TestPostgresRolesCheckRejectsMissingFacts(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")

	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("complete state = %+v, %v", res, err)
	}

	fake.extensions = []string{"pg_stat_statements"}
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "custom_ext") {
		t.Fatalf("missing SQL = %+v, %v", res, err)
	}

	fake.extensions = []string{"pg_stat_statements", "custom_ext"}
	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "custom_ext.control") {
		t.Fatalf("missing file = %+v, %v", res, err)
	}
	touchExtensionControl(t, "custom_ext")

	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "custom_lib") {
		t.Fatalf("missing preload = %+v, %v", res, err)
	}
}

func TestPostgresRolesNonCompletedJournalRechecks(t *testing.T) {
	cfg := rolesPipelineConfig()
	for _, status := range []string{engine.StatusFailed, engine.StatusRunning, engine.StatusIncomplete} {
		t.Run(status, func(t *testing.T) {
			fake := completeRolesPG()
			fake.extensions = []string{"pg_stat_statements"}
			useFakeRolesPG(t, fake)
			step := postgresRoles{}.Instance("app")
			inputs, err := step.Inputs(cfg, engine.Runtime{})
			if err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(t.TempDir(), "journal.json")
			j := &engine.Journal{Steps: map[string]engine.JournalEntry{
				step.ID(): {Status: status, InputsHash: engine.InputsHash(step.ID(), inputs)},
			}}
			if err := j.Save(journal); err != nil {
				t.Fatal(err)
			}
			if _, _, err := engine.Apply(context.Background(), []engine.Step{postgresRoles{}}, cfg, engine.Runtime{}, engine.Options{
				DB: "app", JournalPath: journal,
			}); err != nil {
				t.Fatal(err)
			}
			if !containsString(fake.extensions, "custom_ext") {
				t.Fatalf("status %s did not recheck and create: %v", status, fake.extensions)
			}
			j, err = engine.LoadJournal(journal)
			if err != nil {
				t.Fatal(err)
			}
			if got := j.Steps[step.ID()]; got.Status != engine.StatusCompleted {
				t.Fatalf("status %s journal = %+v", status, got)
			}
		})
	}
}

func TestPostgresRolesPipelineSettingPendingDefersOnlyDependentSQL(t *testing.T) {
	cfg := rolesPipelineConfig()
	cfg.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
		Settings: map[string]string{"custom.worker": "on"},
	}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
	fake.settings = map[string]string{"custom.worker": "off"}
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")

	// The library is loaded and no restart is pending, but the required
	// reloadable setting is still the old value. Dependent SQL must not run.
	res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || !containsWarning(res.Warnings, "extension_settings_pending") {
		t.Fatalf("old required setting result = %+v", res)
	}
	if containsWarning(res.Warnings, "extension_pending_restart") {
		t.Fatalf("a reload-only setting wait synthesized a restart: %+v", res.Warnings)
	}
	if containsString(fake.extensions, "custom_ext") {
		t.Fatalf("dependent SQL ran while its own required setting was old: %v", fake.extensions)
	}
	if len(fake.execs) == 0 {
		t.Fatal("role and grant convergence was blocked by the deferred extension SQL")
	}

	// The effective value on retry creates the SQL without a restart token.
	fake.settings["custom.worker"] = "on"
	res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete {
		t.Fatalf("effective required setting still deferred SQL: %+v", res)
	}
	if !containsString(fake.extensions, "custom_ext") {
		t.Fatalf("dependent SQL did not run: %v", fake.extensions)
	}

	// A required postmaster setting that is pending still defers with the
	// restart warning and no SQL.
	fake.extensions = []string{"pg_stat_statements"}
	fake.settings["custom.worker"] = "off"
	fake.pending = []string{"custom.worker"}
	res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || !containsWarning(res.Warnings, "extension_pending_restart") {
		t.Fatalf("required postmaster pending result = %+v", res)
	}
	if containsString(fake.extensions, "custom_ext") {
		t.Fatalf("dependent SQL ran while its required postmaster setting was pending: %v", fake.extensions)
	}

	// An unrelated pending PostgreSQL setting does not defer this extension's
	// SQL; only its own required keys matter.
	fake.extensions = []string{"pg_stat_statements"}
	fake.settings["custom.worker"] = "on"
	fake.pending = []string{"unrelated.setting"}
	res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete {
		t.Fatalf("an unrelated pending setting deferred SQL: %+v", res)
	}
	if !containsString(fake.extensions, "custom_ext") {
		t.Fatalf("dependent SQL did not run with an unrelated pending setting: %v", fake.extensions)
	}
}

func TestPostgresRolesRawQuoteOverrideReadiness(t *testing.T) {
	for _, where := range []string{"global", "cluster"} {
		t.Run(where, func(t *testing.T) {
			cfg := rolesPipelineConfig()
			cfg.Extensions["custom"] = config.ExtensionDefinition{
				Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
				Settings: map[string]string{"custom.worker": "on"},
			}
			switch where {
			case "global":
				cfg.Postgres.Settings = map[string]string{"custom.worker": "'off'"}
			case "cluster":
				cfg.Clusters[0].Settings = map[string]string{"custom.worker": "'off'"}
			}
			fake := completeRolesPG()
			fake.extensions = []string{"pg_stat_statements"}
			fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
			fake.settings = map[string]string{"custom.worker": "off"}
			useFakeRolesPG(t, fake)
			step := postgresRoles{}.Instance("app")

			res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !res.Incomplete || containsString(fake.extensions, "custom_ext") {
				t.Fatalf("raw quote-content override allowed unquoted actual: %+v, %v", res, fake.extensions)
			}

			fake.settings["custom.worker"] = "'off'"
			res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if res.Incomplete || !containsString(fake.extensions, "custom_ext") {
				t.Fatalf("raw quote-content override did not accept quoted actual: %+v, %v", res, fake.extensions)
			}
		})
	}
}

func TestPostgresRolesBooleanSettingReadiness(t *testing.T) {
	for _, tc := range []struct {
		name     string
		expected string
		actual   string
		typ      string
		wantSQL  bool
	}{
		{name: "true accepted as on", expected: "true", actual: "on", typ: "bool", wantSQL: true},
		{name: "false accepted as off", expected: "off", actual: "false", typ: "bool", wantSQL: true},
		{name: "string type stays exact", expected: "on", actual: "true", typ: "string", wantSQL: false},
		{name: "quote content is not boolean", expected: "'''off'''", actual: "off", typ: "bool", wantSQL: false},
		{name: "unknown placeholder stays exact", expected: "on", actual: "on", typ: "", wantSQL: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := rolesPipelineConfig()
			cfg.Extensions["custom"] = config.ExtensionDefinition{
				Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
				Settings: map[string]string{"custom.worker": tc.expected},
			}
			fake := completeRolesPG()
			fake.extensions = []string{"pg_stat_statements"}
			fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
			fake.settings = map[string]string{"custom.worker": tc.actual}
			fake.settingTypes = map[string]string{"custom.worker": tc.typ}
			useFakeRolesPG(t, fake)

			res, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			created := containsString(fake.extensions, "custom_ext")
			if created != tc.wantSQL {
				t.Fatalf("created = %v, want %v; result %+v", created, tc.wantSQL, res)
			}
			if res.Incomplete == tc.wantSQL {
				t.Fatalf("result incomplete = %v, want %v: %+v", res.Incomplete, !tc.wantSQL, res)
			}
		})
	}
}

func TestPostgresRolesQuoteContentReadiness(t *testing.T) {
	cfg := rolesPipelineConfig()
	cfg.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
		// The rendered literal requires the actual value 'off' (quote content).
		Settings: map[string]string{"custom.worker": "'''off'''"},
	}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
	fake.settings = map[string]string{"custom.worker": "off"}
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")

	res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || containsString(fake.extensions, "custom_ext") {
		t.Fatalf("unquoted actual matched quote-content expectation: %+v, %v", res, fake.extensions)
	}

	fake.settings["custom.worker"] = "'off'"
	res, err = step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete || !containsString(fake.extensions, "custom_ext") {
		t.Fatalf("quote-content actual did not satisfy the literal: %+v, %v", res, fake.extensions)
	}
}

func TestPostgresRolesSettingReadErrorRunsNoDependentSQL(t *testing.T) {
	cfg := rolesPipelineConfig()
	cfg.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
		Settings: map[string]string{"custom.worker": "on"},
	}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
	useFakeRolesPG(t, fake)
	old := rolesExtensionSettings
	rolesExtensionSettings = func(context.Context, pg.Local, string, []string) (map[string]ExtensionSettingFact, error) {
		return nil, errors.New("pg_settings is unavailable")
	}
	t.Cleanup(func() { rolesExtensionSettings = old })

	_, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "pg_settings is unavailable") {
		t.Fatalf("setting read error = %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("roles SQL ran without required setting facts: %v", fake.execs)
	}
}

func TestPostgresRolesApplyDefersOrRefusesMissingPreload(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")

	_, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "custom_lib") || !strings.Contains(err.Error(), "no restart is pending") {
		t.Fatalf("missing preload without pending = %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("wrote before refusing: %+v", fake.execs)
	}

	fake.pending = []string{"shared_preload_libraries"}
	res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || !containsWarning(res.Warnings, "extension_pending_restart") {
		t.Fatalf("deferred result = %+v", res)
	}
	if containsString(fake.extensions, "custom_ext") {
		t.Fatalf("deferred CREATE ran: %v", fake.extensions)
	}
}

func TestPostgresRolesFreshDatabaseBootstrapOrder(t *testing.T) {
	cfg := rolesPipelineConfig()
	cfg.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
		Settings: map[string]string{"custom.worker": "on"},
	}
	fake := completeRolesPG()
	fake.databases = []string{"postgres", "template0", "template1"}
	fake.extensions = nil
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true, "custom_lib": true}
	fake.settings = map[string]string{
		"custom.worker": "on", "pg_stat_statements.max": "10000", "pg_stat_statements.track": "top",
	}
	fake.settingTypes = map[string]string{"custom.worker": "bool"}
	useFakeRolesPG(t, fake)

	res, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Incomplete {
		t.Fatalf("fresh database apply incomplete: %+v", res)
	}
	if !containsString(fake.databases, "app") {
		t.Fatal("the application database was not created")
	}
	createdAt, firstTarget := -1, -1
	for i, exec := range fake.execs {
		if exec.db == "postgres" && strings.Contains(exec.sql, `CREATE DATABASE "app"`) {
			createdAt = i
		}
		if exec.db == "app" && firstTarget < 0 {
			firstTarget = i
		}
	}
	if createdAt < 0 || firstTarget < 0 || createdAt > firstTarget {
		t.Fatalf("CREATE DATABASE did not precede the target connection: %+v", fake.execs)
	}
	var targetSQL strings.Builder
	for _, exec := range fake.execs {
		if exec.db == "app" {
			targetSQL.WriteString(exec.sql)
		}
		if exec.db == "postgres" && strings.Contains(exec.sql, "CREATE EXTENSION") {
			t.Fatalf("extension SQL ran in postgres: %s", exec.sql)
		}
	}
	if !strings.Contains(targetSQL.String(), `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) ||
		!strings.Contains(targetSQL.String(), `CREATE EXTENSION IF NOT EXISTS "custom_ext"`) {
		t.Fatalf("fresh database extension SQL missing:\n%s", targetSQL.String())
	}
}

func TestPostgresRolesFreshDatabaseFailedPrerequisiteHasNoEffects(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	fake.databases = []string{"postgres", "template0", "template1"}
	useFakeRolesPG(t, fake)
	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	if _, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil); err == nil {
		t.Fatal("missing extension prerequisite succeeded")
	}
	if len(fake.execs) != 0 {
		t.Fatalf("a failed prerequisite created the database or roles: %+v", fake.execs)
	}
}

func TestPostgresRolesDefaultsStable(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("defaults check = %+v, %v", res, err)
	}
	res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err != nil || res.Incomplete {
		t.Fatalf("defaults apply = %+v, %v", res, err)
	}
	var sql strings.Builder
	for _, exec := range fake.execs {
		sql.WriteString(exec.sql)
	}
	if !strings.Contains(sql.String(), `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) {
		t.Fatalf("default SQL missing:\n%s", sql.String())
	}
	if strings.Contains(sql.String(), `CREATE EXTENSION IF NOT EXISTS "auto_explain"`) {
		t.Fatalf("auto_explain was created:\n%s", sql.String())
	}
}

func containsWarning(warnings []output.Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}
