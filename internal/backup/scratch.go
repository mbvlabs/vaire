package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
)

// pgBin is the PostgreSQL 18 server binaries.
const pgBin = "/usr/lib/postgresql/18/bin"

// recoveryWait bounds how long recovery of a temporary cluster may take.
const recoveryWait = 24 * time.Hour

// TemporaryPortDir holds one file per temporary cluster, named after it, with the port it reserved: the next allocation
// skips it before the cluster is registered or the instance has started.
// It is on /run, so a reboot frees reservations whose cluster pg_lsclusters
// lists anyway.
const TemporaryPortDir = "/run/eru-ports"

// reserveTemporaryPort picks the first free port of the temporary range and
// records it for owner. A port is taken when a cluster uses it, a socket
// lock for it exists in the shared socket directory, or another owner
// reserved it. The allocation runs under a lock in TemporaryPortDir.
func reserveTemporaryPort(owner string) (int, error) {
	var port int
	err := engine.WithFileLock(filepath.Join(TemporaryPortDir, "ports.lock"), func() error {
		taken := clusterPorts()
		files, _ := os.ReadDir(TemporaryPortDir)
		for _, f := range files {
			raw, err := os.ReadFile(filepath.Join(TemporaryPortDir, f.Name())) //nolint:gosec // G304: eru's own reservations.
			if err != nil {
				continue
			}
			if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				taken[n] = true
			}
		}
		for n := config.TemporaryPortMin; n <= config.TemporaryPortMax; n++ {
			if taken[n] {
				continue
			}
			if _, err := os.Stat(fmt.Sprintf("%s/.s.PGSQL.%d.lock", pgSocketDir, n)); err == nil {
				continue
			}
			port = n
			return os.WriteFile(filepath.Join(TemporaryPortDir, owner), []byte(strconv.Itoa(n)+"\n"), 0o600)
		}
		return output.NewError(output.CodePrecondition,
			fmt.Sprintf("every temporary port (%d-%d) is in use", config.TemporaryPortMin, config.TemporaryPortMax), output.ExitPrecondition,
			"Wait for running restores, forks or drills to finish, then retry.").WithDetails(map[string]any{"reason": "no_temporary_port"})
	})
	return port, err
}

// clusterPorts are the ports pg_lsclusters lists.
func clusterPorts() map[int]bool {
	out, err := exec.Command("pg_lsclusters", "--no-header").Output()
	ports := map[int]bool{}
	if err != nil {
		return ports
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		if n, err := strconv.Atoi(fields[2]); err == nil {
			ports[n] = true
		}
	}
	return ports
}

// LoadDump pipes a pg_dump archive from r into pg_restore of target on the
// cluster at dst, as owner. pg_restore's own errors (an object the owner may
// not create, for example) don't stop the load: they come back as warnings,
// and the caller asks the operator to inspect the result before using it.
func LoadDump(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, r io.Reader, dst pg.Local, target, owner string) ([]string, error) {
	err := sh.Exec(ctx, stepID, engine.Command{Name: "runuser", Args: restoreArgs(dst, target, owner), Stdin: r}, report)
	var scriptErr *engine.ScriptError
	switch {
	case err == nil:
		return nil, nil
	case errors.As(err, &scriptErr):
		warnings := []string{fmt.Sprintf("pg_restore into %s reported errors (exit status %d); inspect it before relying on it", target, scriptErr.Status)}
		return append(warnings, scriptErr.Tail...), nil
	default:
		return nil, wrapTool("pg_restore into "+target, err)
	}
}

// runQuiet runs a probe command and reports only whether it succeeded.
func runQuiet(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return engine.RunQuiet(ctx, name, args...)
}

// restoreArgs runs pg_restore into target on the cluster at dst, as owner.
func restoreArgs(dst pg.Local, target, owner string) []string {
	args := append([]string{"-u", "postgres", "--", "pg_restore", "--no-owner", "--no-privileges", "--role=" + owner}, connArgs(dst)...)
	return append(args, "-d", target)
}
