package steps

import (
	"strconv"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/tune"
)

// PostgreSQL layout on the node (§11, §14).
const (
	// PGVersion is the pinned PostgreSQL major.
	PGVersion = 18
	// PGCluster is the postgresql-common cluster name.
	PGCluster = "main"
	// PGConfDir is the cluster's configuration directory.
	PGConfDir = "/etc/postgresql/18/main"
	// PGConfD is the conf.d directory eru manages.
	PGConfD = PGConfDir + "/conf.d"
	// PGServiceName is the systemd unit for the cluster.
	PGServiceName = "postgresql@18-main"
	// TLSDir holds the certificate PostgreSQL and PgBouncer serve.
	TLSDir = "/etc/eru/tls"
	// TLSCertPath is the served certificate.
	TLSCertPath = TLSDir + "/server.crt"
	// TLSKeyPath is the served private key.
	TLSKeyPath = TLSDir + "/server.key"
	// RuntimeDir holds the secrets services need at runtime.
	RuntimeDir = "/etc/eru/runtime"
	// PgbouncerDir holds the PgBouncer configuration.
	PgbouncerDir = "/etc/pgbouncer"
	// PgbouncerIniPath is the rendered PgBouncer configuration.
	PgbouncerIniPath = PgbouncerDir + "/pgbouncer.ini"
	// PgbouncerUserlistPath is the rendered PgBouncer userlist.
	PgbouncerUserlistPath = PgbouncerDir + "/userlist.txt"
	// CaddyDir holds the Caddy configuration and data.
	CaddyDir = "/etc/caddy"
	// CaddyfilePath is the rendered Caddyfile.
	CaddyfilePath = CaddyDir + "/Caddyfile"
	// CaddyDataDir is Caddy's data directory (certificates, ACME account).
	CaddyDataDir = "/var/lib/caddy"
	// CaddyCertRoot is where Caddy stores issued certificates.
	CaddyCertRoot = CaddyDataDir + "/.local/share/caddy/certificates"
	// SysctlPath is the sysctl drop-in.
	SysctlPath = "/etc/sysctl.d/90-eru.conf"
	// THPUnitPath is the transparent-huge-pages oneshot unit.
	THPUnitPath = "/etc/systemd/system/eru-thp.service"
	// CreateClusterConfPath disables Debian's automatic cluster creation.
	CreateClusterConfPath = "/etc/postgresql-common/createcluster.d/00-eru.conf"
	// PGServiceDropInDir is the cluster service drop-in directory.
	PGServiceDropInDir = "/etc/systemd/system/postgresql@18-main.service.d"
	// PGServiceDropIn is the cluster service drop-in file.
	PGServiceDropIn = PGServiceDropInDir + "/10-eru.conf"
	// SystemdSystemDir holds the eru-certs-sync units.
	SystemdSystemDir = "/etc/systemd/system"
	// PgbouncerService is the PgBouncer systemd unit.
	PgbouncerService = "pgbouncer"
	// CaddyService is the Caddy systemd unit.
	CaddyService = "caddy"
)

// tuningFor computes the tuning result for cfg on host.
func tuningFor(cfg config.Cluster, host hostinfo.Info) tune.Result {
	return tune.Calculate(tune.Input{
		RAMMiB:            host.RAMMiB,
		VCPU:              host.VCPU,
		DiskRotational:    host.DataDisk.Rotational,
		DataDiskGiB:       host.DataDisk.SizeGiB,
		Profile:           cfg.Postgres.Profile,
		ClientConnections: cfg.Postgres.ClientConnections,
		PGMajor:           cfg.Postgres.Version,
	})
}

// maxConnectionsFor returns the tuned max_connections, or the 100 default when
// the host facts are missing (a dry run on an operator machine).
func maxConnectionsFor(cfg config.Cluster, host hostinfo.Info) int {
	res := tuningFor(cfg, host)
	for _, s := range res.Settings {
		if s.Name == "max_connections" {
			if n, err := strconv.Atoi(s.Value); err == nil && n > 0 {
				return n
			}
		}
	}
	return 100
}
