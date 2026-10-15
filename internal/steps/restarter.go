package steps

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

// pgReadyTimeout bounds the wait for PostgreSQL to accept connections.
const pgReadyTimeout = 60 * time.Second

// cmdOutputTail is how much of a command's combined output an error keeps.
const cmdOutputTail = 2048

// postgresRestarter restarts one cluster and waits until it accepts
// connections on its socket. The poolers keep running: every other database
// keeps serving while this one restarts (RFC 7.1). The transaction pooler
// pauses the database around the restart, so its clients wait instead of
// failing. The session pooler isn't paused: in session mode PAUSE waits for
// every client to disconnect.
type postgresRestarter struct {
	cfg config.Cluster
}

// restartCommand is the restart of cluster: its systemd unit, which is what
// runs the cluster. The unit's ExecStop is pg_ctlcluster's -m fast stop.
// pg_ctlcluster restart would only redirect to the same systemctl call when
// it runs as root under systemd, and run pg_ctl itself (leaving the cluster
// outside its unit and its cgroup) otherwise.
func restartCommand(cluster string) (string, []string) {
	return "systemctl", []string{"restart", PGServiceName(cluster) + ".service"}
}

func (r postgresRestarter) Restart(ctx context.Context, cluster string) (time.Duration, error) {
	spec, ok := r.cfg.ClusterByName(cluster)
	if !ok {
		return 0, fmt.Errorf("cluster %s is not in cluster.toml", cluster)
	}
	start := time.Now()
	// A PAUSE that fails or times out (PgBouncer down, a long transaction)
	// doesn't stop the restart: -m fast ends the transaction anyway, and
	// RESUME below clears a pause still in progress.
	_ = poolerAdmin(ctx, PgbouncerPort, "PAUSE "+cluster)
	defer func() { _ = poolerAdmin(context.WithoutCancel(ctx), PgbouncerPort, "RESUME "+cluster) }()
	name, args := restartCommand(cluster)
	if err := runCmd(ctx, name, args...); err != nil {
		return time.Since(start), fmt.Errorf("restart %s: %w%s", PGServiceName(cluster), err, unitDiagnosis(ctx, PGServiceName(cluster)))
	}
	if err := waitReady(ctx, spec.Port); err != nil {
		return time.Since(start), fmt.Errorf("restart %s: %w%s", PGServiceName(cluster), err, unitDiagnosis(ctx, PGServiceName(cluster)))
	}
	// The restart is done when PostgreSQL runs with every setting it was
	// restarted for.
	pending, err := pendingRestartSettings(ctx, ClusterPG(spec))
	if err != nil {
		return time.Since(start), fmt.Errorf("restart %s: reading pending_restart: %w", PGServiceName(cluster), err)
	}
	if len(pending) > 0 {
		return time.Since(start), fmt.Errorf("restart %s: still pending a restart after it: %s", PGServiceName(cluster), strings.Join(pending, ", "))
	}
	return time.Since(start), nil
}

// unitDiagnosis is what systemd knows about a unit whose restart failed: its
// state and result, and its last journal lines, so the run log says why.
func unitDiagnosis(ctx context.Context, unit string) string {
	ctx = context.WithoutCancel(ctx)
	var parts []string
	if out, err := Probe(ctx, "systemctl", "show", "--property=ActiveState,SubState,Result", unit+".service"); err == nil && out != "" {
		parts = append(parts, "unit "+strings.Join(strings.Fields(out), " "))
	}
	if out, err := Probe(ctx, "journalctl", "--unit", unit+".service", "--lines=5", "--no-pager", "--output=cat"); err == nil && out != "" {
		parts = append(parts, "journal: "+strings.Join(strings.Split(out, "\n"), " | "))
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, "; ") + ")"
}

// waitReady polls pg_isready on the cluster's socket until it answers or the
// timeout passes.
func waitReady(ctx context.Context, port int) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for {
		err := runCmd(ctx, "pg_isready", "-q", "-h", PGSocketDir, "-p", strconv.Itoa(port))
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("postgresql did not become ready within 60s: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// runCmd runs a fixed command with the probe environment. A failure carries
// the end of the command's combined output.
func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed restart commands.
	cmd.Env = probeEnv
	out := &tailBuffer{max: cmdOutputTail}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Run(); err != nil {
		if tail := strings.TrimSpace(out.String()); tail != "" {
			return fmt.Errorf("%w: %s", err, strings.Join(strings.Fields(tail), " "))
		}
		return err
	}
	return nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
	cut bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.cut = true
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cut {
		return "…" + string(t.buf)
	}
	return string(t.buf)
}

// Restarter returns the cluster restarter the runner calls for each cluster
// with a confirmed pending restart.
func Restarter(cfg config.Cluster) engine.Restarter { return postgresRestarter{cfg: cfg} }

// PauseDatabase pauses db on the transaction pooler, so its clients wait
// through a restore's cut-over instead of failing, and returns the RESUME.
// A PAUSE that fails or times out doesn't stop the caller.
func PauseDatabase(ctx context.Context, db string) (resume func()) {
	_ = poolerAdmin(ctx, PgbouncerPort, "PAUSE "+db)
	return func() { _ = poolerAdmin(context.WithoutCancel(ctx), PgbouncerPort, "RESUME "+db) }
}

// NeedsRestart reports whether cluster still has a setting waiting for a
// restart (pg_settings.pending_restart). A cluster that doesn't answer
// returns an error, which keeps its restart pending.
func (r postgresRestarter) NeedsRestart(ctx context.Context, cluster string) (bool, error) {
	spec, ok := r.cfg.ClusterByName(cluster)
	if !ok {
		return false, fmt.Errorf("cluster %s is not in cluster.toml", cluster)
	}
	pending, err := pendingRestartSettings(ctx, ClusterPG(spec))
	if err != nil {
		return false, err
	}
	return len(pending) > 0, nil
}
