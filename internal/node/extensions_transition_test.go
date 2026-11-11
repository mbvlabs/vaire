package node

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func extFollowerCfg(t *testing.T) config.Cluster {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s"},
		{Name: "app_follower_2", Node: "db1", Port: 5435, Follows: "app", Size: "small", StatementTimeout: "60s"},
		{Name: "app_follower_1", Node: "db1", Port: 5434, Follows: "app", Size: "small", StatementTimeout: "60s"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func defaultExtensionSettingsFake(sql string) ([]map[string]string, error) {
	for key, value := range map[string]string{
		"pg_stat_statements.max":   "10000",
		"pg_stat_statements.track": "top",
	} {
		if strings.Contains(sql, key) {
			return []map[string]string{{"v": value}}, nil
		}
	}
	return nil, nil
}

func cronSettingFake(h *extHarness) {
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		for key, value := range map[string]string{
			"cron.database_name":          "app",
			"cron.use_background_workers": "on",
			"cron.max_running_jobs":       "3",
			"cron.launch_active_jobs":     "off",
			"pg_stat_statements.max":      "10000",
			"pg_stat_statements.track":    "top",
		} {
			if strings.Contains(sql, key) {
				return []map[string]string{{"v": value}}, nil
			}
		}
		return nil, nil
	}
}

func TestExtensionPrimaryReadinessBooleanFacts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		actual   string
		typ      string
		deferred bool
	}{
		{name: "true accepts on", actual: "on", typ: "bool", deferred: false},
		{name: "string stays exact", actual: "on", typ: "string", deferred: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extTestCfg("custom")
			cfg.Extensions = map[string]config.ExtensionDefinition{
				"custom": {
					Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
					Settings: map[string]string{"custom.worker": "true"},
				},
			}
			h := newExtHarness(t, cfg)
			h.pg.queryFn = func(sql string) ([]map[string]string, error) {
				if strings.Contains(sql, "custom.worker") {
					return []map[string]string{{"v": tc.actual, "vartype": tc.typ}}, nil
				}
				return nil, nil
			}
			ext, err := cfg.ExtensionResolver().Resolve("custom")
			if err != nil {
				t.Fatal(err)
			}
			readiness, err := extensionPrimaryReadiness(context.Background(), cfg, cfg.Clusters[0], ext)
			if err != nil {
				t.Fatal(err)
			}
			if readiness.Deferred != tc.deferred {
				t.Fatalf("deferred = %v, want %v: %+v", readiness.Deferred, tc.deferred, readiness)
			}
		})
	}
}

func pendingExtensionTransitionConfig(t *testing.T) config.Cluster {
	t.Helper()
	cfg := extFollowerCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"custom"}
	}
	return cfg
}

func TestTuneApplyRefusesPendingExtensionTransitionBeforeSourceEffects(t *testing.T) {
	cfg := pendingExtensionTransitionConfig(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	h.preloadPending = true
	h.pg.queryFn = defaultExtensionSettingsFake
	oldApply := tuneApply
	applied := 0
	tuneApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		applied++
		return engine.ApplyData{}, nil, nil
	}
	t.Cleanup(func() { tuneApply = oldApply })

	_, err := handleTuneApply(context.Background(), &Env{Cluster: cfg, Req: request("tune.apply", map[string]any{})})
	if err == nil || !strings.Contains(err.Error(), "extension transition") {
		t.Fatalf("tune.apply = %v", err)
	}
	if applied != 0 {
		t.Fatalf("tune.apply reached the engine %d time(s)", applied)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("tune.apply had effects: saves=%d prepare=%d apply=%d exec=%d restart=%d",
			len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs), h.restartCalls)
	}
}

func TestDeferredRestartRefusesPendingExtensionTransition(t *testing.T) {
	cfg := pendingExtensionTransitionConfig(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	h.preloadPending = true
	h.pg.queryFn = defaultExtensionSettingsFake
	oldLoad, oldAcquire, oldApply, oldClear := deferredLoadState, deferredAcquireCluster, deferredApply, deferredClear
	deferredLoadState = func(string) (engine.State, error) {
		return engine.State{DeferredRestarts: map[string]string{"app": "2026-01-01T00:00:00Z"}}, nil
	}
	deferredAcquireCluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	applied, cleared := 0, 0
	deferredApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		applied++
		return engine.ApplyData{}, nil, nil
	}
	deferredClear = func(...string) { cleared++ }
	t.Cleanup(func() {
		deferredLoadState, deferredAcquireCluster, deferredApply, deferredClear = oldLoad, oldAcquire, oldApply, oldClear
	})

	done, failed := runDeferredRestarts(context.Background(), &jobEnv{Cluster: cfg, Job: "maint"})
	if applied != 0 || cleared != 0 || len(done) != 0 || failed["app"] == "" {
		t.Fatalf("deferred restart applied=%d cleared=%d done=%v failed=%v", applied, cleared, done, failed)
	}
	if len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("deferred restart had effects: exec=%d restart=%d", len(h.pg.execs), h.restartCalls)
	}
}

func TestTuneApplySourceWithoutFollowersIsUnchanged(t *testing.T) {
	cfg := extTestCfg("custom")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	newExtHarness(t, cfg)
	oldApply := tuneApply
	applied := 0
	tuneApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		applied++
		return engine.ApplyData{}, nil, nil
	}
	t.Cleanup(func() { tuneApply = oldApply })
	if _, err := handleTuneApply(context.Background(), &Env{Cluster: cfg, Req: request("tune.apply", map[string]any{})}); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Fatalf("source-only tune.apply reached the engine %d time(s), want 1", applied)
	}
}

func TestExtAddUnsafeSQLOnlyMetadataHasZeroEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	req := request("ext.add", map[string]any{"name": "unsafe_sql", "db": "app", "preinstalled": true})
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil {
		t.Fatal("an unsafe SQL-only extension was added")
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("unsafe metadata had effects: saves=%d prepare=%d apply=%d", len(h.saves), h.prepareCalls, len(h.applyCalls))
	}
}

func TestExtAddWithFollowersOrdersFollowersFirst(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
	}
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	res, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("apply order = %v, want %v", order, want)
	}
	if h.prepareCalls != 3 {
		t.Fatalf("package preflight calls = %d, want source and both followers", h.prepareCalls)
	}
	if res.Data.(extAddResult).RequiredConfirmTokens == nil {
		t.Fatal("the result lost its required tokens")
	}
}

func TestExtAddWithFollowersMissingEachTokenHasNoEffect(t *testing.T) {
	for _, missing := range []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
		t.Run(missing, func(t *testing.T) {
			cfg := extFollowerCfg(t)
			h := newExtHarness(t, cfg)
			h.preloadLoaded["pg_cron"] = true
			cronSettingFake(h)
			var confirms []string
			for _, token := range []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
				if token != missing {
					confirms = append(confirms, token)
				}
			}
			req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
			if len(confirms) > 0 {
				req.Confirm = confirms[0]
				req.Confirms = confirms[1:]
			}
			if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || !strings.Contains(err.Error(), missing) {
				t.Fatalf("missing %s = %v", missing, err)
			}
			if len(h.saves) != 0 || len(h.applyCalls) != 0 || h.prepareCalls != 0 {
				t.Fatalf("missing %s mutated: saves=%d applies=%d prepares=%d", missing, len(h.saves), len(h.applyCalls), h.prepareCalls)
			}
		})
	}
}

func TestExtAddWithFollowersStoppedFollowerRefuses(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.runningFn = func(spec config.ClusterSpec) bool { return spec.Name != "app_follower_1" }
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped follower = %v", err)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatal("a stopped follower was mutated")
	}
}

func followerConfigDriftPlan(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
	if len(opts.Only) > 0 {
		if _, cluster := engine.SplitID(opts.Only[0]); cluster == "app" {
			return engine.PlanData{}, nil
		}
	}
	return engine.PlanData{WouldApply: 1}, nil
}

func TestExtAddFollowerUnrenderedConfigDriftRequiresTokenBeforeEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldPlan := extensionPlan
	extensionPlan = followerConfigDriftPlan
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{"pg_trgm": {Name: "pg_trgm", Version: "1.6"}}, nil
	}
	req := request("ext.add", map[string]any{"name": "pg_trgm", "db": "app"})
	req.Confirm = "app/restart"
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || !strings.Contains(err.Error(), "app_follower_1/restart") {
		t.Fatalf("missing follower config-drift token = %v", err)
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("missing token had effects: saves=%d prepare=%d apply=%d exec=%d",
			len(h.saves), h.prepareCalls, len(h.applyCalls), len(h.pg.execs))
	}
	if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
		t.Fatalf("missing token wrote history: %+v, %v", recs.ExtensionRequirements, err)
	}

	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, opts := range h.applyCalls {
		order = append(order, opts.DB)
	}
	if len(order) < 2 || order[0] != "app_follower_1" || order[1] != "app_follower_2" {
		t.Fatalf("add order = %v, want followers first", order)
	}
}

func TestExtRemoveFollowerUnrenderedConfigDriftRequiresTokenBeforeEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_trgm"}
	cfg.Clusters[1].Extensions = []string{"pg_trgm"}
	cfg.Clusters[2].Extensions = []string{"pg_trgm"}
	h := newExtHarness(t, cfg)
	oldPlan := extensionPlan
	extensionPlan = followerConfigDriftPlan
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		// The first invocation is the zero-effect missing-token attempt; the
		// second still sees the extension installed and performs the DROP.
		if call <= 2 {
			return map[string]extensionInstalledVersion{"pg_trgm": {Name: "pg_trgm", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	req := request("ext.remove", map[string]any{"name": "pg_trgm", "db": "app"})
	req.Confirm = "app"
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || !strings.Contains(err.Error(), "app_follower_1/restart") {
		t.Fatalf("missing follower config-drift token = %v", err)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("missing token had effects: saves=%d apply=%d exec=%d", len(h.saves), len(h.applyCalls), len(h.pg.execs))
	}

	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatal(err)
	}
	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("DROP did not run first: execs=%v applies=%d saves=%d installedCalls=%d", h.pg.execs, len(h.applyCalls), len(h.saves), h.installedCalls)
	}
	var order []string
	for _, opts := range h.applyCalls {
		order = append(order, opts.DB)
	}
	if len(order) < 2 || order[0] != "app" {
		t.Fatalf("remove order = %v, want primary first", order)
	}
}

func TestExtRemoveWithFollowersDropsBeforeSavingAndPrimaryFirst(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call == 1 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatal(err)
	}
	if len(h.pg.execs) == 0 || !strings.Contains(h.pg.execs[0], "DROP EXTENSION") {
		t.Fatalf("DROP did not run first: %v", h.pg.execs)
	}
	if len(h.saves) != 1 {
		t.Fatalf("intent saves = %d", len(h.saves))
	}
	var order []string
	for _, opts := range h.applyCalls {
		order = append(order, opts.DB)
	}
	want := []string{"app", "app_follower_1", "app_follower_2"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("remove order = %v, want primary then followers", order)
	}
}

func TestExtensionApplyTransitionSelectionBypassRefused(t *testing.T) {
	cfg := extFollowerCfg(t)
	oldAcquire, oldPlan := extensionAcquireClusters, extensionPlan
	oldRunning := extensionClusterRunning
	extensionClusterRunning = func(config.ClusterSpec) bool { return true }
	extensionAcquireClusters = func(context.Context, []string, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1, RestartPending: []string{"postgres-config-v1@app"}}, nil
	}
	t.Cleanup(func() {
		extensionAcquireClusters, extensionPlan, extensionClusterRunning = oldAcquire, oldPlan, oldRunning
	})
	req := request("apply", map[string]any{"db": "app", "steps": []string{"pgbouncer-v1"}})
	_, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app", Steps: []string{"pgbouncer-v1"}})
	if err == nil || !strings.Contains(err.Error(), "cannot execute") {
		t.Fatalf("step bypass = %v", err)
	}
}

func TestApplyTransitionOrdersFollowersFirst(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldPlan := extensionPlan
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	req := request("apply", map[string]any{"db": "app"})
	warnings, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("apply transition order = %v, want %v", order, want)
	}
	_ = warnings
}

func TestApplyTransitionMissingFollowerRestartTokenStagesPendingWithoutPrimary(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldPlan := extensionPlan
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		_, cluster := engine.SplitID(opts.Only[0])
		return engine.PlanData{WouldApply: 1, RestartPending: []string{"postgres-config-v1@" + cluster}}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") && len(opts.Confirm.Clusters) == 0 {
			return engine.ApplyData{RestartPending: []string{opts.Only[0]}}, nil
		}
		return engine.ApplyData{}, nil
	}
	req := request("apply", map[string]any{"db": "app"})
	_, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if release != nil {
		defer release()
	}
	var pending *extensionApplyPending
	if !errors.As(err, &pending) {
		t.Fatalf("missing follower token = %v, want a pending staging result", err)
	}
	var applied []string
	for _, opts := range h.applyCalls {
		applied = append(applied, opts.DB+":"+opts.Only[0])
	}
	want := []string{
		"app_follower_1:postgres-config-v1@app_follower_1",
		"app_follower_2:postgres-config-v1@app_follower_2",
	}
	if strings.Join(applied, "|") != strings.Join(want, "|") {
		t.Fatalf("staged followers = %v, want %v", applied, want)
	}
	for _, call := range applied {
		if strings.HasPrefix(call, "app:") {
			t.Fatalf("primary configuration or SQL ran while a follower was pending: %v", applied)
		}
	}
	if !slices.Contains(pending.Pending, "app_follower_1") || !slices.Contains(pending.Tokens, "app_follower_1/restart") {
		t.Fatalf("pending result = %+v", pending)
	}
}

func TestExtRemoveRetriesSavedIntentWithSQLAbsent(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters = cfg.Clusters[:1]
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.checkStepFn = func(id string) (string, error) {
		if id == "postgres-config-v1@app" {
			return "differs", nil
		}
		return "", nil
	}
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("a saved-intent retry was refused: %v", err)
	}
	if len(h.applyCalls) != 1 || h.applyCalls[0].Only[0] != "postgres-config-v1@app" {
		t.Fatalf("retry applied %v", h.applyCalls)
	}
}

func TestExtAddStaleMembershipRefusesBeforeEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	other := cfg.Clone()
	other.Clusters = append(other.Clusters, config.ClusterSpec{Name: "app_follower_3", Node: "db1", Port: 5436, Follows: "app", Size: "small", StatementTimeout: "60s"})
	oldLoad := extensionLoad
	calls := 0
	extensionLoad = func() (config.Cluster, []byte, error) {
		calls++
		if calls <= 2 {
			return h.cfg, h.raw, nil
		}
		return other, h.raw, nil
	}
	t.Cleanup(func() { extensionLoad = oldLoad })
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart", "app_follower_3/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "extension_target_changed" {
		t.Fatalf("stale membership = %v", err)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatal("a stale membership was mutated")
	}
}

func TestExtAddFollowGenerationChangeRefuses(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	other := cfg.Clone()
	other.Clusters[1].FollowGeneration = "0123456789abcdef0123456789abcdef"
	oldLoad := extensionLoad
	calls := 0
	extensionLoad = func() (config.Cluster, []byte, error) {
		calls++
		if calls <= 2 {
			return h.cfg, h.raw, nil
		}
		return other, h.raw, nil
	}
	t.Cleanup(func() { extensionLoad = oldLoad })
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || extReason(t, err) != "extension_target_changed" {
		t.Fatalf("changed generation = %v", err)
	}
}

func TestExtAddFailAfterFollowersReportsTruthfulStages(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app" && opts.Only[0] == "postgres-config-v1@app" {
			return engine.ApplyData{}, errors.New("primary config failed")
		}
		return engine.ApplyData{}, nil
	}
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil || extReason(t, err) != "extension_apply_failed" {
		t.Fatalf("primary failure = %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a typed error: %T", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	completed, _ := details["completed"].([]string)
	if len(completed) != 2 || completed[0] != "app_follower_1" || completed[1] != "app_follower_2" {
		t.Fatalf("completed stages = %v", details)
	}
	if len(h.saves) != 1 {
		t.Fatal("the saved intent was rolled back after followers changed")
	}
}

func TestExtAddServerTokenDoesNotAuthorizeFollower(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "prod/restart"
	if _, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil {
		t.Fatal("a broad server restart token authorized a follower restart")
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 {
		t.Fatal("a broad token mutated the target")
	}
}

func TestExtAddDryRunWithFollowersHasNoEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.DryRun = true
	res, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if len(h.saves) != 0 || len(h.applyCalls) != 0 || h.prepareCalls != 0 {
		t.Fatal("a dry run mutated")
	}
	got := res.Data.(extAddResult)
	if !hasToken(got.RequiredConfirmTokens, "app_follower_1/restart") {
		t.Fatalf("dry run tokens = %v", got.RequiredConfirmTokens)
	}
}

func hasToken(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestUnfollowDesiredSQLFailureKeepsRoutingBlocked(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{}).install(t)
	oldReconcile := reconcileCopySQL
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, errors.New("desired SQL failed")
	}
	t.Cleanup(func() { reconcileCopySQL = oldReconcile })
	if _, err := jobUnfollow(context.Background(), unfollowEnv(node.config(), "app_follower")); err == nil {
		t.Fatal("a desired SQL failure was reported as success")
	}
	if _, ok := node.records().Unfollowing["app_follower"]; !ok {
		t.Fatal("the unfollow ownership was dropped after the SQL failure")
	}
	if stub.reached("finish") || stub.resumed {
		t.Fatal("routing was released after the desired SQL failure")
	}
}

func TestExtRemoveWithFollowersMissingEachTokenHasNoEffect(t *testing.T) {
	for _, missing := range []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
		t.Run(missing, func(t *testing.T) {
			cfg := extFollowerCfg(t)
			cfg.Clusters[0].Extensions = []string{"pg_cron"}
			h := newExtHarness(t, cfg)
			h.preloadLoaded["pg_cron"] = true
			cronSettingFake(h)
			h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
				return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
			}
			req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
			req.Confirm = "app"
			for _, token := range []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
				if token != missing {
					req.Confirms = append(req.Confirms, token)
				}
			}
			if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil {
				t.Fatalf("missing %s was accepted", missing)
			}
			if len(h.pg.execs) != 0 || len(h.saves) != 0 || len(h.applyCalls) != 0 {
				t.Fatalf("missing %s mutated: execs=%d saves=%d applies=%d", missing, len(h.pg.execs), len(h.saves), len(h.applyCalls))
			}
		})
	}
}

func TestUnfollowCronSettlementOrder(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	node := newFollowerNode(t, cfg)
	node.copyReady(cfg, "app_follower")
	oldGuardWriter := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // G304: test-owned path
	})
	t.Cleanup(func() { backup.RestoreCopyGuardWriterForTest(oldGuardWriter) })
	stub := (&followerStub{}).install(t)
	oldDeactivate, oldRetarget := deactivateCopyCron, retargetCopyCron
	oldDeactivateAt, oldIdentity := deactivateCopyCronAt, cronIdentity
	oldCurrentName := cronCurrentName
	oldReconcile, oldCheck, oldPrune, oldCronSetting := reconcileCopySQL, checkCopyExtensions, pruneCopyGuard, checkCopyCronSetting
	var order []string
	deactivateCopyCron = func(context.Context, sqlRunner) (cronSettlement, error) {
		order = append(order, "cron_deactivate")
		return cronSettlement{MetadataDB: "app"}, nil
	}
	cronIdentity = func(context.Context, sqlRunner) (cronSettlement, error) {
		return cronSettlement{MetadataDB: "app", MetadataOID: "42"}, nil
	}
	deactivateCopyCronAt = func(context.Context, sqlRunner, *backup.UnfollowCron) (cronSettlement, error) {
		order = append(order, "cron_deactivate")
		return cronSettlement{MetadataDB: "app", MetadataOID: "42"}, nil
	}
	cronCurrentName = func(context.Context, sqlRunner, cronSettlement) (string, error) {
		return "app_follower", nil
	}
	retargetCopyCron = func(context.Context, sqlRunner, cronSettlement, string, string) (int, error) {
		order = append(order, "cron_retarget")
		return 0, nil
	}
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, nil
	}
	checkCopyExtensions = func(context.Context, config.Cluster, config.ClusterSpec, *backup.CopyPlan, sqlRunner, string) ([]output.Warning, error) {
		return nil, nil
	}
	pruneCopyGuard = func(context.Context, config.Cluster, config.ClusterSpec, string, *backup.CopyPlan, bool, string, func() error) ([]output.Warning, error) {
		return nil, nil
	}
	checkCopyCronSetting = func(context.Context, sqlRunner, string, string) error { return nil }
	t.Cleanup(func() {
		deactivateCopyCron, retargetCopyCron = oldDeactivate, oldRetarget
		deactivateCopyCronAt, cronIdentity = oldDeactivateAt, oldIdentity
		cronCurrentName = oldCurrentName
		reconcileCopySQL, checkCopyExtensions, pruneCopyGuard, checkCopyCronSetting = oldReconcile, oldCheck, oldPrune, oldCronSetting
	})
	stub.before = func(stage string) { order = append(order, stage) }
	if _, err := jobUnfollow(context.Background(), unfollowEnv(node.config(), "app_follower")); err != nil {
		t.Fatal(err)
	}
	index := func(name string) int {
		for i, item := range order {
			if item == name {
				return i
			}
		}
		return -1
	}
	promote, deactivate, rename, retarget := index("promote"), index("cron_deactivate"), index("rename"), index("cron_retarget")
	if promote < 0 || deactivate < 0 || rename < 0 || retarget < 0 {
		t.Fatalf("missing stage: %v", order)
	}
	if promote >= deactivate || deactivate >= rename || rename >= retarget {
		t.Fatalf("cron settlement order = %v", order)
	}
}

func TestConfigValidationRefusesFollowerIntentMismatch(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Extensions: []string{"pg_trgm"}},
		{Name: "app_follower", Follows: "app"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "must equal its source") {
		t.Fatalf("generic follower intent mismatch = %v", err)
	}
}

func TestApplyFollowerIntentMismatchRefusesBeforeEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[1].Extensions = []string{"pg_trgm"}
	h := newExtHarness(t, cfg)
	req := request("apply", map[string]any{"db": "app_follower_2"})
	_, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app_follower_2"})
	if err == nil || extReason(t, err) != "follower_extension_intent" {
		t.Fatalf("follower-scoped apply = %v", err)
	}
	if len(h.applyCalls) != 0 {
		t.Fatal("a mismatched follower intent applied")
	}
}

func TestApplySourceStaleFollowerIntentRefusesBeforeEffects(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_trgm"}
	cfg.Clusters[1].Extensions = []string{"citext"}
	h := newExtHarness(t, cfg)
	env := &Env{Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg}
	if _, err := extensionApplyTransitionForApply(context.Background(), env, cfg, "app"); err == nil || extReason(t, err) != "follower_extension_intent" {
		t.Fatalf("stale source/follower intent = %v", err)
	}
	if len(h.applyCalls) != 0 {
		t.Fatal("a stale source/follower intent applied")
	}
}

func TestExtAddPartialRetryEmbedsAllTokens(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app" && opts.Only[0] == "postgres-config-v1@app" {
			return engine.ApplyData{}, errors.New("primary config failed")
		}
		return engine.ApplyData{}, nil
	}
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	_, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil {
		t.Fatal("the failure was reported as success")
	}
	details := extDetails(t, err)
	retry, _ := details["retry"].(string)
	for _, want := range []string{"vaire ext add pg_cron --db app", "app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry %q lacks %q", retry, want)
		}
	}
	if completed, _ := details["completed"].([]string); len(completed) != 2 {
		t.Fatalf("completed = %v", details)
	}
}

func TestExtRemovePartialRetryEmbedsAllTokens(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
	}
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app" {
			return engine.ApplyData{}, errors.New("primary config failed")
		}
		return engine.ApplyData{}, nil
	}
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	_, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg})
	if err == nil {
		t.Fatal("the failure was reported as success")
	}
	details := extDetails(t, err)
	retry, _ := details["retry"].(string)
	for _, want := range []string{"vaire ext remove pg_cron --db app --confirm app", "app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry %q lacks %q", retry, want)
		}
	}
}

func TestExtAddWithFollowersUpdatesAfterFollowersAndPrimarySQL(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	var order []string
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		order = append(order, "apply:"+opts.DB+":"+opts.Only[0])
		return engine.ApplyData{}, nil
	}
	h.pg.execFn = func(sql string) {
		if strings.Contains(sql, "ALTER EXTENSION") {
			order = append(order, "update")
		}
	}
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call >= 3 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6", Default: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.5", Default: "1.6"}}, nil
	}
	h.updatePath = true
	req := request("ext.add", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower_1/restart", "app_follower_2/restart"}
	res, err := handleExtAdd(context.Background(), &Env{Req: req, Cluster: cfg})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Data.(extAddResult)
	if !got.Updated || got.Version != "1.6" || got.PreviousVersion != "1.5" {
		t.Fatalf("update result = %+v", got)
	}
	roles := -1
	for i, item := range order {
		if item == "apply:app:postgres-roles-v1@app" {
			roles = i
		}
	}
	update := -1
	for i, item := range order {
		if item == "update" {
			update = i
		}
	}
	if roles < 0 || update < 0 || update < roles {
		t.Fatalf("update order = %v", order)
	}
	for i, item := range order {
		if strings.HasPrefix(item, "apply:app_follower") && i > roles {
			t.Fatalf("a follower was configured after primary SQL: %v", order)
		}
	}
}

func TestApplyFullMultipleSourcesStagesAvailableTargetsAndDefersUnconfirmedSource(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s"},
		{Name: "app_follower", Node: "db1", Port: 5434, Follows: "app", Size: "small", StatementTimeout: "60s"},
		{Name: "other", Node: "db1", Port: 5435, Stanza: "other-def456", Size: "small", StatementTimeout: "60s"},
		{Name: "other_follower", Node: "db1", Port: 5436, Follows: "other", Size: "small", StatementTimeout: "60s"},
		{Name: "plain", Node: "db1", Port: 5437, Stanza: "plain-aaa111", Size: "small", StatementTimeout: "60s"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	h := newExtHarness(t, cfg)
	oldPlan, oldAcquire := extensionPlan, extensionAcquireClusters
	var acquired []string
	extensionAcquireClusters = func(_ context.Context, names []string, _ string, _ string, _ time.Duration) (engine.Release, error) {
		acquired = append([]string(nil), names...)
		return func() {}, nil
	}
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") {
			_, cluster := engine.SplitID(opts.Only[0])
			return engine.PlanData{WouldApply: 1, RestartPending: []string{"postgres-config-v1@" + cluster}}, nil
		}
		return engine.PlanData{WouldApply: 1}, nil
	}
	t.Cleanup(func() { extensionPlan, extensionAcquireClusters = oldPlan, oldAcquire })
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") && len(opts.Confirm.Clusters) == 0 {
			return engine.ApplyData{RestartPending: []string{opts.Only[0]}}, nil
		}
		return engine.ApplyData{}, nil
	}
	req := request("apply", nil)
	req.Confirm = "app/restart"
	req.Confirms = []string{"app_follower/restart", "other/restart"} // other_follower/restart is missing
	_, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{})
	var pending *extensionApplyPending
	if !errors.As(err, &pending) {
		t.Fatalf("a missing token = %v, want pending staging", err)
	}
	var staged []string
	for _, opts := range h.applyCalls {
		staged = append(staged, opts.DB+":"+opts.Only[0])
	}
	wantStaged := []string{
		"app_follower:postgres-config-v1@app_follower",
		"app:postgres-config-v1@app",
		"app:postgres-roles-v1@app",
		"other_follower:postgres-config-v1@other_follower",
	}
	if strings.Join(staged, "|") != strings.Join(wantStaged, "|") {
		t.Fatalf("staged targets = %v, want %v", staged, wantStaged)
	}
	for _, call := range staged {
		if strings.HasPrefix(call, "other:") {
			t.Fatalf("the source with a pending follower ran primary configuration/SQL: %v", staged)
		}
	}
	if len(h.saves) != 0 || h.prepareCalls != 0 || len(h.pg.execs) != 0 || h.restartCalls != 0 {
		t.Fatalf("deferred staging had unexpected effects: saves=%d prepare=%d exec=%d restart=%d", len(h.saves), h.prepareCalls, len(h.pg.execs), h.restartCalls)
	}
	if recs, err := backup.LoadRecords(); err != nil || len(recs.ExtensionRequirements) != 0 {
		t.Fatalf("deferred staging wrote extension history: %+v, %v", recs.ExtensionRequirements, err)
	}
	if strings.Join(acquired, ",") != "app,app_follower,other,other_follower" {
		t.Fatalf("locked target set = %v", acquired)
	}
	if !slices.Contains(pending.Pending, "other_follower") || !slices.Contains(pending.Tokens, "other_follower/restart") {
		t.Fatalf("pending result = %+v", pending)
	}
	// With every token, every selected source and follower is ordered, and the
	// unrelated primary is never touched.
	h.applyCalls = nil
	req = request("apply", nil)
	req.Confirms = []string{"app/restart", "app_follower/restart", "other/restart", "other_follower/restart"}
	if _, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{}); err != nil {
		t.Fatal(err)
	} else if release != nil {
		defer release()
	}
	var order []string
	for _, opts := range h.applyCalls {
		order = append(order, opts.DB+":"+opts.Only[0])
	}
	want := []string{
		"app_follower:postgres-config-v1@app_follower",
		"app:postgres-config-v1@app",
		"app:postgres-roles-v1@app",
		"other_follower:postgres-config-v1@other_follower",
		"other:postgres-config-v1@other",
		"other:postgres-roles-v1@other",
	}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("multi-source order = %v, want %v", order, want)
	}
}

func TestApplyPartialFailureRetryEmbedsAllTokensAndProgress(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	oldPlan := extensionPlan
	extensionPlan = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-config-v1") {
			_, cluster := engine.SplitID(opts.Only[0])
			return engine.PlanData{WouldApply: 1, RestartPending: []string{"postgres-config-v1@" + cluster}}, nil
		}
		return engine.PlanData{WouldApply: 1}, nil
	}
	t.Cleanup(func() { extensionPlan = oldPlan })
	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if opts.DB == "app_follower_2" {
			return engine.ApplyData{}, errors.New("follower config failed")
		}
		return engine.ApplyData{}, nil
	}
	req := request("apply", nil)
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	_, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{})
	if release != nil {
		defer release()
	}
	if err == nil {
		t.Fatal("a follower failure was reported as success")
	}
	details := extDetails(t, err)
	retry, _ := details["retry"].(string)
	for _, want := range []string{"vaire apply --db app", "app/restart", "app_follower_1/restart", "app_follower_2/restart"} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry %q lacks %q", retry, want)
		}
	}
	if completed, _ := details["completed"].([]string); !slices.Equal(completed, []string{"app_follower_1"}) {
		t.Fatalf("completed = %v, want [app_follower_1]", details["completed"])
	}
	if pending, _ := details["pending"].([]string); !slices.Equal(pending, []string{"app_follower_2"}) {
		t.Fatalf("pending = %v, want [app_follower_2]", details["pending"])
	}
}

func TestExtRemoveRetryFinishesFollowersAfterPrimaryConverged(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	h.checkStepFn = func(id string) (string, error) {
		if strings.Contains(id, "@app_follower_1") {
			return "differs", nil
		}
		return "", nil
	}
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart", "app_follower_1/restart", "app_follower_2/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatalf("the advertised retry refused the follower-only work: %v", err)
	}
	if len(h.pg.execs) != 0 {
		t.Fatalf("the retry ran DROP again: %v", h.pg.execs)
	}
	if len(h.saves) != 0 {
		t.Fatalf("the retry rewrote converged intent: %v", h.saves)
	}
	var applied []string
	for _, opts := range h.applyCalls {
		applied = append(applied, opts.DB)
	}
	want := []string{"app", "app_follower_1", "app_follower_2"}
	if strings.Join(applied, ",") != strings.Join(want, ",") {
		t.Fatalf("retry applied %v, want %v", applied, want)
	}
}

func TestUnfollowCronRetryResumesFromRecordedIdentity(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	node := newFollowerNode(t, cfg)
	node.copyReady(cfg, "app_follower")
	stub := (&followerStub{}).install(t)
	oldIdentity, oldDeactivate, oldCurrent, oldRetarget := cronIdentity, deactivateCopyCronAt, cronCurrentName, retargetCopyCron
	oldReconcile, oldCheck, oldPrune, oldCronSetting := reconcileCopySQL, checkCopyExtensions, pruneCopyGuard, checkCopyCronSetting
	t.Cleanup(func() {
		cronIdentity, deactivateCopyCronAt, cronCurrentName, retargetCopyCron = oldIdentity, oldDeactivate, oldCurrent, oldRetarget
		reconcileCopySQL, checkCopyExtensions, pruneCopyGuard, checkCopyCronSetting = oldReconcile, oldCheck, oldPrune, oldCronSetting
	})
	cronIdentity = func(context.Context, sqlRunner) (cronSettlement, error) {
		return cronSettlement{MetadataDB: "app", MetadataOID: "42"}, nil
	}
	var deactivatedOIDs []string
	deactivateCopyCronAt = func(_ context.Context, _ sqlRunner, recorded *backup.UnfollowCron) (cronSettlement, error) {
		if recorded != nil {
			deactivatedOIDs = append(deactivatedOIDs, recorded.MetadataOID)
		}
		return cronSettlement{MetadataDB: "app_follower", MetadataOID: "42"}, nil
	}
	cronCurrentName = func(context.Context, sqlRunner, cronSettlement) (string, error) { return "app_follower", nil }
	var retargeted []string
	retargetCopyCron = func(_ context.Context, _ sqlRunner, s cronSettlement, oldDB, newDB string) (int, error) {
		retargeted = append(retargeted, s.MetadataDB+"->"+newDB)
		return 1, nil
	}
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, nil
	}
	checkCopyExtensions = func(context.Context, config.Cluster, config.ClusterSpec, *backup.CopyPlan, sqlRunner, string) ([]output.Warning, error) {
		return nil, nil
	}
	pruneCopyGuard = func(context.Context, config.Cluster, config.ClusterSpec, string, *backup.CopyPlan, bool, string, func() error) ([]output.Warning, error) {
		return nil, nil
	}
	checkCopyCronSetting = func(context.Context, sqlRunner, string, string) error { return nil }

	stub.failAt, stub.failErr = "rename", errors.New("stop immediately after rename")
	if _, err := jobUnfollow(context.Background(), unfollowEnv(node.config(), "app_follower")); err == nil {
		t.Fatal("the interrupted unfollow was reported as success")
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	u, ok := recs.Unfollowing["app_follower"]
	if !ok || u.Cron == nil || !u.Cron.Deactivated || u.Cron.MetadataOID != "42" {
		t.Fatalf("cron checkpoint = %+v", u)
	}
	if f, ok := recs.ActivationFenceFor("app_follower"); !ok || !f.Active() {
		t.Fatalf("interrupted unfollow left no active fence: %+v", f)
	}

	stub.failAt, stub.failErr = "", nil
	if _, err := jobUnfollow(context.Background(), unfollowEnv(node.config(), "app_follower")); err != nil {
		t.Fatalf("the retry did not finish: %v", err)
	}
	if len(deactivatedOIDs) != 2 || deactivatedOIDs[1] != "42" {
		t.Fatalf("retry deactivations = %v", deactivatedOIDs)
	}
	if len(retargeted) != 1 || retargeted[0] != "app_follower->app_follower" {
		t.Fatalf("retargets = %v", retargeted)
	}
	recs, err = backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := recs.Unfollowing["app_follower"]; ok {
		t.Fatal("the finished retry left its unfollow record")
	}
	if _, ok := recs.ActivationFenceFor("app_follower"); ok {
		t.Fatal("the finished retry left its activation fence")
	}
}

func TestSourceCronChangeDoesNotSettleCopy(t *testing.T) {
	cfg := extTestCfg("pg_cron")
	h := newExtHarness(t, cfg)
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.installedFn = func(call int) (map[string]extensionInstalledVersion, error) {
		if call <= 1 {
			return map[string]extensionInstalledVersion{"pg_cron": {Name: "pg_cron", Version: "1.6"}}, nil
		}
		return map[string]extensionInstalledVersion{}, nil
	}
	oldDeactivate, oldRetarget := deactivateCopyCronAt, retargetCopyCron
	calls := 0
	deactivateCopyCronAt = func(context.Context, sqlRunner, *backup.UnfollowCron) (cronSettlement, error) {
		calls++
		return cronSettlement{}, nil
	}
	retargetCopyCron = func(context.Context, sqlRunner, cronSettlement, string, string) (int, error) {
		calls++
		return 0, nil
	}
	t.Cleanup(func() { deactivateCopyCronAt, retargetCopyCron = oldDeactivate, oldRetarget })
	req := request("ext.remove", map[string]any{"name": "pg_cron", "db": "app"})
	req.Confirm = "app"
	req.Confirms = []string{"app/restart"}
	if _, err := handleExtRemove(context.Background(), &Env{Req: req, Cluster: cfg}); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("a source extension change settled a copy's cron: %d calls", calls)
	}
}
