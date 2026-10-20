package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

// Layout of the clusters postgresql-common manages, as the materialised
// cluster needs it. steps keeps the same paths for the live clusters.
const (
	pgConfRoot  = "/etc/postgresql/18"
	pgSocketDir = "/var/run/postgresql"
)

// MaterialiseOverrides is the conf.d file that turns a copy of the source's
// configuration into a temporary cluster's. It loads after vaire's own files.
const MaterialiseOverrides = "99-materialise.conf"

// Materialised is a temporary cluster, 18/<name>, built from a live
// cluster's stanza at a point in time or from the live cluster itself (RFC
// 10): registered with postgresql-common under <data_root>/<name>, on a
// port of the temporary range, with archiving off and hot_standby on. Its
// configuration is a copy of the source's, so the hot-standby floors
// (max_connections, max_worker_processes, max_wal_senders,
// max_prepared_transactions, max_locks_per_transaction) are the source's,
// and only the memory settings are made small.
type Materialised struct {
	// Name is the temporary cluster's name, for example app__restoring.
	Name string
	// Source is the live cluster it copies.
	Source string
	// SourcePort is the live cluster's socket port.
	SourcePort int
	// Port is the temporary cluster's port (RFC 8.2).
	Port   int
	Data   string
	Shell  engine.Shell
	StepID string
	Report engine.Reporter
}

// NewMaterialised reserves a temporary port for cluster name under
// dataRoot. Nothing is created until Restore or BaseBackup.
func NewMaterialised(dataRoot, name, source string, sourcePort int, sh engine.Shell, stepID string, report engine.Reporter) (*Materialised, error) {
	m := &Materialised{
		Name: name, Source: source, SourcePort: sourcePort,
		Data:  filepath.Join(dataRoot, name),
		Shell: sh, StepID: stepID, Report: report,
	}
	if _, err := os.Stat(m.ConfDir()); err == nil {
		return nil, output.NewError(output.CodePrecondition, "cluster 18/"+name+" already exists", output.ExitPrecondition,
			"Drop what is left of it first.").WithDetails(map[string]any{"reason": "cluster_exists", "cluster": name})
	}
	if _, err := os.Stat(m.Data); err == nil {
		return nil, output.NewError(output.CodePrecondition, m.Data+" already exists", output.ExitPrecondition,
			"Remove what is left of an earlier cluster "+name+" first.").WithDetails(map[string]any{"reason": "cluster_exists", "cluster": name})
	}
	port, err := reserveTemporaryPort(name)
	if err != nil {
		return nil, err
	}
	m.Port = port
	return m, nil
}

// ConfDir is the temporary cluster's configuration directory.
func (m *Materialised) ConfDir() string { return filepath.Join(pgConfRoot, m.Name) }

// PG is a SQL runner for the temporary cluster.
func (m *Materialised) PG() pg.Local { return pg.Local{Host: pgSocketDir, Port: m.Port} }

// Restore restores the source's stanza at opts into the data directory,
// registers the cluster and starts it, then waits until recovery has paused
// at the target (opts.Pause), or has promoted there.
func (m *Materialised) Restore(ctx context.Context, d Driver, opts RestoreOptions) error {
	if err := m.makeData(); err != nil {
		return err
	}
	opts.Path = m.Data
	m.log("restoring " + m.Source + "'s stanza into " + m.Data)
	if err := d.Materialise(ctx, opts); err != nil {
		return err
	}
	if err := m.register(); err != nil {
		return err
	}
	switch {
	case !opts.Pause:
		return m.start(ctx, UntilPromoted)
	case opts.Kind == TargetLatest || opts.Kind == "":
		return m.start(ctx, UntilOpen)
	default:
		return m.start(ctx, UntilPaused)
	}
}

// BaseBackup copies the live source with pg_basebackup over its socket,
// registers the cluster and starts it. The copy is consistent and writable:
// it starts as a primary of its own, with archiving off.
func (m *Materialised) BaseBackup(ctx context.Context) error {
	if err := m.makeData(); err != nil {
		return err
	}
	m.log("copying the live " + m.Source + " into " + m.Data)
	if err := m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", pgBin + "/pg_basebackup", "-h", pgSocketDir, "-p", strconv.Itoa(m.SourcePort), "-U", "postgres",
		"-D", m.Data, "-X", "stream", "-c", "fast", "--no-password",
	}}, m.Report); err != nil {
		return wrapTool("pg_basebackup of "+m.Source, err)
	}
	if err := m.register(); err != nil {
		return err
	}
	return m.start(ctx, UntilOpen)
}

// makeData creates the empty data directory, owned by postgres.
func (m *Materialised) makeData() error {
	uid, gid, err := engine.ResolveIDs("postgres", "postgres")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(m.Data, 0o700); err != nil {
		return err
	}
	if err := os.Chown(m.Data, uid, gid); err != nil {
		return err
	}
	return os.Chmod(m.Data, 0o700) //nolint:gosec // G302: a data directory; postgres needs to enter it.
}

// register writes /etc/postgresql/18/<name>: a postgresql.conf that points
// at the data directory and the temporary port, the source's pg_hba,
// pg_ident and conf.d, and the overrides that make it a temporary cluster.
// start.conf is manual, so it never starts on its own at boot.
func (m *Materialised) register() error {
	src := filepath.Join(pgConfRoot, m.Source)
	dir := m.ConfDir()
	files := map[string]string{
		"postgresql.conf": strings.Join([]string{
			"# vaire temporary cluster " + m.Name + ", materialised from " + m.Source + "; dropped when vaire is done with it",
			"data_directory = '" + m.Data + "'",
			"hba_file = '" + filepath.Join(dir, "pg_hba.conf") + "'",
			"ident_file = '" + filepath.Join(dir, "pg_ident.conf") + "'",
			"external_pid_file = '" + pgSocketDir + "/18-" + m.Name + ".pid'",
			"port = " + strconv.Itoa(m.Port),
			"unix_socket_directories = '" + pgSocketDir + "'",
			"cluster_name = '18/" + m.Name + "'",
			"include_dir = 'conf.d'",
			"",
		}, "\n"),
		"start.conf": "manual\n",
	}
	for _, name := range []string{"pg_hba.conf", "pg_ident.conf", "pg_ctl.conf", "environment"} {
		raw, err := os.ReadFile(filepath.Join(src, name)) //nolint:gosec // G304: the source cluster's own configuration.
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		files[name] = string(raw)
	}
	confD, err := os.ReadDir(filepath.Join(src, "conf.d"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, e := range confD {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") || e.Name() == MaterialiseOverrides {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(src, "conf.d", e.Name())) //nolint:gosec // G304: the source cluster's own configuration.
		if err != nil {
			return err
		}
		files[filepath.Join("conf.d", e.Name())] = string(raw)
	}
	files[filepath.Join("conf.d", MaterialiseOverrides)] = strings.Join([]string{
		"# vaire temporary cluster: no archiving, read-only recovery, small memory",
		"port = " + strconv.Itoa(m.Port),
		"listen_addresses = ''",
		"archive_mode = off",
		"hot_standby = on",
		"shared_buffers = 128MB",
		"effective_cache_size = 256MB",
		"maintenance_work_mem = 64MB",
		"huge_pages = off",
		"",
	}, "\n")
	for _, d := range []string{dir, filepath.Join(dir, "conf.d")} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // G301: postgresql-common's configuration directories are world-readable.
			return err
		}
		uid, gid, err := engine.ResolveIDs("postgres", "postgres")
		if err != nil {
			return err
		}
		if err := os.Chown(d, uid, gid); err != nil {
			return err
		}
	}
	for name, content := range files {
		if err := engine.WriteFileAtomicOwned(filepath.Join(dir, name), []byte(content), confMode(name), "postgres", "postgres"); err != nil {
			return err
		}
	}
	return nil
}

// confMode is the mode pg_createcluster and postgres-config-v1 give a live
// cluster's configuration file: pg_hba.conf and pg_ident.conf are 0640, the
// rest world-readable, so pg_lsclusters reads a temporary or leftover
// cluster's postgresql.conf without root as it does a live one's.
func confMode(name string) os.FileMode {
	switch filepath.Base(name) {
	case "pg_hba.conf", "pg_ident.conf":
		return 0o640
	}
	return 0o644
}

// UserOverrides is the conf.d file an operator edits by hand.
// postgres-config-v1 writes it only when missing, so it is its own source
// of truth.
const UserOverrides = "90-overrides.conf"

// Adopt makes the temporary cluster a database's own, 18/<name> on port (a
// fork, RFC 10.2). It stops it and rewrites its registration as
// postgres-cluster-v1 leaves a live cluster's: the source's postgresql.conf
// with this cluster's paths, name and port, start.conf auto, and a conf.d
// with only the source's 90-overrides.conf (copied by register), so the
// source's vaire files and the temporary cluster's overrides don't stay but
// the operator's do. The caller then runs the cluster's steps:
// postgres-cluster-v1@<name> starts it in its cgroup, and
// postgres-config-v1@<name> renders the rest of its configuration as a new
// cluster's, keeps 90-overrides.conf and restarts it. The port reservation
// is freed.
func (m *Materialised) Adopt(ctx context.Context, port int) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(pgConfRoot, m.Source, "postgresql.conf")) //nolint:gosec // G304: the source cluster's own configuration.
	if err != nil {
		return err
	}
	dir := m.ConfDir()
	conf := setConf(string(raw), [][2]string{
		{"data_directory", "'" + m.Data + "'"},
		{"hba_file", "'" + filepath.Join(dir, "pg_hba.conf") + "'"},
		{"ident_file", "'" + filepath.Join(dir, "pg_ident.conf") + "'"},
		{"external_pid_file", "'" + pgSocketDir + "/18-" + m.Name + ".pid'"},
		{"port", strconv.Itoa(port)},
		{"cluster_name", "'18/" + m.Name + "'"},
	})
	if err := engine.WriteFileAtomicOwned(filepath.Join(dir, "postgresql.conf"), []byte(conf), 0o644, "postgres", "postgres"); err != nil {
		return err
	}
	if err := engine.WriteFileAtomicOwned(filepath.Join(dir, "start.conf"), []byte("auto\n"), 0o644, "postgres", "postgres"); err != nil {
		return err
	}
	confD := filepath.Join(dir, "conf.d")
	entries, err := os.ReadDir(confD)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == UserOverrides {
			// register writes 0640; postgres-config-v1 writes it 0644.
			if err := os.Chmod(filepath.Join(confD, e.Name()), 0o644); err != nil { //nolint:gosec // G302: conf.d files are world-readable, as postgres-config-v1 writes them.
				return err
			}
			continue
		}
		if err := os.RemoveAll(filepath.Join(confD, e.Name())); err != nil {
			return err
		}
	}
	m.Port = port
	_ = os.Remove(filepath.Join(TemporaryPortDir, m.Name))
	return nil
}

// setConf sets each key of a postgresql.conf to its value, in place of its
// uncommented line, or at the end when it has none.
func setConf(raw string, values [][2]string) string {
	lines := strings.Split(strings.TrimRight(raw, "\n"), "\n")
	for _, kv := range values {
		found := false
		for i, line := range lines {
			key, _, ok := strings.Cut(line, "=")
			if ok && strings.TrimSpace(key) == kv[0] {
				lines[i] = kv[0] + " = " + kv[1]
				found = true
			}
		}
		if !found {
			lines = append(lines, kv[0]+" = "+kv[1])
		}
	}
	return strings.Join(lines, "\n") + "\n"
}

// Wait conditions for start.
const (
	UntilOpen     = "open"
	UntilPaused   = "paused"
	UntilPromoted = "promoted"
)

// start starts the cluster and waits until it accepts connections and, for
// UntilPaused, recovery has paused at the target, or for UntilPromoted, it
// has left recovery.
func (m *Materialised) start(ctx context.Context, until string) error {
	if err := m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "pg_ctlcluster", Args: []string{"18", m.Name, "start"}}, m.Report); err != nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("start the temporary cluster %s: %w", m.Name, err), output.ExitExternal,
			"Read details.log_tail.").WithDetails(map[string]any{"log_tail": m.LogTail(20)})
	}
	return waitCluster(ctx, m.Name, m.Data, m.Port, until, m.log)
}

// waitCluster waits until cluster name, with its data directory at data,
// accepts connections on port and is where until says. A postmaster that
// stops first returns its log tail: a time outside the restore window ends
// recovery before its target.
func waitCluster(ctx context.Context, name, data string, port int, until string, log func(string)) error {
	deadline := time.Now().Add(recoveryWait)
	lastNote := time.Now()
	for {
		if reached(ctx, port, until) {
			return nil
		}
		if !dataRunning(ctx, data) {
			return output.NewError(output.CodeExternalCommand, "the cluster "+name+" stopped during recovery", output.ExitExternal,
				"Read details.log_tail. A time outside the restore window ends recovery before its target.").
				WithDetails(map[string]any{"log_tail": clusterLogTail(name, 20)})
		}
		if time.Now().After(deadline) {
			return errors.New("the cluster " + name + " did not reach its target within 24 hours")
		}
		if time.Since(lastNote) > time.Minute {
			lastNote = time.Now()
			log("recovery is still replaying WAL")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// reached reports whether the cluster on port is where waitCluster waits
// for it.
func reached(ctx context.Context, port int, until string) bool {
	if runQuiet(ctx, "pg_isready", "-q", "-h", pgSocketDir, "-p", strconv.Itoa(port)) != nil {
		return false
	}
	if until == UntilOpen {
		return true
	}
	rows, err := (pg.Local{Host: pgSocketDir, Port: port}).Query(ctx, "postgres",
		`SELECT pg_is_in_recovery()::text AS r, CASE WHEN pg_is_in_recovery() THEN pg_get_wal_replay_pause_state() ELSE '' END AS p`, nil)
	if err != nil || len(rows) == 0 {
		return false
	}
	if until == UntilPromoted {
		return rows[0]["r"] == "false"
	}
	// A target recovery passed already promotes; that is reached too.
	return rows[0]["p"] == "paused" || rows[0]["r"] == "false"
}

// Running reports whether the temporary cluster's postmaster is alive.
func (m *Materialised) Running(ctx context.Context) bool { return dataRunning(ctx, m.Data) }

// dataRunning reports whether a postmaster runs on the data directory.
func dataRunning(ctx context.Context, data string) bool {
	return runQuiet(ctx, "runuser", "-u", "postgres", "--", pgBin+"/pg_ctl", "-D", data, "status") == nil
}

func (m *Materialised) log(line string) {
	if m.Report != nil {
		m.Report(output.Event{Type: "log", Step: m.StepID, Line: line})
	}
}

// LogTail returns the last n lines of the temporary cluster's log.
func (m *Materialised) LogTail(n int) []string { return clusterLogTail(m.Name, n) }

// clusterLogTail returns the last n lines of cluster name's
// postgresql-common log.
func clusterLogTail(name string, n int) []string {
	raw, err := os.ReadFile("/var/log/postgresql/postgresql-18-" + name + ".log") //nolint:gosec // G304: a cluster vaire named.
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
func (m *Materialised) RecoveredTo(ctx context.Context) string {
	rows, err := m.PG().Query(ctx, "postgres",
		`SELECT coalesce(to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"'), '') AS t`, nil)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0]["t"]
}

// Stop stops the temporary cluster if it runs.
func (m *Materialised) Stop(ctx context.Context) error {
	if !m.Running(ctx) {
		return nil
	}
	return wrapTool("stop the temporary cluster "+m.Name, m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "pg_ctlcluster", Args: []string{
		"18", m.Name, "stop", "-m", "fast",
	}}, m.Report))
}

// Drop tears the temporary cluster down. It runs with a context of its own,
// so a cancelled job still cleans up.
func (m *Materialised) Drop() error {
	return DropTemporaryCluster(m.Shell, m.StepID, m.Report, m.Name, m.Data)
}

// DropTemporaryCluster stops and removes cluster name with pg_dropcluster,
// or, when postgresql-common doesn't know it (a materialise that failed
// before it was registered) or it has no data directory (an aside
// registered before a swap's renames), removes its data and configuration
// directories. Then it frees its port reservation. It runs with a context
// of its own, so a cancelled job still cleans up.
func DropTemporaryCluster(sh engine.Shell, stepID string, report engine.Reporter, name, data string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var err error
	_, confErr := os.Stat(filepath.Join(pgConfRoot, name, "postgresql.conf"))
	_, dataErr := os.Stat(data)
	if confErr == nil && dataErr == nil {
		err = wrapTool("pg_dropcluster "+name, sh.Exec(ctx, stepID, engine.Command{Name: "pg_dropcluster", Args: []string{"18", name, "--stop"}}, report))
	}
	if err == nil {
		err = errors.Join(os.RemoveAll(data), os.RemoveAll(filepath.Join(pgConfRoot, name)))
	}
	_ = os.Remove(filepath.Join(TemporaryPortDir, name))
	return err
}

// DrillName is the temporary cluster a restore drill of db materialises
// into (RFC 9.4).
func DrillName(db string) string { return db + "__drill" }

// RestoringName is the temporary cluster a restore of db materialises into
// (RFC 10.1).
func RestoringName(db string) string { return db + "__restoring" }

// PreRestoreName is the cluster a promote or undo leaves the replaced data
// in: <db>__pre_restore_<UTC yyyymmddThhmmss>. Cluster names have no
// 63-byte limit, so any database name fits.
func PreRestoreName(db string, at time.Time) string {
	return db + "__pre_restore_" + at.UTC().Format("20060102T150405")
}

// LiveDatabase is the database a temporary or leftover cluster name
// (<db>__restoring, <db>__drill, <db>__pre_restore_<ts>) is a copy of, or
// name itself. The copy holds the original's roles, so its owner role is
// the live database's.
func LiveDatabase(name string) string {
	for _, suffix := range []string{"__restoring", "__drill"} {
		if db, ok := strings.CutSuffix(name, suffix); ok && db != "" {
			return db
		}
	}
	if i := strings.LastIndex(name, "__pre_restore_"); i > 0 {
		return name[:i]
	}
	return name
}

// Initdb creates an empty cluster in the data directory, with the live
// clusters' initdb options (data checksums, peer and SCRAM authentication,
// the locale and its provider), registers it and starts it: the target of
// a restore from a dump.
func (m *Materialised) Initdb(ctx context.Context, provider, locale string) error {
	if err := m.makeData(); err != nil {
		return err
	}
	args := []string{"-u", "postgres", "--", pgBin + "/initdb", "-D", m.Data, "--data-checksums",
		"--auth-local=peer", "--auth-host=scram-sha-256", "--locale=" + locale, "--locale-provider=" + provider}
	switch provider {
	case "builtin":
		args = append(args, "--builtin-locale="+locale)
	case "icu":
		args = append(args, "--icu-locale="+locale)
	}
	m.log("creating an empty cluster in " + m.Data)
	if err := m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "runuser", Args: args}, m.Report); err != nil {
		return wrapTool("initdb of "+m.Name, err)
	}
	// The registration in /etc/postgresql holds the configuration, as
	// pg_createcluster leaves it.
	for _, name := range []string{"postgresql.conf", "pg_hba.conf", "pg_ident.conf"} {
		if err := os.Remove(filepath.Join(m.Data, name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := m.register(); err != nil {
		return err
	}
	return m.start(ctx, UntilOpen)
}

// CopyRoles copies every role of the live source into the temporary
// cluster with pg_dumpall --roles-only: password verifiers, memberships and
// per-role settings included, so credentials don't change. The output never
// reaches the log: it carries the verifiers.
func (m *Materialised) CopyRoles(ctx context.Context) error {
	var buf bytes.Buffer
	if err := m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", pgBin + "/pg_dumpall", "-h", pgSocketDir, "-p", strconv.Itoa(m.SourcePort), "--roles-only", "--no-password",
	}, Stdout: &buf}, m.Report); err != nil {
		return wrapTool("pg_dumpall --roles-only of "+m.Source, err)
	}
	// postgres exists already in a fresh cluster; its ALTER ROLE stays.
	var sql strings.Builder
	for _, line := range strings.SplitAfter(buf.String(), "\n") {
		if strings.TrimSpace(line) != "CREATE ROLE postgres;" {
			sql.WriteString(line)
		}
	}
	m.log("copying the roles of " + m.Source)
	return m.PG().Exec(ctx, "postgres", sql.String(), nil)
}

// Swap puts the data directory of cluster from (a stopped or running
// temporary cluster, or a leftover) in the place of live cluster live's, in
// a few seconds, and keeps live's old data directory as cluster aside on
// port, stopped. Both clusters are stopped first; the caller starts live
// again with StartCluster. Both directories are under dataRoot, one
// filesystem, so the renames are atomic. The configuration in
// /etc/postgresql/18/<live> (port, memory, archive_command, cgroup) stays
// live's: only the data moves.
//
// aside is registered before the renames, so a failure after them never
// leaves its data unregistered. Swap resumes a swap that stopped part way
// (the caller records aside first and passes the same names again): after
// the first rename it does the second, after both it finishes the rest.
func Swap(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, dataRoot, live, from, aside string, port int) error {
	liveData, fromData, asideData := filepath.Join(dataRoot, live), filepath.Join(dataRoot, from), filepath.Join(dataRoot, aside)
	leftover := &Materialised{Name: aside, Source: live, Data: asideData, Port: port}
	switch SwapState(dataRoot, live, from, aside) {
	case SwapDone:
		return finishSwap(leftover, liveData, fromData, from)
	case SwapHalfway:
		if err := os.Rename(fromData, liveData); err != nil {
			return err
		}
		return finishSwap(leftover, liveData, fromData, from)
	case SwapConflict:
		return output.NewError(output.CodePrecondition, asideData+" already exists", output.ExitPrecondition,
			"Retry in a second; the name carries the time.").WithDetails(map[string]any{"reason": "cluster_exists", "cluster": aside})
	}
	if err := sh.Exec(ctx, stepID, engine.Command{Name: "systemctl", Args: []string{"stop", "postgresql@18-" + live}}, report); err != nil {
		return wrapTool("stop "+live, err)
	}
	if dataRunning(ctx, fromData) {
		if err := sh.Exec(ctx, stepID, engine.Command{Name: "pg_ctlcluster", Args: []string{"18", from, "stop", "-m", "fast"}}, report); err != nil {
			return wrapTool("stop "+from, err)
		}
	}
	if err := leftover.register(); err != nil {
		return errors.Join(err, os.RemoveAll(leftover.ConfDir()))
	}
	if err := os.Rename(liveData, asideData); err != nil {
		return errors.Join(err, os.RemoveAll(leftover.ConfDir()))
	}
	if err := os.Rename(fromData, liveData); err != nil {
		if back := os.Rename(asideData, liveData); back != nil {
			return errors.Join(err, back)
		}
		return errors.Join(err, os.RemoveAll(leftover.ConfDir()))
	}
	return finishSwap(leftover, liveData, fromData, from)
}

// finishSwap is what follows the renames, each part safe to run again:
// the moved data's postgresql.auto.conf points at its new path, the aside
// is registered (again), from's port reservation becomes aside's and from's
// registration goes.
func finishSwap(leftover *Materialised, liveData, fromData, from string) error {
	if err := repointAutoConf(liveData, fromData); err != nil {
		return err
	}
	if err := leftover.register(); err != nil {
		return err
	}
	_ = os.Rename(filepath.Join(TemporaryPortDir, from), filepath.Join(TemporaryPortDir, leftover.Name))
	return os.RemoveAll(filepath.Join(pgConfRoot, from))
}

// UnregisterAside removes the registration Swap wrote for aside before a
// rename that never happened: only when aside has no data directory.
func UnregisterAside(dataRoot, aside string) error {
	if _, err := os.Stat(filepath.Join(dataRoot, aside)); err == nil || !os.IsNotExist(err) {
		return err
	}
	return os.RemoveAll(filepath.Join(pgConfRoot, aside))
}

// Where a swap of live, from and aside stands, from their data directories.
const (
	// SwapNotStarted: no data directory was renamed yet.
	SwapNotStarted = "not_started"
	// SwapHalfway: live's data is aside, from's isn't moved yet.
	SwapHalfway = "halfway"
	// SwapDone: from's data is live's, live's old data is aside.
	SwapDone = "done"
	// SwapConflict: all three exist, so aside is some other cluster's.
	SwapConflict = "conflict"
)

// SwapState tells where a swap stands (see the Swap constants).
func SwapState(dataRoot, live, from, aside string) string {
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(dataRoot, name))
		return err == nil
	}
	liveOK, fromOK, asideOK := exists(live), exists(from), exists(aside)
	switch {
	case !asideOK:
		return SwapNotStarted
	case liveOK && !fromOK:
		return SwapDone
	case !liveOK && fromOK:
		return SwapHalfway
	case liveOK && fromOK:
		return SwapConflict
	}
	// Neither live nor from: nothing to swap, and the caller's own checks
	// report it.
	return SwapConflict
}

// repointAutoConf rewrites the old data directory's path in the moved data
// directory's postgresql.auto.conf to the new one: pgBackRest's
// restore_command carries --pg1-path, and archive-get reads pg_control
// there.
func repointAutoConf(data, oldData string) error {
	path := filepath.Join(data, "postgresql.auto.conf")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a data directory vaire moved.
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !bytes.Contains(raw, []byte(oldData)) {
		return nil
	}
	return engine.WriteFileAtomicOwned(path, bytes.ReplaceAll(raw, []byte(oldData), []byte(data)), 0o600, "postgres", "postgres")
}

// StartCluster starts live cluster name through its systemd unit, so it
// runs in its cgroup, and waits until it accepts connections on port and is
// where until says (paused at a restore's target, or open).
func StartCluster(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, name, data string, port int, until string) error {
	if err := sh.Exec(ctx, stepID, engine.Command{Name: "systemctl", Args: []string{"start", "postgresql@18-" + name}}, report); err != nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("start %s: %w", name, err), output.ExitExternal,
			"Read details.log_tail.").WithDetails(map[string]any{"log_tail": clusterLogTail(name, 20)})
	}
	return waitCluster(ctx, name, data, port, until, func(line string) {
		if report != nil {
			report(output.Event{Type: "log", Step: stepID, Line: line})
		}
	})
}

// recoverySettings are the archive recovery and recovery target settings,
// everything pgBackRest's restore writes into postgresql.auto.conf for any
// restore type. Promote removes them all, so the promoted cluster runs with
// its own configuration only and they don't travel with the data directory
// into a later undo.
var recoverySettings = []string{
	"restore_command", "archive_cleanup_command", "recovery_end_command",
	"recovery_target", "recovery_target_name", "recovery_target_time", "recovery_target_xid", "recovery_target_lsn",
	"recovery_target_inclusive", "recovery_target_timeline", "recovery_target_action",
}

// recoveryHeader starts the comment pgBackRest writes above its recovery
// settings; it goes with them.
const recoveryHeader = "# Recovery settings generated by pgBackRest"

// Promote ends recovery on the cluster at local, whose data directory is
// data, with pg_promote(), which opens it on a new timeline, then removes
// the recovery settings from its postgresql.auto.conf and reloads. ALTER
// SYSTEM is off on vaire's clusters (allow_alter_system), so the file is
// rewritten directly. A cluster already out of recovery is only cleaned up.
// It returns the settings it removed: the recovery targets among them are
// postmaster settings, which only a restart drops (SettlePromoted).
func Promote(ctx context.Context, local pg.Local, data string) ([]string, error) {
	if err := local.Exec(ctx, "postgres", "SELECT pg_promote() WHERE pg_is_in_recovery();\n", nil); err != nil {
		return nil, err
	}
	rows, err := local.Query(ctx, "postgres", "SELECT pg_is_in_recovery()::text AS r", nil)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0]["r"] != "false" {
		return nil, errors.New("pg_promote() did not end recovery within 60 seconds")
	}
	dropped, err := dropRecoverySettings(data)
	if err != nil {
		return nil, err
	}
	return dropped, local.Exec(ctx, "postgres", "SELECT pg_reload_conf();\n", nil)
}

// SettlePromoted restarts the live cluster name, which Promote just
// promoted, when a recovery setting it removed is one a reload can't drop.
// recovery_target_timeline, recovery_target_time and the other targets are
// postmaster settings: after the reload pg_settings keeps them
// pending_restart, and pg_file_settings reports "cannot be changed without
// restarting the server", which postgres-config-v1's check of the rendered
// configuration refuses. After the restart the cluster runs with exactly
// its rendered configuration and nothing waits for a restart. dropped is
// what Promote removed; a promote resumed after its removal finds the
// settings still pending instead.
func SettlePromoted(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, local pg.Local, name, data string, port int, dropped []string) error {
	restartFor, err := postmasterSettings(ctx, local, dropped)
	if err != nil {
		return err
	}
	pending, err := pendingRecovery(ctx, local)
	if err != nil {
		return err
	}
	for _, p := range pending {
		if !slices.Contains(restartFor, p) {
			restartFor = append(restartFor, p)
		}
	}
	if len(restartFor) == 0 {
		return nil
	}
	if report != nil {
		report(output.Event{Type: "log", Step: stepID, Line: "restarting " + name + " so it runs without the recovery settings " + strings.Join(restartFor, ", ")})
	}
	if err := sh.Exec(ctx, stepID, engine.Command{Name: "systemctl", Args: []string{"restart", "postgresql@18-" + name}}, report); err != nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("restart %s: %w", name, err), output.ExitExternal,
			"Read details.log_tail.").WithDetails(map[string]any{"log_tail": clusterLogTail(name, 20)})
	}
	if err := waitCluster(ctx, name, data, port, UntilOpen, func(line string) {
		if report != nil {
			report(output.Event{Type: "log", Step: stepID, Line: line})
		}
	}); err != nil {
		return err
	}
	if pending, err := pendingRecovery(ctx, local); err != nil {
		return err
	} else if len(pending) > 0 {
		return fmt.Errorf("%s still waits for a restart for %s after one", name, strings.Join(pending, ", "))
	}
	return nil
}

// postmasterSettings are the settings among names that only a restart
// changes. names are recoverySettings entries, so they quote as they are.
func postmasterSettings(ctx context.Context, local pg.Local, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		quoted = append(quoted, "'"+n+"'")
	}
	rows, err := local.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE context = 'postmaster' AND name IN ("+strings.Join(quoted, ", ")+") ORDER BY name", nil)
	if err != nil {
		return nil, err
	}
	return column(rows, "name"), nil
}

// pendingRecovery are the recovery settings that wait for a restart.
func pendingRecovery(ctx context.Context, local pg.Local) ([]string, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
	if err != nil {
		return nil, err
	}
	return recoveryOnly(column(rows, "name")), nil
}

// recoveryOnly keeps the recovery settings among names: any other pending
// restart was not this promote's.
func recoveryOnly(names []string) []string {
	var out []string
	for _, n := range names {
		if slices.Contains(recoverySettings, n) {
			out = append(out, n)
		}
	}
	return out
}

func column(rows []map[string]string, name string) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r[name])
	}
	return out
}

// dropRecoverySettings removes the recovery settings from the data
// directory's postgresql.auto.conf and returns the ones it removed.
func dropRecoverySettings(data string) ([]string, error) {
	path := filepath.Join(data, "postgresql.auto.conf")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a data directory vaire manages.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	kept, dropped := withoutRecoverySettings(string(raw))
	if kept == string(raw) {
		return dropped, nil
	}
	return dropped, engine.WriteFileAtomicOwned(path, []byte(kept), 0o600, "postgres", "postgres")
}

// withoutRecoverySettings is a postgresql.auto.conf without its recovery
// settings and pgBackRest's comment above them, and the settings removed.
func withoutRecoverySettings(raw string) (string, []string) {
	var kept, dropped []string
	for _, line := range strings.SplitAfter(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, recoveryHeader) {
			continue
		}
		key, _, _ := strings.Cut(trimmed, "=")
		if key = strings.TrimSpace(key); slices.Contains(recoverySettings, key) {
			if !slices.Contains(dropped, key) {
				dropped = append(dropped, key)
			}
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, ""), dropped
}

// Timeline is the timeline the open cluster at local writes WAL on, read
// from its current WAL file name, in decimal as --target-timeline takes it.
func Timeline(ctx context.Context, local pg.Local) (string, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT substr(pg_walfile_name(pg_current_wal_lsn()), 1, 8) AS t", nil)
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", errors.New("the cluster returned no WAL position")
	}
	n, err := strconv.ParseInt(rows[0]["t"], 16, 64)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(n, 10), nil
}
