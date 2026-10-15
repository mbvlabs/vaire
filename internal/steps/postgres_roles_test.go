package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

// On a protected database eru_admin is no member of the owner role and
// holds no pg_signal_backend, so its password alone can only read; lifting
// the protection gives both back.
func TestAdminAccessSQL(t *testing.T) {
	open := AdminAccessSQL(config.ClusterSpec{Name: "app"})
	if !strings.Contains(open, `GRANT "app_owner" TO eru_admin WITH ADMIN OPTION;`) || !strings.Contains(open, "REVOKE pg_read_all_data FROM eru_admin") {
		t.Fatalf("unprotected:\n%s", open)
	}
	if !strings.Contains(adminGrants(config.ClusterSpec{Name: "app"}), "pg_signal_backend") {
		t.Fatal("unprotected: eru_admin must hold pg_signal_backend for maint kill")
	}

	spec := config.ClusterSpec{Name: "app", Protected: true}
	shut := AdminAccessSQL(spec)
	for _, want := range []string{
		"GRANT pg_read_all_data TO eru_admin;",
		"REVOKE pg_signal_backend FROM eru_admin;",
		"REVOKE %I FROM eru_admin GRANTED BY %I CASCADE",
		"RAISE EXCEPTION 'eru_admin is still a member of %'",
		"pg_terminate_backend(pid)",
	} {
		if !strings.Contains(shut, want) {
			t.Errorf("protected: missing %q in\n%s", want, shut)
		}
	}
	if strings.Contains(shut, "TO eru_admin WITH ADMIN OPTION") {
		t.Fatalf("protected still grants the owner role:\n%s", shut)
	}
	if strings.Contains(adminGrants(spec), "pg_signal_backend") {
		t.Fatal("protected: eru_admin must not hold pg_signal_backend")
	}
	for _, keep := range []string{"pg_monitor", "pg_maintain", "pg_use_reserved_connections"} {
		if !strings.Contains(adminGrants(spec), keep) {
			t.Errorf("protected: eru_admin lost %s", keep)
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
