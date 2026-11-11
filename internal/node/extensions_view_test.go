package node

import (
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func TestEngineViewKeepsOwnedRecoveryAndFollowTarget(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}, {Name: "other", Port: 5434}}
	recs := backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}
	view := withoutUnfinished(cfg, recs, "")
	if _, ok := view.ClusterByName("app"); ok || !slices.ContainsFunc(view.Awaiting, func(s config.ClusterSpec) bool { return s.Name == "app" }) {
		t.Fatalf("awaiting app was not excluded: %+v", view)
	}
	kept := withoutUnfinished(cfg, recs, "app")
	if _, ok := kept.ClusterByName("app"); !ok {
		t.Fatalf("owned recovery target was excluded: %+v", kept)
	}
	if !hasExpandedStep(kept, steps.RolesStepID("app")) {
		t.Fatal("owned recovery target lost its roles step")
	}

	follow := config.ClusterSpec{Name: "app_follower", Port: 5435, Follows: "app", FollowGeneration: "0123456789abcdef0123456789abcdef"}
	followCfg := config.DefaultCluster()
	followCfg.Clusters = []config.ClusterSpec{follow}
	followView := withoutUnfinished(followCfg, backup.Records{}, "")
	if _, ok := followView.ClusterByName("app_follower"); ok ||
		!slices.ContainsFunc(followView.Awaiting, func(s config.ClusterSpec) bool { return s.Name == "app_follower" }) {
		t.Fatalf("unfinished follow was not excluded: %+v", followView)
	}
	followKept := withoutUnfinished(followCfg, backup.Records{}, "app_follower")
	if _, ok := followKept.ClusterByName("app_follower"); !ok {
		t.Fatalf("owned follow target was excluded: %+v", followKept)
	}
	if !hasExpandedStep(followKept, steps.ConfigStepID("app_follower")) {
		t.Fatal("owned follow target lost its config step")
	}
}

func hasExpandedStep(cfg config.Cluster, id string) bool {
	for _, step := range engine.Expand(steps.Catalog(), cfg) {
		if step.ID() == id {
			return true
		}
	}
	return false
}
