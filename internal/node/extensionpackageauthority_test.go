package node

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestCustomPackageDeclarationRequiresTrustedHostAdmin(t *testing.T) {
	old := config.DefaultCluster()
	next := old
	next.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: "apt", Package: "postgresql-18-custom"},
	}

	if err := checkExtensionPackageChange(old, next, false); err == nil {
		t.Fatal("operator could declare a custom package")
	}

	if err := checkExtensionPackageChange(old, next, true); err != nil {
		t.Fatal(err)
	}

	if err := checkExtensionPackageChange(next, next, false); err != nil {
		t.Fatalf("unchanged declaration blocked an operator edit: %v", err)
	}

	selected := next
	selected.Clusters = []config.ClusterSpec{{Name: "db", Extensions: []string{"custom"}}}

	if err := checkExtensionPackageChange(next, selected, false); err == nil {
		t.Fatal("operator could select a previously declared custom package")
	}
}
