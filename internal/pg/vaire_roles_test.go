package pg

import (
	"strings"
	"testing"
)

func TestIsVaireRole(t *testing.T) {
	for _, role := range []string{"vaire_admin", "vaire_monitor", "pgbouncer_auth", "vaire_write_0a1b2c3d4e5f", "vaire_write_x", "postgres", "pg_maintain", "pg_signal_backend"} {
		if !IsVaireRole(role) {
			t.Errorf("IsVaireRole(%q) = false", role)
		}
	}
	for _, role := range []string{"app_app", "app_ro", "app_migrate", "app_owner", "postgres_app", "pg_maintain_owner", "vaire_direct_only", ""} {
		if IsVaireRole(role) {
			t.Errorf("IsVaireRole(%q) = true", role)
		}
	}
}

func TestVaireRoleSQLAdministrativeRoles(t *testing.T) {
	sql := VaireRoleSQL("r.rolname")
	for _, role := range []string{"vaire_admin", "vaire_monitor", "pgbouncer_auth", "postgres", "pg_maintain", "pg_signal_backend"} {
		if !strings.Contains(sql, "'"+role+"'") {
			t.Errorf("SQL filter omits %s: %s", role, sql)
		}
	}
	if !strings.Contains(sql, "starts_with(r.rolname, 'vaire_write_')") {
		t.Errorf("SQL filter omits short-lived write roles: %s", sql)
	}
}

func TestIsVaireSession(t *testing.T) {
	cases := []struct {
		role, app string
		want      bool
	}{
		{"vaire_admin", "", true},
		{"app_app", "vaire-console", true},
		{"app_app", "vaire top", true},
		{"app_app", "", false},
		{"app_migrate", "psql", false},
		{"postgres", "", true},
		{"pg_maintain", "psql", true},
		{"pg_signal_backend", "", true},
	}
	for _, c := range cases {
		if got := IsVaireSession(c.role, c.app); got != c.want {
			t.Errorf("IsVaireSession(%q, %q) = %v, want %v", c.role, c.app, got, c.want)
		}
	}
}
