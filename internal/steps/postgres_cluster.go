package steps

import (
	"context"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
)

const postgresClusterID = "postgres-cluster-v1"

type postgresCluster struct{}

func (postgresCluster) ID() string { return postgresClusterID }

func (postgresCluster) Describe(config.Cluster) string {
	return "create the PostgreSQL 18 main cluster with data checksums"
}

func (postgresCluster) RenderedFiles(config.Cluster, engine.Runtime) []engine.RenderedFile {
	return []engine.RenderedFile{{Path: PGServiceDropIn, Content: render.PGServiceDropin()}}
}

func (postgresCluster) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{
		"data_directory":  cfg.Server.DataDirectory(),
		"locale_provider": cfg.Postgres.LocaleProvider,
		"locale":          cfg.Postgres.Locale,
		"dropin":          string(render.PGServiceDropin()),
	})
}

// clusterRow returns the 18/main status and data directory, if the cluster
// exists.
func clusterRow(ctx context.Context) (status, datadir string, exists bool) {
	out, err := Probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return "", "", false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == "18" && fields[1] == PGCluster {
			return fields[3], fields[5], true
		}
	}
	return "", "", false
}

func (postgresCluster) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	status, datadir, exists := clusterRow(ctx)
	if !exists {
		return incomplete("cluster 18/main does not exist"), nil
	}
	if datadir != cfg.Server.DataDirectory() {
		return incomplete("cluster 18/main uses " + datadir + ", not " + cfg.Server.DataDirectory()), nil
	}
	if status != "online" {
		return incomplete("cluster 18/main is " + status), nil
	}
	return complete(), nil
}

func (s postgresCluster) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	_, datadir, exists := clusterRow(ctx)
	if exists && datadir != cfg.Server.DataDirectory() {
		return engine.ApplyResult{}, output.NewError(
			output.CodePrecondition,
			"cluster 18/main already exists with data directory "+datadir,
			output.ExitPrecondition,
			"eru never re-creates an existing cluster. Move the data or pick the matching data_root.",
		).WithDetails(map[string]any{"reason": "cluster_datadir_mismatch", "existing": datadir, "wanted": cfg.Server.DataDirectory()})
	}
	if err := writeFile(PGServiceDropIn, string(render.PGServiceDropin()), 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	err := rt.Shell.Run(ctx, s.ID(), Script("postgres-cluster"), map[string]string{
		"DATA_DIR":        cfg.Server.DataDirectory(),
		"LOCALE_PROVIDER": cfg.Postgres.LocaleProvider,
		"LOCALE":          cfg.Postgres.Locale,
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
