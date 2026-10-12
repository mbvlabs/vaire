package node

import (
	"context"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/tune"
)

func handleTuneShow(ctx context.Context, env *Env) (Result, error) {
	rt := runtimeFor(env, true)
	res := tune.Calculate(tuneInput(env.Cluster, rt.Host))
	current := map[string]string{}
	for _, s := range res.Settings {
		current[s.Name] = ""
	}
	rows, err := rt.PG.Query(ctx, "postgres", "SELECT name, current_setting(name) AS setting FROM pg_settings", nil)
	if err == nil {
		for _, row := range rows {
			if _, ok := current[row["name"]]; ok {
				current[row["name"]] = row["setting"]
			}
		}
	}
	pending, err := rt.PG.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
	var pendingNames []string
	if err == nil {
		for _, row := range pending {
			pendingNames = append(pendingNames, row["name"])
		}
	}
	return Result{Data: map[string]any{
		"inputs":          tuneInputs(env.Cluster, rt.Host),
		"settings":        res.Settings,
		"current":         current,
		"pending_restart": pendingNames,
	}}, nil
}

func handleTuneApply(ctx context.Context, env *Env) (Result, error) {
	if env.Req.DryRun {
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, true), engine.Options{Only: steps.TuneStepIDs()})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data}, nil
	}
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, runtimeFor(env, false), engine.Options{
		Only:      steps.TuneStepIDs(),
		Confirmed: env.Req.Confirm != "",
		Restarter: steps.Restarter(),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// tuneInputs is the human-readable tuning input set.
func tuneInputs(cfg config.Cluster, host hostinfo.Info) map[string]any {
	return map[string]any{
		"ram_mib":            host.RAMMiB,
		"vcpu":               host.VCPU,
		"disk_gib":           host.DataDisk.SizeGiB,
		"disk_rotational":    host.DataDisk.Rotational,
		"profile":            cfg.Postgres.Profile,
		"client_connections": cfg.Postgres.ClientConnections,
	}
}

func tuneInput(cfg config.Cluster, host hostinfo.Info) tune.Input {
	return tune.Input{
		RAMMiB:            host.RAMMiB,
		VCPU:              host.VCPU,
		DiskRotational:    host.DataDisk.Rotational,
		DataDiskGiB:       host.DataDisk.SizeGiB,
		Profile:           cfg.Postgres.Profile,
		ClientConnections: cfg.Postgres.ClientConnections,
		PGMajor:           cfg.Postgres.Version,
	}
}
