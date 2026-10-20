package steps

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/render"
)

// exporterConnString is what postgres_exporter 0.20.1 hands lib/pq for a
// URL-form /probe target (config/dsn.go: dsnFromURL moves user out of the
// query, then GetConnectionString rebuilds the URL with url.URL.String).
func exporterConnString(t *testing.T, target string) string {
	t.Helper()
	if !strings.HasPrefix(target, "postgresql://") {
		t.Fatalf("target %q is not a URL: postgres_exporter would parse it as key=value and put the socket path in the URL authority", target)
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	user := u.User.Username()
	if user == "" {
		user = q.Get("user")
	}
	q.Del("user")
	q.Del("password")
	out := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, RawQuery: q.Encode()}
	if user != "" {
		out.User = url.UserPassword(user, "")
	}
	return out.String()
}

// TestPostgresProbeReachesSocket covers the edge-3d8650b bug: every probe
// failed with pg_up 0 after about a second (GetServer's retry sleep)
// because the target never became a connection PostgreSQL could see.
func TestPostgresProbeReachesSocket(t *testing.T) {
	cfg := metricsCluster("app", "other")
	conf := string(render.MetricsConf(MetricsLocations(cfg), MetricsHtpasswdPath, nil))
	re := regexp.MustCompile(`location = /metrics/postgres/other \{[^}]*set \$args "([^"]*)";[^}]*proxy_pass http://127\.0\.0\.1:9187/probe;`)
	m := re.FindStringSubmatch(conf)
	if m == nil {
		t.Fatalf("no probe location for other:\n%s", conf)
	}
	// nginx passes $args as the upstream query string verbatim.
	q, err := url.ParseQuery(m[1])
	if err != nil {
		t.Fatalf("args %q: %v", m[1], err)
	}
	if len(q) != 1 || q.Get("auth_module") != "" {
		t.Errorf("probe args %v: want target only (no auth_module, peer auth)", q)
	}
	conn := exporterConnString(t, q.Get("target"))
	// lib/pq parses the connection string with url.Parse; a socket path in
	// the authority comes out as %2F, which it refuses.
	u, err := url.Parse(conn)
	if err != nil {
		t.Fatalf("lib/pq would refuse %q: %v", conn, err)
	}
	if u.Host != "" {
		t.Errorf("host %q in the authority: lib/pq would dial TCP", u.Host)
	}
	got := u.Query()
	if got.Get("host") != PGSocketDir || got.Get("port") != "5434" || got.Get("sslmode") != "disable" || got.Get("connect_timeout") != "5" {
		t.Errorf("connection %q: want the socket %s, port 5434", conn, PGSocketDir)
	}
	if u.Path != "/other" || u.User.Username() != "vaire_monitor" {
		t.Errorf("connection %q: want database other as vaire_monitor", conn)
	}
	if p, _ := u.User.Password(); p != "" || got.Get("password") != "" {
		t.Errorf("connection %q carries a password; vaire_monitor uses peer auth", conn)
	}

	// The old key=value form, as postgres_exporter rebuilt it, is what
	// lib/pq refused.
	old := url.URL{Scheme: "postgresql", User: url.UserPassword("vaire_monitor", ""), Host: PGSocketDir + ":5434"}
	if _, err := url.Parse(old.String()); err == nil {
		t.Error("the key=value regression check no longer reproduces the refusal")
	}
}

// TestPostgresExporterUnitReachesSocket: the unit must leave AF_UNIX and the
// socket directory alone, and pg_ident must map its user to vaire_monitor.
func TestPostgresExporterUnitReachesSocket(t *testing.T) {
	unit := string(exporterUnits(metricsCluster("app"))[PostgresExporterService])
	if !strings.Contains(unit, "\nUser=postgres_exporter\n") {
		t.Errorf("postgres_exporter runs as another user:\n%s", unit)
	}
	for _, line := range strings.Split(unit, "\n") {
		key, value, _ := strings.Cut(line, "=")
		switch key {
		case "RestrictAddressFamilies":
			if strings.HasPrefix(value, "~") == strings.Contains(value, "AF_UNIX") {
				t.Errorf("%s blocks the PostgreSQL socket", line)
			}
		case "InaccessiblePaths", "TemporaryFileSystem", "BindPaths", "BindReadOnlyPaths":
			for _, p := range strings.Fields(value) {
				p = strings.TrimLeft(p, "-+")
				if strings.HasPrefix(PGSocketDir, p) || strings.HasPrefix("/run/postgresql", p) {
					t.Errorf("%s hides %s", line, PGSocketDir)
				}
			}
		case "ProtectSystem":
			// strict makes /run read-only, which still lets the exporter
			// connect to a socket there; nothing stricter exists.
			if value != "strict" && value != "full" && value != "yes" && value != "" {
				t.Errorf("unexpected %s", line)
			}
		case "SystemCallFilter":
			if strings.Contains(value, "@network-io") && strings.HasPrefix(value, "~") {
				t.Errorf("%s blocks connect()", line)
			}
		}
	}
	ident := string(render.Ident(metricsCluster("app")))
	if !regexp.MustCompile(`(?m)^vaire\s+postgres_exporter\s+vaire_monitor$`).MatchString(ident) {
		t.Errorf("pg_ident lacks postgres_exporter -> vaire_monitor:\n%s", ident)
	}
	if !regexp.MustCompile(`(?m)^local\s+all\s+vaire_monitor\s+peer map=vaire$`).MatchString(string(render.HBA())) {
		t.Error("pg_hba lacks vaire_monitor's peer rule")
	}
}
