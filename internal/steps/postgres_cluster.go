package steps

import (
	"context"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/render"
)

const postgresClusterID = "postgres-cluster-v1"

// postgresCluster creates one database's cluster, 18/<name>, on its socket
// port under <data_root>/<name>, and keeps its systemd drop-in with the
// cgroup limits (RFC 7.2, 8.2).
type postgresCluster struct{ clusterStep }

func (s postgresCluster) ID() string { return s.id(postgresClusterID) }

func (postgresCluster) Instance(cluster string) engine.Step {
	return postgresCluster{clusterStep{cluster: cluster}}
}

func (s postgresCluster) Describe(config.Cluster) string {
	return "create the PostgreSQL 18 cluster " + s.cluster + " with data checksums and its cgroup limits"
}

// clusterDropin renders spec's drop-in.
func clusterDropin(cfg config.Cluster, spec config.ClusterSpec) (string, config.Resources, error) {
	res, err := cfg.Resources(spec)
	if err != nil {
		return "", config.Resources{}, err
	}
	return string(render.PGServiceDropin(res)), res, nil
}

func (s postgresCluster) RenderedFiles(cfg config.Cluster, _ engine.Runtime) []engine.RenderedFile {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil
	}
	dropin, _, err := clusterDropin(cfg, spec)
	if err != nil {
		return nil
	}
	return []engine.RenderedFile{{Path: PGServiceDropIn(spec.Name), Content: []byte(dropin)}}
}

func (s postgresCluster) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return nil, err
	}
	dropin, _, err := clusterDropin(cfg, spec)
	if err != nil {
		return nil, err
	}
	return canonical(map[string]any{
		"data_directory":  cfg.Server.DataDirectory(spec.Name),
		"port":            spec.Port,
		"locale_provider": cfg.Postgres.LocaleProvider,
		"locale":          cfg.Postgres.Locale,
		"dropin":          dropin,
	})
}

// clusterRow is one pg_lsclusters line.
type clusterRow struct {
	Port    int
	Status  string
	DataDir string
}

// lsCluster returns the pg_lsclusters row of 18/<name>, if it exists.
func lsCluster(ctx context.Context, name string) (clusterRow, bool) {
	out, err := Probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return clusterRow{}, false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == strconv.Itoa(PGVersion) && fields[1] == name {
			port, _ := strconv.Atoi(fields[2])
			return clusterRow{Port: port, Status: fields[3], DataDir: fields[5]}, true
		}
	}
	return clusterRow{}, false
}

// ClusterNames lists the 18/<name> clusters pg_lsclusters knows, with
// their status (online, down). db list finds orphans in it.
func ClusterNames(ctx context.Context) map[string]string {
	out, err := Probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return nil
	}
	names := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 6 && fields[0] == strconv.Itoa(PGVersion) {
			names[fields[1]] = fields[3]
		}
	}
	return names
}

// ClusterExists reports whether 18/<name> exists, running or not.
func ClusterExists(ctx context.Context, name string) bool {
	_, ok := lsCluster(ctx, name)
	return ok
}

func (s postgresCluster) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	dropin, _, err := clusterDropin(cfg, spec)
	if err != nil {
		return engine.CheckResult{}, err
	}
	name := "cluster 18/" + spec.Name
	row, exists := lsCluster(ctx, spec.Name)
	switch {
	case !exists:
		return incomplete(name + " does not exist"), nil
	case row.DataDir != cfg.Server.DataDirectory(spec.Name):
		return incomplete(name + " uses " + row.DataDir + ", not " + cfg.Server.DataDirectory(spec.Name)), nil
	case row.Port != spec.Port:
		return incomplete(name + " uses port " + strconv.Itoa(row.Port) + ", not " + strconv.Itoa(spec.Port)), nil
	case row.Status != "online":
		return incomplete(name + " is " + row.Status), nil
	case !fileIs(PGServiceDropIn(spec.Name), dropin, 0o644):
		return incomplete(PGServiceDropIn(spec.Name) + " differs"), nil
	}
	return complete(), nil
}

func (s postgresCluster) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	dropin, res, err := clusterDropin(cfg, spec)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	dataDir := cfg.Server.DataDirectory(spec.Name)
	if row, exists := lsCluster(ctx, spec.Name); exists && (row.DataDir != dataDir || row.Port != spec.Port) {
		return engine.ApplyResult{}, output.NewError(
			output.CodePrecondition,
			"cluster 18/"+spec.Name+" already exists with data directory "+row.DataDir+" on port "+strconv.Itoa(row.Port),
			output.ExitPrecondition,
			"eru never re-creates an existing cluster. Drop it with pg_dropcluster, or move the data to match cluster.toml.",
		).WithDetails(map[string]any{
			"reason": "cluster_mismatch", "cluster": spec.Name,
			"existing": map[string]any{"data_directory": row.DataDir, "port": row.Port},
			"wanted":   map[string]any{"data_directory": dataDir, "port": spec.Port},
		})
	}
	if err := writeFile(PGServiceDropIn(spec.Name), dropin, 0o644); err != nil {
		return engine.ApplyResult{}, err
	}
	memoryMax := "infinity"
	if res.MemoryMaxMiB > 0 {
		memoryMax = strconv.Itoa(res.MemoryMaxMiB) + "M"
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("postgres-cluster"), map[string]string{
		"CLUSTER":         spec.Name,
		"PORT":            strconv.Itoa(spec.Port),
		"DATA_DIR":        dataDir,
		"LOCALE_PROVIDER": cfg.Postgres.LocaleProvider,
		"LOCALE":          cfg.Postgres.Locale,
		"MEMORY_HIGH":     strconv.Itoa(render.MemoryHighMiB(res)) + "M",
		"MEMORY_MAX":      memoryMax,
		"CPU_WEIGHT":      strconv.Itoa(res.CPUWeight),
		"CPU_QUOTA":       res.CPUQuota,
		"IO_WEIGHT":       strconv.Itoa(res.IOWeight),
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
