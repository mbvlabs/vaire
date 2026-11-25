package localenv

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
)

func TestPsqlPath(t *testing.T) {
	dir := t.TempDir()
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	fallback := filepath.Join(bin, "psql")
	managed := filepath.Join(dir, ".vaire-psql")

	for _, name := range []string{fallback, managed} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := &cobra.Command{}
	if got, err := psqlPath(cmd, filepath.Join(dir, "vaire")); err != nil || got != managed {
		t.Fatalf("managed psql: %q, %v", got, err)
	}

	if err := os.Chmod(managed, 0o644); err != nil {
		t.Fatal(err)
	}

	if got, err := psqlPath(cmd, filepath.Join(dir, "vaire")); err != nil || got != fallback {
		t.Fatalf("PATH psql: %q, %v", got, err)
	}
}

func TestClientPath(t *testing.T) {
	dir := t.TempDir()

	bundle := filepath.Join(dir, ".vaire-psql-bundle.test")
	if err := os.Mkdir(bundle, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"psql", "pg_dump", "pg_restore"} {
		if err := os.WriteFile(filepath.Join(bundle, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if err := os.Symlink(filepath.Join(filepath.Base(bundle), "psql"), filepath.Join(dir, ".vaire-psql")); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"psql", "pg_dump", "pg_restore"} {
		got, err := clientPath(filepath.Join(dir, "vaire"), name)
		if err != nil || got != filepath.Join(bundle, name) {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
	}

	if _, err := clientPath(filepath.Join(dir, "vaire"), "../pg_dump"); err == nil {
		t.Fatal("accepted unsupported name")
	}

	if err := os.Chmod(filepath.Join(bundle, "pg_dump"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := clientPath(filepath.Join(dir, "vaire"), "pg_dump"); err == nil {
		t.Fatal("accepted non-executable client")
	}
}
