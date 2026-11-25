package localenv

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

// ClientPath returns a client from the installed sidecar bundle, never PATH.
// Supported names are psql, pg_dump and pg_restore. Missing or non-executable
// clients are errors; callers must not silently mix PostgreSQL versions.
func ClientPath(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}

	return clientPath(exe, name)
}

func clientPath(exe, name string) (string, error) {
	switch name {
	case "psql", "pg_dump", "pg_restore":
	default:
		return "", fmt.Errorf("unsupported PostgreSQL client %q", name)
	}

	psql, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(exe), ".vaire-psql"))
	if err != nil {
		return "", fmt.Errorf("installed PostgreSQL client bundle unavailable: %w", err)
	}

	client := filepath.Join(filepath.Dir(psql), name)

	info, err := os.Stat(client)
	if err != nil {
		return "", fmt.Errorf("installed PostgreSQL client %s unavailable: %w", name, err)
	}

	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("installed PostgreSQL client %s is not executable", name)
	}

	return client, nil
}

// PsqlPath prefers the sidecar installed beside vaire, falling back to PATH.
func PsqlPath(cmd *cobra.Command) (string, error) {
	exe, _ := os.Executable()
	return psqlPath(cmd, exe)
}

func psqlPath(cmd *cobra.Command, exe string) (string, error) {
	managed := filepath.Join(filepath.Dir(exe), ".vaire-psql")
	if exe != "" {
		if info, err := os.Stat(managed); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return managed, nil
		}
	}

	if err := RequireTool(cmd, ToolPsql); err != nil {
		return "", err
	}

	return exec.LookPath("psql")
}
