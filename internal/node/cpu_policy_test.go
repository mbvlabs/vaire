package node

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/state"
)

func TestCPUOnlyResizePreservesFields(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	old := cfg.Clusters[1]
	old.Memory = "768MiB"
	old.CPUQuota = "75%"
	old.Protected = true
	cfg.Clusters[1] = old
	env, store := resizeFixture(t, cfg, "", false)
	before, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	old, _ = before.ClusterByName(old.Name)
	env.Req = request("db.resize", map[string]any{"name": old.Name, "cpu_mode": "spare", "cpu_quota": "none"})
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
	previousCheck, previousApply, previousAdmission := checkSpareCPU, applyResizeStep, checkCPUOnlyResources
	t.Cleanup(func() {
		checkSpareCPU, applyResizeStep, checkCPUOnlyResources = previousCheck, previousApply, previousAdmission
	})
	checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { return nil }
	checks, calls := 0, 0
	checkSpareCPU = func(_ context.Context, name, mode string) error {
		checks++
		if name != old.Name || mode != "spare" {
			t.Fatal(name, mode)
		}
		return nil
	}
	applyResizeStep = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		calls++
		if opts.DB != old.Name || !opts.LeaveRestartsPending || !slices.Equal(opts.Only, []string{"postgres-cluster-v1@" + old.Name}) {
			t.Fatalf("%+v", opts)
		}
		return engine.ApplyData{}, nil, nil
	}
	if _, err := handleDBResize(context.Background(), env); err != nil {
		t.Fatal(err)
	}
	saved, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := saved.ClusterByName(old.Name)
	old.CPUMode, old.CPUQuota = "spare", ""
	if !reflect.DeepEqual(got, old) || checks != 1 || calls != 1 {
		t.Fatalf("got %+v want %+v checks %d calls %d", got, old, checks, calls)
	}
}

func TestCPUOnlyResizeDryRunAndRefusal(t *testing.T) {
	for _, refused := range []bool{false, true} {
		cfg := followerLifecycleCfg(t, "app_follower")
		env, store := resizeFixture(t, cfg, "", true)
		env.Req = request("db.resize", map[string]any{"name": "app_follower", "cpu_mode": "spare"})
		env.Req.DryRun = true
		fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
		oldCheck, oldPlan, oldAdmission := checkSpareCPU, planCPUResize, checkCPUOnlyResources
		t.Cleanup(func() { checkSpareCPU, planCPUResize, checkCPUOnlyResources = oldCheck, oldPlan, oldAdmission })
		checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { return nil }
		checkSpareCPU = func(context.Context, string, string) error {
			if refused {
				return errors.New("unsupported")
			}
			return nil
		}
		plans := 0
		planCPUResize = func(_ context.Context, _ []engine.Step, _ config.Cluster, _ engine.Runtime, opts engine.Options) (engine.PlanData, error) {
			plans++
			if !opts.LeaveRestartsPending || !slices.Equal(opts.Only, []string{"postgres-cluster-v1@app_follower"}) {
				t.Fatal(opts.Only)
			}
			return engine.PlanData{}, nil
		}
		_, before, err := store.Load()
		if err != nil {
			t.Fatal(err)
		}
		_, err = handleDBResize(context.Background(), env)
		if (err != nil) != refused || plans != map[bool]int{true: 0, false: 1}[refused] {
			t.Fatal(err, plans)
		}
		_, after, err := store.Load()
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("dry run saved", err)
		}
	}
}

func TestCPUProposalRefusesBeforeSave(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "dotted", true: "toml"}[full], func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			env, store := resizeFixture(t, cfg, "", false)
			oldStore, oldCheck := configSetStore, checkSpareCPU
			t.Cleanup(func() { configSetStore, checkSpareCPU = oldStore, oldCheck })
			configSetStore = func() state.FileStore { return store }
			checkSpareCPU = func(context.Context, string, string) error { return errors.New("screen refused") }
			revision, err := store.Revision()
			if err != nil {
				t.Fatal(err)
			}
			_, before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"base_revision": revision}
			if full {
				cfg.Clusters[1].CPUMode = "spare"
				raw, err := cfg.Encode()
				if err != nil {
					t.Fatal(err)
				}
				params["toml"] = string(raw)
			} else {
				params["set"] = map[string]string{"clusters.app_follower.cpu_mode": "spare"}
			}
			env.Req = request("config.set", params)
			if _, err := handleConfigSet(context.Background(), env); err == nil || err.Error() != "screen refused" {
				t.Fatal(err)
			}
			_, after, err := store.Load()
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("configuration changed", err)
			}
		})
	}
}

func TestCPUProposalScreensResolvedWeightChange(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[1].Size, cfg.Clusters[1].CPUMode = "", "spare"
	next := cfg
	next.Postgres.DefaultSize = "large"
	oldCheck := checkSpareCPU
	t.Cleanup(func() { checkSpareCPU = oldCheck })
	calls := 0
	checkSpareCPU = func(_ context.Context, name, mode string) error {
		calls++
		if name != "app_follower" || mode != "spare" {
			t.Fatal(name, mode)
		}
		return nil
	}
	if err := checkCPUProposal(context.Background(), cfg, next); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestCPUProposalScreensSameValueExplicitSpare(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[1].CPUMode = "spare"
	oldCheck := checkSpareCPU
	t.Cleanup(func() { checkSpareCPU = oldCheck })
	calls := 0
	checkSpareCPU = func(context.Context, string, string) error { calls++; return nil }
	if err := checkCPUProposalWithRequests(context.Background(), cfg, cfg, map[string]string{
		"clusters.app_follower.cpu_mode": "spare",
	}); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
}

func TestCPUFailureReportsSavedState(t *testing.T) {
	original := output.NewError(output.CodeStepFailed, "not enforced", output.ExitStep, "").
		WithDetails(map[string]any{"reason": "cpu_policy_not_enforced", "desired_saved": nil})
	var result *output.CLIError
	if !errors.As(cpuPolicySaved(original), &result) || result.Details.(map[string]any)["desired_saved"] != true ||
		original.Details.(map[string]any)["desired_saved"] != nil {
		t.Fatal(result)
	}
}

func TestFollowCPUOptionsAndForkRetention(t *testing.T) {
	src := config.ClusterSpec{Name: "source", CPUMode: "spare", CPUQuota: "120%", CPUWeight: 71}
	if forkSpec(src, "fork").CPUMode != "spare" {
		t.Fatal("fork lost CPU mode")
	}
	spec := followerSpec(src, "follower", "")
	if spec.CPUMode != "" || spec.CPUQuota != "120%" || spec.CPUWeight != 71 {
		t.Fatal(spec)
	}
	mode, quota := "spare", "none"
	p := dbFollowParams{Source: "source", CPUMode: &mode, CPUQuota: &quota}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var round dbFollowParams
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if err := followCPU(&spec, round); err != nil || spec.CPUMode != "spare" || spec.CPUQuota != "" {
		t.Fatal(spec, err)
	}
	mode = ""
	if err := followCPU(&spec, p); err == nil {
		t.Fatal("empty explicit mode accepted")
	}
}

func TestFollowSpareRefusedBeforeSave(t *testing.T) {
	for _, dry := range []bool{false, true} {
		cfg := followerLifecycleCfg(t)
		node := newFollowerNode(t, cfg)
		fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
		oldCheck, oldExists := checkSpareCPU, followClusterExists
		t.Cleanup(func() { checkSpareCPU, followClusterExists = oldCheck, oldExists })
		followClusterExists = func(context.Context, string) bool { return false }
		checkSpareCPU = func(context.Context, string, string) error { return errors.New("unavailable") }
		_, before, err := node.store.Load()
		if err != nil {
			t.Fatal(err)
		}
		req := request("db.follow", map[string]any{"source": "app", "name": "app_follower", "cpu_mode": "spare"})
		req.DryRun = dry
		_, err = handleDBFollow(context.Background(), &Env{Cluster: cfg, Req: req})
		if err == nil || err.Error() != "unavailable" {
			t.Fatal(err)
		}
		_, after, err := node.store.Load()
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("follow saved despite refusal", err)
		}
	}
}

func TestUnfollowPreservesCPUPolicy(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[1].CPUMode, cfg.Clusters[1].CPUQuota = "spare", "150%"
	_, store := resizeFixture(t, cfg, "", false)
	if _, err := independent("app_follower", "app_follower-abc123"); err != nil {
		t.Fatal(err)
	}
	saved, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	spec, ok := saved.ClusterByName("app_follower")
	if !ok || spec.Follows != "" || spec.CPUMode != "spare" || spec.CPUQuota != "150%" {
		t.Fatal(spec)
	}
}
