package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/spf13/cobra"
)

func cliExtCfg(extensions ...string) config.Cluster {
	if len(extensions) == 0 {
		extensions = []string{"pg_stat_statements"}
	}
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small",
		StatementTimeout: "60s", Extensions: extensions,
	}}
	return cfg
}

func TestExtCommandMetadata(t *testing.T) {
	root := NewRoot()
	if report := checkCommandMetadata(root); !report.OK {
		t.Fatalf("metadata: %v", report.Failures)
	}
	for path, want := range map[string]struct {
		mutating, dry, destructive bool
		confirm                    string
		needsRestart               string
	}{
		"ext add":    {mutating: true, dry: true, confirm: "{db}/restart", needsRestart: "maybe"},
		"ext remove": {mutating: true, dry: true, destructive: true, confirm: "{db}", needsRestart: "maybe"},
		"ext list":   {needsRestart: "never"},
	} {
		cmd, _, err := root.Find(strings.Split(path, " "))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		meta, ok := metaFor(cmd)
		if !ok {
			t.Fatalf("%s has no metadata", path)
		}
		if meta.Mutating != want.mutating || meta.SupportsDryRun != want.dry || meta.Destructive != want.destructive ||
			meta.ConfirmToken != want.confirm || meta.NeedsRestart != want.needsRestart {
			t.Errorf("%s meta = %+v", path, meta)
		}
	}
}

func TestExtListLines(t *testing.T) {
	data := extListData{
		Extensions: []extListEntry{{
			Name: "vector", ExtName: "vector", Source: "apt", Package: "postgresql-18-pgvector",
			PackageInstalled: true, PackageKnown: true, Available: true, AvailableKnown: true, DefaultVersion: "0.9.0",
			Databases: []extDatabaseState{{DB: "app", Available: true, Enabled: true, Installed: true, Version: "0.8.0", DefaultVersion: "0.9.0", UpdateAvailable: true, PreloadKnown: true}},
		}},
		Databases: []extListDatabase{
			{DB: "app", State: "running", Available: true, PreloadKnown: true, Unmanaged: []extInstalledExtension{{Name: "mystery", Version: "2.0"}}},
			{DB: "stopped", State: "stopped", Available: false, Error: "the cluster is stopped"},
		},
	}
	out := strings.Join(extListLines(data), "\n")
	for _, want := range []string{"NAME", "vector", "postgresql-18-pgvector", "0.9.0", "0.8.0", "update to 0.9.0", "mystery 2.0 (unmanaged)", "stopped, installed extensions are unavailable"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
}

func TestExtMessages(t *testing.T) {
	if got := extAddMessage(extAddResult{Extension: "vector", Database: "app", Version: "0.9.0", Package: "postgresql-18-pgvector", Restarted: true}); !strings.Contains(got, "added vector 0.9.0 to app") || !strings.Contains(got, "restarted app") {
		t.Errorf("add message = %q", got)
	}
	if got := extAddMessage(extAddResult{Extension: "vector", Database: "app", PreviousVersion: "0.8.0", Version: "0.9.0", Updated: true}); !strings.Contains(got, "updated vector in app from 0.8.0 to 0.9.0") {
		t.Errorf("update message = %q", got)
	}
	if got := extRemoveMessage(extRemoveResult{Extension: "vector", Database: "app", Cascade: true, Dependents: []string{"x"}, DependentsTotal: 2, PackageKept: "postgresql-18-pgvector"}); !strings.Contains(got, "removed vector from app") || !strings.Contains(got, "2 dependent objects") || !strings.Contains(got, "package postgresql-18-pgvector stays installed") {
		t.Errorf("remove message = %q", got)
	}
}

func TestExtDBForAdd(t *testing.T) {
	cfg := cliExtCfg()
	if db, err := extDBForAdd(cfg, ""); err != nil || db != "app" {
		t.Fatalf("one database = %q, %v", db, err)
	}
	two := cfg.Clone()
	two.Clusters = append(two.Clusters, config.ClusterSpec{Name: "other", Port: 5434, Size: "small", StatementTimeout: "60s"})
	if _, err := extDBForAdd(two, ""); err == nil || !strings.Contains(err.Error(), "needs --db") {
		t.Fatalf("two databases = %v", err)
	}
	follower := cfg.Clone()
	follower.Clusters[0].Follows = "other"
	if _, err := extDBForAdd(follower, ""); err == nil || !strings.Contains(err.Error(), "no database") {
		t.Fatalf("follower only = %v", err)
	}
}

func TestExtAddRemoveLikelyRestart(t *testing.T) {
	cfg := cliExtCfg()
	if extAddLikelyNeedsRestart(cfg, "app", "pg_trgm", "", map[string]any{}) {
		t.Fatal("pg_trgm was predicted to need a restart")
	}
	if !extAddLikelyNeedsRestart(cfg, "app", "pg_cron", "", map[string]any{}) {
		t.Fatal("pg_cron was not predicted to need a restart")
	}
	if !extAddLikelyNeedsRestart(cfg, "app", "custom", "custom_lib", map[string]any{}) {
		t.Fatal("a custom preload was not predicted to need a restart")
	}
	if !extAddLikelyNeedsRestart(cfg, "app", "custom", "", map[string]any{"settings": map[string]string{"custom.mode": "on"}}) {
		t.Fatal("custom settings were not predicted to need a restart")
	}
	enabled := cliExtCfg("pg_stat_statements", "pg_trgm")
	if extAddLikelyNeedsRestart(enabled, "app", "pg_trgm", "", map[string]any{}) {
		t.Fatal("an already enabled extension without preload was predicted to need a restart")
	}
	if extAddLikelyNeedsRestart(enabled, "app", "pg_stat_statements", "", map[string]any{}) {
		t.Fatal("an already enabled default module was predicted to need a restart")
	}
	if !extAddLikelyNeedsRestart(cliExtCfg("pg_stat_statements", "pg_cron"), "app", "pg_cron", "", map[string]any{}) {
		t.Fatal("an already enabled preload extension was not predicted to need a restart")
	}
	cronCfg := cliExtCfg("pg_stat_statements", "pg_cron")
	if restart, err := extRemoveLikelyNeedsRestart(cronCfg, "app", "pg_cron"); err != nil || !restart {
		t.Fatalf("pg_cron remove restart = %v, %v", restart, err)
	}
	if restart, err := extRemoveLikelyNeedsRestart(cfg, "app", "pg_trgm"); err != nil || restart {
		t.Fatalf("pg_trgm remove restart = %v, %v", restart, err)
	}
	settingsCfg := cliExtCfg("pg_stat_statements", "pg_trgm")
	settingsCfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_trgm": {Settings: map[string]string{"max_files_per_process": "2000"}},
	}
	if restart, err := extRemoveLikelyNeedsRestart(settingsCfg, "app", "pg_trgm"); err != nil || !restart {
		t.Fatalf("pg_trgm settings remove restart = %v, %v", restart, err)
	}
}

func TestExtStandbySafeParamExplicitFalse(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().Bool("standby-safe", false, "")
	if _, supplied := extStandbySafeParam(cmd); supplied {
		t.Fatal("an absent standby-safe flag was reported as supplied")
	}
	if err := cmd.Flags().Set("standby-safe", "false"); err != nil {
		t.Fatal(err)
	}
	value, supplied := extStandbySafeParam(cmd)
	if !supplied || value {
		t.Fatalf("explicit false = %v, supplied=%v", value, supplied)
	}
	cmd = &cobra.Command{Use: "test"}
	cmd.Flags().Bool("standby-safe", false, "")
	if err := cmd.Flags().Set("standby-safe", "true"); err != nil {
		t.Fatal(err)
	}
	value, supplied = extStandbySafeParam(cmd)
	if !supplied || !value {
		t.Fatalf("explicit true = %v, supplied=%v", value, supplied)
	}
}

func TestExtListLinesSource(t *testing.T) {
	data := extListData{Extensions: []extListEntry{{
		Name: "custom", Source: "preinstalled", PackageInstalled: true, PackageKnown: true,
		Available: true, AvailableKnown: true, DefaultVersion: "1.0",
	}}}
	out := strings.Join(extListLines(data), "\n")
	if !strings.Contains(out, "preinstalled") {
		t.Fatalf("source not rendered:\n%s", out)
	}
}

func TestExtFlagMap(t *testing.T) {
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().StringArray("setting", nil, "")
	if err := cmd.Flags().Set("setting", "a=1"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("setting", "b = 'two words'"); err != nil {
		t.Fatal(err)
	}
	got, err := extFlagMap(cmd, "setting")
	if err != nil {
		t.Fatal(err)
	}
	if got["a"] != "1" || got["b"] != "'two words'" {
		t.Fatalf("settings = %+v", got)
	}
	if err := cmd.Flags().Set("setting", "a=2"); err != nil {
		t.Fatal(err)
	}
	if _, err := extFlagMap(cmd, "setting"); err == nil {
		t.Fatal("a conflicting duplicate was accepted")
	}
}
