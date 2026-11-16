package steps

import (
	"context"
	"errors"
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
