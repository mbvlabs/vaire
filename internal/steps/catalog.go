package steps

import "github.com/mbvlabs/eru/internal/engine"

// Catalog returns the steps in run order. Empty until Phase 1 adds steps 1-6.
func Catalog() []engine.Step { return nil }

// Restarter returns the restarter for aggregated restarts. Nil until Phase 2.
func Restarter() engine.Restarter { return nil }
