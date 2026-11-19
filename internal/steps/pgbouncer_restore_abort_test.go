package steps

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// TestPgbouncerApplyRefusesRestoreAbortWithRestoreRecord is the ordinary-apply
// regression: the real pgbouncer step must refuse a restore-abort routing
// continuation while any restore record (here a noninspection-routable
// PhaseRestoring record) still exists, before rendering, writing or reloading.
func TestPgbouncerApplyRefusesRestoreAbortWithRestoreRecord(t *testing.T) {
	oldRecords, oldInstance := backup.RecordsPath, poolerInstance
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	poolerInstance = func() (string, error) { return "test-instance", nil }
	backupRoot := t.TempDir()
	oldRoot := backup.PGConfRootForTest(backupRoot)

	t.Cleanup(func() {
		backup.RecordsPath, poolerInstance = oldRecords, oldInstance

		oldRoot()
	})

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Port: 5433, Stanza: "app-abc123"}
	cfg.Clusters = []config.ClusterSpec{spec}

	dataDir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(backupRoot, spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(backupRoot, spec.Name, "postgresql.conf"),
		[]byte("data_directory = '"+dataDir+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Activation[spec.Name] = backup.ActivationFence{
			OperationID: "abort-op", Kind: "restore-abort", Database: spec.Name, Instance: "test-instance",
			TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
			Aside: "app__pre_restore_20260101T000000", Registration: fmt.Sprintf("%s:%d:", spec.Name, spec.Port),
			Ownership: spec.FollowGeneration, Port: spec.Port, DataDir: dataDir, ConfDir: PGConfDir(spec.Name),
			Phase: "routing", CreatedAt: "2026-01-01T00:00:00Z",
		}
		s.Restores[spec.Name] = backup.Restore{
			ID: "open-restore", Database: spec.Name, Phase: backup.PhaseRestoring, Port: 6100,
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	writes := 0
	oldWrite := writePoolerFile
	writePoolerFile = func(string, string, os.FileMode, string, string) error { writes++; return nil }

	t.Cleanup(func() { writePoolerFile = oldWrite })

	_, err := (pgbouncer{}).applyGuarded(context.Background(), cfg, engine.Runtime{Shell: engine.Shell{DryRun: true}}, nil)
	if err == nil || refusalReason(err) != "routing_eligibility" || !strings.Contains(err.Error(), "still has a restore record") {
		t.Fatalf("pgbouncer apply accepted a restore-abort continuation with a restore record: %v", err)
	}

	if writes != 0 {
		t.Fatalf("refused pgbouncer apply wrote %d pooler files", writes)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.Restores[spec.Name]; !ok {
		t.Fatal("refusal removed the coexisting restore record")
	}

	if f, ok := recs.ActivationFenceFor(spec.Name); !ok || f.Kind != "restore-abort" || f.Phase != "routing" {
		t.Fatalf("refusal changed the routing continuation: %+v", f)
	}
}
