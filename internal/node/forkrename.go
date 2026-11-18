package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/pg"
)

const (
	// forkRenameTerminateTimeout bounds how long pg_terminate_backend waits
	// for one backend. It must stay positive: a zero timeout does not wait
	// in PostgreSQL, so it cannot express the required bounded wait.
	forkRenameTerminateTimeout = 2 * time.Second
	// forkRenameDrainTimeout bounds the whole quiescence attempt, including
	// every termination round and the verification reads.
	forkRenameDrainTimeout = 20 * time.Second
	// forkRenameDrainRounds bounds a drain even when a session reconnects
	// between termination rounds.
	forkRenameDrainRounds = 4
	// forkRenameCompensateTimeout bounds restoring the original
	// datallowconn value after an ordinary failure, cancellation or an
	// ambiguous commit.
	forkRenameCompensateTimeout = 10 * time.Second
)

// forkRenameCopy is the only SQL endpoint a fork rename may mutate: the
// ownership-bound temporary copy. Its constructor requires the Materialised
// copy, its reserved temporary port and its private data directory, so a
// caller cannot accidentally point the rename at the live cluster or the
// source.
type forkRenameCopy struct {
	local   sqlRunner
	port    int
	dataDir string
}

// newForkRenameCopy binds the rename to the owned temporary copy. The live
// cluster's port and the source's port are refused even though they are also
// outside the temporary range; the range check alone would already exclude
// the default 5432 endpoint.
func newForkRenameCopy(m *backup.Materialised, livePort int) (forkRenameCopy, error) {
	if m == nil {
		return forkRenameCopy{}, errors.New("fork rename needs its owned copy")
	}

	if m.Port < config.TemporaryPortMin || m.Port > config.TemporaryPortMax {
		return forkRenameCopy{}, fmt.Errorf("fork rename refuses port %d outside the temporary range", m.Port)
	}

	if livePort <= 0 {
		return forkRenameCopy{}, errors.New("fork rename needs the destination port")
	}

	if m.Port == m.SourcePort || m.Port == livePort {
		return forkRenameCopy{}, errors.New("fork rename refuses the source or destination port")
	}

	dataDir := strings.TrimSpace(m.Data)
	if dataDir == "" || !filepath.IsAbs(dataDir) {
		return forkRenameCopy{}, errors.New("fork rename needs the copy's absolute data directory")
	}

	return forkRenameCopy{local: m.PG(), port: m.Port, dataDir: filepath.Clean(dataDir)}, nil
}

// renameFork renames the source's database and roles in the promoted fork copy.
// A fork's copy always has every old name, so it renames them all; db unfollow
// reads which are left instead. It is private to the fork materialisation: it
// never accepts a general SQL runner, so it cannot be aimed at the source or
// the eventual live cluster.
func renameFork(ctx context.Context, m *backup.Materialised, source, name string, passwords map[string]string, livePort int) error {
	if m == nil || source != m.Source || name != m.Name {
		return errors.New("fork rename does not match its owned copy")
	}

	target, err := newForkRenameCopy(m, livePort)
	if err != nil {
		return err
	}

	return renameForkOnCopy(ctx, target, source, name, passwords)
}

// renameForkOnCopy performs the quiesced rename on an attested copy. The
// existing rename/password SQL is prepared before any mutation, then the
// database's original datallowconn is captured. The gate that refuses new
// connections is committed on its own, sessions are drained by catalog OID,
// and only then does one transaction rename the database and restore the
// original datallowconn before committing. An ordinary failure, cancellation
// or ambiguous commit after the gate restores the original flag by OID, never
// by guessing a database name.
func renameForkOnCopy(ctx context.Context, target forkRenameCopy, source, name string, passwords map[string]string) (err error) {
	if ctx == nil || target.local == nil {
		return errors.New("fork rename needs a context and its owned copy endpoint")
	}

	if source == "" || name == "" || source == name {
		return errors.New("fork rename needs distinct source and destination names")
	}

	statements, err := renameStatements(renameState{database: true, roles: renameRoles()}, source, name, passwords)
	if err != nil {
		return err
	}

	if err := target.attest(ctx); err != nil {
		return err
	}

	database, err := target.database(ctx, source, name)
	if err != nil {
		return err
	}

	if err := target.refusePrepared(ctx, database.oid); err != nil {
		return err
	}

	gated := false
	defer func() {
		if err == nil || !gated {
			return
		}

		compensateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forkRenameCompensateTimeout)
		compErr := restoreForkRenameAllowConnections(compensateCtx, target, database.oid, database.allowConnections)

		cancel()

		err = errors.Join(err, compErr)
	}()

	gated = true

	if err := target.setAllowConnections(ctx, database.name, false); err != nil {
		return fmt.Errorf("refusing new connections to the owned copy's database: %w", err)
	}

	if err := target.verifyAllowConnections(ctx, database.oid, false); err != nil {
		return err
	}

	if err := drainForkRenameSessions(ctx, target, database.oid); err != nil {
		return err
	}

	if err := target.rename(ctx, statements, database, name); err != nil {
		return err
	}

	gated = false

	return nil
}

// attest proves the SQL endpoint is the ownership-bound temporary copy, not
// the default socket, the source or the live destination.
func (t forkRenameCopy) attest(ctx context.Context) error {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT current_setting('port') AS port, current_setting('data_directory') AS data_dir", nil)
	if err != nil {
		return fmt.Errorf("attesting the owned copy's SQL endpoint: %w", err)
	}

	if len(rows) != 1 {
		return errors.New("the owned copy's SQL endpoint returned no unique identity")
	}

	port, err := strconv.Atoi(strings.TrimSpace(rows[0]["port"]))
	if err != nil || port != t.port {
		return fmt.Errorf("the SQL endpoint port %q is not the owned copy's port %d", rows[0]["port"], t.port)
	}

	dataDir := filepath.Clean(strings.TrimSpace(rows[0]["data_dir"]))
	if dataDir != t.dataDir {
		return fmt.Errorf("the SQL endpoint data directory %q is not the owned copy's %q", dataDir, t.dataDir)
	}

	return nil
}

// forkRenameDatabase is the pre-mutation catalog identity of the source
// database. The OID survives the rename and is the only key compensation uses.
type forkRenameDatabase struct {
	oid              string
	name             string
	allowConnections bool
}

// database captures the source database's exact OID and original
// datallowconn and refuses a destination that already exists.
func (t forkRenameCopy) database(ctx context.Context, source, name string) (forkRenameDatabase, error) {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT oid::text AS oid, datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE datname IN ("+
			quoteLiteral(source)+", "+quoteLiteral(name)+") ORDER BY oid", nil)
	if err != nil {
		return forkRenameDatabase{}, fmt.Errorf("reading the owned copy's databases: %w", err)
	}

	var found map[string]string

	for _, row := range rows {
		switch row["name"] {
		case source:
			if found != nil {
				return forkRenameDatabase{}, errors.New("the owned copy has more than one source database")
			}

			found = row
		case name:
			return forkRenameDatabase{}, fmt.Errorf("destination database %s already exists in the owned copy", name)
		}
	}

	if found == nil {
		return forkRenameDatabase{}, fmt.Errorf("source database %s is missing from the owned copy", source)
	}

	oid, err := forkRenameOID(found["oid"])
	if err != nil {
		return forkRenameDatabase{}, err
	}

	currentName := strings.TrimSpace(found["name"])
	if currentName == "" {
		return forkRenameDatabase{}, errors.New("the owned copy's source database has no usable name")
	}

	allow, err := forkRenameBoolValue(found["allowconn"])
	if err != nil {
		return forkRenameDatabase{}, err
	}

	return forkRenameDatabase{oid: oid, name: currentName, allowConnections: allow}, nil
}

// refusePrepared refuses prepared transactions in the captured database: they
// are not backends and must never be terminated or guessed away. The prepared
// view names its database, so join it to pg_database and keep the captured OID
// authoritative.
func (t forkRenameCopy) refusePrepared(ctx context.Context, oid string) error {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT count(*)::text AS n FROM pg_prepared_xacts p JOIN pg_database d ON d.datname = p.database WHERE d.oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("reading the owned copy's prepared transactions: %w", err)
	}

	if len(rows) != 1 {
		return errors.New("the owned copy's prepared transactions returned no unique count")
	}

	if rows[0]["n"] != "0" {
		return fmt.Errorf("the owned copy's database oid %s has prepared transactions; refusing to rename", oid)
	}

	return nil
}

// drainForkRenameSessions terminates only sessions whose catalog datid is the
// captured source OID, then verifies none remain. It never selects by
// application name or a cached PID. The whole attempt is bounded by rounds and
// a context deadline; a reconnecting or otherwise unresolved backend refuses.
func drainForkRenameSessions(ctx context.Context, target forkRenameCopy, oid string) error {
	drainCtx, cancel := context.WithTimeout(ctx, forkRenameDrainTimeout)
	defer cancel()

	for range forkRenameDrainRounds {
		if err := target.refusePrepared(drainCtx, oid); err != nil {
			return err
		}

		pids, err := target.sessions(drainCtx, oid)
		if err != nil {
			return err
		}

		if len(pids) == 0 {
			return nil
		}

		if err := target.terminateSessions(drainCtx, oid); err != nil {
			return err
		}
	}

	if err := target.refusePrepared(drainCtx, oid); err != nil {
		return err
	}

	pids, err := target.sessions(drainCtx, oid)
	if err != nil {
		return err
	}

	if len(pids) != 0 {
		return fmt.Errorf("the owned copy's database oid %s still has %d unresolved session(s)", oid, len(pids))
	}

	return nil
}

// sessions lists the backends connected to the captured database OID.
func (t forkRenameCopy) sessions(ctx context.Context, oid string) ([]string, error) {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT pid::text AS pid FROM pg_stat_activity WHERE datid = "+oid+" ORDER BY pid", nil)
	if err != nil {
		return nil, fmt.Errorf("reading the owned copy's sessions: %w", err)
	}

	pids := make([]string, 0, len(rows))
	for _, row := range rows {
		pid := strings.TrimSpace(row["pid"])
		if _, err := strconv.ParseUint(pid, 10, 32); err != nil {
			return nil, errors.New("the owned copy's sessions returned an invalid pid")
		}

		pids = append(pids, pid)
	}

	return pids, nil
}

// terminateSessions asks PostgreSQL to end the captured database OID's
// backends, waiting at most forkRenameTerminateTimeout for each.
func (t forkRenameCopy) terminateSessions(ctx context.Context, oid string) error {
	timeout := strconv.FormatInt(int64(forkRenameTerminateTimeout/time.Millisecond), 10)

	_, err := t.local.Query(ctx, "postgres",
		"SELECT pid::text AS pid, pg_terminate_backend(pid, "+timeout+")::text AS terminated FROM pg_stat_activity WHERE datid = "+oid+" ORDER BY pid", nil)
	if err != nil {
		return fmt.Errorf("terminating the owned copy's sessions: %w", err)
	}

	return nil
}

// setAllowConnections changes the named database's datallowconn. Callers pass
// only a name read back from the owned copy's catalog.
func (t forkRenameCopy) setAllowConnections(ctx context.Context, name string, allow bool) error {
	return t.local.Exec(ctx, "postgres",
		"ALTER DATABASE "+pg.Quote(name)+" ALLOW_CONNECTIONS "+forkRenameSQLBool(allow)+";", nil)
}

// verifyAllowConnections reads the captured OID back and proves its current
// datallowconn value.
func (t forkRenameCopy) verifyAllowConnections(ctx context.Context, oid string, want bool) error {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("reading the owned copy's database oid %s: %w", oid, err)
	}

	if len(rows) != 1 {
		return fmt.Errorf("the owned copy's database oid %s is missing", oid)
	}

	got, err := forkRenameBoolValue(rows[0]["allowconn"])
	if err != nil {
		return err
	}

	if got != want {
		return fmt.Errorf("the owned copy's database oid %s has allow_connections=%t, want %t", oid, got, want)
	}

	return nil
}

// rename runs the existing rename/password SQL in one explicit transaction,
// restores the captured database's original datallowconn before commit and
// verifies the same OID now has the destination name and original flag. The
// gate is already a separate committed change; the rename's exclusive database
// lock is authoritative here.
func (t forkRenameCopy) rename(ctx context.Context, statements string, database forkRenameDatabase, name string) error {
	if !strings.HasSuffix(statements, "\n") {
		statements += "\n"
	}

	body := "BEGIN;\n" + statements +
		"ALTER DATABASE " + pg.Quote(name) + " ALLOW_CONNECTIONS " + forkRenameSQLBool(database.allowConnections) + ";\n" +
		"DO $vaire$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_database WHERE oid = " + database.oid +
		" AND datname = " + quoteLiteral(name) + " AND datallowconn = " + forkRenameSQLBool(database.allowConnections) +
		") THEN RAISE EXCEPTION 'fork rename postcondition failed'; END IF; END $vaire$;\nCOMMIT;\n"
	if err := t.local.Exec(ctx, "postgres", body, nil); err != nil {
		return fmt.Errorf("renaming %s to %s in its owned copy: %w", database.name, name, err)
	}

	return nil
}

// restoreForkRenameAllowConnections is the bounded compensation after the
// gate. It resolves the captured OID to its current name, so an ambiguous
// commit that already renamed the database is handled without enabling or
// fencing a guessed name.
func restoreForkRenameAllowConnections(ctx context.Context, target forkRenameCopy, oid string, allow bool) error {
	rows, err := target.local.Query(ctx, "postgres",
		"SELECT datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("compensating database oid %s: %w", oid, err)
	}

	if len(rows) != 1 {
		return fmt.Errorf("compensating database oid %s: the captured database is missing and stays fenced", oid)
	}

	current, err := forkRenameBoolValue(rows[0]["allowconn"])
	if err != nil {
		return err
	}

	if current == allow {
		return nil
	}

	name := strings.TrimSpace(rows[0]["name"])
	if name == "" {
		return fmt.Errorf("compensating database oid %s: its current name is empty", oid)
	}

	if err := target.setAllowConnections(ctx, name, allow); err != nil {
		return fmt.Errorf("compensating database oid %s: %w", oid, err)
	}

	return target.verifyAllowConnections(ctx, oid, allow)
}

func forkRenameOID(value string) (string, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || parsed == 0 {
		return "", errors.New("the owned copy's source database has no usable OID")
	}

	return strconv.FormatUint(parsed, 10), nil
}

func forkRenameBoolValue(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "t", "true", "1":
		return true, nil
	case "f", "false", "0":
		return false, nil
	}

	return false, errors.New("the owned copy's database has no usable allow_connections value")
}

func forkRenameSQLBool(value bool) string {
	if value {
		return "true"
	}

	return "false"
}
