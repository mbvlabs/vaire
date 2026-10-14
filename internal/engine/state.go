package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// StatePath is the node's run state beside the journal (§6.2).
const StatePath = "/var/lib/eru/state.json"

// stateLockPath serializes read-modify-write cycles of state.json between
// processes, because backup jobs and RPCs update it concurrently.
const stateLockPath = StatePath + ".lock"

// maxJobs is how many job records state.json keeps.
const maxJobs = 50

// State is /var/lib/eru/state.json: the job records and the lockdown flags.
// Durable records (restores, drills, last results) live in records.json,
// which internal/backup owns.
type State struct {
	SecretsConfirmedAt string `json:"secrets_confirmed_at,omitempty"`
	// LockdownPending is true between ssh.lockdown and ssh.confirm or
	// ssh-revert. The firewall step keeps public 22/tcp open while it is set.
	LockdownPending bool `json:"lockdown_pending"`
	// LockedDownAt is when public SSH was closed for good. Empty means public
	// 22/tcp is still part of the firewall rule set.
	LockedDownAt string `json:"locked_down_at,omitempty"`
	// Jobs are the detachable jobs and timer runs, newest last.
	Jobs []JobRecord `json:"jobs,omitempty"`
}

// JobRecord is one run of eru node run: a detachable job started by an RPC
// (unit eru-job-<id>) or a timer run. PID is the eru node run process, so a
// record still running whose process was killed can be told from a live job.
type JobRecord struct {
	ID           string            `json:"id"`
	Job          string            `json:"job"`
	DB           string            `json:"db,omitempty"`
	Unit         string            `json:"unit,omitempty"`
	InvocationID string            `json:"invocation_id,omitempty"`
	PID          int               `json:"pid,omitempty"`
	State        string            `json:"state"`
	Detail       string            `json:"detail,omitempty"`
	CreatedAt    string            `json:"created_at"`
	StartedAt    string            `json:"started_at,omitempty"`
	FinishedAt   string            `json:"finished_at,omitempty"`
	Result       json.RawMessage   `json:"result,omitempty"`
	Error        *output.ErrorBody `json:"error,omitempty"`
}

// Job states. Queued is written by the RPC before the unit starts.
const (
	JobQueued    = "queued"
	JobRunning   = "running"
	JobSucceeded = "succeeded"
	JobFailed    = "failed"
	JobSkipped   = "skipped"
)

// Final reports whether the job has ended.
func (r JobRecord) Final() bool {
	return r.State == JobSucceeded || r.State == JobFailed || r.State == JobSkipped
}

// Job returns the record with id.
func (s State) Job(id string) (JobRecord, bool) {
	for _, r := range s.Jobs {
		if r.ID == id {
			return r, true
		}
	}
	return JobRecord{}, false
}

// PutJob adds or replaces a job record, keeping the newest maxJobs.
func (s *State) PutJob(rec JobRecord) {
	for i, r := range s.Jobs {
		if r.ID == rec.ID {
			s.Jobs[i] = rec
			return
		}
	}
	s.Jobs = append(s.Jobs, rec)
	if over := len(s.Jobs) - maxJobs; over > 0 {
		s.Jobs = append([]JobRecord(nil), s.Jobs[over:]...)
	}
}

// UpdateState loads state.json, applies fn and saves the result, holding an
// flock on state.json.lock so concurrent writers don't lose each other's
// changes. fn returning an error leaves the file unchanged.
func UpdateState(fn func(*State) error) error {
	return WithFileLock(stateLockPath, func() error {
		st, err := LoadState(StatePath)
		if err != nil {
			return err
		}
		if err := fn(&st); err != nil {
			return err
		}
		return st.Save(StatePath)
	})
}

// WithFileLock runs fn holding an exclusive flock on lockPath, so
// read-modify-write cycles of a node file don't lose each other's changes
// across processes.
func WithFileLock(lockPath string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: fixed node lock paths.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	if err := syscall.Flock(fd, syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(fd, syscall.LOCK_UN) }()
	return fn()
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
