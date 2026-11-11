package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/tune"
)

func renderCluster(t *testing.T, defs map[string]config.ExtensionDefinition) config.Cluster {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.Extensions = defs
	return cfg
}

func TestExtensionsConfBaselineIsStable(t *testing.T) {
	cfg := renderCluster(t, nil)
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"pg_stat_statements"}}
	first, err := ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	want := "# managed by vaire - do not edit; use cluster.toml\n" +
		"shared_preload_libraries = 'pg_stat_statements,auto_explain'\n" +
		"pg_stat_statements.max = 10000\n" +
		"pg_stat_statements.track = top\n" +
		"auto_explain.log_min_duration = '2s'\n" +
		"auto_explain.sample_rate = 0.1\n" +
		"auto_explain.log_analyze = off\n\n"
	if string(first) != want {
		t.Fatalf("baseline output changed:\n%s\n--- want ---\n%s", first, want)
	}
	second, err := ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatal("rendering is not deterministic")
	}
}

func TestExtensionsConfDefaultsOnly(t *testing.T) {
	cfg := renderCluster(t, nil)
	out, err := ExtensionsConf(cfg, config.ClusterSpec{Name: "app"}, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "shared_preload_libraries = 'pg_stat_statements,auto_explain'") {
		t.Fatalf("defaults not preloaded:\n%s", out)
	}
	if strings.Contains(string(out), "cron.") {
		t.Fatalf("pg_cron settings without pg_cron:\n%s", out)
	}
}

func TestExtensionsConfCustomDefinitionAndOrdering(t *testing.T) {
	defs := map[string]config.ExtensionDefinition{
		"zzz": {
			Source: catalog.SourcePreinstalled, Preload: "zzz_lib",
			Settings: map[string]string{"zzz.mode": "on", "zzz.level": "2"},
		},
		"aaa": {
			Source:   catalog.SourcePreinstalled,
			Settings: map[string]string{"aaa.mode": "'{db}'"},
		},
	}
	cfg := renderCluster(t, defs)
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"zzz", "pg_stat_statements", "aaa"}}
	out, err := ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	wantPreload := "shared_preload_libraries = 'pg_stat_statements,auto_explain,zzz_lib'"
	if !strings.Contains(string(out), wantPreload) {
		t.Fatalf("preload order changed:\n%s", out)
	}
	for _, line := range []string{"aaa.mode = 'app'", "zzz.level = 2", "zzz.mode = on"} {
		if !strings.Contains(string(out), line) {
			t.Errorf("missing %q:\n%s", line, out)
		}
	}
	if strings.Index(string(out), "aaa.mode") > strings.Index(string(out), "zzz.level") {
		t.Fatalf("custom settings are not in canonical name order:\n%s", out)
	}
	again, err := ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(out) {
		t.Fatal("map iteration changed the rendered bytes")
	}
}

func TestExtensionsConfUsesDatabaseNameForFollower(t *testing.T) {
	cfg := renderCluster(t, nil)
	spec := config.ClusterSpec{Name: "app_follower", Follows: "app", Extensions: []string{"pg_cron"}}
	out, err := ExtensionsConf(cfg, spec, tune.Result{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "cron.database_name = 'app'") {
		t.Fatalf("follower rendered its cluster name instead of its database:\n%s", out)
	}
	if strings.Contains(string(out), "app_follower") {
		t.Fatalf("follower cluster name leaked into settings:\n%s", out)
	}
}

func TestExtensionsConfUnknownNameFails(t *testing.T) {
	cfg := renderCluster(t, nil)
	out, err := ExtensionsConf(cfg, config.ClusterSpec{Name: "app", Extensions: []string{"nope"}}, tune.Result{})
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Fatalf("unknown extension was skipped: %v", err)
	}
	if out != nil {
		t.Fatalf("invalid intent produced rendered bytes: %q", out)
	}
}

func TestExtensionsConfInvalidDefinitionFails(t *testing.T) {
	cfg := renderCluster(t, map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Settings: map[string]string{"shared_preload_libraries": "evil"}},
	})
	out, err := ExtensionsConf(cfg, config.ClusterSpec{Name: "app", Extensions: []string{"custom"}}, tune.Result{})
	if err == nil || !strings.Contains(err.Error(), "managed by vaire") {
		t.Fatalf("invalid custom definition was not refused: %v", err)
	}
	if out != nil {
		t.Fatalf("invalid intent produced rendered bytes: %q", out)
	}
}
