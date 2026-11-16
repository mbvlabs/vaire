package node

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/pg"
)

const (
	// unfollowRenameTerminateTimeout bounds how long pg_terminate_backend
	// waits for one backend. It must stay positive: a zero timeout does not
	// wait in PostgreSQL, so it cannot express the required bounded wait.
	unfollowRenameTerminateTimeout = 2 * time.Second
	// unfollowRenameDrainTimeout bounds the whole quiescence attempt,
	// including every termination round and the verification reads.
	unfollowRenameDrainTimeout = 20 * time.Second
	// unfollowRenameDrainRounds bounds a drain even when a session reconnects
	// between termination rounds.
	unfollowRenameDrainRounds = 4
	// unfollowRenameCompensateTimeout bounds restoring the original
	// datallowconn value after an ordinary failure, cancellation or an
	// ambiguous commit.
	unfollowRenameCompensateTimeout = 10 * time.Second
)

// unfollowRenameTarget is the only SQL endpoint a db unfollow rename may
// mutate: the follower's own promoted cluster. The cluster name, listening
// port and private data directory are read from cluster.toml, and attestation
// proves the endpoint is that cluster before any rename. It is never the
// source cluster and never a temporary fork copy.
type unfollowRenameTarget struct {
	local   sqlRunner
	cluster string
	port    int
	dataDir string
	// sourcePort and sourceDataDir are the source cluster's endpoint, so the
	// rename can refuse it explicitly even if a caller mixed up the specs.
	sourcePort    int
	sourceDataDir string
}

// validate checks the endpoint's ownership identity without touching SQL. The
// follower cluster and its source are distinct clusters even when they share a
// node and socket directory.
func (t unfollowRenameTarget) validate(name string) error {
	if t.local == nil {
		return errors.New("unfollow rename needs the follower cluster's SQL endpoint")
	}
	if name == "" || t.cluster != name {
		return errors.New("unfollow rename does not match the follower cluster")
	}
	if t.port <= 0 {
		return errors.New("unfollow rename needs the follower cluster's port")
	}
	if t.sourcePort <= 0 {
		return errors.New("unfollow rename needs the source cluster's port")
	}
	if t.port == t.sourcePort {
		return errors.New("unfollow rename refuses the source cluster's port")
	}
	dataDir := strings.TrimSpace(t.dataDir)
	if dataDir == "" || !filepath.IsAbs(dataDir) {
		return errors.New("unfollow rename needs the follower cluster's absolute data directory")
	}
	sourceDir := strings.TrimSpace(t.sourceDataDir)
	if sourceDir == "" || !filepath.IsAbs(sourceDir) {
		return errors.New("unfollow rename needs the source cluster's absolute data directory")
	}
	if filepath.Clean(dataDir) == filepath.Clean(sourceDir) {
		return errors.New("unfollow rename refuses the source cluster's data directory")
	}
	return nil
}

// attest proves the SQL endpoint is the follower's promoted cluster, not the
// source or another cluster reached through the shared socket directory.
func (t unfollowRenameTarget) attest(ctx context.Context, name string) error {
	if err := t.validate(name); err != nil {
		return err
	}
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT current_setting('port') AS port, current_setting('data_directory') AS data_dir", nil)
	if err != nil {
		return fmt.Errorf("attesting the follower cluster's SQL endpoint: %w", err)
	}
	if len(rows) != 1 {
		return errors.New("the follower cluster's SQL endpoint returned no unique identity")
	}
	port, err := strconv.Atoi(strings.TrimSpace(rows[0]["port"]))
	if err != nil || port != t.port {
		return fmt.Errorf("the SQL endpoint port %q is not the follower cluster's port %d", rows[0]["port"], t.port)
	}
	dataDir := filepath.Clean(strings.TrimSpace(rows[0]["data_dir"]))
	if dataDir != filepath.Clean(t.dataDir) {
		return fmt.Errorf("the SQL endpoint data directory %q is not the follower cluster's %q", dataDir, t.dataDir)
	}
	return nil
}

// unfollowRenameDatabase is the pre-mutation catalog identity of the database
// still under the source's name. The OID survives the rename and is the only
// key the gate, drain and compensation use.
type unfollowRenameDatabase struct {
	oid              string
	name             string
	allowConnections bool
}

// database captures the source database's exact OID and original
// datallowconn. readRenames has already proved the database still has the
// source's name and the destination does not exist; this reads the identity
// the gate and compensation must keep.
func (t unfollowRenameTarget) database(ctx context.Context, source string) (unfollowRenameDatabase, error) {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT oid::text AS oid, datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE datname IN ("+
			quoteLiteral(source)+") ORDER BY oid", nil)
	if err != nil {
		return unfollowRenameDatabase{}, fmt.Errorf("reading the follower cluster's source database: %w", err)
	}
	if len(rows) != 1 {
		return unfollowRenameDatabase{}, fmt.Errorf("source database %s has no unique identity in the follower cluster", source)
	}
	oid, err := unfollowRenameOID(rows[0]["oid"])
	if err != nil {
		return unfollowRenameDatabase{}, err
	}
	currentName := strings.TrimSpace(rows[0]["name"])
	if currentName == "" {
		return unfollowRenameDatabase{}, errors.New("the follower cluster's source database has no usable name")
	}
	allow, err := unfollowRenameBoolValue(rows[0]["allowconn"])
	if err != nil {
		return unfollowRenameDatabase{}, err
	}
	return unfollowRenameDatabase{oid: oid, name: currentName, allowConnections: allow}, nil
}

// refusePrepared refuses prepared transactions in the captured database: they
// are not backends, cannot be terminated, and would block the rename. The
// prepared view names its database, so join it to pg_database and keep the
// captured OID authoritative.
func (t unfollowRenameTarget) refusePrepared(ctx context.Context, oid string) error {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT count(*)::text AS n FROM pg_prepared_xacts p JOIN pg_database d ON d.datname = p.database WHERE d.oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("reading the follower cluster's prepared transactions: %w", err)
	}
	if len(rows) != 1 {
		return errors.New("the follower cluster's prepared transactions returned no unique count")
	}
	if rows[0]["n"] != "0" {
		return fmt.Errorf("the follower cluster's database oid %s has prepared transactions; refusing to rename", oid)
	}
	return nil
}

// drainUnfollowRenameSessions terminates only sessions whose catalog datid is
// the captured source OID, then verifies none remain. It never selects by
// application name or a cached PID, and it runs on the follower's own cluster,
// so the source cluster's sessions are never candidates. The whole attempt is
// bounded by rounds and a context deadline; a reconnecting or otherwise
// unresolved backend refuses.
func drainUnfollowRenameSessions(ctx context.Context, target unfollowRenameTarget, oid string) error {
	drainCtx, cancel := context.WithTimeout(ctx, unfollowRenameDrainTimeout)
	defer cancel()
	for round := 0; round < unfollowRenameDrainRounds; round++ {
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
		return fmt.Errorf("the follower cluster's database oid %s still has %d unresolved session(s)", oid, len(pids))
	}
	return nil
}

// sessions lists the backends connected to the captured database OID.
func (t unfollowRenameTarget) sessions(ctx context.Context, oid string) ([]string, error) {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT pid::text AS pid FROM pg_stat_activity WHERE datid = "+oid+" ORDER BY pid", nil)
	if err != nil {
		return nil, fmt.Errorf("reading the follower cluster's sessions: %w", err)
	}
	pids := make([]string, 0, len(rows))
	for _, row := range rows {
		pid := strings.TrimSpace(row["pid"])
		if _, err := strconv.ParseUint(pid, 10, 32); err != nil {
			return nil, errors.New("the follower cluster's sessions returned an invalid pid")
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// terminateSessions asks PostgreSQL to end the captured database OID's
// backends, waiting at most unfollowRenameTerminateTimeout for each.
func (t unfollowRenameTarget) terminateSessions(ctx context.Context, oid string) error {
	timeout := strconv.FormatInt(int64(unfollowRenameTerminateTimeout/time.Millisecond), 10)
	_, err := t.local.Query(ctx, "postgres",
		"SELECT pid::text AS pid, pg_terminate_backend(pid, "+timeout+")::text AS terminated FROM pg_stat_activity WHERE datid = "+oid+" ORDER BY pid", nil)
	if err != nil {
		return fmt.Errorf("terminating the follower cluster's sessions: %w", err)
	}
	return nil
}

// setAllowConnections changes the named database's datallowconn. Callers pass
// only a name read back from the follower cluster's catalog.
func (t unfollowRenameTarget) setAllowConnections(ctx context.Context, name string, allow bool) error {
	return t.local.Exec(ctx, "postgres",
		"ALTER DATABASE "+pg.Quote(name)+" ALLOW_CONNECTIONS "+unfollowRenameSQLBool(allow)+";", nil)
}

// verifyAllowConnections reads the captured OID back and proves its current
// datallowconn value.
func (t unfollowRenameTarget) verifyAllowConnections(ctx context.Context, oid string, want bool) error {
	rows, err := t.local.Query(ctx, "postgres",
		"SELECT datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("reading the follower cluster's database oid %s: %w", oid, err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("the follower cluster's database oid %s is missing", oid)
	}
	got, err := unfollowRenameBoolValue(rows[0]["allowconn"])
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("the follower cluster's database oid %s has allow_connections=%t, want %t", oid, got, want)
	}
	return nil
}

// rename runs the existing rename/password SQL in one explicit transaction,
// restores the captured database's original datallowconn before commit and
// verifies the same OID now has the destination name and original flag. The
// gate is already a separate committed change; the rename's exclusive database
// lock is authoritative here.
func (t unfollowRenameTarget) rename(ctx context.Context, statements string, database unfollowRenameDatabase, name string) error {
	if !strings.HasSuffix(statements, "\n") {
		statements += "\n"
	}
	body := "BEGIN;\n" + statements +
		"ALTER DATABASE " + pg.Quote(name) + " ALLOW_CONNECTIONS " + unfollowRenameSQLBool(database.allowConnections) + ";\n" +
		"DO $vaire$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_database WHERE oid = " + database.oid +
		" AND datname = " + quoteLiteral(name) + " AND datallowconn = " + unfollowRenameSQLBool(database.allowConnections) +
		") THEN RAISE EXCEPTION 'unfollow rename postcondition failed'; END IF; END $vaire$;\nCOMMIT;\n"
	if err := t.local.Exec(ctx, "postgres", body, nil); err != nil {
		return fmt.Errorf("renaming %s to %s in the follower cluster: %w", database.name, name, err)
	}
	return nil
}

// restoreUnfollowRenameAllowConnections is the bounded compensation after the
// gate. It resolves the captured OID to its current name, so an ambiguous
// commit that already renamed the database is handled without enabling or
// fencing a guessed name.
func restoreUnfollowRenameAllowConnections(ctx context.Context, target unfollowRenameTarget, oid string, allow bool) error {
	rows, err := target.local.Query(ctx, "postgres",
		"SELECT datname AS name, datallowconn::text AS allowconn FROM pg_database WHERE oid = "+oid, nil)
	if err != nil {
		return fmt.Errorf("compensating follower database oid %s: %w", oid, err)
	}
	if len(rows) != 1 {
		return fmt.Errorf("compensating follower database oid %s: the captured database is missing and stays fenced", oid)
	}
	current, err := unfollowRenameBoolValue(rows[0]["allowconn"])
	if err != nil {
		return err
	}
	if current == allow {
		return nil
	}
	name := strings.TrimSpace(rows[0]["name"])
	if name == "" {
		return fmt.Errorf("compensating follower database oid %s: its current name is empty", oid)
	}
	if err := target.setAllowConnections(ctx, name, allow); err != nil {
		return fmt.Errorf("compensating follower database oid %s: %w", oid, err)
	}
	return target.verifyAllowConnections(ctx, oid, allow)
}

func unfollowRenameOID(value string) (string, error) {
	parsed, err := strconv.ParseUint(strings.TrimSpace(value), 10, 32)
	if err != nil || parsed == 0 {
		return "", errors.New("the follower cluster's source database has no usable OID")
	}
	return strconv.FormatUint(parsed, 10), nil
}

func unfollowRenameBoolValue(value string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "t", "true", "1":
		return true, nil
	case "f", "false", "0":
		return false, nil
	}
	return false, errors.New("the follower cluster's database has no usable allow_connections value")
}

func unfollowRenameSQLBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}
