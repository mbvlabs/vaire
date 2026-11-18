package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/doctor"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// ext (Phase 7): list and add one database's extensions. The package install
// is server-wide (postgres-packages-v1), the preload and restart touch only the
// named database's cluster, and CREATE/UPDATE SQL runs in that database. A
// source with followers is refused here until follower propagation is
// implemented; a follower is always refused by the dispatcher. Removal is
// refused in this release.

const (
	extensionControlDirDefault = "/usr/share/postgresql/18/extension"
	extensionLibDirDefault     = "/usr/lib/postgresql/18/lib"
	extensionPreviewLimit      = 50
	extensionQueryTimeout      = 10 * time.Second
)

var (
	extensionControlDir = extensionControlDirDefault
	extensionLibDir     = extensionLibDirDefault
)

// The seams below keep host and engine effects out of unit tests without
// production fault hooks. Every one has a production default.
var (
	extensionLoad = func() (config.Cluster, []byte, error) {
		return state.FileStore{}.Load()
	}
	extensionSave = func(raw []byte) error {
		return state.FileStore{}.Save(raw)
	}
	extensionRecords      = backup.LoadRecords
	extensionPrepareStart = steps.PrepareExtensionStart
	extensionApply        = func(ctx context.Context, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.Apply(ctx, steps.Catalog(), cfg, rt, opts)
	}
	extensionPG               = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }
	extensionClusterRunning   = func(spec config.ClusterSpec) bool { return clusterRunning(spec) }
	extensionPreloadFacts     = extensionPreloadFactsDefault
	extensionSettingValues    = extensionSettingValuesDefault
	extensionInstalled        = extensionInstalledDefault
	extensionUpdatePath       = extensionUpdatePathDefault
	extensionPackageInstalled = extensionPackageInstalledDefault
	extensionRestart          = func(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime, report engine.Reporter) (time.Duration, error) {
		return steps.RestarterFor(cfg, rt, report).Restart(ctx, spec.Name)
	}
	extensionClearDeferred    = clearDeferred
	extensionCheckStep        = steps.CheckStep
	extensionPendingRestarts  = extensionPendingRestartsDefault
	extensionJournalPath      = engine.JournalPath
	extensionJournalConverged = extensionJournalConvergedDefault
	// extensionCaptureRequirements records the outgoing and incoming
	// stanza-bound requirements before a direct extension change saves or
	// drops anything. E705 owns the durable history; a failure stops the
	// change.
	extensionCaptureRequirements = func(ctx context.Context, oldCfg config.Cluster, oldSpec config.ClusterSpec, newCfg config.Cluster, newSpec config.ClusterSpec, action string) error {
		return captureExtensionChange(oldCfg, newCfg)
	}
	// extensionCaptureOutgoing and extensionCaptureIncoming are the direct
	// add boundary: outgoing history is durable before package/file work, and
	// incoming history only after that work succeeded. A failed prerequisite
	// therefore cannot retain a proposed provider the configuration never had.
	// The follower add transition can use the same two seams around its own
	// preparation.
	extensionCaptureOutgoing = captureExtensionOutgoing
	extensionCaptureIncoming = captureExtensionIncoming
)

type extListParams struct {
	DB string `json:"db,omitempty"`
}

type extAddParams struct {
	Name         string            `json:"name"`
	DB           string            `json:"db,omitempty"`
	Package      string            `json:"package,omitempty"`
	Preinstalled bool              `json:"preinstalled,omitempty"`
	Preload      string            `json:"preload,omitempty"`
	Settings     map[string]string `json:"settings,omitempty"`
	Schema       string            `json:"schema,omitempty"`
	CopySafety   string            `json:"copy_safety,omitempty"`
	CopySettings map[string]string `json:"copy_settings,omitempty"`
	StandbySafe  *bool             `json:"standby_safe,omitempty"`
}

type extRemoveParams struct {
	Name    string `json:"name"`
	DB      string `json:"db"`
	Cascade bool   `json:"cascade,omitempty"`
}

type extensionInstalledVersion struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Default string `json:"default_version,omitempty"`
	Schema  string `json:"-"`
}

type extAddResult struct {
	Database              string   `json:"database"`
	Extension             string   `json:"extension"`
	ExtName               string   `json:"ext_name,omitempty"`
	Source                string   `json:"source,omitempty"`
	Package               string   `json:"package,omitempty"`
	Preload               string   `json:"preload,omitempty"`
	Schema                string   `json:"schema,omitempty"`
	CopySafety            string   `json:"copy_safety,omitempty"`
	StandbySafe           bool     `json:"standby_safe,omitempty"`
	DescriptorSaved       bool     `json:"descriptor_saved,omitempty"`
	AlreadyEnabled        bool     `json:"already_enabled,omitempty"`
	Created               bool     `json:"created,omitempty"`
	RestartRequired       bool     `json:"restart_required"`
	Restarted             bool     `json:"restarted,omitempty"`
	PendingRestart        bool     `json:"pending_restart,omitempty"`
	PreloadMissing        bool     `json:"preload_missing,omitempty"`
	Version               string   `json:"version,omitempty"`
	PreviousVersion       string   `json:"previous_version,omitempty"`
	DefaultVersion        string   `json:"default_version,omitempty"`
	UpdateAvailable       bool     `json:"update_available,omitempty"`
	Downgrade             bool     `json:"downgrade,omitempty"`
	DirectionUnknown      bool     `json:"direction_unknown,omitempty"`
	Updated               bool     `json:"updated,omitempty"`
	PackageKept           string   `json:"package_kept,omitempty"`
	RequiredConfirmTokens []string `json:"required_confirm_tokens,omitempty"`
	DryRun                bool     `json:"dry_run,omitempty"`
	SQL                   string   `json:"sql,omitempty"`
}

type extRemoveResult struct {
	Database              string   `json:"database"`
	Extension             string   `json:"extension"`
	ExtName               string   `json:"ext_name,omitempty"`
	Enabled               bool     `json:"enabled"`
	Installed             bool     `json:"installed"`
	Dependents            []string `json:"dependents,omitempty"`
	DependentsTotal       int      `json:"dependents_total,omitempty"`
	DependentsTruncated   bool     `json:"dependents_truncated,omitempty"`
	OwnTables             []string `json:"own_tables,omitempty"`
	OwnTablesTotal        int      `json:"own_tables_total,omitempty"`
	OwnTablesTruncated    bool     `json:"own_tables_truncated,omitempty"`
	Cascade               bool     `json:"cascade,omitempty"`
	RestartRequired       bool     `json:"restart_required"`
	Restarted             bool     `json:"restarted,omitempty"`
	PackageKept           string   `json:"package_kept,omitempty"`
	RequiredConfirmTokens []string `json:"required_confirm_tokens,omitempty"`
	DryRun                bool     `json:"dry_run,omitempty"`
}

type extListEntry struct {
	Name             string             `json:"name"`
	ExtName          string             `json:"ext_name,omitempty"`
	Aliases          []string           `json:"aliases,omitempty"`
	Source           string             `json:"source"`
	Package          string             `json:"package,omitempty"`
	Preload          string             `json:"preload,omitempty"`
	Restart          bool               `json:"restart"`
	Default          bool               `json:"default,omitempty"`
	Note             string             `json:"note,omitempty"`
	BuiltIn          bool               `json:"built_in,omitempty"`
	CopySafety       string             `json:"copy_safety,omitempty"`
	StandbySafe      bool               `json:"standby_safe,omitempty"`
	PackageInstalled bool               `json:"package_installed"`
	PackageKnown     bool               `json:"package_known"`
	PackageError     string             `json:"package_error,omitempty"`
	Available        bool               `json:"available"`
	AvailableKnown   bool               `json:"available_known"`
	AvailableError   string             `json:"available_error,omitempty"`
	DefaultVersion   string             `json:"default_version,omitempty"`
	Unused           bool               `json:"unused,omitempty"`
	Databases        []extDatabaseState `json:"databases"`
}

type extDatabaseState struct {
	DB              string `json:"db"`
	Available       bool   `json:"available"`
	State           string `json:"state,omitempty"`
	Error           string `json:"error,omitempty"`
	Enabled         bool   `json:"enabled"`
	Installed       bool   `json:"installed"`
	Version         string `json:"version,omitempty"`
	DefaultVersion  string `json:"default_version,omitempty"`
	UpdateAvailable bool   `json:"update_available,omitempty"`
	PendingRestart  bool   `json:"pending_restart,omitempty"`
	PreloadKnown    bool   `json:"preload_known"`
}

type extListDatabase struct {
	DB           string                      `json:"db"`
	State        string                      `json:"state"`
	Available    bool                        `json:"available"`
	Error        string                      `json:"error,omitempty"`
	PreloadKnown bool                        `json:"preload_known"`
	Unmanaged    []extensionInstalledVersion `json:"unmanaged,omitempty"`

	enabled         []string
	installedValues map[string]extensionInstalledVersion
	loaded          map[string]bool
	pending         bool
}

type extListData struct {
	Extensions []extListEntry    `json:"extensions"`
	Databases  []extListDatabase `json:"databases"`
}

type extensionAvailability struct {
	Known          bool
	Available      bool
	DefaultVersion string
	Error          string
}

// --- commands ---

func handleExtList(ctx context.Context, env *Env) (Result, error) {
	var p extListParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	specs, err := extensionListSpecs(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}

	recs, err := extensionRecords()
	if err != nil {
		return Result{}, err
	}

	entries, err := env.Cluster.ExtensionResolver().List()
	if err != nil {
		return Result{}, config.ValidationError(err)
	}

	used, err := steps.ExtensionPackages(env.Cluster)
	if err != nil {
		return Result{}, err
	}

	usedSet := map[string]bool{}
	for _, pkg := range used {
		usedSet[pkg] = true
	}

	known := map[string]bool{}

	for _, ext := range entries {
		if ext.ExtName != "" {
			known[ext.ExtName] = true
		}
	}

	dbs := make([]extListDatabase, 0, len(specs))
	for _, spec := range specs {
		dbs = append(dbs, extensionListDatabaseFacts(ctx, env.Cluster, spec, recs))
	}

	data := extListData{Extensions: make([]extListEntry, 0, len(entries)), Databases: dbs}
	for _, ext := range entries {
		data.Extensions = append(data.Extensions, extensionListEntryFor(ctx, ext, dbs, usedSet))
	}

	extensionListUnmanaged(data.Databases, known)

	return Result{Data: data}, nil
}

func extensionListEntryFor(ctx context.Context, ext catalog.Extension, dbs []extListDatabase, usedSet map[string]bool) extListEntry {
	availability := extensionAvailabilityFor(ext)

	entry := extListEntry{
		Name: ext.Name, ExtName: ext.ExtName, Aliases: slices.Clone(ext.Aliases),
		Source: ext.Source, Package: ext.Package, Preload: ext.Preload,
		Restart: ext.Restart, Default: ext.Default, Note: ext.Note, BuiltIn: ext.BuiltIn,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		Available: availability.Available, AvailableKnown: availability.Known,
		AvailableError: availability.Error, DefaultVersion: availability.DefaultVersion,
		Databases: []extDatabaseState{},
	}
	if ext.Package == "" {
		entry.PackageInstalled, entry.PackageKnown = true, true
	} else {
		entry.PackageInstalled, entry.PackageKnown, entry.PackageError = extensionPackageInstalled(ctx, ext.Package)
		entry.Unused = entry.PackageKnown && entry.PackageInstalled && !usedSet[ext.Package]
	}

	for _, db := range dbs {
		st, include := extensionListDatabaseState(ext, db)
		if include {
			entry.Databases = append(entry.Databases, st)
		}
	}

	return entry
}

func extensionListDatabaseState(ext catalog.Extension, db extListDatabase) (extDatabaseState, bool) {
	st := extDatabaseState{
		DB: db.DB, Available: db.Available, State: db.State, Error: db.Error,
		Enabled: slices.Contains(db.enabled, ext.Name), PreloadKnown: db.PreloadKnown,
	}
	if !db.Available {
		return st, st.Enabled
	}

	if ext.ExtName != "" {
		if installed, ok := db.installed()[ext.ExtName]; ok {
			st.Installed, st.Version, st.DefaultVersion = true, installed.Version, installed.Default
			st.UpdateAvailable = extensionUpdateAvailable(installed.Version, installed.Default)
		}
	}

	if st.Enabled && ext.Preload != "" && db.PreloadKnown && !db.loaded[ext.Preload] && db.pending {
		st.PendingRestart = true
	}

	return st, st.Enabled || st.Installed
}

func extensionListUnmanaged(dbs []extListDatabase, known map[string]bool) {
	for i := range dbs {
		db := &dbs[i]
		if !db.Available {
			continue
		}

		var unmanaged []extensionInstalledVersion

		for name, got := range db.installed() {
			if name == "plpgsql" || known[name] {
				continue
			}

			unmanaged = append(unmanaged, got)
		}

		sort.Slice(unmanaged, func(i, j int) bool { return unmanaged[i].Name < unmanaged[j].Name })
		db.Unmanaged = unmanaged
	}
}

func handleExtAdd(ctx context.Context, env *Env) (Result, error) {
	ctx = steps.WithFollowerRuntimeOperation(ctx, env.Req.RequestID)

	var p extAddParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	cfg, raw, err := extensionLoad()
	if err != nil {
		return Result{}, err
	}

	recs, err := extensionRecords()
	if err != nil {
		return Result{}, err
	}

	spec, err := extensionTargetSpec(cfg, recs, p.DB, "ext add", true)
	if err != nil {
		return Result{}, err
	}

	var lockedTargets extensionTargetSet

	if !env.Req.DryRun {
		var release func()

		cfg, raw, recs, lockedTargets, release, err = lockExtensionMutation(ctx, env, spec.Name)
		if err != nil {
			return Result{}, err
		}
		defer release()

		spec = lockedTargets.Source
		if err := extensionMutationRefusals(cfg, recs, spec, "ext add"); err != nil {
			return Result{}, err
		}
	}

	updated, ext, descriptorSaved, err := extensionAddIntent(cfg, spec.Name, p)
	if err != nil {
		return Result{}, err
	}

	updatedSpec, ok := updated.ClusterByName(spec.Name)
	if !ok {
		return Result{}, databaseNotFound(spec.Name)
	}

	if err := extensionAddDescriptorChecks(ctx, cfg, updated, updatedSpec, ext, spec.Name, descriptorSaved); err != nil {
		return Result{}, err
	}

	alreadyEnabled := extensionEnabled(cfg, spec, ext.Name)
	if !env.Req.DryRun && len(cfg.FollowerNames(spec.Name)) > 0 {
		propagated, _, err := propagateFollowerExtensions(cfg, updated)
		if err != nil {
			return Result{}, err
		}

		updated = propagated
		if err := updated.Validate(); err != nil {
			return Result{}, config.ValidationError(err)
		}

		return extensionAddWithFollowers(ctx, env, cfg, raw, spec, updated, lockedTargets, ext, descriptorSaved, alreadyEnabled)
	}

	return extensionAddStandalone(ctx, env, cfg, updated, updatedSpec, spec, ext, descriptorSaved, alreadyEnabled)
}

func extensionAddDescriptorChecks(ctx context.Context, cfg, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, db string, descriptorSaved bool) error {
	if err := refuseInstalledExtensionSchemas(ctx, updated, updatedSpec); err != nil {
		return err
	}

	if err := validateTargetExtensionDescriptors(updated, updatedSpec); err != nil {
		return err
	}

	if sourceHasFollowers(updated, db) {
		if err := validateTargetExtensionCopySafety(updated, updatedSpec, false); err != nil {
			return err
		}
	}

	if !descriptorSaved {
		return nil
	}

	affected, err := extensionDefinitionImpact(cfg, updated, ext.Name, db)
	if err != nil {
		return config.ValidationError(err)
	}

	if len(affected) == 0 {
		return nil
	}

	followers := cfg.FollowerNames(db)
	sort.Strings(followers)

	if !slices.Equal(affected, followers) {
		return extensionGlobalIntentConflict(db, ext.Name, affected)
	}

	return nil
}

func extensionAddStandalone(ctx context.Context, env *Env, cfg config.Cluster, updated config.Cluster, updatedSpec config.ClusterSpec, spec config.ClusterSpec, ext catalog.Extension, descriptorSaved, alreadyEnabled bool) (Result, error) {
	pf, err := extensionAddPreflight(ctx, env, cfg, updated, spec, updatedSpec, ext, descriptorSaved, alreadyEnabled)
	if err != nil {
		return Result{}, err
	}

	if env.Req.DryRun {
		return Result{Data: extensionAddDryRunResult(pf, cfg, spec, ext, descriptorSaved, alreadyEnabled)}, nil
	}

	if pf.InstalledKnown && pf.HasInstalled && pf.Got.Default != "" && pf.Got.Default != pf.Got.Version {
		if err := extensionAddRefuseVersionDirection(updatedSpec.Name, ext, pf.Got); err != nil {
			return Result{}, err
		}
	}

	if pf.TokenRequired && !hasConfirmToken(env.Req, updatedSpec.Name+"/restart") {
		return Result{}, confirmRequired(ext.Name+" changes "+updatedSpec.Name+"'s extension configuration, so it needs that cluster's restart token (only that database)", updatedSpec.Name+"/restart")
	}

	if result, ok, err := extensionAddNoOp(ctx, updated, updatedSpec, ext, pf, descriptorSaved, alreadyEnabled); err != nil {
		return Result{}, err
	} else if ok {
		return result, nil
	}

	env.Cluster = updated

	rt := runtimeFor(env, false)
	if err := extensionAddPrepareAndSave(ctx, env, cfg, updated, updatedSpec, ext, descriptorSaved, alreadyEnabled, rt); err != nil {
		return Result{}, err
	}

	applied, warnings, err := extensionAddApplyConfig(ctx, env, updated, updatedSpec, ext, pf.TokenRequired, rt)
	if err != nil {
		return Result{}, err
	}

	restarted := applied.Restarted()
	if pf.RestartExpected {
		again, err := extensionRestartIfPending(ctx, updated, updatedSpec, pf.RestartExpected, applied, rt, env.Report)
		if err != nil {
			return Result{}, extensionPartialError("saving the extension intent succeeded, but restarting its cluster failed: "+err.Error(),
				"extension_apply_failed", updatedSpec.Name, ext.Name, pf.TokenRequired, restarted, applied)
		}

		restarted = restarted || again
	}

	_, roleWarnings, err := extensionAddApplyRoles(ctx, env, updated, updatedSpec, ext, pf.TokenRequired, restarted, rt)
	warnings = append(warnings, roleWarnings...)

	if err != nil {
		return Result{}, err
	}

	got, err := extensionAddVerifyCreated(ctx, updatedSpec, ext)
	if err != nil {
		return Result{}, err
	}

	pendingNow := extensionAddPendingNow(ctx, updatedSpec, pf.PendingSettings, pf.Running)

	return extensionAddFinishResult(ctx, updated, updatedSpec, ext, pf, descriptorSaved, alreadyEnabled, restarted, pendingNow, got, warnings)
}

type extensionAddPreflightFacts struct {
	Running         bool
	PreloadMissing  bool
	RestartExpected bool
	TokenRequired   bool
	ConfigDrift     bool
	Tokens          []string
	PendingSettings []string
	Installed       map[string]extensionInstalledVersion
	InstalledKnown  bool
	Got             extensionInstalledVersion
	HasInstalled    bool
	Runtime         engine.Runtime
}

func extensionAddPreflight(ctx context.Context, env *Env, cfg config.Cluster, updated config.Cluster, spec config.ClusterSpec, updatedSpec config.ClusterSpec, ext catalog.Extension, descriptorSaved, alreadyEnabled bool) (extensionAddPreflightFacts, error) {
	f := extensionAddPreflightFacts{Installed: map[string]extensionInstalledVersion{}}

	preload, err := extensionAddPreloadState(ctx, env, cfg, updated, spec, updatedSpec, ext)
	if err != nil {
		return f, err
	}

	f.Running = preload.Running
	f.PreloadMissing = preload.PreloadMissing

	f.Runtime = runtimeFor(env, false)
	if f.Running {
		f.PendingSettings, err = extensionPendingRestarts(ctx, updatedSpec)
		if err != nil {
			return f, output.NewError(output.CodePrecondition,
				"cannot read "+spec.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
				"Run vaire status --db "+spec.Name+" before retrying; no extension intent was saved.").
				WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": spec.Name, "extension": ext.Name})
		}
	}

	configDetail, err := extensionCheckStep(ctx, updated, f.Runtime, steps.ConfigStepID(updatedSpec.Name))
	if err != nil {
		return f, output.NewError(output.CodePrecondition,
			"cannot check "+spec.Name+"'s configuration convergence: "+err.Error(), output.ExitPrecondition,
			"Run vaire status --db "+spec.Name+" before retrying; no extension intent was saved.").
			WithDetails(map[string]any{"reason": "extension_config_unknown", "database": spec.Name, "extension": ext.Name})
	}

	f.ConfigDrift = configDetail != ""
	f.RestartExpected = preload.NeedRestart || len(f.PendingSettings) > 0
	f.TokenRequired = f.RestartExpected || f.ConfigDrift ||
		((!alreadyEnabled || descriptorSaved) && (ext.Preload != "" || len(ext.Settings) > 0))

	f.Tokens = extensionAddConfirmTokens(updatedSpec.Name, f.TokenRequired)
	if f.Running {
		f.Installed, err = extensionInstalled(ctx, updatedSpec)
		if err != nil {
			return f, output.NewError(output.CodePrecondition,
				"cannot read "+spec.Name+"'s installed extensions: "+err.Error(), output.ExitPrecondition,
				"Run vaire status --db "+spec.Name+" before retrying; no extension intent was saved.").
				WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name, "extension": ext.Name})
		}

		f.InstalledKnown = true
	}

	f.Got, f.HasInstalled = f.Installed[ext.ExtName]

	return f, nil
}

type extensionAddPreloadFacts struct {
	Running        bool
	PreloadMissing bool
	NeedRestart    bool
}

func extensionAddPreloadState(ctx context.Context, env *Env, cfg, updated config.Cluster, spec, updatedSpec config.ClusterSpec, ext catalog.Extension) (extensionAddPreloadFacts, error) {
	var st extensionAddPreloadFacts

	beforePreloads, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return st, config.ValidationError(err)
	}

	afterPreloads, err := updated.ExtensionResolver().Preloads(updatedSpec.Extensions)
	if err != nil {
		return st, config.ValidationError(err)
	}

	preloadChange := !slices.Equal(beforePreloads, afterPreloads)
	preloadKnownNeeded := ext.Preload != ""

	st.Running = extensionClusterRunning(spec)
	if !st.Running && !env.Req.DryRun {
		return st, extensionClusterNotRunning(spec.Name, "ext add")
	}

	var loaded map[string]bool

	pendingPreload := false
	preloadKnown := false

	if st.Running {
		loaded, pendingPreload, err = extensionPreloadFacts(ctx, updatedSpec)
		if err != nil {
			if preloadKnownNeeded {
				return st, output.NewError(output.CodePrecondition,
					"cannot read "+spec.Name+"'s effective preload state: "+err.Error(), output.ExitPrecondition,
					"Run vaire status --db "+spec.Name+" and vaire logs postgres --db "+spec.Name+" before retrying; no extension intent was saved.").
					WithDetails(map[string]any{"reason": "extension_preload_unknown", "database": spec.Name, "extension": ext.Name})
			}
		} else {
			preloadKnown = true
		}
	}

	st.PreloadMissing = preloadKnown && ext.Preload != "" && !loaded[ext.Preload] && !pendingPreload
	st.NeedRestart = preloadChange || (preloadKnown && ext.Preload != "" && !loaded[ext.Preload] && pendingPreload)

	return st, nil
}

func extensionAddDryRunResult(pf extensionAddPreflightFacts, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension, descriptorSaved, alreadyEnabled bool) extAddResult {
	res := extAddResult{
		Database: spec.Name, Extension: ext.Name, ExtName: ext.ExtName, Source: ext.Source,
		Package: ext.Package, Preload: ext.Preload, Schema: ext.Schema,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		DescriptorSaved: descriptorSaved, AlreadyEnabled: alreadyEnabled, Created: pf.Running && !pf.HasInstalled,
		RestartRequired: pf.RestartExpected, PendingRestart: len(pf.PendingSettings) > 0, PreloadMissing: pf.PreloadMissing,
		RequiredConfirmTokens: pf.Tokens, DryRun: true,
		SQL: steps.ExtensionSQL(ext, spec.Name),
	}
	if pf.InstalledKnown && pf.HasInstalled {
		res.Version, res.DefaultVersion = pf.Got.Version, pf.Got.Default
		res.UpdateAvailable = extensionUpdateAvailable(pf.Got.Version, pf.Got.Default)

		res.Downgrade = extensionDowngrade(pf.Got.Version, pf.Got.Default)
		if pf.Got.Default != "" && pf.Got.Default != pf.Got.Version {
			_, ordered := extensionVersionCompare(pf.Got.Version, pf.Got.Default)
			res.DirectionUnknown = !ordered
		}
	}

	if followers := cfg.FollowerNames(spec.Name); len(followers) > 0 {
		res.RequiredConfirmTokens = append(res.RequiredConfirmTokens, extensionRestartTokensFor(cfg, spec.Name)...)
		sort.Strings(res.RequiredConfirmTokens)
		res.RequiredConfirmTokens = slices.Compact(res.RequiredConfirmTokens)
	}

	return res
}

func extensionAddRefuseVersionDirection(db string, ext catalog.Extension, got extensionInstalledVersion) error {
	cmp, ordered := extensionVersionCompare(got.Version, got.Default)
	if !ordered {
		return extensionUpdateDirectionUnknown(db, ext.Name, got.Version, got.Default)
	}

	if cmp > 0 {
		return extensionDowngradeError(db, ext.Name, got.Version, got.Default)
	}

	return nil
}

func extensionAddNoOp(ctx context.Context, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, pf extensionAddPreflightFacts, descriptorSaved, alreadyEnabled bool) (Result, bool, error) {
	// A no-op is allowed only after the configuration files, the runtime and
	// every pending postmaster setting are actually converged. Intent equality
	// or an already loaded library alone is not enough.
	if !alreadyEnabled || descriptorSaved || !pf.InstalledKnown || !pf.HasInstalled ||
		pf.RestartExpected || pf.PreloadMissing || pf.ConfigDrift ||
		pf.Got.Default == "" || pf.Got.Default != pf.Got.Version {
		return Result{}, false, nil
	}

	journalOK, err := extensionJournalConverged(ctx, updated, updatedSpec, pf.Runtime)
	if err != nil {
		return Result{}, false, output.NewError(output.CodePrecondition,
			"cannot check "+updatedSpec.Name+"'s extension journal convergence: "+err.Error(), output.ExitPrecondition,
			"Run vaire status --db "+updatedSpec.Name+" before retrying; no extension intent was saved.").
			WithDetails(map[string]any{"reason": "extension_journal_unknown", "database": updatedSpec.Name, "extension": ext.Name})
	}

	if !journalOK {
		return Result{}, false, nil
	}

	rolesDetail, err := extensionCheckStep(ctx, updated, pf.Runtime, steps.RolesStepID(updatedSpec.Name))
	if err != nil {
		return Result{}, false, output.NewError(output.CodePrecondition,
			"cannot check "+updatedSpec.Name+"'s extension runtime convergence: "+err.Error(), output.ExitPrecondition,
			"Run vaire status --db "+updatedSpec.Name+" before retrying; no extension intent was saved.").
			WithDetails(map[string]any{"reason": "extension_runtime_unknown", "database": updatedSpec.Name, "extension": ext.Name})
	}

	if rolesDetail != "" {
		return Result{}, false, nil
	}

	return Result{Data: extAddResult{
		Database: updatedSpec.Name, Extension: ext.Name, ExtName: ext.ExtName, Source: ext.Source,
		Package: ext.Package, Preload: ext.Preload, Schema: ext.Schema,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		AlreadyEnabled: true, RestartRequired: false, Version: pf.Got.Version, DefaultVersion: pf.Got.Default,
	}}, true, nil
}

func extensionAddPrepareAndSave(ctx context.Context, env *Env, cfg config.Cluster, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, descriptorSaved, alreadyEnabled bool, rt engine.Runtime) error {
	if err := extensionCaptureOutgoing(cfg); err != nil {
		return output.NewError(output.CodePrecondition,
			"recording extension requirements before the change failed: "+err.Error(), output.ExitPrecondition,
			"No proposed extension intent was saved, and no package preparation, restart or extension SQL was requested; any outgoing requirement snapshots already written are retained. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{"reason": "extension_history_capture_failed", "database": updatedSpec.Name, "extension": ext.Name})
	}

	saved := descriptorSaved || !alreadyEnabled
	intentSaved := !saved
	// Prepare the proposed packages and files before saving the proposed
	// intent. A failed prerequisite must leave cluster.toml unchanged and
	// record no incoming requirements.
	if err := extensionPrepareStart(ctx, updated, updatedSpec, nil, rt, env.Report); err != nil {
		hint := "Package installation may have partially run; installed packages are not removed. vaire did not request a restart or run extension SQL. "
		if intentSaved {
			hint += "cluster.toml already enables the extension; retry after fixing the package prerequisite."
		} else {
			hint += "cluster.toml was not changed; retry vaire ext add after fixing the package prerequisite."
		}

		return output.NewError(output.CodePartialFailure,
			"installing extension packages for "+ext.Name+" failed: "+err.Error(), output.ExitStep, hint).
			WithDetails(map[string]any{
				"reason": "package_install_failed", "database": updatedSpec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
			})
	}
	// The incoming snapshot is safe only after preparation succeeded. Packages
	// may already be installed when this fails, so it must not claim that
	// nothing changed, and the invention of the proposed intent still waits.
	if err := extensionCaptureIncoming(updated); err != nil {
		return output.NewError(output.CodePartialFailure,
			"preparing extension packages for "+ext.Name+" succeeded, but recording its incoming requirements failed: "+err.Error(),
			output.ExitStep,
			"Incoming requirement snapshots for one or more stanzas may already be written and are retained; packages may already be installed, cluster.toml is unchanged, and vaire did not request a restart or run extension SQL. Retry after fixing the requirement history store.").
			WithDetails(map[string]any{
				"reason": "extension_history_capture_failed", "database": updatedSpec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
				"incoming_history_may_remain": true,
			})
	}

	if !saved {
		return nil
	}

	newRaw, err := updated.Encode()
	if err != nil {
		return output.NewError(output.CodePartialFailure,
			"preparing extension packages for "+ext.Name+" succeeded and its incoming requirements were recorded, but encoding the extension intent failed: "+err.Error(),
			output.ExitStep,
			"Package files may already be installed; cluster.toml is unchanged, and vaire did not request a restart or run extension SQL. Retry after fixing the extension configuration.").
			WithDetails(map[string]any{
				"reason": "extension_intent_save_failed", "database": updatedSpec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
				"incoming_history_retained": true,
			})
	}

	if err := extensionSave(newRaw); err != nil {
		return output.NewError(output.CodePartialFailure,
			"preparing extension packages for "+ext.Name+" succeeded and its incoming requirements were recorded, but saving the extension intent failed: "+err.Error(),
			output.ExitStep,
			"Package files may already be installed; cluster.toml is unchanged, and vaire did not request a restart or run extension SQL. Retry after fixing the configuration store.").
			WithDetails(map[string]any{
				"reason": "extension_intent_save_failed", "database": updatedSpec.Name, "extension": ext.Name,
				"intent_saved": intentSaved, "config_rollback": "not_needed", "packages_may_remain": true,
				"incoming_history_retained": true,
			})
	}

	return nil
}

func extensionAddApplyConfig(ctx context.Context, env *Env, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, tokenRequired bool, rt engine.Runtime) (engine.ApplyData, []output.Warning, error) {
	applyOpts := engine.Options{
		Only: []string{steps.ConfigStepID(updatedSpec.Name)}, DB: updatedSpec.Name,
		Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	}
	if tokenRequired {
		applyOpts.Confirm = engine.Confirmation{Clusters: []string{updatedSpec.Name}}
	}

	applied, warnings, err := extensionApply(ctx, updated, rt, applyOpts)
	if err != nil {
		return applied, warnings, extensionPartialError("saving the extension intent succeeded, but applying it failed: "+err.Error(),
			"extension_apply_failed", updatedSpec.Name, ext.Name, tokenRequired, false, applied)
	}

	clearExtensionDeferred(applied)

	if extensionApplyIncomplete(applied) || extensionRestartPending(applied, updatedSpec.Name) {
		return applied, warnings, extensionPartialError("saving the extension intent succeeded, but its configuration still waits for a restart",
			"extension_pending_restart", updatedSpec.Name, ext.Name, tokenRequired, false, applied)
	}

	return applied, warnings, nil
}

func extensionAddApplyRoles(ctx context.Context, env *Env, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, tokenRequired, restarted bool, rt engine.Runtime) (engine.ApplyData, []output.Warning, error) {
	rolesOpts := engine.Options{
		Only: []string{steps.RolesStepID(updatedSpec.Name)}, DB: updatedSpec.Name,
		Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	}
	rolesApplied, roleWarnings, err := extensionApply(ctx, updated, rt, rolesOpts)
	clearExtensionDeferred(rolesApplied)

	if err != nil {
		return rolesApplied, roleWarnings, extensionPartialError("the extension intent and configuration are saved, but creating its SQL failed: "+err.Error(),
			"extension_sql_failed", updatedSpec.Name, ext.Name, tokenRequired, restarted, rolesApplied)
	}

	if extensionApplyIncomplete(rolesApplied) {
		if extensionWarningCode(roleWarnings, "extension_pending_restart") {
			return rolesApplied, roleWarnings, extensionPartialError("the extension is configured but its SQL still waits for the preload restart",
				"extension_pending_restart", updatedSpec.Name, ext.Name, tokenRequired, restarted, rolesApplied)
		}

		return rolesApplied, roleWarnings, extensionPartialError("the extension is configured, but the roles step is still incomplete",
			"extension_roles_incomplete", updatedSpec.Name, ext.Name, tokenRequired, restarted, rolesApplied)
	}

	if extensionRestartPending(rolesApplied, updatedSpec.Name) {
		return rolesApplied, roleWarnings, extensionPartialError("the extension is configured but its SQL still waits for a restart",
			"extension_pending_restart", updatedSpec.Name, ext.Name, tokenRequired, restarted, rolesApplied)
	}

	return rolesApplied, roleWarnings, nil
}

func extensionAddVerifyCreated(ctx context.Context, updatedSpec config.ClusterSpec, ext catalog.Extension) (extensionInstalledVersion, error) {
	have, err := extensionInstalled(ctx, updatedSpec)
	if err != nil {
		return extensionInstalledVersion{}, output.NewError(output.CodePartialFailure,
			"the extension intent and SQL ran, but verifying "+ext.Name+" in "+updatedSpec.Name+" failed: "+err.Error(), output.ExitStep,
			"Check vaire ext list --db "+updatedSpec.Name+" before retrying; do not run the SQL again by hand.").
			WithDetails(map[string]any{"reason": "extension_verify_failed", "database": updatedSpec.Name, "extension": ext.Name, "intent_saved": true})
	}

	got, has := have[ext.ExtName]
	if !has {
		return extensionInstalledVersion{}, output.NewError(output.CodePartialFailure,
			ext.Name+" was not created in "+updatedSpec.Name+" after the roles step", output.ExitStep,
			"Retry with vaire apply --db "+updatedSpec.Name+" --confirm "+updatedSpec.Name+"/restart, then check vaire ext list --db "+updatedSpec.Name+".").
			WithDetails(map[string]any{"reason": "extension_not_created", "database": updatedSpec.Name, "extension": ext.Name, "intent_saved": true})
	}

	return got, nil
}

func extensionAddPendingNow(ctx context.Context, spec config.ClusterSpec, before []string, running bool) bool {
	pendingNow := len(before) > 0

	if running {
		if stillPending, err := extensionPendingRestarts(ctx, spec); err == nil {
			pendingNow = len(stillPending) > 0
		}
	}

	return pendingNow
}

func extensionAddFinishResult(ctx context.Context, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, pf extensionAddPreflightFacts, descriptorSaved, alreadyEnabled, restarted, pendingNow bool, got extensionInstalledVersion, warnings []output.Warning) (Result, error) {
	res := extAddResult{
		Database: updatedSpec.Name, Extension: ext.Name, ExtName: ext.ExtName, Source: ext.Source,
		Package: ext.Package, Preload: ext.Preload, Schema: ext.Schema,
		CopySafety: ext.CopySafety, StandbySafe: ext.StandbySafe,
		DescriptorSaved: descriptorSaved, AlreadyEnabled: alreadyEnabled, Created: pf.InstalledKnown && !pf.HasInstalled,
		RestartRequired: pf.RestartExpected, Restarted: restarted, PendingRestart: pendingNow, PreloadMissing: pf.PreloadMissing,
		Version: got.Version, DefaultVersion: got.Default, UpdateAvailable: extensionUpdateAvailable(got.Version, got.Default),
		RequiredConfirmTokens: pf.Tokens,
	}
	if ext.Package != "" {
		res.PackageKept = ext.Package
	}

	if got.Default != "" && got.Default != got.Version {
		outcome, err := extensionAddUpdateIfNeeded(ctx, updated, updatedSpec, ext, got, pf.Tokens, restarted)
		if err != nil {
			return Result{}, err
		}

		if outcome.Skip {
			res.UpdateAvailable, res.Downgrade = false, false
			return Result{Data: res, Warnings: warnings}, nil
		}

		if outcome.Updated {
			res.PreviousVersion, res.Version, res.Updated = outcome.Previous, outcome.Version, true
			res.UpdateAvailable = false
		}
	}

	return Result{Data: res, Warnings: warnings}, nil
}

type extensionAddUpdate struct {
	Previous string
	Version  string
	Updated  bool
	Skip     bool
}

func extensionAddUpdateIfNeeded(ctx context.Context, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, got extensionInstalledVersion, tokens []string, restarted bool) (extensionAddUpdate, error) {
	previous := got.Version

	cmp, ordered := extensionVersionCompare(previous, got.Default)
	if !ordered {
		return extensionAddUpdate{}, extensionUpdateDirectionUnknown(updatedSpec.Name, ext.Name, previous, got.Default)
	}

	if cmp > 0 {
		return extensionAddUpdate{}, extensionDowngradeError(updatedSpec.Name, ext.Name, previous, got.Default)
	}

	if cmp == 0 {
		return extensionAddUpdate{Skip: true}, nil
	}

	supported, err := extensionUpdatePath(ctx, updatedSpec, ext.ExtName, previous, got.Default)
	if err != nil {
		return extensionAddUpdate{}, output.NewError(output.CodePartialFailure,
			ext.Name+" is installed at "+previous+", but reading PostgreSQL's update path to "+got.Default+" failed: "+err.Error(),
			output.ExitStep, "The extension is usable at "+previous+"; retry vaire ext add "+ext.Name+" --db "+updatedSpec.Name+" after fixing catalog access.").
			WithDetails(map[string]any{"reason": "extension_update_path_unknown", "database": updatedSpec.Name, "extension": ext.Name, "version": previous, "default_version": got.Default, "intent_saved": true})
	}

	if !supported {
		return extensionAddUpdate{}, output.NewError(output.CodePrecondition,
			ext.Name+" is installed at "+previous+" and the package's default is "+got.Default+", but PostgreSQL has no supported update path between them",
			output.ExitPrecondition,
			"The extension stays at "+previous+". No package upgrade or downgrade is attempted; use a version whose package documents a supported ALTER EXTENSION UPDATE path.").
			WithDetails(map[string]any{"reason": "extension_update_unsupported", "database": updatedSpec.Name, "extension": ext.Name, "version": previous, "default_version": got.Default, "intent_saved": true})
	}

	readiness, err := extensionPrimaryReadiness(ctx, updated, updatedSpec, ext)
	if err != nil {
		return extensionAddUpdate{}, output.NewError(output.CodePartialFailure,
			ext.Name+" is installed at "+previous+", but its required primary prerequisites cannot be proven: "+err.Error(),
			output.ExitStep, "The extension stays at "+previous+"; retry vaire ext add "+ext.Name+" --db "+updatedSpec.Name+" after fixing the setting or catalog access.").
			WithDetails(map[string]any{"reason": "extension_update_prerequisites_unknown", "database": updatedSpec.Name, "extension": ext.Name, "version": previous, "default_version": got.Default, "intent_saved": true})
	}

	if readiness.Deferred {
		return extensionAddUpdate{}, extensionPartialError(
			ext.Name+" is installed at "+previous+" but its required primary prerequisites are not effective: "+readiness.Detail,
			"extension_update_prerequisites", updatedSpec.Name, ext.Name, readiness.RestartPending, restarted, engine.ApplyData{},
			map[string]any{
				"retry": extensionAddRetry(updatedSpec.Name, ext.Name, tokens), "intent_saved": true,
				"pending": []string{"update"}, "version": previous, "default_version": got.Default,
			})
	}

	sql := "ALTER EXTENSION " + pg.Quote(ext.ExtName) + " UPDATE TO " + extensionLiteral(got.Default) + ";\n"
	if err := extensionPG(updatedSpec).Exec(ctx, updatedSpec.DatabaseName(), sql, nil); err != nil {
		return extensionAddUpdate{}, output.WrapError(output.CodeExternalCommand,
			fmt.Errorf("update %s in %s from %s to %s: %w", ext.Name, updatedSpec.Name, previous, got.Default, err), output.ExitExternal,
			"The extension is still at "+previous+"; read vaire logs postgres --db "+updatedSpec.Name+". No package upgrade or downgrade was attempted.").
			WithDetails(map[string]any{"reason": "extension_update_failed", "database": updatedSpec.Name, "extension": ext.Name, "version": previous, "default_version": got.Default, "intent_saved": true})
	}

	after, err := extensionInstalled(ctx, updatedSpec)
	if err != nil {
		return extensionAddUpdate{}, output.NewError(output.CodePartialFailure,
			"the extension update ran, but verifying "+ext.Name+"'s resulting version failed: "+err.Error(), output.ExitStep,
			"Check vaire ext list --db "+updatedSpec.Name+" before retrying.").
			WithDetails(map[string]any{"reason": "extension_verify_failed", "database": updatedSpec.Name, "extension": ext.Name, "intent_saved": true})
	}

	actual, ok := after[ext.ExtName]
	if !ok || actual.Version != got.Default {
		actualVersion := ""
		if ok {
			actualVersion = actual.Version
		}

		return extensionAddUpdate{}, output.NewError(output.CodePartialFailure,
			"ALTER EXTENSION "+ext.Name+" did not reach "+got.Default+"; the database reports "+actualVersion, output.ExitStep,
			"Check the extension's update path and the cluster log; no package upgrade or downgrade was attempted.").
			WithDetails(map[string]any{"reason": "extension_update_failed", "database": updatedSpec.Name, "extension": ext.Name, "version": actualVersion, "default_version": got.Default, "intent_saved": true})
	}

	return extensionAddUpdate{Previous: previous, Version: actual.Version, Updated: true}, nil
}

// handleExtRemove refuses the explicit removal command before reading the
// target, records or configuration. The transition below remains as the
// retained removal implementation, but no node entry point exposes it.
func handleExtRemove(_ context.Context, env *Env) (Result, error) {
	var p extRemoveParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	return Result{}, extensionRemovalUnsupported(strings.TrimSpace(p.DB), strings.TrimSpace(p.Name))
}

func extensionRemoveExecute(ctx context.Context, env *Env) (Result, error) {
	ctx = steps.WithFollowerRuntimeOperation(ctx, env.Req.RequestID)

	p, err := extensionRemoveParams(env)
	if err != nil {
		return Result{}, err
	}

	cfg, raw, err := extensionLoad()
	if err != nil {
		return Result{}, err
	}

	recs, err := extensionRecords()
	if err != nil {
		return Result{}, err
	}

	spec, err := extensionTargetSpec(cfg, recs, p.DB, "ext remove", false)
	if err != nil {
		return Result{}, err
	}

	var lockedTargets extensionTargetSet

	if !env.Req.DryRun {
		var release func()

		cfg, raw, recs, lockedTargets, release, err = lockExtensionMutation(ctx, env, spec.Name)
		if err != nil {
			return Result{}, err
		}
		defer release()

		spec = lockedTargets.Source
		if err := extensionMutationRefusals(cfg, recs, spec, "ext remove"); err != nil {
			return Result{}, err
		}
	}

	if err := CheckNotProtected(cfg, spec.Name, "ext remove"); err != nil {
		return Result{}, err
	}

	ext, err := extensionRemoveResolve(cfg, p.Name)
	if err != nil {
		return Result{}, err
	}
	// Carry the outgoing descriptor through config/guard execution. It explains
	// existing guard entries and is retired only when the follower catalog
	// proves its SQL absent.
	ctx = steps.WithFollowerRuntimeOutgoing(ctx, []catalog.Extension{ext})

	if !extensionClusterRunning(spec) {
		return Result{}, extensionClusterNotRunning(spec.Name, "ext remove")
	}

	facts, err := extensionRemoveFacts(ctx, env, cfg, spec, ext)
	if err != nil {
		return Result{}, err
	}

	preview, err := extensionRemovePreview(ctx, spec, ext, facts.Installed, p.Cascade)
	if err != nil {
		return Result{}, err
	}

	updated, updatedSpec, _, retry, res, err := extensionRemovePlan(cfg, spec, ext, facts, p, preview, env)
	if err != nil {
		return Result{}, err
	}

	if env.Req.DryRun {
		return Result{Data: res}, nil
	}

	if !hasConfirmToken(env.Req, spec.Name) {
		return Result{}, confirmRequired("removing "+ext.Name+" from "+spec.Name+" drops its objects and their data", spec.Name)
	}

	if len(cfg.FollowerNames(spec.Name)) > 0 {
		return extensionRemoveWithFollowers(ctx, env, cfg, raw, spec, updated, lockedTargets, ext, facts.Installed, facts.Enabled, facts.RestartRequired, p.Cascade, res)
	}

	if facts.RestartRequired && !hasConfirmToken(env.Req, spec.Name+"/restart") {
		return Result{}, confirmRequired(ext.Name+" changes "+spec.Name+"'s extension configuration, so it needs that cluster's restart token (only that database)", spec.Name+"/restart")
	}

	return extensionRemoveStandalone(ctx, env, cfg, spec, updated, updatedSpec, ext, facts, p, retry, res)
}

func extensionRemoveParams(env *Env) (extRemoveParams, error) {
	var p extRemoveParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return p, err
	}

	if strings.TrimSpace(p.DB) == "" {
		return p, extensionDatabaseRequired("ext remove")
	}

	return p, nil
}

func extensionRemoveResolve(cfg config.Cluster, name string) (catalog.Extension, error) {
	ext, err := cfg.ExtensionResolver().Resolve(name)
	if err != nil {
		if _, ok := catalog.Lookup(name); !ok {
			if _, ok := cfg.Extensions[name]; !ok {
				return catalog.Extension{}, extensionNotFound(name)
			}
		}

		return catalog.Extension{}, config.ValidationError(err)
	}

	if !ext.Managed() {
		return catalog.Extension{}, extensionNotManaged(ext.Name)
	}

	if ext.Default {
		return catalog.Extension{}, extensionDefault(ext.Name)
	}

	return ext, nil
}

type extensionRemoveFactsData struct {
	Enabled         bool
	Installed       bool
	RestartRequired bool
	RetryConfig     bool
}

func extensionRemoveFacts(ctx context.Context, env *Env, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension) (extensionRemoveFactsData, error) {
	f := extensionRemoveFactsData{}
	f.Enabled = extensionEnabled(cfg, spec, ext.Name)

	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		return f, output.NewError(output.CodePrecondition,
			"cannot read "+spec.Name+"'s installed extensions: "+err.Error(), output.ExitPrecondition,
			"Run vaire status --db "+spec.Name+" before retrying; nothing was changed.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name, "extension": ext.Name})
	}

	_, f.Installed = installed[ext.ExtName]
	retryPending := []string{}
	retryDetail := ""

	if !f.Enabled && !f.Installed && !env.Req.DryRun {
		needed, pending, detail, err := extensionRemoveRetryNeeded(ctx, cfg, spec, ext, env)
		if err != nil {
			return f, err
		}

		if !needed {
			return f, extensionNotInstalled(ext.Name, spec.Name, f.Enabled, f.Installed)
		}

		f.RetryConfig, retryPending, retryDetail = true, pending, detail
	}

	removedExtensions := removeExtensionName(cfg, spec, ext.Name)

	beforePreloads, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return f, config.ValidationError(err)
	}

	afterPreloads, err := cfg.ExtensionResolver().Preloads(removedExtensions)
	if err != nil {
		return f, config.ValidationError(err)
	}

	pendingSettings := retryPending
	configDetail := retryDetail

	if f.Enabled {
		pendingSettings, err = extensionPendingRestarts(ctx, spec)
		if err != nil {
			return f, output.NewError(output.CodePrecondition,
				"cannot read "+spec.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": spec.Name, "extension": ext.Name})
		}

		configDetail, err = extensionCheckStep(ctx, cfg, runtimeFor(env, false), steps.ConfigStepID(spec.Name))
		if err != nil {
			return f, output.NewError(output.CodePrecondition,
				"cannot check "+spec.Name+"'s configuration convergence: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_config_unknown", "database": spec.Name, "extension": ext.Name})
		}
	}

	settingsRemoval := f.Enabled && len(ext.Settings) > 0
	f.RestartRequired = !slices.Equal(beforePreloads, afterPreloads) ||
		settingsRemoval || (f.Enabled && (len(pendingSettings) > 0 || configDetail != "")) ||
		(f.RetryConfig && (len(retryPending) > 0 || retryDetail != ""))

	return f, nil
}

type extensionRemovePreviewData struct {
	Dependents          []string
	DependentsTotal     int
	DependentsTruncated bool
	OwnTables           []string
	OwnTablesTotal      int
	OwnTablesTruncated  bool
}

func extensionRemovePreview(ctx context.Context, spec config.ClusterSpec, ext catalog.Extension, installed, cascade bool) (extensionRemovePreviewData, error) {
	var p extensionRemovePreviewData
	if !installed {
		return p, nil
	}

	var err error

	p.Dependents, p.DependentsTotal, p.DependentsTruncated, err = extensionPreview(ctx, extensionPG(spec), spec.DatabaseName(), extensionDependentsBody(ext.ExtName), "object")
	if err != nil {
		return p, output.NewError(output.CodePrecondition,
			"cannot read the objects that depend on "+ext.Name+" in "+spec.Name+": "+err.Error(), output.ExitPrecondition,
			"Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_dependency_preview_failed", "database": spec.Name, "extension": ext.Name})
	}

	p.OwnTables, p.OwnTablesTotal, p.OwnTablesTruncated, err = extensionPreview(ctx, extensionPG(spec), spec.DatabaseName(), extensionOwnedTablesBody(ext.ExtName), "name")
	if err != nil {
		return p, output.NewError(output.CodePrecondition,
			"cannot read the tables owned by "+ext.Name+" in "+spec.Name+": "+err.Error(), output.ExitPrecondition,
			"Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_dependency_preview_failed", "database": spec.Name, "extension": ext.Name})
	}

	if p.DependentsTotal > 0 && !cascade {
		return p, output.NewError(output.CodePrecondition,
			ext.Name+" in "+spec.Name+" is used by "+extensionCount(p.DependentsTotal, "object")+": "+extensionPreviewText(p.Dependents, p.DependentsTruncated),
			output.ExitPrecondition,
			"Remove those objects first, or re-run with --cascade to drop them with the extension (their data is lost).").
			WithDetails(map[string]any{
				"reason": "extension_in_use", "database": spec.Name, "extension": ext.Name,
				"dependents": p.Dependents, "dependents_total": p.DependentsTotal, "truncated": p.DependentsTruncated,
				"own_tables": p.OwnTables, "own_tables_total": p.OwnTablesTotal,
			})
	}

	return p, nil
}

func extensionRemovePlan(cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension, facts extensionRemoveFactsData, p extRemoveParams, preview extensionRemovePreviewData, env *Env) (config.Cluster, config.ClusterSpec, []string, string, extRemoveResult, error) {
	updated := cfg.Clone()
	if facts.Enabled {
		setExtensionSelection(&updated, spec.Name, removeExtensionName(cfg, spec, ext.Name))
	}

	if followers := cfg.FollowerNames(spec.Name); len(followers) > 0 {
		propagated, _, err := propagateFollowerExtensions(cfg, updated)
		if err != nil {
			return config.Cluster{}, config.ClusterSpec{}, nil, "", extRemoveResult{}, err
		}

		updated = propagated
		if err := updated.Validate(); err != nil {
			return config.Cluster{}, config.ClusterSpec{}, nil, "", extRemoveResult{}, config.ValidationError(err)
		}
	}

	updatedSpec, _ := updated.ClusterByName(spec.Name)

	tokens := extensionRemoveConfirmTokens(spec.Name, facts.RestartRequired)
	if facts.RestartRequired {
		tokens = append(tokens, extensionRestartTokensFor(cfg, spec.Name)...)
		sort.Strings(tokens)
		tokens = slices.Compact(tokens)
	}

	retry := extensionRemoveRetry(spec.Name, ext.Name, tokens)

	res := extRemoveResult{
		Database: spec.Name, Extension: ext.Name, ExtName: ext.ExtName,
		Enabled: facts.Enabled, Installed: facts.Installed, Dependents: preview.Dependents, DependentsTotal: preview.DependentsTotal,
		DependentsTruncated: preview.DependentsTruncated, OwnTables: preview.OwnTables, OwnTablesTotal: preview.OwnTablesTotal,
		OwnTablesTruncated: preview.OwnTablesTruncated, Cascade: p.Cascade, RestartRequired: facts.RestartRequired,
		RequiredConfirmTokens: tokens, DryRun: env.Req.DryRun,
	}
	if ext.Package != "" {
		res.PackageKept = ext.Package
	}

	return updated, updatedSpec, tokens, retry, res, nil
}

func extensionRemoveStandalone(ctx context.Context, env *Env, cfg config.Cluster, spec config.ClusterSpec, updated config.Cluster, updatedSpec config.ClusterSpec, ext catalog.Extension, facts extensionRemoveFactsData, p extRemoveParams, retry string, res extRemoveResult) (Result, error) {
	if err := validateTargetExtensionDescriptors(updated, updatedSpec); err != nil {
		return Result{}, err
	}

	if err := extensionCaptureRequirements(ctx, cfg, spec, updated, updatedSpec, "remove"); err != nil {
		details := extensionRemovePartialDetails(spec, ext, retry, false, facts.RestartRequired)
		details["reason"] = "extension_history_capture_failed"
		details["outgoing_history_may_remain"] = true
		details["incoming_history_may_remain"] = true

		return Result{}, output.NewError(output.CodePartialFailure,
			"recording extension requirements before the removal failed: "+err.Error(), output.ExitStep,
			"Valid extension history may already have been retained; no DROP, reduced intent save, configuration apply or restart ran. Retry "+retry+" after fixing the requirement history store.").
			WithDetails(details)
	}
	// DROP while the library is still loaded; the reduced configuration and
	// restart follow only after the SQL succeeded.
	sqlDropped, err := extensionRemoveDirectDrop(ctx, spec, ext, facts.Installed, p.Cascade)
	if err != nil {
		return Result{}, err
	}

	if facts.RetryConfig {
		return extensionRemoveRetryConfig(ctx, env, cfg, spec, ext, facts, sqlDropped, res)
	}

	if facts.Enabled {
		return extensionRemoveEnabled(ctx, env, updated, updatedSpec, spec, ext, facts, sqlDropped, retry, res)
	}
	// The SQL was installed outside cluster.toml: dropping it needs no
	// configuration save or restart.
	if err := extensionVerifyAbsent(ctx, spec, ext.ExtName, sqlDropped, false); err != nil {
		return Result{}, err
	}

	return Result{Data: res}, nil
}

func extensionRemoveDirectDrop(ctx context.Context, spec config.ClusterSpec, ext catalog.Extension, installed, cascade bool) (bool, error) {
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
			"cluster.toml and the restart are unchanged. Nothing else was changed; read vaire logs postgres --db "+spec.Name+".").
			WithDetails(map[string]any{"reason": "extension_drop_failed", "database": spec.Name, "extension": ext.Name})
	}

	return true, nil
}

func extensionRemoveRetryConfig(ctx context.Context, env *Env, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension, facts extensionRemoveFactsData, sqlDropped bool, res extRemoveResult) (Result, error) {
	env.Cluster = cfg
	rt := runtimeFor(env, false)

	opts := engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
	}
	if facts.RestartRequired {
		opts.Confirm = engine.Confirmation{Clusters: []string{spec.Name}}
	}

	applied, warnings, err := extensionApply(ctx, cfg, rt, opts)
	clearExtensionDeferred(applied)

	if err != nil {
		return Result{}, extensionPartialError("the extension is absent but applying the reduced configuration failed: "+err.Error(),
			"extension_apply_failed", spec.Name, ext.Name, facts.RestartRequired, applied.Restarted(), applied,
			map[string]any{"sql_dropped": sqlDropped, "intent_saved": true})
	}

	res.Restarted = applied.Restarted()

	if err := extensionVerifyAbsent(ctx, spec, ext.ExtName, sqlDropped, true); err != nil {
		return Result{}, err
	}

	return Result{Data: res, Warnings: warnings}, nil
}

func extensionRemoveEnabled(ctx context.Context, env *Env, updated config.Cluster, updatedSpec config.ClusterSpec, spec config.ClusterSpec, ext catalog.Extension, facts extensionRemoveFactsData, sqlDropped bool, retry string, res extRemoveResult) (Result, error) {
	newRaw, err := updated.Encode()
	if err != nil {
		return Result{}, extensionRemoveIntentFailure("encoding the reduced extension intent", err, spec, ext, retry, sqlDropped, facts.RestartRequired)
	}

	if err := extensionSave(newRaw); err != nil {
		return Result{}, extensionRemoveIntentFailure("saving the reduced cluster.toml", err, spec, ext, retry, sqlDropped, facts.RestartRequired)
	}

	env.Cluster = updated
	rt := runtimeFor(env, false)

	opts := engine.Options{
		Only: []string{steps.ConfigStepID(spec.Name)}, DB: spec.Name,
		Restarter: steps.RestarterFor(updated, rt, env.Report), Report: env.Report,
	}
	if facts.RestartRequired {
		opts.Confirm = engine.Confirmation{Clusters: []string{spec.Name}}
	}

	applied, warnings, err := extensionApply(ctx, updated, rt, opts)
	if err != nil {
		return Result{}, extensionPartialError("saving the reduced cluster.toml succeeded, but applying the reduced configuration failed: "+err.Error(),
			"extension_apply_failed", spec.Name, ext.Name, facts.RestartRequired, false, applied,
			map[string]any{"sql_dropped": sqlDropped, "intent_saved": true})
	}

	clearExtensionDeferred(applied)

	if extensionApplyIncomplete(applied) || extensionRestartPending(applied, spec.Name) {
		return Result{}, extensionPartialError("saving the reduced cluster.toml succeeded, but the configuration still waits for a restart",
			"extension_pending_restart", spec.Name, ext.Name, facts.RestartRequired, false, applied,
			map[string]any{"sql_dropped": sqlDropped, "intent_saved": true})
	}

	restarted := applied.Restarted()
	if facts.RestartRequired {
		again, err := extensionRestartIfPending(ctx, updated, updatedSpec, facts.RestartRequired, applied, rt, env.Report)
		if err != nil {
			return Result{}, extensionPartialError("saving the reduced cluster.toml succeeded, but restarting its cluster failed: "+err.Error(),
				"extension_apply_failed", spec.Name, ext.Name, facts.RestartRequired, restarted, applied,
				map[string]any{"sql_dropped": sqlDropped, "intent_saved": true})
		}

		restarted = restarted || again
	}

	res.Restarted = restarted

	if err := extensionVerifyAbsent(ctx, spec, ext.ExtName, sqlDropped, true); err != nil {
		return Result{}, err
	}

	return Result{Data: res, Warnings: warnings}, nil
}

// extensionRemovePartialDetails is the shared direct-removal partial state.
// sqlDropped reports the DROP that actually ran in this invocation;
// configuration_applied and restarted stay false because every caller stopped
// before those stages. pending names the target whose reduced intent still
// needs the reported retry.
func extensionRemovePartialDetails(spec config.ClusterSpec, ext catalog.Extension, retry string, sqlDropped, restartRequired bool) map[string]any {
	return map[string]any{
		"database": spec.Name, "extension": ext.Name,
		"retry": retry, "sql_dropped": sqlDropped,
		"intent_saved": false, "reduced_intent_saved": false,
		"configuration_applied": false, "restart_required": restartRequired,
		"restarted": false, "completed": []string{}, "pending": []string{spec.Name},
	}
}

// extensionRemoveIntentFailure is the shared Encode/Save failure shape after
// the DROP stage. The Encode path is defensive: a validated config.Cluster has
// no public normal-state fixture that makes TOML encoding fail, so the
// fault-injectable Save path exercises this contract.
func extensionRemoveIntentFailure(stage string, err error, spec config.ClusterSpec, ext catalog.Extension, retry string, sqlDropped, restartRequired bool) error {
	message := "the reduced extension intent for " + ext.Name + " was not written because " + stage + " failed: " + err.Error()

	hint := "No SQL was dropped, and cluster.toml still enables " + ext.Name + ". Retry " + retry + "."
	if sqlDropped {
		message = "dropping " + ext.Name + "'s SQL succeeded, but " + stage + " failed: " + err.Error()
		hint = "The extension's SQL is dropped, but cluster.toml still enables it. Retry " + retry + "."
	}

	details := extensionRemovePartialDetails(spec, ext, retry, sqlDropped, restartRequired)
	details["reason"] = "extension_intent_save_failed"

	return output.NewError(output.CodePartialFailure, message, output.ExitStep, hint).WithDetails(details)
}

func extensionRemoveRetryNeeded(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension, env *Env) (bool, []string, string, error) {
	if !extensionClusterRunning(spec) {
		return false, nil, "", nil
	}

	recs, err := extensionRecords()
	if err != nil {
		return false, nil, "", err
	}

	targets, err := extensionTargetSetFor(cfg, recs, spec.Name)
	if err != nil {
		return false, nil, "", err
	}

	live := append([]config.ClusterSpec{targets.Source}, targets.Running...)

	var (
		pendingAll []string
		detailAll  []string
	)

	for _, target := range live {
		if extensionEnabled(cfg, target, ext.Name) {
			return true, []string{target.Name}, "", nil
		}

		pending, err := extensionPendingRestarts(ctx, target)
		if err != nil {
			return false, nil, "", output.NewError(output.CodePrecondition,
				"cannot read "+target.Name+"'s pending postmaster settings: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_pending_unknown", "database": target.Name})
		}

		detail, err := extensionCheckStep(ctx, cfg, runtimeFor(env, false), steps.ConfigStepID(target.Name))
		if err != nil {
			return false, nil, "", output.NewError(output.CodePrecondition,
				"cannot check "+target.Name+"'s configuration convergence: "+err.Error(), output.ExitPrecondition,
				"Nothing was changed. Retry when the cluster answers.").
				WithDetails(map[string]any{"reason": "extension_config_unknown", "database": target.Name})
		}

		pendingAll = append(pendingAll, pending...)
		if detail != "" {
			detailAll = append(detailAll, target.Name+": "+detail)
		}
	}
	// A removed extension may still be replayed on a follower after the
	// primary has already dropped it. Its old guard must stay until the
	// follower catalog proves the SQL absent; that pending work is a valid
	// retry even when the primary is already converged.
	for _, follower := range targets.Running {
		plan, err := steps.PlanFollowerRuntime(ctx, cfg, follower, steps.FollowerRuntimeRequest{
			Class: followerRuntimeClass(cfg, recs, follower), Outgoing: []catalog.Extension{ext}, RetireAbsent: true,
		})
		if err != nil {
			return false, nil, "", err
		}

		if len(plan.PendingSQL) > 0 {
			pendingAll = append(pendingAll, follower.Name)
			detailAll = append(detailAll, follower.Name+": SQL replay still has "+strings.Join(plan.PendingSQL, ", "))
		}
	}

	return len(pendingAll) > 0 || len(detailAll) > 0, pendingAll, strings.Join(detailAll, "; "), nil
}

func extensionVerifyAbsent(ctx context.Context, spec config.ClusterSpec, extName string, sqlDropped, intentSaved bool) error {
	have, err := extensionInstalled(ctx, spec)
	if err != nil {
		message := "verifying that " + extName + " is absent from " + spec.Name + " failed: " + err.Error()
		if sqlDropped {
			message = "the SQL drop ran, but " + message
		}

		return output.NewError(output.CodePartialFailure, message, output.ExitStep,
			"Check vaire ext list --db "+spec.Name+" before retrying; do not drop the extension by hand.").
			WithDetails(map[string]any{"reason": "extension_verify_failed", "database": spec.Name, "extension": extName, "sql_dropped": sqlDropped, "intent_saved": intentSaved})
	}

	if _, ok := have[extName]; ok {
		message := extName + " is still installed in " + spec.Name
		if sqlDropped {
			message += " after DROP"
		} else {
			message += " even though this removal did not run a DROP"
		}

		return output.NewError(output.CodePartialFailure,
			message, output.ExitStep,
			"Read vaire logs postgres --db "+spec.Name+"; do not drop it by hand.").
			WithDetails(map[string]any{"reason": "extension_drop_failed", "database": spec.Name, "extension": extName, "sql_dropped": sqlDropped, "intent_saved": intentSaved})
	}

	return nil
}

// --- target and intent helpers ---

func extensionListSpecs(cfg config.Cluster, db string) ([]config.ClusterSpec, error) {
	if db != "" {
		spec, err := clusterOf(cfg, db)
		if err != nil {
			return nil, err
		}

		return []config.ClusterSpec{spec}, nil
	}

	return slices.Clone(cfg.Clusters), nil
}

func extensionTargetSpec(cfg config.Cluster, recs backup.Records, db, action string, infer bool) (config.ClusterSpec, error) {
	if db != "" {
		spec, err := clusterOf(cfg, db)
		if err != nil {
			return config.ClusterSpec{}, err
		}

		return spec, extensionMutationRefusals(cfg, recs, spec, action)
	}

	if !infer {
		return config.ClusterSpec{}, extensionDatabaseRequired(action)
	}

	candidates := extensionEligibleSpecs(cfg, recs)
	switch len(candidates) {
	case 0:
		return config.ClusterSpec{}, output.NewError(output.CodePrecondition,
			"there is no database that can take an extension change", output.ExitPrecondition,
			"Create an independent database first: vaire db create <name>.").
			WithDetails(map[string]any{"reason": "database_not_found"})
	case 1:
		return candidates[0], extensionMutationRefusals(cfg, recs, candidates[0], action)
	}

	names := make([]string, len(candidates))
	for i, spec := range candidates {
		names[i] = spec.Name
	}

	return config.ClusterSpec{}, output.NewError(output.CodeInputRequired,
		fmt.Sprintf("%s needs --db: the server has %d eligible databases", action, len(names)), output.ExitInput,
		"Pass --db with one of "+strings.Join(names, ", ")+".").
		WithDetails(map[string]any{"flag": "--db", "databases": names})
}

func extensionEligibleSpecs(cfg config.Cluster, recs backup.Records) []config.ClusterSpec {
	var out []config.ClusterSpec

	for _, spec := range cfg.Clusters {
		if spec.IsFollower() || spec.FollowGeneration != "" || recs.IsAwaiting(spec.Name) {
			continue
		}

		if _, ok := recs.OpenRestore(spec.Name); ok {
			continue
		}

		if _, ok := recs.Unfollowing[spec.Name]; ok {
			continue
		}

		out = append(out, spec)
	}

	return out
}

func extensionMutationRefusals(cfg config.Cluster, recs backup.Records, spec config.ClusterSpec, action string) error {
	if spec.IsFollower() {
		return followerError(spec, action)
	}

	if recs.IsAwaiting(spec.Name) {
		return awaitingError(spec.Name, action)
	}

	if r, ok := recs.OpenRestore(spec.Name); ok {
		return output.NewError(output.CodePrecondition,
			"a restore of "+spec.Name+" is open ("+r.Phase+"); "+action+" waits until it is promoted or aborted",
			output.ExitPrecondition,
			"Finish it with vaire restore promote "+spec.Name+" or vaire restore abort "+spec.Name+".").
			WithDetails(map[string]any{"reason": "restore_in_progress", "database": spec.Name, "restore_id": r.ID})
	}

	if _, ok := recs.Unfollowing[spec.Name]; ok {
		return output.NewError(output.CodePrecondition,
			spec.Name+" has an unfinished unfollow, so "+action+" is refused until it settles", output.ExitPrecondition,
			"Finish it with vaire db unfollow "+spec.Name+" --confirm "+spec.Name+"/unfollow, then retry.").
			WithDetails(map[string]any{"reason": "unfollow_in_progress", "database": spec.Name})
	}

	for follower, u := range recs.Unfollowing {
		if u.Source == spec.Name {
			return output.NewError(output.CodePrecondition,
				"the unfollow of "+follower+" from "+spec.Name+" is unfinished, so "+action+" on the source is refused until it settles",
				output.ExitPrecondition,
				"Finish it with vaire db unfollow "+follower+" --confirm "+follower+"/unfollow, then retry.").
				WithDetails(map[string]any{"reason": "unfollow_in_progress", "database": spec.Name, "follower": follower})
		}
	}
	// A direct extension change writes the shared descriptor and reconciles
	// every live follower. An unsettled activation owns those targets until
	// its continuation completes, even after a materialisation record is gone.
	// Routing is included: only the owning continuation may publish and clear
	// it, so no direct extension mutation may write through it.
	for _, name := range append([]string{spec.Name}, cfg.FollowerNames(spec.Name)...) {
		if err := refuseAnyActivationFence(cfg, name); err != nil {
			return err
		}
	}

	if followers := cfg.FollowerNames(spec.Name); len(followers) > 0 {
		// The target set and follower ordering are checked under the sorted
		// cluster locks by the extension transition; this preflight only
		// keeps an obviously unfinished follower from starting.
		if err := extensionFollowerPreflight(cfg, spec.Name); err != nil {
			return err
		}
	}

	return nil
}

// extensionFollowerPreflight refuses a source mutation whose follower set is
// obviously unusable before any lock or effect. The authoritative membership
// check runs again under the locks.
func extensionFollowerPreflight(cfg config.Cluster, source string) error {
	for _, name := range cfg.FollowerNames(source) {
		f, ok := cfg.ClusterByName(name)
		if !ok {
			continue
		}

		if f.FollowGeneration != "" {
			continue
		}

		if !extensionClusterRunning(f) {
			return output.NewError(output.CodePrecondition,
				name+" is a follower of "+source+" but its cluster is not running",
				output.ExitPrecondition, "Start it first (vaire apply --db "+name+"), then retry.").
				WithDetails(map[string]any{"reason": "follower_not_running", "database": name, "source": source})
		}
	}

	return nil
}

func extensionAddIntent(cfg config.Cluster, db string, p extAddParams) (config.Cluster, catalog.Extension, bool, error) {
	name := strings.TrimSpace(p.Name)
	if name == "" {
		return config.Cluster{}, catalog.Extension{}, false, output.NewError(output.CodeUsage, "ext add needs an extension name", output.ExitUsage, "")
	}

	builtin, isBuiltin := catalog.Lookup(name)

	canonical := name
	if isBuiltin {
		canonical = builtin.Name
	}

	_, hasExisting := cfg.Extensions[canonical]

	direct := p.Package != "" || p.Preinstalled || p.Preload != "" || len(p.Settings) > 0 ||
		p.Schema != "" || p.CopySafety != "" || len(p.CopySettings) > 0 || p.StandbySafe != nil
	if !direct {
		updated, ext, err := extensionAddIntentSelection(cfg, db, name, isBuiltin, hasExisting)
		return updated, ext, false, err
	}

	updated, ext, err := extensionAddIntentDirect(cfg, db, p, canonical, isBuiltin, hasExisting)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, false, err
	}

	return updated, ext, true, nil
}

func extensionAddIntentSelection(cfg config.Cluster, db, name string, isBuiltin, hasExisting bool) (config.Cluster, catalog.Extension, error) {
	if !isBuiltin && !hasExisting {
		return config.Cluster{}, catalog.Extension{}, extensionNotFound(name)
	}

	ext, err := cfg.ExtensionResolver().Resolve(name)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, config.ValidationError(err)
	}

	if !ext.Managed() {
		return config.Cluster{}, catalog.Extension{}, extensionNotManaged(ext.Name)
	}

	updated := cfg.Clone()
	setExtensionSelection(&updated, db, appendExtensionName(cfg, db, ext.Name))

	propagated, _, err := propagateFollowerExtensions(cfg, updated)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, err
	}

	updated = propagated
	if err := updated.Validate(); err != nil {
		return config.Cluster{}, catalog.Extension{}, config.ValidationError(err)
	}

	return updated, ext, nil
}

func extensionAddIntentDirect(cfg config.Cluster, db string, p extAddParams, canonical string, isBuiltin, hasExisting bool) (config.Cluster, catalog.Extension, error) {
	if hasExisting && !isBuiltin {
		return config.Cluster{}, catalog.Extension{}, output.NewError(output.CodePrecondition,
			"extensions."+canonical+" already exists, so ext add will not replace its descriptor", output.ExitPrecondition,
			"Edit it with vaire config edit, then select it by name: vaire ext add "+canonical+" --db "+db+".").
			WithDetails(map[string]any{"reason": "extension_definition_exists", "extension": canonical})
	}

	if p.Preinstalled && p.Package != "" {
		return config.Cluster{}, catalog.Extension{}, output.NewError(output.CodeUsage,
			"--package and --preinstalled are mutually exclusive", output.ExitUsage, "")
	}

	base := config.ExtensionDefinition{}
	if hasExisting {
		base = cfg.Extensions[canonical]
	}

	def := extensionDefinitionFromFlags(base, p)

	updated := cfg.Clone()
	if updated.Extensions == nil {
		updated.Extensions = map[string]config.ExtensionDefinition{}
	}

	updated.Extensions[canonical] = def
	setExtensionSelection(&updated, db, appendExtensionName(cfg, db, canonical))
	// Resolve the proposed provider with placeholder safety so a missing
	// renewal is reported as a renewal error, not as a generic resolver
	// failure. The actual descriptor is resolved again after validation.
	probe := def.Clone()
	if probe.CopySafety == nil {
		v := catalog.CopySafetyUnknown
		probe.CopySafety = &v
	}

	if probe.StandbySafe == nil {
		v := false
		probe.StandbySafe = &v
	}

	probeCfg := updated.Clone()
	probeCfg.Extensions[canonical] = probe

	proposed, err := probeCfg.ExtensionResolver().Resolve(canonical)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, config.ValidationError(err)
	}

	oldExt, oldOK := cfg.ExtensionResolver().Lookup(canonical)
	if oldOK && (oldExt.Source != proposed.Source || oldExt.Package != proposed.Package ||
		oldExt.Preload != proposed.Preload || oldExt.ExtName != proposed.ExtName) {
		if err := extensionAddSafetyRenewal(db, canonical, p); err != nil {
			return config.Cluster{}, catalog.Extension{}, err
		}
	}

	propagated, _, err := propagateFollowerExtensions(cfg, updated)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, err
	}

	updated = propagated
	if err := updated.Validate(); err != nil {
		return config.Cluster{}, catalog.Extension{}, config.ValidationError(err)
	}

	ext, err := updated.ExtensionResolver().Resolve(canonical)
	if err != nil {
		return config.Cluster{}, catalog.Extension{}, config.ValidationError(err)
	}

	if !ext.Managed() {
		return config.Cluster{}, catalog.Extension{}, extensionNotManaged(ext.Name)
	}

	return updated, ext, nil
}

func extensionAddSafetyRenewal(db, canonical string, p extAddParams) error {
	var missing []string
	if p.CopySafety == "" {
		missing = append(missing, "copy_safety")
	}

	if p.StandbySafe == nil {
		missing = append(missing, "standby_safe")
	}

	if len(missing) == 0 {
		return nil
	}

	return extensionSafetyRenewalError(db, canonical, missing)
}

// refuseInstalledExtensionSchemas refuses any selected installed SQL
// extension whose actual metadata namespace contradicts the descriptor's
// explicit schema. It runs before a descriptor/config save and before roles
// SQL or grants, so an unchanged descriptor and a newly selected definition
// are guarded alike. A descriptor with no schema asserts no namespace: its
// built-in grant/owned-object schema may legitimately differ from
// pg_extension.extnamespace. A stopped cluster has no readable catalog here;
// the roles step revalidates before its own effects after a start.
func refuseInstalledExtensionSchemas(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	entries, err := steps.ResolveExtensionSelection(cfg, spec)
	if err != nil {
		return err
	}

	guarded := make([]catalog.Extension, 0, len(entries))
	for _, ext := range entries {
		if ext.ExtName != "" && ext.Schema != "" {
			guarded = append(guarded, ext)
		}
	}

	if len(guarded) == 0 || !extensionClusterRunning(spec) {
		return nil
	}

	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"cannot read "+spec.Name+"'s installed extension schemas: "+err.Error(), output.ExitPrecondition,
			"Nothing was changed. Retry when the cluster answers.").
			WithDetails(map[string]any{"reason": "extension_facts_unavailable", "database": spec.Name})
	}

	schemas := make(map[string]string, len(installed))
	for name, got := range installed {
		schemas[name] = got.Schema
	}

	return steps.ValidateInstalledExtensionSchemas(spec.Name, guarded, schemas)
}

// extensionDefinitionImpact lists the databases other than target whose
// effective current or recovery intent would change with the proposed global
// definition. E704 refuses such a shared change: only the named standalone
// primary may be reconciled here; E705 owns the full target set.
func extensionDefinitionImpact(cfg, proposed config.Cluster, canonical, target string) ([]string, error) {
	oldExt, oldOK := cfg.ExtensionResolver().Lookup(canonical)
	if !oldOK {
		// A brand-new custom key has no valid existing selection. The target's
		// own selection is the only change this command is allowed to make.
		return nil, nil
	}

	newExt, err := proposed.ExtensionResolver().Resolve(canonical)
	if err != nil {
		return nil, err
	}

	if oldExt.SemanticEqual(newExt) {
		return nil, nil
	}

	affected := map[string]bool{}

	add := func(spec config.ClusterSpec) error {
		selections := [][]string{spec.Extensions}
		if spec.RecoveryExtensions != nil {
			selections = append(selections, *spec.RecoveryExtensions)
		}

		for _, names := range selections {
			effective, err := cfg.ExtensionResolver().EffectiveNames(names)
			if err != nil {
				return err
			}

			if slices.Contains(effective, canonical) {
				affected[spec.Name] = true
			}
		}

		return nil
	}
	for _, spec := range cfg.Clusters {
		if err := add(spec); err != nil {
			return nil, err
		}
	}

	for _, spec := range cfg.Awaiting {
		if err := add(spec); err != nil {
			return nil, err
		}
	}

	delete(affected, target)

	out := make([]string, 0, len(affected))
	for name := range affected {
		out = append(out, name)
	}

	sort.Strings(out)

	return out, nil
}

func extensionGlobalIntentConflict(target, canonical string, affected []string) error {
	return output.NewError(output.CodePrecondition,
		"changing [extensions."+canonical+"] would also change "+strings.Join(affected, ", ")+", not only "+target,
		output.ExitPrecondition,
		"Nothing was changed. Edit the shared definition with vaire config edit and apply every affected database deliberately; ext add only reconciles the named standalone primary.").
		WithDetails(map[string]any{"reason": "extension_global_intent_conflict", "database": target, "extension": canonical, "affected": affected})
}

// extensionDefinitionFromFlags applies only the fields the direct command
// supplied over the existing definition. A partial edit never drops an
// explicit provider, safety declaration, schema or setting from a built-in
// override. Removing a field still requires editing cluster.toml.
func extensionSafetyRenewalError(db, canonical string, missing []string) error {
	return output.NewError(output.CodePrecondition,
		"changing the provider or SQL extension name for "+canonical+" requires fresh safety declarations: missing "+strings.Join(missing, ", "),
		output.ExitPrecondition,
		"Pass --copy-safety unknown|safe|disabled and/or --standby-safe explicitly; an existing safety declaration is not inherited when source, package, preload or ext_name changes.").
		WithDetails(map[string]any{"reason": "extension_safety_renewal_required", "database": db, "extension": canonical, "missing": missing})
}

func extensionDefinitionFromFlags(base config.ExtensionDefinition, p extAddParams) config.ExtensionDefinition {
	def := base.Clone()

	switch {
	case p.Preinstalled:
		def.Source, def.Package = catalog.SourcePreinstalled, ""
	case p.Package != "":
		def.Source, def.Package = catalog.SourceAPT, p.Package
	}

	if p.Preload != "" {
		def.Preload = p.Preload
	}

	if len(p.Settings) > 0 {
		def.Settings = mergeExtensionMap(def.Settings, p.Settings)
	}

	if p.Schema != "" {
		def.Schema = p.Schema
	}

	if p.CopySafety != "" {
		v := p.CopySafety
		def.CopySafety = &v
	}

	if len(p.CopySettings) > 0 {
		def.CopySettings = mergeExtensionMap(def.CopySettings, p.CopySettings)
	}

	if p.StandbySafe != nil {
		v := *p.StandbySafe
		def.StandbySafe = &v
	}

	return def
}

func mergeExtensionMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}

	out := make(map[string]string, len(base)+len(override))
	maps.Copy(out, base)
	maps.Copy(out, override)

	return out
}

func extensionEnabled(cfg config.Cluster, spec config.ClusterSpec, name string) bool {
	canonical, _ := cfg.ExtensionResolver().Canonical(name)

	effective, err := cfg.ExtensionResolver().EffectiveNames(spec.Extensions)
	if err != nil {
		return false
	}

	return slices.Contains(effective, canonical)
}

func appendExtensionName(cfg config.Cluster, db, name string) []string {
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		return []string{name}
	}

	out := slices.Clone(spec.Extensions)
	if extensionEnabled(cfg, spec, name) {
		return out
	}

	return append(out, name)
}

func removeExtensionName(cfg config.Cluster, spec config.ClusterSpec, name string) []string {
	canonical, _ := cfg.ExtensionResolver().Canonical(name)

	out := make([]string, 0, len(spec.Extensions))
	for _, n := range spec.Extensions {
		c, _ := cfg.ExtensionResolver().Canonical(n)
		if c != canonical {
			out = append(out, n)
		}
	}

	return out
}

func setExtensionSelection(cfg *config.Cluster, db string, names []string) {
	for i := range cfg.Clusters {
		if cfg.Clusters[i].Name == db {
			cfg.Clusters[i].Extensions = names
			return
		}
	}
}

func extensionAddConfirmTokens(db string, restart bool) []string {
	if !restart {
		return nil
	}

	return []string{db + "/restart"}
}

func extensionRemoveConfirmTokens(db string, restart bool) []string {
	out := []string{db}
	if restart {
		out = append(out, db+"/restart")
	}

	return out
}

// extensionAddTokens and extensionRemoveTokens are the dispatcher's accepted
// token sets. Only the named target's tokens are accepted: a broad
// <server>/restart token can never authorize a direct extension change.
func extensionAddTokens(cfg config.Cluster, params json.RawMessage) []string {
	var p extAddParams
	if json.Unmarshal(params, &p) != nil {
		return nil
	}

	db := p.DB
	if db == "" {
		recs, err := extensionRecords()
		if err != nil {
			return nil
		}

		candidates := extensionEligibleSpecs(cfg, recs)
		if len(candidates) != 1 {
			return nil
		}

		db = candidates[0].Name
	}

	out := make([]string, 0, 1+len(cfg.FollowerNames(db)))
	out = append(out, db+"/restart")
	out = append(out, extensionRestartTokensFor(cfg, db)...)
	sort.Strings(out)

	return slices.Compact(out)
}

func extensionRemoveTokens(cfg config.Cluster, params json.RawMessage) []string {
	var p extRemoveParams
	if json.Unmarshal(params, &p) != nil || p.DB == "" {
		return nil
	}

	out := make([]string, 0, 2+len(cfg.FollowerNames(p.DB)))
	out = append(out, p.DB, p.DB+"/restart")
	out = append(out, extensionRestartTokensFor(cfg, p.DB)...)
	sort.Strings(out)

	return slices.Compact(out)
}

func extensionMutationLock(req rpc.Request) engine.LockMode {
	if req.DryRun {
		return engine.LockShared
	}

	return engine.LockExclusive
}

func extensionClusterLock(req rpc.Request) (string, engine.LockMode) {
	// The handler takes the complete target set's locks once, sorted, after
	// the dispatcher's global exclusive envelope. Taking the named cluster
	// here first would acquire source-then-followers and could deadlock.
	return "", engine.LockNone
}

func hasConfirmToken(req rpc.Request, token string) bool {
	return slices.Contains(req.ConfirmTokens(), token)
}

func extensionClusterNotRunning(db, action string) error {
	return output.NewError(output.CodePrecondition,
		db+"'s cluster is not running, so "+action+" cannot read or change its database", output.ExitPrecondition,
		"Start it first: vaire apply --db "+db+", then retry.").
		WithDetails(map[string]any{"reason": "cluster_not_running", "database": db})
}

func extensionDatabaseRequired(action string) error {
	return output.NewError(output.CodeInputRequired,
		action+" needs --db", output.ExitInput,
		"Pass --db with the database whose extension changes.").
		WithDetails(map[string]any{"flag": "--db", "reason": "database_required"})
}

func extensionNotFound(name string) error {
	return output.NewError(output.CodePrecondition,
		name+" is not in vaire's extension catalogue and has no [extensions."+name+"] definition", output.ExitPrecondition,
		"Run vaire ext list to see known names, or add a definition with --package or --preinstalled.").
		WithDetails(map[string]any{"reason": "extension_not_found", "extension": name})
}

func extensionNotManaged(name string) error {
	return output.NewError(output.CodePrecondition,
		name+" is preload-only and has no CREATE EXTENSION name", output.ExitPrecondition,
		"It is configured through its settings; vaire ext add manages only SQL extensions.").
		WithDetails(map[string]any{"reason": "extension_not_managed", "extension": name})
}

func extensionDefault(name string) error {
	return output.NewError(output.CodePrecondition,
		name+" is a default module that every database keeps enabled, so it cannot be removed", output.ExitPrecondition,
		"Leave the default module enabled; change its settings in cluster.toml instead.").
		WithDetails(map[string]any{"reason": "extension_default", "extension": name})
}

func extensionNotInstalled(name, db string, enabled, installed bool) error {
	return output.NewError(output.CodePrecondition,
		name+" is not enabled in cluster.toml and is not installed in "+db, output.ExitPrecondition,
		"vaire ext list --db "+db+" shows its extensions.").
		WithDetails(map[string]any{"reason": "extension_not_installed", "extension": name, "database": db, "enabled": enabled, "installed": installed})
}

func extensionPartialError(message, reason, db, name string, restart, restarted bool, applied engine.ApplyData, extra ...map[string]any) error {
	retry := ""

	for _, m := range extra {
		if v, ok := m["retry"].(string); ok && v != "" {
			retry = v
		}
	}

	return extensionPartialErrorWithRetry(message, reason, db, name, restart, restarted, applied, retry, extra...)
}

func extensionDefaultRetry(db string, restart bool) string {
	retry := "vaire apply --db " + db
	if restart {
		retry += " --confirm " + db + "/restart"
	}

	return retry
}

func extensionPartialErrorWithRetry(message, reason, db, name string, restart, restarted bool, applied engine.ApplyData, retry string, extra ...map[string]any) error {
	if retry == "" {
		retry = extensionDefaultRetry(db, restart)
	}

	details := map[string]any{
		"reason": reason, "database": db, "extension": name,
		"intent_saved": true, "restart_required": restart, "restarted": restarted,
		"retry": retry,
	}
	for _, m := range extra {
		maps.Copy(details, m)
	}

	if len(applied.Steps) > 0 {
		states := make([]string, 0, len(applied.Steps))
		for _, step := range applied.Steps {
			states = append(states, step.ID+"="+step.State)
		}

		details["steps"] = states
	}

	return output.NewError(output.CodePartialFailure, message, output.ExitStep,
		"Retry with "+retry+". Installed packages are not removed.").WithDetails(details)
}

func extensionAddRetry(db, name string, tokens []string) string {
	var retry strings.Builder
	retry.WriteString("vaire ext add " + name + " --db " + db)

	for _, token := range tokens {
		retry.WriteString(" --confirm " + token)
	}

	return retry.String()
}

func extensionRemoveRetry(db, name string, tokens []string) string {
	var retry strings.Builder
	retry.WriteString("vaire ext remove " + name + " --db " + db + " --confirm " + db)

	for _, token := range tokens {
		retry.WriteString(" --confirm " + token)
	}

	return retry.String()
}

func extensionApplyRetry(server, source string, confirm engine.Confirmation) string {
	var retry strings.Builder
	retry.WriteString("vaire apply --db " + source)

	if confirm.Server {
		retry.WriteString(" --confirm " + server + "/restart")
	}

	for _, cluster := range confirm.Clusters {
		retry.WriteString(" --confirm " + cluster + "/restart")
	}

	return retry.String()
}

func extensionApplyIncomplete(data engine.ApplyData) bool {
	for _, step := range data.Steps {
		if step.State == engine.StatusIncomplete {
			return true
		}
	}

	return false
}

func extensionWarningCode(warnings []output.Warning, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}

	return false
}

func extensionRestartPending(data engine.ApplyData, cluster string) bool {
	for _, id := range data.RestartPending {
		if _, c := engine.SplitID(id); c == cluster {
			return true
		}
	}

	return false
}

// extensionRestartIfPending finishes a restart the configuration step did not
// request because its files already matched: an enabled preload can still wait
// in pg_settings.pending_restart. The caller has already validated the exact
// target's restart token. It restarts only that cluster.
func extensionRestartIfPending(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, required bool, applied engine.ApplyData, rt engine.Runtime, report engine.Reporter) (bool, error) {
	if !required {
		return false, nil
	}

	if applied.Restarted() {
		return true, nil
	}

	pending, err := extensionPendingRestarts(ctx, spec)
	if err != nil {
		return false, err
	}

	if len(pending) == 0 {
		return false, nil
	}

	if _, err := extensionRestart(ctx, cfg, spec, rt, report); err != nil {
		return false, err
	}

	extensionClearDeferred(spec.Name)

	return true, nil
}

func clearExtensionDeferred(data engine.ApplyData) {
	for _, r := range data.Restarts {
		extensionClearDeferred(r.Cluster)
	}

	for _, name := range data.RestartsSettled {
		extensionClearDeferred(name)
	}
}

func extensionUpdateDirectionUnknown(db, name, version, def string) error {
	return output.NewError(output.CodePrecondition,
		name+" is installed at "+version+" and the package's default is "+def+", but vaire cannot establish that the default is not a downgrade",
		output.ExitPrecondition,
		"The extension stays at "+version+"; no ALTER EXTENSION UPDATE is run without a non-downgrade ordering. Update it manually through a documented path, or use numeric dotted versions.").
		WithDetails(map[string]any{"reason": "extension_update_direction_unknown", "database": db, "extension": name, "version": version, "default_version": def})
}

func extensionDowngradeError(db, name, version, def string) error {
	return output.NewError(output.CodePrecondition,
		name+" is installed at "+version+" but its package's default is the older "+def+"; no downgrade is performed",
		output.ExitPrecondition,
		"The extension stays at "+version+". Install the newer package or keep this version; vaire does not downgrade extensions.").
		WithDetails(map[string]any{"reason": "extension_downgrade_refused", "database": db, "extension": name, "version": version, "default_version": def})
}

func extensionUpdateAvailable(version, def string) bool {
	if def == "" || def == version {
		return false
	}

	if cmp, ok := extensionVersionCompare(version, def); ok {
		return cmp < 0
	}

	return true
}

func extensionDowngrade(version, def string) bool {
	if def == "" || def == version {
		return false
	}

	cmp, ok := extensionVersionCompare(version, def)

	return ok && cmp > 0
}

func extensionVersionCompare(a, b string) (int, bool) {
	as, aok := extensionVersionParts(a)

	bs, bok := extensionVersionParts(b)
	if !aok || !bok {
		return 0, false
	}

	for len(as) < len(bs) {
		as = append(as, 0)
	}

	for len(bs) < len(as) {
		bs = append(bs, 0)
	}

	for i := range as {
		if as[i] < bs[i] {
			return -1, true
		}

		if as[i] > bs[i] {
			return 1, true
		}
	}

	return 0, true
}

func extensionVersionParts(v string) ([]int, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, false
	}

	parts := strings.Split(v, ".")

	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return nil, false
		}

		out = append(out, n)
	}

	return out, true
}

func extensionLiteral(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "''") + "'"
}

// --- list facts ---

func extensionListDatabaseFacts(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, recs backup.Records) extListDatabase {
	db := extListDatabase{DB: spec.Name, State: stateRunning, Available: true}

	effective, err := cfg.ExtensionResolver().EffectiveNames(spec.Extensions)
	if err != nil {
		db.State, db.Available, db.Error = "unreachable", false, err.Error()
		return db
	}

	db.enabled = effective
	if recs.IsAwaiting(spec.Name) {
		db.State, db.Available, db.Error = stateAwaiting, false, "awaiting recovery; run vaire db recover "+spec.Name
		return db
	}

	if !extensionClusterRunning(spec) {
		db.State, db.Available, db.Error = stateStopped, false, "the cluster is stopped"
		return db
	}

	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		db.State, db.Available, db.Error = "unreachable", false, err.Error()
		return db
	}

	db.installedValues = installed

	loaded, pending, err := extensionPreloadFacts(ctx, spec)
	if err != nil {
		db.PreloadKnown = false
	} else {
		db.PreloadKnown, db.loaded, db.pending = true, loaded, pending
	}

	return db
}

func (d extListDatabase) installed() map[string]extensionInstalledVersion {
	return d.installedValues
}

func extensionAvailabilityFor(ext catalog.Extension) extensionAvailability {
	if ext.ExtName == "" {
		if ext.Preload == "" {
			return extensionAvailability{Known: true, Available: false, Error: "it has no control file or preload library"}
		}

		if err := extensionLibraryExists(ext.Preload); err != nil {
			return extensionAvailability{Known: true, Available: false, Error: err.Error()}
		}

		return extensionAvailability{Known: true, Available: true}
	}

	control := filepath.Join(extensionControlDir, ext.ExtName+".control")

	raw, err := os.ReadFile(control) // A catalogue name under PostgreSQL's extension directory.
	if err != nil {
		if os.IsNotExist(err) {
			return extensionAvailability{Known: true, Available: false, Error: "control file " + control + " is missing"}
		}

		return extensionAvailability{Known: false, Available: false, Error: "cannot read control file " + control + ": " + err.Error()}
	}

	defaultVersion := parseExtensionControlDefaultVersion(string(raw))
	if module, found, err := parseExtensionControlModule(string(raw)); err != nil {
		return extensionAvailability{Known: true, Available: false, DefaultVersion: defaultVersion, Error: err.Error()}
	} else if found {
		if err := extensionLibraryExists(module); err != nil {
			return extensionAvailability{Known: true, Available: false, DefaultVersion: defaultVersion, Error: err.Error()}
		}
	}

	if ext.Preload != "" {
		if err := extensionLibraryExists(ext.Preload); err != nil {
			return extensionAvailability{Known: true, Available: false, DefaultVersion: defaultVersion, Error: err.Error()}
		}
	}

	return extensionAvailability{Known: true, Available: true, DefaultVersion: defaultVersion}
}

// parseExtensionControlModule reads a control file's module_pathname. found is
// false for a SQL-only extension. A malformed or unsupported form is an error,
// never silently treated as "no library".
func parseExtensionControlModule(text string) (string, bool, error) {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			if strings.HasPrefix(line, "module_pathname") {
				return "", true, errors.New("malformed module_pathname in control file")
			}

			continue
		}

		if strings.TrimSpace(key) != "module_pathname" {
			continue
		}

		value = strings.TrimSpace(value)
		if comment := strings.IndexByte(value, '#'); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}

		value = strings.Trim(value, `'"`)
		if value == "" {
			return "", true, errors.New("empty module_pathname in control file")
		}

		return value, true, nil
	}

	return "", false, nil
}

func extensionLibraryExists(name string) error {
	path, err := extensionLibraryPath(name)
	if err != nil {
		return err
	}

	if _, err := os.Stat(path); err != nil { //nolint:gosec // G703: PostgreSQL's own control file names its module library; this is a read-only existence check.
		if os.IsNotExist(err) {
			return errors.New("library " + path + " is missing")
		}

		return errors.New("cannot read library " + path + ": " + err.Error())
	}

	return nil
}

func extensionLibraryPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New("empty library name")
	}

	switch {
	case strings.HasPrefix(name, "$libdir/"):
		name = strings.TrimPrefix(name, "$libdir/")
	case strings.HasPrefix(name, "$"):
		return "", fmt.Errorf("unsupported module path %q", name)
	}

	if name == "" {
		return "", errors.New("empty module path")
	}

	if !strings.HasSuffix(name, ".so") && !strings.Contains(name, ".so.") {
		name += ".so"
	}

	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}

	return filepath.Join(extensionLibDir, filepath.Clean(name)), nil
}

func parseExtensionControlDefaultVersion(text string) string {
	sc := bufio.NewScanner(strings.NewReader(text))
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), "=")
		if !ok || strings.TrimSpace(key) != "default_version" {
			continue
		}

		value = strings.TrimSpace(value)
		if comment := strings.IndexByte(value, '#'); comment >= 0 {
			value = strings.TrimSpace(value[:comment])
		}

		return strings.Trim(value, `'"`)
	}

	return ""
}

func extensionPackageInstalledDefault(ctx context.Context, pkg string) (bool, bool, string) {
	out, err := steps.Probe(ctx, "dpkg-query", "-W", "-f=${Status}", pkg)
	if err != nil {
		if strings.Contains(err.Error(), "no packages found matching") {
			return false, true, ""
		}

		return false, false, err.Error()
	}

	return steps.PackageInstalled(out), true, ""
}

// --- PostgreSQL facts and previews ---

const extensionInstalledSQL = `SELECT e.extname AS name, e.extversion AS version, coalesce(a.default_version, '') AS default_version, coalesce(n.nspname, '') AS schema
FROM pg_extension e
LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
LEFT JOIN pg_available_extensions a ON a.name = e.extname ORDER BY 1`

func extensionInstalledDefault(ctx context.Context, spec config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
	ctx, cancel := context.WithTimeout(ctx, extensionQueryTimeout)
	defer cancel()

	rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(), extensionInstalledSQL, nil)
	if err != nil {
		return nil, err
	}

	out := map[string]extensionInstalledVersion{}
	for _, row := range rows {
		out[row["name"]] = extensionInstalledVersion{Name: row["name"], Version: row["version"], Default: row["default_version"], Schema: row["schema"]}
	}

	return out, nil
}

func extensionPreloadFactsDefault(ctx context.Context, spec config.ClusterSpec) (map[string]bool, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, extensionQueryTimeout)
	defer cancel()

	local := steps.ClusterPG(spec)

	loaded, err := steps.LoadedPreloads(ctx, local)
	if err != nil {
		return nil, false, err
	}

	pending, err := steps.SharedPreloadsPending(ctx, local)
	if err != nil {
		return nil, false, err
	}

	return loaded, pending, nil
}

// extensionJournalConvergedDefault asks the engine's read-only planner about
// the exact target config and roles instances. Plan compares current Inputs
// hashes, journal status, checks and pending restarts without mutating
// anything, so a completed journal for an older provider or semantic identity
// cannot authorize a no-op.
func extensionJournalConvergedDefault(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime) (bool, error) {
	plan, err := engine.Plan(ctx, steps.Catalog(), cfg, rt, engine.Options{
		Only:        []string{steps.ConfigStepID(spec.Name), steps.RolesStepID(spec.Name)},
		DB:          spec.Name,
		JournalPath: extensionJournalPath,
	})
	if err != nil {
		return false, err
	}

	if len(plan.Steps) != 2 || len(plan.RestartPending) > 0 {
		return false, nil
	}

	for _, step := range plan.Steps {
		if step.Action != "skip" {
			return false, nil
		}
	}

	return true, nil
}

func extensionPrimaryReadiness(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension) (steps.PrimaryExtensionReadiness, error) {
	loaded, preloadPending, err := extensionPreloadFacts(ctx, spec)
	if err != nil {
		return steps.PrimaryExtensionReadiness{}, err
	}

	pendingList, err := extensionPendingRestarts(ctx, spec)
	if err != nil {
		return steps.PrimaryExtensionReadiness{}, err
	}

	pending := map[string]bool{}
	if preloadPending {
		pending["shared_preload_libraries"] = true
	}

	for _, key := range pendingList {
		pending[key] = true
	}

	expected := steps.PrimaryExtensionExpectedSettings(cfg, spec, ext)

	keys := make([]string, 0, len(expected))
	for key := range expected {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	actual, err := extensionSettingValues(ctx, spec, keys)
	if err != nil {
		return steps.PrimaryExtensionReadiness{}, err
	}

	return steps.CheckPrimaryExtensionReadiness(cfg, spec, ext, loaded, pending, actual)
}

func extensionSettingValuesDefault(ctx context.Context, spec config.ClusterSpec, keys []string) (map[string]steps.ExtensionSettingFact, error) {
	out := make(map[string]steps.ExtensionSettingFact, len(keys))
	for _, key := range keys {
		rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(),
			steps.ExtensionSettingFactSQL(key), nil)
		if err != nil {
			return nil, err
		}

		fact := steps.ExtensionSettingFact{}
		if len(rows) > 0 {
			fact = steps.ParseExtensionSettingFact(rows[0])
		}

		out[key] = fact
	}

	return out, nil
}

func extensionPendingRestartsDefault(ctx context.Context, spec config.ClusterSpec) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, extensionQueryTimeout)
	defer cancel()

	rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(), "SELECT name FROM pg_settings WHERE pending_restart ORDER BY name", nil)
	if err != nil {
		return nil, err
	}

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row["name"])
	}

	sort.Strings(out)

	return out, nil
}

func extensionUpdatePathDefault(ctx context.Context, spec config.ClusterSpec, extName, from, to string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, extensionQueryTimeout)
	defer cancel()

	sql := `SELECT coalesce(path, '') AS path FROM pg_extension_update_paths(` + extensionLiteral(extName) + `)
WHERE source = ` + extensionLiteral(from) + ` AND target = ` + extensionLiteral(to)

	rows, err := extensionPG(spec).Query(ctx, spec.DatabaseName(), sql, nil)
	if err != nil {
		return false, err
	}

	if len(rows) == 0 {
		return false, nil
	}

	return rows[0]["path"] != "", nil
}

func extensionDependentsBody(extname string) string {
	// DROP EXTENSION RESTRICT drops extension members plus every object
	// PostgreSQL reaches through incoming extension, internal, automatic,
	// auto-extension, or partition dependencies; only normal dependents
	// outside that closure need CASCADE. Follow those incoming edges over
	// full object addresses, where a whole-object address (objsubid 0)
	// covers its subobjects. An object that would first have to redirect to
	// an owner outside the closure, or whose partition dependency is not
	// satisfied by the closure, is reported conservatively instead of being
	// assumed automatic. PostgreSQL remains authoritative for the actual
	// DROP.
	return `WITH RECURSIVE ext AS (
  SELECT oid FROM pg_extension WHERE extname = ` + extensionLiteral(extname) + `
),
auto_closure(classid, objid, objsubid) AS (
  SELECT 'pg_extension'::regclass, oid, 0 FROM ext
  UNION
  SELECT d.classid, d.objid, d.objsubid
  FROM pg_depend d
  JOIN auto_closure a
    ON d.refclassid = a.classid
   AND d.refobjid = a.objid
   AND (a.objsubid = 0 OR d.refobjsubid = a.objsubid)
  WHERE d.deptype IN ('e', 'a', 'i', 'x', 'P', 'S')
),
unsafe_auto AS (
  SELECT a.classid, a.objid, a.objsubid
  FROM auto_closure a
  WHERE EXISTS (
    SELECT 1
    FROM pg_depend owner
    WHERE owner.classid = a.classid
      AND owner.objid = a.objid
      AND (a.objsubid = 0 OR owner.objsubid = a.objsubid)
      AND owner.deptype IN ('i', 'e')
      AND NOT EXISTS (
        SELECT 1 FROM auto_closure o
        WHERE o.classid = owner.refclassid
          AND o.objid = owner.refobjid
          AND (o.objsubid = 0 OR owner.refobjsubid = o.objsubid)
      )
  )
  OR (
    EXISTS (
      SELECT 1
      FROM pg_depend part
      WHERE part.classid = a.classid
        AND part.objid = a.objid
        AND (a.objsubid = 0 OR part.objsubid = a.objsubid)
        AND part.deptype IN ('P', 'S')
    )
    AND NOT EXISTS (
      SELECT 1
      FROM pg_depend part
      JOIN auto_closure o
        ON o.classid = part.refclassid
       AND o.objid = part.refobjid
       AND (o.objsubid = 0 OR part.refobjsubid = o.objsubid)
      WHERE part.classid = a.classid
        AND part.objid = a.objid
        AND (a.objsubid = 0 OR part.objsubid = a.objsubid)
        AND part.deptype IN ('P', 'S')
    )
  )
)
SELECT DISTINCT pg_describe_object(d.classid, d.objid, d.objsubid) AS object
FROM pg_depend d
JOIN auto_closure a
  ON d.refclassid = a.classid
 AND d.refobjid = a.objid
 AND (a.objsubid = 0 OR d.refobjsubid = a.objsubid)
WHERE d.deptype = 'n'
  AND NOT EXISTS (
    SELECT 1 FROM auto_closure covered
    WHERE covered.classid = d.classid
      AND covered.objid = d.objid
      AND (covered.objsubid = 0 OR covered.objsubid = d.objsubid)
  )
UNION
SELECT DISTINCT pg_describe_object(u.classid, u.objid, u.objsubid) AS object
FROM unsafe_auto u`
}

func extensionOwnedTablesBody(extname string) string {
	return `WITH ext AS (SELECT oid FROM pg_extension WHERE extname = ` + extensionLiteral(extname) + `)
SELECT c.oid::regclass::text AS name FROM pg_depend d JOIN pg_class c ON c.oid = d.objid
WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_extension'::regclass AND d.deptype = 'e'
  AND d.refobjid = (SELECT oid FROM ext) AND c.relkind IN ('r', 'p')`
}

func extensionPreview(ctx context.Context, local sqlRunner, db, body, column string) ([]string, int, bool, error) {
	rows, err := local.Query(ctx, db, body+" ORDER BY 1 LIMIT "+strconv.Itoa(extensionPreviewLimit+1), nil)
	if err != nil {
		return nil, 0, false, err
	}

	truncated := len(rows) > extensionPreviewLimit
	if truncated {
		rows = rows[:extensionPreviewLimit]
	}

	items := make([]string, 0, len(rows))
	for _, row := range rows {
		items = append(items, row[column])
	}

	countRows, err := local.Query(ctx, db, "SELECT count(*) AS n FROM ("+body+") AS preview", nil)
	if err != nil {
		return nil, 0, false, err
	}

	total := len(items)
	if len(countRows) > 0 {
		n, err := strconv.Atoi(countRows[0]["n"])
		if err != nil {
			return nil, 0, false, fmt.Errorf("read preview count %q: %w", countRows[0]["n"], err)
		}

		total = n
	}

	if total < len(items) {
		total = len(items)
	}

	if total > len(items) {
		truncated = true
	}

	return items, total, truncated, nil
}

func extensionPreviewText(items []string, truncated bool) string {
	if len(items) == 0 {
		return "unknown objects"
	}

	shown := items
	if len(shown) > 3 {
		shown = shown[:3]
	}

	text := strings.Join(shown, "; ")
	if len(items) > len(shown) || truncated {
		text += "; ..."
	}

	return text
}

func extensionCount(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}

	return strconv.Itoa(n) + " " + word + "s"
}

// --- doctor facts ---

func extensionFactsForCluster(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, state string, awaiting bool) doctor.ExtensionFacts {
	x := doctor.ExtensionFacts{Checked: true}
	resolver := cfg.ExtensionResolver()

	enabled, err := extensionFactsEnabled(resolver, spec.Extensions)
	if err != nil {
		x.Available, x.Error = false, err.Error()
		x.PackagesKnown, x.PackageError = false, err.Error()
		x.PrerequisitesKnown, x.MissingPrerequisites = false, err.Error()

		return x
	}

	x.Enabled = enabled
	x.Known = map[string]doctor.ExtensionInfo{}

	known, err := extensionFactsKnown(resolver)
	if err != nil {
		x.Available, x.Error = false, err.Error()
		x.PackagesKnown, x.PackageError = false, err.Error()

		return x
	}

	x.Known = known
	packagesKnown, prerequisitesKnown, missingPackages, packageError, prerequisitesError := extensionFactsPrerequisites(ctx, cfg, spec)
	x.PackagesKnown, x.MissingPackages = packagesKnown, missingPackages
	x.PackageError = packageError

	x.PrerequisitesKnown, x.MissingPrerequisites = prerequisitesKnown, prerequisitesError
	if awaiting || state != "running" {
		x.Available, x.Error = false, "the cluster is "+state
		if awaiting {
			x.Error = "awaiting recovery"
		}

		return x
	}

	installed, err := extensionInstalled(ctx, spec)
	if err != nil {
		x.Available, x.Error = false, err.Error()
		return x
	}

	x.Available = true

	x.Installed = map[string]doctor.ExtensionVersion{}
	for name, got := range installed {
		x.Installed[name] = doctor.ExtensionVersion{Version: got.Version, Default: got.Default}
	}

	loaded, pending, err := extensionPreloadFacts(ctx, spec)
	if err != nil {
		x.LoadedKnown, x.PendingKnown = false, false
	} else {
		x.LoadedKnown, x.PendingKnown = true, true
		x.Loaded, x.PendingRestart = loaded, pending
	}

	for name := range installed {
		if name == "plpgsql" {
			continue
		}

		if _, ok := x.Known[name]; !ok {
			x.Unmanaged = append(x.Unmanaged, name)
		}
	}

	sort.Strings(x.Unmanaged)

	return x
}

func extensionFactsEnabled(resolver config.ExtensionResolver, names []string) ([]doctor.ExtensionInfo, error) {
	effective, err := resolver.EffectiveNames(names)
	if err != nil {
		return nil, err
	}

	out := make([]doctor.ExtensionInfo, 0, len(effective))
	for _, name := range effective {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return nil, err
		}

		out = append(out, doctor.ExtensionInfo{Name: ext.Name, ExtName: ext.ExtName, Preload: ext.Preload})
	}

	return out, nil
}

func extensionFactsKnown(resolver config.ExtensionResolver) (map[string]doctor.ExtensionInfo, error) {
	all, err := resolver.List()
	if err != nil {
		return nil, err
	}

	known := map[string]doctor.ExtensionInfo{}

	for _, ext := range all {
		if ext.ExtName != "" {
			known[ext.ExtName] = doctor.ExtensionInfo{Name: ext.Name, ExtName: ext.ExtName, Preload: ext.Preload}
		}
	}

	return known, nil
}

func extensionFactsPrerequisites(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) (packagesKnown, prerequisitesKnown bool, missingPackages []string, packageError, prerequisitesError string) {
	reqs, err := steps.ExtensionPrerequisitesForSpec(cfg, spec)
	if err != nil {
		return false, false, nil, err.Error(), err.Error()
	}

	packagesKnown, prerequisitesKnown = true, true

	seen := map[string]bool{}
	for _, req := range reqs {
		if req.Source != catalog.SourceAPT || req.Package == "" || seen[req.Package] {
			continue
		}

		seen[req.Package] = true

		installed, known, detail := extensionPackageInstalled(ctx, req.Package)
		if !known {
			packagesKnown, packageError = false, detail
			continue
		}

		if !installed {
			missingPackages = append(missingPackages, req.Package)
		}
	}

	sort.Strings(missingPackages)

	if err := steps.VerifyExtensionRequirements(reqs); err != nil {
		prerequisitesError = err.Error()
	}

	return packagesKnown, prerequisitesKnown, missingPackages, packageError, prerequisitesError
}
