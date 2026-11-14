package node

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func forkMaterialisationOwner(ctx context.Context, o materialisationOwnership) bool {
	if !hasForkMaterialisationView(ctx, o.Target) {
		return false
	}
	view, ok := ctx.Value(forkViewKey{}).(*forkView)
	if !ok || view == nil || view.capability == nil {
		return false
	}
	expected := view.capability.owner
	return expected.Kind == materialisationFork && o.Kind == expected.Kind &&
		o.Operation == expected.Operation && o.Job == expected.Job && o.Instance == expected.Instance &&
		o.Source == expected.Source && o.Target == expected.Target && o.Stanza == expected.Stanza &&
		o.IntendedPort == expected.IntendedPort && o.DataPath == expected.DataPath && o.RegisteredPath == expected.RegisteredPath
}

func activeForkAuthority(env *jobEnv) *forkMaterialisationAuthority {
	forkCapabilities.Lock()
	defer forkCapabilities.Unlock()
	for _, a := range forkCapabilities.active {
		if a.env == env {
			return a
		}
	}
	return nil
}

var forkVerifyProvision = verifyForkProvision

func verifyForkProvision(ctx context.Context, cfg config.Cluster, rt engine.Runtime, target string) error {
	return verifyForkProvisionWith(ctx, cfg, rt, target, steps.Catalog())
}

func verifyForkProvisionWith(ctx context.Context, cfg config.Cluster, rt engine.Runtime, target string, catalog []engine.Step) error {
	count := 0
	roles, access := false, false
	for _, step := range engine.Expand(catalog, cfg) {
		if _, cluster := engine.SplitID(step.ID()); cluster != target {
			continue
		}
		if skip, ok := step.(engine.Skipper); ok && skip.SkipReason(cfg) != "" {
			continue
		}
		count++
		result, err := step.Check(ctx, cfg, rt)
		if err != nil || !result.Complete || result.Inconclusive {
			return fmt.Errorf("cannot verify required fork step %s", step.ID())
		}
		if step.ID() == steps.RolesStepID(target) {
			roles = true
		}
		if step.ID() == engine.InstanceID("postgres-access-v1", target) {
			transition, ok := step.(engine.AccessTransition)
			if !ok || transition.AccessTarget() != target {
				return errors.New("fork access verification target mismatch")
			}
			access = true
		}
	}
	if count == 0 || !roles || !access {
		return errors.New("required fork roles or access verification is missing")
	}
	return nil
}

var (
	forkPromote           = (*backup.Materialised).Promote
	forkAdopt             = (*backup.Materialised).Adopt
	forkRename            = renameFork
	forkForgetDatabase    = forgetDatabase
	forkOtherConfigs      = otherConfigs
	forkApplyOtherConfigs = applyOtherConfigs
	forkApplyMetrics      = applyMetrics
	forkFirstBackup       = firstBackup
	forkWriteMetrics      = steps.WriteMetrics
)

func cleanupFailedFork(ctx context.Context, a *forkMaterialisationAuthority, m *backup.Materialised, target string) error {
	if err := forkDrop(m); err != nil {
		return err
	}
	if err := a.run(ctx, func() error {
		if !a.dropped || a.adopted || a.target != target {
			return errors.New("fork cleanup authority unavailable")
		}
		if err := errors.Join(a.artifacts.validateCleanupCompleted(), a.reservation(ctx, false)); err != nil {
			return err
		}
		return forkForgetDatabase(target)
	}); err != nil {
		return err
	}
	return a.retireDropped(ctx)
}
