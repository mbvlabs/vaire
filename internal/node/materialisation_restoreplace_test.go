package node

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

func TestRestorePlacementPrunesDrillAttemptsWithoutLosingHistory(t *testing.T) {
	recs := backup.Records{
		Drilling: map[string]string{"app": "app__drill"},
		DrillAttempts: map[string]backup.MaterialisationAttempt{
			"app": {Target: "app__drill"}, "other": {Target: "other__drill"},
		},
		LastBackup: map[string]map[string]backup.BackupResult{"app": {"full": {Label: "backup-1"}}},
		Drills:     []backup.Drill{{ID: "historic", Result: backup.DrillPass}},
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc"}, {Name: "other", Stanza: "other-abc"}}
	bundle := masterbackup.Bundle{Files: []masterbackup.File{masterbackup.NewFile(masterbackup.RecordsPath, "root", "root", 0o600, raw)}}
	files, pruned, err := restoreFiles(bundle, cfg, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var got backup.Records
	for _, file := range files {
		if file.Path == masterbackup.RecordsPath {
			if err := json.Unmarshal(file.Data, &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(got.Drilling) != 0 || len(got.DrillAttempts) != 0 || got.LastBackup["app"]["full"].Label != "backup-1" || len(got.Drills) != 1 {
		t.Fatalf("placed records: %+v", got)
	}
	count := map[string]int{}
	for _, p := range pruned {
		if p.Kind == "drill" {
			count[p.Name]++
		}
	}
	if count["app__drill"] != 1 || count["other__drill"] != 1 || len(count) != 2 {
		t.Fatalf("pruned drills: %+v", pruned)
	}
}
