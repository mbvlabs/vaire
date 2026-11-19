package render

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestPoolerHBARoleAllowlist(t *testing.T) {
	cfg := config.Cluster{
		Network: config.Network{RoleAllowlist: &config.RoleAllowlist{
			App: []string{"192.0.2.7/24"}, Migrate: []string{"2001:db8::/32"},
		}},
		Transport: config.Transport{Subnet: "10.42.0.0/16"},
	}
	dbs := []PoolerDatabase{{Name: "follower", DBName: "source"}, {Name: "other"}, {Name: "source__restoring", DBName: "source"}}

	for _, session := range []bool{false, true} {
		var hba string
		if session {
			hba = string(SessionHBA(cfg, dbs, []string{"10.42.255.1"}))
		} else {
			hba = string(PgbouncerHBA(cfg, dbs, []string{"10.42.255.1"}))
		}

		for _, want := range []string{
			"hostssl all  source_app  192.0.2.0/24  scram-sha-256",
			"hostssl all  all  10.42.255.1/32  scram-sha-256",
			"host    all  all  ::/0       reject",
		} {
			if !strings.Contains(hba, want) {
				t.Errorf("session=%v missing %q:\n%s", session, want, hba)
			}
		}

		if strings.Contains(hba, "follower_app") || strings.Count(hba, "source_app  192.0.2.0/24") != 1 {
			t.Errorf("follower or duplicate source roles: %s", hba)
		}

		if strings.Contains(hba, "hostnossl") || strings.Contains(hba, "hostssl all  all  0.0.0.0/0") {
			t.Errorf("unexpected broad public access: %s", hba)
		}

		migrate := "hostssl all  source_migrate  2001:db8::/32  scram-sha-256"
		if strings.Contains(hba, migrate) != session {
			t.Errorf("session=%v migrate rule mismatch:\n%s", session, hba)
		}

		if strings.Contains(hba, "hostssl all  source_ro ") ||
			strings.Index(hba, "all  10.42.255.1/32") > strings.Index(hba, "all  ::/0       reject") {
			t.Errorf("expected private access but no public read-only rule: %s", hba)
		}
	}
}

func TestPoolerHBASharedMode(t *testing.T) {
	hba := string(PgbouncerHBA(config.Cluster{Network: config.Network{ClientAllowlist: []string{"192.0.2.0/24"}}}, nil, nil))
	if !strings.Contains(hba, "hostssl all  all  192.0.2.0/24  scram-sha-256") || strings.Contains(hba, "hostnossl") {
		t.Fatal(hba)
	}
}
