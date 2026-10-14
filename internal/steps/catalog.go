package steps

import "github.com/mbvlabs/eru/internal/engine"

// Catalog returns the steps in run order (§11, D1). Phase 2 has steps 1-13,
// Phase 3 adds 14 (pgbackrest-v1) and 17 (timers-v1); later phases add theirs.
func Catalog() []engine.Step {
	return []engine.Step{
		basePackages{},
		adminUser{},
		wireguard{},
		sshdHardening{},
		firewall{},
		hostBaseline{},
		kernelTuning{},
		postgresPackages{},
		postgresCluster{},
		tlsStep{},
		postgresConfig{},
		postgresRoles{},
		pgbouncer{},
		pgbackrestStep{},
		timersStep{},
	}
}

// HostStepIDs are the steps cluster.bootstrap phase=host runs (steps 1-6).
func HostStepIDs() []string {
	return []string{
		basePackagesID,
		adminUserID,
		wireguardID,
		sshdHardeningID,
		firewallID,
		hostBaselineID,
	}
}

// DatabaseStepIDs are the steps cluster.bootstrap phase=database runs (steps
// 7-14 and 17, D1). pgbackrest-v1 skips itself until [backup] names a bucket. tls-v1 runs before postgres-config-v1: the security settings turn
// ssl on and point at /etc/eru/tls, so the certificate and the ssl-cert group
// membership have to exist before PostgreSQL restarts.
func DatabaseStepIDs() []string {
	return []string{
		kernelTuningID,
		postgresPackagesID,
		postgresClusterID,
		tlsID,
		postgresConfigID,
		postgresRolesID,
		pgbouncerID,
		pgbackrestID,
		timersID,
	}
}

// BackupStepIDs are the steps eru backup setup runs after writing [backup]:
// every cluster's archive_command (a reload), the repository with a stanza
// per cluster, and the timers.
func BackupStepIDs() []string {
	return []string{postgresConfigID, pgbackrestID, timersID}
}

// PgBackRestStepIDs is the repository step db create runs once the new
// cluster is up, for its stanza and first full backup.
func PgBackRestStepIDs() []string { return []string{pgbackrestID} }

// PackagesStepIDs is the step that installs pgBackRest. eru backup setup
// plans it to refuse a node that eru apply hasn't brought up to date.
func PackagesStepIDs() []string { return []string{postgresPackagesID} }

// TimersStepIDs is the step a per-database dump schedule change needs.
func TimersStepIDs() []string { return []string{timersID} }

// RolesStepIDs is the step db.create runs on its own.
func RolesStepIDs() []string { return []string{postgresRolesID} }

// PoolerStepIDs is the step db create and db drop run after the cluster
// changes, so the poolers pick up the database's entries with a reload.
func PoolerStepIDs() []string { return []string{pgbouncerID} }

// KernelStepIDs is the kernel tuning step, which sizes the huge-page pool
// for every cluster; db create runs it before a new cluster starts.
func KernelStepIDs() []string { return []string{kernelTuningID} }

// TuneStepIDs are the steps tune.apply runs.
func TuneStepIDs() []string { return []string{kernelTuningID, postgresConfigID} }

// ConfigStepID is cluster's postgres-config-v1 instance. db create, db fork
// and db drop run it for the other databases whose share of the server
// changed.
func ConfigStepID(cluster string) string { return engine.InstanceID(postgresConfigID, cluster) }
