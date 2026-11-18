package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/render"
)

// Recovery starts its first full before enabling the recovered database's
// timers. The service templates must already exist after init --restore,
// even when every database is awaiting recovery, and without schedules.
func TestBackupServiceTemplatesDoNotDependOnDatabasesOrSchedules(t *testing.T) {
	for _, state := range []string{"empty", "awaiting", "unscheduled"} {
		t.Run(state, func(t *testing.T) {
			assertBackupServiceTemplates(t, timersConfig(state))
		})
	}
}

// timersConfig is the cluster one template state describes: no databases,
// every database awaiting recovery, or a database whose schedules are off.
func timersConfig(state string) config.Cluster {
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

	return cfg
}

// assertBackupServiceTemplates checks every database job has its service
// template, none is enabled automatically, and no database timer exists.
func assertBackupServiceTemplates(t *testing.T, cfg config.Cluster) {
	t.Helper()

	files := timerFiles(cfg)

	for _, job := range []string{"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill"} {
		name := "vaire-" + job + "@.service"
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
}

func TestBackupServiceTemplatesRequireBackups(t *testing.T) {
	for name := range timerFiles(config.DefaultCluster()) {
		if strings.Contains(name, "@") {
			t.Errorf("database backup unit without backups: %s", name)
		}
	}
}

func TestNoDumpUnits(t *testing.T) {
	for name := range timerFiles(timersConfig("unscheduled")) {
		if strings.Contains(name, "dump") {
			t.Errorf("dump unit still rendered: %s", name)
		}
	}
}
