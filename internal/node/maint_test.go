package node

import (
	"context"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

// killPG records the database each of vaire maint kill's queries connects to
// and serves one candidate backend, so the test needs no PostgreSQL.
type killPG struct {
	queried []string
}

func (k *killPG) Exec(context.Context, string, string, map[string]string) error { return nil }

func (k *killPG) Query(_ context.Context, db, _ string, _ map[string]string) ([]map[string]string, error) {
	k.queried = append(k.queried, db)
	return []map[string]string{{"pid": "4242", "role": "app_app", "state": "active",
		"state_seconds": "10", "superuser": "f", "query": "select 1"}}, nil
}

// A follower's cluster holds its source's database, not the follower's name:
// candidate selection and termination connect to spec.DatabaseName(), while
// the token and the output keep the follower's name (D9, R3-1).
func TestMaintKillUsesTheClusterDatabaseName(t *testing.T) {
	oldAdmin, oldCluster := maintKillAdminPG, maintKillClusterPG
	t.Cleanup(func() { maintKillAdminPG, maintKillClusterPG = oldAdmin, oldCluster })

	for _, protected := range []bool{false, true} {
		cfg := followerCfg()
		for i := range cfg.Clusters {
			if cfg.Clusters[i].Name == "app_follower" {
				cfg.Clusters[i].Protected = protected
			}
		}
		admin, cluster := &killPG{}, &killPG{}
		maintKillAdminPG = func(config.ClusterSpec) sqlRunner { return admin }
		maintKillClusterPG = func(config.ClusterSpec) sqlRunner { return cluster }

		req := request("maint.kill", map[string]any{"db": "app_follower", "pid": 4242})
		req.Confirm = "app_follower/kill"
		res, err := handleMaintKill(context.Background(), &Env{Cluster: cfg, Req: req})
		if err != nil {
			t.Fatalf("protected %v: %v", protected, err)
		}
		if queried := append(append([]string{}, admin.queried...), cluster.queried...); !slices.Equal(queried, []string{"app", "app"}) {
			t.Fatalf("protected %v: the queries connected to %v, want app for selection and termination", protected, queried)
		}
		if protected && !slices.Equal(cluster.queried, []string{"app"}) {
			t.Fatalf("the protected follower's terminate query named %v, want app", cluster.queried)
		}
		if !protected && len(cluster.queried) != 0 {
			t.Fatalf("an unprotected follower's terminate query ran as the source's superuser: %v", cluster.queried)
		}
		data, _ := res.Data.(map[string]any)
		if data["db"] != "app_follower" {
			t.Errorf("protected %v: the output names %v, want the follower", protected, data["db"])
		}
		if got := killToken(cfg, req.Params); got != "app_follower/kill" {
			t.Errorf("protected %v: token %q, want app_follower/kill", protected, got)
		}
	}
}
