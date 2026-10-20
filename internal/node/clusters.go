package node

import (
	"errors"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/steps"
)

// clusterOf is database db's [[clusters]] entry. Each database is its own
// cluster (RFC 4), so the name finds both.
func clusterOf(cfg config.Cluster, db string) (config.ClusterSpec, error) {
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return config.ClusterSpec{}, databaseNotFound(db)
	}
	return spec, nil
}

// clusterPG talks to database db's cluster on its socket.
func clusterPG(cfg config.Cluster, db string) (pg.Local, error) {
	spec, err := clusterOf(cfg, db)
	if err != nil {
		return pg.Local{}, err
	}
	return steps.ClusterPG(spec), nil
}

// onlyCluster picks db, or the only cluster when db is empty.
func onlyCluster(cfg config.Cluster, db string) (config.ClusterSpec, error) {
	if db != "" {
		return clusterOf(cfg, db)
	}
	if len(cfg.Clusters) == 1 {
		return cfg.Clusters[0], nil
	}
	return config.ClusterSpec{}, output.NewError(output.CodeInputRequired, "this server has several databases; name one with --db", output.ExitInput,
		"Pick a database from details.databases.").WithDetails(map[string]any{"flag": "--db", "databases": clusterNames(cfg)})
}

func clusterNames(cfg config.Cluster) []string {
	names := make([]string, 0, len(cfg.Clusters))
	for _, c := range cfg.Clusters {
		names = append(names, c.Name)
	}
	return names
}

// withHint appends hint to err's hint, keeping its code and details.
func withHint(err error, hint string) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return output.WrapError(output.CodeError, err, output.ExitGeneric, hint)
	}
	out := *cliErr
	if out.Hint != "" {
		hint = out.Hint + " " + hint
	}
	out.Hint = hint
	return &out
}
