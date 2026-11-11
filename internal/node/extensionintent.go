package node

import (
	"context"
	"slices"
	"sort"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

// Follower extension intent is inherited from the source. A direct follower
// edit is refused; a source change propagates to every follower in the same
// save, so apply and the direct extension path never see a split intent.

func canonicalNames(cfg config.Cluster, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		c, _ := cfg.ExtensionResolver().Canonical(name)
		out = append(out, c)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func sameNames(cfg config.Cluster, a, b []string) bool {
	return slices.Equal(canonicalNames(cfg, a), canonicalNames(cfg, b))
}

func sameRecovery(cfg config.Cluster, a, b *[]string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	if a == nil {
		return true
	}
	return slices.Equal(canonicalNames(cfg, *a), canonicalNames(cfg, *b))
}

// propagateFollowerExtensions updates every follower's inherited extension
// intent from its source. It refuses an independent follower edit instead of
// silently overwriting it. changed reports whether the configuration differs.
func propagateFollowerExtensions(old, updated config.Cluster) (config.Cluster, bool, error) {
	out := updated.Clone()
	changed := false
	for i := range out.Clusters {
		follower := out.Clusters[i]
		if !follower.IsFollower() {
			continue
		}
		source, ok := out.ClusterByName(follower.Follows)
		if !ok {
			continue
		}
		oldFollower, oldFollowerOK := old.ClusterByName(follower.Name)
		oldSource, oldSourceOK := old.ClusterByName(follower.Follows)
		sourceChanged := !oldSourceOK || !sameNames(old, oldSource.Extensions, source.Extensions) ||
			!sameRecovery(old, oldSource.RecoveryExtensions, source.RecoveryExtensions)
		followerChanged := !oldFollowerOK || !sameNames(old, oldFollower.Extensions, follower.Extensions) ||
			!sameRecovery(old, oldFollower.RecoveryExtensions, follower.RecoveryExtensions)
		if followerChanged && !sourceChanged {
			return updated, false, followerIntentRefusal(follower.Name, follower.Follows)
		}
		if followerChanged && sourceChanged &&
			(!sameNames(out, follower.Extensions, source.Extensions) || !sameRecovery(out, follower.RecoveryExtensions, source.RecoveryExtensions)) {
			return updated, false, followerIntentRefusal(follower.Name, follower.Follows)
		}
		if !sameNames(out, follower.Extensions, source.Extensions) {
			out.Clusters[i].Extensions = slices.Clone(source.Extensions)
			changed = true
		}
		if !sameRecovery(out, follower.RecoveryExtensions, source.RecoveryExtensions) {
			out.Clusters[i].RecoveryExtensions = cloneRecovery(source.RecoveryExtensions)
			changed = true
		}
	}
	return out, changed, nil
}

func cloneRecovery(in *[]string) *[]string {
	if in == nil {
		return nil
	}
	out := slices.Clone(*in)
	return &out
}

func followerIntentRefusal(follower, source string) error {
	return output.NewError(output.CodePrecondition,
		follower+"'s extension intent is inherited from "+source+", so it cannot be edited on the follower",
		output.ExitPrecondition,
		"Edit clusters."+source+".extensions or .recovery_extensions; the change propagates to its followers.").
		WithDetails(map[string]any{"reason": "follower_extension_intent", "database": follower, "source": source})
}

// refuseUnsafeExtensionConfigRemoval stops a plain configuration change from
// unloading a library while its SQL extension is still installed, or from
// switching an enabled extension's provider or schema without recreating it.
// The safe path is the direct extension command, which drops while the library
// is loaded.
func refuseUnsafeExtensionConfigRemoval(ctx context.Context, old, updated config.Cluster) error {
	recs, recErr := extensionRecords()
	for _, spec := range updated.Clusters {
		if spec.IsFollower() {
			continue
		}
		oldSpec, ok := old.ClusterByName(spec.Name)
		if !ok {
			continue
		}
		oldNames, err := old.ExtensionResolver().EffectiveNames(oldSpec.Extensions)
		if err != nil {
			return config.ValidationError(err)
		}
		newNames, err := updated.ExtensionResolver().EffectiveNames(spec.Extensions)
		if err != nil {
			return config.ValidationError(err)
		}
		newSet := map[string]bool{}
		for _, name := range newNames {
			newSet[name] = true
		}
		for _, name := range oldNames {
			if newSet[name] {
				continue
			}
			ext, err := old.ExtensionResolver().Resolve(name)
			if err != nil {
				return config.ValidationError(err)
			}
			if ext.ExtName == "" {
				continue
			}
			installed, err := extensionSQLInstalled(ctx, recs, recErr, spec, ext.ExtName)
			if err != nil {
				return err
			}
			if installed {
				return extensionRemoveRequired(spec.Name, ext.Name, "removed from cluster.toml")
			}
		}
		// A provider or schema change under an installed SQL extension is
		// equally unsafe: the old extension's objects remain.
		for _, name := range newNames {
			oldExt, oldOK := old.ExtensionResolver().Lookup(name)
			newExt, newOK := updated.ExtensionResolver().Lookup(name)
			if !oldOK || !newOK || oldExt.ExtName == "" {
				continue
			}
			if oldExt.Source == newExt.Source && oldExt.Package == newExt.Package &&
				oldExt.Preload == newExt.Preload && oldExt.Schema == newExt.Schema {
				continue
			}
			installed, err := extensionSQLInstalled(ctx, recs, recErr, spec, oldExt.ExtName)
			if err != nil {
				return err
			}
			if installed {
				return extensionRemoveRequired(spec.Name, oldExt.Name, "provider or schema changed")
			}
		}
	}
	return nil
}

func extensionSQLInstalled(ctx context.Context, recs backup.Records, recErr error, spec config.ClusterSpec, extName string) (bool, error) {
	if recErr != nil {
		return false, output.NewError(output.CodePrecondition,
			"cannot read the node records to decide whether "+extName+" is still installed in "+spec.Name+": "+recErr.Error(),
			output.ExitPrecondition, "Fix records.json first; nothing was saved.").
			WithDetails(map[string]any{"reason": "records_unavailable", "database": spec.Name})
	}
	if recs.IsAwaiting(spec.Name) {
		return false, output.NewError(output.CodePrecondition,
			spec.Name+" awaits recovery, so vaire cannot prove that "+extName+"'s SQL is absent",
			output.ExitPrecondition,
			"Recover it first, then use vaire ext remove "+extName+" --db "+spec.Name+" before changing cluster.toml.").
			WithDetails(map[string]any{"reason": "extension_remove_required", "database": spec.Name})
	}
	if !extensionClusterRunning(spec) {
		return false, output.NewError(output.CodePrecondition,
			spec.Name+" is not running, so vaire cannot prove that "+extName+"'s SQL is absent",
			output.ExitPrecondition,
			"Start it, then use vaire ext remove "+extName+" --db "+spec.Name+" before changing cluster.toml.").
			WithDetails(map[string]any{"reason": "extension_remove_required", "database": spec.Name})
	}
	have, err := extensionInstalled(ctx, spec)
	if err != nil {
		return false, output.NewError(output.CodePrecondition,
			"cannot read "+spec.Name+"'s installed extensions: "+err.Error(),
			output.ExitPrecondition,
			"Nothing was saved. Retry when the cluster answers; use vaire ext remove "+extName+" --db "+spec.Name+" to remove it safely.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
	}
	_, ok := have[extName]
	return ok, nil
}

func extensionRemoveRequired(db, name, why string) error {
	return output.NewError(output.CodePrecondition,
		name+" is "+why+" in "+db+", but its SQL extension may still be installed",
		output.ExitPrecondition,
		"Remove it with vaire ext remove "+name+" --db "+db+" --confirm "+db+"; the DROP runs while the library is loaded, then the reduced configuration is applied.").
		WithDetails(map[string]any{"reason": "extension_remove_required", "database": db, "extension": name})
}
