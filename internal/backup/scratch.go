package backup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

// pgBin is the PostgreSQL 18 server binaries.
const pgBin = "/usr/lib/postgresql/18/bin"

// recoveryWait bounds how long recovery of a temporary cluster may take.
const recoveryWait = 24 * time.Hour

// TemporaryPortDir holds one file per temporary cluster, named after it, with the port it reserved: the next allocation
// skips it before the cluster is registered or the instance has started.
// It is on /run, so a reboot frees reservations whose cluster pg_lsclusters
// lists anyway.
const TemporaryPortDir = "/run/vaire-ports"

// reserveTemporaryPort picks the first free port of the temporary range and
// records it for owner. A port is taken when a cluster uses it, a socket
// lock for it exists in the shared socket directory, or another owner
// reserved it. The allocation runs under a lock in TemporaryPortDir.
func reserveTemporaryPort(owner string) (int, error) {
	var port int

	err := engine.WithFileLock(filepath.Join(TemporaryPortDir, "ports.lock"), func() error {
		if _, err := os.Lstat(filepath.Join(TemporaryPortDir, owner)); !errors.Is(err, os.ErrNotExist) {
			return errors.Join(err, errors.New("reservation owner already exists or is unverifiable"))
		}

		taken := reservedPorts()
		for n := config.TemporaryPortMin; n <= config.TemporaryPortMax; n++ {
			if taken[n] || socketLocked(n) {
				continue
			}

			port = n

			return recordReservedPort(owner, n)
		}

		return noTemporaryPortError()
	})

	return port, err
}

// reservedPorts are the ports pg_lsclusters lists and the ones other owners
// reserved in TemporaryPortDir.
func reservedPorts() map[int]bool {
	taken := clusterPorts()

	files, _ := os.ReadDir(TemporaryPortDir)
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(TemporaryPortDir, f.Name()))
		if err != nil {
			continue
		}

		if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
			taken[n] = true
		}
	}

	return taken
}

// socketLocked reports whether a socket lock file in the shared socket
// directory claims port.
func socketLocked(port int) bool {
	_, err := os.Stat(fmt.Sprintf("%s/.s.PGSQL.%d.lock", pgSocketDir, port))
	return err == nil
}

// recordReservedPort writes owner's reservation of port, exclusively so a
// second allocation for the same owner fails instead of replacing it.
func recordReservedPort(owner string, port int) error {
	f, err := os.OpenFile(filepath.Join(TemporaryPortDir, owner), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}

	_, writeErr := fmt.Fprintln(f, port)

	return errors.Join(writeErr, f.Close())
}

// noTemporaryPortError reports that every temporary port is taken.
func noTemporaryPortError() error {
	return output.NewError(output.CodePrecondition,
		fmt.Sprintf("every temporary port (%d-%d) is in use", config.TemporaryPortMin, config.TemporaryPortMax), output.ExitPrecondition,
		"Wait for running restores, forks or drills to finish, then retry.").WithDetails(map[string]any{"reason": "no_temporary_port"})
}

// clusterPorts are the ports pg_lsclusters lists. The port allocator has no
// caller context to cancel a probe of the local host, so it bounds the probe
// itself rather than leaving a hung pg_lsclusters behind.
func clusterPorts() map[int]bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "pg_lsclusters", "--no-header").Output()

	ports := map[int]bool{}
	if err != nil {
		return ports
	}

	for line := range strings.SplitSeq(string(out), "\n") {
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

// runQuiet runs a probe command and reports only whether it succeeded.
func runQuiet(ctx context.Context, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return engine.RunQuiet(ctx, name, args...)
}

// connArgs are the -h and -p arguments that point a client tool at l.
func connArgs(l pg.Local) []string {
	var args []string
	if l.Host != "" {
		args = append(args, "-h", l.Host)
	}

	if l.Port != 0 {
		args = append(args, "-p", fmt.Sprint(l.Port))
	}

	return args
}
