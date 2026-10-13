package steps

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/secrets"
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
func pgbouncerIni(cfg config.Cluster, rt engine.Runtime) string {
	return string(render.PgbouncerIni(cfg, rt.Host, poolerDatabases(cfg, false)))
}

// poolerDatabases is one [databases] entry per cluster, routed by name to its
// socket and capped at its pool for that pooler: pool_size for the
// transaction pooler, session_pool_size for the session pooler (RFC 8.1).
func poolerDatabases(cfg config.Cluster, session bool) []render.PoolerDatabase {
	out := make([]render.PoolerDatabase, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		db := render.PoolerDatabase{Name: c.Name, Port: c.Port}
		if r, err := cfg.Resources(c); err == nil {
			db.MaxConnections = r.PoolSize
			if session {
				db.MaxConnections = r.SessionPoolSize
			}
		}
		out = append(out, db)
	}
	return out
}

// pgbouncerFile is one file the step owns, apart from the userlist.
type pgbouncerFile struct {
	path, content, owner, group string
	mode                        os.FileMode
}

// pgbouncerFiles are both poolers' configuration and the session pooler's
// unit (RFC 8.1). None of them contains a secret.
func pgbouncerFiles(cfg config.Cluster, rt engine.Runtime) []pgbouncerFile {
	return []pgbouncerFile{
		{PgbouncerIniPath, pgbouncerIni(cfg, rt), pgFileOwner, pgFileGroup, 0o640},
		{PgbouncerHBAPath, string(render.PgbouncerHBA()), pgFileOwner, pgFileGroup, 0o640},
		{SessionIniPath, string(render.SessionIni(cfg, poolerDatabases(cfg, true), SessionSocketDir)), pgFileOwner, pgFileGroup, 0o640},
		{SessionHBAPath, string(render.SessionHBA()), pgFileOwner, pgFileGroup, 0o640},
		{SessionUnitPath, string(render.SessionUnit()), "", "", 0o644},
	}
}

func (pgbouncer) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	files := pgbouncerFiles(cfg, rt)
	out := make([]engine.RenderedFile, 0, len(files))
	for _, f := range files {
		out = append(out, engine.RenderedFile{Path: f.path, Content: []byte(f.content)})
	}
	return out
}

func (pgbouncer) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	// The verifiers change on rotation without changing the desired state, so
	// they are not part of the inputs; Check re-renders them each time.
	files := map[string]string{}
	for _, f := range pgbouncerFiles(cfg, rt) {
		files[f.path] = f.content
	}
	return canonical(map[string]any{"files": files, "users": ServiceRoles})
}

func (pgbouncer) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	for _, f := range pgbouncerFiles(cfg, rt) {
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
	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if len(verifiers) < len(ServiceRoles) {
		return engine.ApplyResult{}, fmt.Errorf("the service role verifiers are missing; run postgres-roles-v1 first")
	}
	for _, f := range pgbouncerFiles(cfg, rt) {
		if err := writeFileOwned(f.path, f.content, f.mode, f.owner, f.group); err != nil {
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

// roleVerifiers reads the SCRAM verifiers of the service roles from the
// first cluster. They are the same in every cluster (RFC 12).
func roleVerifiers(ctx context.Context, cfg config.Cluster) (map[string]string, error) {
	if len(cfg.Clusters) == 0 {
		return nil, fmt.Errorf("there is no cluster to read the service roles from")
	}
	rows, err := ClusterPG(cfg.Clusters[0]).Query(ctx, "postgres",
		"SELECT rolname, rolpassword FROM pg_authid WHERE rolname IN ('pgbouncer_auth','eru_monitor')", nil)
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
	f, err := os.CreateTemp("", "eru-pgpass-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := fmt.Fprintf(f, "*:*:*:eru_monitor:%s\n", strings.TrimSpace(string(password))); err != nil {
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
		"-h", socketDir, "-p", strconv.Itoa(port), "-U", "eru_monitor", "-d", "pgbouncer", "-tAc", "SHOW VERSION")
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
