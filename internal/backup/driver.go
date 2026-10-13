// Package backup is eru's database backup layer (Phase 3): the Driver seam
// with its pgBackRest implementation, scratch restores, the per-database swap,
// age-encrypted long-retention dumps, the durable records in records.json, the
// eru.prom backup lines and the free-space checks.
//
// Each database is its own PostgreSQL cluster with its own WAL stream, so
// pgBackRest keeps one stanza per database in one repository: a driver is
// bound to one stanza and its cluster's port.
package backup

import (
	"context"
	"time"
)

// Driver is the seam between eru and the physical backup tool. pgBackRest is
// the only implementation; WAL-G would be another.
type Driver interface {
	// StanzaCreate creates the stanza in the repository, or checks an
	// existing one against this cluster's system identifier.
	StanzaCreate(ctx context.Context) error
	// Check proves the repository is writable and WAL archiving works.
	Check(ctx context.Context) error
	// Backup takes one backup of type full, diff or incr.
	Backup(ctx context.Context, kind string) (Set, error)
	// Info lists the backup sets. With label it describes that set only,
	// including the databases it contains.
	Info(ctx context.Context, label string) (Info, error)
	// Verify checks every file of the stanza against its checksum.
	Verify(ctx context.Context) (VerifyResult, error)
	// StanzaDelete removes the stanza and everything in it from the
	// repository. pgPath is the dropped cluster's data directory, which
	// pgBackRest wants named even though it no longer exists.
	StanzaDelete(ctx context.Context, pgPath string) error
	// RestoreScratch restores into an empty directory that is not the live
	// data directory, with WAL archiving off.
	RestoreScratch(ctx context.Context, opts RestoreOptions) error
	// ArchiveStatus reads pg_stat_archiver on the live cluster.
	ArchiveStatus(ctx context.Context) (ArchiveStatus, error)
}

// Backup types.
const (
	TypeFull = "full"
	TypeDiff = "diff"
	TypeIncr = "incr"
)

// Set is one backup set in the repository.
type Set struct {
	Label     string   `json:"label"`
	Type      string   `json:"type"`
	Prior     string   `json:"prior,omitempty"`
	Start     string   `json:"start"`
	Stop      string   `json:"stop"`
	Size      int64    `json:"size"`
	Delta     int64    `json:"delta"`
	RepoSize  int64    `json:"repo_size"`
	RepoDelta int64    `json:"repo_delta"`
	WALStart  string   `json:"wal_start,omitempty"`
	WALStop   string   `json:"wal_stop,omitempty"`
	Error     bool     `json:"error,omitempty"`
	Databases []string `json:"databases,omitempty"`
}

// StopTime parses Stop.
func (s Set) StopTime() time.Time {
	t, _ := time.Parse(time.RFC3339, s.Stop)
	return t
}

// Info is the repository as pgBackRest describes it.
type Info struct {
	Stanza         string `json:"stanza"`
	StatusCode     int    `json:"status_code"`
	Status         string `json:"status"`
	Sets           []Set  `json:"sets"`
	ArchiveMin     string `json:"archive_min,omitempty"`
	ArchiveMax     string `json:"archive_max,omitempty"`
	BackupLockHeld bool   `json:"backup_lock_held"`
}

// HasFull reports whether the repository holds a full backup.
func (i Info) HasFull() bool {
	for _, s := range i.Sets {
		if s.Type == TypeFull && !s.Error {
			return true
		}
	}
	return false
}

// Oldest returns the oldest complete set.
func (i Info) Oldest() (Set, bool) {
	for _, s := range i.Sets {
		if !s.Error {
			return s, true
		}
	}
	return Set{}, false
}

// Latest returns the newest complete set.
func (i Info) Latest() (Set, bool) {
	for n := len(i.Sets) - 1; n >= 0; n-- {
		if !i.Sets[n].Error {
			return i.Sets[n], true
		}
	}
	return Set{}, false
}

// Find returns the set with label.
func (i Info) Find(label string) (Set, bool) {
	for _, s := range i.Sets {
		if s.Label == label {
			return s, true
		}
	}
	return Set{}, false
}

// VerifyResult is one pgbackrest verify run.
type VerifyResult struct {
	OK         bool     `json:"ok"`
	StartedAt  string   `json:"started_at"`
	FinishedAt string   `json:"finished_at"`
	Output     []string `json:"output,omitempty"`
}

// ArchiveStatus is pg_stat_archiver plus the derived lag.
type ArchiveStatus struct {
	ArchivedCount    int64  `json:"archived_count"`
	LastArchivedWAL  string `json:"last_archived_wal,omitempty"`
	LastArchivedTime string `json:"last_archived_time,omitempty"`
	FailedCount      int64  `json:"failed_count"`
	LastFailedWAL    string `json:"last_failed_wal,omitempty"`
	LastFailedTime   string `json:"last_failed_time,omitempty"`
	// LagSeconds is how long ago the last segment was archived.
	LagSeconds int64 `json:"lag_seconds"`
}

// Restore target types.
const (
	TargetLatest = "latest"
	TargetTime   = "time"
	TargetSet    = "set"
)

// RestoreOptions describe one scratch restore.
type RestoreOptions struct {
	// Path is the empty data directory to restore into.
	Path string
	// Database limits the restore to one database; the others come back as
	// zeroed sparse files. Empty restores everything (a drill).
	Database string
	// Kind is TargetLatest, TargetTime or TargetSet.
	Kind string
	// Time is the recovery target for TargetTime.
	Time time.Time
	// Set is the backup label for TargetSet.
	Set string
}
