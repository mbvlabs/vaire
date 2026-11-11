package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

func standaloneExtensionCfg() config.Cluster {
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s"},
	}
	cfg.Backup.Database = config.BackupDatabase{}
	return cfg
}

func useNodeConfigStore(t *testing.T, raw []byte) (state.FileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cluster.toml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	store := state.FileStore{Path: path}
	old := nodeConfigStore
	nodeConfigStore = func() state.Store { return store }
	t.Cleanup(func() { nodeConfigStore = old })
	return store, path
}

func failFollowerCatalog(t *testing.T) {
	t.Helper()
	restore := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return nil, errors.New("catalog is down")
	})
	t.Cleanup(restore)
}

func TestFollowerCatalogFailureRefusesAllProductionEntrypointsBeforeEffects(t *testing.T) {
	t.Run("ext add", func(t *testing.T) {
		cfg := extFollowerCfg(t)
		h := newExtHarness(t, cfg)
		failFollowerCatalog(t)
		req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
		req.Confirm = "app/restart"
		req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
		_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
		if err == nil || extReason(t, err) != "follower_sql_unknown" {
			t.Fatalf("ext add = %v", err)
		}
		if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
			t.Fatalf("ext add had effects: saves=%d prepare=%d apply=%d exec=%d", len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs))
		}
		if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
			t.Fatalf("ext add wrote history: %+v, %v", recs.ExtensionRequirements, err)
		}
	})

	t.Run("ext remove", func(t *testing.T) {
		cfg := extFollowerCfg(t)
		cfg.Clusters[0].Extensions = []string{"pg_cron"}
		cfg.Clusters[1].Extensions = []string{"pg_cron"}
		cfg.Clusters[2].Extensions = []string{"pg_cron"}
		h := newExtHarness(t, cfg)
		failFollowerCatalog(t)
		req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
		req.Confirm = "app"
		req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
		_, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg})
		if err == nil || extReason(t, err) != "follower_sql_unknown" {
			t.Fatalf("ext remove = %v", err)
		}
		if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
			t.Fatalf("ext remove had effects: saves=%d apply=%d exec=%d", len(h.saves), len(h.applyCalls), len(h.pg.execs))
		}
	})

	t.Run("config set", func(t *testing.T) {
		cfg := extFollowerCfg(t)
		cfg.Backup.Database = config.BackupDatabase{}
		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		store, path := useNodeConfigStore(t, raw)
		revision, err := store.Revision()
		if err != nil {
			t.Fatal(err)
		}
		h := newExtHarness(t, cfg)
		failFollowerCatalog(t)
		req := request("config.set", map[string]any{"toml": string(raw), "base_revision": revision})
		if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "follower_sql_unknown" {
			t.Fatalf("config set = %v", err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) { //nolint:gosec // G304: test-owned temp path.
			t.Fatalf("config set changed the file: %v", err)
		}
		if len(h.saves) != 0 || len(h.applyCalls) != 0 {
			t.Fatalf("config set had effects: saves=%d apply=%d", len(h.saves), len(h.applyCalls))
		}
	})

	t.Run("plain apply", func(t *testing.T) {
		cfg := extFollowerCfg(t)
		h := newExtHarness(t, cfg)
		failFollowerCatalog(t)
		oldPlan := extensionPlan
		extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
			return engine.PlanData{WouldApply: 1}, nil
		}
		t.Cleanup(func() { extensionPlan = oldPlan })
		req := request("apply", map[string]any{"db": "app"})
		if _, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"}); err == nil || extReason(t, err) != "follower_sql_unknown" {
			t.Fatalf("plain apply = %v", err)
		}
		if len(h.applyCalls) != 0 || len(h.saves) != 0 || h.prepareCalls != 0 {
			t.Fatalf("plain apply had effects: apply=%d saves=%d prepare=%d", len(h.applyCalls), len(h.saves), h.prepareCalls)
		}
	})
}

func TestVerifyFollowerRuntimeBooleanSettingFacts(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  string
		want bool
	}{
		{name: "bool on matches true", typ: "bool", want: true},
		{name: "string on stays exact", typ: "string", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extFollowerCfg(t)
			cfg.Extensions = map[string]config.ExtensionDefinition{
				"custom": {
					Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
					Settings:     map[string]string{"custom.worker": "true"},
					CopySafety:   stringPtr(catalog.CopySafetySafe),
					StandbySafe:  boolPtr(true),
				},
			}
			for i := range cfg.Clusters {
				cfg.Clusters[i].Extensions = []string{"custom"}
			}
			h := newExtHarness(t, cfg)
			h.pg.queryFn = func(sql string) ([]map[string]string, error) {
				if strings.Contains(sql, "custom.worker") {
					return []map[string]string{{"v": "on", "vartype": tc.typ}}, nil
				}
				return defaultExtensionSettingsFake(sql)
			}
			err := verifyFollowerRuntime(context.Background(), cfg, cfg.Clusters[1])
			if (err == nil) != tc.want {
				t.Fatalf("verify = %v, want success %v", err, tc.want)
			}
		})
	}
}

func TestStandalonePrimaryCustomUnknownSafetyIsValid(t *testing.T) {
	t.Run("ext add", func(t *testing.T) {
		cfg := standaloneExtensionCfg()
		h := newExtHarness(t, cfg)
		h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
			return map[string]extensionInstalledVersion{
				"custom":     {Name: "custom", Version: "1.0"},
				"custom_ext": {Name: "custom_ext", Version: "1.0"},
			}, nil
		}
		req := request("ext.add", map[string]any{"name": "custom", "db": "app", "preinstalled": true})
		res, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
		if err != nil {
			t.Fatalf("standalone custom unknown add = %v", err)
		}
		got, ok := res.Data.(extAddResult)
		if !ok || got.Extension != "custom" {
			t.Fatalf("result = %+v", res.Data)
		}
		if len(h.saves) != 1 {
			t.Fatalf("intent saves = %d", len(h.saves))
		}
	})

	t.Run("unrelated config set", func(t *testing.T) {
		cfg := standaloneExtensionCfg()
		cfg.Extensions = map[string]config.ExtensionDefinition{
			"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
		}
		cfg.Clusters[0].Extensions = []string{"custom"}
		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		store, path := useNodeConfigStore(t, raw)
		revision, err := store.Revision()
		if err != nil {
			t.Fatal(err)
		}
		newExtHarness(t, cfg)
		updated := cfg.Clone()
		updated.Postgres.Settings = map[string]string{"work_mem": "8MB"}
		updatedRaw, err := updated.Encode()
		if err != nil {
			t.Fatal(err)
		}
		req := request("config.set", map[string]any{"toml": string(updatedRaw), "base_revision": revision})
		if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
			t.Fatalf("unrelated config set with custom unknown = %v", err)
		}
		got, err := os.ReadFile(path) //nolint:gosec // G304: test-owned temp path.
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "work_mem") {
			t.Fatalf("unrelated setting was not saved: %s", got)
		}
	})

	t.Run("config set with follower gates custom unknown", func(t *testing.T) {
		cfg := extFollowerCfg(t)
		cfg.Backup.Database = config.BackupDatabase{}
		cfg.Extensions = map[string]config.ExtensionDefinition{
			"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
		}
		for i := range cfg.Clusters {
			cfg.Clusters[i].Extensions = []string{"custom"}
		}
		raw, err := cfg.Encode()
		if err != nil {
			t.Fatal(err)
		}
		store, path := useNodeConfigStore(t, raw)
		revision, err := store.Revision()
		if err != nil {
			t.Fatal(err)
		}
		newExtHarness(t, cfg)
		req := request("config.set", map[string]any{"toml": string(raw), "base_revision": revision})
		if _, err := handleConfigSet(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "extension_copy_safety_unknown" {
			t.Fatalf("config set with follower = %v", err)
		}
		if got, err := os.ReadFile(path); err != nil || string(got) != string(raw) { //nolint:gosec // G304: test-owned temp path.
			t.Fatalf("config set with follower changed the file: %v", err)
		}
	})

	t.Run("follow fails before effects", func(t *testing.T) {
		cfg := standaloneExtensionCfg()
		cfg.Extensions = map[string]config.ExtensionDefinition{
			"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
		}
		cfg.Clusters[0].Extensions = []string{"custom"}
		declared := []string{"custom"}
		cfg.Clusters[0].RecoveryExtensions = &declared
		if err := preflightRestoreExtensionPlan(context.Background(), cfg, cfg.Clusters[0], restoreParams{Kind: targetDump, Database: "app"}); err == nil || extReason(t, err) != "extension_copy_safety_unknown" {
			t.Fatalf("copy preflight = %v", err)
		}
		if recs, err := backup.LoadRecords(); err != nil || len(recs.Restores) != 0 {
			t.Fatalf("copy preflight wrote an open restore: %+v, %v", recs.Restores, err)
		}
	})
}

func TestRolesOnlyTransitionDoesNotCreatePrimarySQLBeforeFollowerReady(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:    map[string]string{"addon.mode": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"addon"}
	cfg.Clusters[1].Extensions = []string{"addon"}
	cfg.Clusters[2].Extensions = []string{"addon"}
	h := newExtHarness(t, cfg)
	addonValue := "off"
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "addon.mode") {
			return []map[string]string{{"v": addonValue}}, nil
		}
		return defaultExtensionSettingsFake(sql)
	}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.10"},
		}, nil
	}
	oldPlan := extensionPlan
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			return engine.PlanData{WouldApply: 1}, nil
		}
		return engine.PlanData{}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	rolesCalls := 0
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			rolesCalls++
		}
		return engine.ApplyData{}, nil
	}

	// Config and guards are converged; only the source's missing extension SQL
	// makes this a transition. The follower's required setting is wrong, so
	// the primary roles step must not run.
	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	t.Cleanup(func() { applyReloadCluster = oldReload })
	if _, err := handleApply(context.Background(), &Env{
		Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockExclusive,
	}); err == nil {
		t.Fatal("a bad follower runtime was accepted")
	}
	if rolesCalls != 0 {
		t.Fatalf("the primary roles step ran before the follower was effective: %d", rolesCalls)
	}
	for _, opts := range h.applyCalls {
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			t.Fatalf("primary extension SQL ran before the follower was effective: %v", h.applyCalls)
		}
	}

	// Fix the follower fact. The same roles-only transition now completes
	// without a restart token.
	addonValue = "on"
	h.applyCalls = nil
	req := request("apply", map[string]any{"db": "app"})
	warnings, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatal(err)
	}
	_ = warnings
	if rolesCalls != 1 {
		t.Fatalf("roles calls = %d, want one", rolesCalls)
	}
	var order []string
	for _, opts := range h.applyCalls {
		order = append(order, opts.DB+":"+opts.Only[0])
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") && len(opts.Confirm.Clusters) != 0 {
			t.Fatalf("roles needed a restart token: %+v", opts.Confirm)
		}
	}
	want := []string{
		"app_follower_1:postgres-config-v1@app_follower_1",
		"app_follower_2:postgres-config-v1@app_follower_2",
		"app:postgres-config-v1@app",
		"app:postgres-roles-v1@app",
	}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("roles-only transition order = %v, want %v", order, want)
	}
}

func TestExtAddWithFollowersHonorsPrimarySettingOverride(t *testing.T) {
	for _, where := range []string{"global", "percluster"} {
		t.Run(where, func(t *testing.T) {
			cfg := extFollowerCfg(t)
			switch where {
			case "global":
				cfg.Postgres.Settings = map[string]string{"pgaudit.log": "write"}
			case "percluster":
				for i := range cfg.Clusters {
					cfg.Clusters[i].Settings = map[string]string{"pgaudit.log": "write"}
				}
			}
			cfg.Clusters[0].Extensions = []string{"pgaudit"}
			cfg.Clusters[1].Extensions = []string{"pgaudit"}
			cfg.Clusters[2].Extensions = []string{"pgaudit"}
			h := newExtHarness(t, cfg)
			h.preloadLoaded["pgaudit"] = true
			h.pg.queryFn = func(sql string) ([]map[string]string, error) {
				if strings.Contains(sql, "pgaudit.log") {
					return []map[string]string{{"v": "write"}}, nil
				}
				return defaultExtensionSettingsFake(sql)
			}
			h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
				return map[string]extensionInstalledVersion{
					"pgaudit": {Name: "pgaudit", Version: "1.0", Default: "1.0"},
				}, nil
			}
			req := request("ext.add", map[string]any{"name": "pgaudit", "db": "app"})
			req.Confirm = "app/restart"
			req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
			if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
				t.Fatalf("pgaudit override add = %v", err)
			}
			if len(h.saves) != 1 {
				t.Fatalf("intent saves = %d", len(h.saves))
			}
			var order []string
			for _, opts := range h.applyCalls {
				order = append(order, opts.DB+":"+opts.Only[0])
			}
			want := []string{
				"app_follower_1:postgres-config-v1@app_follower_1",
				"app_follower_2:postgres-config-v1@app_follower_2",
				"app:postgres-config-v1@app",
				"app:postgres-roles-v1@app",
			}
			if strings.Join(order, "|") != strings.Join(want, "|") {
				t.Fatalf("pgaudit override order = %v, want %v", order, want)
			}
		})
	}
}

func TestExtAddWithFollowersUpdateUsesPrimaryReadiness(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings:    map[string]string{"custom.worker": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	cfg.Postgres.Settings = map[string]string{"custom.worker": "'off'"}
	cfg.Clusters[0].Extensions = []string{"custom"}
	cfg.Clusters[1].Extensions = []string{"custom"}
	cfg.Clusters[2].Extensions = []string{"custom"}
	h := newExtHarness(t, cfg)
	version := "1.0"
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"custom_ext": {Name: "custom_ext", Version: version, Default: "2.0"},
		}, nil
	}
	h.updatePath = true
	actual := "off"
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "custom.worker") {
			return []map[string]string{{"v": actual}}, nil
		}
		return defaultExtensionSettingsFake(sql)
	}
	h.pg.execFn = func(sql string) {
		if strings.Contains(sql, "ALTER EXTENSION") {
			version = "2.0"
		}
	}
	req := request("ext.add", map[string]any{"name": "custom", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil {
		t.Fatal("follower-enabled update accepted the wrong quote-content value")
	}
	for _, sql := range h.pg.execs {
		if strings.Contains(sql, "ALTER EXTENSION") {
			t.Fatalf("follower-enabled ALTER ran before the raw quote-content value was effective: %v", h.pg.execs)
		}
	}

	actual = "'off'"
	h.pg.execs = nil
	req = request("ext.add", map[string]any{"name": "custom", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("follower-enabled update = %v", err)
	}
	found := false
	for _, sql := range h.pg.execs {
		if strings.Contains(sql, "ALTER EXTENSION") {
			found = true
		}
	}
	if !found {
		t.Fatalf("follower-enabled supported update did not run: %v", h.pg.execs)
	}
}

func TestExtAddInstalledUpdateWaitsForEffectivePrimarySettings(t *testing.T) {
	for _, where := range []string{"global", "cluster"} {
		t.Run(where, func(t *testing.T) {
			cfg := standaloneExtensionCfg()
			cfg.Extensions = map[string]config.ExtensionDefinition{
				"custom": {
					Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
					Settings:    map[string]string{"custom.worker": "on"},
					CopySafety:  stringPtr(catalog.CopySafetySafe),
					StandbySafe: boolPtr(true),
				},
			}
			switch where {
			case "global":
				cfg.Postgres.Settings = map[string]string{"custom.worker": "'off'"}
			case "cluster":
				cfg.Clusters[0].Settings = map[string]string{"custom.worker": "'off'"}
			}
			cfg.Clusters[0].Extensions = []string{"custom"}
			h := newExtHarness(t, cfg)
			version := "1.0"
			h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
				return map[string]extensionInstalledVersion{
					"custom_ext": {Name: "custom_ext", Version: version, Default: "2.0"},
				}, nil
			}
			h.updatePath = true
			actual := "off"
			h.pg.queryFn = func(sql string) ([]map[string]string, error) {
				if strings.Contains(sql, "custom.worker") {
					return []map[string]string{{"v": actual}}, nil
				}
				return defaultExtensionSettingsFake(sql)
			}
			h.pg.execFn = func(sql string) {
				if strings.Contains(sql, "ALTER EXTENSION") {
					version = "2.0"
				}
			}
			req := request("ext.add", map[string]any{"name": "custom", "db": "app"})
			_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
			if err == nil || extReason(t, err) != "extension_update_prerequisites" {
				t.Fatalf("old primary setting update = %v", err)
			}
			for _, sql := range h.pg.execs {
				if strings.Contains(sql, "ALTER EXTENSION") {
					t.Fatalf("ALTER EXTENSION ran before its required setting was effective: %v", h.pg.execs)
				}
			}

			actual = "'off'"
			req = request("ext.add", map[string]any{"name": "custom", "db": "app"})
			if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
				t.Fatalf("effective setting update = %v", err)
			}
			found := false
			for _, sql := range h.pg.execs {
				if strings.Contains(sql, "ALTER EXTENSION") {
					found = true
				}
			}
			if !found {
				t.Fatalf("supported update did not run after the setting became effective: %v", h.pg.execs)
			}
		})
	}
}

func TestRolesOnlyPolicyChangeDoesNotQueryFollowerExtensionState(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"addon"}
	cfg.Clusters[1].Extensions = []string{"addon"}
	cfg.Clusters[2].Extensions = []string{"addon"}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.10"},
			"addon_ext":          {Name: "addon_ext", Version: "1.0"},
		}, nil
	}
	oldPlan := extensionPlan
	followerConfigPlans := 0
	extensionPlan = func(ctx context.Context, catalogSteps []engine.Step, planCfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") {
			if opts.DB == "app" {
				return engine.PlanData{}, nil
			}
			followerConfigPlans++
			// Run the real production Check. It must never be reached for a
			// roles-only policy change.
			return engine.Plan(ctx, catalogSteps, planCfg, rt, opts)
		}
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			return engine.PlanData{WouldApply: 1}, nil
		}
		return engine.PlanData{}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	failFollowerCatalog(t)

	req := request("apply", map[string]any{"db": "app"})
	warnings, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatalf("roles-only policy change was blocked by follower extension state: %v", err)
	}
	_ = warnings
	if followerConfigPlans != 0 {
		t.Fatalf("a roles-only policy change planned follower configuration %d times", followerConfigPlans)
	}
	if len(h.applyCalls) != 0 {
		t.Fatalf("a roles-only policy change staged a follower transition: %v", h.applyCalls)
	}

	// A stopped follower must not block a roles-only change either: the source
	// gate decides before the target set exists.
	h.runningFn = func(config.ClusterSpec) bool { return false }
	if _, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"}); err != nil {
		t.Fatalf("roles-only change with a stopped follower = %v", err)
	} else if release != nil {
		release()
	}
	if followerConfigPlans != 0 {
		t.Fatalf("a stopped follower was planned for a roles-only change: %d", followerConfigPlans)
	}
}

func TestSettingOnlyRolesDeferralDoesNotSynthesizeRestartToken(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:    map[string]string{"addon.mode": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"addon"}
	cfg.Clusters[1].Extensions = []string{"addon"}
	cfg.Clusters[2].Extensions = []string{"addon"}
	h := newExtHarness(t, cfg)
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "addon.mode") {
			return []map[string]string{{"v": "on"}}, nil
		}
		return defaultExtensionSettingsFake(sql)
	}
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_stat_statements": {Name: "pg_stat_statements", Version: "1.10"},
		}, nil
	}
	oldPlan := extensionPlan
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			return engine.PlanData{WouldApply: 1}, nil
		}
		return engine.PlanData{}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-roles-v1") {
			h.applyWarnings = []output.Warning{{
				Code: "extension_settings_pending", Message: "addon.mode is off, want on",
			}}
			return engine.ApplyData{Steps: []engine.ApplyStep{{
				ID: opts.Only[0], State: engine.StatusIncomplete,
			}}}, nil
		}
		h.applyWarnings = nil
		return engine.ApplyData{}, nil
	}
	req := request("apply", map[string]any{"db": "app"})
	_, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	var pending *extensionApplyPending
	if !errors.As(err, &pending) {
		t.Fatalf("setting-only deferral = %v, want pending", err)
	}
	if slices.Contains(pending.Tokens, "app/restart") {
		t.Fatalf("a reload-only setting wait synthesized a restart token: %v", pending.Tokens)
	}
}

func TestFollowerRawQuoteOverrideBlocksPrimaryUntilCorrect(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:    map[string]string{"addon.worker": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	cfg.Postgres.Settings = map[string]string{"addon.worker": "'off'"}
	cfg.Clusters[0].Extensions = []string{"addon"}
	cfg.Clusters[1].Extensions = []string{"addon"}
	cfg.Clusters[2].Extensions = []string{"addon"}
	h := newExtHarness(t, cfg)
	actual := "off"
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "addon.worker") {
			return []map[string]string{{"v": actual}}, nil
		}
		return defaultExtensionSettingsFake(sql)
	}
	oldPlan := extensionPlan
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	req := request("apply", map[string]any{"db": "app"})
	_, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	if err == nil || !strings.Contains(err.Error(), "addon.worker") {
		t.Fatalf("raw quote override wrong actual = %v", err)
	}
	for _, opts := range h.applyCalls {
		if opts.DB == "app" {
			t.Fatalf("primary configuration/SQL ran before the follower quote-content value was effective: %v", h.applyCalls)
		}
	}

	actual = "'off'"
	h.applyCalls = nil
	req = request("apply", map[string]any{"db": "app"})
	if _, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"}); err != nil {
		t.Fatalf("correct quote-content follower = %v", err)
	} else if release != nil {
		release()
	}
	primary := false
	for _, opts := range h.applyCalls {
		if opts.DB == "app" {
			primary = true
		}
	}
	if !primary {
		t.Fatalf("primary did not proceed after the follower became effective: %v", h.applyCalls)
	}
}

func TestFollowerExpectedRuntimeBlocksPrimaryBeforeConfigAndSQL(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:    map[string]string{"addon.mode": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"addon"}
	cfg.Clusters[1].Extensions = []string{"addon"}
	cfg.Clusters[2].Extensions = []string{"addon"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["addon_ext"] = true
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "addon.mode") {
			return []map[string]string{{"v": "off"}}, nil
		}
		return defaultExtensionSettingsFake(sql)
	}
	oldPlan := extensionPlan
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	req := request("apply", map[string]any{"db": "app"})
	_, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	if err == nil || !strings.Contains(err.Error(), "addon.mode") {
		t.Fatalf("wrong follower runtime = %v", err)
	}
	for _, opts := range h.applyCalls {
		if opts.DB == "app" {
			t.Fatalf("primary configuration or SQL ran before the follower was effective: %v", h.applyCalls)
		}
	}
}

func TestHandleApplyPendingTransitionReturnsStagedResult(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	t.Cleanup(func() { applyReloadCluster = oldReload })
	oldPlan := extensionPlan
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1, RestartPending: []string{opts.Only[0]}}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") && len(opts.Confirm.Clusters) == 0 {
			return engine.ApplyData{RestartPending: []string{opts.Only[0]}}, nil
		}
		return engine.ApplyData{}, nil
	}
	res, err := handleApply(context.Background(), &Env{
		Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockExclusive,
	})
	if err != nil {
		t.Fatalf("handle apply pending = %v", err)
	}
	data, ok := res.Data.(engine.ApplyData)
	if !ok || len(data.RestartPending) == 0 || len(data.ConfirmTokens) == 0 {
		t.Fatalf("pending result = %+v", res.Data)
	}
	for _, opts := range h.applyCalls {
		if opts.DB == "app" {
			t.Fatalf("the normal apply pass or primary ran after a pending follower: %v", h.applyCalls)
		}
	}
}

func TestFollowerRemovalCatalogErrorAfterDropKeepsGuardPending(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	cfg.Clusters[2].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	installedCalls := 0
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		installedCalls++
		if installedCalls == 1 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	dataDir := cfg.Server.DataDirectory("app_follower_1")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldWriter := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // G304: test-owned path
	})
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldWriter) })
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
		Origins:  map[string]string{"pg_cron": "retained"},
	}); err != nil {
		t.Fatal(err)
	}
	sqlCalls := 0
	restoreSQL := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		sqlCalls++
		if sqlCalls <= 2 {
			return map[string]bool{"plpgsql": true, "pg_cron": true}, nil
		}
		return nil, errors.New("catalog went away after the primary DROP")
	})
	defer restoreSQL()

	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	_, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_follower_replay_pending" {
		t.Fatalf("post-DROP catalog error = %v", err)
	}
	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("the primary DROP did not run: %v", h.pg.execs)
	}
	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if !slices.Contains(guard.Preloads, "pg_cron") || guard.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("an unreadable catalog retired the old guard: %+v", guard)
	}
}

func TestFollowerRemovalImmediateAbsenceCompletes(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	cfg.Clusters[2].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	installedCalls := 0
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		installedCalls++
		if installedCalls == 1 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	restoreSQL := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true}, nil
	})
	defer restoreSQL()

	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("immediate absence removal = %v", err)
	}
	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("DROP did not run first: %v", h.pg.execs)
	}
	if len(h.saves) != 1 {
		t.Fatalf("intent saves = %d", len(h.saves))
	}
	var applied []string
	for _, opts := range h.applyCalls {
		applied = append(applied, opts.DB)
	}
	want := []string{"app", "app_follower_1", "app_follower_2"}
	if strings.Join(applied, ",") != strings.Join(want, ",") {
		t.Fatalf("apply order = %v, want %v", applied, want)
	}
}

func TestForkCopyPlanRejectsUnknownCopySafetyBeforeEffects(t *testing.T) {
	cfg := standaloneExtensionCfg()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	cfg.Clusters[0].Extensions = []string{"custom"}
	_, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: cfg.Clusters[0], CopyDB: "app", Standby: true, IncludeRetained: true,
	})
	if err == nil || extReason(t, err) != "extension_copy_safety_unknown" {
		t.Fatalf("fork copy plan = %v", err)
	}
}

func TestFollowerRemovalReplayPendingKeepsGuardAndRetriesWithoutDrop(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	cfg.Clusters[2].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	installedCalls := 0
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		installedCalls++
		if installedCalls == 1 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	dataDir := cfg.Server.DataDirectory("app_follower_1")
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	oldWriter := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // G304: test-owned path
	})
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldWriter) })
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
		Origins:  map[string]string{"pg_cron": "retained"},
	}); err != nil {
		t.Fatal(err)
	}
	followerSQL := map[string]bool{"plpgsql": true, "pg_cron": true}
	restoreSQL := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return followerSQL, nil
	})
	defer restoreSQL()

	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	_, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_follower_replay_pending" {
		t.Fatalf("lagging removal = %v", err)
	}
	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("the primary DROP did not run first: %v", h.pg.execs)
	}
	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if !slices.Contains(guard.Preloads, "pg_cron") || guard.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("the lagging follower lost its old guard: %+v", guard)
	}

	// Replay catches up. The retry must finish without another DROP and
	// without rewriting the already reduced intent.
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = nil
	}
	h.cfg = cfg
	followerSQL = map[string]bool{"plpgsql": true}
	h.pg.execs = nil
	h.saves = nil
	h.applyCalls = nil
	h.checkStepFn = func(id string) (string, error) {
		if strings.Contains(id, "@app_follower_1") {
			return "the follower copy guard differs", nil
		}
		return "", nil
	}
	req = request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("retry after replay absence = %v", err)
	}
	if len(h.pg.execs) != 0 {
		t.Fatalf("the retry ran another DROP: %v", h.pg.execs)
	}
	if len(h.saves) != 0 {
		t.Fatalf("the retry rewrote converged intent: %d", len(h.saves))
	}
	var applied []string
	for _, opts := range h.applyCalls {
		applied = append(applied, opts.DB)
	}
	if !slices.Contains(applied, "app_follower_1") {
		t.Fatalf("the retry did not refresh the follower: %v", applied)
	}
}
