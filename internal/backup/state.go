package backup

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

// RecordsPath holds the node's durable records, beside state.json (which
// keeps only jobs and the lockdown flags). The master backup carries it.
const RecordsPath = "/var/lib/vaire/records.json"

// RecordsMode is records.json's mode; root owns it.
const RecordsMode os.FileMode = 0o600

// recordsLockPath serializes read-modify-write cycles of records.json.
const recordsLockPath = RecordsPath + ".lock"

// maxDrills is how many drill results records.json keeps per database.
const maxDrills = 12

// Records is /var/lib/vaire/records.json: open restores per database, restore
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
	// Drilling are the running drills' temporary clusters, by database. A
	// drill that was killed leaves its entry, so the next drill of the
	// database knows the <db>__drill cluster is its own and drops it.
	Drilling map[string]string `json:"drilling,omitempty"`
	// LastBackup is the last successful backup per database, then per type.
	LastBackup map[string]map[string]BackupResult `json:"last_backups,omitempty"`
	// LastVerify is the last verify run of each database's stanza.
	LastVerify map[string]VerifyResult `json:"last_verifies,omitempty"`
	// LastFailure is when each database's last backup of a type failed,
	// cleared by the next success of that type (Phase 4).
	LastFailure map[string]map[string]string `json:"last_backup_failures,omitempty"`
	// Repo is each database's stanza facts from its last backup, verify or
	// expiry (Phase 4).
	Repo map[string]RepoFacts `json:"repo,omitempty"`
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
	// Undoing are the restore undos under way, by database, recorded before
	// the data directories are renamed: an undo that fails after its swap
	// resumes from the same names, and its aside cluster is never lost.
	Undoing map[string]Undo `json:"undoing,omitempty"`
	// AwaitingRecovery are the databases a rebuilt master hasn't recovered
	// yet (Phase 5): cluster.toml has them, the server has no cluster for
	// them. db recover (or db drop) clears an entry.
	AwaitingRecovery map[string]Awaiting `json:"awaiting_recovery,omitempty"`
}

// Awaiting is one database awaiting recovery after a rebuild.
type Awaiting struct {
	Stanza string `json:"stanza"`
	Since  string `json:"since"`
}

// IsAwaiting reports whether db awaits recovery.
func (s Records) IsAwaiting(db string) bool {
	_, ok := s.AwaitingRecovery[db]
	return ok
}

// durableHash covers the sections that exist nowhere else and must reach
// the master bundle at once: dropped and replaced stanzas, timeline
// switches, databases awaiting recovery. Backup facts change after every
// backup and ride along in the next bundle.
func (s Records) durableHash() string {
	raw, _ := json.Marshal(struct {
		Dropped   []DroppedStanza           `json:"d"`
		Timelines map[string]TimelineSwitch `json:"t"`
		Awaiting  map[string]Awaiting       `json:"a"`
	}{s.DroppedStanzas, s.Timelines, s.AwaitingRecovery})
	return masterbackup.SHA256(raw)
}

// Undo is one restore undo under way.
type Undo struct {
	// From is the leftover the undo swaps back in.
	From Leftover `json:"from"`
	// Aside is the <db>__pre_restore_<ts> cluster the restored data becomes.
	Aside string `json:"aside"`
	// Stanza is the restored data's stanza, cluster.toml's at the start.
	Stanza string `json:"stanza,omitempty"`
	// Abandoned is the restored data's timeline, read before the swap.
	Abandoned string `json:"abandoned,omitempty"`
	// LastArchivedAt is when the restored data last archived WAL, read
	// before the swap, for an undo that drops the dump's stanza.
	LastArchivedAt string `json:"last_archived_at,omitempty"`
	StartedAt      string `json:"started_at"`
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
	// LastArchivedAt is when the cluster last archived a WAL segment to
	// the stanza before the drop: the end of its restore window. pgbackrest
	// info names the newest segment, not its time.
	LastArchivedAt string `json:"last_archived_at,omitempty"`
	// Reason is why the stanza stopped being the database's: empty for db
	// drop (records from before the field mean that too), or
	// DroppedReasonReplaced when a restore from a dump, or its undo, gave
	// the live database a new stanza.
	Reason string `json:"reason,omitempty"`
}

// DroppedReasonReplaced marks a stanza kept after restore promote (or undo)
// switched its database to another stanza.
const DroppedReasonReplaced = "replaced"

// Replaced reports whether a restore replaced the stanza, rather than db
// drop dropping its database.
func (d DroppedStanza) Replaced() bool { return d.Reason == DroppedReasonReplaced }

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
	// Aside is the <db>__pre_restore_<ts> cluster promote puts the live
	// data in, recorded before it renames a data directory: a promote that
	// fails after the swap resumes with it, and abort refuses once the copy
	// is swapped in.
	Aside string `json:"aside,omitempty"`
	// AsideArchivedAt is when the live data last archived WAL, read before
	// the swap of a restore from a dump, whose old stanza is dropped.
	AsideArchivedAt string `json:"aside_archived_at,omitempty"`
	// Recover marks a db recover after a rebuild (Phase 5): the live
	// cluster promote sets aside is an empty scaffold, dropped at once.
	Recover bool `json:"recover,omitempty"`
	// NoFull skips the full backup promote starts (db recover --all takes
	// the fulls once every database is back).
	NoFull bool `json:"no_full,omitempty"`
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

// IsTemporary reports whether cluster is one restore or drill owns: an
// open restore's <db>__restoring, the aside of a promote or undo under way,
// a leftover or a drill's <db>__drill.
func (s Records) IsTemporary(cluster string) bool {
	for _, name := range s.Drilling {
		if name == cluster {
			return true
		}
	}
	for db, r := range s.Restores {
		if RestoringName(db) == cluster || (r.Aside != "" && r.Aside == cluster) {
			return true
		}
	}
	for _, u := range s.Undoing {
		if u.Aside == cluster {
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
	ID       string `json:"id"`
	Database string `json:"database,omitempty"`
	// Stanza is the stanza the drill restored from; see VerifyResult.Stanza.
	Stanza      string `json:"stanza,omitempty"`
	StartedAt   string `json:"started_at"`
	FinishedAt  string `json:"finished_at"`
	BackupLabel string `json:"backup_label,omitempty"`
	Target      string `json:"target"`
	RecoveredTo string `json:"recovered_to,omitempty"`
	// NoWALReplayed is set when recovery replayed no transaction after the
	// backup: RecoveredTo is then the backup set's stop time.
	NoWALReplayed bool         `json:"no_wal_replayed,omitempty"`
	Checks        []DrillCheck `json:"checks"`
	Result        string       `json:"result"`
	Reason        string       `json:"reason,omitempty"`
}

// DescribeRecoveredTo is RecoveredTo for people: the replayed commit time,
// or the backup's end when no transaction was replayed after it.
func (d Drill) DescribeRecoveredTo() string {
	if d.NoWALReplayed {
		return d.RecoveredTo + " (the backup's end; no transaction replayed after it)"
	}
	return d.RecoveredTo
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
	// RepoSize is what the set takes in the repository (its delta for
	// bundled sets), Delta what changed since its prior, and
	// DurationSeconds the time from its start to its stop (Phase 4).
	RepoSize        int64 `json:"repo_size,omitempty"`
	Delta           int64 `json:"delta,omitempty"`
	DurationSeconds int64 `json:"duration_seconds,omitempty"`
}

// RepoFacts is what pgbackrest info said about a database's stanza after
// its last backup, verify or expiry (Phase 4): the facts
// pgbackrest_exporter would otherwise poll the bucket for.
type RepoFacts struct {
	Stanza string `json:"stanza"`
	// WindowStart is the start of the oldest complete set: the earliest
	// point a restore can reach.
	WindowStart string `json:"window_start,omitempty"`
	Sets        int    `json:"sets"`
	// RepoSize is the sum of every set's repository size.
	RepoSize int64  `json:"repo_size"`
	At       string `json:"at"`
}

// FactsFrom summarises info as RepoFacts at time at.
func FactsFrom(info Info, at string) RepoFacts {
	out := RepoFacts{Stanza: info.Stanza, At: at}
	for _, s := range info.Sets {
		if s.Error {
			continue
		}
		out.Sets++
		out.RepoSize += s.RepoSize
		if s.Type == TypeFull && s.Start != "" && (out.WindowStart == "" || s.Start < out.WindowStart) {
			out.WindowStart = s.Start
		}
	}
	return out
}

// setDuration is the seconds between a set's start and stop, 0 when either
// doesn't parse.
func setDuration(set Set) int64 {
	start, err1 := time.Parse(time.RFC3339, set.Start)
	stop, err2 := time.Parse(time.RFC3339, set.Stop)
	if err1 != nil || err2 != nil || stop.Before(start) {
		return 0
	}
	return int64(stop.Sub(start).Seconds())
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
				"Inspect "+RecordsPath+" and fix or remove it. vaire never resets it on its own.",
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
	if s.Drilling == nil {
		s.Drilling = map[string]string{}
	}
	if s.Undoing == nil {
		s.Undoing = map[string]Undo{}
	}
	if s.AwaitingRecovery == nil {
		s.AwaitingRecovery = map[string]Awaiting{}
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
		before := s.durableHash()
		if err := fn(&s); err != nil {
			return err
		}
		s.Drills = trimDrills(s.Drills)
		raw, err := json.MarshalIndent(s, "", "  ")
		if err != nil {
			return err
		}
		if err := engine.WriteFileAtomic(RecordsPath, append(raw, '\n'), RecordsMode); err != nil {
			return err
		}
		if s.durableHash() != before {
			masterbackup.MarkDirty("records.json")
		}
		return nil
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

// trimDrills keeps the newest maxDrills drills of each database, in order.
func trimDrills(drills []Drill) []Drill {
	count := map[string]int{}
	keep := make([]bool, len(drills))
	for i := len(drills) - 1; i >= 0; i-- {
		count[drills[i].Database]++
		keep[i] = count[drills[i].Database] <= maxDrills
	}
	out := make([]Drill, 0, len(drills))
	for i, d := range drills {
		if keep[i] {
			out = append(out, d)
		}
	}
	return out
}

// DrillOf returns the newest drill of db that ran on stanza: like VerifyOf,
// a new stanza has no drill yet, and a drill recorded before drills named
// their stanza counts for none.
func (s Records) DrillOf(db, stanza string) (Drill, bool) {
	for i := len(s.Drills) - 1; i >= 0; i-- {
		if s.Drills[i].Database == db && s.Drills[i].Stanza != "" && s.Drills[i].Stanza == stanza {
			return s.Drills[i], true
		}
	}
	return Drill{}, false
}

// VerifyOf is db's last verify when it ran on stanza: a new stanza (after a
// restore from a dump, or a database created again) has no verify yet. A
// result recorded before verifies named their stanza counts for none.
func (s Records) VerifyOf(db, stanza string) (VerifyResult, bool) {
	v, ok := s.LastVerify[db]
	if !ok || v.Stanza == "" || v.Stanza != stanza {
		return VerifyResult{}, false
	}
	return v, true
}

// RecordBackup stores set as db's last backup of its type.
func (s *Records) RecordBackup(db string, set Set, finishedAt string) {
	if s.LastBackup[db] == nil {
		s.LastBackup[db] = map[string]BackupResult{}
	}
	s.LastBackup[db][set.Type] = BackupResult{
		Label: set.Label, FinishedAt: finishedAt, Size: set.Size,
		RepoSize: set.RepoSize, Delta: set.Delta, DurationSeconds: setDuration(set),
	}
	if f := s.LastFailure[db]; f != nil {
		delete(f, set.Type)
	}
}

// LastDrill is db's newest drill result.
func (s Records) LastDrill(db string) (Drill, bool) {
	for i := len(s.Drills) - 1; i >= 0; i-- {
		if s.Drills[i].Database == db {
			return s.Drills[i], true
		}
	}
	return Drill{}, false
}

// RecordBackupFailure stores when db's last backup of kind failed. A later
// success of that type clears it.
func (s *Records) RecordBackupFailure(db, kind, at string) {
	if s.LastFailure == nil {
		s.LastFailure = map[string]map[string]string{}
	}
	if s.LastFailure[db] == nil {
		s.LastFailure[db] = map[string]string{}
	}
	s.LastFailure[db][kind] = at
}

// RecordRepo stores db's stanza facts.
func (s *Records) RecordRepo(db string, facts RepoFacts) {
	if s.Repo == nil {
		s.Repo = map[string]RepoFacts{}
	}
	s.Repo[db] = facts
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
