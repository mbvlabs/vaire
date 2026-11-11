package node

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestNewPrestartMaterialisedAttachesHook(t *testing.T) {
	old := newMaterialised
	newMaterialised = func(string, string, string, int, engine.Shell, string, engine.Reporter) (*backup.Materialised, error) {
		return &backup.Materialised{}, nil
	}
	t.Cleanup(func() { newMaterialised = old })
	env := &jobEnv{Cluster: config.DefaultCluster()}
	spec := config.ClusterSpec{Name: "app"}
	m, err := newPrestartMaterialised("/data", "copy", "app", 5433, env, env.Cluster, spec, nil)
	if err != nil || m == nil || m.BeforeStart == nil {
		t.Fatalf("newPrestartMaterialised = %+v, %v", m, err)
	}
}

func TestPrestartFollowerCopyAttachesHook(t *testing.T) {
	m := &backup.Materialised{}
	env := &jobEnv{Cluster: config.DefaultCluster()}
	spec := config.ClusterSpec{Name: "app_follower", Follows: "app"}
	prestartFollowerCopy(m, env, env.Cluster, spec, nil)
	if m.BeforeStart == nil {
		t.Fatal("follower copy lacks the extension prestart hook")
	}
}

func TestPrestartHookRefusesMissingPrerequisites(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"nope"}}}
	env := &jobEnv{Cluster: cfg}
	spec := cfg.Clusters[0]
	m := &backup.Materialised{}
	prestartCopy(m, env, cfg, spec, nil)
	if m.BeforeStart == nil {
		t.Fatal("prestartCopy did not attach a hook")
	}
	if err := m.BeforeStart(context.Background()); err == nil {
		t.Fatal("a missing extension prerequisite did not stop the prestart hook")
	}
}

func TestRestartFollowerPreflightFailureStopsBeforeRestart(t *testing.T) {
	old := prepareTargetExtensions
	prepareTargetExtensions = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime, engine.Reporter) error {
		return errors.New("missing extension prerequisite")
	}
	t.Cleanup(func() { prepareTargetExtensions = old })
	env := &jobEnv{Cluster: config.DefaultCluster()}
	err := restartFollower(context.Background(), env, env.Cluster, config.ClusterSpec{Name: "app_follower"})
	if err == nil || !strings.Contains(err.Error(), "missing extension prerequisite") {
		t.Fatalf("restartFollower preflight = %v", err)
	}
}
