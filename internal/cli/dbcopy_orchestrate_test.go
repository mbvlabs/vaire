package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkstationSourceSpec(t *testing.T) {
	spec, err := workstationSourceSpec(copySourceInput{URL: "postgres://alice:p%3Ass@db.example/app?sslmode=verify-full"})
	if err != nil || spec.Host != "db.example" || spec.Port != 5432 || spec.Database != "app" || spec.Password != "p:ss" || spec.TLSMode != "verify-full" {
		t.Fatalf("source parsing failed: %v", err)
	}

	if _, err := workstationSourceSpec(copySourceInput{URL: "postgres://alice:secret@db.example/app?sslmode=disable"}); err == nil {
		t.Fatal("accepted disabled TLS")
	}
}

func TestWorkstationRestoreCommand(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "fake-restore")

	script := `#!/bin/sh
test "$PGHOST" = target.example || exit 1
test "$PGHOSTADDR" = 127.0.0.1 || exit 2
test "$PGPORT" = 6544 || exit 3
test "$PGSSLMODE" = verify-full || exit 4
test "$PGSSLROOTCERT" = system || exit 5
test "$PGUSER" = copy_migrate || exit 6
test "$PGDATABASE" = copy || exit 7
test -z "$PGOPTIONS" || exit 8
test "$*" = "--exit-on-error --single-transaction --no-owner --no-acl --no-tablespaces --no-password --dbname copy --use-list $HOME/selection $HOME/archive" || exit 9
`
	if err := os.WriteFile(tool, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PGOPTIONS", "unsafe-inherited-option")

	cmd := workstationRestoreCommand(context.Background(), tool, dir, "target.example", 6544, "copy", "copy_migrate", filepath.Join(dir, "pass"))
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}

	if strings.Contains(strings.Join(cmd.Args, " "), "password-value") || strings.Contains(strings.Join(cmd.Env, " "), "password-value") {
		t.Fatal("password included in command")
	}
}
