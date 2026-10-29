package cli

import (
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/contexts"
)

// A no-bucket rebuild's next commands drop followers before their sources,
// create the sources and re-follow the followers: no db create of a follower,
// whatever order the configuration lists them in (D12, R3-4).
func TestRestoreResultDataNoBucketOrdersFollowersFirst(t *testing.T) {
	source := config.ClusterSpec{Name: "app", Node: "db1", Port: 5433}
	follower := config.ClusterSpec{Name: "app_follower", Node: "db1", Port: 5434, Follows: "app"}
	want := []string{
		"vaire db drop app_follower --confirm app_follower",
		"vaire db drop app --confirm app",
		"vaire db create app",
		"vaire db follow app --name app_follower",
		"vaire doctor",
		"vaire server backup list",
	}
	for _, clusters := range [][]config.ClusterSpec{{source, follower}, {follower, source}} {
		cfg := config.DefaultCluster()
		cfg.Clusters = clusters
		r := &initRun{cfg: cfg, restore: &restoreRun{state: &contexts.RestoreProgress{}}}
		if got := r.restoreResultData().Next; !slices.Equal(got, want) {
			t.Fatalf("configuration %v:\n got %v\nwant %v", clusters, got, want)
		}
	}
}
