package node

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
)

// methods is the node's method table. M3+ add entries here.
func methods() map[string]method {
	list := []method{
		{
			name:    "plan",
			lock:    fixedLock(engine.LockShared),
			cluster: true,
			runLog:  always,
			handler: handlePlan,
		},
		{
			name: "apply",
			lock: func(req rpc.Request) engine.LockMode {
				if req.DryRun {
					return engine.LockShared
				}
				return engine.LockExclusive
			},
			cluster: true,
			confirm: restartToken,
			runLog:  func(req rpc.Request) bool { return !req.DryRun },
			handler: handleApply,
		},
		{
			name:    "selfcheck",
			handler: handleSelfcheck,
		},
	}
	table := make(map[string]method, len(list))
	for _, m := range list {
		table[m.name] = m
	}
	return table
}

// restartToken is {cluster}/restart.
func restartToken(cfg config.Cluster, _ json.RawMessage) string {
	return cfg.Name() + "/restart"
}

type stepParams struct {
	Steps []string `json:"steps"`
}

// decodeParams decodes params strictly. Methods that cross versions decode
// leniently instead.
func decodeParams(raw json.RawMessage, v any) error {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return output.NewError(output.CodeUsage, "params do not decode: "+err.Error(), output.ExitUsage, "")
	}
	return nil
}

func runtimeFor(env *Env, dryRun bool) engine.Runtime {
	return engine.Runtime{
		DryRun:  dryRun,
		Shell:   engine.Shell{DryRun: dryRun, Secrets: secrets.Default, Log: env.Log},
		Secrets: secrets.Default,
		Log:     env.Log,
	}
}

func handlePlan(ctx context.Context, env *Env) (Result, error) {
	var p stepParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data}, nil
}

func handleApply(ctx context.Context, env *Env) (Result, error) {
	var p stepParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: p.Steps})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data}, nil
	}
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, false), engine.Options{
		Only: p.Steps,
		// A non-empty confirm was already checked against restartToken.
		Confirmed: env.Req.Confirm != "",
		Restarter: steps.Restarter(),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}

func handleSelfcheck(context.Context, *Env) (Result, error) {
	data, warnings, err := SelfCheck()
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}
