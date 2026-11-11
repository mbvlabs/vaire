package node

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/steps"
)

// The extension transition planner and caller-locked executor. Direct
// extension commands and plain config/apply both use this ordered path: the
// complete source/follower set, exact restart tokens, sorted cluster locks,
// history before effects, and a truthful stage result on failure.

type extensionTargetSet struct {
	Source   config.ClusterSpec
	Running  []config.ClusterSpec
	Awaiting []config.ClusterSpec
}

// The transition executor's host seams. Production uses the engine's sorted
// multi-lock and read-only planner; lifecycle tests replace them.
var (
	extensionAcquireClusters = engine.AcquireClusters
	extensionPlan            = engine.Plan
)

func extensionTargetSetFor(cfg config.Cluster, recs backup.Records, source string) (extensionTargetSet, error) {
	src, ok := cfg.ClusterByName(source)
	if !ok {
		return extensionTargetSet{}, databaseNotFound(source)
	}
	out := extensionTargetSet{Source: src}
	for _, f := range cfg.FollowersOf(source) {
		if recs.IsAwaiting(f.Name) || f.FollowGeneration != "" {
			out.Awaiting = append(out.Awaiting, f)
			continue
		}
		if _, ok := recs.Unfollowing[f.Name]; ok {
			return extensionTargetSet{}, output.NewError(output.CodePrecondition,
				"the unfollow of "+f.Name+" is unfinished, so an extension change on "+source+" is refused",
				output.ExitPrecondition, "Finish it with vaire db unfollow "+f.Name+" --confirm "+f.Name+"/unfollow, then retry.").
				WithDetails(map[string]any{"reason": "unfollow_in_progress", "database": source, "follower": f.Name})
		}
		if !extensionClusterRunning(f) {
			return extensionTargetSet{}, output.NewError(output.CodePrecondition,
				f.Name+" is a follower of "+source+" but its cluster is not running",
				output.ExitPrecondition, "Start it first (vaire apply --db "+f.Name+"), then retry.").
				WithDetails(map[string]any{"reason": "follower_not_running", "database": f.Name, "source": source})
		}
		out.Running = append(out.Running, f)
	}
	sort.Slice(out.Running, func(i, j int) bool { return out.Running[i].Name < out.Running[j].Name })
	sort.Slice(out.Awaiting, func(i, j int) bool { return out.Awaiting[i].Name < out.Awaiting[j].Name })
	return out, nil
}

func (s extensionTargetSet) names() []string {
	out := []string{s.Source.Name}
	for _, f := range s.Running {
		out = append(out, f.Name)
	}
	sort.Strings(out)
	return out
}

func (s extensionTargetSet) all() []config.ClusterSpec {
	out := []config.ClusterSpec{s.Source}
	out = append(out, s.Running...)
	out = append(out, s.Awaiting...)
	return out
}

// lockExtensionMutation takes the global envelope's already-held exclusive
// lock and the complete target set's cluster locks once, sorted. It reloads
// desired state under those locks and refuses a changed target set instead of
// acting on a stale plan.
func lockExtensionMutation(ctx context.Context, env *Env, source string) (config.Cluster, []byte, backup.Records, extensionTargetSet, func(), error) {
	cfg, raw, err := extensionLoad()
	if err != nil {
		return cfg, raw, backup.Records{}, extensionTargetSet{}, nil, err
	}
	recs, err := extensionRecords()
	if err != nil {
		return cfg, raw, recs, extensionTargetSet{}, nil, err
	}
	initial, err := extensionTargetSetFor(cfg, recs, source)
	if err != nil {
		return cfg, raw, recs, extensionTargetSet{}, nil, err
	}
	release, err := extensionAcquireClusters(ctx, initial.names(), env.Req.Method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return cfg, raw, recs, extensionTargetSet{}, nil, err
	}
	cfg2, raw2, err := extensionLoad()
	if err != nil {
		release()
		return cfg, raw, recs, extensionTargetSet{}, nil, err
	}
	recs2, err := extensionRecords()
	if err != nil {
		release()
		return cfg2, raw2, recs2, extensionTargetSet{}, nil, err
	}
	current, err := extensionTargetSetFor(cfg2, recs2, source)
	if err != nil {
		release()
		return cfg2, raw2, recs2, extensionTargetSet{}, nil, err
	}
	if !slices.Equal(initial.names(), current.names()) {
		release()
		return cfg2, raw2, recs2, extensionTargetSet{}, nil, output.NewError(output.CodePrecondition,
			source+"'s follower target set changed while this change waited",
			output.ExitPrecondition, "Nothing was changed. Retry for the current target set.").
			WithDetails(map[string]any{"reason": "extension_target_changed", "database": source, "before": initial.names(), "after": current.names()})
	}
	for i := range initial.Running {
		if initial.Running[i].FollowGeneration != current.Running[i].FollowGeneration {
			release()
			return cfg2, raw2, recs2, extensionTargetSet{}, nil, output.NewError(output.CodePrecondition,
				initial.Running[i].Name+"'s follow generation changed while this change waited",
				output.ExitPrecondition, "Nothing was changed. Retry for the current follow.").
				WithDetails(map[string]any{"reason": "extension_target_changed", "database": initial.Running[i].Name})
		}
	}
	return cfg2, raw2, recs2, current, release, nil
}

// sameExtensionTargetSet compares the live target identities captured under
// the caller's locks with a freshly computed set. A changed name, follow
// generation or awaiting membership invalidates the token preflight.
func sameExtensionTargetSet(locked, current extensionTargetSet) bool {
	if len(locked.Running) != len(current.Running) || len(locked.Awaiting) != len(current.Awaiting) {
		return false
	}
	for i := range locked.Running {
		if locked.Running[i].Name != current.Running[i].Name ||
			locked.Running[i].FollowGeneration != current.Running[i].FollowGeneration {
			return false
		}
	}
	for i := range locked.Awaiting {
		if locked.Awaiting[i].Name != current.Awaiting[i].Name ||
			locked.Awaiting[i].FollowGeneration != current.Awaiting[i].FollowGeneration {
			return false
		}
	}
	return true
}

// extensionConfigPlanTokens reads the actual engine plan for each live
// target's proposed configuration against the current journal. A target that
// would apply or still has restart work needs its exact restart token before
// any direct extension mutation, even when the new descriptor itself is
// SQL-only and has no preload or setting.
func extensionConfigPlanTokens(ctx context.Context, cfg config.Cluster, targets []config.ClusterSpec) ([]string, error) {
	rt := runtimeFor(&Env{Cluster: cfg}, true)
	var out []string
	for _, target := range targets {
		plan, err := extensionPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{
			Only: []string{steps.ConfigStepID(target.Name)}, DB: target.Name, JournalPath: extensionJournalPath,
		})
		if err != nil {
			return nil, err
		}
		if plan.WouldApply > 0 || len(plan.RestartPending) > 0 {
			out = append(out, target.Name+"/restart")
		}
	}
	sort.Strings(out)
	return out, nil
}

func extensionRestartNeeded(old config.Cluster, oldSpec config.ClusterSpec, updated config.Cluster, newSpec config.ClusterSpec, name string, descriptorSaved bool) bool {
	before, err := old.ExtensionResolver().Preloads(oldSpec.Extensions)
	if err != nil {
		return true
	}
	after, err := updated.ExtensionResolver().Preloads(newSpec.Extensions)
	if err != nil {
		return true
	}
	ext, err := updated.ExtensionResolver().Resolve(name)
	if err != nil {
		return true
	}
	enabledBefore := extensionEnabled(old, oldSpec, name)
	return descriptorSaved || !slices.Equal(before, after) ||
		(!enabledBefore && (ext.Preload != "" || len(ext.Settings) > 0))
}

// extensionAddWithFollowers is the ordered add transition for a source with
// followers: history and intent, packages, followers' configuration and
// effective preloads, the primary's configuration, then its SQL.
// validateTargetExtensionDescriptors checks ordinary descriptor, provider and
// setting compatibility before any history, save, package, configuration,
// restart or SQL effect. The copy/standby gate is separate and applies only
// when the data is actually copied or followed.
func validateTargetExtensionDescriptors(cfg config.Cluster, spec config.ClusterSpec) error {
	exts, err := steps.ResolveExtensionSelection(cfg, spec)
	if err != nil {
		return err
	}
	return steps.ValidateExtensionDescriptorSafety(cfg, spec, exts)
}

// validateTargetExtensionCopySafety applies the copy/standby gate to every
// selected descriptor, SQL-only included, when a copy or follower will load
// the data.
func validateTargetExtensionCopySafety(cfg config.Cluster, spec config.ClusterSpec, standby bool) error {
	exts, err := steps.ResolveExtensionSelection(cfg, spec)
	if err != nil {
		return err
	}
	return steps.ValidateExtensionMetadataSafety(cfg, spec, exts, standby)
}

// validateTargetExtensionMetadata is the full descriptor plus copy/standby
// gate used by a source that has current or awaiting followers.
func validateTargetExtensionMetadata(cfg config.Cluster, spec config.ClusterSpec, standby bool) error {
	if err := validateTargetExtensionDescriptors(cfg, spec); err != nil {
		return err
	}
	return validateTargetExtensionCopySafety(cfg, spec, standby)
}

// sourceHasFollowers reports whether source currently has a follower entry,
// including one awaiting recovery. Such a source may later load its data as a
// copy, so all selected metadata is gated now.
func sourceHasFollowers(cfg config.Cluster, source string) bool {
	for _, spec := range cfg.Clusters {
		if spec.Follows == source {
			return true
		}
	}
	for _, spec := range cfg.Awaiting {
		if spec.Follows == source {
			return true
		}
	}
	return false
}

// followerRuntimeClass classifies one follower's available runtime evidence.
// An awaiting or not-yet-materialized target is metadata-only; a materialized
// follower must answer its actual SQL catalog.
func followerRuntimeClass(cfg config.Cluster, recs backup.Records, spec config.ClusterSpec) steps.FollowerRuntimeClass {
	if spec.FollowGeneration != "" || recs.IsAwaiting(spec.Name) {
		return steps.FollowerRuntimeAwaiting
	}
	for _, awaiting := range cfg.Awaiting {
		if awaiting.Name == spec.Name {
			return steps.FollowerRuntimeAwaiting
		}
	}
	return steps.FollowerRuntimeLive
}

// preflightFollowerRuntime validates every follower's inherited runtime
// metadata before any history, save, package, configuration, restart or DROP
// effect. outgoing is the pre-change descriptor a removal still needs as
// transition evidence; it is never current intent and is retired only after
// actual follower SQL absence.
func prepareFollowerRuntimePlans(ctx context.Context, cfg config.Cluster, recs backup.Records, followers []config.ClusterSpec, outgoing []catalog.Extension) (map[steps.FollowerRuntimePlanIdentity]steps.FollowerRuntimePlan, error) {
	plans := map[steps.FollowerRuntimePlanIdentity]steps.FollowerRuntimePlan{}
	operationID := steps.FollowerRuntimeOperation(ctx)
	for _, follower := range followers {
		if !follower.IsFollower() {
			continue
		}
		id, err := steps.FollowerRuntimeIdentity(cfg, follower, operationID)
		if err != nil {
			return nil, err
		}
		plan, err := steps.PlanFollowerRuntime(ctx, cfg, follower, steps.FollowerRuntimeRequest{
			Class:    followerRuntimeClass(cfg, recs, follower),
			Outgoing: outgoing,
		})
		if err != nil {
			return nil, err
		}
		plans[id] = plan
	}
	return plans, nil
}

func preflightFollowerRuntime(ctx context.Context, cfg config.Cluster, recs backup.Records, followers []config.ClusterSpec, outgoing []catalog.Extension) error {
	_, err := prepareFollowerRuntimePlans(ctx, cfg, recs, followers, outgoing)
	return err
}

func extensionAddWithFollowers(ctx context.Context, env *Env, old config.Cluster, raw []byte, spec config.ClusterSpec, updated config.Cluster, lockedTargets extensionTargetSet, ext catalog.Extension, descriptorSaved, alreadyEnabled bool) (Result, error) {
	recs, err := extensionRecords()
	if err != nil {
		return Result{}, err
	}
	targets, err := extensionTargetSetFor(updated, recs, spec.Name)
	if err != nil {
		return Result{}, err
	}
	if !sameExtensionTargetSet(lockedTargets, targets) {
		return Result{}, output.NewError(output.CodePrecondition,
			spec.Name+"'s follower target set changed while its extension change was being prepared",
			output.ExitPrecondition, "Nothing was changed. Retry for the current target set.").
			WithDetails(map[string]any{"reason": "extension_target_changed", "database": spec.Name})
	}
	updatedSpec, _ := updated.ClusterByName(spec.Name)
	rt := runtimeFor(env, false)
	installedKnown := false
	installed := map[string]extensionInstalledVersion{}
	if ext.ExtName != "" && extensionClusterRunning(spec) {
		var err error
		installed, err = extensionInstalled(ctx, spec)
		if err != nil {
			return Result{}, output.NewError(output.CodePrecondition,
				"cannot read "+spec.Name+"'s installed extensions: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
		}
		installedKnown = true
		if got, ok := installed[ext.ExtName]; ok && got.Default != "" && got.Default != got.Version {
			cmp, ordered := extensionVersionCompare(got.Version, got.Default)
			if !ordered {
				return Result{}, extensionUpdateDirectionUnknown(spec.Name, ext.Name, got.Version, got.Default)
			}
			if cmp > 0 {
				return Result{}, extensionDowngradeError(spec.Name, ext.Name, got.Version, got.Default)
			}
		}
	}
	var tokens []string
	tokenSet := map[string]bool{}
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)
	// The proposed configuration is part of the token decision, not only the
	// new descriptor's preload/pending metadata. A follower whose desired
	// postmaster setting is not rendered yet has no PostgreSQL pending_restart
	// until the configuration step runs; without this plan check a SQL-only add
	// could save history/packages/configuration and only then discover that the
	// follower's exact restart token was never authorized.
	planTokens, err := extensionConfigPlanTokens(ctx, updated, live)
	if err != nil {
		return Result{}, output.NewError(output.CodePrecondition,
			"cannot check the proposed extension configuration on every affected cluster: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the clusters answer.").
			WithDetails(map[string]any{"reason": "extension_config_unknown", "database": spec.Name})
	}
	for _, token := range planTokens {
		if !tokenSet[token] {
			tokens = append(tokens, token)
			tokenSet[token] = true
		}
	}
	for _, target := range live {
		oldTarget, _ := old.ClusterByName(target.Name)
		needed := extensionRestartNeeded(old, oldTarget, updated, target, ext.Name, descriptorSaved)
		if !needed && extensionClusterRunning(target) {
			pending, err := extensionPendingRestarts(ctx, target)
			if err != nil {
				return Result{}, output.NewError(output.CodePrecondition,
					"cannot read "+target.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
					"Nothing was changed. Retry when the cluster answers.").
					WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": target.Name})
			}
			needed = len(pending) > 0
			if !needed && ext.Preload != "" {
				loaded, pendingPreload, err := extensionPreloadFacts(ctx, target)
				if err != nil {
					return Result{}, output.NewError(output.CodePrecondition,
						"cannot read "+target.Name+"'s effective preload state: "+err.Error(), output.ExitPrecondition,
						"Nothing was changed. Retry when the cluster answers.").
						WithDetails(map[string]any{"reason": "extension_preload_unknown", "database": target.Name})
				}
				needed = !loaded[ext.Preload] && pendingPreload
			}
		}
		if needed && !tokenSet[target.Name] {
			tokens = append(tokens, target.Name+"/restart")
			tokenSet[target.Name] = true
		}
	}
	for _, token := range tokens {
		if !hasConfirmToken(env.Req, token) {
			return Result{}, confirmRequired(ext.Name+" changes the extension configuration of every affected cluster; "+token+" is required (a source token never authorizes a follower restart)", token)
		}
	}
	retry := extensionAddRetry(spec.Name, ext.Name, tokens)
	if !extensionClusterRunning(spec) {
		return Result{}, extensionClusterNotRunning(spec.Name, "ext add")
	}
	if err := validateTargetExtensionMetadata(updated, updatedSpec, false); err != nil {
		return Result{}, err
	}
	plans, err := prepareFollowerRuntimePlans(ctx, updated, recs, targets.all(), nil)
	if err != nil {
		return Result{}, err
	}
	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	if err := captureExtensionChange(old, updated); err != nil {
		return Result{}, output.NewError(output.CodePrecondition,
			"recording extension requirements before the change failed: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{"reason": "extension_history_capture_failed", "database": spec.Name})
	}
	newRaw, err := updated.Encode()
	if err != nil {
		return Result{}, err
	}
	if string(newRaw) != string(raw) {
		if err := extensionSave(newRaw); err != nil {
			return Result{}, err
		}
	}
	env.Cluster = updated
	for _, target := range targets.all() {
		if err := extensionPrepareStart(ctx, updated, target, nil, rt, env.Report); err != nil {
			return Result{}, extensionPartialError("the extension intent is saved, but installing its packages failed: "+err.Error(),
				"package_install_failed", spec.Name, ext.Name, len(tokens) > 0, false, engine.ApplyData{},
				map[string]any{"retry": retry, "intent_saved": true, "completed": []string{}, "pending": targets.names(), "packages_may_remain": true})
		}
	}
	var warnings []output.Warning
	var completed []string
	for _, follower := range targets.Running {
		applied, w, err := extensionApply(ctx, updated, rt, engine.Options{
			Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name,
			Confirm: extensionConfirmFor(follower.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
		})
		warnings = append(warnings, w...)
		clearExtensionDeferred(applied)
		if err != nil || extensionApplyIncomplete(applied) || extensionRestartPending(applied, follower.Name) {
			msg := "the extension intent is saved, but configuring follower " + follower.Name + " failed"
			if err != nil {
				msg += ": " + err.Error()
			}
			return Result{}, extensionPartialError(msg, "extension_apply_failed", spec.Name, ext.Name, len(tokens) > 0, applied.Restarted(), applied,
				map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		if err := verifyTargetPreloads(ctx, updated, follower); err != nil {
			return Result{}, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", spec.Name, ext.Name, true, applied.Restarted(), applied,
				map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		completed = append(completed, follower.Name)
	}
	primary, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Confirm: extensionConfirmFor(spec.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	warnings = append(warnings, w...)
	clearExtensionDeferred(primary)
	if err != nil || extensionApplyIncomplete(primary) || extensionRestartPending(primary, spec.Name) {
		msg := "the extension intent is saved and its followers are configured, but configuring " + spec.Name + " failed"
		if err != nil {
			msg += ": " + err.Error()
		}
		return Result{}, extensionPartialError(msg, "extension_apply_failed", spec.Name, ext.Name, len(tokens) > 0, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}
	if err := verifyExtensionSettings(ctx, updated, spec, ext); err != nil {
		return Result{}, extensionPartialError("the primary did not apply the required extension settings: "+err.Error(),
			"extension_settings_missing", spec.Name, ext.Name, len(tokens) > 0, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}
	if err := verifyTargetPreloads(ctx, updated, spec); err != nil {
		return Result{}, extensionPartialError("the primary did not load its required libraries: "+err.Error(),
			"extension_preload_missing", spec.Name, ext.Name, true, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}
	roles, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.RolesStepID(spec.Name)}, DB: spec.Name,
		Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	warnings = append(warnings, w...)
	clearExtensionDeferred(roles)
	if err != nil || extensionApplyIncomplete(roles) || extensionRestartPending(roles, spec.Name) {
		msg := "the extension intent and every cluster's configuration are saved, but creating the SQL failed"
		if err != nil {
			msg += ": " + err.Error()
		}
		return Result{}, extensionPartialError(msg, "extension_sql_failed", spec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"sql"}})
	}
	have, err := extensionInstalled(ctx, updatedSpec)
	if err != nil {
		return Result{}, extensionPartialError("the SQL ran, but verifying "+ext.Name+" failed: "+err.Error(),
			"extension_verify_failed", spec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"verify"}})
	}
	got, has := have[ext.ExtName]
	if !has {
		return Result{}, extensionPartialError(ext.Name+" was not created after the roles step",
			"extension_not_created", spec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"sql"}})
	}
	res := extAddResult{
		Database: spec.Name, Extension: ext.Name, ExtName: ext.ExtName, Source: ext.Source,
		Package: ext.Package, Preload: ext.Preload, Schema: ext.Schema,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		DescriptorSaved: descriptorSaved, AlreadyEnabled: alreadyEnabled, Created: installedKnown && !hasInstalled(installed, ext.ExtName),
		RestartRequired: len(tokens) > 0, Restarted: primary.Restarted(), Version: got.Version,
		DefaultVersion: got.Default, RequiredConfirmTokens: tokens,
	}
	if ext.Package != "" {
		res.PackageKept = ext.Package
	}
	res.UpdateAvailable = extensionUpdateAvailable(got.Version, got.Default)
	res.Downgrade = extensionDowngrade(got.Version, got.Default)
	if got.Default != "" && got.Default != got.Version {
		_, ordered := extensionVersionCompare(got.Version, got.Default)
		res.DirectionUnknown = !ordered
	}
	if got.Default != "" && got.Default != got.Version {
		cmp, ordered := extensionVersionCompare(got.Version, got.Default)
		if !ordered {
			return Result{}, extensionUpdateDirectionUnknown(spec.Name, ext.Name, got.Version, got.Default)
		}
		if cmp > 0 {
			return Result{}, extensionDowngradeError(spec.Name, ext.Name, got.Version, got.Default)
		}
		supported, err := extensionUpdatePath(ctx, updatedSpec, ext.ExtName, got.Version, got.Default)
		if err != nil || !supported {
			return Result{}, extensionPartialError(ext.Name+" is installed at "+got.Version+", but no supported update path to "+got.Default+" is available",
				"extension_update_unsupported", spec.Name, ext.Name, len(tokens) > 0, true, roles,
				map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
		}
		readiness, err := extensionPrimaryReadiness(ctx, updated, updatedSpec, ext)
		if err != nil {
			return Result{}, extensionPartialError(ext.Name+" is installed at "+got.Version+", but its required primary prerequisites cannot be proven: "+err.Error(),
				"extension_update_prerequisites_unknown", spec.Name, ext.Name, len(tokens) > 0, true, roles,
				map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
		}
		if readiness.Deferred {
			return Result{}, extensionPartialError(ext.Name+" is installed at "+got.Version+" but its required primary prerequisites are not effective: "+readiness.Detail,
				"extension_update_prerequisites", spec.Name, ext.Name, readiness.RestartPending, primary.Restarted(), primary,
				map[string]any{"retry": retry, "intent_saved": true, "pending": []string{"update"}, "version": got.Version, "default_version": got.Default})
		}
		sql := "ALTER EXTENSION " + pg.Quote(ext.ExtName) + " UPDATE TO " + extensionLiteral(got.Default) + ";\n"
		if err := extensionPG(updatedSpec).Exec(ctx, updatedSpec.DatabaseName(), sql, nil); err != nil {
			return Result{}, extensionPartialError("updating "+ext.Name+" from "+got.Version+" to "+got.Default+" failed: "+err.Error(),
				"extension_update_failed", spec.Name, ext.Name, len(tokens) > 0, true, roles,
				map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
		}
		after, err := extensionInstalled(ctx, updatedSpec)
		if err != nil || after[ext.ExtName].Version != got.Default {
			return Result{}, extensionPartialError("ALTER EXTENSION "+ext.Name+" did not reach "+got.Default,
				"extension_update_failed", spec.Name, ext.Name, len(tokens) > 0, true, roles,
				map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
		}
		res.PreviousVersion, res.Version, res.Updated = got.Version, got.Default, true
		res.UpdateAvailable = false
	}
	return Result{Data: res, Warnings: warnings}, nil
}

func hasInstalled(installed map[string]extensionInstalledVersion, name string) bool {
	_, ok := installed[name]
	return ok
}

func verifyTargetPreloads(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	if spec.IsFollower() {
		return verifyFollowerRuntime(ctx, cfg, spec)
	}
	want, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return err
	}
	if len(want) == 0 {
		return nil
	}
	loaded, pending, err := extensionPreloadFacts(ctx, spec)
	if err != nil {
		return err
	}
	for _, library := range want {
		if loaded[library] || pending {
			continue
		}
		return output.NewError(output.CodePrecondition,
			spec.Name+" did not load "+library+" after its restart",
			output.ExitPrecondition, "Check its configuration and log; the transition is incomplete.").
			WithDetails(map[string]any{"reason": "extension_preload_missing", "database": spec.Name, "library": library})
	}
	return nil
}

// verifyFollowerRuntime checks the complete follower plan, not only the
// extension just added: every required library must already be loaded (a
// pending restart is unfinished, not success), and every expected copy setting
// must be effective. The plan's own mandatory overlay is the expected value,
// never the source's unsuppressed ordinary setting.
func verifyFollowerRuntime(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	// Verification is post-effect: an unreadable catalog keeps the existing
	// guard obligations and becomes an explicit pending result, never an
	// inferred empty set.
	id, err := steps.FollowerRuntimeIdentity(cfg, spec, steps.FollowerRuntimeOperation(ctx))
	if err != nil {
		return err
	}
	snapshot, ok := steps.FollowerRuntimePlanFor(ctx, id)
	req := steps.FollowerRuntimeRequest{
		Class: steps.FollowerRuntimeLive, Outgoing: steps.FollowerRuntimeOutgoing(ctx),
		RetireAbsent: true, AllowUnknownSQL: true,
	}
	if ok {
		req.Snapshot = &snapshot
	}
	plan, err := steps.PlanFollowerRuntime(ctx, cfg, spec, req)
	if err != nil {
		return err
	}
	loaded, preloadPending, err := extensionPreloadFacts(ctx, spec)
	if err != nil {
		return err
	}
	for _, library := range plan.Preloads {
		if loaded[library] {
			continue
		}
		if preloadPending {
			return &steps.FollowerRuntimePendingError{
				Database: spec.Name, Reason: "extension_preload_pending",
				Detail: "library " + library + " waits for a restart",
			}
		}
		return output.NewError(output.CodePrecondition,
			spec.Name+" did not load "+library+" yet",
			output.ExitPrecondition, "Restart the follower, then retry the transition; the transition is incomplete.").
			WithDetails(map[string]any{"reason": "extension_preload_missing", "database": spec.Name, "library": library})
	}
	pendingSettings, pendingErr := extensionPendingRestarts(ctx, spec)
	pendingSet := map[string]bool{}
	for _, key := range pendingSettings {
		pendingSet[key] = true
	}
	keys := make([]string, 0, len(plan.ExpectedSettings))
	for key := range plan.ExpectedSettings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := plan.ExpectedSettings[key]
		rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(),
			steps.ExtensionSettingFactSQL(key), nil)
		if err != nil || len(rows) == 0 {
			if err == nil {
				err = errors.New("no row")
			}
			return fmt.Errorf("reading %s: %w", key, err)
		}
		if steps.ExtensionSettingMatches(want, steps.ParseExtensionSettingFact(rows[0])) {
			continue
		}
		if pendingErr != nil || pendingSet[key] {
			return &steps.FollowerRuntimePendingError{
				Database: spec.Name, Reason: "extension_settings_pending",
				Detail: key + " = " + want + " waits for a restart",
			}
		}
		return output.NewError(output.CodePrecondition,
			spec.Name+" did not apply "+key+" = "+want,
			output.ExitPrecondition, "Check the follower's configuration and restart, then retry.").
			WithDetails(map[string]any{"reason": "extension_settings_missing", "database": spec.Name, "setting": key})
	}
	if !plan.SQLFactsComplete {
		return &steps.FollowerRuntimePendingError{
			Database: spec.Name, Reason: "follower_sql_unknown",
			Detail: "the installed SQL catalog could not be read",
		}
	}
	if len(plan.PendingSQL) > 0 {
		return &steps.FollowerRuntimePendingError{
			Database: spec.Name, Reason: "extension_sql_replay_pending",
			Detail: "SQL extensions still replaying: " + strings.Join(plan.PendingSQL, ", "),
		}
	}
	return nil
}

func verifyExtensionSettings(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension) error {
	expected := steps.PrimaryExtensionExpectedSettings(cfg, spec, ext)
	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		want := expected[key]
		rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(),
			steps.ExtensionSettingFactSQL(key), nil)
		if err != nil || len(rows) == 0 {
			return fmt.Errorf("reading %s: %w", key, err)
		}
		fact := steps.ParseExtensionSettingFact(rows[0])
		if !steps.ExtensionSettingMatches(want, fact) {
			return fmt.Errorf("%s is %s, want %s", key, fact.Value, want)
		}
	}
	return nil
}

func extensionConfirmFor(name string, tokenSet map[string]bool) engine.Confirmation {
	if tokenSet[name] {
		return engine.Confirmation{Clusters: []string{name}}
	}
	return engine.Confirmation{}
}

// extensionRemoveWithFollowers is the ordered remove transition: history,
// preview/authorization, primary DROP while loaded, reduced intent, primary
// configuration, then followers.
func extensionRemoveWithFollowers(ctx context.Context, env *Env, old config.Cluster, raw []byte, spec config.ClusterSpec, updated config.Cluster, lockedTargets extensionTargetSet, ext catalog.Extension, installed, enabled, restart, cascade bool, res extRemoveResult) (Result, error) {
	recs, err := extensionRecords()
	if err != nil {
		return Result{}, err
	}
	targets, err := extensionTargetSetFor(updated, recs, spec.Name)
	if err != nil {
		return Result{}, err
	}
	if !sameExtensionTargetSet(lockedTargets, targets) {
		return Result{}, output.NewError(output.CodePrecondition,
			spec.Name+"'s follower target set changed while its extension removal was being prepared",
			output.ExitPrecondition, "Nothing was changed. Retry for the current target set.").
			WithDetails(map[string]any{"reason": "extension_target_changed", "database": spec.Name})
	}
	var tokens []string
	tokenSet := map[string]bool{}
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)
	planTokens, err := extensionConfigPlanTokens(ctx, updated, live)
	if err != nil {
		return Result{}, output.NewError(output.CodePrecondition,
			"cannot check the proposed extension configuration on every affected cluster: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the clusters answer.").
			WithDetails(map[string]any{"reason": "extension_config_unknown", "database": spec.Name})
	}
	for _, token := range planTokens {
		name := strings.TrimSuffix(token, "/restart")
		if !tokenSet[name] {
			tokens = append(tokens, token)
			tokenSet[name] = true
		}
	}
	restartNeeded := restart
	if !restartNeeded {
		for _, target := range live {
			pending, err := extensionPendingRestarts(ctx, target)
			if err != nil {
				return Result{}, output.NewError(output.CodePrecondition,
					"cannot read "+target.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
					"Nothing was changed. Retry when the cluster answers.").
					WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": target.Name})
			}
			if len(pending) > 0 {
				restartNeeded = true
				break
			}
		}
	}
	if restartNeeded {
		for _, target := range live {
			if !tokenSet[target.Name] {
				tokens = append(tokens, target.Name+"/restart")
				tokenSet[target.Name] = true
			}
		}
	}
	for _, token := range tokens {
		if !hasConfirmToken(env.Req, token) {
			return Result{}, confirmRequired("removing "+ext.Name+" changes every affected cluster's configuration; "+token+" is required", token)
		}
	}
	retry := extensionRemoveRetry(spec.Name, ext.Name, tokens)
	if !extensionClusterRunning(spec) {
		return Result{}, extensionClusterNotRunning(spec.Name, "ext remove")
	}
	if err := validateTargetExtensionMetadata(updated, targets.Source, false); err != nil {
		return Result{}, err
	}
	plans, err := prepareFollowerRuntimePlans(ctx, updated, recs, targets.all(), []catalog.Extension{ext})
	if err != nil {
		return Result{}, err
	}
	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	if err := captureExtensionChange(old, updated); err != nil {
		return Result{}, output.NewError(output.CodePrecondition,
			"recording extension requirements before the removal failed: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{"reason": "extension_history_capture_failed", "database": spec.Name})
	}
	if installed {
		sql := "DROP EXTENSION IF EXISTS " + pg.Quote(ext.ExtName)
		if cascade {
			sql += " CASCADE"
		}
		if err := extensionPG(spec).Exec(ctx, spec.DatabaseName(), sql+";\n", nil); err != nil {
			return Result{}, output.WrapError(output.CodeExternalCommand,
				fmt.Errorf("drop %s from %s: %w", ext.Name, spec.Name, err), output.ExitExternal,
				"cluster.toml and every restart are unchanged; read vaire logs postgres --db "+spec.Name+".").
				WithDetails(map[string]any{"reason": "extension_drop_failed", "database": spec.Name, "extension": ext.Name})
		}
	}
	newRaw, err := updated.Encode()
	if err != nil {
		return Result{}, err
	}
	if enabled && string(newRaw) != string(raw) {
		if err := extensionSave(newRaw); err != nil {
			return Result{}, extensionPartialError("the extension SQL is dropped, but saving the reduced cluster.toml failed: "+err.Error(),
				"extension_intent_save_failed", spec.Name, ext.Name, restartNeeded, false, engine.ApplyData{},
				map[string]any{"retry": retry, "sql_dropped": true, "intent_saved": false, "completed": []string{}, "pending": targets.names()})
		}
	}
	env.Cluster = updated
	rt := runtimeFor(env, false)
	var warnings []output.Warning
	var completed []string
	primary, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Confirm: extensionConfirmFor(spec.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	warnings = append(warnings, w...)
	clearExtensionDeferred(primary)
	if err != nil || extensionApplyIncomplete(primary) || extensionRestartPending(primary, spec.Name) {
		return Result{}, extensionPartialError("the extension SQL is dropped and cluster.toml is reduced, but applying the primary's configuration failed",
			"extension_apply_failed", spec.Name, ext.Name, restartNeeded, primary.Restarted(), primary,
			map[string]any{"retry": retry, "sql_dropped": true, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}
	completed = append(completed, spec.Name)
	var pendingFollowers []string
	var pendingDetail []string
	for _, follower := range targets.Running {
		applied, w, err := extensionApply(ctx, updated, rt, engine.Options{
			Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name,
			Confirm: extensionConfirmFor(follower.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
		})
		warnings = append(warnings, w...)
		clearExtensionDeferred(applied)
		if err != nil || extensionApplyIncomplete(applied) || extensionRestartPending(applied, follower.Name) {
			return Result{}, extensionPartialError("the extension SQL is dropped and the primary is configured, but follower "+follower.Name+" failed",
				"extension_apply_failed", spec.Name, ext.Name, restartNeeded, applied.Restarted(), applied,
				map[string]any{"retry": retry, "sql_dropped": true, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		if err := verifyTargetPreloads(ctx, updated, follower); err != nil {
			var pending *steps.FollowerRuntimePendingError
			if errors.As(err, &pending) {
				// The follower's SQL still replays the removed extension, or
				// its catalog is temporarily unreadable. Its old guard stays
				// in place; report the truthful pending state and keep the
				// supported retry.
				pendingFollowers = append(pendingFollowers, follower.Name)
				pendingDetail = append(pendingDetail, pending.Error())
				completed = append(completed, follower.Name+" (staged, pending)")
				continue
			}
			return Result{}, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", spec.Name, ext.Name, restartNeeded, applied.Restarted(), applied,
				map[string]any{"retry": retry, "sql_dropped": true, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		completed = append(completed, follower.Name)
	}
	if err := extensionVerifyAbsent(ctx, spec, ext.ExtName, true); err != nil {
		return Result{}, err
	}
	if len(pendingFollowers) > 0 {
		return Result{}, extensionPartialError(
			"the extension SQL is dropped and the primary is configured, but followers still replay or cannot yet prove the removed extension absent: "+strings.Join(pendingDetail, "; "),
			"extension_follower_replay_pending", spec.Name, ext.Name, restartNeeded, primary.Restarted(), primary,
			map[string]any{"retry": retry, "sql_dropped": true, "intent_saved": true, "completed": completed, "pending": pendingFollowers})
	}
	res.Restarted = primary.Restarted()
	res.RestartRequired = restartNeeded
	res.RequiredConfirmTokens = append([]string{spec.Name}, tokens...)
	return Result{Data: res, Warnings: warnings}, nil
}

func refuseExtensionUnloadTargets(ctx context.Context, cfg config.Cluster, source string) error {
	recs, err := extensionRecords()
	if err != nil {
		return err
	}
	targets, err := extensionTargetSetFor(cfg, recs, source)
	if err != nil {
		return err
	}
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)
	for _, target := range live {
		if err := steps.RefuseExtensionUnload(ctx, cfg, target, steps.FirstManagedConfig(target)); err != nil {
			return err
		}
	}
	return nil
}

func extensionSourceTransitionNeeded(ctx context.Context, cfg config.Cluster, source string, configSelected, rolesSelected bool) (bool, error) {
	// A scoped apply may not bypass a selected source/follower transition.
	return extensionTransitionNeededFor(ctx, cfg, source, configSelected, rolesSelected)
}

func selectedTransitionSources(cfg config.Cluster, steps []string) []string {
	if len(steps) == 0 {
		return cfg.Sources()
	}
	set := map[string]bool{}
	for _, id := range steps {
		base, cluster := engine.SplitID(id)
		switch base {
		case "postgres-config-v1", "postgres-roles-v1":
			if cluster == "" {
				for _, source := range cfg.Sources() {
					set[source] = true
				}
				continue
			}
			if len(cfg.FollowerNames(cluster)) > 0 {
				set[cluster] = true
			}
		}
	}
	out := make([]string, 0, len(set))
	for source := range set {
		out = append(out, source)
	}
	sort.Strings(out)
	return out
}

// extensionSourceSelection reports which source extension steps the request
// actually selected. An empty selection means the complete apply.
func extensionSourceSelection(selected []string, source string) (configSelected, rolesSelected bool) {
	if len(selected) == 0 {
		return true, true
	}
	for _, id := range selected {
		base, cluster := engine.SplitID(id)
		if cluster != "" && cluster != source {
			continue
		}
		switch base {
		case "postgres-config-v1":
			configSelected = true
		case "postgres-roles-v1":
			rolesSelected = true
		}
	}
	return configSelected, rolesSelected
}

// sourceExtensionTransitionNeeded decides only from the selected source work
// and actual source facts. It must not construct the follower target set: a
// roles-only or protected-policy change with no selected extension
// configuration and no missing desired SQL is not an extension transition.
func sourceExtensionTransitionNeeded(ctx context.Context, cfg config.Cluster, source string, configSelected, rolesSelected bool) (bool, error) {
	spec, ok := cfg.ClusterByName(source)
	if !ok {
		return false, databaseNotFound(source)
	}
	rt := runtimeFor(&Env{Cluster: cfg}, true)
	if configSelected {
		sourcePlan, err := extensionPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: []string{steps.ConfigStepID(source)}, DB: source})
		if err != nil {
			return false, err
		}
		if sourcePlan.WouldApply > 0 || len(sourcePlan.RestartPending) > 0 {
			return true, nil
		}
	}
	if !rolesSelected {
		return false, nil
	}
	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		return false, output.NewError(output.CodePrecondition,
			"cannot read "+source+"'s installed extensions before deciding whether dependent SQL is pending: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": source})
	}
	desired, err := steps.DesiredExtensionNames(cfg, spec)
	if err != nil {
		return false, err
	}
	for _, ext := range desired {
		if ext.ExtName != "" && !hasInstalled(installed, ext.ExtName) {
			return true, nil
		}
	}
	return false, nil
}

// sourceExtensionDependencyPending reports whether the source's current
// extension intent still has a real runtime dependency that is not effective:
// a desired SQL extension is missing, a required preload is not loaded, or a
// required setting does not have its expected value. It is read-only and does
// not treat every unrelated configuration drift as an extension transition.
func sourceExtensionDependencyPending(ctx context.Context, cfg config.Cluster, source string) (bool, error) {
	spec, ok := cfg.ClusterByName(source)
	if !ok {
		return false, databaseNotFound(source)
	}
	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		return false, output.NewError(output.CodePrecondition,
			"cannot read "+source+"'s installed extensions before its extension transition: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": source})
	}
	desired, err := steps.DesiredExtensionNames(cfg, spec)
	if err != nil {
		return false, err
	}
	for _, ext := range desired {
		if ext.ExtName != "" && !hasInstalled(installed, ext.ExtName) {
			return true, nil
		}
	}
	for _, ext := range desired {
		readiness, err := extensionPrimaryReadiness(ctx, cfg, spec, ext)
		if err != nil {
			return false, err
		}
		if readiness.Deferred {
			return true, nil
		}
	}
	return false, nil
}

// refuseNarrowExtensionTransition keeps tune.apply and the deferred-restart
// maintenance job from writing or restarting a source past a genuine
// extension dependency transition while a running follower is not effective.
// The complete source/follower graph and exact restart tokens belong to an
// ordinary vaire apply; a narrow tune or deferred restart has neither.
func refuseNarrowExtensionTransition(ctx context.Context, cfg config.Cluster, source string) error {
	pending, err := sourceExtensionDependencyPending(ctx, cfg, source)
	if err != nil {
		return err
	}
	if !pending {
		return nil
	}
	recs, err := extensionRecords()
	if err != nil {
		return err
	}
	targets, err := extensionTargetSetFor(cfg, recs, source)
	if err != nil {
		return err
	}
	for _, follower := range targets.Running {
		if err := verifyFollowerRuntime(ctx, cfg, follower); err != nil {
			return output.NewError(output.CodePrecondition,
				source+"'s extension transition is still pending and follower "+follower.Name+" is not effective: "+err.Error(),
				output.ExitPrecondition,
				"Run vaire apply --db "+source+" with the complete source/follower restart tokens; tune.apply and deferred maintenance do not replace that transition.").
				WithDetails(map[string]any{
					"reason": "extension_transition_requires_apply", "database": source, "follower": follower.Name,
				})
		}
	}
	return nil
}

func extensionTransitionNeededFor(ctx context.Context, cfg config.Cluster, source string, configSelected, rolesSelected bool) (bool, error) {
	needed, err := sourceExtensionTransitionNeeded(ctx, cfg, source, configSelected, rolesSelected)
	if err != nil || !needed {
		return needed, err
	}
	// The source itself has an extension dependency transition. Only now does
	// the complete target set and follower readiness matter.
	recs, err := extensionRecords()
	if err != nil {
		return false, err
	}
	targets, err := extensionTargetSetFor(cfg, recs, source)
	if err != nil {
		return false, err
	}
	rt := runtimeFor(&Env{Cluster: cfg}, true)
	for _, follower := range targets.Running {
		plan, err := extensionPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name})
		if err != nil {
			return false, err
		}
		if plan.WouldApply > 0 || len(plan.RestartPending) > 0 {
			return true, nil
		}
	}
	return true, nil
}

// applyExtensionTransitions is the plain apply/config path's ordered
// transition. It runs before the normal engine pass and returns the sorted
// target locks the caller holds through that pass. A scoped apply that cannot
// carry the complete transition refuses rather than bypassing it.
func applyExtensionTransitions(ctx context.Context, env *Env, p stepParams) ([]output.Warning, func(), error) {
	ctx = steps.WithFollowerRuntimeOperation(ctx, env.Req.RequestID)
	cfg := env.Cluster
	if err := refuseActiveActivationFences(cfg, p); err != nil {
		return nil, nil, err
	}
	if p.DB == "" && len(p.Steps) > 0 && !applySelectionMayTransition(env.Req) {
		for _, source := range cfg.Sources() {
			needed, err := extensionSourceTransitionNeeded(ctx, cfg, source, true, true)
			if err != nil {
				return nil, nil, err
			}
			if needed {
				return nil, nil, output.NewError(output.CodePrecondition,
					"this scoped apply selects steps that cannot execute "+source+"'s source/follower extension transition",
					output.ExitPrecondition, "Run a full vaire apply, or select the configuration/roles steps for the transition.").
					WithDetails(map[string]any{"reason": "extension_transition_selection", "database": source})
			}
		}
		return nil, nil, nil
	}
	if p.DB != "" {
		for _, id := range p.Steps {
			if _, cluster := engine.SplitID(id); cluster != "" && cluster != p.DB {
				return nil, nil, output.NewError(output.CodeUsage,
					"the step "+id+" names "+cluster+", not the selected database "+p.DB,
					output.ExitUsage, "Pass --step instances for "+p.DB+", or omit --db to apply every selected instance.").
					WithDetails(map[string]any{"reason": "step_selection_conflict", "database": p.DB, "step": id})
			}
		}
		spec, ok := cfg.ClusterByName(p.DB)
		if !ok {
			return nil, nil, databaseNotFound(p.DB)
		}
		if spec.IsFollower() {
			source, ok := cfg.ClusterByName(spec.Follows)
			if ok && (!sameNames(cfg, spec.Extensions, source.Extensions) || !sameRecovery(cfg, spec.RecoveryExtensions, source.RecoveryExtensions)) {
				return nil, nil, output.NewError(output.CodePrecondition,
					spec.Name+"'s extension intent differs from "+source.Name+"'s inherited intent",
					output.ExitPrecondition, "Run vaire config set on "+source.Name+" to propagate the change, then apply the source/follower transition.").
					WithDetails(map[string]any{"reason": "follower_extension_intent", "database": spec.Name, "source": source.Name})
			}
			recs, err := extensionRecords()
			if err != nil {
				return nil, nil, err
			}
			if err := preflightFollowerRuntime(ctx, cfg, recs, []config.ClusterSpec{spec}, nil); err != nil {
				return nil, nil, err
			}
			return nil, nil, nil
		}
		if len(cfg.FollowerNames(p.DB)) == 0 {
			return nil, nil, nil
		}
		if !applySelectionMayTransition(env.Req) {
			needed, err := extensionSourceTransitionNeeded(ctx, cfg, p.DB, true, true)
			if err != nil {
				return nil, nil, err
			}
			if needed {
				return nil, nil, output.NewError(output.CodePrecondition,
					"this scoped apply selects steps that cannot execute "+p.DB+"'s source/follower extension transition",
					output.ExitPrecondition, "Run a full vaire apply, or select the configuration/roles steps for the transition.").
					WithDetails(map[string]any{"reason": "extension_transition_selection", "database": p.DB})
			}
			return nil, nil, nil
		}
		configSelected, rolesSelected := extensionSourceSelection(p.Steps, p.DB)
		needed, err := extensionTransitionNeededFor(ctx, cfg, p.DB, configSelected, rolesSelected)
		if err != nil {
			return nil, nil, err
		}
		if !needed {
			// Roles-only or other unrelated convergence is the normal engine
			// pass's work; it must not construct the follower target set or
			// query follower extension state.
			return nil, nil, nil
		}
		recs, err := extensionRecords()
		if err != nil {
			return nil, nil, err
		}
		targets, err := extensionTargetSetFor(cfg, recs, p.DB)
		if err != nil {
			return nil, nil, err
		}
		if err := validateTargetExtensionMetadata(cfg, spec, false); err != nil {
			return nil, nil, err
		}
		if err := preflightFollowerRuntime(ctx, cfg, recs, targets.all(), nil); err != nil {
			return nil, nil, err
		}
		release, err := extensionAcquireClusters(ctx, targets.names(), env.Req.Method, env.Req.RequestID, env.Req.LockTimeout())
		if err != nil {
			return nil, nil, err
		}
		plans, err := prepareFollowerRuntimePlans(ctx, cfg, recs, targets.all(), nil)
		if err != nil {
			return nil, release, err
		}
		ctx = steps.WithFollowerRuntimePlans(ctx, plans)
		if err := refuseExtensionUnloadTargets(ctx, cfg, p.DB); err != nil {
			return nil, release, err
		}
		needed, err = extensionTransitionNeededFor(ctx, cfg, p.DB, configSelected, rolesSelected)
		if err != nil {
			return nil, release, err
		}
		if !needed {
			return nil, release, nil
		}
		warnings, err := extensionApplyTransitionForApply(ctx, env, cfg, p.DB)
		if err != nil {
			var pending *extensionApplyPending
			if errors.As(err, &pending) {
				release()
				pending.Warnings = append(warnings, pending.Warnings...)
				return nil, nil, pending
			}
		}
		return warnings, release, err
	}
	recs, err := extensionRecords()
	if err != nil {
		return nil, nil, err
	}
	var neededSources []string
	var names []string
	for _, source := range selectedTransitionSources(cfg, p.Steps) {
		configSelected, rolesSelected := extensionSourceSelection(p.Steps, source)
		needed, err := extensionTransitionNeededFor(ctx, cfg, source, configSelected, rolesSelected)
		if err != nil {
			return nil, nil, err
		}
		if !needed {
			continue
		}
		targets, err := extensionTargetSetFor(cfg, recs, source)
		if err != nil {
			return nil, nil, err
		}
		if err := validateTargetExtensionMetadata(cfg, targets.Source, false); err != nil {
			return nil, nil, err
		}
		if err := preflightFollowerRuntime(ctx, cfg, recs, targets.all(), nil); err != nil {
			return nil, nil, err
		}
		neededSources = append(neededSources, source)
		names = append(names, targets.names()...)
	}
	if len(neededSources) == 0 {
		return nil, nil, nil
	}
	release, err := extensionAcquireClusters(ctx, names, env.Req.Method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return nil, nil, err
	}
	plans := map[steps.FollowerRuntimePlanIdentity]steps.FollowerRuntimePlan{}
	for _, source := range neededSources {
		targets, err := extensionTargetSetFor(cfg, recs, source)
		if err != nil {
			return nil, release, err
		}
		more, err := prepareFollowerRuntimePlans(ctx, cfg, recs, targets.all(), nil)
		if err != nil {
			return nil, release, err
		}
		for id, plan := range more {
			plans[id] = plan
		}
	}
	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	for _, source := range neededSources {
		if err := refuseExtensionUnloadTargets(ctx, cfg, source); err != nil {
			return nil, release, err
		}
	}
	var warnings []output.Warning
	var pendingData engine.ApplyData
	var pendingClusters []string
	var pendingTokens []string
	for _, source := range neededSources {
		more, err := extensionApplyTransitionForApply(ctx, env, cfg, source)
		warnings = append(warnings, more...)
		if err != nil {
			var pending *extensionApplyPending
			if errors.As(err, &pending) {
				mergeApplyData(&pendingData, pending.Data)
				pendingClusters = append(pendingClusters, pending.Pending...)
				pendingTokens = append(pendingTokens, pending.Tokens...)
				warnings = append(warnings, pending.Warnings...)
				continue
			}
			return warnings, release, err
		}
	}
	if len(pendingClusters) > 0 {
		release()
		sort.Strings(pendingClusters)
		pendingClusters = slices.Compact(pendingClusters)
		sort.Strings(pendingTokens)
		pendingTokens = slices.Compact(pendingTokens)
		sort.Strings(pendingData.ConfirmTokens)
		pendingData.ConfirmTokens = slices.Compact(pendingData.ConfirmTokens)
		sort.Strings(pendingData.RestartPending)
		pendingData.RestartPending = slices.Compact(pendingData.RestartPending)
		return nil, nil, &extensionApplyPending{
			Data: pendingData, Warnings: warnings, Pending: pendingClusters, Tokens: pendingTokens,
			Message: "the extension transition is staged but not complete; " + strings.Join(pendingClusters, ", ") + " still need a restart, replay or effective setting",
		}
	}
	return warnings, release, nil
}

// extensionApplyPending says the safe staging work ran, but a restart, replay
// or dependent SQL remains. handleApply turns it into a successful pending
// result; it is not a failed apply.
type extensionApplyPending struct {
	Data     engine.ApplyData
	Warnings []output.Warning
	Pending  []string
	Tokens   []string
	Stage    string
	Message  string
}

func (e *extensionApplyPending) Error() string { return e.Message }

func mergeApplyData(dst *engine.ApplyData, src engine.ApplyData) {
	dst.Steps = append(dst.Steps, src.Steps...)
	dst.Applied += src.Applied
	dst.Skipped += src.Skipped
	dst.Total += src.Total
	dst.Restarts = append(dst.Restarts, src.Restarts...)
	dst.RestartsSettled = append(dst.RestartsSettled, src.RestartsSettled...)
	dst.RestartPending = append(dst.RestartPending, src.RestartPending...)
	dst.ConfirmTokens = append(dst.ConfirmTokens, src.ConfirmTokens...)
}

func extensionPendingResult(source string, data engine.ApplyData, completed, pending, tokens []string, stage string) *extensionApplyPending {
	sort.Strings(tokens)
	tokens = slices.Compact(tokens)
	sort.Strings(pending)
	pending = slices.Compact(pending)
	retry := "vaire apply --db " + source
	for _, token := range tokens {
		retry += " --confirm " + token
	}
	data.ConfirmTokens = append(data.ConfirmTokens, tokens...)
	sort.Strings(data.ConfirmTokens)
	data.ConfirmTokens = slices.Compact(data.ConfirmTokens)
	return &extensionApplyPending{
		Data: data, Pending: pending, Tokens: tokens, Stage: stage,
		Message: "the extension transition is staged but not complete: " + strings.Join(pending, ", ") + " still need a restart, replay or effective setting",
		Warnings: []output.Warning{{
			Code:    "extension_apply_pending",
			Message: "staged work is saved; retry with " + retry,
		}},
	}
}

// extensionApplyTransitionForApply runs the safe staged transition for the
// plain apply/config path. It stages followers first, without demanding their
// restart tokens. Only when every follower is effective does it stage the
// source's configuration. The roles step defers its dependent extension SQL
// until that extension's own required libraries/settings are effective; an
// unrelated pending PostgreSQL setting does not block it. Missing tokens leave
// the affected target pending; they are not a reason to skip safe staging.
// The caller holds the global exclusive envelope and sorted target locks.
func extensionApplyTransitionForApply(ctx context.Context, env *Env, cfg config.Cluster, source string) ([]output.Warning, error) {
	recs, err := extensionRecords()
	if err != nil {
		return nil, err
	}
	targets, err := extensionTargetSetFor(cfg, recs, source)
	if err != nil {
		return nil, err
	}
	if len(targets.Running) == 0 {
		return nil, nil
	}
	for _, follower := range targets.Running {
		if !sameNames(cfg, follower.Extensions, targets.Source.Extensions) ||
			!sameRecovery(cfg, follower.RecoveryExtensions, targets.Source.RecoveryExtensions) {
			return nil, output.NewError(output.CodePrecondition,
				follower.Name+"'s extension intent differs from "+source+"'s inherited intent",
				output.ExitPrecondition, "Run vaire config set on "+source+" to propagate the change, then apply the source/follower transition.").
				WithDetails(map[string]any{"reason": "follower_extension_intent", "database": follower.Name, "source": source})
		}
	}
	rt := runtimeFor(env, false)
	needed := false
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)
	allTargetNames := make([]string, 0, len(live))
	for _, target := range live {
		allTargetNames = append(allTargetNames, target.Name)
	}
	retryAll := extensionApplyRetry(cfg.Name(), source, engine.Confirmation{Clusters: allTargetNames})
	for _, target := range live {
		plan, err := extensionPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: []string{steps.ConfigStepID(target.Name)}, DB: target.Name})
		if err != nil {
			return nil, err
		}
		if plan.WouldApply > 0 || len(plan.RestartPending) > 0 {
			needed = true
		}
	}
	// A roles-only transition still matters when the source is missing one of
	// its required SQL extensions. Without this, the normal engine pass would
	// create that extension before the follower's runtime is verified.
	sourceInstalled, err := extensionInstalled(ctx, targets.Source)
	if err != nil {
		return nil, output.NewError(output.CodePrecondition,
			"cannot read "+source+"'s installed extensions before its dependent SQL: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": source})
	}
	desiredSQL, err := steps.DesiredExtensionNames(cfg, targets.Source)
	if err != nil {
		return nil, err
	}
	for _, ext := range desiredSQL {
		if ext.ExtName != "" && !hasInstalled(sourceInstalled, ext.ExtName) {
			needed = true
			break
		}
	}
	if !needed {
		return nil, nil
	}
	if err := steps.RefuseExtensionUnload(ctx, cfg, targets.Source, steps.FirstManagedConfig(targets.Source)); err != nil {
		return nil, err
	}
	serverToken := hasConfirmToken(env.Req, cfg.Name()+"/restart")
	authorized := func(name string) bool {
		return serverToken || hasConfirmToken(env.Req, name+"/restart")
	}
	confirmFor := func(name string) engine.Confirmation {
		if authorized(name) {
			return engine.Confirmation{Clusters: []string{name}}
		}
		return engine.Confirmation{}
	}
	var warnings []output.Warning
	var data engine.ApplyData
	var completed []string
	var pending []string
	var pendingTokens []string
	// Followers first. Each may stage its configuration, guard and packages
	// without its restart token; only an authorized target restarts.
	for _, follower := range targets.Running {
		applied, w, err := extensionApply(ctx, cfg, rt, engine.Options{
			Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name,
			Confirm: confirmFor(follower.Name), Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
		})
		warnings = append(warnings, w...)
		clearExtensionDeferred(applied)
		if err != nil {
			return warnings, extensionPartialError("the configuration is saved, but follower "+follower.Name+" failed before the primary",
				"extension_apply_failed", source, "", true, applied.Restarted(), applied,
				map[string]any{"retry": retryAll, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		mergeApplyData(&data, applied)
		if extensionApplyIncomplete(applied) || extensionRestartPending(applied, follower.Name) {
			pending = append(pending, follower.Name)
			pendingTokens = append(pendingTokens, follower.Name+"/restart")
			continue
		}
		if err := verifyTargetPreloads(ctx, cfg, follower); err != nil {
			var followerPending *steps.FollowerRuntimePendingError
			if errors.As(err, &followerPending) {
				pending = append(pending, follower.Name)
				switch followerPending.Reason {
				case "extension_preload_pending", "extension_settings_pending":
					pendingTokens = append(pendingTokens, follower.Name+"/restart")
				}
				warnings = append(warnings, output.Warning{Code: followerPending.Reason, Message: followerPending.Error()})
				continue
			}
			return warnings, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", source, "", true, applied.Restarted(), applied,
				map[string]any{"retry": retryAll, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}
		completed = append(completed, follower.Name)
	}
	if len(pending) > 0 {
		return warnings, extensionPendingResult(source, data, completed, pending, pendingTokens, "followers")
	}
	// All followers are effective. The source configuration may now be staged,
	// but dependent SQL waits for its own effective prerequisites.
	applied, w, err := extensionApply(ctx, cfg, rt, engine.Options{
		Only: []string{steps.ConfigStepID(source)}, DB: source,
		Confirm: confirmFor(source), Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
	})
	warnings = append(warnings, w...)
	clearExtensionDeferred(applied)
	if err != nil {
		return warnings, extensionPartialError("the configuration is saved, but the primary's configuration failed",
			"extension_apply_failed", source, "", true, applied.Restarted(), applied,
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": completed, "pending": []string{source}})
	}
	mergeApplyData(&data, applied)
	if extensionApplyIncomplete(applied) {
		pending = append(pending, source)
		pendingTokens = append(pendingTokens, source+"/restart")
		return warnings, extensionPendingResult(source, data, completed, pending, pendingTokens, "primary_config")
	}
	sourceRestartPending := extensionRestartPending(applied, source)
	roles, w, err := extensionApply(ctx, cfg, rt, engine.Options{
		Only: []string{steps.RolesStepID(source)}, DB: source,
		Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
	})
	warnings = append(warnings, w...)
	clearExtensionDeferred(roles)
	if err != nil {
		return warnings, extensionPartialError("the configuration is saved, but the primary's extension SQL failed",
			"extension_sql_failed", source, "", true, true, roles,
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": append(completed, source), "pending": []string{"sql"}})
	}
	mergeApplyData(&data, roles)
	if extensionApplyIncomplete(roles) {
		// Roles and other SQL ran, but the dependent extension SQL is deferred
		// until its own required libraries/settings are effective. A pure
		// reload-only setting wait needs no synthetic restart token.
		pending = append(pending, source)
		if extensionWarningCode(w, "extension_pending_restart") || !extensionWarningCode(w, "extension_settings_pending") {
			pendingTokens = append(pendingTokens, source+"/restart")
		}
		return warnings, extensionPendingResult(source, data, completed, pending, pendingTokens, "sql")
	}
	if extensionRestartPending(roles, source) {
		return warnings, extensionPartialError("the configuration is saved, but the primary's extension SQL left a pending restart",
			"extension_sql_failed", source, "", true, true, roles,
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": append(completed, source), "pending": []string{"sql"}})
	}
	if sourceRestartPending {
		// The configuration is staged and dependent SQL already ran because
		// its own required libraries/settings were effective. An unrelated
		// pending postmaster setting still leaves the apply truthfully pending.
		pending = append(pending, source)
		pendingTokens = append(pendingTokens, source+"/restart")
		return warnings, extensionPendingResult(source, data, completed, pending, pendingTokens, "primary_restart")
	}
	return warnings, nil
}

func extensionRestartTokensFor(cfg config.Cluster, source string) []string {
	var out []string
	for _, f := range cfg.FollowerNames(source) {
		out = append(out, f+"/restart")
	}
	sort.Strings(out)
	return out
}
