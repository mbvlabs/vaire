package node

import (
	"context"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/tune"
)

type tuneShowParams struct {
	// DB picks the cluster; empty means the only one.
	DB string `json:"db,omitempty"`
}

func handleTuneShow(ctx context.Context, env *Env) (Result, error) {
	var p tuneShowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	rt := runtimeFor(env, true)
	data := map[string]any{"server": serverBudget(env.Cluster, rt.Host)}
	if len(env.Cluster.Clusters) == 0 && p.DB == "" {
		return Result{Data: data}, nil
	}
	spec, err := onlyCluster(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	res := steps.TuningFor(env.Cluster, spec, rt.Host)
	current := map[string]string{}
	for _, s := range res.Settings {
		current[s.Name] = ""
	}
	local := steps.ClusterPG(spec)
	rows, err := local.Query(ctx, "postgres", "SELECT name, current_setting(name) AS setting FROM pg_settings", nil)
	if err == nil {
		for _, row := range rows {
			if _, ok := current[row["name"]]; ok {
				current[row["name"]] = row["setting"]
			}
		}
	}
	pending, err := local.Query(ctx, "postgres", "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
	var pendingNames []string
	if err == nil {
		for _, row := range pending {
			pendingNames = append(pendingNames, row["name"])
		}
	}
	data["database"] = spec.Name
	data["inputs"] = tuneInputs(env.Cluster, spec, rt.Host)
	data["settings"] = res.Settings
	data["current"] = current
	data["pending_restart"] = pendingNames
	return Result{Data: data}, nil
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
		Confirm:   restartConfirmation(env.Cluster, env.Req),
		Restarter: steps.Restarter(env.Cluster),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// serverBudget is the server's memory sheet (RFC 7.3): RAM, the reserve
// and what is left for clusters, and what the clusters ask for.
func serverBudget(cfg config.Cluster, host hostinfo.Info) map[string]any {
	override := 0
	if cfg.Server.MemoryReserve != "" {
		override, _ = config.ParseMemoryMiB(cfg.Server.MemoryReserve)
	}
	reserve := tune.ServerReserve(host.RAMMiB, override)
	assigned := 0
	for _, c := range cfg.Clusters {
		if r, err := cfg.Resources(c); err == nil {
			assigned += r.MemoryMiB
		}
	}
	return map[string]any{
		"ram_mib":                host.RAMMiB,
		"vcpu":                   host.VCPU,
		"reserve_mib":            reserve,
		"cluster_budget_mib":     tune.ClusterBudget(host.RAMMiB, reserve),
		"cluster_memory_mib":     assigned,
		"clusters":               len(cfg.Clusters),
		"cpu_share":              tune.CPUShare(host.VCPU, len(cfg.Clusters)),
		"huge_pages":             steps.HugePagesFor(cfg, host),
		"memory_reserve_setting": cfg.Server.MemoryReserve,
	}
}

// tuneInputs is the human-readable tuning input set for spec's cluster.
func tuneInputs(cfg config.Cluster, spec config.ClusterSpec, host hostinfo.Info) map[string]any {
	res, _ := cfg.Resources(spec)
	profile := spec.Profile
	if profile == "" {
		profile = cfg.Postgres.Profile
	}
	return map[string]any{
		"memory_mib":         res.MemoryMiB,
		"vcpu_share":         tune.CPUShare(host.VCPU, len(cfg.Clusters)),
		"max_connections":    res.MaxConnections,
		"disk_gib":           host.DataDisk.SizeGiB,
		"disk_rotational":    host.DataDisk.Rotational,
		"profile":            profile,
		"client_connections": cfg.Postgres.ClientConnections,
	}
}
