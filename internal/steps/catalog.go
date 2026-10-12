package steps

import "github.com/mbvlabs/eru/internal/engine"

// Catalog returns the steps in run order (§11, D1). Phase 2 has steps 1-13;
// later phases append theirs.
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
		postgresConfig{},
		postgresRoles{},
		tlsStep{},
		pgbouncer{},
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
// 7-13, D1).
func DatabaseStepIDs() []string {
	return []string{
		kernelTuningID,
		postgresPackagesID,
		postgresClusterID,
		postgresConfigID,
		postgresRolesID,
		tlsID,
		pgbouncerID,
	}
}

// Restarter returns the PostgreSQL restarter for aggregated restarts.
func Restarter() engine.Restarter { return restarter }

// RolesStepIDs is the step db.create runs on its own.
func RolesStepIDs() []string { return []string{postgresRolesID} }

// TuneStepIDs are the steps tune.apply runs.
func TuneStepIDs() []string { return []string{kernelTuningID, postgresConfigID} }
