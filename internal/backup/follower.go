package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

// Followers (Phase 6) are materialised like a fork, but stay standbys: from
// the source's stanza with Restore (Pause, TargetLatest: pgBackRest's
// --type=standby), or from the live source with StandbyBaseBackup. Either
// way the copy is started once as a temporary cluster until it is open
// (consistent), then Adopt makes it the follower's own cluster.

// StandbyBaseBackup copies the live source with pg_basebackup over its
// socket, streaming its WAL through the replication slot slot (which the
// caller created with its WAL reserved), and leaves standby.signal in the
// copy, so it starts as a standby of the source instead of a primary of its
// own. It registers the cluster and starts it until it is open: it has
// replayed the WAL the backup streamed and is consistent.
func (m *Materialised) StandbyBaseBackup(ctx context.Context, slot string) error {
	if err := m.makeData(); err != nil {
		return err
	}
	m.log("copying the live " + m.Source + " into " + m.Data + " through the slot " + slot)
	if err := m.Shell.Exec(ctx, m.StepID, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", pgBin + "/pg_basebackup", "-h", pgSocketDir, "-p", strconv.Itoa(m.SourcePort), "-U", "postgres",
		"-D", m.Data, "-X", "stream", "-S", slot, "-c", "fast", "--no-password",
	}}, m.Report); err != nil {
		return wrapTool("pg_basebackup of "+m.Source, err)
	}
	if err := engine.WriteFileAtomicOwned(filepath.Join(m.Data, "standby.signal"), nil, 0o600, "postgres", "postgres"); err != nil {
		return err
	}
	if err := m.register(); err != nil {
		return err
	}
	return m.start(ctx, UntilOpen)
}

// DropRecoverySettings removes pgBackRest's recovery settings
// (restore_command, recovery_target*) from the data directory's
// postgresql.auto.conf: a follower's live in its 60-vaire-follower.conf, which
// vaire renders from cluster.toml, and postgresql.auto.conf would override
// them.
func DropRecoverySettings(data string) error {
	_, err := dropRecoverySettings(data)
	return err
}

// RestartCluster restarts live cluster name through its unit and waits
// until it is open: db unfollow restarts the promoted follower without its
// hot-standby floors and replication settings (postmaster ones among them).
func RestartCluster(ctx context.Context, sh engine.Shell, stepID string, report engine.Reporter, name, data string, port int) error {
	if err := sh.Exec(ctx, stepID, engine.Command{Name: "systemctl", Args: []string{"restart", "postgresql@18-" + name}}, report); err != nil {
		return output.WrapError(output.CodeExternalCommand, fmt.Errorf("restart %s: %w", name, err), output.ExitExternal,
			"Read details.log_tail.").WithDetails(map[string]any{"log_tail": clusterLogTail(name, 20)})
	}
	return waitCluster(ctx, name, data, port, UntilOpen, func(line string) {
		if report != nil {
			report(output.Event{Type: "log", Step: stepID, Line: line})
		}
	})
}
