package pg

import "testing"

func TestIsEruRole(t *testing.T) {
	for _, role := range []string{"eru_admin", "eru_monitor", "pgbouncer_auth", "eru_write_0a1b2c3d4e5f", "eru_write_x"} {
		if !IsEruRole(role) {
			t.Errorf("IsEruRole(%q) = false", role)
		}
	}
	for _, role := range []string{"app_app", "app_ro", "app_migrate", "app_owner", "postgres", "eru_direct_only", ""} {
		if IsEruRole(role) {
			t.Errorf("IsEruRole(%q) = true", role)
		}
	}
}

func TestIsEruSession(t *testing.T) {
	cases := []struct {
		role, app string
		want      bool
	}{
		{"eru_admin", "", true},
		{"app_app", "eru-console", true},
		{"app_app", "eru top", true},
		{"app_app", "", false},
		{"app_migrate", "psql", false},
		{"postgres", "", false},
	}
	for _, c := range cases {
		if got := IsEruSession(c.role, c.app); got != c.want {
			t.Errorf("IsEruSession(%q, %q) = %v, want %v", c.role, c.app, got, c.want)
		}
	}
}
