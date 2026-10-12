// Package render turns cluster.toml and host facts into the managed config
// files on the node (§11.1). Every renderer is pure: the same inputs always
// produce the same bytes, so steps can hash and diff them.
package render

import (
	"bytes"
	"embed"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"github.com/mbvlabs/eru/internal/catalog"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/tune"
)

//go:embed templates/*.tmpl
var tmplFS embed.FS

var tmpl = template.Must(template.New("").ParseFS(tmplFS, "templates/*.tmpl"))

// setting is one rendered name/value pair with an optional reason comment.
type setting struct {
	Name   string
	Value  string
	Reason string
}

// execute renders the named template with data.
func execute(name string, data any) []byte {
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		panic("render " + name + ": " + err.Error())
	}
	return buf.Bytes()
}

// SysctlConf renders /etc/sysctl.d/90-eru.conf (§11.1).
func SysctlConf(host hostinfo.Info, hugePages int) []byte {
	return execute("sysctl.conf.tmpl", map[string]any{"RAMMiB": host.RAMMiB, "HugePages": hugePages})
}

// THPUnit renders /etc/systemd/system/eru-thp.service (§11.1).
func THPUnit() []byte { return execute("eru-thp.service.tmpl", nil) }

// CreateClusterConf renders
// /etc/postgresql-common/createcluster.d/00-eru.conf.
func CreateClusterConf() []byte { return execute("createcluster.conf.tmpl", nil) }

// TuningConf renders conf.d/10-eru-tuning.conf from the tuning result.
func TuningConf(res tune.Result) []byte {
	items := make([]setting, 0, len(res.Settings))
	for _, s := range res.Settings {
		items = append(items, setting{Name: s.Name, Value: s.Value, Reason: s.Reason})
	}
	return execute("10-eru-tuning.conf.tmpl", map[string]any{"Settings": items})
}

// SecurityConf renders conf.d/20-eru-security.conf (§14.1).
func SecurityConf() []byte { return execute("20-eru-security.conf.tmpl", nil) }

// ExtensionsConf renders conf.d/30-eru-extensions.conf: shared_preload_libraries
// and the catalog settings. auto_explain.log_min_duration is twice the tuned
// log_min_duration_statement.
func ExtensionsConf(cfg config.Cluster, res tune.Result) []byte {
	enabled := enabledExtensions(cfg)
	items := make([]setting, 0)
	if preloads := catalog.Preloads(enabled); len(preloads) > 0 {
		items = append(items, setting{Name: "shared_preload_libraries", Value: "'" + strings.Join(preloads, ",") + "'"})
	}
	for _, name := range enabled {
		ext, ok := catalog.Lookup(name)
		if !ok {
			continue
		}
		keys := make([]string, 0, len(ext.Settings))
		for k := range ext.Settings {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, setting{Name: k, Value: ext.Settings[k]})
		}
	}
	if _, ok := catalog.Lookup("auto_explain"); ok {
		base := baseLogDuration(res)
		items = append(items,
			setting{Name: "auto_explain.log_min_duration", Value: "'" + doubleDuration(base) + "'"},
			setting{Name: "auto_explain.sample_rate", Value: "0.1"},
			setting{Name: "auto_explain.log_analyze", Value: "off"},
		)
	}
	return execute("30-eru-extensions.conf.tmpl", map[string]any{"Settings": items})
}

// enabledExtensions is the union of the per-database extension lists, sorted.
func enabledExtensions(cfg config.Cluster) []string {
	seen := map[string]bool{}
	for _, db := range cfg.Databases {
		for _, name := range db.Extensions {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// baseLogDuration is the tuned log_min_duration_statement, defaulting to 1s.
func baseLogDuration(res tune.Result) string {
	for _, s := range res.Settings {
		if s.Name == "log_min_duration_statement" {
			return s.Value
		}
	}
	return "1s"
}

// doubleDuration doubles a PostgreSQL duration literal ("500ms" -> "1000ms").
func doubleDuration(value string) string {
	for _, unit := range []string{"ms", "min", "s", "h"} {
		if rest, ok := strings.CutSuffix(value, unit); ok {
			if n, err := strconv.Atoi(rest); err == nil {
				return strconv.Itoa(n*2) + unit
			}
		}
	}
	return value
}

// SettingsConf renders conf.d/50-eru-settings.conf from [postgres.settings].
func SettingsConf(cfg config.Cluster) []byte {
	keys := make([]string, 0, len(cfg.Postgres.Settings))
	for k := range cfg.Postgres.Settings {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]setting, 0, len(keys))
	for _, k := range keys {
		items = append(items, setting{Name: k, Value: quote(cfg.Postgres.Settings[k])})
	}
	return execute("50-eru-settings.conf.tmpl", map[string]any{"Settings": items})
}

// quote single-quotes a PostgreSQL string literal, doubling embedded quotes.
func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// OverridesHeader renders conf.d/90-overrides.conf.
func OverridesHeader() []byte { return execute("90-overrides.conf.tmpl", nil) }

// HBA renders pg_hba.conf (§14.1).
func HBA(cfg config.Cluster) []byte {
	return execute("pg_hba.conf.tmpl", map[string]any{
		"WGSubnet":  cfg.Transport.Subnet,
		"Allowlist": cfg.Network.ClientAllowlist,
	})
}

// Ident renders pg_ident.conf (§14.1).
func Ident(cfg config.Cluster) []byte {
	return execute("pg_ident.conf.tmpl", map[string]any{"AdminUser": cfg.Server.AdminUser})
}

// PgbouncerIni renders /etc/pgbouncer/pgbouncer.ini (§14.3).
func PgbouncerIni(cfg config.Cluster, host hostinfo.Info, maxConnections int) []byte {
	poolSize := clamp(2*host.VCPU, 10, 50)
	return execute("pgbouncer.ini.tmpl", map[string]any{
		"ClientConnections": cfg.Postgres.ClientConnections,
		"DefaultPoolSize":   poolSize,
		"MaxDBConnections":  maxInt(maxConnections-20, 1),
	})
}

// PgbouncerUserlist renders /etc/pgbouncer/userlist.txt from SCRAM verifiers.
func PgbouncerUserlist(verifiers map[string]string) []byte {
	users := make([]string, 0, len(verifiers))
	for user := range verifiers {
		users = append(users, user)
	}
	sort.Strings(users)
	entries := make([]map[string]string, 0, len(users))
	for _, user := range users {
		entries = append(entries, map[string]string{"User": user, "Verifier": verifiers[user]})
	}
	return execute("userlist.txt.tmpl", map[string]any{"Entries": entries})
}

// NginxConf renders /etc/nginx/nginx.conf (§15.2): ACME challenges on 80,
// and on 443 HTTPS plus direct-TLS PostgreSQL routed to PgBouncer by ALPN.
func NginxConf(cfg config.Cluster, acmeWebroot string) []byte {
	var allow []string
	if cfg.Network.ExposePgbouncer {
		for _, cidr := range cfg.Network.ClientAllowlist {
			if p, err := netip.ParsePrefix(cidr); err == nil {
				allow = append(allow, p.Masked().String())
			}
		}
	}
	return execute("nginx.conf.tmpl", map[string]any{
		"Domain":      cfg.Domain,
		"WGSubnet":    cfg.Transport.Subnet,
		"Allowlist":   allow,
		"ACMEWebroot": acmeWebroot,
	})
}

// PgbouncerHBA renders /etc/pgbouncer/pgbouncer_hba.conf: TLS everywhere
// except the loopback hop from nginx.
func PgbouncerHBA() []byte { return execute("pgbouncer_hba.conf.tmpl", nil) }

// PGServiceDropin renders the postgresql@18-main.service drop-in.
func PGServiceDropin() []byte { return execute("postgresql-service.conf.tmpl", nil) }

// CertsUnits renders the eru-certs service and timer, which run
// eru node run certs-renew (D11).
//
// timers-v1 (Phase 5) later takes ownership of the .timer; this step installs
// it so certificates renew before then.
func CertsUnits() map[string][]byte {
	return map[string][]byte{
		"eru-certs.service": execute("eru-certs.service.tmpl", nil),
		"eru-certs.timer":   execute("eru-certs.timer.tmpl", nil),
	}
}

func clamp(v, lo, hi int) int { return minInt(maxInt(v, lo), hi) }

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
