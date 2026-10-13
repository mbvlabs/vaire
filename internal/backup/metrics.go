package backup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/engine"
)

// TextfilePath is eru's node_exporter textfile (§15.4). node_exporter
// arrives in Phase 5 and scrapes it from then on; eru writes it from Phase 3.
const TextfilePath = "/var/lib/node_exporter/textfile/eru.prom"

// WriteMetrics renders the eru.prom backup lines from records.json and the
// job records in state.json. It is called after every backup, verify, drill,
// dump and restore change, and a failure to write is never fatal for the job.
func WriteMetrics(cluster, node string) error {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return err
	}
	bs, err := LoadRecords()
	if err != nil {
		return err
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	line("# HELP eru_info eru build on this node.")
	line("# TYPE eru_info gauge")
	line(`eru_info{version=%q,cluster=%q,node=%q} 1`, buildinfo.Version, cluster, node)
	dbs := make([]string, 0, len(bs.LastBackup))
	for db := range bs.LastBackup {
		dbs = append(dbs, db)
	}
	sort.Strings(dbs)
	line("# HELP eru_backup_last_success_timestamp_seconds Last successful pgBackRest backup per database and type.")
	line("# TYPE eru_backup_last_success_timestamp_seconds gauge")
	for _, db := range dbs {
		for _, kind := range []string{TypeFull, TypeDiff, TypeIncr} {
			if r, ok := bs.LastBackup[db][kind]; ok {
				line(`eru_backup_last_success_timestamp_seconds{db=%q,type=%q} %d`, db, kind, unix(r.FinishedAt))
			}
		}
	}
	verified := make([]string, 0, len(bs.LastVerify))
	for db, v := range bs.LastVerify {
		if v.OK {
			verified = append(verified, db)
		}
	}
	sort.Strings(verified)
	if len(verified) > 0 {
		line("# HELP eru_backup_verify_last_success_timestamp_seconds Last pgbackrest verify of a database's stanza that passed.")
		line("# TYPE eru_backup_verify_last_success_timestamp_seconds gauge")
		for _, db := range verified {
			line(`eru_backup_verify_last_success_timestamp_seconds{db=%q} %d`, db, unix(bs.LastVerify[db].FinishedAt))
		}
	}
	drills := map[string]Drill{}
	for _, d := range bs.Drills {
		drills[d.Database] = d
	}
	if len(drills) > 0 {
		drilled := make([]string, 0, len(drills))
		for db := range drills {
			drilled = append(drilled, db)
		}
		sort.Strings(drilled)
		line("# HELP eru_restore_drill_last_run_timestamp_seconds Last restore drill per database.")
		line("# TYPE eru_restore_drill_last_run_timestamp_seconds gauge")
		for _, db := range drilled {
			line(`eru_restore_drill_last_run_timestamp_seconds{db=%q} %d`, db, unix(drills[db].FinishedAt))
		}
		line("# HELP eru_restore_drill_last_result Result of the last restore drill per database.")
		line("# TYPE eru_restore_drill_last_result gauge")
		for _, db := range drilled {
			line(`eru_restore_drill_last_result{db=%q,result=%q} 1`, db, drills[db].Result)
		}
	}
	inProgress := 0
	if len(bs.Restores) > 0 {
		inProgress = 1
	}
	line("# HELP eru_restore_in_progress Whether a restore of any database is open.")
	line("# TYPE eru_restore_in_progress gauge")
	line("eru_restore_in_progress %d", inProgress)
	type jobKey struct{ job, db string }
	skipped := map[jobKey]int64{}
	for _, j := range st.Jobs {
		if j.State == engine.JobSkipped {
			skipped[jobKey{j.Job, j.DB}] = unix(j.FinishedAt)
		}
	}
	if len(skipped) > 0 {
		keys := make([]jobKey, 0, len(skipped))
		for k := range skipped {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].job != keys[j].job {
				return keys[i].job < keys[j].job
			}
			return keys[i].db < keys[j].db
		})
		line("# HELP eru_job_last_skipped_timestamp_seconds Last time a timer job was skipped because a lock was busy.")
		line("# TYPE eru_job_last_skipped_timestamp_seconds gauge")
		for _, k := range keys {
			line(`eru_job_last_skipped_timestamp_seconds{job=%q,db=%q} %d`, k.job, k.db, skipped[k])
		}
	}
	if err := os.MkdirAll(filepath.Dir(TextfilePath), 0o755); err != nil { //nolint:gosec // G301: node_exporter reads this directory.
		return err
	}
	return engine.WriteFileAtomic(TextfilePath, []byte(b.String()), 0o644)
}

func unix(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	return t.Unix()
}
