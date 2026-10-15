package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/config"
)

type templateStep struct{ countedStep }

func (s templateStep) Instance(cluster string) Step {
	return countedStep{InstanceID(s.id, cluster)}
}

type fakeRestarter struct {
	needs     map[string]bool
	restarted []string
}

func (f *fakeRestarter) Restart(_ context.Context, cluster string) (time.Duration, error) {
	f.restarted = append(f.restarted, cluster)
	return time.Millisecond, nil
}

func (f *fakeRestarter) NeedsRestart(_ context.Context, cluster string) (bool, error) {
	return f.needs[cluster], nil
}

// A pending restart the cluster no longer needs (PostgreSQL already runs
// every setting) is cleared without a restart, with or without its confirm
// token; one still needed waits for its token.
func TestSettledRestartsClearWithoutRestarting(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
	catalog := []Step{templateStep{countedStep{"cfg-v1"}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &Journal{Steps: map[string]JournalEntry{}, RestartPending: []string{"cfg-v1@app", "cfg-v1@other"}}
	for _, id := range []string{"cfg-v1@app", "cfg-v1@other"} {
		j.Steps[id] = JournalEntry{Status: StatusCompleted, InputsHash: InputsHash(id, []byte(id))}
	}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	r := &fakeRestarter{needs: map[string]bool{"app": true}}
	data, _, err := Apply(context.Background(), catalog, cfg, Runtime{}, Options{JournalPath: path, Restarter: r})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.restarted) != 0 {
		t.Errorf("restarted %v without a token", r.restarted)
	}
	if !slices.Equal(data.RestartsSettled, []string{"other"}) {
		t.Errorf("settled %v, want [other]", data.RestartsSettled)
	}
	if !slices.Equal(data.RestartPending, []string{"cfg-v1@app"}) {
		t.Errorf("still pending %v, want [cfg-v1@app]", data.RestartPending)
	}
	got, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.RestartPending, []string{"cfg-v1@app"}) {
		t.Errorf("journal pending %v", got.RestartPending)
	}

	// With app's token, app restarts; other isn't touched.
	data, _, err = Apply(context.Background(), catalog, cfg, Runtime{}, Options{JournalPath: path, Restarter: r, Confirm: Confirmation{Clusters: []string{"app", "other"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.restarted, []string{"app"}) || len(data.RestartPending) != 0 {
		t.Errorf("restarted %v, pending %v", r.restarted, data.RestartPending)
	}
}

type driftStep struct {
	countedStep
	drifted bool
	applied *int
}

func (s driftStep) Recheck() bool { return true }
func (s driftStep) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	if s.drifted {
		return CheckResult{Detail: "ufw rules differ"}, nil
	}
	return CheckResult{Complete: true}, nil
}
func (s driftStep) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	*s.applied++
	return ApplyResult{}, nil
}

// A Rechecker that the journal says is current still applies when the host
// drifted, and skips as journaled when it didn't.
func TestRecheckerAppliesOnDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &Journal{Steps: map[string]JournalEntry{"fw-v1": {Status: StatusCompleted, InputsHash: InputsHash("fw-v1", []byte("fw-v1"))}}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	applied := 0
	step := driftStep{countedStep: countedStep{"fw-v1"}, applied: &applied}
	if _, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path}); err != nil {
		t.Fatal(err)
	}
	if applied != 0 {
		t.Fatalf("applied %d times with no drift", applied)
	}
	step.drifted = true
	plan, err := Plan(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Steps) != 1 || plan.Steps[0].Reason != ReasonCheckIncomplete || plan.Steps[0].Detail != "ufw rules differ" {
		t.Errorf("plan %+v", plan.Steps)
	}
	if _, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path}); err != nil {
		t.Fatal(err)
	}
	if applied != 1 {
		t.Errorf("applied %d times after drift, want 1", applied)
	}
}
