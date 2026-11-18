package node

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func TestHandleApplyEngineFailurePreservesPartialDataAndWarnings(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	newExtHarness(t, cfg)

	oldReload, oldEngine, oldTransitions := applyReloadCluster, applyEngine, applyTransitions
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	applyTransitions = func(context.Context, *Env, stepParams) ([]output.Warning, func(), error) {
		return nil, nil, nil
	}
	partial := engine.ApplyData{
		Steps: []engine.ApplyStep{{
			ID: "postgres-cluster-v1@app", Cluster: "app", Index: 1, State: engine.StatusCompleted,
		}},
		Applied: 1, Total: 3,
	}
	applyEngine = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return partial, []output.Warning{{Code: "engine_warning", Message: "the cluster step completed before the refusal"}},
			output.NewError(output.CodeStepFailed, "configuration refused", output.ExitStep, "fix the extension state, then retry apply").
				WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": "app"})
	}

	t.Cleanup(func() { applyReloadCluster, applyEngine, applyTransitions = oldReload, oldEngine, oldTransitions })

	_, err := handleApply(context.Background(), &Env{
		Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockExclusive,
	})
	if err == nil {
		t.Fatal("engine partial failure was reported as success")
	}

	details := extDetails(t, err)
	if details["reason"] != "extension_unload_unproven" {
		t.Fatalf("existing typed details were lost: %v", details)
	}

	got, ok := details["apply_data"].(engine.ApplyData)
	if !ok || got.Steps[0].ID != partial.Steps[0].ID || got.Applied != 1 || got.Total != 3 {
		t.Fatalf("engine partial data = %#v (details %v)", details["apply_data"], details)
	}

	failure := output.Fail(nil, err)
	if !slices.ContainsFunc(failure.Warnings, func(w output.Warning) bool { return w.Code == "engine_warning" }) {
		t.Fatalf("engine warnings were discarded: %+v", failure.Warnings)
	}
}

func TestHandleApplyTransitionFailurePreservesWarnings(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	newExtHarness(t, cfg)

	oldReload, oldTransitions := applyReloadCluster, applyTransitions
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	applyTransitions = func(context.Context, *Env, stepParams) ([]output.Warning, func(), error) {
		return []output.Warning{{Code: "transition_warning", Message: "an earlier target started"}},
			nil, errors.New("transition failed after an effect")
	}

	t.Cleanup(func() { applyReloadCluster, applyTransitions = oldReload, oldTransitions })

	_, err := handleApply(context.Background(), &Env{
		Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockExclusive,
	})
	if err == nil {
		t.Fatal("transition failure was reported as success")
	}

	failure := output.Fail(nil, err)
	if !slices.ContainsFunc(failure.Warnings, func(w output.Warning) bool { return w.Code == "transition_warning" }) {
		t.Fatalf("transition warnings were discarded: %+v", failure.Warnings)
	}
}
