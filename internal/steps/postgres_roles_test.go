package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// An old completed journal entry must not hide an access-policy upgrade.
func TestRolesAccessPolicyUpgrade(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", Protected: true}}}
	step := postgresRoles{}.Instance("app")
	inputs, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	var current roleInputs
	if err := json.Unmarshal(inputs, &current); err != nil {
		t.Fatal(err)
	}
	// Preserve the previous release's exact field order and encoding.
	oldInputs, err := canonical(struct {
		Admin      bool           `json:"admin"`
		Monitor    bool           `json:"monitor"`
		Auth       bool           `json:"auth"`
		AuthSQL    string         `json:"auth_sql"`
		MonitorSQL string         `json:"monitor_sql"`
		DB         databaseInputs `json:"database"`
	}{current.Admin, current.Monitor, current.Auth, current.AuthSQL, current.MonitorSQL, current.DB})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		step.ID(): {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(step.ID(), oldInputs)},
	}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Plan(context.Background(), []engine.Step{postgresRoles{}}, cfg, engine.Runtime{}, engine.Options{JournalPath: path})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WouldApply != 1 || plan.Steps[0].Reason != engine.ReasonInputsChanged {
		t.Fatalf("old completed entry skipped the policy upgrade: %+v", plan)
	}
	j.Steps[step.ID()] = engine.JournalEntry{Status: engine.StatusCompleted, InputsHash: engine.InputsHash(step.ID(), inputs)}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	plan, err = engine.Plan(context.Background(), []engine.Step{postgresRoles{}}, cfg, engine.Runtime{}, engine.Options{JournalPath: path})
	if err != nil || plan.WouldApply != 0 {
		t.Fatalf("current policy should skip: %+v, %v", plan, err)
	}
}

// On a protected database vaire_admin is no member of the owner role and
// holds neither pg_signal_backend nor pg_maintain, so its password alone can
// only read; lifting the protection gives the owner membership back.
func TestAdminAccessSQL(t *testing.T) {
	open := AdminAccessSQL(config.ClusterSpec{Name: "app"})
	if !strings.Contains(open, `GRANT "app_owner" TO vaire_admin WITH ADMIN OPTION;`) || !strings.Contains(open, "REVOKE pg_read_all_data FROM vaire_admin") {
		t.Fatalf("unprotected:\n%s", open)
	}
	if !strings.Contains(adminGrants(config.ClusterSpec{Name: "app"}), "pg_signal_backend") {
		t.Fatal("unprotected: vaire_admin must hold pg_signal_backend for maint kill")
	}

	spec := config.ClusterSpec{Name: "app", Protected: true}
	shut := AdminAccessSQL(spec)
	for _, want := range []string{
		"GRANT pg_read_all_data TO vaire_admin;",
		"REVOKE pg_signal_backend FROM vaire_admin;",
		"REVOKE pg_maintain FROM vaire_admin;",
		"REVOKE %I FROM vaire_admin GRANTED BY %I CASCADE",
		"RAISE EXCEPTION 'vaire_admin is still a member of %'",
		"pg_terminate_backend(pid)",
	} {
		if !strings.Contains(shut, want) {
			t.Errorf("protected: missing %q in\n%s", want, shut)
		}
	}
	if strings.Contains(shut, "TO vaire_admin WITH ADMIN OPTION") {
		t.Fatalf("protected still grants the owner role:\n%s", shut)
	}
	if strings.Contains(adminGrants(spec), "pg_signal_backend") {
		t.Fatal("protected: vaire_admin must not hold pg_signal_backend")
	}
	if strings.Contains(adminGrants(spec), "pg_maintain") {
		t.Fatal("protected: vaire_admin must not hold pg_maintain (it could LOCK TABLE the application's tables)")
	}
	for _, keep := range []string{"pg_monitor", "pg_use_reserved_connections"} {
		if !strings.Contains(adminGrants(spec), keep) {
			t.Errorf("protected: vaire_admin lost %s", keep)
		}
	}
}

// Protecting a database or lifting its protection changes the roles step's
// inputs, so apply re-runs it.
func TestRolesInputsFollowProtection(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app", StatementTimeout: "30s"}}}
	step := postgresRoles{}.Instance("app")
	before, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	cfg.Clusters[0].Protected = true
	after, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("the inputs ignore the protection")
	}
	if got := RolesStepID("app"); got != step.ID() {
		t.Fatalf("RolesStepID = %s, want %s", got, step.ID())
	}
}

// A write role past its VALID UNTIL can't log in again through either
// pooler.
func TestPoolerAuthRefusesExpiredRoles(t *testing.T) {
	if n := strings.Count(pgbouncerAuthSQL, "(valuntil IS NULL OR valuntil > now())"); n != 2 {
		t.Fatalf("valuntil check in %d auth functions, want 2", n)
	}
}
