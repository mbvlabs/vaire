package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/steps"
)

// phase2Jobs are the timer jobs Phase 2 implements. The rest arrive later.
var phase2Jobs = map[string]bool{"certs-sync": true}

// jobNames lists every job eru node run accepts.
var jobNames = []string{
	"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill",
	"vps-backup", "maint-daily", "maint-weekly", "certs-sync", "metrics-refresh",
}

// RunJob runs one timer job. certs-sync takes the shared lock with a zero
// timeout; a busy lock exits 0 with a skipped result.
func RunJob(ctx context.Context, job string, report engine.Reporter) (any, error) {
	if !knownJob(job) {
		return nil, output.NewError(output.CodeUsage, "unknown job "+job, output.ExitUsage, "Use one of details.jobs.").
			WithDetails(map[string]any{"jobs": jobNames})
	}
	if !phase2Jobs[job] {
		return nil, output.NewError(output.CodeError, "job "+job+" is not implemented yet", output.ExitGeneric,
			"This job arrives in a later phase of the roadmap.")
	}
	release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), 0)
	if err != nil {
		if report != nil {
			report(output.Event{Type: "log", Step: job, Line: "the node lock is busy; skipping " + job})
		}
		return map[string]any{"job": job, "skipped": "lock_busy"}, nil
	}
	defer release()
	return certsSync(ctx, report)
}

func knownJob(job string) bool {
	for _, name := range jobNames {
		if name == job {
			return true
		}
	}
	return false
}

// certsSync copies Caddy's certificate for the domain into /etc/eru/tls and
// reloads PostgreSQL and PgBouncer when it changed (D11).
func certsSync(ctx context.Context, report engine.Reporter) (any, error) {
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return nil, err
	}
	certPath, keyPath, err := findCaddyCert(cfg.Domain, cfg.TLS.Mode == "self-signed")
	if err != nil {
		return nil, output.NewError(output.CodePrecondition, err.Error(), output.ExitPrecondition,
			"Caddy has not issued the certificate yet; certs-sync runs again on the next path change or timer.")
	}
	changed, err := copyIfDifferent(certPath, steps.TLSCertPath, 0o644)
	if err != nil {
		return nil, err
	}
	keyChanged, err := copyIfDifferent(keyPath, steps.TLSKeyPath, 0o640)
	if err != nil {
		return nil, err
	}
	if keyChanged {
		if err := runQuiet(ctx, "chown", "root:ssl-cert", steps.TLSKeyPath); err != nil {
			return nil, fmt.Errorf("chown %s: %w", steps.TLSKeyPath, err)
		}
	}
	if !changed && !keyChanged {
		return map[string]any{"changed": false}, nil
	}
	if report != nil {
		report(output.Event{Type: "log", Step: "certs-sync", Line: "installed the certificate for " + cfg.Domain})
	}
	_ = runQuiet(ctx, "pg_ctlcluster", "18", steps.PGCluster, "reload")
	_ = runQuiet(ctx, "systemctl", "reload", steps.PgbouncerService)
	return map[string]any{"changed": true, "not_after": certNotAfter(steps.TLSCertPath)}, nil
}

// findCaddyCert locates Caddy's certificate and key for domain. Self-signed
// mode uses the leaf from Caddy's internal CA (certificates/local/...); acme
// mode only takes a certificate from a public issuer, so a leftover internal
// leaf never passes for an ACME one.
func findCaddyCert(domain string, internal bool) (cert, key string, err error) {
	matches, _ := filepath.Glob(filepath.Join(steps.CaddyCertRoot, "*", domain, domain+".crt"))
	for _, m := range matches {
		issuer := filepath.Base(filepath.Dir(filepath.Dir(m)))
		if (issuer == "local") == internal {
			return m, strings.TrimSuffix(m, ".crt") + ".key", nil
		}
	}
	return "", "", fmt.Errorf("caddy has no certificate for %s yet", domain)
}

// copyIfDifferent copies src to dst when their sha256 differs.
func copyIfDifferent(src, dst string, mode os.FileMode) (bool, error) {
	want, err := hashFile(src)
	if err != nil {
		return false, err
	}
	if have, err := hashFile(dst); err == nil && have == want {
		return false, nil
	}
	raw, err := os.ReadFile(src) //nolint:gosec // G304: Caddy's certificate store.
	if err != nil {
		return false, err
	}
	if err := engine.WriteFileAtomic(dst, raw, mode); err != nil {
		return false, err
	}
	return true, nil
}

func hashFile(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed certificate paths.
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func runQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed reload commands.
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	return cmd.Run()
}
