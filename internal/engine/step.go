package engine

import (
	"context"
	"fmt"
	"regexp"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/secrets"
)

// Step is one versioned, idempotent unit of host convergence (§9.1).
type Step interface {
	ID() string
	Describe(cfg config.Cluster) string
	// Inputs returns canonical bytes of everything Apply depends on. Secrets
	// are hashed, never raw.
	Inputs(cfg config.Cluster, rt Runtime) ([]byte, error)
	Check(ctx context.Context, cfg config.Cluster, rt Runtime) (CheckResult, error)
	Apply(ctx context.Context, cfg config.Cluster, rt Runtime, report Reporter) (ApplyResult, error)
}

// CheckResult says whether the host already matches the step.
type CheckResult struct {
	Complete bool
	Detail   string
}

// RestartNeed is what a step needs after Apply.
type RestartNeed string

// Restart needs. A reload is the step's own job; a restart is aggregated and
// performed at most once per run.
const (
	RestartNone    RestartNeed = "none"
	RestartReload  RestartNeed = "reload"
	RestartRestart RestartNeed = "restart"
)

// ApplyResult is what Apply changed.
type ApplyResult struct {
	Restart RestartNeed
	Notes   []string
}

// Runtime is what a step may use while it runs.
type Runtime struct {
	DryRun  bool
	Shell   Shell
	Secrets *secrets.Registry
	Log     *RunLog
	// Host hostinfo.Info arrives in M3, PG *pg.Local in M4.
}

// Restarter performs the aggregated restart at the end of an apply. M4
// provides the PostgreSQL restarter.
type Restarter interface {
	Restart(ctx context.Context) error
}

// IDPattern is the versioned step ID format, for example postgres-config-v1.
var IDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$`)

// ValidateIDs checks every ID against IDPattern and refuses duplicates.
func ValidateIDs(steps []Step) error {
	seen := map[string]bool{}
	for _, step := range steps {
		id := step.ID()
		if !IDPattern.MatchString(id) {
			return fmt.Errorf("step ID %q does not match %s", id, IDPattern)
		}
		if seen[id] {
			return fmt.Errorf("duplicate step ID %q", id)
		}
		seen[id] = true
	}
	return nil
}
