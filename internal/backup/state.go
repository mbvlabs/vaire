package backup

import (
	"encoding/json"
	"os"
	"sort"
	"time"

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
// leftovers, drills, the dropped stanzas and the last backup, verify and dump
// results per database, and the timelines restore undo went back to.
type Records struct {
	// Restores are the open restores, by database: one per database at a
	// time. A record stays until promote or abort.
	Restores map[string]Restore `json:"restores,omitempty"`
	// Leftovers are the databases a promote set aside, kept until cleanup.
	Leftovers []Leftover `json:"restore_leftovers,omitempty"`
	// Drills are the last restore drills, newest last.
	Drills []Drill `json:"drills,omitempty"`
	// LastBackup is the last successful backup per database, then per type.
	LastBackup map[string]map[string]BackupResult `json:"last_backups,omitempty"`
	// LastVerify is the last verify run of each database's stanza.
	LastVerify map[string]VerifyResult `json:"last_verifies,omitempty"`
	// LastDump is the last dump per database.
	LastDump map[string]Dump `json:"last_dump,omitempty"`
	// DroppedStanzas are the stanzas of dropped databases. db drop keeps a
	// database's stanza, so its backups and WAL can still be restored, and
	// records it here; the stanza-expiry job deletes the stanza and the
	// record once ExpiresAt has passed.
	DroppedStanzas []DroppedStanza `json:"dropped_stanzas,omitempty"`
	// Timelines are the timelines restore undo went back to, by database:
	// the restored branch it left is newer, so pgBackRest's latest would
	// follow it, and later restores of the stanza pass this timeline
	// instead. A promote clears the database's entry.
	Timelines map[string]TimelineSwitch `json:"timelines,omitempty"`
}

// TimelineSwitch is the timeline a database is on after restore undo.
type TimelineSwitch struct {
	Stanza     string `json:"stanza"`
	Timeline   string `json:"timeline"`
	SwitchedAt string `json:"switched_at"`
	// Abandoned is the timeline of the restored branch undo left.
	Abandoned string `json:"abandoned,omitempty"`
}

// TargetTimeline is the timeline a restore of db from stanza follows: the
// one recorded by restore undo, or empty for pgBackRest's latest.
func (s Records) TargetTimeline(db, stanza string) string {
	if t, ok := s.Timelines[db]; ok && t.Stanza == stanza {
		return t.Timeline
	}
	return ""
}

// DroppedStanza is one dropped database's kept stanza.
type DroppedStanza struct {
	Database  string `json:"database"`
	Stanza    string `json:"stanza"`
	DroppedAt string `json:"dropped_at"`
	// ExpiresAt is DroppedAt plus the database's restore window
	// (retention_full) at the drop.
	ExpiresAt string `json:"expires_at"`
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
	// Port is the temporary cluster's port once it is materialised; the
	// session pooler routes <db>__restoring to it.
	Port int `json:"port,omitempty"`
	// Timeline is the --target-timeline the restore passed, if any.
	Timeline string `json:"timeline,omitempty"`
}

// Leftover is a cluster a promote or undo put aside, stopped: the
// one-command undo, until cleanup drops it.
type Leftover struct {
	Database string `json:"database"`
	// Name is the <db>__pre_restore_<ts> cluster.
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	RestoreID string `json:"restore_id"`
	// Port is the leftover cluster's registered port, from the temporary
	// range.
	Port int `json:"port,omitempty"`
	// Stanza is the stanza the leftover's data belongs to. It differs from
	// the database's own after a restore from a dump, which starts a new
	// stanza; undo then takes this one back.
	Stanza string `json:"stanza,omitempty"`
}

// IsTemporary reports whether cluster is one restore owns: an open
// restore's <db>__restoring or a leftover.
func (s Records) IsTemporary(cluster string) bool {
	for db := range s.Restores {
		if RestoringName(db) == cluster {
			return true
		}
	}
	for _, l := range s.Leftovers {
		if l.Name == cluster {
			return true
		}
	}
	return false
}

// Drill is one restore drill.
type Drill struct {
	ID          string       `json:"id"`
	Database    string       `json:"database,omitempty"`
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
		s.LastBackup = map[string]map[string]BackupResult{}
	}
	if s.LastVerify == nil {
		s.LastVerify = map[string]VerifyResult{}
	}
	if s.LastDump == nil {
		s.LastDump = map[string]Dump{}
	}
	if s.Timelines == nil {
		s.Timelines = map[string]TimelineSwitch{}
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

// LastDrill returns the newest drill of db.
func (s Records) LastDrill(db string) (Drill, bool) {
	for i := len(s.Drills) - 1; i >= 0; i-- {
		if s.Drills[i].Database == db {
			return s.Drills[i], true
		}
	}
	return Drill{}, false
}

// RecordBackup stores set as db's last backup of its type.
func (s *Records) RecordBackup(db string, set Set, finishedAt string) {
	if s.LastBackup[db] == nil {
		s.LastBackup[db] = map[string]BackupResult{}
	}
	s.LastBackup[db][set.Type] = BackupResult{Label: set.Label, FinishedAt: finishedAt, Size: set.Size}
}

// Dropped returns the dropped stanza of db, if any. A name dropped twice has
// two; the newest wins.
func (s Records) Dropped(db string) (DroppedStanza, bool) {
	for i := len(s.DroppedStanzas) - 1; i >= 0; i-- {
		if s.DroppedStanzas[i].Database == db {
			return s.DroppedStanzas[i], true
		}
	}
	return DroppedStanza{}, false
}

// Expired returns the dropped stanzas whose window has passed at now. A
// record without a parseable ExpiresAt never expires on its own.
func (s Records) Expired(now time.Time) []DroppedStanza {
	var out []DroppedStanza
	for _, d := range s.DroppedStanzas {
		if t, err := time.Parse(time.RFC3339, d.ExpiresAt); err == nil && !now.Before(t) {
			out = append(out, d)
		}
	}
	return out
}
