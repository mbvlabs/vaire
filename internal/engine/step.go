package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/secrets"
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

// Rechecker is implemented by steps whose result can drift on the host with
// no input changing, such as a ufw rule added or deleted by hand. The runner
// runs Check even when the journal says the step is current, and applies the
// step when the host no longer matches, so "vaire apply" repairs what doctor
// reports. Check must be cheap and read-only: it also runs in every plan.
type Rechecker interface {
	Recheck() bool
}

// Skipper is implemented by steps that only apply to some configurations,
// such as pgbackrest-v1 without [backup]. A non-empty SkipReason skips the
// step without a journal entry, so it runs as soon as the configuration
// needs it.
type Skipper interface {
	SkipReason(cfg config.Cluster) string
}

// CheckResult says whether the host already matches the step.
type CheckResult struct {
	Complete bool
	Detail   string
}

// RestartNeed is what a step needs after Apply.
type RestartNeed string

// Restart needs. A reload is the step's own job; a restart is aggregated per
// cluster and performed at most once per cluster and run. A restart from a
// cluster's instance restarts that cluster; one from a server-level step
// restarts every cluster, one at a time.
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
	// RotateSecrets asks steps to replace runtime secrets already on disk with
	// the ones in the request. Without it a step writes a runtime secret only
	// when the file is missing, so an operator with stale credentials can't
	// overwrite newer ones. vaire backup setup sets it.
	RotateSecrets bool
}

// Restarter restarts one PostgreSQL cluster and reports how long it was
// unavailable. The runner calls it once per cluster with a pending restart,
// one cluster at a time.
type Restarter interface {
	Restart(ctx context.Context, cluster string) (time.Duration, error)
}

// RestartChecker is implemented by restarters that can ask a running cluster
// whether it still waits for a restart. Before restarting, the runner clears
// a pending restart the cluster no longer needs (a restart that worked but
// reported a failure, or a cluster restarted by hand), without restarting.
// An error keeps the restart pending.
type RestartChecker interface {
	NeedsRestart(ctx context.Context, cluster string) (bool, error)
}

// Template is implemented by a step that runs once per cluster (RFC 11.2).
// The catalogue holds the template; Plan and Apply expand it into one
// instance per [[clusters]] entry, journaled as <id>@<cluster> with its own
// input hash, so a change to one cluster re-runs nothing for another.
type Template interface {
	Step
	// Instance returns the step for one cluster. Its ID is
	// InstanceID(template ID, cluster).
	Instance(cluster string) Step
}

// IDPattern is the versioned step ID format, for example postgres-config-v1.
var IDPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$`)

// instanceSep separates a template's ID from the cluster in an instance ID.
const instanceSep = "@"

// InstanceID is the ID of template id's instance for cluster.
func InstanceID(id, cluster string) string { return id + instanceSep + cluster }

// SplitID splits an instance ID into the template ID and the cluster. A
// server-level step has no cluster.
func SplitID(id string) (template, cluster string) {
	template, cluster, _ = strings.Cut(id, instanceSep)
	return template, cluster
}

// Expand replaces every Template in steps with one instance per cluster in
// cfg, in [[clusters]] order, keeping catalogue order between steps.
func Expand(steps []Step, cfg config.Cluster) []Step {
	out := make([]Step, 0, len(steps))
	for _, step := range steps {
		t, ok := step.(Template)
		if !ok {
			out = append(out, step)
			continue
		}
		for _, c := range cfg.Clusters {
			out = append(out, t.Instance(c.Name))
		}
	}
	return out
}

// ValidateIDs checks every ID against IDPattern and refuses duplicates. It
// takes the catalogue, before Expand.
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
