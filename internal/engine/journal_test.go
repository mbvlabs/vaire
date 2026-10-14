package engine

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestForgetClusterAndSteps(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &Journal{Steps: map[string]JournalEntry{
		"pgbouncer-v1":            {Status: StatusCompleted},
		"timers-v1":               {Status: StatusCompleted},
		"postgres-cluster-v1@app": {Status: StatusCompleted},
		"postgres-config-v1@app":  {Status: StatusCompleted},
		"postgres-config-v1@app2": {Status: StatusCompleted},
	}, RestartPending: []string{"postgres-config-v1@app", "postgres-config-v1@app2"}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}

	if err := ForgetCluster(path, "app"); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"postgres-cluster-v1@app", "postgres-config-v1@app"} {
		if _, ok := got.Steps[id]; ok {
			t.Errorf("%s is still journaled", id)
		}
	}
	for _, id := range []string{"pgbouncer-v1", "timers-v1", "postgres-config-v1@app2"} {
		if _, ok := got.Steps[id]; !ok {
			t.Errorf("%s was removed", id)
		}
	}
	if want := []string{"postgres-config-v1@app2"}; !slices.Equal(got.RestartPending, want) {
		t.Errorf("restart_pending = %v, want %v", got.RestartPending, want)
	}

	if err := ForgetSteps(path, "pgbouncer-v1"); err != nil {
		t.Fatal(err)
	}
	got, err = LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Steps["pgbouncer-v1"]; ok {
		t.Error("pgbouncer-v1 is still journaled")
	}
	if _, ok := got.Steps["timers-v1"]; !ok {
		t.Error("timers-v1 was removed")
	}
}

func TestForgetMissingJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	if err := ForgetCluster(path, "app"); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadJournal(path); err != nil {
		t.Fatal(err)
	}
}
