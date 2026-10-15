package steps

import (
	"context"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

// Catalog returns the steps in run order (§11, D1). Phase 2 has steps 1-13,
// Phase 3 adds 14 (pgbackrest-v1) and 17 (timers-v1), Phase 4 adds 16
// (metrics-v1), Phase 5 adds 15 (master-backup-v1); step 18, doctor, is a
// gate after apply, not a step.
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
		masterBackupStep{},
		metricsStep{},
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
		masterBackupID,
		metricsID,
		timersID,
	}
}

// BackupStepIDs are the steps eru backup setup runs after writing [backup]:
// every cluster's archive_command (a reload), the repository with a stanza
// per cluster, and the timers.
func BackupStepIDs() []string {
	return []string{postgresConfigID, pgbackrestID, masterBackupID, timersID}
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

// Doctor's drift checks run these steps' Check (Phase 4).
const (
	SSHDStepID     = sshdHardeningID
	FirewallStepID = firewallID
)

// CheckStep runs the Check of the catalogue step (or instance) id without
// applying anything, for doctor's drift checks. detail is empty when the
// host matches.
func CheckStep(ctx context.Context, cfg config.Cluster, rt engine.Runtime, id string) (string, error) {
	for _, step := range engine.Expand(Catalog(), cfg) {
		if step.ID() != id {
			continue
		}
		res, err := step.Check(ctx, cfg, rt)
		if err != nil {
			return "", err
		}
		if res.Complete {
			return "", nil
		}
		if res.Detail == "" {
			return "it differs", nil
		}
		return res.Detail, nil
	}
	return "", nil
}

// ServiceActive reports whether a systemd unit is active.
func ServiceActive(ctx context.Context, unit string) bool { return serviceActive(ctx, unit) }

// ConfigStepID is cluster's postgres-config-v1 instance. db create, db fork
// and db drop run it for the other databases whose share of the server
// changed.
func ConfigStepID(cluster string) string { return engine.InstanceID(postgresConfigID, cluster) }
