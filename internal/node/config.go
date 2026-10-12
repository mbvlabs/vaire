package node

import (
	"context"
	"os"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/state"
	"github.com/mbvlabs/eru/internal/steps"
)

type configSetParams struct {
	TOML         string            `json:"toml"`
	Set          map[string]string `json:"set"`
	BaseRevision string            `json:"base_revision"`
}

type configValidateParams struct {
	TOML string `json:"toml"`
}

func handleConfigGet(context.Context, *Env) (Result, error) {
	store := state.FileStore{}
	raw, err := os.ReadFile(state.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitContext, "Run eru cluster init first.")
		}
		return Result{}, err
	}
	revision, err := store.Revision()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]string{"toml": string(raw), "revision": revision}}, nil
}

func handleConfigSet(ctx context.Context, env *Env) (Result, error) {
	var p configSetParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	store := state.FileStore{}
	current, err := store.Revision()
	if err != nil {
		return Result{}, err
	}
	if p.BaseRevision != current {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"cluster.toml changed since it was read",
			output.ExitPrecondition,
			"Re-read the configuration and retry.",
		).WithDetails(map[string]any{"reason": "config_changed", "base_revision": p.BaseRevision, "revision": current})
	}
	raw, err := newConfigBytes(store, p)
	if err != nil {
		return Result{}, err
	}
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return Result{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Result{}, config.ValidationError(err)
	}
	// A dry run previews the plan for the new file without saving it.
	revision := current
	if !env.Req.DryRun {
		if err := store.Save(raw); err != nil {
			return Result{}, err
		}
		if revision, err = store.Revision(); err != nil {
			return Result{}, err
		}
	}
	env.Cluster = cfg
	plan, planErr := engine.Plan(ctx, steps.Catalog(), cfg, runtimeFor(env, true), engine.Options{})
	data := map[string]any{"revision": revision, "plan": plan, "saved": !env.Req.DryRun}
	var warnings []output.Warning
	if planErr != nil {
		warnings = append(warnings, output.Warning{Code: "plan_unavailable", Message: "the change was saved but the plan preview could not be computed: " + planErr.Error()})
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// newConfigBytes returns the desired-state bytes for a config.set request,
// either the supplied TOML or the current file with the dotted keys applied.
func newConfigBytes(store state.FileStore, p configSetParams) ([]byte, error) {
	if p.TOML != "" && len(p.Set) > 0 {
		return nil, output.NewError(output.CodeUsage, "pass either toml or set, not both", output.ExitUsage, "")
	}
	cfg, _, err := store.Load()
	if err != nil {
		return nil, err
	}
	if p.TOML != "" {
		updated, err := config.ParseCluster([]byte(p.TOML))
		if err != nil {
			return nil, output.NewError(output.CodeConfig, "the supplied cluster.toml does not parse: "+err.Error(), output.ExitConfig, "")
		}
		if changed := config.ImmutableChanges(cfg, updated); len(changed) > 0 {
			return nil, output.NewError(
				output.CodePrecondition,
				"the new cluster.toml changes keys that are immutable after init: "+strings.Join(changed, ", "),
				output.ExitPrecondition,
				"schema, name, nodes.*.wg_address and transport.* cannot be changed once a cluster exists.",
			).WithDetails(map[string]any{"reason": "immutable_key", "keys": changed})
		}
		return []byte(p.TOML), nil
	}
	for key, value := range p.Set {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, config.ValidationError(err)
	}
	return cfg.Encode()
}

func handleConfigValidate(_ context.Context, env *Env) (Result, error) {
	var p configValidateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	raw := []byte(p.TOML)
	if p.TOML == "" {
		file, err := os.ReadFile(state.Path)
		if err != nil {
			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitContext, "Run eru cluster init first.")
		}
		raw = file
	}
	problems := validateProblems(raw)
	return Result{Data: map[string]any{"valid": len(problems) == 0, "problems": problems}}, nil
}

// validateProblems returns the parse and validation problems in raw.
func validateProblems(raw []byte) []string {
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return []string{err.Error()}
	}
	if err := cfg.Validate(); err != nil {
		return []string{err.Error()}
	}
	return nil
}
