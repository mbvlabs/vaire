package steps

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/exporters"
	"github.com/mbvlabs/eru/internal/promfile"
	"github.com/mbvlabs/eru/internal/render"
	"github.com/mbvlabs/eru/internal/versions"
)

const metricsID = "metrics-v1"

// Files and units metrics-v1 owns (Phase 4).
const (
	MetricsConfPath     = "/etc/nginx/eru-metrics.conf"
	MetricsHtpasswdPath = "/etc/nginx/eru-metrics.htpasswd" //nolint:gosec // G101: a path, not a credential.
	MetricsTargetsPath  = "/etc/nginx/eru-metrics-targets.json"
	MetricsJailPath     = "/etc/fail2ban/jail.d/eru-metrics.conf"
	// PostgresExporterConfPath is postgres_exporter's --config.file. eru
	// logs in by peer auth, so it has no auth modules; the file exists so
	// the exporter doesn't warn "Error loading config" at every start.
	PostgresExporterConfPath = "/etc/postgres_exporter/postgres_exporter.yml"
	ExportersSlice           = "eru-exporters.slice"

	NodeExporterService     = "node_exporter.service"
	PostgresExporterService = "postgres_exporter.service"
	PgbouncerExporterTx     = "pgbouncer_exporter.service"
	PgbouncerExporterSess   = "pgbouncer_exporter-session.service"

	NodeExporterAddr     = "127.0.0.1:9100"
	PostgresExporterAddr = "127.0.0.1:9187"
	PgbouncerTxAddr      = "127.0.0.1:9127"
	PgbouncerSessAddr    = "127.0.0.1:9128"
)

// exporterUsers are the system users the exporters run as. postgres_exporter
// is in pg_ident's eru map, so it logs in as eru_monitor with peer auth.
var exporterUsers = []string{"node_exporter", "postgres_exporter", "pgbouncer_exporter"}

// metricsJail turns on fail2ban's stock nginx-http-auth jail for bad
// /metrics passwords; nginx logs the client's real address thanks to the
// PROXY protocol on its internal hops.
const metricsJail = `# managed by eru
[nginx-http-auth]
enabled = true
port = 443
logpath = /var/log/nginx/error.log
backend = auto
maxretry = 10
findtime = 10m
bantime = 1h
`

// postgresExporterConf has no auth modules: every probe logs in as
// eru_monitor by peer auth. It can't be empty: the exporter's YAML decoder
// fails on an empty document.
const postgresExporterConf = `# managed by eru
auth_modules: {}
`

type metricsStep struct{}

func (metricsStep) ID() string { return metricsID }

func (metricsStep) Describe(cfg config.Cluster) string {
	if !cfg.Metrics.Enabled {
		return "stop the exporters and remove the /metrics endpoint ([metrics] enabled = false)"
	}
	return "install node_exporter, postgres_exporter and the pgbouncer_exporters on loopback, and the authenticated /metrics endpoint on nginx"
}

// metricsFile is one file the step writes.
type metricsFile struct {
	path, content, owner, group string
	mode                        os.FileMode
}

// exporterUnits are the units metrics-v1 runs for cfg: node_exporter and
// postgres_exporter always, the pgbouncer_exporters while there are
// databases (both poolers stop with the last one).
func exporterUnits(cfg config.Cluster) map[string][]byte {
	units := map[string][]byte{
		NodeExporterService: render.ExporterService(render.ExporterUnit{
			Description: "Prometheus node_exporter (eru)",
			User:        "node_exporter",
			Slice:       ExportersSlice,
			Binary:      exporters.BinaryPath(versions.NodeExporter),
			Args: []string{
				"--web.listen-address=" + NodeExporterAddr,
				"--collector.textfile.directory=" + promfile.Dir,
				"--collector.systemd",
				"--collector.systemd.unit-include=(postgresql@18-.+|pgbouncer|eru-.+|nginx|wg-quick@wg0)[.](service|timer)",
			},
			MemoryMax: "64M",
		}),
		PostgresExporterService: render.ExporterService(render.ExporterUnit{
			Description: "Prometheus postgres_exporter, one probe per cluster (eru)",
			User:        "postgres_exporter",
			Slice:       ExportersSlice,
			Binary:      exporters.BinaryPath(versions.PostgresExporter),
			Args: []string{
				"--web.listen-address=" + PostgresExporterAddr,
				"--config.file=" + PostgresExporterConfPath,
				"--collector.stat_statements",
				"--collector.stat_statements.limit=50",
				"--collector.stat_checkpointer",
				"--collector.database_wraparound",
				"--collector.long_running_transactions",
			},
			MemoryMax: "128M",
		}),
	}
	if len(cfg.Clusters) > 0 {
		units[PgbouncerExporterTx] = pgbouncerExporterUnit("transaction pooler", PgbouncerTxAddr, PgbouncerExporterEnv, PgbouncerService+".service")
		units[PgbouncerExporterSess] = pgbouncerExporterUnit("session pooler", PgbouncerSessAddr, PgbouncerSessionExporterEnv, SessionPoolerService+".service")
	}
	return units
}

// The pgbouncer_exporters' connection strings carry eru_monitor's password,
// so they live in /etc/eru/runtime (systemd reads EnvironmentFile as PID 1).
const (
	PgbouncerExporterEnv        = RuntimeDir + "/pgbouncer_exporter.env"
	PgbouncerSessionExporterEnv = RuntimeDir + "/pgbouncer_exporter-session.env"
)

func pgbouncerExporterUnit(pooler, addr, env, after string) []byte {
	return render.ExporterService(render.ExporterUnit{
		Description:     "Prometheus pgbouncer_exporter for the " + pooler + " (eru)",
		User:            "pgbouncer_exporter",
		Slice:           ExportersSlice,
		Binary:          exporters.BinaryPath(versions.PgbouncerExporter),
		Args:            []string{"--web.listen-address=" + addr},
		EnvironmentFile: env,
		MemoryMax:       "32M",
		After:           []string{after},
	})
}

// pgbouncerConnection is a pgbouncer_exporter's connection string: the
// pooler's admin console over its socket as eru_monitor (a stats_user).
func pgbouncerConnection(socketDir string, port int, password string) string {
	return fmt.Sprintf("PGBOUNCER_EXPORTER_CONNECTION_STRING=postgres://eru_monitor:%s@/pgbouncer?host=%s&port=%d&sslmode=disable\n", password, socketDir, port)
}

// allExporterUnits lists every unit name the step may own, for teardown.
var allExporterUnits = []string{NodeExporterService, PostgresExporterService, PgbouncerExporterTx, PgbouncerExporterSess}

// MetricsLocations are the /metrics endpoint's paths for cfg (Phase 4 D5):
// node, a probe per database, both poolers while there are databases, and
// the http_sd target list.
func MetricsLocations(cfg config.Cluster) []render.MetricsLocation {
	prefix := cfg.Metrics.PathPrefix
	if prefix == "" {
		prefix = "/metrics"
	}
	server, node := cfg.Name(), ""
	if n, ok := cfg.Master(); ok {
		node = n.Name
	}
	base := func(component string, extra ...string) map[string]string {
		labels := map[string]string{"eru_server": server, "eru_node": node, "eru_component": component}
		for i := 0; i+1 < len(extra); i += 2 {
			labels[extra[i]] = extra[i+1]
		}
		return labels
	}
	out := []render.MetricsLocation{
		{Path: prefix + "/targets.json", Alias: MetricsTargetsPath},
		{Path: prefix + "/node", Upstream: "http://" + NodeExporterAddr + "/metrics", Labels: base("node")},
	}
	for _, c := range cfg.Clusters {
		out = append(out, render.MetricsLocation{
			Path:     prefix + "/postgres/" + c.Name,
			Upstream: "http://" + PostgresExporterAddr + "/probe",
			Args:     render.ProbeArgs(render.ProbeTarget(PGSocketDir, c)),
			Labels:   base("postgres", "db", c.Name),
		})
	}
	if len(cfg.Clusters) > 0 {
		out = append(out,
			render.MetricsLocation{Path: prefix + "/pgbouncer", Upstream: "http://" + PgbouncerTxAddr + "/metrics", Labels: base("pgbouncer", "pooler", "transaction")},
			render.MetricsLocation{Path: prefix + "/pgbouncer-session", Upstream: "http://" + PgbouncerSessAddr + "/metrics", Labels: base("pgbouncer", "pooler", "session")},
		)
	}
	return out
}

// metricsFiles are the files the step writes, without secrets: the units,
// the slice, the nginx include, the target list and the fail2ban jail.
func metricsFiles(cfg config.Cluster) []metricsFile {
	locations := MetricsLocations(cfg)
	files := []metricsFile{
		{path: SystemdSystemDir + "/" + ExportersSlice, content: string(render.ExportersSlice()), mode: 0o644},
		{path: MetricsConfPath, content: string(render.MetricsConf(locations, MetricsHtpasswdPath, cfg.Metrics.Allowlist)), mode: 0o644},
		{path: MetricsTargetsPath, content: string(render.TargetsJSON(NodeHostname(cfg), locations)), owner: "root", group: "www-data", mode: 0o640},
		{path: MetricsJailPath, content: metricsJail, mode: 0o644},
		{path: PostgresExporterConfPath, content: postgresExporterConf, mode: 0o644},
	}
	units := exporterUnits(cfg)
	for _, name := range allExporterUnits {
		if content, ok := units[name]; ok {
			files = append(files, metricsFile{path: SystemdSystemDir + "/" + name, content: string(content), mode: 0o644})
		}
	}
	return files
}

func (metricsStep) RenderedFiles(cfg config.Cluster, _ engine.Runtime) []engine.RenderedFile {
	if !cfg.Metrics.Enabled {
		return nil
	}
	var out []engine.RenderedFile
	for _, f := range metricsFiles(cfg) {
		out = append(out, engine.RenderedFile{Path: f.path, Content: []byte(f.content)})
	}
	return out
}

func (metricsStep) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	files := map[string]string{}
	if cfg.Metrics.Enabled {
		for _, f := range metricsFiles(cfg) {
			files[f.path] = f.content
		}
	}
	pins := map[string]string{}
	for _, name := range versions.Exporters {
		pins[name] = versions.ExporterVersion(name)
	}
	return canonical(map[string]any{"enabled": cfg.Metrics.Enabled, "files": files, "pins": pins, "arch": runtime.GOARCH})
}

func (metricsStep) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	if !cfg.Metrics.Enabled {
		if _, err := os.Stat(MetricsConfPath); err == nil {
			return incomplete(MetricsConfPath + " is still there"), nil
		}
		for _, unit := range allExporterUnits {
			if serviceActive(ctx, unit) {
				return incomplete(unit + " is still running"), nil
			}
		}
		return complete(), nil
	}
	for _, name := range versions.Exporters {
		a, err := exporters.ArtifactFor(name, runtime.GOARCH)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if !exporters.Installed(a) {
			return incomplete(name + " " + a.Version + " is not installed"), nil
		}
	}
	for _, f := range metricsFiles(cfg) {
		if !fileIsOwned(f.path, f.content, f.mode, f.owner, f.group) {
			return incomplete(f.path + " differs"), nil
		}
	}
	if _, err := os.Stat(MetricsHtpasswdPath); err != nil {
		return incomplete(MetricsHtpasswdPath + " is missing"), nil
	}
	units := exporterUnits(cfg)
	for _, unit := range allExporterUnits {
		_, want := units[unit]
		if want && !serviceActive(ctx, unit) {
			return incomplete(unit + " is not active"), nil
		}
		if !want && serviceActive(ctx, unit) {
			return incomplete(unit + " runs without a database"), nil
		}
	}
	if err := httpOK(ctx, "http://"+NodeExporterAddr+"/metrics"); err != nil {
		return incomplete("node_exporter does not answer: " + err.Error()), nil
	}
	return complete(), nil
}

func (s metricsStep) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if !cfg.Metrics.Enabled {
		for _, path := range []string{MetricsConfPath, MetricsTargetsPath, PostgresExporterConfPath} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return engine.ApplyResult{}, err
			}
		}
		err := rt.Shell.Run(ctx, s.ID(), Script("metrics"), map[string]string{"PHASE": "disable", "ALL_UNITS": strings.Join(allExporterUnits, " ")}, report)
		return engine.ApplyResult{Restart: engine.RestartNone}, err
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("metrics"), map[string]string{
		"PHASE":          "users",
		"EXPORTER_USERS": strings.Join(exporterUsers, " "),
		"TEXTFILE_DIR":   promfile.Dir,
	}, report); err != nil {
		return engine.ApplyResult{}, err
	}
	for _, name := range versions.Exporters {
		a, err := exporters.ArtifactFor(name, runtime.GOARCH)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if !exporters.Installed(a) {
			if report != nil {
				report(output.Event{Type: "log", Step: s.ID(), Line: "downloading " + a.Tarball})
			}
		}
		if err := exporters.Fetch(ctx, a); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if err := writeHtpasswd(rt, false); err != nil {
		return engine.ApplyResult{}, err
	}
	previous, hadPrevious := readMaybe(MetricsConfPath)
	for _, f := range metricsFiles(cfg) {
		if err := writeFileOwned(f.path, f.content, f.mode, f.owner, f.group); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if len(cfg.Clusters) > 0 {
		if err := writePgbouncerExporterEnv(); err != nil {
			return engine.ApplyResult{}, err
		}
	}
	if _, err := Probe(ctx, "nginx", "-t"); err != nil {
		restoreFile(MetricsConfPath, previous, hadPrevious)
		return engine.ApplyResult{}, fmt.Errorf("nginx refused the metrics configuration; the previous one is back: %w", err)
	}
	units := exporterUnits(cfg)
	var start, stop []string
	for _, unit := range allExporterUnits {
		if _, ok := units[unit]; ok {
			start = append(start, unit)
		} else if _, err := os.Stat(SystemdSystemDir + "/" + unit); err == nil {
			stop = append(stop, unit)
		}
	}
	if err := rt.Shell.Run(ctx, s.ID(), Script("metrics"), map[string]string{
		"PHASE":       "start",
		"START_UNITS": strings.Join(start, " "),
		"STOP_UNITS":  strings.Join(stop, " "),
	}, report); err != nil {
		return engine.ApplyResult{}, err
	}
	for _, unit := range stop {
		_ = os.Remove(SystemdSystemDir + "/" + unit)
	}
	for _, name := range versions.Exporters {
		if a, err := exporters.ArtifactFor(name, runtime.GOARCH); err == nil {
			_ = exporters.RemoveOld(name, a.Dir())
		}
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, nil
}

// writePgbouncerExporterEnv writes both pgbouncer_exporters' environment
// files from the monitor password postgres-roles-v1 keeps.
func writePgbouncerExporterEnv() error {
	raw, err := os.ReadFile(RuntimeDir + "/monitor.pass")
	if err != nil {
		return fmt.Errorf("the eru_monitor password is missing (%w); run eru apply so postgres-roles-v1 writes it", err)
	}
	password := strings.TrimSpace(string(raw))
	if err := writeFile(PgbouncerExporterEnv, pgbouncerConnection(PGSocketDir, PgbouncerPort, password), 0o600); err != nil {
		return err
	}
	return writeFile(PgbouncerSessionExporterEnv, pgbouncerConnection(SessionSocketDir, SessionPoolerPort, password), 0o600)
}

// writeHtpasswd writes the /metrics credential's hash from the request's
// secrets (Phase 4 D5): only when the file is missing, unless rotate, so an
// operator with stale credentials never replaces a newer one. nginx reads
// the file on every request, so no reload is needed.
func writeHtpasswd(rt engine.Runtime, rotate bool) error {
	if _, err := os.Stat(MetricsHtpasswdPath); err == nil && !rotate {
		return nil
	}
	hash := rt.SecretValues[SecretMetricsHash]
	if hash == "" {
		return output.NewError(output.CodeInputRequired, "the /metrics credential is missing on the node and the request carries none", output.ExitInput,
			"Run eru apply from a machine whose context has the metrics credential (eru login with a current transfer bundle).").
			WithDetails(map[string]any{"secret": SecretMetricsHash})
	}
	if !strings.HasPrefix(hash, "$2") {
		return errors.New("the metrics credential hash is not bcrypt")
	}
	return writeFileOwned(MetricsHtpasswdPath, HtpasswdLine(hash), 0o640, "root", "www-data")
}

// WriteHtpasswd replaces the /metrics credential's hash (eru metrics rotate).
func WriteHtpasswd(rt engine.Runtime) error { return writeHtpasswd(rt, true) }

// HtpasswdLine is the htpasswd file for the metrics user's hash.
func HtpasswdLine(hash string) string { return "metrics:" + hash + "\n" }

func readMaybe(path string) (string, bool) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: managed file paths from the step catalogue.
	if err != nil {
		return "", false
	}
	return string(raw), true
}

func restoreFile(path, content string, existed bool) {
	if !existed {
		_ = os.Remove(path)
		return
	}
	_ = writeFile(path, content, 0o644)
}

// httpOK GETs url on loopback and wants 200.
func httpOK(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// HTTPStatus GETs url on loopback and returns the status code; doctor uses it
// for the exporters and the endpoint.
func HTTPStatus(ctx context.Context, url string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// MetricsStepIDs is the step db create, db fork and db drop run after the
// poolers, so a database's /metrics path and targets.json entry follow.
func MetricsStepIDs() []string { return []string{metricsID} }
