//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/contexts"
)

func TestTunnelSocketPathShort(t *testing.T) {
	dir, err := os.MkdirTemp("", "eru")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	t.Setenv("XDG_CONFIG_HOME", dir)
	got, err := tunnelSocketPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if want := contexts.Path("prod", TunnelSocketFile); got != want {
		t.Fatalf("socket = %s, want %s", got, want)
	}
}

func TestTunnelSocketPathFallsBackWhenTooLong(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 120))
	t.Setenv("XDG_CONFIG_HOME", long)
	runtimeDir, err := os.MkdirTemp("", "eru-rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("TMPDIR", runtimeDir)
	got, err := tunnelSocketPath("prod")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) > maxSocketPath || !strings.HasPrefix(got, runtimeDir) || !strings.HasSuffix(got, ".sock") {
		t.Fatalf("fallback socket = %s", got)
	}
	again, err := tunnelSocketPath("prod")
	if err != nil || again != got {
		t.Fatalf("fallback is not stable: %s, %v", again, err)
	}
	other, _ := tunnelSocketPath("staging")
	if other == got {
		t.Fatal("two contexts share a fallback socket")
	}
	info, err := os.Stat(filepath.Dir(got))
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("fallback directory mode: %v, %v", info, err)
	}
}

func TestPrivateDirRefusesLooseMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "eru")
	if err := os.Mkdir(dir, 0o755); err != nil { //nolint:gosec // G301: the test needs a loose mode.
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil { //nolint:gosec // G302: the test needs a loose mode.
		t.Fatal(err)
	}
	if err := privateDir(dir); err == nil {
		t.Fatal("a 0755 directory was accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	if err := privateDir(link); err == nil {
		t.Fatal("a symlink was accepted")
	}
}

func TestTunnelPersist(t *testing.T) {
	c := &contexts.Context{}
	t.Setenv(envTunnelPersist, "")
	if d, err := tunnelPersist(c); err != nil || d != DefaultTunnelPersist {
		t.Fatalf("default = %s, %v", d, err)
	}
	c.TunnelPersist = "2m"
	if d, _ := tunnelPersist(c); d != 2*time.Minute {
		t.Fatalf("context = %s, want 2m", d)
	}
	t.Setenv(envTunnelPersist, "0")
	if d, _ := tunnelPersist(c); d != 0 {
		t.Fatalf("env = %s, want 0", d)
	}
	t.Setenv(envTunnelPersist, "soon")
	if d, err := tunnelPersist(c); err == nil || d != DefaultTunnelPersist {
		t.Fatalf("invalid = %s, %v; want the default and an error", d, err)
	}
}
