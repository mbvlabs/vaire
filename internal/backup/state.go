package backup

import (
	"encoding/json"
	"os"
	"sort"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
)

// RecordsPath holds the node's durable records, beside state.json (which
// keeps only jobs and the lockdown flags). The master backup carries it.
const RecordsPath = "/var/lib/eru/records.json"

// recordsLockPath serializes read-modify-write cycles of records.json.
const recordsLockPath = RecordsPath + ".lock"

// maxDrills is how many drill results records.json keeps.
const maxDrills = 12

// Records is /var/lib/eru/records.json: open restores per database, restore
// leftovers, drills and the last backup, verify and dump results. Timeline
// switches and dropped stanzas join them with per-cluster restores and
// drops.
type Records struct {
	// Restores are the open restores, by database: one per database at a
	// time. A record stays until promote or abort.
	Restores map[string]Restore `json:"restores,omitempty"`
	// Leftovers are the databases a promote set aside, kept until cleanup.
	Leftovers []Leftover `json:"restore_leftovers,omitempty"`
	// Drills are the last restore drills, newest last.
	Drills []Drill `json:"drills,omitempty"`
	// LastBackup is the last successful backup per type.
	LastBackup map[string]BackupResult `json:"last_backup,omitempty"`
	// LastVerify is the last verify run.
	LastVerify *VerifyResult `json:"last_verify,omitempty"`
	// LastDump is the last dump per database.
	LastDump map[string]Dump `json:"last_dump,omitempty"`
	// DroppedStanzas are the stanzas of dropped databases. db drop keeps a
	// database's stanza, so its backups and WAL can still be restored, and
	// records it here; dropped-stanza expiry (unit 25) removes it.
	DroppedStanzas []DroppedStanza `json:"dropped_stanzas,omitempty"`
}

// DroppedStanza is one dropped database's kept stanza.
type DroppedStanza struct {
	Database  string `json:"database"`
	Stanza    string `json:"stanza"`
	DroppedAt string `json:"dropped_at"`
	// FinalDump is the id of the final dump db drop took, if any.
	FinalDump string `json:"final_dump,omitempty"`
}

// Restore phases.
const (
	PhaseRestoring = "restoring"
	PhaseReady     = "ready"
	PhaseFailed    = "failed"
)

// Restore is one open restore of a database.
type Restore struct {
	ID       string `json:"id"`
	Database string `json:"database"`
	// Target describes the recovery target: latest, a time, a set label or
	// a dump id.
	Target    string   `json:"target"`
	Kind      string   `json:"kind"`
	Phase     string   `json:"phase"`
	StartedAt string   `json:"started_at"`
	ReadyAt   string   `json:"ready_at,omitempty"`
	Error     string   `json:"error,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
	// Promote is set when the restore was started with --promote.
	Promote bool `json:"promote,omitempty"`
}

// Leftover is a database a promote put aside: the one-command undo, until
// cleanup drops it.
type Leftover struct {
	Database string `json:"database"`
	// Name is the <db>__pre_restore_<ts> database.
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	RestoreID string `json:"restore_id"`
}

// Drill is one restore drill.
type Drill struct {
	ID          string       `json:"id"`
	StartedAt   string       `json:"started_at"`
	FinishedAt  string       `json:"finished_at"`
	BackupLabel string       `json:"backup_label,omitempty"`
	Target      string       `json:"target"`
	RecoveredTo string       `json:"recovered_to,omitempty"`
	Checks      []DrillCheck `json:"checks"`
	Result      string       `json:"result"`
	Reason      string       `json:"reason,omitempty"`
}

// DrillCheck is one check of a drill.
type DrillCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// Drill results.
const (
	DrillPass    = "pass"
	DrillFail    = "fail"
	DrillSkipped = "skipped"
)

// BackupResult is the last successful backup of one type.
type BackupResult struct {
	Label      string `json:"label"`
	FinishedAt string `json:"finished_at"`
	Size       int64  `json:"size"`
}

// LoadRecords reads records.json. A missing file is empty records; a file
// that does not decode returns precondition_failed and is never reset.
func LoadRecords() (Records, error) {
	raw, err := os.ReadFile(RecordsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return decode(nil)
		}
		return Records{}, err
	}
	return decode(raw)
}

func decode(raw []byte) (Records, error) {
	var s Records
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return Records{}, output.NewError(
				output.CodePrecondition,
				"the node records file is corrupt",
				output.ExitPrecondition,
				"Inspect "+RecordsPath+" and fix or remove it. eru never resets it on its own.",
			).WithDetails(map[string]any{"reason": "records_corrupt", "path": RecordsPath})
		}
	}
	if s.Restores == nil {
		s.Restores = map[string]Restore{}
	}
	if s.LastBackup == nil {
		s.LastBackup = map[string]BackupResult{}
	}
	if s.LastDump == nil {
		s.LastDump = map[string]Dump{}
	}
	return s, nil
}

// UpdateRecords applies fn to records.json under its lock. fn returning an
// error leaves the file unchanged.
func UpdateRecords(fn func(*Records) error) error {
	return engine.WithFileLock(recordsLockPath, func() error {
		s, err := LoadRecords()
		if err != nil {
			return err
		}
		if err := fn(&s); err != nil {
			return err
		}
		if len(s.Drills) > maxDrills {
			s.Drills = append([]Drill(nil), s.Drills[len(s.Drills)-maxDrills:]...)
		}
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		return engine.WriteFileAtomic(RecordsPath, append(raw, '\n'), 0o600)
	})
}

// OpenRestore returns the open restore of db, if any.
func (s Records) OpenRestore(db string) (Restore, bool) {
	r, ok := s.Restores[db]
	return r, ok
}

// LeftoversOf returns the leftovers of db, or all of them when db is empty.
func (s Records) LeftoversOf(db string) []Leftover {
	var out []Leftover
	for _, l := range s.Leftovers {
		if db == "" || l.Database == db {
			out = append(out, l)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// LastDrill returns the newest drill.
func (s Records) LastDrill() (Drill, bool) {
	if len(s.Drills) == 0 {
		return Drill{}, false
	}
	return s.Drills[len(s.Drills)-1], true
}
