package backup

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/promfile"
)

func TestBackupFamilies(t *testing.T) {
	rs, err := decode(nil)
	if err != nil {
		t.Fatal(err)
	}
	set := Set{Label: "20261014-010000F", Type: TypeFull, Start: "2026-10-14T01:00:00Z", Stop: "2026-10-14T01:02:30Z", Size: 1000, RepoSize: 300, Delta: 1000}
	rs.RecordBackup("app", set, set.Stop)
	rs.RecordBackupFailure("app", TypeDiff, "2026-10-14T02:00:00Z")
	rs.RecordBackupFailure("app", TypeFull, "2026-10-13T02:00:00Z")
	rs.RecordBackup("app", set, set.Stop) // a full success clears the full failure
	rs.RecordRepo("app", FactsFrom(Info{Stanza: "app-abc123", Sets: []Set{
		set,
		{Type: TypeFull, Start: "2026-10-07T01:00:00Z", RepoSize: 200},
		{Type: TypeDiff, Start: "2026-10-06T01:00:00Z", RepoSize: 50},
		{Type: TypeFull, Start: "2026-10-01T01:00:00Z", Error: true},
	}}, "2026-10-14T01:03:00Z"))
	rs.LastVerify["app"] = VerifyResult{OK: true, FinishedAt: "2026-10-14T04:00:00Z"}
	rs.LastVerify["other"] = VerifyResult{OK: false, FinishedAt: "2026-10-14T04:00:00Z"}
	rs.Drills = []Drill{{Database: "app", FinishedAt: "2026-10-01T05:00:00Z", Result: DrillPass}}
	jobs := []engine.JobRecord{{Job: "backup-full", DB: "other", State: engine.JobSkipped, FinishedAt: "2026-10-14T01:00:00Z"}}
	out := string(promfile.Render(BackupFamilies("prod", "db1", "1.0.0", rs, jobs)))
	for _, line := range []string{
		`vaire_info{version="1.0.0",server="prod",node="db1"} 1`,
		`vaire_backup_last_success_timestamp_seconds{db="app",type="full"} 1791939750`,
		`vaire_backup_last_size_bytes{db="app",type="full"} 1000`,
		`vaire_backup_last_repo_size_bytes{db="app",type="full"} 300`,
		`vaire_backup_last_duration_seconds{db="app",type="full"} 150`,
		`vaire_backup_last_failure_timestamp_seconds{db="app",type="diff"} 1791943200`,
		`vaire_backup_window_start_timestamp_seconds{db="app"} 1791334800`,
		`vaire_backup_sets{db="app"} 3`,
		`vaire_backup_repo_size_bytes{db="app"} 550`,
		`vaire_backup_verify_last_success_timestamp_seconds{db="app"}`,
		`vaire_restore_drill_last_result{db="app",result="pass"} 1`,
		`vaire_restore_in_progress 0`,
		`vaire_job_last_skipped_timestamp_seconds{job="backup-full",db="other"}`,
	} {
		if !strings.Contains(out, line) {
			t.Errorf("missing %s in\n%s", line, out)
		}
	}
	for _, absent := range []string{`type="full"} 1791856800`, `verify_last_success_timestamp_seconds{db="other"}`} {
		if strings.Contains(out, absent) {
			t.Errorf("unexpected %s", absent)
		}
	}
}
