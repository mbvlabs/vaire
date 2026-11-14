package steps

import (
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
)

func TestCatalogAccessStepFollowsRoles(t *testing.T) {
	catalog := Catalog()
	if err := engine.ValidateIDs(catalog); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(catalog))
	for _, step := range catalog {
		ids = append(ids, step.ID())
	}
	roles, access := slices.Index(ids, postgresRolesID), slices.Index(ids, postgresAccessID)
	if roles < 0 || access != roles+1 {
		t.Fatalf("access must immediately follow roles: %v", ids)
	}
	dbIDs := DatabaseStepIDs()
	roles, access = slices.Index(dbIDs, postgresRolesID), slices.Index(dbIDs, postgresAccessID)
	if roles < 0 || access != roles+1 {
		t.Fatalf("database steps do not include access after roles: %v", dbIDs)
	}
	if slices.Contains(RolesStepIDs(), postgresAccessID) {
		t.Fatal("role and protection synchronization must not authorize access")
	}
}
