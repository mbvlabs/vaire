package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
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
	Stopped  []config.ClusterSpec
	Awaiting []config.ClusterSpec
}

// The transition executor's host seams. Production uses the engine's sorted
// multi-lock and read-only planner; lifecycle tests replace them.
var (
	extensionAcquireClusters = engine.AcquireClusters
	extensionPlan            = engine.Plan
	extensionValidateStopped = extensionValidateStoppedStart
	extensionRefuseUnload    = steps.RefuseExtensionUnload
	extensionTargetPresent   = func(cfg config.Cluster, spec config.ClusterSpec) bool {
		_, err := os.Stat(cfg.Server.DataDirectory(spec.Name))
		return err == nil
	}
)

func extensionTargetSetFor(cfg config.Cluster, recs backup.Records, source string) (extensionTargetSet, error) {
	return extensionTargetSetForMode(cfg, recs, source, false)
}

func extensionApplyTargetSetFor(cfg config.Cluster, recs backup.Records, source string) (extensionTargetSet, error) {
	return extensionTargetSetForMode(cfg, recs, source, true)
}

func extensionTargetSetForMode(cfg config.Cluster, recs backup.Records, source string, allowStopped bool) (extensionTargetSet, error) {
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
			if !allowStopped {
				return extensionTargetSet{}, output.NewError(output.CodePrecondition,
					f.Name+" is a follower of "+source+" but its cluster is not running",
					output.ExitPrecondition, "Start it first (vaire apply --db "+f.Name+"), then retry.").
					WithDetails(map[string]any{"reason": "follower_not_running", "database": f.Name, "source": source})
			}

			if !extensionTargetPresent(cfg, f) {
				// Configured but never materialised: metadata only, exactly
				// like an awaiting target. There is nothing to start yet.
				out.Awaiting = append(out.Awaiting, f)
				continue
			}

			out.Stopped = append(out.Stopped, f)

			continue
		}

		out.Running = append(out.Running, f)
	}

	sort.Slice(out.Running, func(i, j int) bool { return out.Running[i].Name < out.Running[j].Name })
	sort.Slice(out.Stopped, func(i, j int) bool { return out.Stopped[i].Name < out.Stopped[j].Name })
	sort.Slice(out.Awaiting, func(i, j int) bool { return out.Awaiting[i].Name < out.Awaiting[j].Name })

	return out, nil
}

func (s extensionTargetSet) names() []string {
	out := make([]string, 0, 1+len(s.Running)+len(s.Stopped))

	out = append(out, s.Source.Name)
	for _, f := range s.Running {
		out = append(out, f.Name)
	}

	for _, f := range s.Stopped {
		out = append(out, f.Name)
	}

	sort.Strings(out)

	return out
}

func (s extensionTargetSet) all() []config.ClusterSpec {
	out := make([]config.ClusterSpec, 0, 1+len(s.Running)+len(s.Stopped)+len(s.Awaiting))
	out = append(out, s.Source)
	out = append(out, s.Running...)
	out = append(out, s.Stopped...)
	out = append(out, s.Awaiting...)

	return out
}

// liveFollowers is the followers whose runtime can be verified: those already
// running plus the stopped ones this transition starts first.
func (s extensionTargetSet) liveFollowers() []config.ClusterSpec {
	out := append(append([]config.ClusterSpec(nil), s.Running...), s.Stopped...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

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
func validateTargetExtensionMetadata(cfg config.Cluster, spec config.ClusterSpec) error {
	if err := validateTargetExtensionDescriptors(cfg, spec); err != nil {
		return err
	}

	return validateTargetExtensionCopySafety(cfg, spec, false)
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
// An awaiting or not-yet-materialized target is metadata-only; a stopped but
// materialized follower is explicitly stopped (metadata-only until the
// engine-owned start makes its catalog authoritative); a running follower must
// answer its actual SQL catalog.
func followerRuntimeClass(cfg config.Cluster, recs backup.Records, spec config.ClusterSpec) steps.FollowerRuntimeClass {
	if spec.FollowGeneration != "" || recs.IsAwaiting(spec.Name) {
		return steps.FollowerRuntimeAwaiting
	}

	for _, awaiting := range cfg.Awaiting {
		if awaiting.Name == spec.Name {
			return steps.FollowerRuntimeAwaiting
		}
	}

	if !extensionClusterRunning(spec) {
		if !extensionTargetPresent(cfg, spec) {
			return steps.FollowerRuntimeAwaiting
		}

		return steps.FollowerRuntimeStopped
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

	installed, installedKnown, err := extensionAddInstalledFacts(ctx, spec, ext)
	if err != nil {
		return Result{}, err
	}

	tokens, tokenSet, err := extensionAddTransitionTokens(ctx, env, old, updated, spec, targets, ext, descriptorSaved)
	if err != nil {
		return Result{}, err
	}

	retry := extensionAddRetry(spec.Name, ext.Name, tokens)
	if !extensionClusterRunning(spec) {
		return Result{}, extensionClusterNotRunning(spec.Name, "ext add")
	}

	if err := validateTargetExtensionMetadata(updated, updatedSpec); err != nil {
		return Result{}, err
	}

	plans, err := prepareFollowerRuntimePlans(ctx, updated, recs, targets.all(), nil)
	if err != nil {
		return Result{}, err
	}

	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	saved := descriptorSaved || !alreadyEnabled

	intentSaved := !saved
	if err := extensionAddStageSavedIntent(ctx, env, old, updated, spec, targets, ext, raw, retry, rt, intentSaved); err != nil {
		return Result{}, err
	}

	env.Cluster = updated
	restart := len(tokens) > 0

	warnings, completed, err := extensionAddConfigureFollowers(ctx, env, updated, rt, targets, tokenSet, retry, spec, ext, restart)
	if err != nil {
		return Result{}, err
	}

	primary, primaryWarnings, err := extensionAddConfigurePrimary(ctx, env, updated, rt, spec, ext, tokenSet, retry, restart, completed)
	warnings = append(warnings, primaryWarnings...)

	if err != nil {
		return Result{}, err
	}

	roles, got, roleWarnings, err := extensionAddRunRoles(ctx, env, updated, rt, spec, updatedSpec, ext, retry, restart, completed)
	warnings = append(warnings, roleWarnings...)

	if err != nil {
		return Result{}, err
	}

	return extensionAddFollowerResult(ctx, updated, updatedSpec, spec, ext, installed, installedKnown, descriptorSaved, alreadyEnabled, restart, primary, roles, tokens, retry, got, warnings)
}

func extensionAddFollowerResult(ctx context.Context, updated config.Cluster, updatedSpec config.ClusterSpec, spec config.ClusterSpec, ext catalog.Extension, installed map[string]extensionInstalledVersion, installedKnown, descriptorSaved, alreadyEnabled, restart bool, primary, roles engine.ApplyData, tokens []string, retry string, got extensionInstalledVersion, warnings []output.Warning) (Result, error) {
	res := extAddResult{
		Database: spec.Name, Extension: ext.Name, ExtName: ext.ExtName, Source: ext.Source,
		Package: ext.Package, Preload: ext.Preload, Schema: ext.Schema,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		DescriptorSaved: descriptorSaved, AlreadyEnabled: alreadyEnabled, Created: installedKnown && !hasInstalled(installed, ext.ExtName),
		RestartRequired: restart, Restarted: primary.Restarted(), Version: got.Version,
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
		previous, version, wasUpdated, err := extensionAddFollowerUpdateIfNeeded(ctx, updated, updatedSpec, ext, got, primary, roles, tokens, retry)
		if err != nil {
			return Result{}, err
		}

		if wasUpdated {
			res.PreviousVersion, res.Version, res.Updated = previous, version, true
			res.UpdateAvailable = false
		}
	}

	return Result{Data: res, Warnings: warnings}, nil
}

func extensionAddInstalledFacts(ctx context.Context, spec config.ClusterSpec, ext catalog.Extension) (map[string]extensionInstalledVersion, bool, error) {
	installed := map[string]extensionInstalledVersion{}
	if ext.ExtName == "" || !extensionClusterRunning(spec) {
		return installed, false, nil
	}

	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		return nil, false, output.NewError(output.CodePrecondition,
			"cannot read "+spec.Name+"'s installed extensions: "+err.Error(), output.ExitPrecondition,
			"Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
	}

	if got, ok := installed[ext.ExtName]; ok && got.Default != "" && got.Default != got.Version {
		cmp, ordered := extensionVersionCompare(got.Version, got.Default)
		if !ordered {
			return nil, false, extensionUpdateDirectionUnknown(spec.Name, ext.Name, got.Version, got.Default)
		}

		if cmp > 0 {
			return nil, false, extensionDowngradeError(spec.Name, ext.Name, got.Version, got.Default)
		}
	}

	return installed, true, nil
}

func extensionAddTransitionTokens(ctx context.Context, env *Env, old, updated config.Cluster, spec config.ClusterSpec, targets extensionTargetSet, ext catalog.Extension, descriptorSaved bool) ([]string, map[string]bool, error) {
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)

	tokens, tokenSet, err := extensionPlanRestartTokens(ctx, updated, live, spec.Name)
	if err != nil {
		return nil, nil, err
	}

	tokens, tokenSet, err = extensionAddRuntimeRestartTokens(ctx, old, updated, ext, descriptorSaved, live, tokens, tokenSet)
	if err != nil {
		return nil, nil, err
	}

	for _, token := range tokens {
		if !hasConfirmToken(env.Req, token) {
			return nil, nil, confirmRequired(ext.Name+" changes the extension configuration of every affected cluster; "+token+" is required (a source token never authorizes a follower restart)", token)
		}
	}

	return tokens, tokenSet, nil
}

// extensionPlanRestartTokens checks the proposed configuration and seeds the
// restart tokens every live target already needs. A follower whose desired
// postmaster setting is not rendered yet has no PostgreSQL pending_restart
// until the configuration step runs; without this plan check a SQL-only add
// could save history/packages/configuration and only then discover that the
// follower's exact restart token was never authorized.
func extensionPlanRestartTokens(ctx context.Context, updated config.Cluster, live []config.ClusterSpec, db string) ([]string, map[string]bool, error) {
	var tokens []string

	tokenSet := map[string]bool{}

	planTokens, err := extensionConfigPlanTokens(ctx, updated, live)
	if err != nil {
		return nil, nil, output.NewError(output.CodePrecondition,
			"cannot check the proposed extension configuration on every affected cluster: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the clusters answer.").
			WithDetails(map[string]any{"reason": "extension_config_unknown", "database": db})
	}

	for _, token := range planTokens {
		name := strings.TrimSuffix(token, "/restart")
		if !tokenSet[name] {
			tokens = append(tokens, token)
			tokenSet[name] = true
		}
	}

	return tokens, tokenSet, nil
}

func extensionAddRuntimeRestartTokens(ctx context.Context, old, updated config.Cluster, ext catalog.Extension, descriptorSaved bool, live []config.ClusterSpec, tokens []string, tokenSet map[string]bool) ([]string, map[string]bool, error) {
	for _, target := range live {
		oldTarget, _ := old.ClusterByName(target.Name)

		needed := extensionRestartNeeded(old, oldTarget, updated, target, ext.Name, descriptorSaved)
		if !needed && extensionClusterRunning(target) {
			pending, err := extensionPendingRestarts(ctx, target)
			if err != nil {
				return nil, nil, output.NewError(output.CodePrecondition,
					"cannot read "+target.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
					"Nothing was changed. Retry when the cluster answers.").
					WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": target.Name})
			}

			needed = len(pending) > 0
			if !needed && ext.Preload != "" {
				loaded, pendingPreload, err := extensionPreloadFacts(ctx, target)
				if err != nil {
					return nil, nil, output.NewError(output.CodePrecondition,
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

	return tokens, tokenSet, nil
}

func extensionAddStageSavedIntent(ctx context.Context, env *Env, old, updated config.Cluster, spec config.ClusterSpec, targets extensionTargetSet, ext catalog.Extension, raw []byte, retry string, rt engine.Runtime, intentSaved bool) error {
	// Capture outgoing definitions exactly once before any package effect. A
	// failure may already have written valid outgoing snapshots, so it must not
	// claim that nothing changed; the snapshots are retained.
	if err := extensionCaptureOutgoing(old); err != nil {
		return output.NewError(output.CodePrecondition,
			"recording the outgoing extension requirements before the change failed: "+err.Error(),
			output.ExitPrecondition, "No proposed extension intent was saved, and no package preparation, restart or extension SQL was requested; any outgoing requirement snapshots already written are retained. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{"reason": "extension_history_capture_failed", "database": spec.Name, "extension": ext.Name})
	}
	// Prepare the proposed packages and files for every affected target before
	// recording the incoming definitions or saving the intent. Package effects
	// are not rolled back, so a failure here must leave no incoming history,
	// saved intent, restart or SQL behind.
	for _, target := range targets.all() {
		if err := extensionPrepareStart(ctx, updated, target, nil, rt, env.Report); err != nil {
			hint := "Package installation may have partially run; installed packages are not removed. vaire did not request a restart or run extension SQL. "
			if intentSaved {
				hint += "cluster.toml already enables the extension; retry after fixing the package prerequisite."
			} else {
				hint += "cluster.toml was not changed; retry vaire ext add after fixing the package prerequisite."
			}

			return output.NewError(output.CodePartialFailure,
				"installing extension packages for "+ext.Name+" on "+target.Name+" failed: "+err.Error(),
				output.ExitStep, hint).
				WithDetails(map[string]any{
					"reason": "package_install_failed", "database": spec.Name, "extension": ext.Name,
					"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true, "failed_target": target.Name,
					"retry": retry, "completed": []string{}, "pending": targets.names(),
				})
		}
	}
	// Only now record the incoming definitions. A failure here may follow
	// package effects and may have appended part of the incoming history, so it
	// neither claims untouched packages nor a clean history.
	if err := extensionCaptureIncoming(updated); err != nil {
		return output.NewError(output.CodePartialFailure,
			"preparing extension packages for "+ext.Name+" succeeded, but recording its incoming requirements failed: "+err.Error(),
			output.ExitStep,
			"Package files may already be installed and the incoming requirement snapshot may be partially written and retained. cluster.toml is unchanged, and vaire did not request a restart or run extension SQL. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{
				"reason": "extension_history_capture_failed", "database": spec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
				"incoming_history_may_remain": true,
				"retry":                       retry, "completed": []string{}, "pending": targets.names(),
			})
	}

	newRaw, err := updated.Encode()
	if err != nil {
		intentHint := "cluster.toml was not changed;"
		if intentSaved {
			intentHint = "cluster.toml already enables the extension;"
		}

		return output.NewError(output.CodePartialFailure,
			"preparing extension packages for "+ext.Name+" succeeded and its incoming requirements were recorded, but encoding the extension intent failed: "+err.Error(),
			output.ExitStep,
			"Package files may already be installed; "+intentHint+" vaire did not request a restart or run extension SQL. Retry after fixing the extension configuration.").
			WithDetails(map[string]any{
				"reason": "extension_intent_save_failed", "database": spec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
				"incoming_history_retained": true,
				"retry":                     retry, "completed": []string{}, "pending": targets.names(),
			})
	}

	if string(newRaw) != string(raw) {
		if err := extensionSave(newRaw); err != nil {
			intentHint := "cluster.toml was not changed;"
			if intentSaved {
				intentHint = "cluster.toml already enables the extension;"
			}

			return output.NewError(output.CodePartialFailure,
				"preparing extension packages for "+ext.Name+" succeeded and its incoming requirements were recorded, but saving the extension intent failed: "+err.Error(),
				output.ExitStep,
				"Package files may already be installed; "+intentHint+" vaire did not request a restart or run extension SQL. Retry after fixing the configuration store.").
				WithDetails(map[string]any{
					"reason": "extension_intent_save_failed", "database": spec.Name, "extension": ext.Name,
					"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
					"incoming_history_retained": true,
					"retry":                     retry, "completed": []string{}, "pending": targets.names(),
				})
		}
	}

	return nil
}

func extensionAddConfigureFollowers(ctx context.Context, env *Env, updated config.Cluster, rt engine.Runtime, targets extensionTargetSet, tokenSet map[string]bool, retry string, spec config.ClusterSpec, ext catalog.Extension, restart bool) ([]output.Warning, []string, error) {
	var (
		warnings  []output.Warning
		completed []string
	)

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

			return warnings, nil, extensionPartialError(msg, "extension_apply_failed", spec.Name, ext.Name, restart, applied.Restarted(), applied,
				map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}

		if err := verifyTargetPreloads(ctx, updated, follower); err != nil {
			return warnings, nil, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", spec.Name, ext.Name, true, applied.Restarted(), applied,
				map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}

		completed = append(completed, follower.Name)
	}

	return warnings, completed, nil
}

func extensionAddConfigurePrimary(ctx context.Context, env *Env, updated config.Cluster, rt engine.Runtime, spec config.ClusterSpec, ext catalog.Extension, tokenSet map[string]bool, retry string, restart bool, completed []string) (engine.ApplyData, []output.Warning, error) {
	primary, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Confirm: extensionConfirmFor(spec.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	clearExtensionDeferred(primary)

	if err != nil || extensionApplyIncomplete(primary) || extensionRestartPending(primary, spec.Name) {
		msg := "the extension intent is saved and its followers are configured, but configuring " + spec.Name + " failed"
		if err != nil {
			msg += ": " + err.Error()
		}

		return primary, w, extensionPartialError(msg, "extension_apply_failed", spec.Name, ext.Name, restart, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}

	if err := verifyExtensionSettings(ctx, updated, spec, ext); err != nil {
		return primary, w, extensionPartialError("the primary did not apply the required extension settings: "+err.Error(),
			"extension_settings_missing", spec.Name, ext.Name, restart, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}

	if err := verifyTargetPreloads(ctx, updated, spec); err != nil {
		return primary, w, extensionPartialError("the primary did not load its required libraries: "+err.Error(),
			"extension_preload_missing", spec.Name, ext.Name, true, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}

	return primary, w, nil
}

func extensionAddRunRoles(ctx context.Context, env *Env, updated config.Cluster, rt engine.Runtime, spec config.ClusterSpec, updatedSpec config.ClusterSpec, ext catalog.Extension, retry string, restart bool, completed []string) (engine.ApplyData, extensionInstalledVersion, []output.Warning, error) {
	roles, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.RolesStepID(spec.Name)}, DB: spec.Name,
		Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	clearExtensionDeferred(roles)

	if err != nil || extensionApplyIncomplete(roles) || extensionRestartPending(roles, spec.Name) {
		msg := "the extension intent and every cluster's configuration are saved, but creating the SQL failed"
		if err != nil {
			msg += ": " + err.Error()
		}

		return roles, extensionInstalledVersion{}, w, extensionPartialError(msg, "extension_sql_failed", spec.Name, ext.Name, restart, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"sql"}})
	}

	have, err := extensionInstalled(ctx, updatedSpec)
	if err != nil {
		return roles, extensionInstalledVersion{}, w, extensionPartialError("the SQL ran, but verifying "+ext.Name+" failed: "+err.Error(),
			"extension_verify_failed", spec.Name, ext.Name, restart, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"verify"}})
	}

	got, has := have[ext.ExtName]
	if !has {
		return roles, extensionInstalledVersion{}, w, extensionPartialError(ext.Name+" was not created after the roles step",
			"extension_not_created", spec.Name, ext.Name, restart, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "completed": append(completed, spec.Name), "pending": []string{"sql"}})
	}

	return roles, got, w, nil
}

func extensionAddFollowerUpdateIfNeeded(ctx context.Context, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, got extensionInstalledVersion, primary, roles engine.ApplyData, tokens []string, retry string) (string, string, bool, error) {
	cmp, ordered := extensionVersionCompare(got.Version, got.Default)
	if !ordered {
		return "", "", false, extensionUpdateDirectionUnknown(updatedSpec.Name, ext.Name, got.Version, got.Default)
	}

	if cmp > 0 {
		return "", "", false, extensionDowngradeError(updatedSpec.Name, ext.Name, got.Version, got.Default)
	}

	supported, err := extensionUpdatePath(ctx, updatedSpec, ext.ExtName, got.Version, got.Default)
	if err != nil || !supported {
		return "", "", false, extensionPartialError(ext.Name+" is installed at "+got.Version+", but no supported update path to "+got.Default+" is available",
			"extension_update_unsupported", updatedSpec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
	}

	readiness, err := extensionPrimaryReadiness(ctx, updated, updatedSpec, ext)
	if err != nil {
		return "", "", false, extensionPartialError(ext.Name+" is installed at "+got.Version+", but its required primary prerequisites cannot be proven: "+err.Error(),
			"extension_update_prerequisites_unknown", updatedSpec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
	}

	if readiness.Deferred {
		return "", "", false, extensionPartialError(ext.Name+" is installed at "+got.Version+" but its required primary prerequisites are not effective: "+readiness.Detail,
			"extension_update_prerequisites", updatedSpec.Name, ext.Name, readiness.RestartPending, primary.Restarted(), primary,
			map[string]any{"retry": retry, "intent_saved": true, "pending": []string{"update"}, "version": got.Version, "default_version": got.Default})
	}

	sql := "ALTER EXTENSION " + pg.Quote(ext.ExtName) + " UPDATE TO " + extensionLiteral(got.Default) + ";\n"
	if err := extensionPG(updatedSpec).Exec(ctx, updatedSpec.DatabaseName(), sql, nil); err != nil {
		return "", "", false, extensionPartialError("updating "+ext.Name+" from "+got.Version+" to "+got.Default+" failed: "+err.Error(),
			"extension_update_failed", updatedSpec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
	}

	after, err := extensionInstalled(ctx, updatedSpec)
	if err != nil || after[ext.ExtName].Version != got.Default {
		return "", "", false, extensionPartialError("ALTER EXTENSION "+ext.Name+" did not reach "+got.Default,
			"extension_update_failed", updatedSpec.Name, ext.Name, len(tokens) > 0, true, roles,
			map[string]any{"retry": retry, "intent_saved": true, "version": got.Version, "default_version": got.Default})
	}

	return got.Version, got.Default, true, nil
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

	if err := verifyFollowerRuntimePreloads(spec, plan, loaded, preloadPending); err != nil {
		return err
	}

	if err := verifyFollowerRuntimeSettings(ctx, spec, plan); err != nil {
		return err
	}

	return verifyFollowerRuntimeSQL(spec, plan)
}

func verifyFollowerRuntimePreloads(spec config.ClusterSpec, plan steps.FollowerRuntimePlan, loaded map[string]bool, preloadPending bool) error {
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

	return nil
}

func verifyFollowerRuntimeSettings(ctx context.Context, spec config.ClusterSpec, plan steps.FollowerRuntimePlan) error {
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

	return nil
}

func verifyFollowerRuntimeSQL(spec config.ClusterSpec, plan steps.FollowerRuntimePlan) error {
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

// extensionConfirmFor authorizes one target's configuration apply. tokenSet
// is keyed by bare target name; the full "<target>/restart" text is kept in
// the tokens list used to validate the request and advertise retries.
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

	tokens, tokenSet, restartNeeded, retry, err := extensionRemoveTransitionTokens(ctx, env, updated, spec, targets, ext, restart)
	if err != nil {
		return Result{}, err
	}

	if !extensionClusterRunning(spec) {
		return Result{}, extensionClusterNotRunning(spec.Name, "ext remove")
	}

	if err := validateTargetExtensionMetadata(updated, targets.Source); err != nil {
		return Result{}, err
	}

	plans, err := prepareFollowerRuntimePlans(ctx, updated, recs, targets.all(), []catalog.Extension{ext})
	if err != nil {
		return Result{}, err
	}

	ctx = steps.WithFollowerRuntimePlans(ctx, plans)

	updatedSpec, _ := updated.ClusterByName(spec.Name)
	if err := extensionRemoveCaptureRequirements(ctx, old, spec, updated, updatedSpec, ext, retry, restartNeeded, targets); err != nil {
		return Result{}, err
	}

	sqlDropped, err := extensionRemoveDrop(ctx, spec, ext, installed, cascade)
	if err != nil {
		return Result{}, err
	}

	if err := extensionRemoveSaveIntent(updated, raw, enabled, spec, ext, retry, sqlDropped, restartNeeded, targets.names()); err != nil {
		return Result{}, err
	}

	env.Cluster = updated
	rt := runtimeFor(env, false)

	primary, warnings, err := extensionRemoveConfigurePrimary(ctx, env, updated, rt, spec, ext, tokenSet, retry, restartNeeded, sqlDropped, nil)
	if err != nil {
		return Result{}, err
	}

	completed := []string{spec.Name}
	pendingFollowers, pendingDetail, completed, followerWarnings, err := extensionRemoveConfigureFollowers(ctx, env, updated, rt, targets, tokenSet, retry, spec, ext, restartNeeded, sqlDropped, completed)
	warnings = append(warnings, followerWarnings...)

	if err != nil {
		return Result{}, err
	}

	if err := extensionVerifyAbsent(ctx, spec, ext.ExtName, sqlDropped, true); err != nil {
		return Result{}, err
	}

	if len(pendingFollowers) > 0 {
		return Result{}, extensionPartialError(
			"the primary is configured, but followers still replay or cannot yet prove the removed extension absent: "+strings.Join(pendingDetail, "; "),
			"extension_follower_replay_pending", spec.Name, ext.Name, restartNeeded, primary.Restarted(), primary,
			map[string]any{"retry": retry, "sql_dropped": sqlDropped, "intent_saved": true, "completed": completed, "pending": pendingFollowers})
	}

	res.Restarted = primary.Restarted()
	res.RestartRequired = restartNeeded

	res.RequiredConfirmTokens = append([]string{spec.Name}, tokens...)

	return Result{Data: res, Warnings: warnings}, nil
}

func extensionRemoveTransitionTokens(ctx context.Context, env *Env, updated config.Cluster, spec config.ClusterSpec, targets extensionTargetSet, ext catalog.Extension, restart bool) ([]string, map[string]bool, bool, string, error) {
	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)

	tokens, tokenSet, err := extensionPlanRestartTokens(ctx, updated, live, spec.Name)
	if err != nil {
		return nil, nil, false, "", err
	}

	restartNeeded, err := extensionPendingRestartNeeded(ctx, live, restart)
	if err != nil {
		return nil, nil, false, "", err
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
			return nil, nil, false, "", confirmRequired("removing "+ext.Name+" changes every affected cluster's configuration; "+token+" is required", token)
		}
	}

	return tokens, tokenSet, restartNeeded, extensionRemoveRetry(spec.Name, ext.Name, tokens), nil
}

func extensionPendingRestartNeeded(ctx context.Context, live []config.ClusterSpec, restart bool) (bool, error) {
	if restart {
		return true, nil
	}

	for _, target := range live {
		pending, err := extensionPendingRestarts(ctx, target)
		if err != nil {
			return false, output.NewError(output.CodePrecondition,
				"cannot read "+target.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": target.Name})
		}

		if len(pending) > 0 {
			return true, nil
		}
	}

	return false, nil
}

func extensionRemoveCaptureRequirements(ctx context.Context, old config.Cluster, spec config.ClusterSpec, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, retry string, restartNeeded bool, targets extensionTargetSet) error {
	// The reduced intent is a known subset of the old definitions (a removal
	// never introduces a proposed provider), so both snapshots are recorded
	// before the DROP. A capture failure can already have appended history; it
	// truthfully retains it and does not claim that nothing happened.
	if err := extensionCaptureRequirements(ctx, old, spec, updated, updatedSpec, "remove"); err != nil {
		details := extensionRemovePartialDetails(spec, ext, retry, false, restartNeeded)
		details["reason"] = "extension_history_capture_failed"
		details["outgoing_history_may_remain"] = true
		details["incoming_history_may_remain"] = true
		details["pending"] = targets.names()

		return output.NewError(output.CodePartialFailure,
			"recording extension requirements before the removal failed: "+err.Error(),
			output.ExitStep,
			"The extension SQL was not dropped and no configuration, restart or SQL was applied. Outgoing or reduced requirement snapshots may already be written and retained; retry after fixing the requirement history store.").
			WithDetails(details)
	}

	return nil
}

func extensionRemoveDrop(ctx context.Context, spec config.ClusterSpec, ext catalog.Extension, installed, cascade bool) (bool, error) {
	if !installed {
		return false, nil
	}

	sql := "DROP EXTENSION IF EXISTS " + pg.Quote(ext.ExtName)
	if cascade {
		sql += " CASCADE"
	}

	if err := extensionPG(spec).Exec(ctx, spec.DatabaseName(), sql+";\n", nil); err != nil {
		return false, output.WrapError(output.CodeExternalCommand,
			fmt.Errorf("drop %s from %s: %w", ext.Name, spec.Name, err), output.ExitExternal,
			"cluster.toml and every restart are unchanged; read vaire logs postgres --db "+spec.Name+".").
			WithDetails(map[string]any{"reason": "extension_drop_failed", "database": spec.Name, "extension": ext.Name})
	}

	return true, nil
}

func extensionRemoveSaveIntent(updated config.Cluster, raw []byte, enabled bool, spec config.ClusterSpec, ext catalog.Extension, retry string, sqlDropped, restartRequired bool, pending []string) error {
	newRaw, err := updated.Encode()
	if err != nil {
		return extensionRemoveFollowerIntentFailure("encoding the reduced cluster.toml", err, spec, ext, retry, sqlDropped, restartRequired, pending)
	}

	if enabled && string(newRaw) != string(raw) {
		if err := extensionSave(newRaw); err != nil {
			return extensionRemoveFollowerIntentFailure("saving the reduced cluster.toml", err, spec, ext, retry, sqlDropped, restartRequired, pending)
		}
	}

	return nil
}

func extensionRemoveConfigurePrimary(ctx context.Context, env *Env, updated config.Cluster, rt engine.Runtime, spec config.ClusterSpec, ext catalog.Extension, tokenSet map[string]bool, retry string, restartNeeded, sqlDropped bool, completed []string) (engine.ApplyData, []output.Warning, error) {
	primary, w, err := extensionApply(ctx, updated, rt, engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Confirm: extensionConfirmFor(spec.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	})
	clearExtensionDeferred(primary)

	if err != nil || extensionApplyIncomplete(primary) || extensionRestartPending(primary, spec.Name) {
		return primary, w, extensionPartialError("applying the primary's configuration failed",
			"extension_apply_failed", spec.Name, ext.Name, restartNeeded, primary.Restarted(), primary,
			map[string]any{"retry": retry, "sql_dropped": sqlDropped, "intent_saved": true, "completed": completed, "pending": []string{spec.Name}})
	}

	return primary, w, nil
}

func extensionRemoveConfigureFollowers(ctx context.Context, env *Env, updated config.Cluster, rt engine.Runtime, targets extensionTargetSet, tokenSet map[string]bool, retry string, spec config.ClusterSpec, ext catalog.Extension, restartNeeded, sqlDropped bool, completed []string) ([]string, []string, []string, []output.Warning, error) {
	var (
		warnings         []output.Warning
		pendingFollowers []string
		pendingDetail    []string
	)

	for _, follower := range targets.Running {
		applied, w, err := extensionApply(ctx, updated, rt, engine.Options{
			Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name,
			Confirm: extensionConfirmFor(follower.Name, tokenSet), Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
		})
		warnings = append(warnings, w...)

		clearExtensionDeferred(applied)

		if err != nil || extensionApplyIncomplete(applied) || extensionRestartPending(applied, follower.Name) {
			return nil, nil, nil, warnings, extensionPartialError("the primary is configured, but follower "+follower.Name+" failed",
				"extension_apply_failed", spec.Name, ext.Name, restartNeeded, applied.Restarted(), applied,
				map[string]any{"retry": retry, "sql_dropped": sqlDropped, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}

		if err := verifyTargetPreloads(ctx, updated, follower); err != nil {
			if pending, ok := errors.AsType[*steps.FollowerRuntimePendingError](err); ok {
				// The follower's SQL still replays the removed extension, or
				// its catalog is temporarily unreadable. Its old guard stays
				// in place; report the truthful pending state and keep the
				// supported retry.
				pendingFollowers = append(pendingFollowers, follower.Name)
				pendingDetail = append(pendingDetail, pending.Error())
				completed = append(completed, follower.Name+" (staged, pending)")

				continue
			}

			return nil, nil, nil, warnings, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", spec.Name, ext.Name, restartNeeded, applied.Restarted(), applied,
				map[string]any{"retry": retry, "sql_dropped": sqlDropped, "intent_saved": true, "completed": completed, "pending": []string{follower.Name}})
		}

		completed = append(completed, follower.Name)
	}

	return pendingFollowers, pendingDetail, completed, warnings, nil
}

func extensionRemoveFollowerIntentFailure(stage string, cause error, spec config.ClusterSpec, ext catalog.Extension, retry string, sqlDropped, restartRequired bool, pending []string) error {
	err := extensionRemoveIntentFailure(stage, cause, spec, ext, retry, sqlDropped, restartRequired)

	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return err
	}

	copied := *cliErr

	details := map[string]any{}
	if have, ok := cliErr.Details.(map[string]any); ok {
		maps.Copy(details, have)
	}

	details["completed"] = []string{}

	details["pending"] = append([]string{}, pending...)
	copied.Details = details

	return &copied
}

func refuseExtensionUnloadTargets(ctx context.Context, cfg config.Cluster, source string) error {
	recs, err := extensionRecords()
	if err != nil {
		return err
	}

	targets, err := extensionApplyTargetSetFor(cfg, recs, source)
	if err != nil {
		return err
	}

	live := append([]config.ClusterSpec{targets.Source}, targets.liveFollowers()...)
	for _, target := range live {
		if !extensionClusterRunning(target) {
			// A stopped target's catalog is unknown. Its managed-file fallback
			// would refuse the supported preload removal before the engine has
			// started it, so this pre-start guard covers only running targets.
			// The stopped target is started on its still-old managed runtime
			// and proven live after start, before any reduced configuration.
			continue
		}

		if err := extensionRefuseUnload(ctx, cfg, target, steps.FirstManagedConfig(target)); err != nil {
			return err
		}
	}

	return nil
}

func extensionSourceTransitionNeeded(ctx context.Context, cfg config.Cluster, source string, configSelected, rolesSelected bool) (bool, error) {
	// A scoped apply may not bypass a selected source/follower transition.
	return extensionTransitionNeededFor(ctx, cfg, source, configSelected, rolesSelected)
}

// applySelectionStartsClusters reports whether the request explicitly selects
// the engine's cluster step. Such a selection may start a stopped target but
// cannot execute an extension configuration/roles transition; its primary SQL
// still has to wait for the ordinary supported apply pass.
func applySelectionStartsClusters(steps []string) bool {
	if len(steps) == 0 {
		return true
	}

	for _, id := range steps {
		if base, _ := engine.SplitID(id); base == "postgres-cluster-v1" {
			return true
		}
	}

	return false
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

	if !extensionClusterRunning(spec) {
		// A stopped target's pg_extension is unknown, not empty. The apply
		// transition starts the engine-owned cluster before it re-reads the
		// catalog; a scoped start-only selection is allowed to proceed without
		// this preflight. Never infer that installed SQL is absent.
		return true, nil
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

	targets, err := extensionApplyTargetSetFor(cfg, recs, source)
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
		return applyExtensionTransitionsScoped(ctx, cfg, p)
	}

	if p.DB != "" {
		return applyExtensionTransitionsForDB(ctx, env, cfg, p)
	}

	return applyExtensionTransitionsForSources(ctx, env, cfg, p)
}

func applyExtensionTransitionsScoped(ctx context.Context, cfg config.Cluster, p stepParams) ([]output.Warning, func(), error) {
	if applySelectionStartsClusters(p.Steps) {
		// A selected start may bring a stopped cluster up, but it does not
		// execute configuration, roles or extension SQL. The supported
		// extension transition runs in a later apply that selects those
		// steps (or a full apply).
		return nil, nil, nil
	}

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

func applyExtensionTransitionsForDB(ctx context.Context, env *Env, cfg config.Cluster, p stepParams) ([]output.Warning, func(), error) {
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
		return applyExtensionTransitionsForFollower(ctx, env, cfg, p, spec)
	}

	if len(cfg.FollowerNames(p.DB)) == 0 {
		return nil, nil, nil
	}

	if !applySelectionMayTransition(env.Req) {
		needed, err := extensionSourceTransitionNeeded(ctx, cfg, p.DB, true, true)
		if err != nil {
			return nil, nil, err
		}

		if needed && !applySelectionStartsClusters(p.Steps) {
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

	return applyExtensionTransitionsForSource(ctx, env, cfg, p.DB, configSelected, rolesSelected)
}

func applyExtensionTransitionsForFollower(ctx context.Context, env *Env, cfg config.Cluster, p stepParams, spec config.ClusterSpec) ([]output.Warning, func(), error) {
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

	plans, err := prepareFollowerRuntimePlans(ctx, cfg, recs, []config.ClusterSpec{spec}, nil)
	if err != nil {
		return nil, nil, err
	}

	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	if err := refuseExtensionApplyTargets(ctx, cfg, extensionTargetSet{Source: spec}); err != nil {
		return nil, nil, err
	}

	if !extensionClusterRunning(spec) && extensionTargetPresent(cfg, spec) &&
		(applySelectionMayTransition(env.Req) || applySelectionStartsClusters(p.Steps)) {
		return extensionApplyFollowerStart(ctx, env, cfg, spec)
	}

	return nil, nil, nil
}

func extensionApplyFollowerStart(ctx context.Context, env *Env, cfg config.Cluster, spec config.ClusterSpec) ([]output.Warning, func(), error) {
	if err := extensionValidateStopped(ctx, cfg, spec); err != nil {
		return nil, nil, err
	}

	warnings, started, err := extensionStartStoppedTarget(ctx, env, cfg, spec)
	if err != nil {
		startedTargets := []string{}
		if started {
			startedTargets = append(startedTargets, spec.Name)
		}

		stage := extensionStartStageStart
		if started {
			stage = extensionStartStageUnload
		}

		return warnings, nil, extensionStartFailure(err, spec.Name, startedTargets, []string{}, spec.Name, stage, !started)
	}

	return warnings, nil, nil
}

func applyExtensionTransitionsForSource(ctx context.Context, env *Env, cfg config.Cluster, db string, configSelected, rolesSelected bool) ([]output.Warning, func(), error) {
	recs, err := extensionRecords()
	if err != nil {
		return nil, nil, err
	}

	targets, err := extensionApplyTargetSetFor(cfg, recs, db)
	if err != nil {
		return nil, nil, err
	}

	spec, _ := cfg.ClusterByName(db)
	if err := validateTargetExtensionMetadata(cfg, spec); err != nil {
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
	if err := refuseExtensionUnloadTargets(ctx, cfg, db); err != nil {
		return nil, release, err
	}

	needed, err := extensionTransitionNeededFor(ctx, cfg, db, configSelected, rolesSelected)
	if err != nil {
		return nil, release, err
	}

	if !needed {
		return nil, release, nil
	}

	warnings, err := extensionApplyTransitionForApply(ctx, env, cfg, db)
	if err != nil {
		if pending, ok := errors.AsType[*extensionApplyPendingError](err); ok {
			release()

			pending.Warnings = append(warnings, pending.Warnings...)

			return nil, nil, pending
		}
	}

	return warnings, release, err
}

func applyExtensionTransitionsForSources(ctx context.Context, env *Env, cfg config.Cluster, p stepParams) ([]output.Warning, func(), error) {
	recs, err := extensionRecords()
	if err != nil {
		return nil, nil, err
	}

	neededSources, names, err := extensionNeededSources(ctx, cfg, recs, p.Steps)
	if err != nil {
		return nil, nil, err
	}

	if len(neededSources) == 0 {
		return nil, nil, nil
	}

	release, err := extensionAcquireClusters(ctx, names, env.Req.Method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return nil, nil, err
	}

	plans, err := extensionSourceRuntimePlans(ctx, cfg, recs, neededSources)
	if err != nil {
		return nil, release, err
	}

	ctx = steps.WithFollowerRuntimePlans(ctx, plans)
	for _, source := range neededSources {
		if err := refuseExtensionUnloadTargets(ctx, cfg, source); err != nil {
			return nil, release, err
		}
	}

	warnings, pending, err := extensionApplySourcesInSequence(ctx, env, cfg, neededSources)
	if err != nil {
		return warnings, release, err
	}

	if pending == nil {
		return warnings, release, nil
	}

	release()

	return nil, nil, pending
}

func extensionNeededSources(ctx context.Context, cfg config.Cluster, recs backup.Records, selected []string) ([]string, []string, error) {
	var (
		neededSources []string
		names         []string
	)

	for _, source := range selectedTransitionSources(cfg, selected) {
		configSelected, rolesSelected := extensionSourceSelection(selected, source)

		needed, err := extensionTransitionNeededFor(ctx, cfg, source, configSelected, rolesSelected)
		if err != nil {
			return nil, nil, err
		}

		if !needed {
			continue
		}

		targets, err := extensionApplyTargetSetFor(cfg, recs, source)
		if err != nil {
			return nil, nil, err
		}

		if err := validateTargetExtensionMetadata(cfg, targets.Source); err != nil {
			return nil, nil, err
		}

		if err := preflightFollowerRuntime(ctx, cfg, recs, targets.all(), nil); err != nil {
			return nil, nil, err
		}

		neededSources = append(neededSources, source)
		names = append(names, targets.names()...)
	}

	return neededSources, names, nil
}

func extensionSourceRuntimePlans(ctx context.Context, cfg config.Cluster, recs backup.Records, sources []string) (map[steps.FollowerRuntimePlanIdentity]steps.FollowerRuntimePlan, error) {
	plans := map[steps.FollowerRuntimePlanIdentity]steps.FollowerRuntimePlan{}

	for _, source := range sources {
		targets, err := extensionApplyTargetSetFor(cfg, recs, source)
		if err != nil {
			return nil, err
		}

		more, err := prepareFollowerRuntimePlans(ctx, cfg, recs, targets.all(), nil)
		if err != nil {
			return nil, err
		}

		maps.Copy(plans, more)
	}

	return plans, nil
}

func extensionApplySourcesInSequence(ctx context.Context, env *Env, cfg config.Cluster, sources []string) ([]output.Warning, *extensionApplyPendingError, error) {
	var (
		warnings        []output.Warning
		pendingData     engine.ApplyData
		pendingClusters []string
		pendingTokens   []string
	)

	for _, source := range sources {
		more, err := extensionApplyTransitionForApply(ctx, env, cfg, source)
		warnings = append(warnings, more...)

		if err != nil {
			if pending, ok := errors.AsType[*extensionApplyPendingError](err); ok {
				mergeApplyData(&pendingData, pending.Data)
				pendingClusters = append(pendingClusters, pending.Pending...)
				pendingTokens = append(pendingTokens, pending.Tokens...)
				warnings = append(warnings, pending.Warnings...)

				continue
			}

			return warnings, nil, err
		}
	}

	if len(pendingClusters) == 0 {
		return warnings, nil, nil
	}

	sort.Strings(pendingClusters)
	pendingClusters = slices.Compact(pendingClusters)

	sort.Strings(pendingTokens)
	pendingTokens = slices.Compact(pendingTokens)

	sort.Strings(pendingData.ConfirmTokens)
	pendingData.ConfirmTokens = slices.Compact(pendingData.ConfirmTokens)
	sort.Strings(pendingData.RestartPending)
	pendingData.RestartPending = slices.Compact(pendingData.RestartPending)

	return warnings, &extensionApplyPendingError{
		Data: pendingData, Warnings: warnings, Pending: pendingClusters, Tokens: pendingTokens,
		Message: "the extension transition is staged but not complete; " + strings.Join(pendingClusters, ", ") + " still need a restart, replay or effective setting",
	}, nil
}

// extensionApplyPendingError reports that the safe staging work ran, but a
// restart, replay or dependent SQL remains. handleApply turns it into a
// successful pending result; it is not a failed apply.
type extensionApplyPendingError struct {
	Data     engine.ApplyData
	Warnings []output.Warning
	Pending  []string
	Tokens   []string
	Stage    string
	Message  string
}

func (e *extensionApplyPendingError) Error() string { return e.Message }

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

func extensionPendingResult(source string, data engine.ApplyData, pending, tokens []string, stage string) *extensionApplyPendingError {
	sort.Strings(tokens)
	tokens = slices.Compact(tokens)

	sort.Strings(pending)
	pending = slices.Compact(pending)

	var retry strings.Builder
	retry.WriteString("vaire apply --db " + source)

	for _, token := range tokens {
		retry.WriteString(" --confirm " + token)
	}

	data.ConfirmTokens = append(data.ConfirmTokens, tokens...)
	sort.Strings(data.ConfirmTokens)
	data.ConfirmTokens = slices.Compact(data.ConfirmTokens)

	return &extensionApplyPendingError{
		Data: data, Pending: pending, Tokens: tokens, Stage: stage,
		Message: "the extension transition is staged but not complete: " + strings.Join(pending, ", ") + " still need a restart, replay or effective setting",
		Warnings: []output.Warning{{
			Code:    "extension_apply_pending",
			Message: "staged work is saved; retry with " + retry.String(),
		}},
	}
}

// extensionValidateStoppedStart preserves a stopped follower's existing
// persisted guard for the engine-owned start. The old managed configuration and
// guard remain the runtime until the live catalog proves which preloads may be
// removed; rewriting the proposed guard here would unload an old library before
// that proof. A missing guard is valid: handoff may have proved the permanent
// configuration already supplies every obligation.
func extensionValidateStoppedStart(ctx context.Context, cfg config.Cluster, follower config.ClusterSpec) error {
	id, err := steps.FollowerRuntimeIdentity(cfg, follower, steps.FollowerRuntimeOperation(ctx))
	if err != nil {
		return err
	}

	if _, ok := steps.FollowerRuntimePlanFor(ctx, id); !ok {
		return output.NewError(output.CodePrecondition,
			"no complete safe runtime plan is available for stopped follower "+follower.Name,
			output.ExitPrecondition, "Nothing was started. Retry apply so vaire can rebuild the follower's guarded runtime.").
			WithDetails(map[string]any{"reason": "follower_runtime_snapshot_missing", "database": follower.Name})
	}

	if _, _, err := backup.ReadCopyGuard(cfg.Server.DataDirectory(follower.Name)); err != nil {
		return output.NewError(output.CodePrecondition,
			"cannot read "+follower.Name+"'s persisted copy guard before starting it: "+err.Error(),
			output.ExitPrecondition, "Nothing was started. Fix the follower's guard or configuration, then retry apply.").
			WithDetails(map[string]any{"reason": "extension_guard_unproven", "database": follower.Name})
	}

	return nil
}

const (
	extensionStartStageStart    = "start"
	extensionStartStageUnload   = "unload_proof"
	extensionStartErrorReason   = "extension_start_partial"
	extensionStartUnknownReason = "extension_start_state_unknown"
)

// extensionStartFailure turns a start or post-start unload-proof failure into
// the transition's public partial error. A start failure is explicitly
// unknown rather than reported as stopped; a proof failure records the targets
// already observed running. The old configuration remains in every case and no
// reduced configuration or SQL was applied by this transition.
func extensionStartFailure(err error, database string, started, pending []string, failed, stage string, startUnknown bool) error {
	details := map[string]any{}

	var base *output.CLIError
	if errors.As(err, &base) {
		switch have := base.Details.(type) {
		case map[string]any:
			maps.Copy(details, have)
		case nil:
		default:
			details["cause_details"] = have
		}
	}

	if _, ok := details["reason"]; !ok {
		details["reason"] = extensionStartErrorReason
	}

	details["database"] = database
	details["outcome"] = "partial"
	details["partial_reason"] = extensionStartErrorReason
	details["started"] = len(started) > 0
	details["started_targets"] = append([]string{}, started...)
	details["pending_targets"] = append([]string{}, pending...)
	details["failed_target"] = failed
	details["failed_stage"] = stage
	details["start_attempted"] = true
	details["start_unknown"] = startUnknown
	details["configuration_staged"] = false
	details["sql_run"] = false
	details["packages_may_remain"] = true

	var hint string

	if stage == extensionStartStageUnload {
		details["old_configuration_retained"] = true
		hint = failed + " was started on its old configuration; the reduced configuration and SQL were not applied. Keep the library that the installed extension still needs; this release does not remove extensions. Run vaire apply --db " + database + ", then retry apply."
	} else {
		details["start_unknown_reason"] = extensionStartUnknownReason
		hint = "The start of " + failed + " was attempted, but its final state is unknown. Configuration and SQL were not applied; package files may already be installed. Check vaire status --db " + failed + ", then retry apply."
	}

	if base != nil {
		copied := *base
		copied.Details = details
		copied.Hint = hint

		return &copied
	}

	return output.NewError(output.CodeStepFailed, err.Error(), output.ExitStep, hint).WithDetails(details)
}

// extensionStartStoppedTarget starts one stopped, materialized target through
// the engine's own cluster step. started reports that the target was observed
// answering after the start, not merely that a start was attempted.
func extensionStartStoppedTarget(ctx context.Context, env *Env, cfg config.Cluster, spec config.ClusterSpec) ([]output.Warning, bool, error) {
	if extensionClusterRunning(spec) || !extensionTargetPresent(cfg, spec) {
		return nil, false, nil
	}

	rt := runtimeFor(env, false)
	applied, warnings, err := extensionApply(ctx, cfg, rt, engine.Options{
		Only: []string{steps.ClusterStepID(spec.Name)}, DB: spec.Name,
		Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
	})
	clearExtensionDeferred(applied)

	if err != nil {
		return warnings, false, err
	}

	if !extensionClusterRunning(spec) {
		return warnings, false, output.NewError(output.CodePrecondition,
			spec.Name+" did not answer after its extension-preparation start",
			output.ExitPrecondition, "Read vaire logs postgres --db "+spec.Name+", then retry apply.").
			WithDetails(map[string]any{"reason": extensionStartUnknownReason, "database": spec.Name})
	}
	// The target is now answering on its old managed configuration. Prove the
	// reduced intent's preload removal against the actual catalog before any
	// configuration write can apply it. A stopped target's pre-start proof is
	// deliberately deferred to this point.
	if err := extensionRefuseUnload(ctx, cfg, spec, steps.FirstManagedConfig(spec)); err != nil {
		return warnings, true, err
	}

	return warnings, true, nil
}

// refuseExtensionApplyTargets validates the complete transition target set
// before any guard write, package effect or start. Ordinary apply has no
// owning continuation for an activation fence, so every phase, routing
// included, is refused. Materialisation and unfinished-follow guards use the
// same contract as the ordinary apply entry points.
func refuseExtensionApplyTargets(ctx context.Context, cfg config.Cluster, targets extensionTargetSet) error {
	names := targets.names()
	for _, name := range names {
		if err := refuseAnyActivationFence(cfg, name); err != nil {
			return err
		}

		if err := refuseUnfinishedFollowApply(cfg, name); err != nil {
			return err
		}

		if err := guardMaterialisationApply(ctx, cfg, name); err != nil {
			return err
		}
	}

	return nil
}

// extensionStartStoppedTargets makes stopped, materialized source/follower
// targets authoritative before any catalog-dependent decision. The existing
// persisted guard and old managed runtime are validated and left in place,
// then the engine starts the source and its followers. A stopped target is
// unknown runtime, never evidence of absent installed extras; after this
// returns the caller must re-read the real catalog and defer anything still
// pending to the supported normal pass.
func extensionStartStoppedTargets(ctx context.Context, env *Env, cfg config.Cluster, targets extensionTargetSet) ([]output.Warning, error) {
	var warnings []output.Warning

	if !extensionClusterRunning(targets.Source) && !extensionTargetPresent(cfg, targets.Source) {
		return nil, nil
	}

	for _, follower := range targets.Stopped {
		var err error

		ctx, err = extensionEnsureStoppedFollowerPlan(ctx, cfg, follower)
		if err != nil {
			return warnings, err
		}

		if err := extensionValidateStopped(ctx, cfg, follower); err != nil {
			return warnings, err
		}
	}

	startTargets := make([]config.ClusterSpec, 0, 1+len(targets.Stopped))
	if !extensionClusterRunning(targets.Source) {
		startTargets = append(startTargets, targets.Source)
	}

	startTargets = append(startTargets, targets.Stopped...)

	started := make([]string, 0, len(startTargets))
	for i, target := range startTargets {
		more, wasStarted, err := extensionStartStoppedTarget(ctx, env, cfg, target)

		warnings = append(warnings, more...)
		if wasStarted {
			started = append(started, target.Name)
		}

		if err != nil {
			stage, pending, unknown := extensionStartStageStart, targetNames(startTargets[i+1:]), true
			if wasStarted {
				stage, pending, unknown = extensionStartStageUnload, targetNames(startTargets[i+1:]), false
			}

			return warnings, extensionStartFailure(err, targets.Source.Name, started, pending, target.Name, stage, unknown)
		}
	}

	return warnings, nil
}

func extensionEnsureStoppedFollowerPlan(ctx context.Context, cfg config.Cluster, follower config.ClusterSpec) (context.Context, error) {
	id, err := steps.FollowerRuntimeIdentity(cfg, follower, steps.FollowerRuntimeOperation(ctx))
	if err != nil {
		return ctx, err
	}

	if _, ok := steps.FollowerRuntimePlanFor(ctx, id); ok {
		return ctx, nil
	}
	// The apply caller normally prepared this plan under its locks.
	// Keep the guard boundary self-contained for direct callers that
	// reached the same transition without one.
	recs, err := extensionRecords()
	if err != nil {
		return ctx, err
	}

	plans, err := prepareFollowerRuntimePlans(ctx, cfg, recs, []config.ClusterSpec{follower}, nil)
	if err != nil {
		return ctx, err
	}

	return steps.WithFollowerRuntimePlans(ctx, plans), nil
}

func targetNames(specs []config.ClusterSpec) []string {
	out := make([]string, 0, len(specs))
	for _, spec := range specs {
		out = append(out, spec.Name)
	}

	return out
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

	targets, err := extensionApplyTargetSetFor(cfg, recs, source)
	if err != nil {
		return nil, err
	}

	if len(targets.Running) == 0 && len(targets.Stopped) == 0 {
		return nil, nil
	}

	followers := targets.liveFollowers()
	if err := extensionApplyInheritedIntent(cfg, source, targets.Source, followers); err != nil {
		return nil, err
	}

	if err := refuseExtensionApplyTargets(ctx, cfg, targets); err != nil {
		return nil, err
	}

	if !extensionTargetPresent(cfg, targets.Source) {
		// A first bootstrap has no previous extension runtime to transition.
		// The ordinary engine pass creates and starts the cluster.
		return nil, nil
	}

	warnings, err := extensionStartStoppedTargets(ctx, env, cfg, targets)
	if err != nil {
		return warnings, err
	}

	rt := runtimeFor(env, false)

	live := append([]config.ClusterSpec{targets.Source}, followers...)
	retryAll := extensionApplyRetry(cfg.Name(), source, engine.Confirmation{Clusters: targetNames(live)})

	needed, err := extensionApplyNeeded(ctx, cfg, rt, targets.Source, source, live)
	if err != nil {
		return warnings, err
	}

	if !needed {
		return warnings, nil
	}

	if err := extensionRefuseUnload(ctx, cfg, targets.Source, steps.FirstManagedConfig(targets.Source)); err != nil {
		return warnings, err
	}

	confirmFor := extensionApplyConfirmFor(env, cfg.Name())
	stage, err := extensionApplyFollowersStage(ctx, env, cfg, rt, source, followers, retryAll, confirmFor)

	warnings = append(warnings, stage.Warnings...)
	if err != nil {
		return warnings, err
	}

	if len(stage.Pending) > 0 {
		return warnings, extensionPendingResult(source, stage.Data, stage.Pending, stage.PendingTokens, "followers")
	}

	more, err := extensionApplySourceStages(ctx, env, cfg, source, rt, retryAll, confirmFor, stage)
	warnings = append(warnings, more...)

	return warnings, err
}

func extensionApplySourceStages(ctx context.Context, env *Env, cfg config.Cluster, source string, rt engine.Runtime, retryAll string, confirmFor func(string) engine.Confirmation, stage extensionApplyStage) ([]output.Warning, error) {
	var warnings []output.Warning
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
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": stage.Completed, "pending": []string{source}})
	}

	mergeApplyData(&stage.Data, applied)

	if extensionApplyIncomplete(applied) {
		stage.Pending = append(stage.Pending, source)
		stage.PendingTokens = append(stage.PendingTokens, source+"/restart")

		return warnings, extensionPendingResult(source, stage.Data, stage.Pending, stage.PendingTokens, "primary_config")
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
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": append(stage.Completed, source), "pending": []string{"sql"}})
	}

	mergeApplyData(&stage.Data, roles)

	if extensionApplyIncomplete(roles) {
		// Roles and other SQL ran, but the dependent extension SQL is deferred
		// until its own required libraries/settings are effective. A pure
		// reload-only setting wait needs no synthetic restart token.
		stage.Pending = append(stage.Pending, source)
		if extensionWarningCode(w, "extension_pending_restart") || !extensionWarningCode(w, "extension_settings_pending") {
			stage.PendingTokens = append(stage.PendingTokens, source+"/restart")
		}

		return warnings, extensionPendingResult(source, stage.Data, stage.Pending, stage.PendingTokens, "sql")
	}

	if extensionRestartPending(roles, source) {
		return warnings, extensionPartialError("the configuration is saved, but the primary's extension SQL left a pending restart",
			"extension_sql_failed", source, "", true, true, roles,
			map[string]any{"retry": retryAll, "intent_saved": true, "completed": append(stage.Completed, source), "pending": []string{"sql"}})
	}

	if sourceRestartPending {
		// The configuration is staged and dependent SQL already ran because
		// its own required libraries/settings were effective. An unrelated
		// pending postmaster setting still leaves the apply truthfully pending.
		stage.Pending = append(stage.Pending, source)
		stage.PendingTokens = append(stage.PendingTokens, source+"/restart")

		return warnings, extensionPendingResult(source, stage.Data, stage.Pending, stage.PendingTokens, "primary_restart")
	}

	return warnings, nil
}

func extensionApplyInheritedIntent(cfg config.Cluster, source string, sourceSpec config.ClusterSpec, followers []config.ClusterSpec) error {
	for _, follower := range followers {
		if !sameNames(cfg, follower.Extensions, sourceSpec.Extensions) ||
			!sameRecovery(cfg, follower.RecoveryExtensions, sourceSpec.RecoveryExtensions) {
			return output.NewError(output.CodePrecondition,
				follower.Name+"'s extension intent differs from "+source+"'s inherited intent",
				output.ExitPrecondition, "Run vaire config set on "+source+" to propagate the change, then apply the source/follower transition.").
				WithDetails(map[string]any{"reason": "follower_extension_intent", "database": follower.Name, "source": source})
		}
	}

	return nil
}

func extensionApplyNeeded(ctx context.Context, cfg config.Cluster, rt engine.Runtime, sourceSpec config.ClusterSpec, source string, live []config.ClusterSpec) (bool, error) {
	needed := false

	for _, target := range live {
		plan, err := extensionPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: []string{steps.ConfigStepID(target.Name)}, DB: target.Name})
		if err != nil {
			return false, err
		}

		if plan.WouldApply > 0 || len(plan.RestartPending) > 0 {
			needed = true
		}
	}
	// A roles-only transition still matters when the source is missing one of
	// its required SQL extensions. Without this, the normal engine pass would
	// create that extension before the follower's runtime is verified.
	sourceInstalled, err := extensionInstalled(ctx, sourceSpec)
	if err != nil {
		return false, output.NewError(output.CodePrecondition,
			"cannot read "+source+"'s installed extensions before its dependent SQL: "+err.Error(),
			output.ExitPrecondition, "Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": source})
	}

	desiredSQL, err := steps.DesiredExtensionNames(cfg, sourceSpec)
	if err != nil {
		return false, err
	}

	for _, ext := range desiredSQL {
		if ext.ExtName != "" && !hasInstalled(sourceInstalled, ext.ExtName) {
			needed = true
			break
		}
	}

	return needed, nil
}

func extensionApplyConfirmFor(env *Env, server string) func(string) engine.Confirmation {
	serverToken := hasConfirmToken(env.Req, server+"/restart")

	return func(name string) engine.Confirmation {
		if serverToken || hasConfirmToken(env.Req, name+"/restart") {
			return engine.Confirmation{Clusters: []string{name}}
		}

		return engine.Confirmation{}
	}
}

type extensionApplyStage struct {
	Data          engine.ApplyData
	Warnings      []output.Warning
	Completed     []string
	Pending       []string
	PendingTokens []string
}

func extensionApplyFollowersStage(ctx context.Context, env *Env, cfg config.Cluster, rt engine.Runtime, source string, followers []config.ClusterSpec, retryAll string, confirmFor func(string) engine.Confirmation) (extensionApplyStage, error) {
	var stage extensionApplyStage

	for _, follower := range followers {
		applied, w, err := extensionApply(ctx, cfg, rt, engine.Options{
			Only: []string{steps.ConfigStepID(follower.Name)}, DB: follower.Name,
			Confirm: confirmFor(follower.Name), Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
		})
		stage.Warnings = append(stage.Warnings, w...)

		clearExtensionDeferred(applied)

		if err != nil {
			return stage, extensionPartialError("the configuration is saved, but follower "+follower.Name+" failed before the primary",
				"extension_apply_failed", source, "", true, applied.Restarted(), applied,
				map[string]any{"retry": retryAll, "intent_saved": true, "completed": stage.Completed, "pending": []string{follower.Name}})
		}

		mergeApplyData(&stage.Data, applied)

		if extensionApplyIncomplete(applied) || extensionRestartPending(applied, follower.Name) {
			stage.Pending = append(stage.Pending, follower.Name)
			stage.PendingTokens = append(stage.PendingTokens, follower.Name+"/restart")

			continue
		}

		if err := verifyTargetPreloads(ctx, cfg, follower); err != nil {
			if followerPending, ok := errors.AsType[*steps.FollowerRuntimePendingError](err); ok {
				stage.Pending = append(stage.Pending, follower.Name)

				switch followerPending.Reason {
				case "extension_preload_pending", "extension_settings_pending":
					stage.PendingTokens = append(stage.PendingTokens, follower.Name+"/restart")
				}

				stage.Warnings = append(stage.Warnings, output.Warning{Code: followerPending.Reason, Message: followerPending.Error()})

				continue
			}

			return stage, extensionPartialError("follower "+follower.Name+" did not apply its complete runtime: "+err.Error(),
				"extension_preload_missing", source, "", true, applied.Restarted(), applied,
				map[string]any{"retry": retryAll, "intent_saved": true, "completed": stage.Completed, "pending": []string{follower.Name}})
		}

		stage.Completed = append(stage.Completed, follower.Name)
	}

	return stage, nil
}

func extensionRestartTokensFor(cfg config.Cluster, source string) []string {
	followers := cfg.FollowerNames(source)

	out := make([]string, 0, len(followers))
	for _, f := range followers {
		out = append(out, f+"/restart")
	}

	sort.Strings(out)

	return out
}
