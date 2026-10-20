package steps

import (
	"os"
	"slices"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/tune"
)

// PostgreSQL layout on the node (§11, §14).
const (
	// PGVersion is the pinned PostgreSQL major.
	PGVersion = 18
	// PGSocketDir holds every cluster's Unix socket; a cluster's port names
	// its socket file.
	PGSocketDir = "/var/run/postgresql"
	// TLSDir holds the certificate PostgreSQL and PgBouncer serve.
	TLSDir = "/etc/vaire/tls"
	// TLSCertPath is the served certificate.
	TLSCertPath = TLSDir + "/server.crt"
	// TLSKeyPath is the served private key.
	TLSKeyPath = TLSDir + "/server.key"
	// TLSDirMode is TLSDir's mode: PostgreSQL, PgBouncer and nginx enter it.
	TLSDirMode os.FileMode = 0o755
	// TLSCertOwner, TLSCertGroup and TLSCertMode are the served
	// certificate's: world-readable.
	TLSCertOwner             = "root"
	TLSCertGroup             = "root"
	TLSCertMode  os.FileMode = 0o644
	// TLSKeyOwner, TLSKeyGroup and TLSKeyMode are the served private key's.
	// PostgreSQL and PgBouncer are in the ssl-cert group.
	TLSKeyOwner             = "root"
	TLSKeyGroup             = "ssl-cert"
	TLSKeyMode  os.FileMode = 0o640
	// RuntimeDir holds the secrets services need at runtime.
	RuntimeDir = "/etc/vaire/runtime"
	// PgbouncerDir holds the PgBouncer configuration.
	PgbouncerDir = "/etc/pgbouncer"
	// PgbouncerIniPath is the rendered PgBouncer configuration.
	PgbouncerIniPath = PgbouncerDir + "/pgbouncer.ini"
	// PgbouncerUserlistPath is the rendered PgBouncer userlist.
	PgbouncerUserlistPath = PgbouncerDir + "/userlist.txt"
	// PgbouncerHBAPath is PgBouncer's own hba file.
	PgbouncerHBAPath = PgbouncerDir + "/pgbouncer_hba.conf"
	// SessionIniPath is the session pooler's configuration (RFC 8.1).
	SessionIniPath = PgbouncerDir + "/session.ini"
	// SessionHBAPath is the session pooler's hba file.
	SessionHBAPath = PgbouncerDir + "/session_hba.conf"
	// SessionUnitPath is the session pooler's systemd unit.
	SessionUnitPath = SystemdSystemDir + "/" + SessionPoolerService + ".service"
	// SessionSocketDir is the session pooler's runtime directory: its socket,
	// which carries its admin console, and its pidfile.
	SessionSocketDir = "/run/" + SessionPoolerService
	// NginxConfPath is the rendered nginx configuration.
	NginxConfPath = "/etc/nginx/nginx.conf"
	// ACMEDir holds the ACME account key.
	ACMEDir = "/etc/vaire/acme"
	// ACMEDirMode is ACMEDir's mode: root only.
	ACMEDirMode os.FileMode = 0o700
	// ACMEAccountKeyPath is the ACME account's private key, root 0600.
	ACMEAccountKeyPath             = ACMEDir + "/account.key"
	ACMEAccountKeyMode os.FileMode = 0o600
	// ACMEWebroot is served by nginx for HTTP-01 challenges. It sits outside
	// /var/lib/vaire, which is root 0700 and that nginx's workers can't enter.
	ACMEWebroot = "/var/lib/vaire-acme"
	// ACMEChallengeDir is where challenge tokens are written.
	ACMEChallengeDir = ACMEWebroot + "/.well-known/acme-challenge"
	// SysctlPath is the sysctl drop-in.
	SysctlPath = "/etc/sysctl.d/90-vaire.conf"
	// THPUnitPath is the transparent-huge-pages oneshot unit.
	THPUnitPath = "/etc/systemd/system/vaire-thp.service"
	// CreateClusterConfPath disables Debian's automatic cluster creation.
	CreateClusterConfPath = "/etc/postgresql-common/createcluster.d/00-vaire.conf"
	// SystemdSystemDir holds the vaire-certs units.
	SystemdSystemDir = "/etc/systemd/system"
	// PgbouncerService is the PgBouncer systemd unit.
	PgbouncerService = "pgbouncer"
	// PgbouncerPort is the transaction pooler's port, behind nginx on 443.
	PgbouncerPort = 6432
	// SessionPoolerService is the session pooler's systemd unit.
	SessionPoolerService = "vaire-pgbouncer-session"
	// SessionPoolerPort is the session pooler's port: where operators,
	// migrations and tools that need a session connect (RFC 8.1).
	SessionPoolerPort = 5432
	// NginxService is the nginx systemd unit.
	NginxService = "nginx"
)

// Each database is its own PostgreSQL cluster, 18/<name> in
// postgresql-common's terms (RFC 5), so its paths and units are functions of
// the cluster's name.

// PGConfDir is the cluster's configuration directory.
func PGConfDir(cluster string) string { return "/etc/postgresql/18/" + cluster }

// PGConfD is the cluster's conf.d directory, which vaire manages.
func PGConfD(cluster string) string { return PGConfDir(cluster) + "/conf.d" }

// PGServiceName is the cluster's systemd unit.
func PGServiceName(cluster string) string { return "postgresql@18-" + cluster }

// PGServiceDropInDir is the cluster unit's drop-in directory.
func PGServiceDropInDir(cluster string) string {
	return SystemdSystemDir + "/" + PGServiceName(cluster) + ".service.d"
}

// PGServiceDropIn is the cluster unit's vaire drop-in: its cgroup limits.
func PGServiceDropIn(cluster string) string { return PGServiceDropInDir(cluster) + "/10-vaire.conf" }

// PGLogPath is the cluster's log file.
func PGLogPath(cluster string) string { return "/var/log/postgresql/postgresql-18-" + cluster + ".log" }

// ClusterPG talks to spec's cluster on its socket.
func ClusterPG(spec config.ClusterSpec) pg.Local {
	return pg.Local{Host: PGSocketDir, Port: spec.Port}
}

// AdminPG talks to spec's cluster as vaire_admin over its socket, as root
// (pg_ident maps root to vaire_admin only): maintenance and reports run with
// vaire_admin's pg_maintain, pg_monitor and pg_signal_backend, not as the
// superuser (Phase 4).
func AdminPG(spec config.ClusterSpec) pg.Local {
	return pg.Local{Host: PGSocketDir, Port: spec.Port, User: "vaire_admin", AsRoot: true}
}

// PoolerAdminPG talks to a pooler's admin console as the pgbouncer user,
// which PgBouncer lets in over its socket from the postgres OS user.
func PoolerAdminPG(socketDir string, port int) pg.Local {
	return pg.Local{Host: socketDir, Port: port, User: "pgbouncer"}
}

// PostgreSQL and PgBouncer both run as the postgres user, so every
// configuration file they read is owned by postgres:postgres and written with
// that owner from the start.
const (
	pgFileOwner = "postgres"
	pgFileGroup = "postgres"
)

// tuningFor computes spec's tuning on host (RFC 7.3): the cluster's memory
// stands in for RAM, its share of the CPU caps the worker settings, its size
// sets max_connections, and its share of the data disk, in proportion to its
// memory, sizes WAL (RFC 7.7). The shares count the databases awaiting
// recovery too, so a database recovered first keeps its tuning when the
// others come back.
func tuningFor(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) tune.Result {
	res, _ := cfg.Resources(spec)
	profile := spec.Profile
	if profile == "" {
		profile = cfg.Postgres.Profile
	}
	return tune.Calculate(tune.Input{
		RAMMiB:            res.MemoryMiB,
		VCPU:              host.VCPU,
		Clusters:          len(cfg.Clusters) + len(cfg.Awaiting),
		DiskRotational:    host.DataDisk.Rotational,
		DataDiskGiB:       diskShareGiB(cfg, res.MemoryMiB, host.DataDisk.SizeGiB),
		Profile:           profile,
		MaxConnections:    res.MaxConnections,
		ClientConnections: cfg.Postgres.ClientConnections,
		PGMajor:           cfg.Postgres.Version,
	})
}

// diskShareGiB is a cluster's share of the data disk: diskGiB in proportion
// of memoryMiB to the memory of every cluster.
func diskShareGiB(cfg config.Cluster, memoryMiB, diskGiB int) int {
	total := 0
	for _, c := range slices.Concat(cfg.Clusters, cfg.Awaiting) {
		if r, err := cfg.Resources(c); err == nil {
			total += r.MemoryMiB
		}
	}
	if total <= 0 || memoryMiB >= total {
		return diskGiB
	}
	return diskGiB * memoryMiB / total
}

// hugePagesFor is the huge pages the server reserves (RFC 7.3): the sum of
// the estimates of every cluster whose tuning has huge_pages = try.
func hugePagesFor(cfg config.Cluster, host hostinfo.Info) int {
	total := 0
	for _, c := range cfg.Clusters {
		total += tuningFor(cfg, c, host).HugePages
	}
	return total
}

// TuningFor is spec's tuning, for tune show.
func TuningFor(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) tune.Result {
	return tuningFor(cfg, spec, host)
}

// HugePagesFor is the server's huge-page pool, for tune show.
func HugePagesFor(cfg config.Cluster, host hostinfo.Info) int {
	return hugePagesFor(cfg, host)
}
