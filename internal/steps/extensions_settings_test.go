package steps

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/tune"
)

func followerSettingsTestPlan(t *testing.T, cfg config.Cluster, follower config.ClusterSpec) FollowerRuntimePlan {
	t.Helper()
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true}, nil
	})
	t.Cleanup(restore)
	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func TestPlanFollowerRuntimeExpectedSettingsOnlyExtensionOwnedKeys(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:    map[string]string{"addon.mode": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"addon"}
	}
	follower = cfg.Clusters[1]
	cfg.Postgres.Settings = map[string]string{
		"addon.mode":          "off",
		"random_page_cost":    "1.10",
		"hash_mem_multiplier": "2.0",
		"client_min_messages": "WARNING",
	}
	follower.Settings = map[string]string{"addon.mode": "on", "work_mem": "8MB"}

	plan := followerSettingsTestPlan(t, cfg, follower)
	if got := plan.ExpectedSettings["addon.mode"]; got != "on" {
		t.Fatalf("extension-owned explicit setting = %q, want the follower's on: %+v", got, plan.ExpectedSettings)
	}
	for _, unrelated := range []string{"random_page_cost", "hash_mem_multiplier", "client_min_messages", "work_mem"} {
		if _, ok := plan.ExpectedSettings[unrelated]; ok {
			t.Fatalf("unrelated setting %s became an extension dependency: %+v", unrelated, plan.ExpectedSettings)
		}
	}
}

func TestPlanFollowerRuntimeExpectedSettingsFollowRenderedSelection(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)

	plan := followerSettingsTestPlan(t, cfg, follower)
	if len(plan.ExpectedSettings) != 0 {
		t.Fatalf("empty extensions acquired implicit default settings: %+v", plan.ExpectedSettings)
	}
	rendered, err := render.ExtensionsConf(cfg, follower, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rendered), "pg_stat_statements.max") {
		t.Fatalf("the test premise is stale: the renderer wrote pg_stat_statements settings:\n%s", rendered)
	}

	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings:    map[string]string{"custom.mode": "on"},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"custom"}
	}
	follower = cfg.Clusters[1]
	plan = followerSettingsTestPlan(t, cfg, follower)
	if got := plan.ExpectedSettings["custom.mode"]; got != "on" {
		t.Fatalf("custom-only expected settings = %+v", plan.ExpectedSettings)
	}
	if _, ok := plan.ExpectedSettings["pg_stat_statements.max"]; ok {
		t.Fatalf("implicit pg_stat_statements defaults returned with custom-only intent: %+v", plan.ExpectedSettings)
	}
	rendered, err = render.ExtensionsConf(cfg, follower, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rendered), "custom.mode = on") {
		t.Fatalf("rendered configuration lacks the declared custom setting:\n%s", rendered)
	}
	if !strings.Contains(string(rendered), "shared_preload_libraries = 'pg_stat_statements,auto_explain") {
		t.Fatalf("rendered configuration lacks the implicit runtime preloads:\n%s", rendered)
	}
	if strings.Contains(string(rendered), "pg_stat_statements.max") {
		t.Fatalf("the renderer wrote implicit settings the plan does not expect:\n%s", rendered)
	}
}

func TestExtensionSettingMatchesPostgresTypes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  string
		fact  ExtensionSettingFact
		match bool
	}{
		{name: "real 1.10 equals 1.1", want: "1.10", fact: ExtensionSettingFact{Value: "1.1", Type: "real"}, match: true},
		{name: "real mismatch", want: "1.10", fact: ExtensionSettingFact{Value: "1.2", Type: "real"}, match: false},
		{name: "integer canonical", want: "010000", fact: ExtensionSettingFact{Value: "10000", Type: "integer"}, match: true},
		{name: "enum case", want: "ON", fact: ExtensionSettingFact{Value: "on", Type: "enum"}, match: true},
		{name: "enum mismatch", want: "OFF", fact: ExtensionSettingFact{Value: "on", Type: "enum"}, match: false},
		{name: "unit suffix equivalence", want: "1MB", fact: ExtensionSettingFact{Value: "1024kB", Type: "integer", Unit: "kB"}, match: true},
		{name: "plain value uses base unit", want: "1024", fact: ExtensionSettingFact{Value: "1MB", Type: "integer", Unit: "kB"}, match: true},
		{name: "block unit plain value", want: "16384", fact: ExtensionSettingFact{Value: "128MB", Type: "integer", Unit: "8kB"}, match: true},
		{name: "time unit", want: "2s", fact: ExtensionSettingFact{Value: "2000ms", Type: "integer", Unit: "ms"}, match: true},
		{name: "unknown unit stays exact", want: "1wa", fact: ExtensionSettingFact{Value: "1wa", Type: "integer", Unit: "wa"}, match: true},
		{name: "unknown unit mismatch stays exact", want: "1wa", fact: ExtensionSettingFact{Value: "2wa", Type: "integer", Unit: "wa"}, match: false},
		{name: "string stays case-sensitive", want: "ON", fact: ExtensionSettingFact{Value: "on", Type: "string"}, match: false},
		{name: "quoted literal content stays distinct", want: "'on'", fact: ExtensionSettingFact{Value: "on", Type: "string"}, match: false},
		{name: "quoted literal content matches itself", want: "'on'", fact: ExtensionSettingFact{Value: "'on'", Type: "string"}, match: true},
		{name: "bool prefix", want: "tru", fact: ExtensionSettingFact{Value: "on", Type: "bool"}, match: true},
		{name: "ambiguous o is not bool", want: "o", fact: ExtensionSettingFact{Value: "on", Type: "bool"}, match: false},
		{name: "empty type stays exact", want: "on", fact: ExtensionSettingFact{Value: "on"}, match: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExtensionSettingMatches(tc.want, tc.fact); got != tc.match {
				t.Fatalf("ExtensionSettingMatches(%q, %+v) = %v, want %v", tc.want, tc.fact, got, tc.match)
			}
		})
	}
}

func TestPlanFollowerRuntimeStoppedClassSkipsCatalogButKeepsGuard(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	cfg.Clusters[1].Extensions = []string{"pg_cron"}
	follower = cfg.Clusters[1]
	installedCalls := 0
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		installedCalls++
		return nil, context.Canceled
	})
	t.Cleanup(restore)

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeStopped})
	if err != nil {
		t.Fatal(err)
	}
	if installedCalls != 0 {
		t.Fatalf("stopped planning read the unavailable catalog %d times", installedCalls)
	}
	if plan.SQLFactsComplete {
		t.Fatal("stopped planning claimed complete SQL facts")
	}
	if got := plan.ExpectedSettings["cron.database_name"]; got != "app" {
		t.Fatalf("stopped planning lost the current ordinary setting: %+v", plan.ExpectedSettings)
	}
	if plan.ExpectedSettings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("stopped planning lost the mandatory copy overlay: %+v", plan.ExpectedSettings)
	}
	if !slices.Contains(plan.Preloads, "pg_cron") {
		t.Fatalf("stopped planning lost the desired preload: %v", plan.Preloads)
	}
}

func TestPlanFollowerRuntimeExpectedSettingsExtensionOwnedTypes(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"typed": {
			Source: catalog.SourcePreinstalled, ExtName: "typed_ext",
			Settings: map[string]string{
				"custom.ratio": "'1.10'",
				"custom.mode":  "'ON'",
				"custom.size":  "'1MB'",
			},
			CopySafety:  stringPtr(catalog.CopySafetySafe),
			StandbySafe: boolPtr(true),
		},
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"typed"}
	}
	follower = cfg.Clusters[1]

	plan := followerSettingsTestPlan(t, cfg, follower)
	for key, want := range map[string]string{"custom.ratio": "1.10", "custom.mode": "ON", "custom.size": "1MB"} {
		if plan.ExpectedSettings[key] != want {
			t.Fatalf("%s = %q, want %q (all: %+v)", key, plan.ExpectedSettings[key], want, plan.ExpectedSettings)
		}
	}
	for key, fact := range map[string]ExtensionSettingFact{
		"custom.ratio": {Value: "1.1", Type: "real"},
		"custom.mode":  {Value: "on", Type: "enum"},
		"custom.size":  {Value: "1024kB", Type: "integer", Unit: "kB"},
	} {
		if !ExtensionSettingMatches(plan.ExpectedSettings[key], fact) {
			t.Fatalf("%s did not match its canonical PostgreSQL value: want %q fact %+v", key, plan.ExpectedSettings[key], fact)
		}
	}
	if !slices.ContainsFunc(plan.Requirements, func(ext catalog.Extension) bool { return ext.Name == "typed" }) {
		t.Fatalf("typed extension missing from the plan requirements: %+v", plan.Requirements)
	}
}
