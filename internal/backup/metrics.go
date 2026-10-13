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
	line("# HELP eru_backup_last_success_timestamp_seconds Last successful pgBackRest backup per type.")
	line("# TYPE eru_backup_last_success_timestamp_seconds gauge")
	for _, kind := range []string{TypeFull, TypeDiff, TypeIncr} {
		if r, ok := bs.LastBackup[kind]; ok {
			line(`eru_backup_last_success_timestamp_seconds{type=%q} %d`, kind, unix(r.FinishedAt))
		}
	}
	if v := bs.LastVerify; v != nil && v.OK {
		line("# HELP eru_backup_verify_last_success_timestamp_seconds Last pgbackrest verify that passed.")
		line("# TYPE eru_backup_verify_last_success_timestamp_seconds gauge")
		line("eru_backup_verify_last_success_timestamp_seconds %d", unix(v.FinishedAt))
	}
	if d, ok := bs.LastDrill(); ok {
		line("# HELP eru_restore_drill_last_run_timestamp_seconds Last restore drill.")
		line("# TYPE eru_restore_drill_last_run_timestamp_seconds gauge")
		line("eru_restore_drill_last_run_timestamp_seconds %d", unix(d.FinishedAt))
		line("# HELP eru_restore_drill_last_result Result of the last restore drill.")
		line("# TYPE eru_restore_drill_last_result gauge")
		line(`eru_restore_drill_last_result{result=%q} 1`, d.Result)
	}
	inProgress := 0
	if len(bs.Restores) > 0 {
		inProgress = 1
	}
	line("# HELP eru_restore_in_progress Whether a restore of any database is open.")
	line("# TYPE eru_restore_in_progress gauge")
	line("eru_restore_in_progress %d", inProgress)
	skipped := map[string]int64{}
	for _, j := range st.Jobs {
		if j.State == engine.JobSkipped {
			skipped[j.Job] = unix(j.FinishedAt)
		}
	}
	if len(skipped) > 0 {
		names := make([]string, 0, len(skipped))
		for name := range skipped {
			names = append(names, name)
		}
		sort.Strings(names)
		line("# HELP eru_job_last_skipped_timestamp_seconds Last time a timer job was skipped because the node lock was busy.")
		line("# TYPE eru_job_last_skipped_timestamp_seconds gauge")
		for _, name := range names {
			line(`eru_job_last_skipped_timestamp_seconds{job=%q} %d`, name, skipped[name])
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
