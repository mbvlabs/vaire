package steps

import (
	"context"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestCustomPackageAuthority(t *testing.T) {
	for _, tc := range []struct {
		name    string
		req     ExtensionPrerequisite
		trusted bool
		allowed bool
	}{
		{"catalog", ExtensionPrerequisite{Name: "hypopg", Source: catalog.SourceAPT, Package: "postgresql-18-hypopg"}, false, true},
		{"catalog override", ExtensionPrerequisite{Name: "hypopg", Source: catalog.SourceAPT, Package: "postgresql-18-other"}, false, false},
		{"custom", ExtensionPrerequisite{Name: "custom", Source: catalog.SourceAPT, Package: "postgresql-18-custom"}, false, false},
		{"trusted", ExtensionPrerequisite{Name: "custom", Source: catalog.SourceAPT, Package: "postgresql-18-custom"}, true, true},
		{"preinstalled", ExtensionPrerequisite{Name: "custom", Source: catalog.SourcePreinstalled}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckExtensionPackageAuthority([]ExtensionPrerequisite{tc.req}, tc.trusted)
			if (err == nil) != tc.allowed {
				t.Fatalf("authority error = %v", err)
			}
		})
	}
}

func TestCustomPackageBoundariesRefuseBeforeScript(t *testing.T) {
	old := runExtensionPackageScript

	t.Cleanup(func() { runExtensionPackageScript = old })

	called := false
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		called = true
		return nil
	}

	req := ExtensionPrerequisite{Name: "custom", Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom"}
	if err := PrepareExtensionRequirements(context.Background(), []ExtensionPrerequisite{req}, engine.Runtime{}, nil); err == nil || called {
		t.Fatalf("historical requirement: err=%v script=%v", err, called)
	}

	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom"}}}

	cfg.Extensions = map[string]config.ExtensionDefinition{"custom": {Source: catalog.SourceAPT, Package: req.Package}}
	if err := installPostgresPackages(context.Background(), cfg, postgresPackagesID, engine.Runtime{}, nil); err == nil || called {
		t.Fatalf("host package step: err=%v script=%v", err, called)
	}
}
