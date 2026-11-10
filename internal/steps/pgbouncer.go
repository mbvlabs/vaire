package steps

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/secrets"
)

const pgbouncerID = "pgbouncer-v1"

type pgbouncer struct{}

func (pgbouncer) ID() string { return pgbouncerID }

func (pgbouncer) Describe(config.Cluster) string {
	return "render both poolers' configuration with an entry per database, the session pooler's unit and userlist.txt, then reload PgBouncer"
}

// SkipReason skips the step while there is no database: the service roles
// whose verifiers fill userlist.txt live in each database's cluster.
func (pgbouncer) SkipReason(cfg config.Cluster) string {
	if len(cfg.Clusters) == 0 {
		return "no_databases"
	}
	return ""
}

// pgbouncerIni is the transaction pooler's rendered ini, which never
// contains a secret.
func pgbouncerIni(cfg config.Cluster, rt engine.Runtime, dbs []render.PoolerDatabase) string {
	return string(render.PgbouncerIni(cfg, rt.Host, dbs))
}

// poolerDatabases is one [databases] entry per cluster, routed by name to its
// socket and capped at its pool for that pooler: pool_size for the
// transaction pooler, session_pool_size for the session pooler (RFC 8.1).
// An unfinished unfollow's alias is left out on both poolers until its
// privileges are settled: a reload or a pooler restart then cannot expose a
// promoted database whose renames or protection policy are still in flight.
// Both poolers are rendered from one records snapshot so they cannot disagree
// about which aliases the transition withholds.
func poolerDatabases(cfg config.Cluster, recs backup.Records, session bool) []render.PoolerDatabase {
	fenced := fencedPoolerAliases(recs)
	out := make([]render.PoolerDatabase, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		if fenced[c.Name] {
			continue
		}
		db := render.PoolerDatabase{Name: c.Name, Port: c.Port}
		if c.IsFollower() {
			// The follower's cluster holds its source's database (Phase 6).
			db.DBName = c.Follows
		}
		if r, err := cfg.Resources(c); err == nil {
			db.MaxConnections = r.PoolSize
			if session {
				db.MaxConnections = r.SessionPoolSize
			}
		}
		out = append(out, db)
	}
	if session {
		out = append(out, restoreEntries(cfg, recs)...)
	}
	return out
}

// fencedPoolerAliases are the databases whose unfollow has not settled its
// privileges yet. The record is durable, so a render or restart after an
// interruption keeps withholding them.
func fencedPoolerAliases(recs backup.Records) map[string]bool {
	var out map[string]bool
	for name, u := range recs.Unfollowing {
		if u.Settled {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[name] = true
	}
	return out
}

// poolerRecords is one read of records.json for rendering both poolers. A
// missing file is the node's legitimate empty initial state; any other read or
// decode failure is unknown transition state and stops the render rather than
// dropping an unfinished unfollow's fence. The aliases stay withheld until the
// records are readable again.
func poolerRecords() (backup.Records, error) {
	recs, err := backup.LoadRecords()
	if err != nil {
		return backup.Records{}, output.NewError(output.CodePrecondition,
			"the node's unfollow records could not be read, so the poolers' configuration cannot be rendered safely: "+err.Error(),
			output.ExitPrecondition,
			"Inspect the node's records file and fix it; vaire never resets it on its own.").
			WithDetails(map[string]any{"reason": "unfollow_state_unknown"})
	}
	return recs, nil
}

// restoreEntries route each ready restore's <db>__restoring to its
// temporary cluster, where the database is still called <db>, so vaire sql
// --db <db>__restoring reaches the paused copy (RFC 10.1). Only the session
// pooler has them; promote and abort reload it without them.
func restoreEntries(cfg config.Cluster, recs backup.Records) []render.PoolerDatabase {
	var out []render.PoolerDatabase
	for db, r := range recs.Restores {
		if _, ok := cfg.ClusterByName(db); !ok || r.Phase != backup.PhaseReady || r.Port == 0 {
			continue
		}
		out = append(out, render.PoolerDatabase{Name: backup.RestoringName(db), DBName: db, Port: r.Port, MaxConnections: restoreEntryPool})
	}
	return out
}

// restoreEntryPool caps the session pooler's connections to a restore's copy:
// enough to inspect it.
const restoreEntryPool = 5

// pgbouncerFile is one file the step owns, apart from the userlist.
type pgbouncerFile struct {
	path, content, owner, group string
	mode                        os.FileMode
}

// poolerFilePaths are the managed files pgbouncerFiles renders. Production
// values are the constants in pgcommon.go; they are variables so the
// fail-closed regression can use real files in a temporary directory.
var poolerFilePaths = struct {
	ini, hba, sessionIni, sessionHBA, sessionUnit string
}{
	ini: PgbouncerIniPath, hba: PgbouncerHBAPath, sessionIni: SessionIniPath,
	sessionHBA: SessionHBAPath, sessionUnit: SessionUnitPath,
}

// pgbouncerFiles are both poolers' configuration and the session pooler's
// unit (RFC 8.1). None of them contains a secret. The records are read once,
// before any file is rendered or written: an unfinished unfollow's alias and a
// ready restore are both taken from that one snapshot, and unreadable records
// fail the whole render closed. The normal step does not narrow its config
// here: the engine already handed it the view that keeps an explicitly owned
// follow or recovery target.
func pgbouncerFiles(cfg config.Cluster, rt engine.Runtime) ([]pgbouncerFile, error) {
	recs, err := poolerRecords()
	if err != nil {
		return nil, err
	}
	return pgbouncerFilesFrom(cfg, rt, recs), nil
}

// pgbouncerFilesFrom renders both poolers from one records snapshot.
func pgbouncerFilesFrom(cfg config.Cluster, rt engine.Runtime, recs backup.Records) []pgbouncerFile {
	return []pgbouncerFile{
		{poolerFilePaths.ini, pgbouncerIni(cfg, rt, poolerDatabases(cfg, recs, false)), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.hba, string(render.PgbouncerHBA()), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionIni, string(render.SessionIni(cfg, poolerDatabases(cfg, recs, true), SessionSocketDir)), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionHBA, string(render.SessionHBA()), pgFileOwner, pgFileGroup, 0o640},
		{poolerFilePaths.sessionUnit, string(render.SessionUnit()), "", "", 0o644},
	}
}

// writePoolerFile writes one of both poolers' managed files. It is a variable
// so the fail-closed regression can prove unreadable records leave the existing
// safe files untouched.
var writePoolerFile = writeFileOwned

func (pgbouncer) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		// Inputs runs before a plan renders a diff, so an unreadable records
		// file has already failed the plan; there is nothing safe to show.
		return nil
	}
	out := make([]engine.RenderedFile, 0, len(files))
	for _, f := range files {
		out = append(out, engine.RenderedFile{Path: f.path, Content: []byte(f.content)})
	}
	return out
}

func (pgbouncer) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	// The verifiers change on rotation without changing the desired state, so
	// they are not part of the inputs; Check re-renders them each time.
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return nil, err
	}
	rendered := map[string]string{}
	for _, f := range files {
		rendered[f.path] = f.content
	}
	return canonical(map[string]any{"files": rendered, "users": ServiceRoles})
}

func (pgbouncer) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return engine.CheckResult{}, err
	}
	for _, f := range files {
		if !fileIsOwned(f.path, f.content, f.mode, f.owner, f.group) {
			return incomplete(f.path + " differs"), nil
		}
	}
	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil || len(verifiers) < len(ServiceRoles) {
		return incomplete("cannot read the service role verifiers"), nil
	}
	if !fileIsOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup) {
		return incomplete(PgbouncerUserlistPath + " differs"), nil
	}
	for _, unit := range []string{PgbouncerService, SessionPoolerService} {
		if !serviceActive(ctx, unit) {
			return incomplete(unit + " is not active"), nil
		}
	}
	if err := pgbouncerResponds(ctx, PGSocketDir, PgbouncerPort); err != nil {
		return incomplete("the transaction pooler does not answer: " + err.Error()), nil
	}
	if err := pgbouncerResponds(ctx, SessionSocketDir, SessionPoolerPort); err != nil {
		return incomplete("the session pooler does not answer: " + err.Error()), nil
	}
	return complete(), nil
}

func (s pgbouncer) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	files, err := pgbouncerFiles(cfg, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if len(verifiers) < len(ServiceRoles) {
		return engine.ApplyResult{}, fmt.Errorf("the service role verifiers are missing; run postgres-roles-v1 first")
	}
	for _, f := range files {
		if err := writePoolerFile(f.path, f.content, f.mode, f.owner, f.group); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if err := writeFileOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("pgbouncer"), map[string]string{
		"PGBOUNCER_DIR":   PgbouncerDir,
		"SESSION_SERVICE": SessionPoolerService,
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// ApplyPoolerFence writes both poolers' rendered configuration and reloads
// them, without touching userlist.txt: db unfollow uses it to withhold an
// unsettled alias durably, and the service roles (and their verifiers) do not
// change. Unlike the pgbouncer-v1 step it always reloads, so a retry after a
// crash between the file writes and the reload still completes the cut.
//
// It is a full-configuration entry point, so it applies the routing lifecycle
// eligibility itself: a database awaiting recovery or an unfinished ordinary
// follow has no routable cluster and is withheld exactly as general apply would
// withhold it. Both the eligibility and the unsettled-unfollow aliases come
// from one records snapshot. The normal pgbouncer-v1 step must not use this
// narrowing: the engine already hands it a view that keeps an explicitly owned
// follow or recovery target. Unreadable records fail before any file is
// overwritten or a service is reloaded, so a retry never re-exposes an alias it
// cannot prove is safe.
func ApplyPoolerFence(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	recs, err := poolerRecords()
	if err != nil {
		return err
	}
	files := pgbouncerFilesFrom(cfg.RoutingView(recs.IsAwaiting, ""), rt, recs)
	for _, f := range files {
		if err := writePoolerFile(f.path, f.content, f.mode, f.owner, f.group); err != nil {
			return err
		}
	}
	return rt.Shell.Run(ctx, pgbouncerID, Script("pgbouncer"), map[string]string{
		"PGBOUNCER_DIR":   PgbouncerDir,
		"SESSION_SERVICE": SessionPoolerService,
	}, report)
}

// roleVerifiers reads the SCRAM verifiers of the service roles from the
// first cluster. They are the same in every cluster (RFC 12).
func roleVerifiers(ctx context.Context, cfg config.Cluster) (map[string]string, error) {
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("there is no cluster to read the service roles from")
	}
	rows, err := ClusterPG(cfg.Clusters[0]).Query(ctx, "postgres",
		"SELECT rolname, rolpassword FROM pg_authid WHERE rolname IN ('pgbouncer_auth','vaire_monitor')", nil)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		verifier := row["rolpassword"]
		if verifier == "" {
			continue
		}
		secrets.Add(verifier)
		out[row["rolname"]] = verifier
	}
	return out, nil
}

// pgbouncerResponds proves a pooler's admin console answers on its socket,
// using the monitor password the node keeps for the exporters.
func pgbouncerResponds(ctx context.Context, socketDir string, port int) error {
	password, err := os.ReadFile(RuntimeDir + "/monitor.pass")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "vaire-pgpass-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := fmt.Fprintf(f, "*:*:*:vaire_monitor:%s\n", strings.TrimSpace(string(password))); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return err
	}
	_, err = ProbeWithEnv(ctx, []string{"PGPASSFILE=" + name}, "psql",
		"-h", socketDir, "-p", strconv.Itoa(port), "-U", "vaire_monitor", "-d", "pgbouncer", "-tAc", "SHOW VERSION")
	return err
}

// poolerAdminTimeout bounds one admin console command. PAUSE waits for the
// database's running queries to finish, so it gets the longest.
const poolerAdminTimeout = 15 * time.Second

// poolerAdmin runs one admin console command on the pooler at port. It logs
// in as pgbouncer over the socket as the postgres OS user, which PgBouncer
// lets in without a password because it runs as that user.
func poolerAdmin(ctx context.Context, port int, command string) error {
	ctx, cancel := context.WithTimeout(ctx, poolerAdminTimeout)
	defer cancel()
	return runCmd(ctx, "runuser", "-u", "postgres", "--", "psql", "-X", "-q",
		"-h", PGSocketDir, "-p", strconv.Itoa(port), "-U", "pgbouncer", "-d", "pgbouncer", "-c", command)
}
