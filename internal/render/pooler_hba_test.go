package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestPoolerHBADatabaseAllowlist(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{
		{Name: "alpha", ClientAllowlist: []string{"192.0.2.9/24", "2001:db8::1/64", "invalid"}},
		{Name: "beta", ClientAllowlist: []string{"198.51.100.4/32"}},
		{Name: "all", ClientAllowlist: []string{"203.0.113.0/24"}},
		{Name: "follower", Follows: "alpha", ClientAllowlist: []string{"203.0.113.1/32"}},
		{Name: "missing", ClientAllowlist: []string{"203.0.113.2/32"}},
		{Name: "unsettled", ClientAllowlist: []string{"203.0.113.3/32"}},
		{Name: "alpha__auth", ClientAllowlist: []string{"203.0.113.4/32"}},
		{Name: "pgbouncer", ClientAllowlist: []string{"203.0.113.5/32"}},
	}}
	dbs := []PoolerDatabase{
		{Name: "alpha"}, {Name: "beta"}, {Name: "all"},
		{Name: "follower", DBName: "alpha"},
		{Name: "alpha__restoring", DBName: "alpha"},
		{Name: "unsettled", DBName: "alpha"},
		{Name: "not_configured"},
		{Name: "alpha__auth"}, {Name: "pgbouncer"},
	}

	for _, session := range []bool{false, true} {
		var hba string
		if session {
			hba = string(SessionHBA(cfg, dbs, []string{"10.42.255.1"}))
		} else {
			hba = string(PgbouncerHBA(cfg, dbs, []string{"10.42.255.1"}))
		}

		for _, want := range []string{
			`hostssl "alpha"  alpha_app  192.0.2.0/24  scram-sha-256`,
			`hostssl "alpha"  alpha_ro  2001:db8::/64  scram-sha-256`,
			`hostssl "beta"  beta_app  198.51.100.4/32  scram-sha-256`,
			`hostssl "all"  all_ro  203.0.113.0/24  scram-sha-256`,
			`hostssl all  all  10.42.255.1/32  scram-sha-256`,
			`host    all  all  0.0.0.0/0  reject`,
		} {
			if !strings.Contains(hba, want) {
				t.Errorf("session=%v missing %q:\n%s", session, want, hba)
			}
		}

		migrate := `hostssl "alpha"  alpha_migrate  192.0.2.0/24  scram-sha-256`
		if strings.Contains(hba, migrate) != session {
			t.Errorf("session=%v migrate rule mismatch:\n%s", session, hba)
		}

		for _, forbidden := range []string{
			"invalid", "follower_app", "missing_app", "unsettled_app",
			"alpha__restoring", "alpha__auth", "pgbouncer_app", "alpha_owner", "vaire_admin",
			`hostssl all  alpha_app`, `hostssl "beta"  alpha_app`,
		} {
			if strings.Contains(hba, forbidden) {
				t.Errorf("session=%v unexpected %q:\n%s", session, forbidden, hba)
			}
		}
	}
}
