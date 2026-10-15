package steps

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
)

const masterBackupID = "master-backup-v1"

// masterBackupStep (step 15, Phase 5) prepares the node for the master
// bundle: the directory for local bundles, the recovery recipient the
// bundles are encrypted to (with or without a bucket), and this install's
// instance id for the epoch fence. timers-v1 renders the units that run the
// job. A restore places the instance id and the local epoch before this step
// runs, and it keeps them.
type masterBackupStep struct{}

func (masterBackupStep) ID() string { return masterBackupID }

func (masterBackupStep) Describe(config.Cluster) string {
	return "prepare the master backup: its directory, the recovery recipient and the instance id"
}

func (masterBackupStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return canonical(map[string]any{"dir": masterbackup.Dir, "recipient": masterbackup.RecipientPath, "instance": masterbackup.InstancePath})
}

// ReadRecipient returns the recovery recipient on the node, "" when missing.
func ReadRecipient() string {
	raw, err := os.ReadFile(masterbackup.RecipientPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

func dirIs(path string, perm os.FileMode) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && info.Mode().Perm() == perm
}

func (masterBackupStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	switch {
	case !dirIs(masterbackup.Dir, 0o700):
		return incomplete(masterbackup.Dir + " is missing"), nil
	case ReadRecipient() == "":
		return incomplete(masterbackup.RecipientPath + " is missing"), nil
	}
	if id, err := masterbackup.ReadInstance(""); err != nil || id == "" {
		return incomplete(masterbackup.InstancePath + " is missing"), nil
	}
	return complete(), nil
}

// Recheck: the recipient and the instance id are state on the host, and a
// missing one must come back on the next apply.
func (masterBackupStep) Recheck() bool { return true }

func (masterBackupStep) Apply(_ context.Context, _ config.Cluster, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	var notes []string
	if err := os.MkdirAll(masterbackup.Dir, 0o700); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := os.Chmod(masterbackup.Dir, 0o700); err != nil { //nolint:gosec // G302: a directory root must enter.
		return engine.ApplyResult{}, err
	}
	if ReadRecipient() == "" {
		recipient := strings.TrimSpace(rt.SecretValues[SecretBackupAgeRecipient])
		if recipient == "" {
			// Fall back to the copy pgbackrest-v1 keeps with a bucket.
			recipient = ReadBackupEnv()[envRecipient]
		}
		if recipient == "" {
			return engine.ApplyResult{}, output.NewError(output.CodeInputRequired,
				"the recovery recipient is not on this node and the request has none", output.ExitInput,
				"Run eru apply from a context whose recovery kit has the age identity (eru login from a current secrets document).").
				WithDetails(map[string]any{"secret": SecretBackupAgeRecipient, "path": masterbackup.RecipientPath})
		}
		if err := os.MkdirAll(RuntimeDir, 0o700); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeFile(masterbackup.RecipientPath, recipient+"\n", 0o600); err != nil {
			return engine.ApplyResult{}, err
		}
		notes = append(notes, "wrote the recovery recipient")
	}
	id, err := masterbackup.ReadInstance("")
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return engine.ApplyResult{}, err
	}
	if id == "" {
		if id, err = masterbackup.NewInstanceID(); err != nil {
			return engine.ApplyResult{}, err
		}
		if err := writeFile(masterbackup.InstancePath, id+"\n", 0o600); err != nil {
			return engine.ApplyResult{}, err
		}
		notes = append(notes, "made the instance id "+id)
	}
	if len(notes) > 0 {
		// The first bundle follows at once instead of at the daily run.
		masterbackup.MarkDirty(masterBackupID)
	}
	return engine.ApplyResult{Restart: engine.RestartNone, Notes: notes}, nil
}
