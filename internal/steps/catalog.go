package steps

import "github.com/mbvlabs/eru/internal/engine"

// Catalog returns the steps in run order (§11). Phase 1 has the host steps
// 1-6; Phase 2 appends 7 onwards.
func Catalog() []engine.Step {
	return []engine.Step{
		basePackages{},
		adminUser{},
		wireguard{},
		sshdHardening{},
		firewall{},
		hostBaseline{},
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

// Restarter returns the restarter for aggregated restarts. Nil until Phase 2.
func Restarter() engine.Restarter { return nil }
