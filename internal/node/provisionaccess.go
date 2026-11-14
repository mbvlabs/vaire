package node

import (
	"context"
	"fmt"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

type ownedProvisionApply func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error)

// existingSettlementSteps excludes every access transition, not merely the
// current access step ID: an existing-target restore or unfollow is not access
// authorization. Only per-target instances are selected.
func existingSettlementSteps(catalog []engine.Step, cfg config.Cluster, name string) []string {
	var ids []string
	for _, step := range engine.Expand(catalog, cfg) {
		_, cluster := engine.SplitID(step.ID())
		if cluster != name {
			continue
		}
		if _, widens := step.(engine.AccessTransition); widens {
			continue
		}
		ids = append(ids, step.ID())
	}
	return ids
}

// settleExistingTarget runs per-target convergence without granting access.
// An empty selection must not be passed to Apply: it means "all steps".
func settleExistingTarget(ctx context.Context, cfg config.Cluster, rt engine.Runtime, name, recovering string, report engine.Reporter) error {
	catalog := steps.Catalog()
	ids := existingSettlementSteps(catalog, cfg, name)
	if len(ids) == 0 {
		return fmt.Errorf("no settlement steps for %q", name)
	}
	_, _, err := engine.Apply(ctx, catalog, cfg, rt, engine.Options{
		DB: name, Only: ids, Confirm: engine.Confirmation{Clusters: []string{name}},
		Recovering: recovering, Restarter: steps.Restarter(cfg), Report: report,
	})
	return err
}

// provisionOwnedTarget is only for the lifecycle operation that owns a new,
// independent target. It does not publish routes, timers or credentials. Its
// caller must wait for success before doing so, and must not use it to bypass
// the existing-database access confirmation. Standbys use their own existing
// follower provisioning path and never receive local access writes.
func provisionOwnedTarget(ctx context.Context, cfg config.Cluster, rt engine.Runtime, name, recovering string, report engine.Reporter) error {
	return provisionOwnedTargetWith(ctx, cfg, rt, name, recovering, report, steps.Catalog(), engine.Apply, steps.Restarter(cfg))
}

// provisionOwnedTargetWith splits target-scoped steps around the access step:
// roles and its prerequisites first, exact-target authorization second, then
// any later per-database steps. Global routing and timers are never selected.
func provisionOwnedTargetWith(ctx context.Context, cfg config.Cluster, rt engine.Runtime, name, recovering string,
	report engine.Reporter, catalog []engine.Step, apply ownedProvisionApply, restarter engine.Restarter,
) error {
	spec, ok := cfg.ClusterByName(name)
	if !ok || name == "" {
		return fmt.Errorf("owned access target %q is not configured", name)
	}
	if spec.IsFollower() {
		return fmt.Errorf("owned access target %q is a physical follower", name)
	}
	if recovering != "" && recovering != name {
		return fmt.Errorf("recovery target does not match owned access target")
	}
	if rt.DryRun || apply == nil {
		return fmt.Errorf("owned target provisioning requires an apply runner")
	}
	if err := engine.ValidateIDs(catalog); err != nil {
		return err
	}
	rolesID := steps.RolesStepID(name)
	accessID := engine.InstanceID("postgres-access-v1", name)
	var before, after []string
	var rolesSeen, accessSeen bool
	for _, step := range engine.Expand(catalog, cfg) {
		_, cluster := engine.SplitID(step.ID())
		if cluster != name {
			continue
		}
		switch step.ID() {
		case rolesID:
			rolesSeen = true
		case accessID:
			if accessSeen || !rolesSeen {
				return fmt.Errorf("owned access step must follow roles exactly once")
			}
			if access, ok := step.(engine.AccessTransition); !ok || access.AccessTarget() != name {
				return fmt.Errorf("owned access step does not name the exact target")
			}
			accessSeen = true
			continue
		}
		if accessSeen {
			after = append(after, step.ID())
		} else {
			before = append(before, step.ID())
		}
	}
	if !rolesSeen || !accessSeen {
		return fmt.Errorf("owned target catalog lacks roles or access step")
	}
	base := engine.Options{
		DB: name, Recovering: recovering, Confirm: engine.Confirmation{Clusters: []string{name}},
		Restarter: restarter, Report: report,
	}
	run := func(ids []string, internal bool) error {
		if len(ids) == 0 {
			return nil
		}
		opts := base
		opts.Only = ids
		if internal {
			opts.InternalAccess = map[string]bool{name: true}
		}
		_, _, err := apply(ctx, catalog, cfg, rt, opts)
		return err
	}
	if err := run(before, false); err != nil {
		return err
	}
	if err := run([]string{accessID}, true); err != nil {
		return err
	}
	return run(after, false)
}

// bootstrapDatabaseSteps stages global prerequisites, each visible database,
// and finally shared routing/backup steps. No shared consumer can expose a new
// target until every owned target's access policy has succeeded.
func bootstrapDatabaseSteps(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter,
	catalog []engine.Step, apply ownedProvisionApply, restarter engine.Restarter, failureHint string,
	owner *bootstrapOwnership, complete func(string) error,
) (engine.ApplyData, []output.Warning, error) {
	if owner != nil && complete == nil {
		return engine.ApplyData{}, nil, fmt.Errorf("bootstrap ownership is unavailable")
	}
	selected := map[string]bool{}
	for _, id := range steps.DatabaseStepIDs() {
		selected[id] = true
	}
	var before, after []string
	pastAccess := false
	for _, step := range catalog {
		if !selected[step.ID()] {
			continue
		}
		if step.ID() == "postgres-access-v1" {
			pastAccess = true
		}
		if _, perDatabase := step.(engine.Template); perDatabase {
			continue
		}
		if pastAccess {
			after = append(after, step.ID())
		} else {
			before = append(before, step.ID())
		}
	}
	if !pastAccess {
		return engine.ApplyData{}, nil, fmt.Errorf("bootstrap catalog has no access step")
	}
	var data engine.ApplyData
	var warnings []output.Warning
	collect := func(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		opts.FailureHint = failureHint
		part, w, err := apply(ctx, catalog, cfg, rt, opts)
		data.Steps = append(data.Steps, part.Steps...)
		data.Applied += part.Applied
		data.Skipped += part.Skipped
		data.Total += part.Total
		data.RestartPending = part.RestartPending
		data.ConfirmTokens = part.ConfirmTokens
		data.Restarts = append(data.Restarts, part.Restarts...)
		data.RestartsSettled = append(data.RestartsSettled, part.RestartsSettled...)
		warnings = append(warnings, w...)
		return part, w, err
	}
	shared := func(ids []string) error {
		if len(ids) == 0 {
			return nil
		}
		_, _, err := collect(ctx, catalog, cfg, rt, engine.Options{Only: ids, Report: report, Restarter: restarter})
		return err
	}
	if err := shared(before); err != nil {
		return data, warnings, err
	}
	visible := cfg
	if engine.ClusterView != nil {
		visible = engine.ClusterView(cfg, "")
	}
	for _, spec := range visible.Clusters {
		if spec.IsFollower() {
			ids := existingSettlementSteps(catalog, cfg, spec.Name)
			if len(ids) == 0 {
				return data, warnings, fmt.Errorf("no settlement steps for %q", spec.Name)
			}
			_, _, err := collect(ctx, catalog, cfg, rt, engine.Options{DB: spec.Name, Only: ids, Report: report, Restarter: restarter})
			if err != nil {
				return data, warnings, err
			}
		} else {
			if owner == nil {
				return data, warnings, fmt.Errorf("bootstrap target %s is not owned", spec.Name)
			}
			done, owned := owner.Targets[spec.Name]
			if !owned {
				return data, warnings, fmt.Errorf("bootstrap target %s is not owned", spec.Name)
			}
			runTarget := collect
			if done {
				runTarget = func(ctx context.Context, catalog []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
					opts.InternalAccess = nil
					opts.Confirm = engine.Confirmation{}
					return collect(ctx, catalog, cfg, rt, opts)
				}
			}
			if err := provisionOwnedTargetWith(ctx, cfg, rt, spec.Name, "", report, catalog, runTarget, restarter); err != nil {
				return data, warnings, err
			}
			if !done {
				if err := complete(spec.Name); err != nil {
					return data, warnings, err
				}
			}
		}
	}
	if err := shared(after); err != nil {
		return data, warnings, err
	}
	return data, warnings, nil
}
