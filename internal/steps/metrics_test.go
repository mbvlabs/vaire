package steps

import (
	"encoding/json"
	"net/url"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/render"
)

func metricsCluster(dbs ...string) config.Cluster {
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	for i, db := range dbs {
		cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: db, Node: "db1", Port: 5433 + i, Stanza: db + "-abc123"})
	}
	return cfg
}

func TestMetricsLocations(t *testing.T) {
	none := MetricsLocations(metricsCluster())
	if len(none) != 2 {
		t.Fatalf("no databases: %d locations, want targets.json and node", len(none))
	}
	locs := MetricsLocations(metricsCluster("app", "other", "third"))
	var paths []string
	for _, l := range locs {
		paths = append(paths, l.Path)
	}
	want := "/metrics/targets.json /metrics/node /metrics/postgres/app /metrics/postgres/other /metrics/postgres/third /metrics/pgbouncer /metrics/pgbouncer-session"
	if strings.Join(paths, " ") != want {
		t.Errorf("paths %v", paths)
	}
	app := locs[2]
	q, err := url.ParseQuery(app.Args)
	if err != nil {
		t.Fatal(err)
	}
	if q.Get("target") != "postgresql://vaire_monitor@/app?connect_timeout=5&host=%2Fvar%2Frun%2Fpostgresql&port=5433&sslmode=disable" {
		t.Errorf("target %q", q.Get("target"))
	}
	if app.Upstream != "http://127.0.0.1:9187/probe" || app.Labels["db"] != "app" || app.Labels["vaire_component"] != "postgres" {
		t.Errorf("app location %+v", app)
	}
}

func TestMetricsConfRender(t *testing.T) {
	cfg := metricsCluster("app")
	conf := string(render.MetricsConf(MetricsLocations(cfg), MetricsHtpasswdPath, nil))
	if strings.Count(conf, "auth_basic_user_file "+MetricsHtpasswdPath) != 5 {
		t.Errorf("every location needs the credential:\n%s", conf)
	}
	if strings.Contains(conf, "deny all") {
		t.Error("no allowlist, but deny all")
	}
	// The probe target is set before proxy_pass, so the client's query
	// string can't choose another DSN.
	i := strings.Index(conf, `set $args "target=`)
	j := strings.Index(conf, "proxy_pass http://127.0.0.1:9187/probe;")
	if i < 0 || j < 0 || i > j {
		t.Errorf("probe location:\n%s", conf)
	}
	if strings.Contains(conf, "/probe?") {
		t.Error("the probe target must not ride in proxy_pass")
	}
	allowed := string(render.MetricsConf(MetricsLocations(cfg), MetricsHtpasswdPath, []string{"198.51.100.7/24", "2001:db8::1"}))
	if !strings.Contains(allowed, "allow 198.51.100.0/24;") || !strings.Contains(allowed, "allow 2001:db8::1;") || !strings.Contains(allowed, "deny all;") ||
		!strings.Contains(allowed, "allow 127.0.0.1;") {
		t.Errorf("allowlist:\n%s", allowed)
	}
}

func TestTargetsJSON(t *testing.T) {
	cfg := metricsCluster("app")
	raw := render.TargetsJSON("db1.example.com", MetricsLocations(cfg))
	var targets []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(raw, &targets); err != nil {
		t.Fatal(err)
	}
	if len(targets) != 4 {
		t.Fatalf("%d targets, want node, app and both poolers", len(targets))
	}
	for _, tg := range targets {
		if tg.Targets[0] != "db1.example.com:443" || tg.Labels["__scheme__"] != "https" || !strings.HasPrefix(tg.Labels["__metrics_path__"], "/metrics/") {
			t.Errorf("target %+v", tg)
		}
		if tg.Labels["vaire_server"] != "prod" || tg.Labels["vaire_node"] != "db1" {
			t.Errorf("labels %v", tg.Labels)
		}
	}
}

func TestExporterUnits(t *testing.T) {
	units := exporterUnits(metricsCluster())
	if _, ok := units[PgbouncerExporterTx]; ok {
		t.Error("no databases, but a pgbouncer_exporter")
	}
	node := string(units[NodeExporterService])
	for _, want := range []string{
		"User=node_exporter", "Slice=vaire-exporters.slice", "--web.listen-address=127.0.0.1:9100",
		"--collector.textfile.directory=/var/lib/node_exporter/textfile", "MemoryMax=64M", "ProtectSystem=strict",
		"/usr/local/lib/vaire/exporters/node_exporter-1.12.1/node_exporter",
	} {
		if !strings.Contains(node, want) {
			t.Errorf("node_exporter unit lacks %s:\n%s", want, node)
		}
	}
	if strings.Contains(node, `\.`) {
		t.Error("systemd would warn about the backslash escape in ExecStart")
	}
	pg := string(units[PostgresExporterService])
	if !strings.Contains(pg, "--collector.stat_statements") || strings.Contains(pg, "DATA_SOURCE_NAME") || !strings.Contains(pg, "127.0.0.1:9187") {
		t.Errorf("postgres_exporter unit:\n%s", pg)
	}
	with := exporterUnits(metricsCluster("app"))
	sess := string(with[PgbouncerExporterSess])
	if !strings.Contains(sess, "EnvironmentFile="+PgbouncerSessionExporterEnv) || !strings.Contains(sess, "127.0.0.1:9128") {
		t.Errorf("session pgbouncer_exporter unit:\n%s", sess)
	}
}

func TestPgbouncerConnection(t *testing.T) {
	got := pgbouncerConnection(SessionSocketDir, SessionPoolerPort, "pw")
	want := "PGBOUNCER_EXPORTER_CONNECTION_STRING=postgres://vaire_monitor:pw@/pgbouncer?host=/run/vaire-pgbouncer-session&port=5432&sslmode=disable\n"
	if got != want {
		t.Errorf("got %q", got)
	}
}

func TestMetricsFilesKeepSecretsOut(t *testing.T) {
	for _, f := range metricsFiles(metricsCluster("app")) {
		if f.path == MetricsHtpasswdPath || f.path == PgbouncerExporterEnv || f.path == PgbouncerSessionExporterEnv {
			t.Errorf("%s holds a secret and must not be in the rendered files", f.path)
		}
	}
}

func TestNginxConfProxyProtocol(t *testing.T) {
	conf := string(render.NginxConf(metricsCluster("app"), "db1.example.com", ACMEWebroot))
	for _, want := range []string{
		"proxy_protocol on;",
		"listen 127.0.0.1:6433 ssl proxy_protocol;",
		"listen 127.0.0.1:6434 ssl proxy_protocol;",
		"listen 127.0.0.1:6435 proxy_protocol;",
		"listen 127.0.0.1:8443 ssl proxy_protocol default_server;",
		"real_ip_header proxy_protocol;",
		"include /etc/nginx/vaire-metrics*.conf;",
		"limit_req_zone $binary_remote_addr zone=vaire_metrics",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("nginx.conf lacks %q", want)
		}
	}
}
