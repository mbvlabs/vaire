package engine

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

type incompleteStep struct {
	id            string
	checkComplete bool
	applyResult   ApplyResult
	applied       int
}

func (s *incompleteStep) ID() string                     { return s.id }
func (s *incompleteStep) Describe(config.Cluster) string { return s.id }
func (s *incompleteStep) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s *incompleteStep) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return CheckResult{Complete: s.checkComplete}, nil
}
func (s *incompleteStep) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	s.applied++
	return s.applyResult, nil
}

type restartingStep struct{ id string }

func (s *restartingStep) ID() string                     { return s.id }
func (s *restartingStep) Describe(config.Cluster) string { return s.id }
func (s *restartingStep) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s *restartingStep) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return CheckResult{Complete: true}, nil
}
func (s *restartingStep) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	return ApplyResult{Restart: RestartRestart}, nil
}

type restartTemplate struct{ base string }

func (t restartTemplate) ID() string                     { return t.base }
func (t restartTemplate) Describe(config.Cluster) string { return t.base }
func (t restartTemplate) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(t.base), nil
}
func (t restartTemplate) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return CheckResult{Complete: true}, nil
}
func (t restartTemplate) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	return ApplyResult{}, nil
}
func (t restartTemplate) Instance(cluster string) Step {
	return &restartingStep{id: InstanceID(t.base, cluster)}
}

type incompleteTemplate struct {
	base string
	app  *incompleteStep
}

func (t incompleteTemplate) ID() string                     { return t.base }
func (t incompleteTemplate) Describe(config.Cluster) string { return t.base }
func (t incompleteTemplate) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(t.base), nil
}
func (t incompleteTemplate) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	return CheckResult{Complete: true}, nil
}
func (t incompleteTemplate) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	return ApplyResult{}, nil
}
func (t incompleteTemplate) Instance(cluster string) Step {
	if cluster == "app" {
		return t.app
	}
	return &incompleteStep{id: InstanceID(t.base, cluster)}
}

// An incomplete Apply is journaled with its inputs hash but not completed. The
// run continues to later steps, and the next apply rechecks it and finishes
// the work; a later apply then skips it normally.
func TestIncompleteApplyIsJournaledAndRetried(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	roles := &incompleteStep{
		id:          "roles-v1",
		applyResult: ApplyResult{Incomplete: true, Notes: []string{"extension pending restart"}},
	}
	later := &incompleteStep{id: "later-v1"}
	var events []string
	report := Reporter(func(ev output.Event) {
		if ev.Type == "step" {
			events = append(events, ev.Step+" "+ev.State)
		}
	})
	if _, _, err := Apply(context.Background(), []Step{roles, later}, config.Cluster{}, Runtime{}, Options{JournalPath: path, Report: report}); err != nil {
		t.Fatal(err)
	}
	if roles.applied != 1 || later.applied != 1 {
		t.Fatalf("applied roles=%d later=%d, want both once", roles.applied, later.applied)
	}
	if !slices.Contains(events, "roles-v1 incomplete") || !slices.Contains(events, "later-v1 completed") {
		t.Fatalf("events = %v", events)
	}
	j, err := LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Steps[roles.ID()]; got.Status != StatusIncomplete || got.CompletedAt != "" || got.Error != "" || got.InputsHash == "" {
		t.Fatalf("incomplete journal entry = %+v", got)
	}

	// The next apply sees the missing SQL (Check is false), applies again and
	// now completes it.
	roles.applyResult = ApplyResult{}
	if _, _, err := Apply(context.Background(), []Step{roles, later}, config.Cluster{}, Runtime{}, Options{JournalPath: path}); err != nil {
		t.Fatal(err)
	}
	if roles.applied != 2 || later.applied != 1 {
		t.Fatalf("retry applied roles=%d later=%d", roles.applied, later.applied)
	}
	j, err = LoadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := j.Steps[roles.ID()]; got.Status != StatusCompleted || got.CompletedAt == "" || got.Error != "" {
		t.Fatalf("completed journal entry = %+v", got)
	}

	// With the host now matching, the completed entry skips.
	roles.checkComplete = true
	if _, _, err := Apply(context.Background(), []Step{roles, later}, config.Cluster{}, Runtime{}, Options{JournalPath: path}); err != nil {
		t.Fatal(err)
	}
	if roles.applied != 2 {
		t.Fatalf("completed step applied again: %d", roles.applied)
	}
}

// A deferred step runs before the end-of-run restart. Without a token it stays
// pending; a token for another database changes nothing; the confirmed token
// restarts the exact cluster. The next same-hash apply creates the SQL.
func TestIncompleteContinuesToAuthorizedRestart(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	configStepID := InstanceID("config-v1", "app")
	roles := &incompleteStep{id: "roles-v1@app", applyResult: ApplyResult{Incomplete: true}}
	later := &incompleteStep{id: "later-v1@app"}
	catalog := []Step{
		restartTemplate{base: "config-v1"},
		incompleteTemplate{base: "roles-v1", app: roles},
		incompleteTemplate{base: "later-v1", app: later},
	}
	restarter := &fakeRestarter{needs: map[string]bool{"app": true, "other": false}}

	data, _, err := Apply(context.Background(), catalog, cfg, Runtime{}, Options{JournalPath: path, Restarter: restarter, DB: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if len(restarter.restarted) != 0 {
		t.Fatalf("restarted without a token: %v", restarter.restarted)
	}
	if !slices.Equal(data.RestartPending, []string{configStepID}) {
		t.Fatalf("pending = %v", data.RestartPending)
	}
	if roles.applied != 1 || later.applied != 1 {
		t.Fatalf("first run applied roles=%d later=%d", roles.applied, later.applied)
	}

	// A token for another database does not consume app's pending restart.
	if _, _, err := Apply(context.Background(), catalog, cfg, Runtime{}, Options{
		JournalPath: path, Restarter: restarter, Confirm: Confirmation{Clusters: []string{"other"}}, DB: "app",
	}); err != nil {
		t.Fatal(err)
	}
	if len(restarter.restarted) != 0 {
		t.Fatalf("a wrong token restarted %v", restarter.restarted)
	}
	if j, err := LoadJournal(path); err != nil || !slices.Equal(j.RestartPending, []string{configStepID}) {
		t.Fatalf("wrong token changed pending: %v, %v", j.RestartPending, err)
	}

	// The confirmed run still has roles before the end-of-run restart, so it
	// defers again, then restarts exactly app.
	data, _, err = Apply(context.Background(), catalog, cfg, Runtime{}, Options{
		JournalPath: path, Restarter: restarter, Confirm: Confirmation{Clusters: []string{"app"}}, DB: "app",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(restarter.restarted, []string{"app"}) || len(data.RestartPending) != 0 {
		t.Fatalf("restarted %v, pending %v", restarter.restarted, data.RestartPending)
	}
	if roles.applied != 3 {
		t.Fatalf("roles did not reapply before the restart: %d", roles.applied)
	}
	if j, err := LoadJournal(path); err != nil || j.Steps[roles.ID()].Status != StatusIncomplete {
		t.Fatalf("roles after confirmed run = %+v, %v", j.Steps[roles.ID()], err)
	}

	// The next apply sees the library loaded, creates the SQL and completes.
	roles.applyResult = ApplyResult{}
	if _, _, err := Apply(context.Background(), catalog, cfg, Runtime{}, Options{JournalPath: path, Restarter: restarter, DB: "app"}); err != nil {
		t.Fatal(err)
	}
	if roles.applied != 4 {
		t.Fatalf("post-restart retry applied roles %d times", roles.applied)
	}
	if j, err := LoadJournal(path); err != nil || j.Steps[roles.ID()].Status != StatusCompleted {
		t.Fatalf("roles after retry = %+v, %v", j.Steps[roles.ID()], err)
	}
}

// A matching-hash noncompleted entry must not shortcut to completed on a
// coarse Check: failed, running and incomplete entries all run Check, and an
// incomplete result applies the step.
func TestNonCompletedStatusesRecheck(t *testing.T) {
	for _, status := range []string{StatusFailed, StatusRunning, StatusIncomplete} {
		t.Run(status, func(t *testing.T) {
			step := &incompleteStep{id: "roles-v1"}
			path := filepath.Join(t.TempDir(), "journal.json")
			j := &Journal{Steps: map[string]JournalEntry{
				step.ID(): {Status: status, InputsHash: InputsHash(step.ID(), []byte(step.ID()))},
			}}
			if err := j.Save(path); err != nil {
				t.Fatal(err)
			}
			if _, _, err := Apply(context.Background(), []Step{step}, config.Cluster{}, Runtime{}, Options{JournalPath: path}); err != nil {
				t.Fatal(err)
			}
			if step.applied != 1 {
				t.Fatalf("status %s did not recheck and apply: %d", status, step.applied)
			}
		})
	}
}
