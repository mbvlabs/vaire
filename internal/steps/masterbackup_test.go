package steps

import (
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestMasterBackupUnits(t *testing.T) {
	cfg := metricsCluster("app")
	files := timerFiles(cfg)
	path := string(files[MasterBackupPath])
	if !strings.Contains(path, "PathExists=/var/lib/vaire/master-backup/dirty") || !strings.Contains(path, "Unit=vaire-master-backup.service") {
		t.Errorf("path unit:\n%s", path)
	}
	if !strings.Contains(string(files[MasterBackupService]), "vaire node run master-backup") {
		t.Error("master-backup service")
	}
	if !strings.Contains(string(files[MasterBackupTimer]), "OnCalendar=*-*-* 02:00:00") {
		t.Errorf("daily timer:\n%s", files[MasterBackupTimer])
	}
	if _, ok := files[FenceService]; ok {
		t.Error("no bucket, but the fence check")
	}

	cfg.Backup = config.Backup{Provider: "r2", Endpoint: "https://x.r2.cloudflarestorage.com", Region: "auto", Bucket: "b", Prefix: "prod", URIStyle: "path", Database: cfg.Backup.Database}
	files = timerFiles(cfg)
	fence := string(files[FenceService])
	for _, want := range []string{"Before=pgbouncer.service vaire-pgbouncer-session.service", "vaire node run fence-check", "WantedBy=multi-user.target", "TimeoutStartSec=30"} {
		if !strings.Contains(fence, want) {
			t.Errorf("fence service lacks %q:\n%s", want, fence)
		}
	}
	if !strings.Contains(string(files[FenceTimer]), "OnCalendar=*:0/5") {
		t.Error("fence timer every five minutes")
	}
	for _, name := range []string{MasterBackupService, FenceService} {
		if !ownedService.MatchString(name) {
			t.Errorf("%s is not owned", name)
		}
	}
	var enabled []string
	for _, name := range sortedNames(files) {
		if enabledUnit(name) {
			enabled = append(enabled, name)
		}
		if !unitNamePattern.MatchString(name) {
			t.Errorf("%s fails the unit name guard", name)
		}
	}
	for _, want := range []string{MasterBackupPath, MasterBackupTimer, FenceService, FenceTimer} {
		if !slices.Contains(enabled, want) {
			t.Errorf("%s is not enabled", want)
		}
	}
	if slices.Contains(enabled, MasterBackupService) {
		t.Error("the job service itself is enabled; only its path and timer should be")
	}
}

func TestMasterBackupStepInCatalogue(t *testing.T) {
	ids := DatabaseStepIDs()
	i, j := slices.Index(ids, pgbackrestID), slices.Index(ids, masterBackupID)
	if j < 0 || j != i+1 {
		t.Fatalf("master-backup-v1 at %d, pgbackrest-v1 at %d: %v", j, i, ids)
	}
	if !slices.Contains(BackupStepIDs(), masterBackupID) {
		t.Error("backup setup doesn't run master-backup-v1")
	}
}
