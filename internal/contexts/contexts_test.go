package contexts

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
)

func TestContextHintOnNode(t *testing.T) {
	saved := nodeMarker
	t.Cleanup(func() { nodeMarker = saved })

	nodeMarker = filepath.Join(t.TempDir(), "cluster.toml")
	if got := contextHint("default"); got != "default" {
		t.Fatalf("off a node: got %q, want the default hint", got)
	}
	if err := os.WriteFile(nodeMarker, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := contextHint("default"); !strings.Contains(got, "eru node") || !strings.Contains(got, "your own machine") {
		t.Fatalf("on a node: got %q, want the run-from-your-machine hint", got)
	}
}

func TestValidateNameHintsAtDashDash(t *testing.T) {
	var cliErr *output.CLIError
	if err := ValidateName("select 1"); !errors.As(err, &cliErr) || !strings.Contains(cliErr.Hint, "eru psql --db <db> -- -c") {
		t.Fatalf("got %v, want a hint about --", err)
	}
	if err := ValidateName("Prod"); !errors.As(err, &cliErr) || strings.Contains(cliErr.Hint, "psql") {
		t.Fatalf("got %v, want the plain naming hint", err)
	}
}
