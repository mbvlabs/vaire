package steps

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// dpkg reports ${Status} as "<selection> <flag> <state>". vaire holds what it
// installs, so "hold ok installed" is a healthy package.
func TestPackageInstalled(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		want         bool
	}{
		{"held installed", "hold ok installed", true},
		{"unheld installed", "install ok installed", true},
		{"triggers awaited", "hold ok triggers-awaited", false},
		{"unconfigured", "install ok half-configured", false},
		{"unpacked", "hold ok unpacked", false},
		{"half installed", "install ok half-installed", false},
		{"removed with config", "deinstall ok config-files", false},
		{"not installed", "purge ok not-installed", false},
		{"broken reinstall required", "install reinstreq installed", false},
		{"missing output", "", false},
		{"query error", "dpkg-query: error: unable to access the dpkg database", false},
		{"not-installed is not installed", "install ok not-installed", false},
		{"extra field", "install ok installed extra", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := PackageInstalled(tc.status); got != tc.want {
				t.Fatalf("PackageInstalled(%q) = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

// Apply installs and holds postgresql-18 and pgbackrest; a host it configured
// is complete even though dpkg reports their selection as "hold".
func TestPostgresPackagesCheckAcceptsHeldPackages(t *testing.T) {
	cfg := config.DefaultCluster()
	oldProbe := packageProbe

	t.Cleanup(func() { packageProbe = oldProbe })

	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			return "hold ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n"), nil
		}

		return "", nil
	}

	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("held packages = %+v, %v", res, err)
	}

	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			if args[len(args)-1] == "pgbackrest" {
				return "", errors.New("dpkg-query: no packages found matching pgbackrest")
			}

			return "hold ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n"), nil
		}

		return "", nil
	}

	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "pgbackrest") {
		t.Fatalf("query error = %+v, %v", res, err)
	}
}

// pg18PackageProbe models a converged PG18 host: every package is installed
// and held except the named ones, which dpkg reports as not installed.
func pg18PackageProbe(missing ...string) func(context.Context, string, ...string) (string, error) {
	absent := map[string]bool{}
	for _, pkg := range missing {
		absent[pkg] = true
	}

	return func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			if absent[args[len(args)-1]] {
				return "unknown ok not-installed", nil
			}

			return "hold ok installed", nil
		case "apt-mark":
			return strings.Join(pgHold, "\n"), nil
		}

		return "", nil
	}
}

// Debian 13's PGDG publishes no postgresql-contrib binary for PostgreSQL 18:
// postgresql-18 Provides postgresql-contrib-18 and ships the contrib control
// files and libraries. A journal-completed apply must stay complete on the
// next plan; requiring the virtual package would reapply forever.
func TestPostgresPackagesPG18ContribVirtualConverges(t *testing.T) {
	extensionTestDirs(t)

	for _, name := range []string{"pg_trgm", "pg_stat_statements"} {
		touchExtensionControl(t, name)
	}

	for _, name := range []string{"pg_trgm", "pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}

	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"pg_trgm"}}}

	oldScript := runExtensionPackageScript
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		return nil
	}

	t.Cleanup(func() { runExtensionPackageScript = oldScript })

	oldProbe := packageProbe
	packageProbe = pg18PackageProbe("postgresql-contrib")

	t.Cleanup(func() { packageProbe = oldProbe })

	if err := installPostgresPackages(context.Background(), cfg, postgresPackagesID, engine.Runtime{}, nil); err != nil {
		t.Fatalf("apply: %v", err)
	}

	inputs, err := (postgresPackages{}).Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}

	journal := filepath.Join(t.TempDir(), "journal.json")

	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		postgresPackagesID: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(postgresPackagesID, inputs)},
	}}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}

	plan, err := engine.Plan(context.Background(), []engine.Step{postgresPackages{}}, cfg, engine.Runtime{}, engine.Options{JournalPath: journal})
	if err != nil || plan.WouldApply != 0 {
		t.Fatalf("plan after apply = %+v, %v; want no reapply", plan, err)
	}

	for _, pkg := range []string{"postgresql-18", "postgresql-client-18"} {
		if !slices.Contains(pgPackages, pkg) {
			t.Errorf("required package %s is missing from %v", pkg, pgPackages)
		}
	}

	if slices.Contains(pgPackages, "postgresql-contrib") {
		t.Errorf("virtual postgresql-contrib is still required: %v", pgPackages)
	}

	// The virtual package is gone, but the concrete PG18 package is not:
	// a missing postgresql-18 is still a real failure.
	packageProbe = pg18PackageProbe("postgresql-18")

	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "postgresql-18") {
		t.Fatalf("missing postgresql-18 = %+v, %v", res, err)
	}
}
