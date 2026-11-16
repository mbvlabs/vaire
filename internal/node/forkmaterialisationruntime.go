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

// cleanupFailedFork rolls back a fork that got past its data/config creation.
// The prepared activation fence is retired only after the birth-bound physical
// drop, the cluster.toml removal and the materialisation-record retirement all
// succeeded. Keeping it until then makes every intermediate crash recoverable
// and never lets intent alone authorize deletion.
func cleanupFailedFork(ctx context.Context, a *forkMaterialisationAuthority, m *backup.Materialised, target, opID string) error {
	o := a.artifacts.owner
	targetCfg, err := capturePreparedForkConfig(a.env.Cluster, target, o, a.artifacts.dataRoot)
	if err != nil {
		return err
	}
	fresh, err := freshPreparedForkConfig(targetCfg)
	if err != nil {
		return err
	}
	expected, hasFence, err := preparedForkFenceForCleanup(fresh, target, opID)
	if err != nil {
		return err
	}
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
		// Re-prove the exact target configuration under the live lease
		// immediately before the configuration write.
		fresh, err := freshPreparedForkConfig(targetCfg)
		if err != nil {
			return err
		}
		if err := CheckNotProtected(fresh, target, "db fork cleanup"); err != nil {
			return err
		}
		if err := refuseSourceDrop(fresh, target); err != nil {
			return err
		}
		return forkForgetDatabase(target)
	}); err != nil {
		return err
	}
	if err := a.retireDropped(ctx); err != nil {
		return err
	}
	if !hasFence {
		return nil
	}
	return a.run(ctx, func() error {
		return retireAbandonedForkFence(expected, nil)
	})
}
