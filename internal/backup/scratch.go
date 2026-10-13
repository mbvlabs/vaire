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

// scratchReadyWait bounds how long recovery of a scratch instance may take.
const scratchReadyWait = 24 * time.Hour

// ScratchRoot is where scratch restores go: beside the live data directory,
// so they land on the same filesystem, and outside /var/lib/eru, which is
// root 0700 and closed to postgres.
func ScratchRoot(dataDirectory string) string {
	return filepath.Join(filepath.Dir(filepath.Clean(dataDirectory)), "eru-scratch")
}

// Scratch is one throwaway instance restored from the repository: a data
// directory, its own socket directory and a log, under ScratchRoot/<id>.
type Scratch struct {
	ID   string
	Base string
	// Port is the instance's port from the temporary range (RFC 8.2).
	Port   int
	Data   string
	Socket string
	Shell  engine.Shell
	StepID string
	Report engine.Reporter
}

// NewScratch creates the directories for scratch id, owned by postgres.
func NewScratch(dataDirectory, id string, sh engine.Shell, stepID string, report engine.Reporter) (*Scratch, error) {
	root := ScratchRoot(dataDirectory)
	s := &Scratch{
		ID:     id,
		Base:   filepath.Join(root, id),
		Shell:  sh,
		StepID: stepID,
		Report: report,
	}
	s.Data = filepath.Join(s.Base, "data")
	s.Socket = filepath.Join(s.Base, "sock")
	uid, gid, err := engine.ResolveIDs("postgres", "postgres")
	if err != nil {
		return nil, err
	}
	for _, dir := range []string{root, s.Base, s.Data, s.Socket} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chown(dir, uid, gid); err != nil {
			return nil, err
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory; postgres needs to enter it.
			return nil, err
		}
	}
	if s.Port, err = reserveTemporaryPort(root, s.Base); err != nil {
		_ = os.RemoveAll(s.Base)
		return nil, err
	}
	return s, nil
}

// portFile records a scratch instance's port in its directory, so the next
// allocation skips it before the instance has started. Remove frees it.
const portFile = "port"

// reserveTemporaryPort picks the first free port of the temporary range and
// records it in base. A port is taken when a cluster uses it, a socket lock
// for it exists in the shared socket directory, or another scratch instance
// recorded it. The allocation runs under a lock in root.
func reserveTemporaryPort(root, base string) (int, error) {
	var port int
	err := engine.WithFileLock(filepath.Join(root, "ports.lock"), func() error {
		taken := clusterPorts()
		dirs, _ := os.ReadDir(root)
		for _, d := range dirs {
			raw, err := os.ReadFile(filepath.Join(root, d.Name(), portFile)) //nolint:gosec // G304: eru's own scratch directories.
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
			if _, err := os.Stat(fmt.Sprintf("/var/run/postgresql/.s.PGSQL.%d.lock", n)); err == nil {
				continue
			}
			port = n
			return os.WriteFile(filepath.Join(base, portFile), []byte(strconv.Itoa(n)+"\n"), 0o600)
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

// PG is a SQL runner for the scratch instance.
func (s *Scratch) PG() pg.Local { return pg.Local{Host: s.Socket, Port: s.Port} }

// Restore runs the driver's scratch restore into the data directory.
func (s *Scratch) Restore(ctx context.Context, d Driver, opts RestoreOptions) error {
	opts.Path = s.Data
	return d.RestoreScratch(ctx, opts)
}

// Start writes the scratch configuration and starts the instance, then waits
// until recovery has promoted it and it accepts connections. The instance
// listens only on its own socket, with archive_mode off so it can never push
// WAL to the repository, and hot_standby off so recovery doesn't need the
// live cluster's connection limits.
func (s *Scratch) Start(ctx context.Context) error {
	conf := strings.Join([]string{
		"# eru scratch instance " + s.ID + "; removed when eru is done with it",
		"port = " + strconv.Itoa(s.Port),
		"listen_addresses = ''",
		"unix_socket_directories = '" + s.Socket + "'",
		"hba_file = '" + filepath.Join(s.Base, "pg_hba.conf") + "'",
		"ident_file = '" + filepath.Join(s.Base, "pg_ident.conf") + "'",
		"archive_mode = off",
		"hot_standby = off",
		"shared_buffers = 128MB",
		"max_connections = 20",
		"huge_pages = off",
		"ssl = off",
		"logging_collector = off",
		"",
	}, "\n")
	files := map[string]string{
		filepath.Join(s.Data, "postgresql.conf"): conf,
		filepath.Join(s.Base, "pg_hba.conf"):     "local all postgres peer\n",
		filepath.Join(s.Base, "pg_ident.conf"):   "",
	}
	for path, content := range files {
		if err := engine.WriteFileAtomicOwned(path, []byte(content), 0o600, "postgres", "postgres"); err != nil {
			return err
		}
	}
	if err := s.Shell.Exec(ctx, s.StepID, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", pgBin + "/pg_ctl", "-D", s.Data, "-l", s.logPath(), "-W", "start",
	}}, s.Report); err != nil {
		return wrapTool("start the scratch instance", err)
	}
	deadline := time.Now().Add(scratchReadyWait)
	lastNote := time.Now()
	for {
		if err := runQuiet(ctx, "pg_isready", "-q", "-h", s.Socket, "-p", strconv.Itoa(s.Port)); err == nil {
			return nil
		}
		if !s.running(ctx) {
			return output.NewError(output.CodeExternalCommand, "the scratch instance stopped during recovery", output.ExitExternal,
				"Read details.log_tail. A time outside the restore window ends recovery before its target.").
				WithDetails(map[string]any{"log_tail": s.LogTail(20)})
		}
		if time.Now().After(deadline) {
			return errors.New("the scratch instance did not finish recovery within 24 hours")
		}
		if time.Since(lastNote) > time.Minute {
			lastNote = time.Now()
			s.log("recovery is still replaying WAL")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *Scratch) logPath() string { return filepath.Join(s.Base, "postgres.log") }

func (s *Scratch) log(line string) {
	if s.Report != nil {
		s.Report(output.Event{Type: "log", Step: s.StepID, Line: line})
	}
}

// running reports whether the scratch postmaster is alive.
func (s *Scratch) running(ctx context.Context) bool {
	return runQuiet(ctx, "runuser", "-u", "postgres", "--", pgBin+"/pg_ctl", "-D", s.Data, "status") == nil
}

// LogTail returns the last n lines of the scratch instance's log.
func (s *Scratch) LogTail(n int) []string {
	raw, err := os.ReadFile(s.logPath())
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

// RecoveredTo is the commit time recovery replayed up to.
func (s *Scratch) RecoveredTo(ctx context.Context) string {
	rows, err := s.PG().Query(ctx, "postgres",
		`SELECT coalesce(to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS t`, nil)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0]["t"]
}

// Stop stops the instance if it runs.
func (s *Scratch) Stop(ctx context.Context) error {
	if !s.running(ctx) {
		return nil
	}
	return wrapTool("stop the scratch instance", s.Shell.Exec(ctx, s.StepID, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", pgBin + "/pg_ctl", "-D", s.Data, "-m", "fast", "-w", "-t", "300", "stop",
	}}, s.Report))
}

// Remove stops the instance and deletes its directories. It runs with a
// context of its own, so a cancelled job still cleans up.
func (s *Scratch) Remove() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	stopErr := s.Stop(ctx)
	if err := os.RemoveAll(s.Base); err != nil {
		return err
	}
	return stopErr
}

// CopyDatabase pipes pg_dump -Fc of srcDB on the instance at src into
// pg_restore of target on the cluster at dst, as owner. pg_restore's own
// errors (an object the owner may not create, for example) don't stop the
// copy: they come back as warnings, and the caller asks the operator to
// inspect the copy before using it.
func CopyDatabase(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, src pg.Local, srcDB string, dst pg.Local, target, owner string) ([]string, error) {
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	dumpErr := make(chan error, 1)
	go func() {
		err := PgDump(ctx, sh, stepID, report, src, srcDB, pw)
		_ = pw.Close()
		dumpErr <- err
	}()
	restoreErr := sh.Exec(ctx, stepID, engine.Command{Name: "runuser", Args: restoreArgs(dst, target, owner), Stdin: pr}, report)
	_ = pr.Close()
	if err := <-dumpErr; err != nil {
		return nil, err
	}
	var scriptErr *engine.ScriptError
	switch {
	case restoreErr == nil:
		return nil, nil
	case errors.As(restoreErr, &scriptErr):
		warnings := []string{fmt.Sprintf("pg_restore into %s reported errors (exit status %d); inspect it before relying on it", target, scriptErr.Status)}
		return append(warnings, scriptErr.Tail...), nil
	default:
		return nil, wrapTool("pg_restore into "+target, restoreErr)
	}
}

// LoadDump pipes a pg_dump archive from r into pg_restore of target on the
// cluster at dst, as owner, with CopyDatabase's error handling.
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
