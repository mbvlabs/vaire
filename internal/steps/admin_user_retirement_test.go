package steps

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestAdminUserRetiredBootstrapWithFailedJournal(t *testing.T) {
	dir := t.TempDir()
	step := adminUser{operatorsPath: filepath.Join(dir, "operators.toml"), devicesPath: filepath.Join(dir, "devices.toml")}

	var cfg config.Cluster

	cfg.Server.AdminUser = "mbv"

	ops, err := (config.Operators{}).Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(step.operatorsPath, ops, 0o600); err != nil {
		t.Fatal(err)
	}

	journal := &engine.Journal{Steps: map[string]engine.JournalEntry{step.ID(): {Status: engine.StatusFailed}}}
	opts := engine.Options{JournalPath: filepath.Join(dir, "journal.json")}

	if err := journal.Save(opts.JournalPath); err != nil {
		t.Fatal(err)
	}

	checkAdminUserPlan(t, step, cfg, opts, "apply", "previous_failure")

	registry := config.AccessRegistry{Operators: []config.AccessOperator{{ID: "op-mbv", Name: "mbv", Role: config.AccessRoleOwner, BootstrapOwner: true}}}

	raw, err := registry.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(step.devicesPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	checkAdminUserPlan(t, step, cfg, opts, "skip", "bootstrap_access_retired")

	if _, err := step.Apply(t.Context(), cfg, engine.Runtime{}, nil); err == nil {
		t.Fatal("direct apply bypassed trusted root")
	}
}

func TestAdminUserRetirementChangesBootstrapInputs(t *testing.T) {
	dir := t.TempDir()
	step := adminUser{operatorsPath: filepath.Join(dir, "operators.toml")}

	var cfg config.Cluster

	cfg.Server.AdminUser = "mbv"

	bootstrap := []byte("[[operators]]\nname = 'mbv'\nssh_public_key = 'fixture-temporary-key'\n")
	if err := os.WriteFile(step.operatorsPath, bootstrap, 0o600); err != nil {
		t.Fatal(err)
	}

	inputs, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}

	journal := &engine.Journal{Steps: map[string]engine.JournalEntry{step.ID(): {
		Status: engine.StatusCompleted, InputsHash: engine.InputsHash(step.ID(), inputs),
	}}}
	opts := engine.Options{JournalPath: filepath.Join(dir, "journal.json")}

	if err := journal.Save(opts.JournalPath); err != nil {
		t.Fatal(err)
	}

	checkAdminUserPlan(t, step, cfg, opts, "skip", "journal")

	if err := os.WriteFile(step.operatorsPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	// The pre-fix step had no retirement skip, even on a managed node.
	checkAdminUserPlan(t, struct{ engine.Step }{step}, cfg, opts, "apply", "inputs_changed")
}

func checkAdminUserPlan(t *testing.T, step engine.Step, cfg config.Cluster, opts engine.Options, action, reason string) {
	t.Helper()

	plan, err := engine.Plan(t.Context(), []engine.Step{step}, cfg, engine.Runtime{}, opts)
	if err != nil {
		t.Fatal(err)
	}

	if len(plan.Steps) != 1 || plan.Steps[0].Action != action || plan.Steps[0].Reason != reason {
		t.Fatalf("want %s (%s), got %+v", action, reason, plan.Steps)
	}
}
