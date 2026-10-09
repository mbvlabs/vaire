//go:build !e2etest

package steps

import "github.com/mbvlabs/eru/internal/engine"

// Catalog returns the steps in run order. Empty until M3 adds steps 1-6.
func Catalog() []engine.Step { return nil }

// Restarter returns the restarter for aggregated restarts. Nil until M4.
func Restarter() engine.Restarter { return nil }
