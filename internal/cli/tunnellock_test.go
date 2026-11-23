package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/workspaces"
)

func TestBootstrapTunnelLockDoesNotCreateWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	const name = "fresh"

	path := filepath.Join(workspaces.PendingDir(name), TunnelLockFile)

	release, err := lockTunnelPath(t.Context(), name, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(workspaces.Dir(name)); !os.IsNotExist(err) {
		t.Fatalf("bootstrap lock created the destination workspace: %v", err)
	}
}
