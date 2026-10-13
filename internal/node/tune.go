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
	// DB picks the cluster; empty means the only one, or the server sheet
	// alone when there are none or several.
	DB string `json:"db,omitempty"`
}

func handleTuneShow(ctx context.Context, env *Env) (Result, error) {
	var p tuneShowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	rt := runtimeFor(env, true)
	data := map[string]any{"server": serverBudget(env.Cluster, rt.Host)}
	warnings := oversubscriptionWarnings(env.Cluster, rt.Host)
	if p.DB == "" && len(env.Cluster.Clusters) != 1 {
		return Result{Data: data, Warnings: warnings}, nil
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
	return Result{Data: data, Warnings: warnings}, nil
}

func handleTuneApply(ctx context.Context, env *Env) (Result, error) {
	if env.Req.DryRun {
		rt := runtimeFor(env, true)
		data, err := engine.Plan(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{Only: steps.TuneStepIDs()})
		if err != nil {
			return Result{}, err
		}
		return Result{Data: data, Warnings: oversubscriptionWarnings(env.Cluster, rt.Host)}, nil
	}
	rt := runtimeFor(env, false)
	data, warnings, err := engine.Apply(ctx, steps.Catalog(), env.Cluster, rt, engine.Options{
		Only:      steps.TuneStepIDs(),
		Confirm:   restartConfirmation(env.Cluster, env.Req),
		Restarter: steps.Restarter(env.Cluster),
		Report:    env.Report,
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Warnings: append(warnings, oversubscriptionWarnings(env.Cluster, rt.Host)...)}, nil
}

// serverBudget is the server's budget sheet (RFC 7.5): RAM, the reserve
// and what is left for clusters, each cluster's size and limits, what is
// allocated and free, what CommitLimit leaves, and whether IOWeight= has any
// effect on the data disk.
func serverBudget(cfg config.Cluster, host hostinfo.Info) map[string]any {
	h, sheet := budgetSheet(cfg, host)
	clusters := make([]map[string]any, 0, len(sheet.Clusters))
	for i, d := range sheet.Clusters {
		res, _ := cfg.Resources(cfg.Clusters[i])
		row := map[string]any{
			"name":              d.Name,
			"size":              d.Size,
			"memory_mib":        d.MemoryMiB,
			"memory_high_mib":   res.MemoryMiB + res.MemoryMiB/4,
			"cpu_weight":        d.CPUWeight,
			"io_weight":         res.IOWeight,
			"max_connections":   res.MaxConnections,
			"shared_memory_mib": d.SharedMemoryMiB,
			"huge_pages":        d.HugePages,
			"running":           d.Running,
		}
		if res.MemoryMaxMiB > 0 {
			row["memory_max_mib"] = res.MemoryMaxMiB
		}
		if res.CPUQuota != "" {
			row["cpu_quota"] = res.CPUQuota
		}
		if mib, ok := clusterMemoryCurrentMiB(d.Name); ok {
			row["memory_current_mib"] = mib
		}
		clusters = append(clusters, row)
	}
	ioEnforced, ioReason := hostinfo.IOWeight(host.DataDisk.Device)
	out := map[string]any{
		"ram_mib":                host.RAMMiB,
		"vcpu":                   host.VCPU,
		"reserve_mib":            sheet.ReserveMiB,
		"cluster_budget_mib":     sheet.BudgetMiB,
		"cluster_memory_mib":     sheet.AllocatedMiB,
		"free_mib":               sheet.FreeMiB,
		"clusters":               clusters,
		"cpu_share":              tune.CPUShare(host.VCPU, len(cfg.Clusters)),
		"huge_pages":             steps.HugePagesFor(cfg, host),
		"memory_reserve_setting": cfg.Server.MemoryReserve,
		"io_weight_enforced":     ioEnforced,
		"io_weight":              ioReason,
		"disk_gib":               h.DiskGiB,
		"disk_used_gib":          h.DiskUsedGiB,
	}
	if h.MemoryKnown {
		out["commit_limit_mib"] = h.CommitLimitMiB
		out["committed_mib"] = h.CommittedMiB
		out["huge_pages_free"] = h.HugePagesFree
	}
	return out
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
