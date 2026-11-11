package node

import (
	"context"
	"slices"
	"sort"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

// Extension requirements for copies and history (Phase 7). The plan is built
// before a copy starts and before a restore point is used. It refuses an
// unsafe or unprovable combination before any slot, ownership, file or start
// effect; it never treats today's intent as proof for an older restore point.

// extensionBackupInfo is the repository fact read for historical coverage. It
// is a variable so tests can model a backup chain without pgBackRest.
var extensionBackupInfo = func(ctx context.Context, spec config.ClusterSpec) (backup.Info, error) {
	return driver(spec, "extension.plan", nil).Info(ctx, "")
}

type extensionCopyRequest struct {
	// Intent is the database whose current configuration the copy will load:
	// the source for a fork or follower, the target otherwise.
	Intent config.ClusterSpec
	// Stanza is the immutable history owner. Empty for a live base backup or
	// a dump.
	Stanza string
	// CopyDB is the database name inside the copy before any rename.
	CopyDB string
	// Options is the restore point for a stanza copy; zero for a base backup.
	Options backup.RestoreOptions
	// Historical says the data comes from a stanza point (or a dump).
	Historical bool
	// IncludeRetained adds the stanza's retained snapshots even when the copy
	// is a live base backup (no historical coverage is claimed, but a library
	// the data still holds must not vanish).
	IncludeRetained bool
	// Dump says the data is a long-retention dump: unbound, so the operator's
	// declaration is the only historical requirement.
	Dump bool
	// Standby says the copy starts as a physical standby, even if it promotes
	// immediately. It requires standby_safe on every requirement.
	Standby bool
	// Declaration is the history owner's recovery_extensions. nil is absent,
	// a pointer to an empty slice is the operator's explicit assertion, and a
	// populated slice is the complete declared requirement.
	Declaration *[]string
}

// preflightRestoreExtensionPlan refuses an unsafe restore point before the
// open-restore record is written. The job builds the same plan again under
// its locks.
func preflightRestoreExtensionPlan(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, p restoreParams) error {
	opts := backup.RestoreOptions{Kind: p.Kind, Set: p.Set}
	if p.Kind == backup.TargetTime {
		opts.Time, _ = time.Parse(time.RFC3339, p.Time)
	}
	stanza := spec.Stanza
	if p.Kind == targetDump {
		stanza = ""
	}
	_, err := buildExtensionCopyPlan(ctx, cfg, extensionCopyRequest{
		Intent: spec, Stanza: stanza, CopyDB: p.Database, Options: opts,
		Historical: p.Kind != targetDump, Dump: p.Kind == targetDump, Standby: p.Kind != targetDump,
		Declaration: spec.RecoveryExtensions,
	})
	return err
}

// extensionCopyPlan is the plan plus the installation prerequisites the
// prestart hook verifies.
type extensionCopyPlan struct {
	Plan   *backup.CopyPlan
	Reqs   []steps.ExtensionPrerequisite
	Merged []catalog.Extension
}

func buildExtensionCopyPlan(ctx context.Context, cfg config.Cluster, req extensionCopyRequest) (*extensionCopyPlan, error) {
	resolver := cfg.ExtensionResolver()
	currentNames, err := resolver.EffectiveNames(req.Intent.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	current, err := resolveExtensions(resolver, currentNames)
	if err != nil {
		return nil, config.ValidationError(err)
	}

	known := append([]catalog.Extension(nil), current...)
	var declared []catalog.Extension
	declaration := "known"
	coverage := ""
	historical := req.Historical || req.Dump

	switch {
	case req.Dump:
		declaration, declared, err = declaredRequirements(resolver, req.Declaration, req.Intent.Name, "the dump")
		if err != nil {
			return nil, err
		}
	case req.Historical || req.IncludeRetained:
		recs, err := backup.LoadRecords()
		if err != nil {
			return nil, err
		}
		hist, _ := recs.ExtensionRequirementsFor(req.Stanza)
		coverage = hist.CoverageSince
		known = append(known, recs.RetainedExtensions(req.Stanza)...)
		if req.Historical {
			root, ok := selectedBackupRoot(ctx, req)
			covered := ok && coverage != "" && coverageAtOrBefore(coverage, root)
			if !covered {
				declaration, declared, err = declaredRequirements(resolver, req.Declaration, req.Intent.Name, "restore point "+req.Stanza)
				if err != nil {
					return nil, err
				}
			}
		}
	}

	merged, guard, err := backup.MergeCopyRequirements(known, declared, req.Standby, req.CopyDB)
	if err != nil {
		return nil, err
	}
	permanent := permanentExtensionSettings(cfg, req.Intent, current)
	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: merged, Current: current, Permanent: permanent,
		Explicit: req.Intent.Settings, CopyDB: req.CopyDB, Pending: true,
	})
	if err != nil {
		return nil, err
	}
	guard.Settings = decision.Settings
	// The copied configuration already carries current intent's preloads. The
	// guard only needs the libraries current intent does not name; the guard
	// composer adds current and operator preloads on every install, so a later
	// rewrite never accumulates obsolete current intent.
	currentPreloads := map[string]bool{}
	var desiredPreloads []string
	for _, ext := range current {
		if ext.Preload != "" {
			currentPreloads[ext.Preload] = true
			desiredPreloads = append(desiredPreloads, ext.Preload)
		}
	}
	sort.Strings(desiredPreloads)
	desiredPreloads = slices.Compact(desiredPreloads)
	extraPreloads := guard.Preloads[:0]
	for _, p := range guard.Preloads {
		if !currentPreloads[p] {
			extraPreloads = append(extraPreloads, p)
		}
	}
	guard.Preloads = extraPreloads
	reqs := make([]steps.ExtensionPrerequisite, 0, len(merged))
	cron := false
	for _, ext := range merged {
		if ext.Name == "pg_cron" || ext.ExtName == "pg_cron" {
			cron = true
		}
		if ext.ExtName == "" && ext.Preload == "" {
			continue
		}
		reqs = append(reqs, steps.ExtensionPrerequisite{
			Name: ext.Name, Source: ext.Source, Package: ext.Package,
			ExtName: ext.ExtName, Preload: ext.Preload,
		})
	}
	return &extensionCopyPlan{
		Plan: &backup.CopyPlan{
			Guard: guard, Requirements: merged, Stanza: req.Stanza,
			CoverageSince: coverage, Historical: historical, Cron: cron, Declaration: declaration,
			DesiredPreloads: desiredPreloads,
			ValidateGuard:   finalPreloadValidator(cfg, req.Intent, req.CopyDB, merged, permanent, req.Standby, nil),
		},
		Reqs: reqs, Merged: merged,
	}, nil
}

func resolveExtensions(resolver config.ExtensionResolver, names []string) ([]catalog.Extension, error) {
	out := make([]catalog.Extension, 0, len(names))
	for _, name := range names {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, ext)
	}
	return out, nil
}

// permanentExtensionSettings is what the cluster's permanent configuration
// supplies: server settings, the current descriptors' resolved ordinary
// settings and, last, the database's explicit settings. An explicit setting
// therefore beats an inherited descriptor default; a mandatory copy
// suppression still wins in the guard while the copy is pending.
func permanentExtensionSettings(cfg config.Cluster, spec config.ClusterSpec, current []catalog.Extension) map[string]string {
	out := map[string]string{}
	for k, v := range cfg.Postgres.Settings {
		out[k] = v
	}
	for _, ext := range current {
		for k, v := range ext.ResolvedSettings(spec.DatabaseName()) {
			out[k] = v
		}
	}
	for k, v := range spec.Settings {
		out[k] = v
	}
	return out
}

// finalPreloadValidator binds every library in the final composed guard to an
// explicit, compatible resolved descriptor before the copy first starts. A
// library the operator's own configuration adds is accepted only when a
// definition supplies its copy/standby safety and its package/control/library
// files verify; an unknown or contradictory library refuses before load.
func finalPreloadValidator(cfg config.Cluster, spec config.ClusterSpec, copyDB string, known []catalog.Extension, permanent map[string]string, standby bool, released map[string]string) func(backup.CopyGuard) error {
	return func(g backup.CopyGuard) error {
		return steps.ValidateCopyGuardPreloads(cfg, spec, copyDB, known, permanent, standby, released, g)
	}
}

// declaredRequirements resolves the operator's declaration. Absence is not an
// assertion that a historical point used nothing; an explicitly empty list is.
func declaredRequirements(resolver config.ExtensionResolver, declaration *[]string, db, what string) (string, []catalog.Extension, error) {
	if declaration == nil {
		return "", nil, output.NewError(output.CodePrecondition,
			what+" of "+db+" has no recorded extension coverage, and clusters."+db+".recovery_extensions is absent",
			output.ExitPrecondition,
			"Declare the complete extension requirement for that point in clusters."+db+".recovery_extensions (use [] only when it truly used none), then retry.").
			WithDetails(map[string]any{"reason": "extension_history_unknown", "database": db})
	}
	if len(*declaration) == 0 {
		return "empty", nil, nil
	}
	names, err := resolver.EffectiveNames(*declaration)
	if err != nil {
		return "", nil, config.ValidationError(err)
	}
	exts, err := resolveExtensions(resolver, names)
	if err != nil {
		return "", nil, config.ValidationError(err)
	}
	return "populated", exts, nil
}

func selectedBackupRoot(ctx context.Context, req extensionCopyRequest) (time.Time, bool) {
	if req.Stanza == "" {
		return time.Time{}, false
	}
	info, err := extensionBackupInfo(ctx, req.Intent)
	if err != nil {
		return time.Time{}, false
	}
	return backup.SelectedBackupStart(info, req.Options)
}

func coverageAtOrBefore(coverage string, root time.Time) bool {
	t, err := time.Parse(time.RFC3339, coverage)
	if err != nil {
		return false
	}
	return !root.Before(t)
}

// captureExtensionIntent records every stanza's current requirements before an
// intent change, a backup, a drop or a copy. A stanza is the binding; a
// follower has none and its data belongs to its source's stanza.
func captureExtensionIntent(cfg config.Cluster, at time.Time) error {
	seen := map[string]bool{}
	add := func(spec config.ClusterSpec) error {
		if spec.IsFollower() || spec.Stanza == "" || seen[spec.Stanza] {
			return nil
		}
		seen[spec.Stanza] = true
		resolver := cfg.ExtensionResolver()
		names, err := resolver.EffectiveNames(spec.Extensions)
		if err != nil {
			return err
		}
		exts, err := resolveExtensions(resolver, names)
		if err != nil {
			return err
		}
		if spec.RecoveryExtensions != nil {
			declared, err := resolver.EffectiveNames(*spec.RecoveryExtensions)
			if err != nil {
				return err
			}
			more, err := resolveExtensions(resolver, declared)
			if err != nil {
				return err
			}
			exts = append(exts, more...)
		}
		return backup.RecordExtensionRequirements(spec.Stanza, spec.Name, exts, at)
	}
	for _, spec := range cfg.Clusters {
		if err := add(spec); err != nil {
			return err
		}
	}
	for _, spec := range cfg.Awaiting {
		if err := add(spec); err != nil {
			return err
		}
	}
	return nil
}

// captureExtensionChange records the outgoing and incoming requirements before
// a configuration save. The outgoing descriptors are resolved through the old
// configuration, so a removed definition is still captured.
func captureExtensionChange(old, updated config.Cluster) error {
	now := time.Now().UTC()
	if err := captureExtensionIntent(old, now); err != nil {
		return err
	}
	return captureExtensionIntent(updated, now)
}

// captureStanzaRequirements is the producer-side capture: before a backup,
// first full or dump is taken, the stanza's current requirements are recorded.
func captureStanzaRequirements(cfg config.Cluster, spec config.ClusterSpec) error {
	return steps.CaptureStanzaRequirements(cfg, spec)
}

// recordCopyRequirements records the union a copy actually used against its
// own stanza, starting observed coverage now. A declaration is an assertion
// about an older point; recording its snapshots does not backdate coverage.
func recordCopyRequirements(stanza, db string, plan *backup.CopyPlan) error {
	if stanza == "" || plan == nil || len(plan.Requirements) == 0 {
		return nil
	}
	return backup.RecordExtensionRequirements(stanza, db, plan.Requirements, time.Now().UTC())
}
