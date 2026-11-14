package steps

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
)

const postgresClusterID = "postgres-cluster-v1"

// postgresCluster creates one database's cluster, 18/<name>, on its socket
// port under <data_root>/<name>, and keeps its systemd drop-in with the
// cgroup limits (RFC 7.2, 8.2).
type postgresCluster struct{ clusterStep }

func (s postgresCluster) ID() string { return s.id(postgresClusterID) }

func (postgresCluster) Recheck() bool { return true }

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

// ClusterNamesStrict returns PostgreSQL 18 cluster names and their primary
// online/down state. An empty map is a successful inventory with no clusters;
// errors never return a partial inventory.
func ClusterNamesStrict(ctx context.Context) (map[string]string, error) {
	return clusterNamesStrict(ctx, Probe)
}

func clusterNamesStrict(ctx context.Context, probe func(context.Context, string, ...string) (string, error)) (map[string]string, error) {
	out, err := probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return nil, err
	}
	return parseClusterNamesStrict(out)
}

func parseClusterNamesStrict(out string) (map[string]string, error) {
	names := make(map[string]string)
	seen := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 7 {
			return nil, errors.New("pg_lsclusters returned an incomplete line")
		}
		// Older PostgreSQL major versions can have a dotted version number.
		version := strings.Split(fields[0], ".")
		if len(version) > 2 {
			return nil, errors.New("pg_lsclusters returned an invalid version")
		}
		for _, part := range version {
			if part == "" || strings.Trim(part, "0123456789") != "" {
				return nil, errors.New("pg_lsclusters returned an invalid version")
			}
		}
		port, err := strconv.Atoi(fields[2])
		if err != nil || port < 1 || port > 65535 || fields[4] == "<unknown>" || fields[5] == "<unknown>" || fields[6] == "<unknown>" {
			return nil, errors.New("pg_lsclusters returned incomplete cluster information")
		}
		status, err := clusterPrimaryStatus(fields[3])
		if err != nil {
			return nil, err
		}
		key := fields[0] + "/" + fields[1]
		if seen[key] {
			return nil, errors.New("pg_lsclusters returned a duplicate cluster")
		}
		seen[key] = true
		if fields[0] == strconv.Itoa(PGVersion) {
			names[fields[1]] = status
		}
	}
	return names, nil
}

// clusterPrimaryStatus accepts the modifiers emitted by pg_lsclusters without
// --start-conf: recovery, the recognized supervisors, and missing binaries.
func clusterPrimaryStatus(status string) (string, error) {
	parts := strings.Split(status, ",")
	if parts[0] != "online" && parts[0] != "down" {
		return "", errors.New("pg_lsclusters returned an unrecognized status")
	}
	seen := make(map[string]bool)
	for _, modifier := range parts[1:] {
		switch modifier {
		case "recovery", "pacemaker", "patroni", "binaries_missing":
		default:
			return "", errors.New("pg_lsclusters returned an unrecognized status modifier")
		}
		if seen[modifier] || (modifier == "pacemaker" && seen["patroni"]) || (modifier == "patroni" && seen["pacemaker"]) {
			return "", errors.New("pg_lsclusters returned duplicate or conflicting status modifiers")
		}
		seen[modifier] = true
	}
	return parts[0], nil
}

// ClusterExists reports whether 18/<name> exists, running or not.
func ClusterExists(ctx context.Context, name string) bool {
	_, ok := lsCluster(ctx, name)
	return ok
}

// AllClusterNames lists every PostgreSQL cluster pg_lsclusters knows, across
// all major versions, mapped to its major-version number. Unlike ClusterNames,
// a missing or unreadable inventory is an error: a copy target name must fail
// closed when the full cluster inventory cannot be read.
func AllClusterNames(ctx context.Context) (map[string]string, error) {
	out, err := Probe(ctx, "pg_lsclusters", "-h")
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return nil, errors.New("pg_lsclusters returned an incomplete line")
		}
		if _, err := strconv.Atoi(fields[0]); err != nil {
			return nil, errors.New("pg_lsclusters returned an invalid major version")
		}
		names[fields[1]] = fields[0]
	}
	return names, nil
}

func (s postgresCluster) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if err := config.CheckDatabaseName(spec.Name); err != nil {
		return engine.CheckResult{}, err
	}
	dropin, res, err := clusterDropin(cfg, spec)
	if err != nil {
		return engine.CheckResult{}, err
	}
	name := "cluster 18/" + spec.Name
	row, exists := lsCluster(ctx, spec.Name)
	primary, statusErr := clusterPrimaryStatus(row.Status)
	switch {
	case !exists:
		return incomplete(name + " does not exist"), nil
	case row.DataDir != cfg.Server.DataDirectory(spec.Name):
		return incomplete(name + " uses " + row.DataDir + ", not " + cfg.Server.DataDirectory(spec.Name)), nil
	case row.Port != spec.Port:
		return incomplete(name + " uses port " + strconv.Itoa(row.Port) + ", not " + strconv.Itoa(spec.Port)), nil
	case statusErr != nil || primary != "online":
		return incomplete(name + " is " + row.Status), nil
	case !fileIs(PGServiceDropIn(spec.Name), dropin, 0o644):
		return incomplete(PGServiceDropIn(spec.Name) + " differs"), nil
	}
	overlay, err := cpuOverlayExists(PGServiceDropInDir(spec.Name))
	if err != nil {
		return engine.CheckResult{}, err
	}
	if overlay {
		return checkCPUWithOverlay(name, res, true, hostinfo.CPUObservation{}), nil
	}
	return checkCPUWithOverlay(name, res, false, hostinfo.ObserveCPUService(ctx, PGServiceName(spec.Name))), nil
}

func checkCPUWithOverlay(name string, res config.Resources, overlay bool, facts hostinfo.CPUObservation) engine.CheckResult {
	if overlay {
		return incomplete(name + " has a CPU-only overlay")
	}
	return checkCPUObserved(name, res, facts)
}

func checkCPUObserved(name string, res config.Resources, facts hostinfo.CPUObservation) engine.CheckResult {
	err := hostinfo.ValidateCPUPolicy(cpuPolicy(res), facts)
	if err == nil {
		return complete()
	}
	var mismatch *hostinfo.CPUPolicyMismatch
	if errors.As(err, &mismatch) && mismatch.Reason == "effective_policy_unknown" {
		return engine.CheckResult{Inconclusive: true, Detail: name + " CPU policy is unknown"}
	}
	return incomplete(name + " CPU policy differs")
}

// CheckSpareCPU is the read-only pre-save screen for node configuration
// writers. Call on the target node before saving a spare request. Unknown is
// distinct from unsupported and is also refused.
func CheckSpareCPU(ctx context.Context, cluster, mode string) error {
	return checkSpareCPU(ctx, cluster, mode, hostinfo.CPUServiceScreen)
}

func checkSpareCPU(ctx context.Context, cluster, mode string, screen func(context.Context, string) hostinfo.CPUScreen) error {
	if mode != "" && mode != "normal" && mode != "spare" {
		return output.NewError(output.CodeValidation, "invalid CPU mode", output.ExitConfig, "Use normal or spare.")
	}
	if mode != "spare" {
		return nil
	}
	fact := screen(ctx, PGServiceName(cluster))
	if fact.Capability == hostinfo.CPUCapable {
		return nil
	}
	reason := "spare_cpu_capability_unknown"
	message := "Cannot verify native spare CPU scheduling for " + cluster
	if fact.Capability == hostinfo.CPUUnsupported {
		reason = "spare_cpu_unsupported"
		message = "Native spare CPU scheduling is unsupported for " + cluster
	}
	return output.NewError(output.CodePrecondition, message, output.ExitPrecondition,
		"Inspect the service cgroup and native CPU controller support, then retry.").
		WithDetails(map[string]any{"reason": reason, "cluster": cluster, "capability": fact.Capability, "capability_reason": fact.Reason})
}

func cpuPolicy(res config.Resources) hostinfo.CPUPolicy {
	want := hostinfo.CPUPolicy{Idle: res.CPUMode == "spare"}
	if res.CPUWeight > 0 {
		want.Weight = uint64(res.CPUWeight)
	}
	if res.CPUQuota != "" {
		percent, _ := strconv.ParseFloat(strings.TrimSuffix(res.CPUQuota, "%"), 64) // ResolveResources validates the quota.
		want.QuotaPercent = &percent
	}
	return want
}

func applyCPUChecked(ctx context.Context, cluster string, res config.Resources,
	screen func(context.Context, string) hostinfo.CPUScreen,
	run func() error, observe func(context.Context, string) hostinfo.CPUObservation,
) error {
	if err := checkSpareCPU(ctx, cluster, res.CPUMode, screen); err != nil {
		return err
	}
	if err := run(); err != nil {
		var scriptErr *engine.ScriptError
		if errors.As(err, &scriptErr) {
			for _, line := range scriptErr.Tail {
				reason := ""
				switch line {
				case "vaire_cpu_policy_set_failed":
					reason = "runtime_property_update_failed"
				case "vaire_cpu_start_failed":
					reason = "startup_cpu_attribute_failed"
				}
				if reason != "" {
					cpuErr := cpuPolicyError(cluster, res, observe(ctx, PGServiceName(cluster)), reason)
					cpuErr.Cause = err
					return cpuErr
				}
			}
		}
		return err
	}
	facts := observe(ctx, PGServiceName(cluster))
	if err := hostinfo.ValidateCPUPolicy(cpuPolicy(res), facts); err != nil {
		var mismatch *hostinfo.CPUPolicyMismatch
		_ = errors.As(err, &mismatch)
		reason := "effective_policy_unknown"
		if mismatch != nil {
			reason = mismatch.Reason
		}
		return cpuPolicyError(cluster, res, facts, reason)
	}
	return nil
}

func cpuPolicyError(cluster string, res config.Resources, facts hostinfo.CPUObservation, mismatch string) *output.CLIError {
	details := map[string]any{
		"reason": "cpu_policy_not_enforced", "cluster": cluster,
		"desired":       map[string]any{"mode": res.CPUMode, "weight": res.CPUWeight, "quota": res.CPUQuota},
		"desired_saved": nil, "effective_known": facts.Screen.Capability == hostinfo.CPUCapable && facts.Idle != nil && facts.Quota != nil && (res.CPUMode == "spare" || facts.Weight != nil),
		"effective":          map[string]any{"idle": facts.Idle, "weight": facts.Weight, "quota": facts.Quota},
		"observation_reason": facts.Reason, "capability_reason": facts.Screen.Reason, "mismatch": mismatch,
	}
	return output.NewError(output.CodeStepFailed, "CPU policy was not verified for "+cluster, output.ExitStep,
		"Inspect the native service cgroup and re-run apply; unrelated settings are not rolled back.").WithDetails(details)
}

func clusterScriptEnv(cfg config.Cluster, spec config.ClusterSpec, res config.Resources) map[string]string {
	memoryMax := "infinity"
	if res.MemoryMaxMiB > 0 {
		memoryMax = strconv.Itoa(res.MemoryMaxMiB) + "M"
	}
	return map[string]string{
		"CLUSTER":         spec.Name,
		"PORT":            strconv.Itoa(spec.Port),
		"DATA_DIR":        cfg.Server.DataDirectory(spec.Name),
		"LOCALE_PROVIDER": cfg.Postgres.LocaleProvider,
		"LOCALE":          cfg.Postgres.Locale,
		"MEMORY_HIGH":     strconv.Itoa(render.MemoryHighMiB(res)) + "M",
		"MEMORY_MAX":      memoryMax,
		"CPU_WEIGHT":      render.PGCPUWeight(res),
		"CPU_QUOTA":       res.CPUQuota,
		"IO_WEIGHT":       strconv.Itoa(res.IOWeight),
	}
}

func (s postgresCluster) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	spec, err := s.spec(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if err := config.CheckDatabaseName(spec.Name); err != nil {
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
			"vaire never re-creates an existing cluster. Drop it with pg_dropcluster, or move the data to match cluster.toml.",
		).WithDetails(map[string]any{
			"reason": "cluster_mismatch", "cluster": spec.Name,
			"existing": map[string]any{"data_directory": row.DataDir, "port": row.Port},
			"wanted":   map[string]any{"data_directory": dataDir, "port": spec.Port},
		})
	}
	err = applyCPUChecked(ctx, spec.Name, res, hostinfo.CPUServiceScreen, func() error {
		if err := prepareFullCPUDropin(PGServiceDropInDir(spec.Name), func() error {
			return writeFile(PGServiceDropIn(spec.Name), dropin, 0o644)
		}); err != nil {
			return err
		}
		return rt.Shell.Run(ctx, s.ID(), Script("postgres-cluster"), clusterScriptEnv(cfg, spec, res), report)
	}, hostinfo.ObserveCPUService)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
