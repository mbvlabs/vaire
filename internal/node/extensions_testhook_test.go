package node

import (
	"context"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// Node lifecycle tests model storage, poolers and ownership, not the host's
// installed package/control-file tree. Their explicit preflight tests replace
// this seam themselves.
func init() {
	prepareTargetExtensions = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime, engine.Reporter) error {
		return nil
	}
	prepareCopyExtensions = func(context.Context, *extensionCopyPlan, engine.Runtime, engine.Reporter) error {
		return nil
	}
	applyUnfollowedConfig = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		return nil
	}
	deactivateCopyCron = func(context.Context, sqlRunner) (cronSettlement, error) { return cronSettlement{}, nil }
	retargetCopyCron = func(context.Context, sqlRunner, cronSettlement, string, string) (int, error) { return 0, nil }
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, nil
	}
	checkCopyExtensions = func(context.Context, config.Cluster, config.ClusterSpec, *backup.CopyPlan, sqlRunner, string) ([]output.Warning, error) {
		return nil, nil
	}
	pruneCopyGuard = func(context.Context, config.Cluster, config.ClusterSpec, string, *backup.CopyPlan, bool, string, func() error) ([]output.Warning, error) {
		return nil, nil
	}
	checkCopyCronSetting = func(context.Context, sqlRunner, string, string) error { return nil }
}
