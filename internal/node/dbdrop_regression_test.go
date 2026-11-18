package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

type dropConfigStore struct {
	cfg config.Cluster
}

func (s *dropConfigStore) Load() (config.Cluster, []byte, error) { return s.cfg, nil, nil }
func (s *dropConfigStore) Save(raw []byte) error {
	cfg, err := config.ParseCluster(raw)
	if err == nil {
		s.cfg = cfg
	}

	return err
}
func (*dropConfigStore) Revision() (string, error) { return "", nil }

func TestDBDropConvergesOnFilteredConfig(t *testing.T) {
	for _, remaining := range [][]string{{"keep"}, {}} {
		t.Run("remaining="+jsonNames(remaining), func(t *testing.T) {
			_, root := materialisationFixture(t)
			oldRecords := backup.RecordsPath
			backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")

			t.Cleanup(func() { backup.RecordsPath = oldRecords })

			cfg := config.DefaultCluster()
			cfg.Server.DataRoot = root

			cfg.Clusters = []config.ClusterSpec{{Name: "dropme", Size: "small"}}
			for _, name := range remaining {
				cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: name, Size: "small", Port: 5434})
			}

			store := &dropConfigStore{cfg: cfg}
			env := &Env{Cluster: cfg, Req: rpc.Request{
				Params:  json.RawMessage(`{"name":"dropme"}`),
				Confirm: "dropme",
			}}

			oldStore, oldExists, oldLeftovers := dbDropStore, dbDropClusterExists, dbDropLeftovers
			oldJournal, oldOther := dbDropForgetJournal, dbDropOtherConfigs
			oldApply, oldRouting := dbDropApplyOtherConfigs, dbDropRouting
			oldMetrics, oldClear := dbDropMetrics, dbDropClearDeferred

			t.Cleanup(func() {
				dbDropStore, dbDropClusterExists, dbDropLeftovers = oldStore, oldExists, oldLeftovers
				dbDropForgetJournal, dbDropOtherConfigs = oldJournal, oldOther
				dbDropApplyOtherConfigs, dbDropRouting = oldApply, oldRouting
				dbDropMetrics, dbDropClearDeferred = oldMetrics, oldClear
			})

			dbDropStore = func() state.Store { return store }
			dbDropClusterExists = func(context.Context, string) bool { return false }
			dbDropLeftovers = func(*Env, string) error { return nil }
			dbDropForgetJournal = func(string, string) error { return nil }
			dbDropOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, string, string) []string {
				return nil
			}
			dbDropApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
				return nil
			}
			dbDropMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
				return nil
			}
			dbDropClearDeferred = func(...string) {}

			var routed config.Cluster

			dbDropRouting = func(_ context.Context, _ *Env, got config.Cluster) error {
				routed = got
				return nil
			}

			if _, err := handleDBDrop(context.Background(), env); err != nil {
				t.Fatalf("drop: %v", err)
			}

			if len(routed.Clusters) != len(remaining) {
				t.Fatalf("routing saw %d clusters, want %d: %+v", len(routed.Clusters), len(remaining), routed.Clusters)
			}

			for _, spec := range routed.Clusters {
				if spec.Name == "dropme" {
					t.Fatalf("routing still sees the dropped database: %+v", routed.Clusters)
				}
			}

			if len(env.Cluster.Clusters) != len(remaining) {
				t.Fatalf("env.Cluster kept the dropped database: %+v", env.Cluster.Clusters)
			}
		})
	}
}

func jsonNames(names []string) string {
	if len(names) == 0 {
		return "none"
	}

	return names[0]
}

func TestDBDropUnconfiguredTargetUsesRequestedName(t *testing.T) {
	_, root := materialisationFixture(t)
	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")

	t.Cleanup(func() { backup.RecordsPath = oldRecords })

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	env := &Env{Cluster: cfg, Req: rpc.Request{Params: json.RawMessage(`{"name":"ghost"}`), Confirm: "ghost"}}

	spec, inConfig := cfg.ClusterByName("ghost")
	if inConfig {
		t.Fatal("fixture unexpectedly configured ghost")
	}

	follow, err := openDropFollowCleanup(env, cfg, "ghost", spec, inConfig)
	if err != nil {
		t.Fatalf("unconfigured drop was refused for an empty name: %v", err)
	}

	if follow != nil {
		t.Fatal("unconfigured target unexpectedly opened follower cleanup")
	}
	// The requested name must reach the ownership read; the old empty spec
	// name failed path validation before any record was examined.
	if err := refuseUnverifiedFollowerDrop("ghost", spec); err != nil {
		t.Fatalf("unconfigured ownership check: %v", err)
	}
}
