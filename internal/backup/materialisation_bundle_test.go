package backup

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/masterbackup"
)

func TestMaterialisationOwnershipExcludedFromMasterBundle(t *testing.T) {
	if masterbackup.Allowed("/var/lib/vaire/materialise-example.json") {
		t.Fatal("node-local materialisation ownership must not be bundled")
	}
}
