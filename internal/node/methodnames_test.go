package node

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/rpc"
)

// Every registered method must pass DecodeRequest's name check, or the node
// refuses the request before it reaches the handler (db.write_access did).
func TestRegisteredMethodNamesAreWellFormed(t *testing.T) {
	for name := range methods() {
		if !rpc.ValidMethod(name) {
			t.Errorf("method %q fails rpc's method name check", name)
		}
	}
}
