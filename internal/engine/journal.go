package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/secrets"
)

// JournalPath is the node's step journal (§9.3).
const JournalPath = "/var/lib/eru/journal.json"

// JournalSchema is the journal format version.
const JournalSchema = 1

// Step states recorded in the journal.
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// Journal records what ran on this node.
type Journal struct {
	Schema     int                     `json:"schema"`
	EruVersion string                  `json:"eru_version"`
	UpdatedAt  string                  `json:"updated_at"`
	Steps      map[string]JournalEntry `json:"steps"`
	// RestartPending lists steps whose restart has not been performed yet. It
	// survives runs, so a later apply with the confirm token performs it.
	RestartPending []string `json:"restart_pending,omitempty"`
}

// JournalEntry is one step's last run.
type JournalEntry struct {
	Status      string `json:"status"`
	InputsHash  string `json:"inputs_hash"`
	StartedAt   string `json:"started_at,omitempty"`
	CompletedAt string `json:"completed_at,omitempty"`
	Restart     string `json:"restart,omitempty"`
	Error       string `json:"error,omitempty"`
}

// LoadJournal reads path. A missing journal is empty. A journal that does not
// decode returns precondition_failed with reason journal_corrupt; it is never
// reset silently.
func LoadJournal(path string) (*Journal, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed journal path.
	if err != nil {
		if os.IsNotExist(err) {
			return &Journal{Schema: JournalSchema, Steps: map[string]JournalEntry{}}, nil
		}
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(raw, &j); err != nil || j.Schema != JournalSchema {
		return nil, journalCorrupt(path)
	}
	if j.Steps == nil {
		j.Steps = map[string]JournalEntry{}
	}
	return &j, nil
}

func journalCorrupt(path string) *output.CLIError {
	return output.NewError(
		output.CodePrecondition,
		"the step journal is corrupt",
		output.ExitPrecondition,
		"Inspect "+path+" and restore it, or move it aside to re-run every step. eru never resets it on its own.",
	).WithDetails(map[string]any{"reason": "journal_corrupt", "path": path})
}

// Save writes the journal atomically with mode 0600. Error strings are
// redacted before they reach disk.
func (j *Journal) Save(path string) error {
	j.Schema = JournalSchema
	j.EruVersion = buildinfo.Version
	j.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	for id, entry := range j.Steps {
		entry.Error = secrets.Redact(entry.Error)
		j.Steps[id] = entry
	}
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(path, append(raw, '\n'), 0o600)
}
