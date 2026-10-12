package backup

import (
	"encoding/json"
	"sort"

	"github.com/mbvlabs/eru/internal/engine"
)

// maxDrills is how many drill results state.json keeps.
const maxDrills = 12

// State is the backup section of /var/lib/eru/state.json.
type State struct {
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

// LoadState reads the backup section of state.json.
func LoadState() (State, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return State{}, err
	}
	return decode(st.Backup)
}

func decode(raw json.RawMessage) (State, error) {
	var s State
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return State{}, err
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

// Update applies fn to the backup section under the state lock. The rest of
// state.json is kept as it is.
func Update(fn func(*State) error) error {
	return engine.UpdateState(func(st *engine.State) error {
		s, err := decode(st.Backup)
		if err != nil {
			return err
		}
		if err := fn(&s); err != nil {
			return err
		}
		if len(s.Drills) > maxDrills {
			s.Drills = append([]Drill(nil), s.Drills[len(s.Drills)-maxDrills:]...)
		}
		raw, err := json.Marshal(s)
		if err != nil {
			return err
		}
		st.Backup = raw
		return nil
	})
}

// OpenRestore returns the open restore of db, if any.
func (s State) OpenRestore(db string) (Restore, bool) {
	r, ok := s.Restores[db]
	return r, ok
}

// LeftoversOf returns the leftovers of db, or all of them when db is empty.
func (s State) LeftoversOf(db string) []Leftover {
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
func (s State) LastDrill() (Drill, bool) {
	if len(s.Drills) == 0 {
		return Drill{}, false
	}
	return s.Drills[len(s.Drills)-1], true
}
