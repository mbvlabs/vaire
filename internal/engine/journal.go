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

// Journal records what ran on this node. Steps is keyed by step ID, and a
// cluster's instance by <id>@<cluster>.
type Journal struct {
	Schema     int                     `json:"schema"`
	EruVersion string                  `json:"eru_version"`
	UpdatedAt  string                  `json:"updated_at"`
	Steps      map[string]JournalEntry `json:"steps"`
	// RestartPending lists steps whose restart has not been performed yet. It
	// survives runs, so a later apply with the confirm token performs it.
	RestartPending []string `json:"restart_pending,omitempty"`
}

// update applies fn to j and to the journal on disk, under the journal's own
// file lock. Applies limited to different clusters run side by side under
// the shared node lock, so each change is merged into the latest file
// instead of overwriting it with this run's copy.
func (j *Journal) update(path string, fn func(*Journal)) error {
	fn(j)
	return WithFileLock(path+".lock", func() error {
		disk, err := LoadJournal(path)
		if err != nil {
			return err
		}
		fn(disk)
		return disk.Save(path)
	})
}

// setEntry records one step's entry.
func (j *Journal) setEntry(path, id string, entry JournalEntry) error {
	return j.update(path, func(j *Journal) { j.Steps[id] = entry })
}

// clearPending drops ids from the pending restarts.
func (j *Journal) clearPending(path string, ids []string) error {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	return j.update(path, func(j *Journal) {
		kept := j.RestartPending[:0:0]
		for _, id := range j.RestartPending {
			if !drop[id] {
				kept = append(kept, id)
			}
		}
		j.RestartPending = kept
	})
}

// ForgetCluster removes cluster's step instances (<id>@<cluster>) and their
// pending restarts from the journal at path, so a cluster created again
// under the same name runs every instance fresh instead of skipping it on a
// hash its predecessor left. db drop and a failed db create call it.
func ForgetCluster(path, cluster string) error {
	return forget(path, func(id string) bool {
		_, c := SplitID(id)
		return c == cluster
	})
}

// ForgetSteps removes the entries and pending restarts of the steps ids from
// the journal at path, so the next run applies them again.
func ForgetSteps(path string, ids ...string) error {
	drop := map[string]bool{}
	for _, id := range ids {
		drop[id] = true
	}
	return forget(path, func(id string) bool { return drop[id] })
}

// forget removes every entry and pending restart whose ID matches, under the
// journal's file lock. An unchanged journal is not rewritten.
func forget(path string, match func(id string) bool) error {
	return WithFileLock(path+".lock", func() error {
		j, err := LoadJournal(path)
		if err != nil {
			return err
		}
		changed := false
		for id := range j.Steps {
			if match(id) {
				delete(j.Steps, id)
				changed = true
			}
		}
		kept := j.RestartPending[:0:0]
		for _, id := range j.RestartPending {
			if match(id) {
				changed = true
				continue
			}
			kept = append(kept, id)
		}
		if !changed {
			return nil
		}
		j.RestartPending = kept
		return j.Save(path)
	})
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
	return WriteFileAtomic(path, append(raw, '\n'), 0o600)
}

func (j *Journal) addPending(id string) {
	for _, existing := range j.RestartPending {
		if existing == id {
			return
		}
	}
	j.RestartPending = append(j.RestartPending, id)
}
