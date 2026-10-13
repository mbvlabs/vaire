package steps

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/secrets"
)

const pgbouncerID = "pgbouncer-v1"

type pgbouncer struct{}

func (pgbouncer) ID() string { return pgbouncerID }

func (pgbouncer) Describe(config.Cluster) string {
	return "render pgbouncer.ini, pgbouncer_hba.conf and userlist.txt, then restart PgBouncer"
}

// SkipReason skips the step while there is no database: the service roles
// whose verifiers fill userlist.txt live in each database's cluster.
func (pgbouncer) SkipReason(cfg config.Cluster) string {
	if len(cfg.Clusters) == 0 {
		return "no_databases"
	}
	return ""
}

// pgbouncerIni is the rendered ini, which never contains a secret.
func pgbouncerIni(cfg config.Cluster, rt engine.Runtime) string {
	return string(render.PgbouncerIni(cfg, rt.Host, poolerConnections(cfg)))
}

// poolerConnections is max_db_connections while one wildcard entry serves
// every database: the smallest cluster's transaction pool.
func poolerConnections(cfg config.Cluster) int {
	out := 0
	for _, c := range cfg.Clusters {
		if r, err := cfg.Resources(c); err == nil && (out == 0 || r.PoolSize < out) {
			out = r.PoolSize
		}
	}
	return out
}

func (pgbouncer) RenderedFiles(cfg config.Cluster, rt engine.Runtime) []engine.RenderedFile {
	return []engine.RenderedFile{
		{Path: PgbouncerIniPath, Content: []byte(pgbouncerIni(cfg, rt))},
		{Path: PgbouncerHBAPath, Content: render.PgbouncerHBA()},
	}
}

func (pgbouncer) Inputs(cfg config.Cluster, rt engine.Runtime) ([]byte, error) {
	// The verifiers change on rotation without changing the desired state, so
	// they are not part of the inputs; Check re-renders them each time.
	return canonical(map[string]any{"ini": pgbouncerIni(cfg, rt), "hba": string(render.PgbouncerHBA()), "users": ServiceRoles})
}

func (pgbouncer) Check(ctx context.Context, cfg config.Cluster, rt engine.Runtime) (engine.CheckResult, error) {
	if !fileIsOwned(PgbouncerIniPath, pgbouncerIni(cfg, rt), 0o640, pgFileOwner, pgFileGroup) {
		return incomplete(PgbouncerIniPath + " differs"), nil
	}
	if !fileIsOwned(PgbouncerHBAPath, string(render.PgbouncerHBA()), 0o640, pgFileOwner, pgFileGroup) {
		return incomplete(PgbouncerHBAPath + " differs"), nil
	}
	verifiers, err := roleVerifiers(ctx, cfg)
	if err != nil || len(verifiers) < len(ServiceRoles) {
		return incomplete("cannot read the service role verifiers"), nil
	}
	if !fileIsOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup) {
		return incomplete(PgbouncerUserlistPath + " differs"), nil
	}
	if !serviceActive(ctx, PgbouncerService) {
		return incomplete("pgbouncer is not active"), nil
	}
	if err := pgbouncerResponds(ctx); err != nil {
		return incomplete("pgbouncer does not answer: " + err.Error()), nil
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
	if err := writeFileOwned(PgbouncerIniPath, pgbouncerIni(cfg, rt), 0o640, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFileOwned(PgbouncerHBAPath, string(render.PgbouncerHBA()), 0o640, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeFileOwned(PgbouncerUserlistPath, string(render.PgbouncerUserlist(verifiers)), 0o640, pgFileOwner, pgFileGroup); err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("pgbouncer"), map[string]string{"PGBOUNCER_DIR": PgbouncerDir}, report)
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

// pgbouncerResponds proves the PgBouncer admin console answers, using the
// monitor password the node keeps for the exporters.
func pgbouncerResponds(ctx context.Context) error {
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
		"-h", "/var/run/postgresql", "-p", "6432", "-U", "eru_monitor", "-d", "pgbouncer", "-tAc", "SHOW VERSION")
	return err
}
