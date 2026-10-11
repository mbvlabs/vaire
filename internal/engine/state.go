package engine

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// StatePath is the node's run state beside the journal (§6.2).
const StatePath = "/var/lib/eru/state.json"

// State is /var/lib/eru/state.json. Later phases add last_apply, restore and
// drills.
type State struct {
	SecretsConfirmedAt string `json:"secrets_confirmed_at,omitempty"`
	// LockdownPending is true between ssh.lockdown and ssh.confirm or
	// ssh-revert. The firewall step keeps public 22/tcp open while it is set.
	LockdownPending bool `json:"lockdown_pending"`
	// LockedDownAt is when public SSH was closed for good. Empty means public
	// 22/tcp is still part of the firewall rule set.
	LockedDownAt string `json:"locked_down_at,omitempty"`
}

// PublicSSHOpen reports whether the firewall rule set keeps public 22/tcp.
func (s State) PublicSSHOpen() bool {
	return s.LockedDownAt == "" || s.LockdownPending
}

// LoadState reads path. A missing file is an empty state. A file that does not
// decode returns precondition_failed; it is never reset silently.
func LoadState(path string) (State, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed state path.
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return State{}, output.NewError(
			output.CodePrecondition,
			"the node state file is corrupt",
			output.ExitPrecondition,
			"Inspect "+path+" and fix or remove it. eru never resets it on its own.",
		).WithDetails(map[string]any{"reason": "state_corrupt", "path": path})
	}
	return s, nil
}

// Save writes the state atomically with mode 0600.
func (s State) Save(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return WriteFileAtomic(path, append(raw, '\n'), 0o600)
}
