package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/render"
)

// Recovery starts its first full before enabling the recovered database's
// timers. The service templates must already exist after init --restore,
// even when every database is awaiting recovery, and without schedules.
func TestBackupServiceTemplatesDoNotDependOnDatabasesOrSchedules(t *testing.T) {
	for _, state := range []string{"empty", "awaiting", "unscheduled"} {
		t.Run(state, func(t *testing.T) {
			cfg := config.DefaultCluster()
			cfg.Backup.Provider = "r2"
			cfg.Backup.Bucket = "backups"
			cfg.Clusters = nil
			switch state {
			case "awaiting":
				cfg.Awaiting = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123"}}
			case "unscheduled":
				cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123"}}
				cfg.Backup.Database = config.BackupDatabase{}
			}
			files := timerFiles(cfg)
			for _, job := range []string{"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill", "db-dump"} {
				name := "eru-" + job + "@.service"
				if got := string(files[name]); got != string(render.JobService(job, "--db %i")) {
					t.Errorf("%s missing or incorrect", name)
				}
				if enabledUnit(name) {
					t.Errorf("%s must not be enabled automatically", name)
				}
			}
			for name := range files {
				if strings.Contains(name, "@") && strings.HasSuffix(name, ".timer") {
					t.Errorf("unexpected database timer %s", name)
				}
			}
		})
	}
}

func TestBackupServiceTemplatesRequireBackups(t *testing.T) {
	for name := range timerFiles(config.DefaultCluster()) {
		if strings.Contains(name, "@") {
			t.Errorf("database backup unit without backups: %s", name)
		}
	}
}
