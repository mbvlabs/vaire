package node

import (
	"context"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

type configSetParams struct {
	TOML             string            `json:"toml"`
	Set              map[string]string `json:"set"`
	BaseRevision     string            `json:"base_revision"`
	ExposureProposal string            `json:"exposure_proposal"`
}

type configValidateParams struct {
	TOML string `json:"toml"`
}

var configSetStore = func() state.FileStore { return state.FileStore{} }
var configSetSchedules = checkSchedules
var configSetHostname = syncNodeHostname
var configSetPlan = engine.Plan
var nodeConfigStore = func() state.Store { return configSetStore() }

func handleConfigGet(_ context.Context, env *Env) (Result, error) {
	store := state.FileStore{}

	raw, err := os.ReadFile(state.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitWorkspace, "Run vaire server init first.")
		}

		return Result{}, err
	}

	revision, err := store.Revision()
	if err != nil {
		return Result{}, err
	}

	if env != nil && env.DeviceIdentity != nil {
		// The managed wire contract is the typed desired state, never raw
		// contents or comments potentially written by a host administrator.
		cfg, err := config.ParseCluster(raw)
		if err != nil {
			return Result{}, output.NewError(output.CodeConfig, "the node's desired state is invalid", output.ExitConfig, "")
		}

		if memberRead(env) {
			// Use the file just read, not an independently loaded snapshot.
			copyEnv := *env
			copyEnv.Cluster = cfg
			cfg = memberConfig(&copyEnv, cfg)
			revision = ""
		}

		raw, err = cfg.Encode()
		if err != nil {
			return Result{}, err
		}
	}

	return Result{Data: map[string]string{"toml": string(raw), "revision": revision}}, nil
}

func handleConfigSet(ctx context.Context, env *Env) (Result, error) {
	ctx = steps.WithFollowerRuntimeOperation(ctx, env.Req.RequestID)

	var p configSetParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	store := nodeConfigStore()

	current, err := store.Revision()
	if err != nil {
		return Result{}, err
	}

	if p.BaseRevision != current {
		return Result{}, output.NewError(
			output.CodePrecondition,
			"cluster.toml changed since it was read",
			output.ExitPrecondition,
			"Re-read the configuration and retry.",
		).WithDetails(map[string]any{"reason": "config_changed", "base_revision": p.BaseRevision, "revision": current})
	}

	raw, cfg, old, err := prepareConfigSet(store, p)
	if err != nil {
		return Result{}, err
	}

	if cfg.Server.AdminUser != old.Server.AdminUser || cfg.Server.DataRoot != old.Server.DataRoot {
		return Result{}, trustedHostAdminRequired()
	}

	if err := checkExtensionPackageChange(old, cfg, env.TrustedHostAdmin); err != nil {
		return Result{}, err
	}

	// These pure policy checks run before protection verification so a
	// combined destructive request cannot consume a TOTP code before it is
	// refused. Deleting a database declaration belongs to db drop, not a
	// configuration edit.
	if err := refuseClusterDeclarationRemoval(old, cfg); err != nil {
		return Result{}, err
	}

	if err := refuseExtensionSelectionReduction(old, cfg); err != nil {
		return Result{}, err
	}

	if err := guardRecoveryFloorChanges(old, cfg, runtimeFor(env, true).Host); err != nil {
		return Result{}, err
	}

	if err := checkCPUProposalWithRequests(ctx, old, cfg, p.Set); err != nil {
		return Result{}, err
	}

	if err := configSetSchedules(ctx, cfg); err != nil {
		return Result{}, err
	}

	var save func() error

	save, err = configExposureSave(ctx, env, store, raw, old, cfg, p, current)
	if err != nil {
		return Result{}, err
	}

	if err := configSetSafetyChecks(ctx, env, old, cfg); err != nil {
		return Result{}, err
	}

	if err := checkExtensionConfigChange(ctx, env, old, cfg); err != nil {
		return Result{}, err
	}

	extensionWarnings := extensionIntentWarnings(old, cfg)
	// A dry run previews the plan for the new file without saving it.
	revision, synced, err := saveConfigSet(ctx, env, store, old, cfg, p, current, save)
	if err != nil {
		return Result{}, err
	}

	env.Cluster = cfg
	rt := runtimeFor(env, true)
	plan, planErr := configSetPlan(ctx, steps.Catalog(), cfg, rt, engine.Options{})
	data := map[string]any{"revision": revision, "plan": plan, "saved": !env.Req.DryRun}

	warnings := append(append(synced, extensionWarnings...), oversubscriptionWarnings(cfg, rt.Host)...)
	if env.Req.DryRun {
		if increases := config.ExposureIncreases(old, cfg); len(increases) > 0 {
			data["exposure_increases"] = increases
			data["exposure_proposal"] = exposureProposal(current, raw)

			addExposureFactorStatus(data, env)

			warnings = append(warnings, output.Warning{
				Code:    "exposure_increase",
				Message: "proposed configuration widens access or enables an exposure control: " + strings.Join(increases, ", "),
			})
		}
	}

	if planErr != nil {
		warnings = append(warnings, output.Warning{Code: "plan_unavailable", Message: "the change was saved but the plan preview could not be computed: " + planErr.Error()})
	}

	return Result{Data: data, Warnings: warnings}, nil
}

func addExposureFactorStatus(data map[string]any, env *Env) {
	if status, err := operatorFactorStatus(env); err == nil {
		data["exposure_factors"] = status
	}
}

// refuseClusterDeclarationRemoval keeps cluster deletion on its guarded path.
// Removing any existing primary or follower declaration from cluster.toml
// would bypass db drop's protection, follower, stanza, role and cleanup work,
// so a configuration edit must refuse it before other safety checks.
func refuseClusterDeclarationRemoval(old, updated config.Cluster) error {
	for _, spec := range old.Clusters {
		if _, ok := updated.ClusterByName(spec.Name); ok {
			continue
		}

		return output.NewError(output.CodePrecondition,
			"removing the database declaration for "+spec.Name+" is not supported",
			output.ExitPrecondition,
			"Drop it explicitly with vaire db drop "+spec.Name+" --confirm "+spec.Name+"; a configuration edit never deletes a database. Nothing was changed.").
			WithDetails(map[string]any{"reason": "database_drop_required", "database": spec.Name})
	}

	return nil
}

// prepareConfigSet builds the desired configuration and propagates a
// source's extension intent to its followers before validation.
func prepareConfigSet(store state.Store, p configSetParams) ([]byte, config.Cluster, config.Cluster, error) {
	raw, err := newConfigBytes(store, p)
	if err != nil {
		return nil, config.Cluster{}, config.Cluster{}, err
	}

	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return nil, config.Cluster{}, config.Cluster{}, err
	}

	old, _, oldErr := store.Load()
	if oldErr == nil {
		propagated, changed, err := propagateFollowerExtensions(old, cfg)
		if err != nil {
			return nil, config.Cluster{}, config.Cluster{}, err
		}

		if changed {
			cfg = propagated
			if raw, err = cfg.Encode(); err != nil {
				return nil, config.Cluster{}, config.Cluster{}, err
			}
		}
	}

	if err := cfg.Validate(); err != nil {
		return nil, config.Cluster{}, config.Cluster{}, config.ValidationError(err)
	}

	old, _, err = store.Load()
	if err != nil {
		return nil, config.Cluster{}, config.Cluster{}, err
	}

	return raw, cfg, old, nil
}

func configSetSafetyChecks(ctx context.Context, env *Env, old, cfg config.Cluster) error {
	if err := checkProtectionAvailable(newlyProtected(old, cfg), env); err != nil {
		return err
	}

	// Widening changes already verified the factor for this exact config
	// proposal. In particular, don't consume the same TOTP a second time.
	if !env.Req.DryRun && len(config.ExposureIncreases(old, cfg)) == 0 {
		if len(Unprotected(old, cfg)) != 0 {
			if err := verifyOperatorApproval(ctx, env); err != nil {
				return err
			}
		}
	}

	return checkRepoMove(ctx, old, cfg)
}

func saveConfigSet(ctx context.Context, env *Env, store state.Store, old, cfg config.Cluster, p configSetParams, current string, save func() error) (string, []output.Warning, error) {
	if env.Req.DryRun {
		return current, nil, nil
	}

	if err := invalidateCPUProofs(cpuProofTargets(old, cfg, p.Set)); err != nil {
		return "", nil, err
	}
	// vaire_admin's write access follows the protection before the file says
	// so: a protection is never saved while vaire_admin can still write to
	// the database.
	synced, err := syncProtectedAccess(ctx, env, old, cfg)
	if err != nil {
		return "", nil, err
	}

	if err := save(); err != nil {
		return "", nil, err
	}

	if err := configSetHostname(cfg); err != nil {
		return "", nil, err
	}

	revision, err := store.Revision()
	if err != nil {
		return "", nil, err
	}

	return revision, synced, nil
}

func exposureProposal(base string, raw []byte) string {
	return state.RevisionOf(append([]byte(base+"\n"), raw...))
}

func configExposureSave(ctx context.Context, env *Env, store state.Store, raw []byte, old, cfg config.Cluster, p configSetParams, current string) (func() error, error) {
	if env.Req.DryRun {
		return nil, nil
	}

	if len(config.ExposureIncreases(old, cfg)) == 0 {
		return func() error { return store.Save(raw) }, nil
	}

	approved, ok := store.(interface {
		SaveApproved([]byte, string, func() error) error
	})
	if !ok {
		return nil, output.NewError(output.CodePrecondition, "config store does not support exposure approval", output.ExitPrecondition, "")
	}

	if p.ExposureProposal != exposureProposal(current, raw) {
		return nil, output.NewError(output.CodePrecondition, "the exposure proposal is missing or no longer matches", output.ExitPrecondition,
			"Preview the exact change again before approving it.").WithDetails(map[string]any{"reason": "exposure_proposal_changed"})
	}

	if err := verifyOperatorApproval(ctx, env); err != nil {
		return nil, err
	}

	// The proof is internal to this request: same raw config and same base
	// revision, checked again by the store under its activation lock.
	return func() error { return approved.SaveApproved(raw, current, func() error { return nil }) }, nil
}

func checkExtensionConfigChange(ctx context.Context, env *Env, old, cfg config.Cluster) error {
	if err := refuseUnsafeExtensionConfigRemoval(ctx, old, cfg); err != nil {
		return err
	}

	if err := refusePendingConfigChanges(old, cfg); err != nil {
		return err
	}

	var followers []config.ClusterSpec

	for _, spec := range cfg.Clusters {
		if spec.IsFollower() {
			followers = append(followers, spec)
		}
	}

	recs, err := extensionRecords()
	if err != nil {
		return err
	}

	if err := preflightFollowerRuntime(ctx, cfg, recs, followers, nil); err != nil {
		return err
	}

	for _, spec := range cfg.Clusters {
		if spec.IsFollower() {
			continue
		}

		if err := validateTargetExtensionDescriptors(cfg, spec); err != nil {
			return err
		}

		if sourceHasFollowers(cfg, spec.Name) {
			if err := validateTargetExtensionCopySafety(cfg, spec, false); err != nil {
				return err
			}
		}
	}

	if !env.Req.DryRun {
		if err := captureExtensionChange(old, cfg); err != nil {
			return output.NewError(output.CodePrecondition,
				"recording extension requirements before the configuration change failed: "+err.Error(),
				output.ExitPrecondition, "Nothing was saved. Retry after fixing the requirement history store.").
				WithDetails(map[string]any{"reason": "extension_history_capture_failed"})
		}
	}

	return nil
}

// newConfigBytes returns the desired-state bytes for a config.set request,
// either the supplied TOML or the current file with the dotted keys applied.
func newConfigBytes(store state.Store, p configSetParams) ([]byte, error) {
	if p.TOML != "" && len(p.Set) > 0 {
		return nil, output.NewError(output.CodeUsage, "pass either toml or set, not both", output.ExitUsage, "")
	}

	cfg, _, err := store.Load()
	if err != nil {
		return nil, err
	}

	if p.TOML != "" {
		updated, err := config.ParseCluster([]byte(p.TOML))
		if err != nil {
			return nil, output.NewError(output.CodeConfig, "the supplied cluster.toml does not parse: "+err.Error(), output.ExitConfig, "")
		}

		if changed := config.ImmutableChanges(cfg, updated); len(changed) > 0 {
			return nil, output.NewError(
				output.CodePrecondition,
				"the new cluster.toml changes keys that are immutable after init: "+strings.Join(changed, ", "),
				output.ExitPrecondition,
				config.ImmutableHint(),
			).WithDetails(map[string]any{"reason": "immutable_key", "keys": changed})
		}

		return []byte(p.TOML), nil
	}

	for key, value := range p.Set {
		if err := cfg.Set(key, value); err != nil {
			return nil, err
		}
	}

	return cfg.Encode()
}

func handleConfigValidate(_ context.Context, env *Env) (Result, error) {
	var p configValidateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	raw := []byte(p.TOML)
	if memberRead(env) && p.TOML != "" {
		return Result{}, deviceAccessDenied()
	}

	if p.TOML == "" {
		file, err := os.ReadFile(state.Path)
		if err != nil {
			if memberRead(env) {
				return Result{}, output.NewError(output.CodePrecondition, "server configuration could not be checked", output.ExitPrecondition, "Contact a server administrator.")
			}

			return Result{}, output.NewError(output.CodeClusterNotInitialized, "this node has no cluster.toml", output.ExitWorkspace, "Run vaire server init first.")
		}

		raw = file
	}

	problems := validateProblems(raw)
	if memberRead(env) && len(problems) != 0 {
		problems = []string{"server configuration is invalid; contact a server administrator"}
	}

	return Result{Data: map[string]any{"valid": len(problems) == 0, "problems": problems}}, nil
}

// validateProblems returns the parse and validation problems in raw.
func validateProblems(raw []byte) []string {
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return []string{err.Error()}
	}

	if err := cfg.Validate(); err != nil {
		return []string{err.Error()}
	}

	return nil
}

// extensionIntentWarnings is the transition preflight for descriptor edits
// that affect an enabled database or recovery declaration. It is advisory:
// apply still decides from step inputs, while E7-05 owns follower ordering.
func extensionIntentWarnings(old, updated config.Cluster) []output.Warning {
	changes := config.ExtensionIntentChanges(old, updated)
	if len(changes) == 0 {
		return nil
	}

	return []output.Warning{{
		Code:    "extension_intent_changed",
		Message: "extension intent changed for " + strings.Join(changes, ", ") + "; review the plan before applying",
	}}
}

// checkSchedules runs systemd-analyze calendar on every backup schedule, so
// a timer never fails to load: the node has the final say on OnCalendar.
func checkSchedules(ctx context.Context, cfg config.Cluster) error {
	var problems []string

	for key, value := range cfg.ScheduleValues() {
		if _, err := steps.Probe(ctx, "systemd-analyze", "calendar", value); err != nil {
			problems = append(problems, key+" "+strconv.Quote(value)+" is not a valid OnCalendar expression")
		}
	}

	if len(problems) == 0 {
		return nil
	}

	sort.Strings(problems)

	return output.NewError(output.CodeValidation, strings.Join(problems, "; "), output.ExitConfig,
		"Use systemd OnCalendar syntax, for example \"Sun *-*-* 01:00:00\" or \"daily\".").WithDetails(map[string]any{"problems": problems})
}

// checkRepoMove refuses to move backups to another endpoint, bucket or
// prefix once a stanza has backups there: the restore windows would be lost.
func checkRepoMove(ctx context.Context, old, updated config.Cluster) error {
	if !old.BackupConfigured() || !updated.BackupConfigured() {
		return nil
	}

	ob, nb := old.Backup, updated.Backup
	if ob.Endpoint == nb.Endpoint && ob.Bucket == nb.Bucket && ob.Prefix == nb.Prefix {
		return nil
	}

	all, err := driver(config.ClusterSpec{}, "config.set", nil).InfoAll(ctx)
	if err != nil {
		return nil
	}

	inUse := false
	for _, info := range all {
		inUse = inUse || len(info.Sets) > 0
	}

	if !inUse {
		return nil
	}

	return output.NewError(output.CodePrecondition,
		"backups already live in "+ob.Bucket+"/"+ob.Prefix+"; moving them would lose the restore window",
		output.ExitPrecondition,
		"Keep the endpoint, bucket and prefix. Re-running vaire backup setup with the same bucket rotates the keys.").
		WithDetails(map[string]any{"reason": "repo_in_use", "bucket": ob.Bucket, "prefix": ob.Prefix})
}

// syncNodeHostname keeps node.toml's hostname in step with cluster.toml: it
// is <node>.<domain>, so it moves when the domain does.
func syncNodeHostname(cfg config.Cluster) error {
	n, err := config.LoadNode(config.NodePath)
	if err != nil {
		if config.IsNotExist(err) {
			return nil
		}

		return err
	}

	hostname := cfg.Hostname(n.NodeName)
	if n.Hostname == hostname {
		return nil
	}

	n.Hostname = hostname

	raw, err := config.EncodeNode(n)
	if err != nil {
		return err
	}

	if err := engine.WriteFileAtomic(config.NodePath, raw, config.FileMode); err != nil {
		return err
	}

	masterbackup.MarkDirty("node.toml")

	return nil
}
