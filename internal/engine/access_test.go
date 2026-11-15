package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

type accessFake struct {
	cluster string
	calls   *[]string
	pending map[string]bool
	fail    bool
	unknown string
}

func (s accessFake) ID() string {
	if s.cluster == "" {
		return "access-v1"
	}
	return InstanceID("access-v1", s.cluster)
}
func (s accessFake) Instance(cluster string) Step   { s.cluster = cluster; return s }
func (s accessFake) AccessTarget() string           { return s.cluster }
func (s accessFake) Describe(config.Cluster) string { return "access" }
func (s accessFake) Inputs(config.Cluster, Runtime) ([]byte, error) {
	return []byte(s.ID()), nil
}
func (s accessFake) Check(context.Context, config.Cluster, Runtime) (CheckResult, error) {
	if s.cluster == s.unknown {
		return CheckResult{}, errors.New("inspection unavailable")
	}
	return CheckResult{Complete: !s.pending[s.cluster], Detail: "coverage"}, nil
}
func (s accessFake) Apply(context.Context, config.Cluster, Runtime, Reporter) (ApplyResult, error) {
	*s.calls = append(*s.calls, s.cluster)
	if s.fail {
		return ApplyResult{}, errors.New("transaction failed")
	}
	s.pending[s.cluster] = false
	return ApplyResult{}, nil
}

func TestAccessPreflightAndIsolation(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "a"}, {Name: "b"}}}
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"none", Options{}},
		{"restart", Options{Confirm: Confirmation{Server: true, Clusters: []string{"a", "b"}}}},
		{"wrong token", Options{AccessConfirm: map[string]bool{"a/restart": true}}},
		{"cross db", Options{AccessConfirm: map[string]bool{"a": true}}},
		{"internal cross db", Options{InternalAccess: map[string]bool{"a": true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []string
			step := accessFake{calls: &calls, pending: map[string]bool{"a": true, "b": true}}
			tc.opts.JournalPath = filepath.Join(t.TempDir(), "journal.json")
			// Safe steps after access still run when its preflight refuses.
			data, _, err := Apply(context.Background(), []Step{step, countedStep{"safe-v1"}}, cfg, Runtime{}, tc.opts)
			var cli *output.CLIError
			if !errors.As(err, &cli) || cli.Code != output.CodeUnsafeAction || cli.ExitCode != output.ExitUnsafe {
				t.Fatalf("wrong refusal: %v", err)
			}
			if len(calls) != 0 || data.Applied != 1 {
				t.Fatalf("calls %v, safe outcome %+v", calls, data)
			}
		})
	}
}

func TestAccessPlanAndAuthorizedApply(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "a"}, {Name: "b"}}}
	var calls []string
	step := accessFake{calls: &calls, pending: map[string]bool{"a": true, "b": true}}
	path := filepath.Join(t.TempDir(), "journal.json")
	plan, err := Plan(context.Background(), []Step{step}, cfg, Runtime{DryRun: true}, Options{JournalPath: path})
	if err != nil || !slices.Equal(plan.AccessConfirmTokens, []string{"a/access", "b/access"}) || len(calls) != 0 {
		t.Fatalf("plan %+v calls %v err %v", plan, calls, err)
	}
	_, _, err = Apply(context.Background(), []Step{step}, cfg, Runtime{}, Options{
		JournalPath: path, DB: "a", InternalAccess: map[string]bool{"a": true},
	})
	if err != nil || !slices.Equal(calls, []string{"a"}) {
		t.Fatalf("calls %v err %v", calls, err)
	}
	// Existing grants drift despite a completed journal: confirmation required again.
	step.pending["a"] = true
	_, _, err = Apply(context.Background(), []Step{step}, cfg, Runtime{}, Options{JournalPath: path, DB: "a"})
	if err == nil || len(calls) != 1 {
		t.Fatalf("drift not gated: calls %v err %v", calls, err)
	}
}

func TestAccessUnknownAndFailedSQL(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "a"}, {Name: "b"}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	var calls []string
	step := accessFake{calls: &calls, pending: map[string]bool{"a": true, "b": true}, unknown: "b"}
	_, _, err := Apply(context.Background(), []Step{step}, cfg, Runtime{}, Options{
		JournalPath: path, AccessConfirm: map[string]bool{"a": true, "b": true},
	})
	if err == nil || len(calls) != 0 {
		t.Fatalf("unknown allowed access writes: calls %v err %v", calls, err)
	}
	step.unknown, step.fail = "", true
	_, _, err = Apply(context.Background(), []Step{step}, cfg, Runtime{}, Options{
		JournalPath: path, DB: "a", AccessConfirm: map[string]bool{"a": true},
	})
	if err == nil {
		t.Fatal("failed transaction reported success")
	}
	j, err := LoadJournal(path)
	if err != nil || j.Steps["access-v1@a"].Status == StatusCompleted {
		t.Fatalf("failed transaction journal: %+v, %v", j, err)
	}
}

func TestUnselectedAccessDoesNotBlockSafeStep(t *testing.T) {
	var calls []string
	step := accessFake{calls: &calls, pending: map[string]bool{"a": true}, unknown: "a"}
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "a"}}}
	data, _, err := Apply(context.Background(), []Step{step, countedStep{"safe-v1"}}, cfg, Runtime{}, Options{
		JournalPath: filepath.Join(t.TempDir(), "journal.json"), Only: []string{"safe-v1"},
	})
	if err != nil || data.Applied != 1 || len(calls) != 0 {
		t.Fatalf("unselected access blocked safe step: %+v %v %v", data, calls, err)
	}
}

func TestAccessTokenCannotRestart(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "a"}}}
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &Journal{Steps: map[string]JournalEntry{}, RestartPending: []string{"cfg-v1@a"}}
	j.Steps["cfg-v1@a"] = JournalEntry{Status: StatusCompleted, InputsHash: InputsHash("cfg-v1@a", []byte("cfg-v1@a"))}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	r := &fakeRestarter{needs: map[string]bool{"a": true}}
	_, _, err := Apply(context.Background(), []Step{templateStep{countedStep{"cfg-v1"}}}, cfg, Runtime{}, Options{
		JournalPath: path, Restarter: r, AccessConfirm: map[string]bool{"a": true},
	})
	if err != nil || len(r.restarted) != 0 {
		t.Fatalf("access token restarted %v: %v", r.restarted, err)
	}
}

func TestStepFailedPreservesTypedStepDetails(t *testing.T) {
	cause := output.NewError(output.CodeStepFailed, "policy not enforced", output.ExitStep, "retry apply").
		WithDetails(map[string]any{"reason": "cpu_policy_not_enforced", "effective_known": false})
	wrapped := stepFailed("postgres-cluster-v1@a", 1, cause)
	if wrapped.Code != output.CodeStepFailed || wrapped.ExitCode != output.ExitStep || wrapped.Hint != "retry apply" {
		t.Fatalf("typed error lost: %+v", wrapped)
	}
	details, ok := wrapped.Details.(map[string]any)
	if !ok || details["reason"] != "cpu_policy_not_enforced" || details["effective_known"] != false {
		t.Fatalf("typed details lost: %+v", wrapped.Details)
	}
}
