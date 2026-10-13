package steps

import (
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/tune"
)

// PostgreSQL layout on the node (§11, §14).
const (
	// PGVersion is the pinned PostgreSQL major.
	PGVersion = 18
	// PGSocketDir holds every cluster's Unix socket; a cluster's port names
	// its socket file.
	PGSocketDir = "/var/run/postgresql"
	// TLSDir holds the certificate PostgreSQL and PgBouncer serve.
	TLSDir = "/etc/eru/tls"
	// TLSCertPath is the served certificate.
	TLSCertPath = TLSDir + "/server.crt"
	// TLSKeyPath is the served private key.
	TLSKeyPath = TLSDir + "/server.key"
	// TLSKeyOwner and TLSKeyGroup own the served private key. PostgreSQL and
	// PgBouncer are in the ssl-cert group.
	TLSKeyOwner = "root"
	TLSKeyGroup = "ssl-cert"
	// RuntimeDir holds the secrets services need at runtime.
	RuntimeDir = "/etc/eru/runtime"
	// PgbouncerDir holds the PgBouncer configuration.
	PgbouncerDir = "/etc/pgbouncer"
	// PgbouncerIniPath is the rendered PgBouncer configuration.
	PgbouncerIniPath = PgbouncerDir + "/pgbouncer.ini"
	// PgbouncerUserlistPath is the rendered PgBouncer userlist.
	PgbouncerUserlistPath = PgbouncerDir + "/userlist.txt"
	// PgbouncerHBAPath is PgBouncer's own hba file.
	PgbouncerHBAPath = PgbouncerDir + "/pgbouncer_hba.conf"
	// NginxConfPath is the rendered nginx configuration.
	NginxConfPath = "/etc/nginx/nginx.conf"
	// ACMEDir holds the ACME account key.
	ACMEDir = "/etc/eru/acme"
	// ACMEAccountKeyPath is the ACME account's private key.
	ACMEAccountKeyPath = ACMEDir + "/account.key"
	// ACMEWebroot is served by nginx for HTTP-01 challenges. It sits outside
	// /var/lib/eru, which is root 0700 and that nginx's workers can't enter.
	ACMEWebroot = "/var/lib/eru-acme"
	// ACMEChallengeDir is where challenge tokens are written.
	ACMEChallengeDir = ACMEWebroot + "/.well-known/acme-challenge"
	// SysctlPath is the sysctl drop-in.
	SysctlPath = "/etc/sysctl.d/90-eru.conf"
	// THPUnitPath is the transparent-huge-pages oneshot unit.
	THPUnitPath = "/etc/systemd/system/eru-thp.service"
	// CreateClusterConfPath disables Debian's automatic cluster creation.
	CreateClusterConfPath = "/etc/postgresql-common/createcluster.d/00-eru.conf"
	// SystemdSystemDir holds the eru-certs units.
	SystemdSystemDir = "/etc/systemd/system"
	// PgbouncerService is the PgBouncer systemd unit.
	PgbouncerService = "pgbouncer"
	// NginxService is the nginx systemd unit.
	NginxService = "nginx"
)

// Each database is its own PostgreSQL cluster, 18/<name> in
// postgresql-common's terms (RFC 5), so its paths and units are functions of
// the cluster's name.

// PGConfDir is the cluster's configuration directory.
func PGConfDir(cluster string) string { return "/etc/postgresql/18/" + cluster }

// PGConfD is the cluster's conf.d directory, which eru manages.
func PGConfD(cluster string) string { return PGConfDir(cluster) + "/conf.d" }

// PGServiceName is the cluster's systemd unit.
func PGServiceName(cluster string) string { return "postgresql@18-" + cluster }

// PGServiceDropInDir is the cluster unit's drop-in directory.
func PGServiceDropInDir(cluster string) string {
	return SystemdSystemDir + "/" + PGServiceName(cluster) + ".service.d"
}

// PGServiceDropIn is the cluster unit's eru drop-in: its cgroup limits.
func PGServiceDropIn(cluster string) string { return PGServiceDropInDir(cluster) + "/10-eru.conf" }

// PGLogPath is the cluster's log file.
func PGLogPath(cluster string) string { return "/var/log/postgresql/postgresql-18-" + cluster + ".log" }

// ClusterPG talks to spec's cluster on its socket.
func ClusterPG(spec config.ClusterSpec) pg.Local {
	return pg.Local{Host: PGSocketDir, Port: spec.Port}
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
// memory, sizes WAL (RFC 7.7).
func tuningFor(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) tune.Result {
	res, _ := cfg.Resources(spec)
	profile := spec.Profile
	if profile == "" {
		profile = cfg.Postgres.Profile
	}
	return tune.Calculate(tune.Input{
		RAMMiB:            res.MemoryMiB,
		VCPU:              host.VCPU,
		Clusters:          len(cfg.Clusters),
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
	for _, c := range cfg.Clusters {
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
