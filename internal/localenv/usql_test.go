package localenv

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUsqlPathPrefersInstalledSidecar(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	managed := filepath.Join(filepath.Dir(exe), ".vaire-usql")

	f, err := os.OpenFile(managed, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0755)
	if err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.Remove(managed) })
	pathDir := t.TempDir()

	pathUsql := filepath.Join(pathDir, "usql")
	if err := os.WriteFile(pathUsql, []byte("path usql"), 0755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", pathDir)

	got, err := UsqlPath(nil)
	if err != nil || got != managed {
		t.Fatalf("UsqlPath() = %q, %v; want managed %q", got, err, managed)
	}

	if err := os.Remove(managed); err != nil {
		t.Fatal(err)
	}

	got, err = UsqlPath(nil)
	if err != nil || got != pathUsql {
		t.Fatalf("UsqlPath() = %q, %v; want PATH %q", got, err, pathUsql)
	}
}

func TestUsqlDownloadContext(t *testing.T) {
	if ctx := usqlDownloadContext(nil); ctx == nil || ctx.Err() != nil { //nolint:staticcheck // SA1012: explicitly test the supported nil-context fallback.
		t.Fatalf("nil input produced an unusable download context: %v", ctx)
	}

	parent, cancel := context.WithCancel(context.Background())
	defer cancel()

	ctx := usqlDownloadContext(parent)
	if ctx != parent {
		t.Fatal("the caller's context was replaced")
	}

	cancel()

	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("download context error = %v, want cancellation", ctx.Err())
	}
}
