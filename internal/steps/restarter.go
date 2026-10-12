package steps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/mbvlabs/eru/internal/engine"
)

// pgReadyTimeout bounds the wait for PostgreSQL to accept connections.
const pgReadyTimeout = 60 * time.Second

// postgresRestarter performs the aggregated restart: stop PgBouncer so clients
// do not hit a restarting server, restart PostgreSQL, wait for readiness, then
// start PgBouncer again.
type postgresRestarter struct{}

func (postgresRestarter) Restart(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	// PgBouncer first, so no client connects to a server mid-restart.
	_ = runCmd(ctx, "systemctl", "stop", PgbouncerService)
	if err := runCmd(ctx, "pg_ctlcluster", "18", PGCluster, "restart", "-m", "fast"); err != nil {
		_ = runCmd(ctx, "systemctl", "start", PgbouncerService)
		return time.Since(start), fmt.Errorf("restart postgresql: %w", err)
	}
	if err := waitReady(ctx); err != nil {
		_ = runCmd(ctx, "systemctl", "start", PgbouncerService)
		return time.Since(start), err
	}
	if err := runCmd(ctx, "systemctl", "start", PgbouncerService); err != nil {
		return time.Since(start), fmt.Errorf("start pgbouncer: %w", err)
	}
	return time.Since(start), nil
}

// waitReady polls pg_isready until the cluster answers or the timeout passes.
func waitReady(ctx context.Context) error {
	deadline := time.Now().Add(pgReadyTimeout)
	for {
		if err := runCmd(ctx, "pg_isready", "-q", "-h", "/var/run/postgresql"); err == nil {
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

// restarter is the engine.Restarter the catalogue hands the runner.
var restarter engine.Restarter = postgresRestarter{}
