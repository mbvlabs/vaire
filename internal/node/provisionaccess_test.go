package node

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
)

type provisionState struct {
	roles, installed bool
	fail             bool
	events           []string
}

func TestCreateAndCopyAccessFailurePreventsPoolerExposure(t *testing.T) {
	n := newCopyTestNode(t)
	cfg := n.saved()
	spec := config.ClusterSpec{Name: "copyapp", Node: "db1", Port: 5434, Stanza: "copyapp-abcdef", Size: "small", StatementTimeout: "60s"}
	cfg.Clusters = append(cfg.Clusters, spec)
	previous, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	oldKernel, oldNew, oldPooler, oldExists := createApplyKernel, createApplyNewCluster, createReloadPoolers, createClusterExists
	t.Cleanup(func() {
		createApplyKernel, createApplyNewCluster, createReloadPoolers, createClusterExists = oldKernel, oldNew, oldPooler, oldExists
	})
	createApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, nil
	}
	createClusterExists = func(context.Context, string) bool { return true }
	var target string
	createApplyNewCluster = func(_ context.Context, _ config.Cluster, _ engine.Runtime, name, _ string, _ engine.Reporter) error {
		target = name
		return errors.New("owned access failed")
	}
	exposed := 0
	createReloadPoolers = func(context.Context, *Env, config.Cluster) error {
		exposed++
		return nil
	}
	for _, copyMode := range []bool{false, true} {
		env := &Env{Cluster: cfg, Req: rpc.Request{}}
		_, _, err := provisionDatabase(context.Background(), env, cfg, previous, spec, createOptions{deferInitialBackup: copyMode})
		if err == nil || target != spec.Name || exposed != 0 {
			t.Fatalf("copy=%v target=%q exposure=%d err=%v", copyMode, target, exposed, err)
		}
	}
}

func TestForkAccessFailurePreventsPoolerExposure(t *testing.T) {
	runOwnedForkFixture(t, "access")
}

type provisionStep struct {
	id, cluster string
	state       *provisionState
}

func (s provisionStep) ID() string {
	if s.cluster == "" {
		return s.id
	}
	return engine.InstanceID(s.id, s.cluster)
}
func (s provisionStep) Instance(cluster string) engine.Step { s.cluster = cluster; return s }
func (s provisionStep) Describe(config.Cluster) string      { return s.id }
func (s provisionStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte(s.ID()), nil
}
func (s provisionStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (s provisionStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	s.state.events = append(s.state.events, s.id)
	if s.id == "postgres-roles-v1" {
		s.state.roles = true
	}
	return engine.ApplyResult{}, nil
}

type provisionAccess struct{ provisionStep }

func (s provisionAccess) Instance(cluster string) engine.Step {
	s.cluster = cluster
	return s
}
func (s provisionAccess) AccessTarget() string { return s.cluster }
func (s provisionAccess) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	s.state.events = append(s.state.events, "inspect")
	if !s.state.roles {
		return engine.CheckResult{}, errors.New("roles do not exist")
	}
	return engine.CheckResult{Complete: s.state.installed}, nil
}
func (s provisionAccess) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	s.state.events = append(s.state.events, "grant")
	if s.state.fail {
		return engine.ApplyResult{}, errors.New("transaction rolled back")
	}
	s.state.installed = true
	return engine.ApplyResult{}, nil
}

func TestExistingSettlementExcludesAccessAndOtherTargets(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
	state := &provisionState{}
	catalog := []engine.Step{
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		provisionStep{id: "after-v1", state: state},
	}
	got := existingSettlementSteps(catalog, cfg, "app")
	want := []string{"postgres-roles-v1@app", "after-v1@app"}
	if !slices.Equal(got, want) {
		t.Fatalf("existing target settlement = %v, want %v", got, want)
	}
	_, _, err := engine.Apply(context.Background(), catalog, cfg, engine.Runtime{}, engine.Options{
		Only: got, DB: "app", JournalPath: filepath.Join(t.TempDir(), "journal.json"),
	})
	if err != nil || !slices.Equal(state.events, []string{"postgres-roles-v1", "after-v1"}) {
		t.Fatalf("existing settlement inspected or widened access: %v, %v", state.events, err)
	}
}

func TestOwnedProvisionStagesAndExactAuthorization(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
	state := &provisionState{}
	catalog := []engine.Step{
		provisionStep{id: "postgres-config-v1", state: state},
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		provisionStep{id: "after-v1", state: state},
	}
	path := filepath.Join(t.TempDir(), "journal.json")
	var stages [][]string
	apply := func(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		stages = append(stages, slices.Clone(opts.Only))
		if opts.DB != "app" || opts.Confirm.Server || !opts.Confirm.Covers("app") || opts.Confirm.Covers("other") {
			t.Fatalf("scope: %+v", opts)
		}
		for target, allowed := range opts.InternalAccess {
			if !allowed || target != "app" || len(opts.InternalAccess) != 1 {
				t.Fatalf("internal authorization crossed targets: %v", opts.InternalAccess)
			}
		}
		if len(stages) == 2 && !opts.InternalAccess["app"] {
			t.Fatal("access stage not internally authorized")
		}
		opts.JournalPath = path
		return engine.Apply(ctx, catalog, cfg, rt, opts)
	}
	err := provisionOwnedTargetWith(context.Background(), cfg, engine.Runtime{}, "app", "", nil, catalog, apply, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(stages) != 3 || !slices.Equal(stages[0], []string{"postgres-config-v1@app", "postgres-roles-v1@app"}) ||
		!slices.Equal(stages[1], []string{"postgres-access-v1@app"}) || !slices.Equal(stages[2], []string{"after-v1@app"}) {
		t.Fatalf("stages: %v", stages)
	}
	if !state.installed || !slices.Equal(state.events, []string{"postgres-config-v1", "postgres-roles-v1", "inspect", "inspect", "grant", "after-v1"}) {
		t.Fatalf("events: %v", state.events)
	}
}

func TestOwnedProvisionFailureStopsBeforeExposure(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "standby", Follows: "app"}}}
	state := &provisionState{fail: true}
	catalog := []engine.Step{
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		provisionStep{id: "after-v1", state: state},
	}
	path := filepath.Join(t.TempDir(), "journal.json")
	apply := func(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		opts.JournalPath = path
		return engine.Apply(ctx, catalog, cfg, rt, opts)
	}
	exposed := false
	err := provisionOwnedTargetWith(context.Background(), cfg, engine.Runtime{}, "app", "", nil, catalog, apply, nil)
	if err == nil {
		exposed = true // callers expose only on success
	}
	if err == nil || exposed || state.installed || slices.Contains(state.events, "after-v1") {
		t.Fatalf("failure exposed target: %v, %v", state.events, err)
	}
	j, err := engine.LoadJournal(path)
	if err != nil || j.Steps["postgres-access-v1@app"].Status == engine.StatusCompleted {
		t.Fatalf("failed grant journal: %+v, %v", j, err)
	}
	if err := provisionOwnedTargetWith(context.Background(), cfg, engine.Runtime{}, "standby", "", nil, catalog, apply, nil); err == nil {
		t.Fatal("physical follower accepted for local access writes")
	}
	if err := provisionOwnedTargetWith(context.Background(), cfg, engine.Runtime{}, "app", "other", nil, catalog, apply, nil); err == nil {
		t.Fatal("different recovery target accepted")
	}
}

type bootstrapGlobal struct{ id string }

func (s bootstrapGlobal) ID() string                     { return s.id }
func (s bootstrapGlobal) Describe(config.Cluster) string { return s.id }
func (s bootstrapGlobal) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s bootstrapGlobal) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}
func (s bootstrapGlobal) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, nil
}

func TestBootstrapStagesOwnedTargetsBeforeSharedExposure(t *testing.T) {
	cfg := config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}, {Name: "other"}}}
	state := &provisionState{}
	catalog := []engine.Step{
		bootstrapGlobal{id: "kernel-tuning-v1"},
		provisionStep{id: "postgres-roles-v1", state: state},
		provisionAccess{provisionStep{id: "postgres-access-v1", state: state}},
		bootstrapGlobal{id: "pgbouncer-v1"},
	}
	oldView := engine.ClusterView
	engine.ClusterView = nil
	t.Cleanup(func() { engine.ClusterView = oldView })
	var runs []string
	runner := func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		runs = append(runs, strings.Join(opts.Only, ","))
		for name := range opts.InternalAccess {
			if name != opts.DB || len(opts.InternalAccess) != 1 {
				t.Fatalf("internal access crosses target: %+v", opts)
			}
		}
		return engine.ApplyData{}, nil, nil
	}
	owner := &bootstrapOwnership{Targets: map[string]bool{"app": false, "other": false}}
	complete := func(target string) error { owner.Targets[target] = true; return nil }
	_, _, err := bootstrapDatabaseSteps(context.Background(), cfg, engine.Runtime{}, nil, catalog, runner, nil, "", owner, complete)
	want := []string{"kernel-tuning-v1", "postgres-roles-v1@app", "postgres-access-v1@app", "postgres-roles-v1@other", "postgres-access-v1@other", "pgbouncer-v1"}
	if err != nil || !slices.Equal(runs, want) {
		t.Fatalf("bootstrap stages %v, err %v", runs, err)
	}
	runs = nil
	owner.Targets["app"], owner.Targets["other"] = false, false
	fail := func(ctx context.Context, c []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		if slices.Contains(opts.Only, "postgres-access-v1@other") {
			return engine.ApplyData{}, nil, errors.New("access failed")
		}
		return runner(ctx, c, cfg, rt, opts)
	}
	_, _, err = bootstrapDatabaseSteps(context.Background(), cfg, engine.Runtime{}, nil, catalog, fail, nil, "", owner, complete)
	if err == nil || slices.Contains(runs, "pgbouncer-v1") {
		t.Fatalf("bootstrap exposed a failed target: %v, %v", runs, err)
	}
}
