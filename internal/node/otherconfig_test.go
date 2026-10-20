package node

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// fakeConfig stands in for postgres-config-v1: its inputs depend on how many
// databases share the server, as the real step's WAL sizes do. A cluster in
// restart needs a restart for its change.
type fakeConfig struct {
	cluster string
	applied *[]string
	restart map[string]bool
}

func (f fakeConfig) ID() string {
	if f.cluster == "" {
		return "postgres-config-v1"
	}
	return engine.InstanceID("postgres-config-v1", f.cluster)
}
func (f fakeConfig) Describe(config.Cluster) string { return "fake config" }
func (f fakeConfig) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return fmt.Appendf(nil, "%s:%d", f.cluster, len(cfg.Clusters)), nil
}
func (f fakeConfig) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (f fakeConfig) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	*f.applied = append(*f.applied, f.cluster)
	if f.restart[f.cluster] {
		return engine.ApplyResult{Restart: engine.RestartRestart}, nil
	}
	return engine.ApplyResult{Restart: engine.RestartReload}, nil
}
func (f fakeConfig) Instance(cluster string) engine.Step {
	return fakeConfig{cluster: cluster, applied: f.applied, restart: f.restart}
}

// noRestarts fails the test when a restart is attempted.
type noRestarts struct{ t *testing.T }

func (r noRestarts) Restart(_ context.Context, cluster string) (time.Duration, error) {
	r.t.Errorf("restarted %s", cluster)
	return 0, nil
}

func TestOtherConfigsCreateAndDrop(t *testing.T) {
	ctx := context.Background()
	journal := filepath.Join(t.TempDir(), "journal.json")
	var applied []string
	catalog := []engine.Step{fakeConfig{applied: &applied, restart: map[string]bool{"other": true}}}
	opts := engine.Options{JournalPath: journal, Restarter: noRestarts{t}}
	rt := engine.Runtime{}

	// app2 and other are configured; pending is in cluster.toml without a
	// cluster yet.
	before := budgetCluster(config.ClusterSpec{Name: "app2"}, config.ClusterSpec{Name: "other"})
	if _, _, err := engine.Apply(ctx, catalog, before, rt, opts); err != nil {
		t.Fatal(err)
	}
	applied = nil

	// db create big: the others' inputs change; pending and big don't count.
	after := budgetCluster(config.ClusterSpec{Name: "app2"}, config.ClusterSpec{Name: "other"},
		config.ClusterSpec{Name: "pending"}, config.ClusterSpec{Name: "big"})
	others := otherConfigs(ctx, catalog, after, rt, "big", journal)
	if want := []string{"postgres-config-v1@app2", "postgres-config-v1@other"}; !slices.Equal(others, want) {
		t.Fatalf("others = %v, want %v", others, want)
	}
	warnings := applyOtherConfigs(ctx, catalog, after, rt, others, opts)
	if want := []string{"app2", "other"}; !slices.Equal(applied, want) {
		t.Errorf("applied %v, want %v", applied, want)
	}
	// other's change needs a restart: pending, with its token, not done.
	var pending bool
	for _, w := range warnings {
		pending = pending || (w.Code == "restart_pending" && strings.Contains(w.Message, "--confirm other/restart"))
	}
	if !pending {
		t.Errorf("warnings %v lack restart_pending for other", warnings)
	}
	// A following apply has nothing left for them.
	if left := otherConfigs(ctx, catalog, after, rt, "big", journal); len(left) != 0 {
		t.Errorf("left after create: %v", left)
	}

	// db drop other: app2 changes again (and big, configured in between).
	applied = nil
	if _, _, err := engine.Apply(ctx, catalog, after, rt, engine.Options{JournalPath: journal, Only: []string{"postgres-config-v1@big"}, Restarter: noRestarts{t}}); err != nil {
		t.Fatal(err)
	}
	dropped := budgetCluster(config.ClusterSpec{Name: "app2"}, config.ClusterSpec{Name: "pending"}, config.ClusterSpec{Name: "big"})
	others = otherConfigs(ctx, catalog, dropped, rt, "", journal)
	if want := []string{"postgres-config-v1@app2", "postgres-config-v1@big"}; !slices.Equal(others, want) {
		t.Fatalf("others after drop = %v, want %v", others, want)
	}
}

func TestCreateRunsNumbering(t *testing.T) {
	cfg := budgetCluster(config.ClusterSpec{Name: "app2"}, config.ClusterSpec{Name: "big"})
	runs := createRuns(cfg, "big", []string{"postgres-config-v1@app2"})
	if len(runs) != 5 {
		t.Fatalf("runs = %d, want kernel, big, poolers, metrics and app2's config (no backups)", len(runs))
	}
	if !slices.Equal(runs[3].Only, []string{"metrics-v1"}) || !slices.Equal(runs[4].Only, []string{"postgres-config-v1@app2"}) || runs[1].DB != "big" {
		t.Errorf("runs = %+v", runs)
	}
	if got := dropRuns(budgetCluster(), nil); len(got) != 2 {
		t.Errorf("dropping the last database: %d runs, want the timers and metrics", len(got))
	}
}
