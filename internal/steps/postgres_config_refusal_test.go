package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestPostgresConfigUnloadRefusalCarriesObservedState(t *testing.T) {
	restoreRoot := PGConfRootForTest(t.TempDir())
	defer restoreRoot()

	confDir := PGConfDir("app")
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_cron'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abc123"}}
	spec := cfg.Clusters[0]

	_, err := (postgresConfig{}).Instance(spec.Name).Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || refusalReason(err) != "extension_unload_unproven" {
		t.Fatalf("unload refusal = %v", err)
	}

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("unload refusal is not typed: %T", err)
	}

	details, ok := cliErr.Details.(map[string]any)
	if !ok {
		t.Fatalf("details = %#v", cliErr.Details)
	}

	if details["database"] != "app" || details["step"] != postgresConfigID ||
		details["configuration_staged"] != false || details["config_files_written"] != false ||
		details["sql_run"] != false || details["earlier_steps_may_have_run"] != true {
		t.Fatalf("config refusal details = %v", details)
	}
}
