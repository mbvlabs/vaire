package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
)

type inconclusiveStep struct {
	countedStep
	result  CheckResult
	applied *int
}

func (s inconclusiveStep) Recheck() bool { return true }
func (s inconclusiveStep) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return s.result, nil
}
func (s inconclusiveStep) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	*s.applied++
	return ApplyResult{}, nil
}

func TestInconclusiveMatchingCompletedDoesNotMutate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	id := "cpu-v1"
	j := &Journal{Steps: map[string]JournalEntry{id: {Status: StatusCompleted, InputsHash: InputsHash(id, []byte(id)), CompletedAt: "before"}}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary journal.
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	step := inconclusiveStep{countedStep: countedStep{id}, result: CheckResult{Inconclusive: true, Detail: "cpu cgroup unavailable"}, applied: &applied}
	plan, err := Plan(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path})
	if err != nil || plan.WouldApply != 0 || plan.Steps[0].Reason != ReasonCheckInconclusive || !strings.Contains(plan.Steps[0].Detail, "unknown") {
		t.Fatalf("plan %+v: %v", plan, err)
	}
	result, warnings, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path})
	if err != nil || applied != 0 || result.Skipped != 1 || result.Steps[0].Reason != ReasonCheckInconclusive ||
		!strings.Contains(result.Steps[0].Detail, "unknown") || len(warnings) != 1 || warnings[0].Code != "step_check_inconclusive" {
		t.Fatalf("apply %+v warnings %+v error %v", result, warnings, err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary journal.
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("journal changed: %v", err)
	}
}

func TestPlanAfterInvalidatingCompletionsMatchesJournalInvalidation(t *testing.T) {
	for _, status := range []string{StatusCompleted, StatusFailed, StatusRunning} {
		t.Run(status, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			id := "cpu-v1"
			j := &Journal{Steps: map[string]JournalEntry{
				id: {Status: status, InputsHash: InputsHash(id, []byte(id))},
			}, RestartPending: []string{id}}
			if err := j.Save(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path) //nolint:gosec // Test-owned journal.
			if err != nil {
				t.Fatal(err)
			}
			applied := 0
			step := inconclusiveStep{countedStep: countedStep{id}, result: CheckResult{Inconclusive: true}, applied: &applied}
			opts := Options{JournalPath: path}
			preview, err := PlanAfterInvalidatingCompletions(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, opts, []string{id})
			if err != nil {
				t.Fatal(err)
			}
			after, err := os.ReadFile(path) //nolint:gosec // Test-owned journal.
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("preview mutated journal: %v", err)
			}
			if err := InvalidateStepCompletion(path, id); err != nil {
				t.Fatal(err)
			}
			actual, err := Plan(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, opts)
			if err != nil || !slices.Equal(preview.RestartPending, actual.RestartPending) ||
				len(preview.Steps) != 1 || len(actual.Steps) != 1 ||
				preview.Steps[0].Action != actual.Steps[0].Action || preview.Steps[0].Reason != actual.Steps[0].Reason {
				t.Fatalf("preview %+v actual %+v error %v", preview, actual, err)
			}
		})
	}
}

func TestInconclusiveDoesNotSkipOtherJournalStates(t *testing.T) {
	for _, tc := range []struct {
		name, status, hash string
		present            bool
	}{
		{"missing", "", "", false},
		{"changed", StatusCompleted, "old", true},
		{"failed", StatusFailed, "same", true},
		{"running", StatusRunning, "same", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.json")
			id := "cpu-v1"
			j := &Journal{Steps: map[string]JournalEntry{}}
			if tc.present {
				hash := tc.hash
				if hash == "same" {
					hash = InputsHash(id, []byte(id))
				}
				j.Steps[id] = JournalEntry{Status: tc.status, InputsHash: hash}
			}
			if err := j.Save(path); err != nil {
				t.Fatal(err)
			}
			applied := 0
			step := inconclusiveStep{countedStep: countedStep{id}, result: CheckResult{Complete: true, Inconclusive: true}, applied: &applied}
			data, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path})
			if err != nil || applied != 1 || data.Applied != 1 {
				t.Fatalf("result %+v applied %d err %v", data, applied, err)
			}
		})
	}
}

func TestFailedAndInterruptedStepsAlwaysRetry(t *testing.T) {
	for _, status := range []string{StatusFailed, StatusRunning} {
		path := filepath.Join(t.TempDir(), "journal.json")
		id := "cpu-v1"
		j := &Journal{Steps: map[string]JournalEntry{id: {Status: status, InputsHash: InputsHash(id, []byte(id))}}}
		if err := j.Save(path); err != nil {
			t.Fatal(err)
		}
		applied := 0
		step := inconclusiveStep{countedStep: countedStep{id}, result: CheckResult{Complete: true}, applied: &applied}
		data, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path})
		if err != nil || data.Applied != 1 || applied != 1 {
			t.Fatalf("status %s: data %+v applied %d err %v", status, data, applied, err)
		}
	}
}

type noRestartCallbacks struct{ checks, restarts int }

func (r *noRestartCallbacks) NeedsRestart(context.Context, string) (bool, error) {
	r.checks++
	return false, nil
}

func (r *noRestartCallbacks) Restart(context.Context, string) (time.Duration, error) {
	r.restarts++
	return 0, nil
}

func TestLeaveRestartsPendingNeverCallsRestarter(t *testing.T) {
	for _, haveRestarter := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "journal.json")
		cfg := config.Cluster{ClusterName: "prod", Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
		catalog := []Step{templateStep{countedStep{"cfg-v1"}}}
		j := &Journal{Steps: map[string]JournalEntry{}, RestartPending: []string{"cfg-v1@app", "cfg-v1@other"}}
		for _, id := range j.RestartPending {
			j.Steps[id] = JournalEntry{Status: StatusCompleted, InputsHash: InputsHash(id, []byte(id))}
		}
		if err := j.Save(path); err != nil {
			t.Fatal(err)
		}
		opts := Options{JournalPath: path, DB: "app", Only: []string{"cfg-v1@app"}, Confirm: Confirmation{Server: true}, LeaveRestartsPending: true}
		r := &noRestartCallbacks{}
		if haveRestarter {
			opts.Restarter = r
		}
		data, warnings, err := Apply(context.Background(), catalog, cfg, Runtime{}, opts)
		if err != nil || r.checks != 0 || r.restarts != 0 || !slices.Equal(data.RestartPending, []string{"cfg-v1@app"}) ||
			!slices.Equal(data.ConfirmTokens, []string{"app/restart"}) || len(warnings) != 1 || warnings[0].Code != "restart_pending" {
			t.Fatalf("data %+v warnings %+v callbacks %+v err %v", data, warnings, r, err)
		}
		got, err := LoadJournal(path)
		if err != nil || !slices.Equal(got.RestartPending, []string{"cfg-v1@app", "cfg-v1@other"}) {
			t.Fatalf("pending %+v err %v", got.RestartPending, err)
		}
	}
}

func TestInvalidateStepCompletionPreservesPendingAndRetries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	id := "cpu-v1"
	other := "other-v1"
	j := &Journal{Steps: map[string]JournalEntry{
		id:    {Status: StatusCompleted, InputsHash: InputsHash(id, []byte(id))},
		other: {Status: StatusFailed, Error: "keep"},
	}, RestartPending: []string{id, other}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	if err := InvalidateStepCompletion(path, id); err != nil {
		t.Fatal(err)
	}
	got, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := got.Steps[id]; exists || got.Steps[other].Status != StatusFailed || !slices.Equal(got.RestartPending, []string{id, other}) {
		t.Fatalf("%+v", got)
	}
	applied := 0
	step := inconclusiveStep{countedStep: countedStep{id}, result: CheckResult{Inconclusive: true}, applied: &applied}
	data, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path, LeaveRestartsPending: true})
	if err != nil || applied != 1 || data.Applied != 1 {
		t.Fatalf("data %+v applied %d err %v", data, applied, err)
	}
}

func TestInvalidateOnlyCompletedExactStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &Journal{Steps: map[string]JournalEntry{
		"cpu-v1@app":   {Status: StatusFailed},
		"cpu-v1@app2":  {Status: StatusCompleted},
		"cpu-v1@other": {Status: StatusRunning},
	}, RestartPending: []string{"cpu-v1@app", "cpu-v1@app2"}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary journal.
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"cpu-v1@app", "cpu-v1@other", "cpu-v1@missing"} {
		if err := InvalidateStepCompletion(path, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := InvalidateStepCompletion(path, "cpu-v1@app@other"); err == nil {
		t.Fatal("invalid instance ID accepted")
	}
	after, err := os.ReadFile(path) //nolint:gosec // Test-owned temporary journal.
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("journal unexpectedly changed: %v", err)
	}
}
