package backup

import (
	"encoding/json"
	"sort"
	"time"
)

// PrunedRecord is one thing a rebuild dropped from records.json because its
// data was on the lost server.
type PrunedRecord struct {
	Kind     string `json:"kind"`
	Database string `json:"database"`
	Name     string `json:"name,omitempty"`
}

// AwaitingDatabase is one database a rebuild marks awaiting recovery.
type AwaitingDatabase struct {
	Name   string
	Stanza string
}

// PruneForRestore prepares a bundle's records.json for a rebuilt master
// (Phase 5, D9): restore leftovers, open restores, drills and undos under
// way are dropped (their clusters were on the lost disk); dropped and
// replaced stanzas, timeline switches and the backup history are kept; and
// every database is marked awaiting recovery.
func PruneForRestore(raw []byte, databases []AwaitingDatabase, now time.Time) ([]byte, []PrunedRecord, error) {
	r, err := decode(raw)
	if err != nil {
		return nil, nil, err
	}
	var pruned []PrunedRecord
	for _, l := range r.Leftovers {
		pruned = append(pruned, PrunedRecord{Kind: "restore_leftover", Database: l.Database, Name: l.Name})
	}
	r.Leftovers = nil
	for db, rs := range r.Restores {
		pruned = append(pruned, PrunedRecord{Kind: "open_restore", Database: db, Name: RestoringName(rs.Database)})
	}
	r.Restores = map[string]Restore{}
	for db, name := range r.Drilling {
		pruned = append(pruned, PrunedRecord{Kind: "drill", Database: db, Name: name})
	}
	r.Drilling = map[string]string{}
	for db, u := range r.Undoing {
		pruned = append(pruned, PrunedRecord{Kind: "undo", Database: db, Name: u.Aside})
	}
	r.Undoing = map[string]Undo{}
	r.AwaitingRecovery = map[string]Awaiting{}
	since := now.UTC().Format(time.RFC3339)
	for _, d := range databases {
		r.AwaitingRecovery[d.Name] = Awaiting{Stanza: d.Stanza, Since: since}
	}
	sort.Slice(pruned, func(i, j int) bool {
		if pruned[i].Database != pruned[j].Database {
			return pruned[i].Database < pruned[j].Database
		}
		return pruned[i].Kind < pruned[j].Kind
	})
	out, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	return append(out, '\n'), pruned, nil
}
