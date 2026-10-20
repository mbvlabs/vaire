package backup

import (
	"sort"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/promfile"
)

// TextfilePath is vaire's backup textfile (§15.4), which node_exporter's
// textfile collector serves from Phase 4 on.
const TextfilePath = promfile.Dir + "/" + promfile.Backup

// WriteMetrics renders vaire.prom from records.json and the job records in
// state.json. It is called after every backup, verify, drill, dump and
// restore change, and by metrics-refresh; a failure to write is never fatal
// for the job.
func WriteMetrics(server, node string) error {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return err
	}
	rs, err := LoadRecords()
	if err != nil {
		return err
	}
	return promfile.Write(promfile.Backup, BackupFamilies(server, node, buildinfo.Version, rs, st.Jobs))
}

// BackupFamilies are vaire.prom's metric families: the build, and per
// database the last backups, verifies and drills, the stanza facts, open
// restores and skipped timer jobs.
func BackupFamilies(server, node, version string, rs Records, jobs []engine.JobRecord) []*promfile.Family {
	info := promfile.Gauge("vaire_info", "vaire build on this node.")
	info.Add(1, promfile.L("version", version, "server", server, "node", node)...)

	last := promfile.Gauge("vaire_backup_last_success_timestamp_seconds", "Last successful pgBackRest backup per database and type.")
	size := promfile.Gauge("vaire_backup_last_size_bytes", "Database size of the last successful backup per database and type.")
	repoSize := promfile.Gauge("vaire_backup_last_repo_size_bytes", "Repository size of the last successful backup per database and type.")
	duration := promfile.Gauge("vaire_backup_last_duration_seconds", "Duration of the last successful backup per database and type.")
	for _, db := range promfile.SortedKeys(rs.LastBackup) {
		for _, kind := range []string{TypeFull, TypeDiff, TypeIncr} {
			r, ok := rs.LastBackup[db][kind]
			if !ok {
				continue
			}
			labels := promfile.L("db", db, "type", kind)
			last.Add(float64(unix(r.FinishedAt)), labels...)
			size.Add(float64(r.Size), labels...)
			if r.RepoSize > 0 {
				repoSize.Add(float64(r.RepoSize), labels...)
			}
			if r.DurationSeconds > 0 {
				duration.Add(float64(r.DurationSeconds), labels...)
			}
		}
	}

	failed := promfile.Gauge("vaire_backup_last_failure_timestamp_seconds", "Last failed backup per database and type, until a success of that type.")
	for _, db := range promfile.SortedKeys(rs.LastFailure) {
		for _, kind := range promfile.SortedKeys(rs.LastFailure[db]) {
			failed.Add(float64(unix(rs.LastFailure[db][kind])), promfile.L("db", db, "type", kind)...)
		}
	}

	windowStart := promfile.Gauge("vaire_backup_window_start_timestamp_seconds", "Start of the oldest complete backup in each database's stanza: the earliest restorable point.")
	sets := promfile.Gauge("vaire_backup_sets", "Complete backup sets in each database's stanza.")
	stanzaSize := promfile.Gauge("vaire_backup_repo_size_bytes", "Repository size of every backup set in each database's stanza.")
	for _, db := range promfile.SortedKeys(rs.Repo) {
		f := rs.Repo[db]
		if f.WindowStart != "" {
			windowStart.Add(float64(unix(f.WindowStart)), promfile.L("db", db)...)
		}
		sets.Add(float64(f.Sets), promfile.L("db", db)...)
		stanzaSize.Add(float64(f.RepoSize), promfile.L("db", db)...)
	}

	verified := promfile.Gauge("vaire_backup_verify_last_success_timestamp_seconds", "Last pgbackrest verify of a database's stanza that passed.")
	for _, db := range promfile.SortedKeys(rs.LastVerify) {
		if v := rs.LastVerify[db]; v.OK {
			verified.Add(float64(unix(v.FinishedAt)), promfile.L("db", db)...)
		}
	}

	drills := map[string]Drill{}
	for _, d := range rs.Drills {
		drills[d.Database] = d
	}
	drillRun := promfile.Gauge("vaire_restore_drill_last_run_timestamp_seconds", "Last restore drill per database.")
	drillResult := promfile.Gauge("vaire_restore_drill_last_result", "Result of the last restore drill per database.")
	for _, db := range promfile.SortedKeys(drills) {
		drillRun.Add(float64(unix(drills[db].FinishedAt)), promfile.L("db", db)...)
		drillResult.Add(1, promfile.L("db", db, "result", drills[db].Result)...)
	}

	inProgress := promfile.Gauge("vaire_restore_in_progress", "Whether a restore of any database is open.")
	open := 0.0
	if len(rs.Restores) > 0 {
		open = 1
	}
	inProgress.Add(open)

	return []*promfile.Family{info, last, size, repoSize, duration, failed, windowStart, sets, stanzaSize,
		verified, drillRun, drillResult, inProgress, skippedJobs(jobs)}
}

// skippedJobs is the last time each timer job was skipped on a busy lock.
func skippedJobs(jobs []engine.JobRecord) *promfile.Family {
	type key struct{ job, db string }
	skipped := map[key]int64{}
	for _, j := range jobs {
		if j.State == engine.JobSkipped {
			skipped[key{j.Job, j.DB}] = unix(j.FinishedAt)
		}
	}
	keys := make([]key, 0, len(skipped))
	for k := range skipped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].job != keys[j].job {
			return keys[i].job < keys[j].job
		}
		return keys[i].db < keys[j].db
	})
	f := promfile.Gauge("vaire_job_last_skipped_timestamp_seconds", "Last time a timer job was skipped because a lock was busy.")
	for _, k := range keys {
		f.Add(float64(skipped[k]), promfile.L("job", k.job, "db", k.db)...)
	}
	return f
}

func unix(ts string) int64 {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return 0
	}
	return t.Unix()
}
