package steps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

// pgReadyTimeout bounds the wait for PostgreSQL to accept connections.
const pgReadyTimeout = 60 * time.Second

// postgresRestarter restarts one cluster and waits until it accepts
// connections on its socket. The poolers keep running: every other database
// keeps serving while this one restarts (RFC 7.1). The transaction pooler
// pauses the database around the restart, so its clients wait instead of
// failing. The session pooler isn't paused: in session mode PAUSE waits for
// every client to disconnect.
type postgresRestarter struct {
	cfg config.Cluster
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
	if err := runCmd(ctx, "pg_ctlcluster", strconv.Itoa(PGVersion), cluster, "restart", "-m", "fast"); err != nil {
		return time.Since(start), fmt.Errorf("restart %s: %w", PGServiceName(cluster), err)
	}
	if err := waitReady(ctx, spec.Port); err != nil {
		return time.Since(start), err
	}
	return time.Since(start), nil
}

// waitReady polls pg_isready on the cluster's socket until it answers or the
// timeout passes.
func waitReady(ctx context.Context, port int) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for {
		if err := runCmd(ctx, "pg_isready", "-q", "-h", PGSocketDir, "-p", strconv.Itoa(port)); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("postgresql did not become ready within 60s")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func runCmd(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed restart commands.
	cmd.Env = probeEnv
	return cmd.Run()
}

// Restarter returns the cluster restarter the runner calls for each cluster
// with a confirmed pending restart.
func Restarter(cfg config.Cluster) engine.Restarter { return postgresRestarter{cfg: cfg} }
