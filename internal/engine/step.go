package engine

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/pg"
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

// RenderedFile is one file a step writes, for plan diffs.
type RenderedFile struct {
	Path    string
	Content []byte
}

// FileRenderer is implemented by steps that write managed files, so plan can
// show what would change on disk. Files holding secrets are left out.
type FileRenderer interface {
	RenderedFiles(cfg config.Cluster, rt Runtime) []RenderedFile
}

// Forcer is implemented by steps that must run even when the journal and the
// input hash say they are current, for example to write rotated secrets that
// are never part of the inputs. Force returns the reason, or "" when the
// step can skip as usual.
type Forcer interface {
	Force(cfg config.Cluster, rt Runtime) string
}

// Skipper is implemented by steps that only apply to some configurations,
// such as pgbackrest-v1 without [backup]. A non-empty SkipReason skips the
// step without a journal entry, so it runs as soon as the configuration
// needs it.
type Skipper interface {
	SkipReason(cfg config.Cluster) string
}

// RestartBarrier is implemented by steps that need a pending PostgreSQL
// restart done before they run, such as pgbackrest-v1, which needs
// archive_mode on. The runner restarts first when the restart is confirmed;
// otherwise it leaves the step for a later apply.
type RestartBarrier interface {
	NeedsRestartFirst() bool
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
	// Warnings are non-fatal notices a step surfaces to the caller.
	Warnings []output.Warning
}

// Runtime is what a step may use while it runs.
type Runtime struct {
	DryRun  bool
	Shell   Shell
	Secrets *secrets.Registry
	// SecretValues are the request's secrets by name, for steps that need a
	// raw value (for example the admin sudo password). Every value is also
	// registered with Secrets.
	SecretValues map[string]string
	Log          *RunLog
	// Host is the node's hardware description, filled once per request.
	Host hostinfo.Info
	// PG talks to the local PostgreSQL as the postgres OS user.
	PG pg.Local
	// RotateSecrets asks steps to replace runtime secrets already on disk with
	// the ones in the request. Without it a step writes a runtime secret only
	// when the file is missing, so an operator with stale credentials can't
	// overwrite newer ones. eru backup setup sets it.
	RotateSecrets bool
}

// Restarter performs the aggregated restart at the end of an apply and reports
// how long the database was unavailable.
type Restarter interface {
	Restart(ctx context.Context) (time.Duration, error)
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
