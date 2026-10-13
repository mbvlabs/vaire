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

// SecurityConf renders a cluster's conf.d/20-eru-security.conf (§14.1): no
// TCP listener, and port only names the socket (RFC 8.2).
func SecurityConf(port int) []byte {
	return execute("20-eru-security.conf.tmpl", map[string]any{"Port": port})
}

// ExtensionsConf renders a cluster's conf.d/30-eru-extensions.conf:
// shared_preload_libraries and the catalog settings of its database's
// extensions. auto_explain.log_min_duration is twice the tuned
// log_min_duration_statement.
func ExtensionsConf(spec config.ClusterSpec, res tune.Result) []byte {
	enabled := append([]string(nil), spec.Extensions...)
	sort.Strings(enabled)
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

// SettingsConf renders a cluster's conf.d/50-eru-settings.conf: the
// setup-wide [postgres.settings], with the cluster's [clusters.settings]
// winning.
func SettingsConf(cfg config.Cluster, spec config.ClusterSpec) []byte {
	merged := map[string]string{}
	for k, v := range cfg.Postgres.Settings {
		merged[k] = v
	}
	for k, v := range spec.Settings {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	items := make([]setting, 0, len(keys))
	for _, k := range keys {
		items = append(items, setting{Name: k, Value: quote(merged[k])})
	}
	return execute("50-eru-settings.conf.tmpl", map[string]any{"Settings": items})
}

// quote single-quotes a PostgreSQL string literal, doubling embedded quotes.
func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// OverridesHeader renders conf.d/90-overrides.conf.
func OverridesHeader() []byte { return execute("90-overrides.conf.tmpl", nil) }

// HBA renders a cluster's pg_hba.conf (§14.1). It is the same for every
// cluster and allows the socket only (RFC 12).
func HBA() []byte { return execute("pg_hba.conf.tmpl", nil) }

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
		"MaxDBConnections":  maxInt(maxConnections, 1),
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
		"Hostname":    cfg.MasterHostname(),
		"WGSubnet":    cfg.Transport.Subnet,
		"Allowlist":   allow,
		"ACMEWebroot": acmeWebroot,
	})
}

// PgbouncerHBA renders /etc/pgbouncer/pgbouncer_hba.conf: TLS everywhere
// except the loopback hop from nginx.
func PgbouncerHBA() []byte { return execute("pgbouncer_hba.conf.tmpl", nil) }

// PGServiceDropin renders a cluster's postgresql@18-<cluster>.service
// drop-in: its cgroup limits (RFC 7.2). MemoryHigh is the cluster's memory
// plus a 25% allowance for backends; MemoryMax and CPUQuota only when set.
func PGServiceDropin(r config.Resources) []byte {
	return execute("postgresql-service.conf.tmpl", map[string]any{
		"MemoryHigh": MemoryHighMiB(r),
		"MemoryMax":  r.MemoryMaxMiB,
		"CPUWeight":  r.CPUWeight,
		"CPUQuota":   r.CPUQuota,
		"IOWeight":   r.IOWeight,
	})
}

// MemoryHighMiB is a cluster's MemoryHigh: its memory plus 25%.
func MemoryHighMiB(r config.Resources) int { return r.MemoryMiB + r.MemoryMiB/4 }

// CertsService renders eru-certs.service, which runs eru node run
// certs-renew (D11). tls-v1 installs it and starts it once.
func CertsService() []byte { return execute("eru-certs.service.tmpl", nil) }

// CertsTimer renders eru-certs.timer, the hourly certificate check. timers-v1
// owns it from Phase 3 on.
func CertsTimer() []byte { return execute("eru-certs.timer.tmpl", nil) }

// BackupSecrets are the values pgbackrest.conf carries that never enter a
// step's inputs.
type BackupSecrets struct {
	KeyID      string
	Secret     string
	CipherPass string
}

// PgBackRestConf renders /etc/pgbackrest/pgbackrest.conf (§13.1).
func PgBackRestConf(cfg config.Cluster, host hostinfo.Info, sec BackupSecrets) []byte {
	// One stanza, the first cluster's, until the file gets a section per
	// cluster.
	first, _ := cfg.BackupCluster()
	endpoint := strings.TrimSuffix(strings.TrimPrefix(cfg.Backup.Endpoint, "https://"), "/")
	hostPart, port := endpoint, ""
	if h, p, ok := strings.Cut(endpoint, ":"); ok {
		hostPart, port = h, p
	}
	return execute("pgbackrest.conf.tmpl", map[string]any{
		"EndpointHost": hostPart,
		"EndpointPort": port,
		"Bucket":       cfg.Backup.Bucket,
		"Region":       cfg.Backup.Region,
		"URIStyle":     cfg.Backup.URIStyle,
		"KeyID":        sec.KeyID,
		"Secret":       sec.Secret,
		"Prefix":       cfg.Backup.Prefix,
		"CipherPass":   sec.CipherPass,
		// retention_full is the restore window in weeks. Time-based
		// retention keeps it, however many full backups are taken in it.
		"RetentionDays": cfg.Backup.Database.RetentionFull * 7,
		"LocalRepo":     cfg.Backup.Database.LocalRepo,
		"ProcessMax":    maxInt(1, host.VCPU/2),
		"Stanza":        first.Stanza,
		"Port":          first.Port,
		"DataDirectory": cfg.Server.DataDirectory(first.Name),
	})
}

// BackupConf renders a cluster's conf.d/40-eru-backup.conf: archive_mode on
// from creation, with archive-push to the cluster's stanza once [backup]
// names a bucket and /bin/true before (RFC 9.1).
func BackupConf(cfg config.Cluster, spec config.ClusterSpec) []byte {
	stanza := ""
	if cfg.BackupConfigured() {
		stanza = spec.Stanza
	}
	return execute("40-eru-backup.conf.tmpl", map[string]any{"Stanza": stanza})
}

// JobService renders the eru-<job>.service a timer starts (§18). arg is
// appended to eru node run <job>, for example --db %i for the dump template.
func JobService(job, arg string) []byte {
	return execute("eru-job.service.tmpl", map[string]any{"Job": job, "Arg": arg})
}

// JobTimer renders an eru-<job>.timer (§18).
func JobTimer(description, schedule, jitter string) []byte {
	return execute("eru-job.timer.tmpl", map[string]any{"Description": description, "Schedule": schedule, "Jitter": jitter})
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
