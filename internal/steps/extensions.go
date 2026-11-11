package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/pg"
)

// extensionSQLDeferral is one extension whose dependent SQL waits for an
// effective prerequisite. RestartPending says the fix is a confirmed restart;
// otherwise the required setting value is simply not effective yet.
type extensionSQLDeferral struct {
	Name           string
	Detail         string
	RestartPending bool
}

// ExtensionPrerequisite is the installation/file part of one resolved
// extension requirement. E705's retained descriptor snapshots can be reduced
// to this shape for package installation and prestart verification; settings
// and copy/standby safety stay in the caller's own planning.
type ExtensionPrerequisite struct {
	Name    string `json:"name"`
	Source  string `json:"source"`
	Package string `json:"package,omitempty"`
	ExtName string `json:"ext_name,omitempty"`
	Preload string `json:"preload,omitempty"`
}

// CaptureStanzaRequirements records spec's current resolved requirements
// against its immutable stanza before a backup, first full or dump is taken.
// A failure stops the producer; today's intent is never assumed to cover an
// older point.
func CaptureStanzaRequirements(cfg config.Cluster, spec config.ClusterSpec) error {
	if spec.IsFollower() || spec.Stanza == "" {
		return nil
	}
	resolver := cfg.ExtensionResolver()
	names, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return err
	}
	exts := make([]catalog.Extension, 0, len(names))
	for _, name := range names {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return err
		}
		exts = append(exts, ext)
	}
	return backup.RecordExtensionRequirements(spec.Stanza, spec.Name, exts, time.Now().UTC())
}

// ExtensionPrerequisites resolves the effective extension selections in cfg,
// including implicit defaults and present recovery declarations, plus the
// databases the engine view leaves awaiting recovery. Preinstalled entries
// carry no package but are still returned so their files can be verified.
func ExtensionPrerequisites(cfg config.Cluster) ([]ExtensionPrerequisite, error) {
	resolver := cfg.ExtensionResolver()
	seen := map[ExtensionPrerequisite]bool{}
	var out []ExtensionPrerequisite
	add := func(spec config.ClusterSpec) error {
		selections := [][]string{spec.Extensions}
		if spec.RecoveryExtensions != nil {
			selections = append(selections, *spec.RecoveryExtensions)
		}
		for _, declared := range selections {
			effective, err := resolver.EffectiveNames(declared)
			if err != nil {
				return err
			}
			for _, name := range effective {
				ext, err := resolver.Resolve(name)
				if err != nil {
					return err
				}
				req := ExtensionPrerequisite{
					Name: ext.Name, Source: ext.Source, Package: ext.Package,
					ExtName: ext.ExtName, Preload: ext.Preload,
				}
				if req.ExtName == "" && req.Preload == "" {
					continue
				}
				if seen[req] {
					continue
				}
				seen[req] = true
				out = append(out, req)
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
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.ExtName != b.ExtName {
			return a.ExtName < b.ExtName
		}
		if a.Preload != b.Preload {
			return a.Preload < b.Preload
		}
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		return a.Source < b.Source
	})
	return out, nil
}

// ExtensionPackages is the sorted, deduplicated APT package union required by
// cfg's effective extension selections. Preinstalled entries contribute none.
func ExtensionPackages(cfg config.Cluster) ([]string, error) {
	reqs, err := ExtensionPrerequisites(cfg)
	if err != nil {
		return nil, err
	}
	return extensionPackagesFrom(reqs), nil
}

func extensionPackagesFrom(reqs []ExtensionPrerequisite) []string {
	seen := map[string]bool{}
	for _, req := range reqs {
		if req.Source == catalog.SourceAPT && req.Package != "" {
			seen[req.Package] = true
		}
	}
	out := make([]string, 0, len(seen))
	for pkg := range seen {
		out = append(out, pkg)
	}
	sort.Strings(out)
	return out
}

// ExtensionPrerequisitesForSpec is the current runtime requirement projection
// of one database, without recovery declarations or other clusters. Guards use
// it so a target start never installs the whole host's union.
func ExtensionPrerequisitesForSpec(cfg config.Cluster, spec config.ClusterSpec) ([]ExtensionPrerequisite, error) {
	return extensionRuntimeRequirementsForSpec(cfg, spec)
}

// extensionRuntimeRequirementsForSpec resolves every effective name that has a
// SQL control file or a preload library. It includes the implicit default
// modules, so a preload-only default such as auto_explain is installed/held
// and its library verified even when no database names it. SQL creation is a
// separate projection (extensionSQLSelection): auto_explain has no
// CREATE EXTENSION name and never appears there.
func extensionRuntimeRequirementsForSpec(cfg config.Cluster, spec config.ClusterSpec) ([]ExtensionPrerequisite, error) {
	resolver := cfg.ExtensionResolver()
	effective, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return nil, err
	}
	out := make([]ExtensionPrerequisite, 0, len(effective))
	for _, name := range effective {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return nil, err
		}
		if ext.ExtName == "" && ext.Preload == "" {
			continue
		}
		out = append(out, ExtensionPrerequisite{
			Name: ext.Name, Source: ext.Source, Package: ext.Package,
			ExtName: ext.ExtName, Preload: ext.Preload,
		})
	}
	return out, nil
}

// extensionSQLSelection resolves spec's effective selection to the
// descriptors that create SQL. Preload-only modules are not SQL extensions and
// are filtered here; runtime prerequisites still include them.
func extensionSQLSelection(cfg config.Cluster, spec config.ClusterSpec) ([]catalog.Extension, error) {
	resolver := cfg.ExtensionResolver()
	effective, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Extension, 0, len(effective))
	for _, name := range effective {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return nil, err
		}
		if ext.ExtName == "" {
			continue
		}
		out = append(out, ext)
	}
	return out, nil
}

// extensionControlDir and extensionLibDir are PostgreSQL 18's Debian layout.
// They are variables so tests can use a temporary tree without touching the
// host.
var (
	extensionControlDir = "/usr/share/postgresql/18/extension"
	extensionLibDir     = "/usr/lib/postgresql/18/lib"
	extensionStat       = os.Stat
)

// VerifyExtensionRequirements fails when a resolved extension's control file
// or declared preload library is absent. Package-name inference is never used:
// the actual PG18 files are checked.
func VerifyExtensionRequirements(reqs []ExtensionPrerequisite) error {
	if missing := missingExtensionFiles(reqs); len(missing) > 0 {
		return errors.New(strings.Join(missing, "; "))
	}
	return nil
}

// missingExtensionFiles lists absent control files, control-file module
// libraries and declared preload libraries, one path each, with an actionable
// source/package diagnosis. Unreadable or unsupported control-file facts are
// explicit errors, not silently "no library".
func missingExtensionFiles(reqs []ExtensionPrerequisite) []string {
	seen := map[string]bool{}
	var out []string
	add := func(req ExtensionPrerequisite, kind, path string) {
		if seen[path] {
			return
		}
		seen[path] = true
		if _, err := extensionStat(path); err == nil {
			return
		}
		out = append(out, extensionFileError(req, kind, path))
	}
	addError := func(req ExtensionPrerequisite, format string, args ...any) {
		out = append(out, fmt.Sprintf("extension %q: %s", extensionDisplayName(req), fmt.Sprintf(format, args...)))
	}
	for _, req := range reqs {
		if req.ExtName != "" {
			control := filepath.Join(extensionControlDir, req.ExtName+".control")
			add(req, "control file", control)
			module, found, err := extensionControlModule(control)
			switch {
			case err != nil:
				addError(req, "control file %s: %v", control, err)
			case found:
				add(req, "library", module)
			}
		}
		if req.Preload != "" {
			lib, err := extensionLibraryPath(req.Preload)
			if err != nil {
				addError(req, "preload library %q: %v", req.Preload, err)
			} else {
				add(req, "preload library", lib)
			}
		}
	}
	sort.Strings(out)
	return out
}

func extensionDisplayName(req ExtensionPrerequisite) string {
	if req.Name != "" {
		return req.Name
	}
	return req.ExtName
}

func extensionFileError(req ExtensionPrerequisite, kind, path string) string {
	name := extensionDisplayName(req)
	if req.Source == catalog.SourcePreinstalled {
		return fmt.Sprintf("preinstalled extension %q is missing its %s %s; install it or change [extensions.%s]", name, kind, path, name)
	}
	return fmt.Sprintf("extension %q from package %q is missing its %s %s; verify the PostgreSQL 18 package is installed", name, req.Package, kind, path)
}

// extensionLibraryPath resolves a PostgreSQL module/library name to the PG18
// file. It accepts bare names, names already ending in .so, $libdir/ paths and
// absolute paths, and never appends a second .so.
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

// extensionControlModule reads a PG18 control file's module_pathname. found
// is false for a SQL-only extension. Unreadable files and unsupported module
// forms are errors, never silently treated as "no library".
func extensionControlModule(path string) (module string, found bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed PostgreSQL 18 extension directory.
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cannot read control file: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			if strings.HasPrefix(line, "module_pathname") {
				return "", true, errors.New("malformed module_pathname")
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
			return "", true, errors.New("empty module_pathname")
		}
		resolved, err := extensionLibraryPath(value)
		if err != nil {
			return "", true, fmt.Errorf("unsupported module_pathname %q: %w", value, err)
		}
		return resolved, true, nil
	}
	return "", false, nil
}

// runExtensionPackageScript is the injectable shell seam for the package
// script. It is not a production fault hook: tests replace it to inspect the
// environment and model command failures.
var runExtensionPackageScript = func(ctx context.Context, stepID string, env map[string]string, rt engine.Runtime, report engine.Reporter) error {
	return rt.Shell.Run(ctx, stepID, Script("postgres-packages"), env, report)
}

// ValidateCopyGuardPreloads binds every library in a final composed copy
// guard to an explicit, compatible resolved descriptor. A library the
// operator's own configuration adds is accepted only when a definition
// supplies its copy/standby safety and its package/control/library files
// verify; an unknown or contradictory library refuses before the copy loads.
func ValidateCopyGuardPreloads(cfg config.Cluster, spec config.ClusterSpec, copyDB string, known []catalog.Extension, permanent map[string]string, standby bool, released map[string]string, g backup.CopyGuard) error {
	return validateCopyGuardPreloads(cfg, spec, copyDB, known, nil, permanent, standby, released, g)
}

// ValidateCopyGuardPreloadsWithTransition is ValidateCopyGuardPreloads with
// outgoing removal descriptors. Their libraries stay loaded while their SQL is
// still present, but they are already installed and being retired: a fresh
// copy-safety declaration is not required for them. A current or retained
// descriptor for the same library still wins and is validated normally.
func ValidateCopyGuardPreloadsWithTransition(cfg config.Cluster, spec config.ClusterSpec, copyDB string, known, transition []catalog.Extension, permanent map[string]string, standby bool, released map[string]string, g backup.CopyGuard) error {
	return validateCopyGuardPreloads(cfg, spec, copyDB, known, transition, permanent, standby, released, g)
}

func validateCopyGuardPreloads(cfg config.Cluster, spec config.ClusterSpec, copyDB string, known, transition []catalog.Extension, permanent map[string]string, standby bool, released map[string]string, g backup.CopyGuard) error {
	if len(g.Preloads) == 0 {
		return nil
	}
	defs := map[string][]catalog.Extension{}
	add := func(ext catalog.Extension) {
		if ext.Preload == "" {
			return
		}
		defs[ext.Preload] = append(defs[ext.Preload], ext.Clone())
	}
	for _, ext := range known {
		add(ext)
	}
	list, err := cfg.ExtensionResolver().List()
	if err != nil {
		return config.ValidationError(err)
	}
	for _, ext := range list {
		add(ext)
	}
	transitionNames := map[string]bool{}
	for _, ext := range transition {
		transitionNames[ext.Name] = true
	}
	var reqs []ExtensionPrerequisite
	for _, library := range g.Preloads {
		allMatches := compatiblePreloadDefinitions(defs[library])
		if len(allMatches) == 0 {
			return output.NewError(output.CodePrecondition,
				"the copy's final preload union contains "+library+", which no extension definition explains",
				output.ExitPrecondition,
				"Declare it in [extensions.<name>] with copy_safety and standby_safe, or remove it from the copied configuration; the copy was not started.").
				WithDetails(map[string]any{"reason": "extension_preload_unknown", "library": library, "database": spec.Name})
		}
		var matches []catalog.Extension
		for _, ext := range allMatches {
			if !transitionNames[ext.Name] {
				matches = append(matches, ext)
			}
		}
		transitionOnly := false
		if len(matches) == 0 {
			matches, transitionOnly = allMatches, true
		}
		base := matches[0]
		if len(matches) > 1 {
			return output.NewError(output.CodePrecondition,
				"the copy's preload library "+library+" matches definitions with contradictory safety metadata: "+preloadDefinitionNames(matches),
				output.ExitPrecondition,
				"Keep one compatible definition for that library; the copy was not started.").
				WithDetails(map[string]any{"reason": "extension_preload_ambiguous", "library": library, "database": spec.Name})
		}
		if !transitionOnly {
			switch base.CopySafety {
			case catalog.CopySafetySafe:
			case catalog.CopySafetyDisabled:
				for k, v := range base.ResolvedCopySettings(copyDB) {
					want := catalog.NormalizedSettingValue(v)
					if prev, ok := released[k]; ok && prev == want {
						// This key was released only after the permanent
						// configuration was proven to supply the same value. The
						// validator must not recreate the guard suppression.
						continue
					}
					got, ok := g.Settings[k]
					if !ok {
						got, ok = permanent[k]
					}
					if !ok || catalog.NormalizedSettingValue(got) != want {
						return output.NewError(output.CodePrecondition,
							"the copy's library "+library+" requires "+k+" = "+v+" but the final guard does not enforce it",
							output.ExitPrecondition,
							"Keep the extension's copy_settings in the final guard, or declare it copy_safe; the copy was not started.").
							WithDetails(map[string]any{"reason": "extension_preload_unsafe", "library": library, "setting": k, "database": spec.Name})
					}
				}
			default:
				return output.NewError(output.CodePrecondition,
					"the copy's library "+library+" comes from "+base.Name+" without a copy safety declaration",
					output.ExitPrecondition,
					"Declare copy_safety safe or disabled in its definition; the copy was not started.").
					WithDetails(map[string]any{"reason": "extension_preload_unsafe", "library": library, "database": spec.Name})
			}
			if standby && !base.StandbySafe {
				return output.NewError(output.CodePrecondition,
					"the copy's library "+library+" comes from "+base.Name+", which is not declared standby_safe",
					output.ExitPrecondition,
					"Declare standby_safe only when the native code is safe on a standby; the copy was not started.").
					WithDetails(map[string]any{"reason": "extension_preload_standby_unsafe", "library": library, "database": spec.Name})
			}
		}
		reqs = append(reqs, ExtensionPrerequisite{
			Name: base.Name, Source: base.Source, Package: base.Package,
			ExtName: base.ExtName, Preload: base.Preload,
		})
	}
	if err := VerifyExtensionRequirements(reqs); err != nil {
		return output.NewError(output.CodePrecondition,
			"the copy's final preload union is missing a package or library: "+err.Error(),
			output.ExitPrecondition,
			"Install the required PostgreSQL 18 package or file; the copy was not started.").
			WithDetails(map[string]any{"reason": "extension_preload_missing", "database": spec.Name})
	}
	return nil
}

func compatiblePreloadDefinitions(defs []catalog.Extension) []catalog.Extension {
	var out []catalog.Extension
	for _, ext := range defs {
		duplicate := false
		for _, have := range out {
			if samePreloadMetadata(have, ext) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, ext)
		}
	}
	return out
}

func samePreloadMetadata(a, b catalog.Extension) bool {
	if a.ExtName != b.ExtName || a.Source != b.Source || a.Package != b.Package ||
		a.Preload != b.Preload || a.CopySafety != b.CopySafety || a.StandbySafe != b.StandbySafe {
		return false
	}
	if len(a.CopySettings) != len(b.CopySettings) {
		return false
	}
	for k, v := range a.CopySettings {
		if catalog.NormalizedSettingValue(b.CopySettings[k]) != catalog.NormalizedSettingValue(v) {
			return false
		}
	}
	return true
}

func preloadDefinitionNames(defs []catalog.Extension) string {
	names := make([]string, 0, len(defs))
	for _, ext := range defs {
		names = append(names, ext.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ReconcileCopyGuard refreshes a non-follower's copy guard after the permanent
// configuration is rendered. The guard always carries the complete union:
// retained requirements, current desired preloads and the operator's own
// preloads. It is removed only when the permanent configuration supplies every
// value. A malformed existing guard is an error, never silently discarded.
func ReconcileCopyGuard(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	dataDir := cfg.Server.DataDirectory(spec.Name)
	if spec.IsFollower() {
		// Guard refresh happens after the preflight has proven the target. If
		// the catalog becomes unreadable between that proof and this write,
		// use the identity-bound complete preflight plan so no SQL-only or
		// unguarded mandatory obligation is lost. Without that plan, stop
		// before any guard write, reload, start or restart.
		id, err := FollowerRuntimeIdentity(cfg, spec, FollowerRuntimeOperation(ctx))
		if err != nil {
			return err
		}
		snapshot, ok := FollowerRuntimePlanFor(ctx, id)
		req := FollowerRuntimeRequest{
			Class: FollowerRuntimeLive, Outgoing: FollowerRuntimeOutgoing(ctx),
			RetireAbsent: true, AllowUnknownSQL: true,
		}
		if ok {
			req.Snapshot = &snapshot
		}
		plan, err := PlanFollowerRuntime(ctx, cfg, spec, req)
		if err != nil {
			return err
		}
		_, _, err = backup.ReadCopyGuard(dataDir)
		if err != nil {
			return err
		}
		if err := ValidateCopyGuardPreloadsWithTransition(cfg, spec, spec.DatabaseName(), plan.Requirements, plan.Outgoing, plan.Permanent, true, nil, plan.Guard); err != nil {
			return err
		}
		// The plan is the complete winning union. It keeps every retained,
		// outgoing or operator obligation; when the actual catalog proves the
		// last removed SQL absent, an empty plan deliberately removes the
		// obsolete guard block rather than pinning it forever.
		_, err = backup.InstallCopyGuard(dataDir, plan.Guard)
		return err
	}
	_, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		return err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	resolver := cfg.ExtensionResolver()
	currentNames, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return config.ValidationError(err)
	}
	current := make([]catalog.Extension, 0, len(currentNames))
	for _, name := range currentNames {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return config.ValidationError(err)
		}
		current = append(current, ext)
	}
	desired := map[string]bool{}
	for _, ext := range current {
		desired[ext.Name] = true
	}
	var retained []catalog.Extension
	for _, ext := range recs.RetainedExtensions(spec.Stanza) {
		if !desired[ext.Name] {
			retained = append(retained, ext)
		}
	}
	_, base, err := backup.MergeCopyRequirements(retained, nil, false, spec.DatabaseName())
	if err != nil {
		return err
	}
	permanent := map[string]string{}
	for k, v := range cfg.Postgres.Settings {
		permanent[k] = v
	}
	for _, ext := range current {
		for k, v := range ext.ResolvedSettings(spec.DatabaseName()) {
			permanent[k] = v
		}
	}
	for k, v := range spec.Settings {
		permanent[k] = v
	}
	known := append(append([]catalog.Extension(nil), current...), retained...)
	fenceActive := false
	if f, ok := recs.ActivationFenceFor(spec.Name); ok && f.Active() {
		fenceActive = true
	}
	actualPermanent, err := backup.EffectivePermanentSettings(PGConfDir(spec.Name), dataDir)
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"the permanent configuration's effective precedence for "+spec.Name+" cannot be proven: "+err.Error(),
			output.ExitPrecondition,
			"Fix the cluster's configuration files; vaire will not release a copy suppression without proof.").
			WithDetails(map[string]any{"reason": "extension_permanent_unproven", "database": spec.Name})
	}
	instance, err := guardInstance()
	if err != nil || strings.TrimSpace(instance) == "" {
		return output.NewError(output.CodePrecondition,
			"the node instance identity is missing, so "+spec.Name+"'s guard handoff cannot be scoped",
			output.ExitPrecondition, "Repair the node's instance file deliberately; vaire will not inherit unscoped handoff authority.").
			WithDetails(map[string]any{"reason": "guard_instance_unknown", "database": spec.Name})
	}
	instance = strings.TrimSpace(instance)
	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: known, Current: current, Permanent: permanent,
		Explicit: spec.Settings, CopyDB: spec.DatabaseName(),
		Pending: fenceActive, Handoff: recs.GuardHandoffFor(spec.Name, instance, spec.Stanza),
		HandoffPermanent: actualPermanent,
	})
	if err != nil {
		return err
	}
	base.Settings = decision.Settings
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.SetGuardHandoff(spec.Name, instance, spec.Stanza, decision.Handoff)
		return nil
	}); err != nil {
		return err
	}
	desiredPreloads, err := resolver.Preloads(spec.Extensions)
	if err != nil {
		return config.ValidationError(err)
	}
	plan := &backup.CopyPlan{
		Guard: base, DesiredPreloads: desiredPreloads,
		ValidateGuard: func(g backup.CopyGuard) error {
			return ValidateCopyGuardPreloads(cfg, spec, spec.DatabaseName(), known, permanent, false, decision.Handoff, g)
		},
	}
	composed, _, err := backup.InstallCopyPlanGuard(PGConfDir(spec.Name), dataDir, plan)
	if err != nil {
		return err
	}
	confPreloads, err := backup.EffectiveConfPreloads(PGConfDir(spec.Name))
	if err != nil {
		return err
	}
	permanentPreloads := append([]string(nil), confPreloads...)
	if backup.CopyGuardCovered(composed, permanentPreloads, actualPermanent) {
		_, err = backup.RemoveCopyGuard(dataDir)
		return err
	}
	_, err = backup.InstallCopyGuard(dataDir, composed)
	return err
}

// ValidateExtensionDescriptorSafety checks ordinary descriptor/provider and
// setting compatibility without requiring copy or standby safety. A standalone
// primary may legitimately use a custom descriptor whose copy safety is still
// unknown; the copy/follower gate applies only when data will actually be
// loaded as a copy.
func ValidateExtensionDescriptorSafety(cfg config.Cluster, spec config.ClusterSpec, exts []catalog.Extension) error {
	merged, err := backup.MergeExtensionRequirements(exts, nil)
	if err != nil {
		return err
	}
	// Validate ordinary settings only. Copy settings belong to the copy/standby
	// gate and must not conflict with a standalone primary's ordinary intent.
	ordinary := make([]catalog.Extension, 0, len(merged))
	for _, ext := range merged {
		ext = ext.Clone()
		ext.CopySafety = catalog.CopySafetySafe
		ext.CopySettings = nil
		ordinary = append(ordinary, ext)
	}
	_, err = backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: ordinary, Current: ordinary, CopyDB: spec.DatabaseName(), Pending: true,
	})
	return err
}

// ValidateExtensionMetadataSafety checks every selected descriptor's runtime
// safety metadata without reading files or changing anything: copy_safety must
// be known, disabled copy settings must be compatible, standby_safe must be
// explicit for a physical copy, and providers/ordinary settings must agree.
func ValidateExtensionMetadataSafety(cfg config.Cluster, spec config.ClusterSpec, exts []catalog.Extension, standby bool) error {
	if _, _, err := backup.MergeCopyRequirements(exts, nil, standby, spec.DatabaseName()); err != nil {
		return err
	}
	_, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: exts, Current: exts, CopyDB: spec.DatabaseName(), Pending: true,
	})
	return err
}

// ResolveExtensionSelection resolves spec's effective current descriptors.
func ResolveExtensionSelection(cfg config.Cluster, spec config.ClusterSpec) ([]catalog.Extension, error) {
	resolver := cfg.ExtensionResolver()
	names, err := resolver.EffectiveNames(spec.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}
	out := make([]catalog.Extension, 0, len(names))
	for _, name := range names {
		ext, err := resolver.Resolve(name)
		if err != nil {
			return nil, config.ValidationError(err)
		}
		out = append(out, ext)
	}
	return out, nil
}

// FollowerRuntimeClass says how much live evidence the caller can supply for a
// follower runtime plan. It is explicit: an unavailable catalog is never
// treated as an empty one, and a non-materialized target never pretends its
// SQL set was queried.
type FollowerRuntimeClass string

const (
	// FollowerRuntimeLive means the follower's data is materialized and its
	// actual pg_extension catalog is authoritative. The planner queries it and
	// refuses if it cannot, unless the caller explicitly allows an unknown
	// result for a post-effect guard refresh.
	FollowerRuntimeLive FollowerRuntimeClass = "live"
	// FollowerRuntimeAwaiting means the target has no materialized data yet.
	// Only metadata can be validated; no SQL absence is inferred.
	FollowerRuntimeAwaiting FollowerRuntimeClass = "awaiting"
)

// FollowerRuntimeRequest is the explicit evidence and transition context for
// one follower runtime plan. Outgoing descriptors are the pre-change
// descriptors a removal still needs to explain existing guard entries; they
// are never current intent. RetireAbsent permits retiring a non-current SQL
// descriptor only after the actual follower catalog proves its SQL absent.
// AllowUnknownSQL keeps every existing guard obligation when the catalog cannot
// be read after primary effects; it never claims a complete plan.
type FollowerRuntimeRequest struct {
	Class           FollowerRuntimeClass
	Outgoing        []catalog.Extension
	RetireAbsent    bool
	AllowUnknownSQL bool
	// Snapshot is the complete immutable preflight plan for this exact
	// identity. It is used only when fresh SQL facts are unavailable; a
	// successful catalog read always supersedes it.
	Snapshot *FollowerRuntimePlan
}

// FollowerRuntimePlanIdentity binds a preflight runtime plan to the actual
// node, operation, source/follower stanza, follow generation, port and
// candidate intent. It is deliberately comparable so plans can live in a
// context map under held target locks.
type FollowerRuntimePlanIdentity struct {
	Instance         string
	OperationID      string
	Source           string
	Follower         string
	Stanza           string
	FollowGeneration string
	Candidate        string
	Port             int
}

type followerRuntimePlansKey struct{}
type followerRuntimeOperationKey struct{}

// WithFollowerRuntimeOperation carries the current operation ID into the
// post-effect guard/verification boundary.
func WithFollowerRuntimeOperation(ctx context.Context, operationID string) context.Context {
	return context.WithValue(ctx, followerRuntimeOperationKey{}, operationID)
}

// FollowerRuntimeOperation returns the current operation ID.
func FollowerRuntimeOperation(ctx context.Context) string {
	operationID, _ := ctx.Value(followerRuntimeOperationKey{}).(string)
	return operationID
}

// WithFollowerRuntimePlans attaches complete preflight runtime plans. The map
// is deep-copied so a later caller cannot mutate a plan another stage holds.
func WithFollowerRuntimePlans(ctx context.Context, plans map[FollowerRuntimePlanIdentity]FollowerRuntimePlan) context.Context {
	if len(plans) == 0 {
		return ctx
	}
	out := make(map[FollowerRuntimePlanIdentity]FollowerRuntimePlan, len(plans))
	for id, plan := range plans {
		out[id] = cloneFollowerRuntimePlan(plan)
	}
	return context.WithValue(ctx, followerRuntimePlansKey{}, out)
}

// FollowerRuntimePlanFor returns the identity-bound preflight plan, if any.
func FollowerRuntimePlanFor(ctx context.Context, id FollowerRuntimePlanIdentity) (FollowerRuntimePlan, bool) {
	plans, _ := ctx.Value(followerRuntimePlansKey{}).(map[FollowerRuntimePlanIdentity]FollowerRuntimePlan)
	plan, ok := plans[id]
	if !ok {
		return FollowerRuntimePlan{}, false
	}
	return cloneFollowerRuntimePlan(plan), true
}

// FollowerRuntimeIdentity builds the comparable identity for one follower
// runtime. The instance is the actual installation identity, never host text.
func FollowerRuntimeIdentity(cfg config.Cluster, spec config.ClusterSpec, operationID string) (FollowerRuntimePlanIdentity, error) {
	if !spec.IsFollower() {
		return FollowerRuntimePlanIdentity{}, fmt.Errorf("%s is not a follower", spec.Name)
	}
	source, ok := cfg.ClusterByName(spec.Follows)
	if !ok {
		return FollowerRuntimePlanIdentity{}, fmt.Errorf("%s follows %s, which is not in cluster.toml", spec.Name, spec.Follows)
	}
	instance, err := guardInstance()
	if err != nil || strings.TrimSpace(instance) == "" {
		if err == nil {
			err = errors.New("empty instance identity")
		}
		return FollowerRuntimePlanIdentity{}, fmt.Errorf("the node instance identity is missing, so %s's runtime plan cannot be scoped: %w", spec.Name, err)
	}
	candidate, err := json.Marshal(struct {
		PostgresSettings   map[string]string
		SourceSettings     map[string]string
		SourceExtensions   []string
		SourceRecovery     *[]string
		FollowerSettings   map[string]string
		FollowerExtensions []string
		Definitions        map[string]config.ExtensionDefinition
	}{
		PostgresSettings:   cfg.Postgres.Settings,
		SourceSettings:     source.Settings,
		SourceExtensions:   source.Extensions,
		SourceRecovery:     source.RecoveryExtensions,
		FollowerSettings:   spec.Settings,
		FollowerExtensions: spec.Extensions,
		Definitions:        cfg.Extensions,
	})
	if err != nil {
		return FollowerRuntimePlanIdentity{}, err
	}
	sum := sha256.Sum256(candidate)
	return FollowerRuntimePlanIdentity{
		Instance: strings.TrimSpace(instance), OperationID: operationID,
		Source: source.Name, Follower: spec.Name, Stanza: source.Stanza,
		FollowGeneration: spec.FollowGeneration, Candidate: hex.EncodeToString(sum[:]), Port: spec.Port,
	}, nil
}

// FollowerRuntimePlan is the complete runtime expectation for one follower:
// the current inherited descriptors, actually retained data requirements and
// the winning guard. It is read-only and shared by metadata preflight, guard
// refresh and effective verification.
type FollowerRuntimePlan struct {
	Guard        backup.CopyGuard
	Requirements []catalog.Extension
	Preloads     []string
	// Settings are the guard's own assignments. They are not the complete
	// expected runtime; ExpectedSettings is.
	Settings map[string]string
	// ExpectedSettings is the complete ordinary-plus-mandatory-copy overlay the
	// follower must have effective, as decoded runtime values (descriptor
	// literals decoded once, raw global/spec/follower overrides unchanged).
	// Verification checks every key before any primary configuration or SQL
	// runs. The serialized Guard.Settings map remains config literals.
	ExpectedSettings map[string]string
	Cron             bool
	Retained         []catalog.Extension
	// Outgoing are the pre-change descriptors still kept as transition
	// evidence because their SQL is present (or not yet proven absent). They
	// are not current intent and do not need a fresh copy-safety declaration
	// while a removal is in progress.
	Outgoing []catalog.Extension
	// Permanent is the actual underlying permanent configuration used to
	// decide which mandatory settings are already supplied.
	Permanent map[string]string
	// SQLFactsComplete says the actual installed SQL catalog was read. A false
	// value is an explicit incomplete fact set, never "no extensions".
	SQLFactsComplete bool
	// PendingSQL lists non-current SQL extensions still present in the actual
	// follower catalog. A removal is pending until they disappear by replay.
	PendingSQL []string
}

type followerRuntimeContextKey struct{}

// WithFollowerRuntimeOutgoing carries the removal's outgoing descriptor through
// the configuration/guard execution path. It is transition evidence, not
// current intent: only those descriptors may be reported as pending removal.
func WithFollowerRuntimeOutgoing(ctx context.Context, outgoing []catalog.Extension) context.Context {
	if len(outgoing) == 0 {
		return ctx
	}
	return context.WithValue(ctx, followerRuntimeContextKey{}, append([]catalog.Extension(nil), outgoing...))
}

// FollowerRuntimeOutgoing returns the removal's outgoing transition evidence.
func FollowerRuntimeOutgoing(ctx context.Context) []catalog.Extension {
	outgoing, _ := ctx.Value(followerRuntimeContextKey{}).([]catalog.Extension)
	return outgoing
}

// FollowerRuntimePendingError says a follower's safe staging completed but its
// runtime is not yet fully effective: a restart or WAL replay is still
// outstanding. Callers report it as truthful pending work, never as success.
type FollowerRuntimePendingError struct {
	Database string
	Reason   string
	Detail   string
}

func (e *FollowerRuntimePendingError) Error() string {
	message := e.Database + " is not fully effective yet"
	if e.Detail != "" {
		message += ": " + e.Detail
	}
	return message
}

// followerInstalledExtensions reads the follower's actual installed SQL
// extensions. A stopped or unreadable follower returns an error, never an
// empty fact set.
var followerInstalledExtensions = func(ctx context.Context, spec config.ClusterSpec) (map[string]bool, error) {
	rows, err := ClusterPG(spec).Query(ctx, spec.DatabaseName(), "SELECT extname AS name FROM pg_extension ORDER BY 1", nil)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, row := range rows {
		if name := row["name"]; name != "" {
			out[name] = true
		}
	}
	return out, nil
}

// SetFollowerInstalledExtensionsForTest replaces the live catalog reader for a
// test in another package and returns the restore function.
func SetFollowerInstalledExtensionsForTest(f func(context.Context, config.ClusterSpec) (map[string]bool, error)) func() {
	old := followerInstalledExtensions
	followerInstalledExtensions = f
	return func() { followerInstalledExtensions = old }
}

// PlanFollowerRuntime resolves one continuously following runtime without
// feeding every historical stanza variant into it. It collects current intent,
// outgoing transition evidence, actual installed SQL and existing guard
// obligations first, then derives one complete runtime union. Current
// inherited descriptors win their previous ordinary runtime variants; only
// genuine retained extras keep strict historical ambiguity checks. Unknown or
// unreadable required facts refuse before any effect.
func PlanFollowerRuntime(ctx context.Context, cfg config.Cluster, follower config.ClusterSpec, req FollowerRuntimeRequest) (FollowerRuntimePlan, error) {
	if !follower.IsFollower() {
		return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
			follower.Name+" is not a follower, so it has no inherited runtime plan",
			output.ExitPrecondition, "Use the ordinary database configuration path.")
	}
	source, ok := cfg.ClusterByName(follower.Follows)
	if !ok {
		return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
			follower.Follows+" is not in cluster.toml, so "+follower.Name+" has no inherited runtime",
			output.ExitPrecondition, "Restore the source database's configuration before changing the follower.").
			WithDetails(map[string]any{"reason": "database_not_found", "database": follower.Follows})
	}
	class := req.Class
	if class == "" {
		// Default to the strict live class: an omitted class must never turn a
		// materialized follower into a metadata-only plan.
		class = FollowerRuntimeLive
	}
	switch class {
	case FollowerRuntimeLive, FollowerRuntimeAwaiting:
	default:
		return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
			"unknown follower runtime evidence class "+string(class),
			output.ExitPrecondition, "This is a vaire bug; report it with the run log.")
	}
	db := source.DatabaseName()
	resolver := cfg.ExtensionResolver()
	currentNames, err := resolver.EffectiveNames(source.Extensions)
	if err != nil {
		return FollowerRuntimePlan{}, config.ValidationError(err)
	}
	current, err := resolveExtensionDescriptors(resolver, currentNames)
	if err != nil {
		return FollowerRuntimePlan{}, config.ValidationError(err)
	}
	// Current inherited metadata safety is independent of the preload list and
	// files: every selected descriptor, SQL-only included, must declare
	// compatible copy/standby safety before history, save, package or DROP.
	if _, _, err := backup.MergeCopyRequirements(current, nil, true, db); err != nil {
		return FollowerRuntimePlan{}, err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return FollowerRuntimePlan{}, err
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	existing, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}
	// Actual installed SQL facts. A live catalog error is unknown state, not
	// an empty set; only an explicit post-effect refresh may keep the existing
	// obligations without claiming completeness. An awaiting target is
	// metadata-only and never pretends the catalog was queried.
	sqlComplete := false
	var installed map[string]bool
	if class == FollowerRuntimeLive {
		var factsErr error
		installed, factsErr = followerInstalledExtensions(ctx, follower)
		if factsErr != nil {
			if !req.AllowUnknownSQL {
				return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
					"cannot read "+follower.Name+"'s installed SQL extensions: "+factsErr.Error(),
					output.ExitPrecondition, "Start the follower and retry when its catalog answers; nothing was changed.").
					WithDetails(map[string]any{"reason": "follower_sql_unknown", "database": follower.Name})
			}
			if req.Snapshot == nil {
				// Reconstructing from the preload guard alone would silently
				// lose a SQL-only retained descriptor or an unguarded
				// mandatory suppression found by preflight. Stop before any
				// guard write, reload, start or restart.
				return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
					"cannot read "+follower.Name+"'s installed SQL extensions and no complete preflight runtime plan is available",
					output.ExitPrecondition, "Start the follower and retry when its catalog answers; nothing was changed.").
					WithDetails(map[string]any{"reason": "follower_runtime_snapshot_missing", "database": follower.Name})
			}
			return followerRuntimePlanFromSnapshot(*req.Snapshot), nil
		}
		sqlComplete = true
	}
	currentByName := map[string]catalog.Extension{}
	currentBySQL := map[string]catalog.Extension{}
	currentByPreload := map[string]catalog.Extension{}
	currentBySetting := map[string]catalog.Extension{}
	for _, ext := range current {
		currentByName[ext.Name] = ext
		if ext.ExtName != "" {
			currentBySQL[ext.ExtName] = ext
		}
		if ext.Preload != "" {
			currentByPreload[ext.Preload] = ext
		}
		for k := range ext.ResolvedSettings(db) {
			currentBySetting[k] = ext
		}
		for k := range ext.ResolvedCopySettings(db) {
			currentBySetting[k] = ext
		}
	}
	// Outgoing descriptors are the immediate pre-change definitions a removal
	// still needs to explain existing guard entries. Current intent always wins
	// the same name, SQL extension or library; outgoing is not retained
	// history and is retired only after actual SQL absence.
	outgoingByName := map[string]catalog.Extension{}
	outgoingBySQL := map[string]catalog.Extension{}
	outgoingByPreload := map[string]catalog.Extension{}
	outgoingBySetting := map[string]catalog.Extension{}
	for _, ext := range req.Outgoing {
		if ext.Name == "" {
			continue
		}
		if _, ok := currentByName[ext.Name]; ok {
			continue
		}
		if ext.ExtName != "" {
			if _, ok := currentBySQL[ext.ExtName]; ok {
				continue
			}
		}
		if prev, ok := outgoingByName[ext.Name]; ok && !prev.SemanticEqual(ext) {
			return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
				"the outgoing transition has ambiguous metadata for "+ext.Name,
				output.ExitPrecondition, "Keep one compatible outgoing definition; the follower was not changed.").
				WithDetails(map[string]any{"reason": "follower_outgoing_ambiguous", "database": follower.Name, "extension": ext.Name})
		}
		ext = ext.Clone()
		outgoingByName[ext.Name] = ext
		if ext.ExtName != "" {
			outgoingBySQL[ext.ExtName] = ext
		}
		if ext.Preload != "" {
			outgoingByPreload[ext.Preload] = ext
		}
		for k := range ext.ResolvedSettings(db) {
			outgoingBySetting[k] = ext
		}
		for k := range ext.ResolvedCopySettings(db) {
			outgoingBySetting[k] = ext
		}
	}
	retained := map[string]catalog.Extension{}
	addRetained := func(ext catalog.Extension) error {
		if ext.Name == "" {
			return nil
		}
		if _, ok := currentByName[ext.Name]; ok {
			return nil
		}
		if ext.ExtName != "" {
			if _, ok := currentBySQL[ext.ExtName]; ok {
				return nil
			}
		}
		if _, ok := outgoingByName[ext.Name]; ok {
			return nil
		}
		if ext.ExtName != "" {
			if _, ok := outgoingBySQL[ext.ExtName]; ok {
				return nil
			}
		}
		if prev, ok := retained[ext.Name]; ok {
			if !prev.SemanticEqual(ext) {
				return output.NewError(output.CodePrecondition,
					"follower "+follower.Name+" has ambiguous retained metadata for "+ext.Name,
					output.ExitPrecondition, "Keep one compatible retained definition; the follower was not changed.").
					WithDetails(map[string]any{"reason": "follower_retained_ambiguous", "database": follower.Name, "extension": ext.Name})
			}
			return nil
		}
		retained[ext.Name] = ext.Clone()
		return nil
	}
	if sqlComplete {
		for sqlName := range installed {
			if sqlName == "plpgsql" {
				continue
			}
			if _, ok := currentBySQL[sqlName]; ok {
				continue
			}
			if _, ok := outgoingBySQL[sqlName]; ok {
				continue
			}
			ext, err := retainedDescriptor(resolver, recs, source.Stanza, sqlName, "", "")
			if err != nil {
				return FollowerRuntimePlan{}, err
			}
			if err := addRetained(ext); err != nil {
				return FollowerRuntimePlan{}, err
			}
		}
	}
	// Existing guard metadata is the retained requirement evidence for a
	// continuously following runtime. A current library or setting wins its
	// old runtime variant before history is consulted, so an ordinary update
	// cannot become permanently ambiguous.
	if present {
		for _, lib := range existing.Preloads {
			if _, ok := currentByPreload[lib]; ok {
				continue
			}
			if _, ok := outgoingByPreload[lib]; ok {
				continue
			}
			ext, err := retainedDescriptor(resolver, recs, source.Stanza, "", lib, "")
			if err != nil {
				return FollowerRuntimePlan{}, err
			}
			if err := addRetained(ext); err != nil {
				return FollowerRuntimePlan{}, err
			}
		}
		for k := range existing.Settings {
			if _, ok := currentBySetting[k]; ok {
				continue
			}
			if _, ok := outgoingBySetting[k]; ok {
				continue
			}
			ext, err := retainedDescriptor(resolver, recs, source.Stanza, "", "", k)
			if err != nil {
				return FollowerRuntimePlan{}, err
			}
			if err := addRetained(ext); err != nil {
				return FollowerRuntimePlan{}, err
			}
		}
	}
	retainedList := valuesOf(retained)
	outgoingList := valuesOf(outgoingByName)
	all := append(append([]catalog.Extension(nil), current...), retainedList...)
	all = append(all, outgoingList...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}
		return all[i].ExtName < all[j].ExtName
	})
	// Retire a non-current SQL descriptor only when the actual follower
	// catalog proves its SQL absent. A preload-only descriptor has no SQL
	// absence proof and stays. A retired descriptor's library/settings remain
	// if another kept descriptor still requires them.
	kept := make([]catalog.Extension, 0, len(all))
	retired := map[string]catalog.Extension{}
	for _, ext := range all {
		_, isCurrent := currentByName[ext.Name]
		if req.RetireAbsent && sqlComplete && !isCurrent && ext.ExtName != "" && !installed[ext.ExtName] {
			retired[ext.Name] = ext
			continue
		}
		kept = append(kept, ext)
	}
	// Validate the required metadata without the outgoing descriptor: it is
	// leaving, and its old copy declaration must not block its own removal.
	// Genuine retained extras still require complete, compatible safety.
	safety := make([]catalog.Extension, 0, len(kept))
	for _, ext := range kept {
		if _, isOutgoing := outgoingByName[ext.Name]; isOutgoing {
			continue
		}
		safety = append(safety, ext)
	}
	if _, _, err := backup.MergeCopyRequirements(safety, nil, true, db); err != nil {
		return FollowerRuntimePlan{}, err
	}
	// Current and explicit operator settings win an outgoing descriptor's
	// ordinary runtime variant. Mandatory copy suppression still overlays and
	// still conflicts with another descriptor's contradictory suppression.
	baseKeys := map[string]bool{}
	for k := range cfg.Postgres.Settings {
		baseKeys[k] = true
	}
	for _, ext := range current {
		for k := range ext.ResolvedSettings(db) {
			baseKeys[k] = true
		}
		for k := range ext.ResolvedCopySettings(db) {
			baseKeys[k] = true
		}
	}
	for k := range follower.Settings {
		baseKeys[k] = true
	}
	guardKnown := make([]catalog.Extension, 0, len(kept))
	for _, ext := range kept {
		if _, isOutgoing := outgoingByName[ext.Name]; isOutgoing {
			ext = filterTransitionOrdinary(ext, baseKeys)
		}
		guardKnown = append(guardKnown, ext)
	}
	actualPermanent, err := backup.EffectivePermanentSettings(PGConfDir(follower.Name), dataDir)
	if err != nil {
		return FollowerRuntimePlan{}, output.NewError(output.CodePrecondition,
			"follower "+follower.Name+"'s permanent configuration cannot be proven: "+err.Error(),
			output.ExitPrecondition, "Fix its configuration; the follower was not changed.").
			WithDetails(map[string]any{"reason": "follower_permanent_unproven", "database": follower.Name})
	}
	permanent := map[string]string{}
	for k, v := range actualPermanent {
		permanent[k] = v
	}
	for _, ext := range current {
		for k, v := range ext.ResolvedSettings(db) {
			if _, ok := permanent[k]; !ok {
				permanent[k] = v
			}
		}
	}
	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: guardKnown, Current: current, Permanent: permanent,
		Explicit: follower.Settings, CopyDB: db, Pending: true,
	})
	if err != nil {
		return FollowerRuntimePlan{}, err
	}
	// Expected runtime: global settings, current ordinary settings, the
	// follower's explicit settings, then retained/outgoing ordinary settings
	// that no higher-precedence source supplies, and finally every own
	// mandatory copy overlay. This is what verification checks, not just the
	// guard assignments.
	//
	// ExpectedSettings holds decoded runtime values. Descriptor settings and
	// copy settings are PostgreSQL config literals and are decoded exactly
	// once; [postgres.settings] and the follower's explicit settings are raw
	// values the renderer quotes, so they are already decoded.
	expected := map[string]string{}
	// Renderer order: 30-vaire-extensions.conf, then 50-vaire-settings.conf
	// (setup-wide settings followed by the database's explicit settings).
	for _, ext := range current {
		for k, literal := range ext.ResolvedSettings(db) {
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}
	for k, v := range cfg.Postgres.Settings {
		expected[k] = v
	}
	for k, v := range follower.Settings {
		expected[k] = v
	}
	// permanentDecoded is the same precedence in decoded runtime form. The
	// guard's Permanent map may still carry literals for its own decisions.
	permanentDecoded := map[string]string{}
	for k, v := range actualPermanent {
		permanentDecoded[k] = v
	}
	for _, ext := range current {
		for k, literal := range ext.ResolvedSettings(db) {
			if _, ok := permanentDecoded[k]; !ok {
				permanentDecoded[k] = catalog.NormalizedSettingValue(literal)
			}
		}
	}
	for _, ext := range kept {
		if _, isCurrent := currentByName[ext.Name]; isCurrent {
			continue
		}
		view := ext
		if _, isOutgoing := outgoingByName[ext.Name]; isOutgoing {
			view = filterTransitionOrdinary(ext, baseKeys)
		}
		mandatory := map[string]string{}
		if view.CopySafety == catalog.CopySafetyDisabled {
			for k, literal := range view.ResolvedCopySettings(db) {
				mandatory[k] = catalog.NormalizedSettingValue(literal)
			}
		}
		for k, literal := range view.ResolvedSettings(db) {
			if _, own := mandatory[k]; own {
				continue
			}
			if _, ok := expected[k]; ok {
				continue
			}
			if supplied, ok := permanentDecoded[k]; ok {
				// The permanent configuration already supplies this retained
				// key; the guard deliberately does not override it.
				expected[k] = supplied
				continue
			}
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}
	for _, ext := range kept {
		if ext.CopySafety != catalog.CopySafetyDisabled {
			continue
		}
		for k, literal := range ext.ResolvedCopySettings(db) {
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}
	retainedPreloads := make([]string, 0, len(kept))
	for _, ext := range kept {
		if _, isCurrent := currentByName[ext.Name]; isCurrent {
			continue
		}
		if ext.Preload != "" {
			retainedPreloads = append(retainedPreloads, ext.Preload)
		}
	}
	sort.Strings(retainedPreloads)
	retainedPreloads = slices.Compact(retainedPreloads)
	currentPreloads, err := resolver.Preloads(source.Extensions)
	if err != nil {
		return FollowerRuntimePlan{}, config.ValidationError(err)
	}
	base := backup.CopyGuard{Preloads: retainedPreloads, Settings: decision.Settings}
	keptPreloads := map[string]bool{}
	for _, ext := range kept {
		if ext.Preload != "" {
			keptPreloads[ext.Preload] = true
		}
	}
	droppedPreloads := map[string]bool{}
	for _, ext := range retired {
		if ext.Preload != "" && !keptPreloads[ext.Preload] {
			droppedPreloads[ext.Preload] = true
		}
	}
	composed, err := backup.ComposeCopyGuardDropping(PGConfDir(follower.Name), dataDir, base, currentPreloads, droppedPreloads)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}
	pendingSQL := map[string]bool{}
	if req.RetireAbsent && sqlComplete {
		for _, ext := range kept {
			if _, isOutgoing := outgoingByName[ext.Name]; !isOutgoing {
				// A genuinely retained extra is not a pending removal. Only
				// the explicitly outgoing descriptor may hold a transition
				// open while its SQL still replays.
				continue
			}
			if ext.ExtName != "" && installed[ext.ExtName] {
				pendingSQL[ext.ExtName] = true
			}
		}
	}
	cron := false
	for _, ext := range kept {
		if ext.Name == "pg_cron" || ext.ExtName == "pg_cron" {
			cron = true
		}
	}
	keptNonCurrent := make([]catalog.Extension, 0, len(kept))
	keptOutgoing := make([]catalog.Extension, 0, len(outgoingList))
	for _, ext := range kept {
		if _, isCurrent := currentByName[ext.Name]; isCurrent {
			continue
		}
		keptNonCurrent = append(keptNonCurrent, ext)
		if _, isOutgoing := outgoingByName[ext.Name]; isOutgoing {
			keptOutgoing = append(keptOutgoing, ext)
		}
	}
	return FollowerRuntimePlan{
		Guard: composed, Requirements: kept, Preloads: composed.Preloads,
		Settings: composed.Settings, ExpectedSettings: expected, Cron: cron,
		Retained: keptNonCurrent, Outgoing: keptOutgoing, Permanent: permanent,
		SQLFactsComplete: sqlComplete, PendingSQL: sortedKeys(pendingSQL),
	}, nil
}

// followerRuntimePlanFromSnapshot preserves every preflight obligation when a
// post-effect catalog read fails. It never claims complete SQL facts: the plan
// is explicitly pending, so primary configuration/SQL must not treat the
// follower as ready until a fresh catalog read supersedes the snapshot.
func followerRuntimePlanFromSnapshot(snapshot FollowerRuntimePlan) FollowerRuntimePlan {
	plan := cloneFollowerRuntimePlan(snapshot)
	plan.SQLFactsComplete = false
	pending := map[string]bool{}
	for _, ext := range plan.Outgoing {
		if ext.ExtName != "" {
			pending[ext.ExtName] = true
		}
	}
	plan.PendingSQL = sortedKeys(pending)
	return plan
}

// filterTransitionOrdinary drops ordinary settings a current or explicit
// source already owns, so an outgoing descriptor's old runtime variant cannot
// override current intent. Mandatory copy settings stay for the copy overlay
// and conflict checks.
func filterTransitionOrdinary(ext catalog.Extension, keepCurrent map[string]bool) catalog.Extension {
	out := ext.Clone()
	if len(out.Settings) == 0 {
		return out
	}
	filtered := map[string]string{}
	for k, v := range out.Settings {
		if !keepCurrent[k] {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		filtered = nil
	}
	out.Settings = filtered
	return out
}

func cloneFollowerRuntimePlan(plan FollowerRuntimePlan) FollowerRuntimePlan {
	out := plan
	out.Guard = plan.Guard.Clone()
	out.Requirements = cloneExtensionSlice(plan.Requirements)
	out.Preloads = slices.Clone(plan.Preloads)
	out.Settings = cloneStringMap(plan.Settings)
	out.ExpectedSettings = cloneStringMap(plan.ExpectedSettings)
	out.Retained = cloneExtensionSlice(plan.Retained)
	out.Outgoing = cloneExtensionSlice(plan.Outgoing)
	out.Permanent = cloneStringMap(plan.Permanent)
	out.PendingSQL = slices.Clone(plan.PendingSQL)
	return out
}

func cloneExtensionSlice(exts []catalog.Extension) []catalog.Extension {
	if exts == nil {
		return nil
	}
	out := make([]catalog.Extension, len(exts))
	for i := range exts {
		out[i] = exts[i].Clone()
	}
	return out
}

func cloneStringMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func resolveExtensionDescriptors(resolver config.ExtensionResolver, names []string) ([]catalog.Extension, error) {
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

func retainedDescriptor(resolver config.ExtensionResolver, recs backup.Records, stanza, sqlName, preload, setting string) (catalog.Extension, error) {
	matches := func(exts []catalog.Extension) []catalog.Extension {
		var out []catalog.Extension
		for _, ext := range exts {
			if sqlName != "" && ext.ExtName != sqlName {
				continue
			}
			if preload != "" && ext.Preload != preload {
				continue
			}
			if setting != "" {
				if _, ok := ext.ResolvedSettings("")[setting]; !ok {
					if _, ok := ext.ResolvedCopySettings("")[setting]; !ok {
						continue
					}
				}
			}
			out = append(out, ext)
		}
		return out
	}
	found := matches(recs.RetainedExtensions(stanza))
	if len(found) == 0 {
		list, err := resolver.List()
		if err != nil {
			return catalog.Extension{}, config.ValidationError(err)
		}
		found = matches(list)
	}
	unique := map[string]catalog.Extension{}
	for _, ext := range found {
		key := ext.Name + "\x00" + ext.ExtName + "\x00" + ext.Preload
		if prev, ok := unique[key]; ok && !prev.SemanticEqual(ext) {
			return catalog.Extension{}, output.NewError(output.CodePrecondition,
				"retained metadata for "+ext.Name+" is ambiguous",
				output.ExitPrecondition, "Keep one compatible definition; the follower was not changed.").
				WithDetails(map[string]any{"reason": "follower_retained_ambiguous", "extension": ext.Name})
		}
		unique[key] = ext
	}
	if len(unique) == 0 {
		what := sqlName
		if what == "" {
			if preload != "" {
				what = "library " + preload
			} else {
				what = "setting " + setting
			}
		}
		return catalog.Extension{}, output.NewError(output.CodePrecondition,
			"the follower's retained data requires "+what+", which no definition explains",
			output.ExitPrecondition, "Declare it in [extensions.<name>] with copy_safety and standby_safe; the follower was not changed.").
			WithDetails(map[string]any{"reason": "follower_retained_unknown", "extension": what})
	}
	if len(unique) > 1 {
		what := sqlName
		if what == "" {
			if preload != "" {
				what = "library " + preload
			} else {
				what = "setting " + setting
			}
		}
		return catalog.Extension{}, output.NewError(output.CodePrecondition,
			"the follower's retained data has ambiguous metadata for "+what,
			output.ExitPrecondition, "Keep one compatible definition; the follower was not changed.").
			WithDetails(map[string]any{"reason": "follower_retained_ambiguous", "extension": what})
	}
	for _, ext := range unique {
		return ext, nil
	}
	return catalog.Extension{}, nil
}

func valuesOf(m map[string]catalog.Extension) []catalog.Extension {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]catalog.Extension, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}

// FollowerGuardDrifted reports whether the installed guard differs from the
// plan's complete winning union. It is read-only; Check uses it to retry a
// guard refresh even when every managed file already matches.
func FollowerGuardDrifted(installed backup.CopyGuard, present bool, plan FollowerRuntimePlan) bool {
	if !present {
		return len(plan.Guard.Preloads) > 0 || len(plan.Guard.Settings) > 0
	}
	have := map[string]bool{}
	for _, lib := range installed.Preloads {
		have[lib] = true
	}
	want := map[string]bool{}
	for _, lib := range plan.Guard.Preloads {
		want[lib] = true
		if !have[lib] {
			return true
		}
	}
	for lib := range have {
		if !want[lib] {
			return true
		}
	}
	if len(installed.Settings) != len(plan.Guard.Settings) {
		return true
	}
	for k, wantValue := range plan.Guard.Settings {
		got, ok := installed.Settings[k]
		if !ok || catalog.NormalizedSettingValue(got) != catalog.NormalizedSettingValue(wantValue) {
			return true
		}
	}
	return false
}

var guardInstance = func() (string, error) { return masterbackup.ReadInstance("") }

// SetGuardInstanceForTest replaces the instance identity seam for a test in
// another package and returns the restore function.
func SetGuardInstanceForTest(f func() (string, error)) func() {
	old := guardInstance
	guardInstance = f
	return func() { guardInstance = old }
}

// managedExtensionsConfPath is the last applied extension configuration for
// a cluster. It is the evidence a stopped cluster can offer when its live
// catalog cannot be read.
var managedExtensionsConfPath = func(cluster string) string {
	return PGConfD(cluster) + "/30-vaire-extensions.conf"
}

// FirstManagedConfig reports whether spec has no vaire-managed conf.d files
// yet: its first configuration, with no earlier managed extension
// configuration that could be unloaded. An unreadable directory is not
// treated as first, so the refusal path can report it instead of guessing.
func FirstManagedConfig(spec config.ClusterSpec) bool {
	for _, name := range []string{
		"10-vaire-tuning.conf",
		"20-vaire-security.conf",
		"30-vaire-extensions.conf",
		"40-vaire-backup.conf",
		"50-vaire-settings.conf",
	} {
		_, err := os.Stat(PGConfD(spec.Name) + "/" + name)
		switch {
		case err == nil:
			return false
		case os.IsNotExist(err):
			continue
		default:
			return false
		}
	}
	return true
}

// RefuseExtensionUnload stops a configuration change from removing a library
// that an installed, no-longer-desired SQL extension still uses. The safe path
// is the direct extension command, which drops while the library is loaded. A
// stopped or unreadable cluster is judged from its last managed extension
// file: removing a preload is refused because installed SQL cannot be proven
// absent; a new or unchanged cluster is not blocked.
func RefuseExtensionUnload(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, firstConfig bool) error {
	installedRows, queryErr := ClusterPG(spec).Query(ctx, spec.DatabaseName(), "SELECT extname AS name FROM pg_extension ORDER BY 1", nil)
	if queryErr != nil {
		if firstConfig {
			// A cluster's first managed configuration has no previous managed
			// extension file to unload; it is not blocked merely for being
			// stopped or not yet reachable.
			return nil
		}
		return refuseUnloadFromManagedFiles(cfg, spec)
	}
	installed := map[string]bool{}
	for _, row := range installedRows {
		installed[row["name"]] = true
	}
	desired := map[string]bool{}
	entries, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		return err
	}
	for _, ext := range entries {
		desired[ext.ExtName] = true
	}
	loaded, err := LoadedPreloads(ctx, ClusterPG(spec))
	if err != nil {
		if firstConfig {
			// No earlier managed configuration exists to unload from.
			return nil
		}
		return refuseUnloadFromManagedFiles(cfg, spec)
	}
	rendered, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range rendered {
		keep[p] = true
	}
	defs := map[string]catalog.Extension{}
	list, err := cfg.ExtensionResolver().List()
	if err != nil {
		return err
	}
	for _, ext := range list {
		if ext.ExtName != "" {
			defs[ext.ExtName] = ext
		}
	}
	for name := range installed {
		if name == "plpgsql" {
			continue
		}
		ext, known := defs[name]
		if !known {
			if desired[name] {
				continue
			}
			for p := range loaded {
				if !keep[p] {
					return output.NewError(output.CodePrecondition,
						"applying this configuration would unload "+p+" while unmanaged extension "+name+"'s SQL is still installed in "+spec.Name,
						output.ExitPrecondition,
						"Declare it in [extensions.<name>] with copy/standby safety, or remove it with vaire ext remove; nothing was applied.").
						WithDetails(map[string]any{"reason": "extension_remove_required", "database": spec.Name, "extension": name, "library": p})
				}
			}
			continue
		}
		if ext.Preload != "" && loaded[ext.Preload] && !keep[ext.Preload] {
			return output.NewError(output.CodePrecondition,
				"applying this configuration would unload "+ext.Preload+" while "+name+"'s SQL is still installed in "+spec.Name,
				output.ExitPrecondition,
				"Remove it with vaire ext remove "+ext.Name+" --db "+spec.Name+" --confirm "+spec.Name+", or add it to extensions; nothing was applied.").
				WithDetails(map[string]any{"reason": "extension_remove_required", "database": spec.Name, "extension": ext.Name, "library": ext.Preload})
		}
	}
	return nil
}

func refuseUnloadFromManagedFiles(cfg config.Cluster, spec config.ClusterSpec) error {
	oldPreloads, err := managedPreloads(managedExtensionsConfPath(spec.Name))
	if err != nil {
		if os.IsNotExist(err) {
			if _, dirErr := os.Stat(PGConfDir(spec.Name)); dirErr == nil {
				return output.NewError(output.CodePrecondition,
					spec.Name+" exists but its last managed extension configuration is missing, so vaire cannot prove its preloads safe",
					output.ExitPrecondition,
					"Restore "+managedExtensionsConfPath(spec.Name)+", or start the cluster so vaire can read pg_extension; nothing was applied.").
					WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": spec.Name})
			}
			// A new cluster has no last configuration to unload.
			return nil
		}
		return output.NewError(output.CodePrecondition,
			"vaire cannot read "+spec.Name+"'s last managed extension configuration to prove its preloads safe: "+err.Error(),
			output.ExitPrecondition,
			"Fix access to "+managedExtensionsConfPath(spec.Name)+", or start the cluster so vaire can read pg_extension; nothing was applied.").
			WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": spec.Name})
	}
	newPreloads, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return err
	}
	keep := map[string]bool{}
	for _, p := range newPreloads {
		keep[p] = true
	}
	for _, p := range oldPreloads {
		if keep[p] {
			continue
		}
		return output.NewError(output.CodePrecondition,
			"applying this configuration would unload "+p+" from "+spec.Name+", but its cluster is stopped or unreadable so vaire cannot prove no installed extension still uses it",
			output.ExitPrecondition,
			"Start "+spec.Name+" and retry so vaire can read pg_extension, or remove the extension with vaire ext remove before changing its preload; nothing was applied.").
			WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": spec.Name, "library": p})
	}
	return nil
}

// managedPreloads reads the shared_preload_libraries value from a managed
// 30-vaire-extensions.conf. A missing setting is an empty list.
func managedPreloads(path string) ([]string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a cluster's own managed configuration.
	if err != nil {
		return nil, err
	}
	value := ""
	for _, line := range strings.Split(string(raw), "\n") {
		key, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && strings.TrimSpace(key) == "shared_preload_libraries" {
			value = strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	if value == "" {
		return nil, nil
	}
	seen := map[string]bool{}
	var out []string
	for _, part := range strings.Split(value, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" && !seen[part] {
			seen[part] = true
			out = append(out, part)
		}
	}
	return out, nil
}

// PrepareExtensionStart installs and verifies one target database's current// extension requirements before a selected step or direct lifecycle operation
// has any effect. It never resolves the whole host's union and never restarts.
// extra carries retained historical requirements when E705 supplies them; nil
// means today's configuration only.
func PrepareExtensionStart(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, extra []ExtensionPrerequisite, rt engine.Runtime, report engine.Reporter) error {
	reqs, err := extensionRuntimeRequirementsForSpec(cfg, spec)
	if err != nil {
		return err
	}
	reqs = append(reqs, extra...)
	return PrepareExtensionRequirements(ctx, reqs, rt, report)
}

// PrepareExtensionRequirements installs/holds and verifies an explicit,
// already-resolved requirement set. It is the shared extension-only effect
// boundary: no full raw cfg union, no repository setup, no upgrade and no
// uninstall.
func PrepareExtensionRequirements(ctx context.Context, reqs []ExtensionPrerequisite, rt engine.Runtime, report engine.Reporter) error {
	packages := extensionPackagesFrom(reqs)
	if len(packages) > 0 {
		env := map[string]string{
			"PACKAGES":           "",
			"HOLD":               "",
			"EXTENSION_PACKAGES": strings.Join(packages, " "),
			"PGDG_SCRIPT":        "",
			"PGBACKREST_MIN":     "",
			"STOP_PGBOUNCER":     "0",
		}
		if err := runExtensionPackageScript(ctx, postgresPackagesID, env, rt, report); err != nil {
			return fmt.Errorf("installing extension packages before starting: %w; installed packages are not removed", err)
		}
	}
	if err := VerifyExtensionRequirements(reqs); err != nil {
		return fmt.Errorf("extension prerequisites are not met: %w; installed packages are not removed", err)
	}
	return nil
}

// DesiredExtensionNames is the SQL-creation projection of spec's current
// intent. It excludes preload-only modules: they are runtime prerequisites,
// not CREATE EXTENSION entries.
func DesiredExtensionNames(cfg config.Cluster, spec config.ClusterSpec) ([]catalog.Extension, error) {
	return extensionSQLSelection(cfg, spec)
}

// DesiredExtensionSQL is the SQL that creates spec's missing desired SQL
// extensions in db. It never updates a version, drops an extra or creates a
// historical-only extension.
func DesiredExtensionSQL(cfg config.Cluster, spec config.ClusterSpec, installed map[string]bool, db string) (string, []string, error) {
	entries, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var created []string
	for _, ext := range entries {
		if installed[ext.ExtName] {
			continue
		}
		b.WriteString(ExtensionSQL(ext, db))
		created = append(created, ext.Name)
	}
	return b.String(), created, nil
}

// ExtensionSQL is the SQL that creates ext in database db: its schema first,
// then CREATE EXTENSION, then the built-in minimum grants to db's owner role.
// It returns "" for a preload-only module such as auto_explain. Custom
// extensions get no grants, so vaire never widens access to unknown SQL.
func ExtensionSQL(ext catalog.Extension, db string) string {
	if ext.ExtName == "" {
		return ""
	}
	var b strings.Builder
	if ext.Schema != "" {
		fmt.Fprintf(&b, "CREATE SCHEMA IF NOT EXISTS %s;\n", pg.Quote(ext.Schema))
	}
	create := "CREATE EXTENSION IF NOT EXISTS " + pg.Quote(ext.ExtName)
	if ext.Schema != "" {
		create += " SCHEMA " + pg.Quote(ext.Schema)
	}
	b.WriteString(create + ";\n")
	owner := pg.Quote(db + "_owner")
	for _, grant := range builtinExtensionGrants(ext) {
		b.WriteString(strings.ReplaceAll(grant, "{owner}", owner) + "\n")
	}
	return b.String()
}

// builtinExtensionGrants are the minimum grants the built-in entries need for
// an application owner to use them. Custom extensions deliberately get none.
// The grants use the resolved schema, so an operator schema override never
// grants a default schema the extension is not installed in.
func builtinExtensionGrants(ext catalog.Extension) []string {
	switch ext.Name {
	case "pg_cron":
		schema := ext.Schema
		if schema == "" {
			schema = "cron"
		}
		return []string{"GRANT USAGE ON SCHEMA " + pg.Quote(schema) + " TO {owner};"}
	case "pg_partman":
		if ext.Schema == "" {
			return nil
		}
		schema := pg.Quote(ext.Schema)
		return []string{
			"GRANT ALL ON SCHEMA " + schema + " TO {owner};",
			"GRANT ALL ON ALL TABLES IN SCHEMA " + schema + " TO {owner};",
			"GRANT EXECUTE ON ALL FUNCTIONS IN SCHEMA " + schema + " TO {owner};",
			"GRANT EXECUTE ON ALL PROCEDURES IN SCHEMA " + schema + " TO {owner};",
		}
	}
	return nil
}

// ParsePreloads splits a shared_preload_libraries value into its libraries.
func ParsePreloads(value string) map[string]bool {
	out := map[string]bool{}
	for _, part := range strings.Split(value, ",") {
		part = strings.Trim(strings.TrimSpace(part), `"'`)
		if part != "" {
			out[part] = true
		}
	}
	return out
}

// LoadedPreloads reads the running cluster's effective
// shared_preload_libraries.
func LoadedPreloads(ctx context.Context, local pg.Local) (map[string]bool, error) {
	rows, err := queryColumn(ctx, local, "postgres", "SELECT current_setting('shared_preload_libraries')")
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return map[string]bool{}, nil
	}
	return ParsePreloads(rows[0]), nil
}

// SharedPreloadsPending reports whether the running cluster has a pending
// restart for shared_preload_libraries. It lets a caller distinguish "the
// confirmed restart will load this library" from a missing prerequisite.
func SharedPreloadsPending(ctx context.Context, local pg.Local) (bool, error) {
	pending, err := pendingRestartSettings(ctx, local)
	if err != nil {
		return false, err
	}
	for _, name := range pending {
		if name == "shared_preload_libraries" {
			return true, nil
		}
	}
	return false, nil
}

// extensionCreationSQL is the SQL for a running cluster: an extension whose
// required library is not loaded is deferred only when shared_preload_libraries
// itself is pending a restart. A missing library with no such pending restart
// is an actionable error, not a promise that a restart would cure it.
func extensionCreationSQL(cfg config.Cluster, spec config.ClusterSpec, loaded, pending map[string]bool) (string, []string, error) {
	sqlExtensions, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var deferred []string
	for _, ext := range sqlExtensions {
		if ext.Preload != "" && !loaded[ext.Preload] {
			if !pending["shared_preload_libraries"] {
				return "", nil, fmt.Errorf("extension %q needs library %q preloaded in %s, but it is not loaded and no restart is pending to load it; verify the package and 30-vaire-extensions.conf", ext.Name, ext.Preload, spec.Name)
			}
			deferred = append(deferred, ext.Name)
			continue
		}
		// An effective library is not enough when one of the descriptor's own
		// settings still waits for a restart. Only this extension's required
		// keys matter; an unrelated pending PostgreSQL setting does not defer
		// its SQL. Reload-only settings never appear in pending_restart.
		blocked := false
		for key := range ext.ResolvedSettings(spec.Name) {
			if pending[key] {
				blocked = true
				break
			}
		}
		if blocked {
			deferred = append(deferred, ext.Name)
			continue
		}
		b.WriteString(ExtensionSQL(ext, spec.Name))
	}
	return b.String(), deferred, nil
}

// PrimaryExtensionExpectedSettings is the effective ordinary value for one
// extension's own setting keys on a primary: the descriptor default, then the
// setup-wide [postgres.settings], then the database's explicit
// [clusters.settings], matching the renderer's load order. Copy overlays do
// not apply to a primary.
//
// The returned map is decoded runtime values, not config literals. Descriptor
// settings are PostgreSQL config literals, so each is decoded exactly once
// before merging. [postgres.settings] and [clusters.settings] values are raw
// values that the renderer quotes itself; they are already decoded and must
// not be normalized again.
func PrimaryExtensionExpectedSettings(cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension) map[string]string {
	ordinary := ext.ResolvedSettings(spec.Name)
	if len(ordinary) == 0 {
		return nil
	}
	expected := make(map[string]string, len(ordinary))
	for k, literal := range ordinary {
		expected[k] = catalog.NormalizedSettingValue(literal)
	}
	for k := range expected {
		if raw, ok := cfg.Postgres.Settings[k]; ok {
			expected[k] = raw
		}
	}
	for k := range expected {
		if raw, ok := spec.Settings[k]; ok {
			expected[k] = raw
		}
	}
	return expected
}

// ExtensionSettingFact is one effective PostgreSQL setting value together with
// its pg_settings vartype. An empty Type means PostgreSQL did not report a
// type (a custom placeholder), so the value is compared exactly.
type ExtensionSettingFact struct {
	Value string
	Type  string
}

// ExtensionSettingFactSQL reads one required setting's effective value and
// type. current_setting is the authority for the value; pg_settings supplies
// the type only when PostgreSQL knows the setting. A placeholder that is not
// in pg_settings still reports its current_setting value with an empty type.
func ExtensionSettingFactSQL(key string) string {
	k := literal(key)
	return "SELECT current_setting(" + k + ", true) AS v, coalesce((SELECT vartype FROM pg_settings WHERE name = " + k + "), '') AS vartype"
}

// ParseExtensionSettingFact reads ExtensionSettingFactSQL's one row.
func ParseExtensionSettingFact(row map[string]string) ExtensionSettingFact {
	return ExtensionSettingFact{Value: row["v"], Type: row["vartype"]}
}

// ExtensionSettingMatches compares an expected decoded setting value with an
// actual fact. PostgreSQL boolean GUCs canonicalize true/on/yes/1 and
// false/off/no/0 to on/off; both sides of a boolean comparison therefore use
// PostgreSQL's boolean input forms. Every other setting, including a custom
// placeholder with no pg_settings type, is compared exactly, so quote content
// and arbitrary text are never normalized.
func ExtensionSettingMatches(want string, fact ExtensionSettingFact) bool {
	if fact.Type == "bool" {
		w, ok := postgresBool(want)
		if !ok {
			return false
		}
		got, ok := postgresBool(fact.Value)
		if !ok {
			return false
		}
		return w == got
	}
	return fact.Value == want
}

// postgresBool accepts PostgreSQL's unambiguous boolean input forms. It does
// not trim or strip quotes: malformed or quote-content values are not booleans.
func postgresBool(value string) (bool, bool) {
	switch strings.ToLower(value) {
	case "true", "on", "yes", "1":
		return true, true
	case "false", "off", "no", "0":
		return false, true
	}
	return false, false
}

// PrimaryExtensionReadiness is the shared effective-prerequisite decision for
// a primary extension CREATE or UPDATE. RestartPending says the fix is a
// confirmed restart; otherwise a required setting simply is not effective yet.
type PrimaryExtensionReadiness struct {
	Deferred       bool
	RestartPending bool
	Detail         string
}

// CheckPrimaryExtensionReadiness compares the extension's required primary
// library and setting values with the target's actual values. It is the one
// helper used by dependent CREATE and by the direct ALTER UPDATE path.
func CheckPrimaryExtensionReadiness(cfg config.Cluster, spec config.ClusterSpec, ext catalog.Extension, loaded, pending map[string]bool, actual map[string]ExtensionSettingFact) (PrimaryExtensionReadiness, error) {
	if ext.Preload != "" && !loaded[ext.Preload] {
		if !pending["shared_preload_libraries"] {
			return PrimaryExtensionReadiness{}, fmt.Errorf("extension %q needs library %q preloaded in %s, but it is not loaded and no restart is pending to load it; verify the package and 30-vaire-extensions.conf", ext.Name, ext.Preload, spec.Name)
		}
		return PrimaryExtensionReadiness{
			Deferred: true, RestartPending: true,
			Detail: "library " + ext.Preload + " waits for a restart",
		}, nil
	}
	expected := PrimaryExtensionExpectedSettings(cfg, spec, ext)
	for _, key := range sortedStringKeys(expected) {
		got, ok := actual[key]
		if !ok {
			return PrimaryExtensionReadiness{}, fmt.Errorf("extension %q requires %s, but %s did not report its effective value; no dependent SQL was run", ext.Name, key, spec.Name)
		}
		// expected is already decoded (descriptor literal decoded once, raw
		// global/spec overrides untouched) and got.Value is the decoded
		// current_setting value. A PostgreSQL boolean GUC is compared through
		// its boolean input forms; every other setting stays exact.
		want := expected[key]
		if ExtensionSettingMatches(want, got) {
			continue
		}
		if pending[key] {
			return PrimaryExtensionReadiness{
				Deferred: true, RestartPending: true,
				Detail: key + " = " + want + " waits for a restart",
			}, nil
		}
		return PrimaryExtensionReadiness{
			Deferred: true,
			Detail:   key + " is " + got.Value + ", want " + want,
		}, nil
	}
	return PrimaryExtensionReadiness{}, nil
}

// extensionCreationSQLWithReadiness creates only SQL whose own preload and
// required setting values are actually effective. installed skips SQL that is
// already present, so unrelated pending settings cannot block convergence.
// actual is the target's real pg_settings values for every required key.
func extensionCreationSQLWithReadiness(cfg config.Cluster, spec config.ClusterSpec, loaded, pending, installed map[string]bool, actual map[string]ExtensionSettingFact) (string, []extensionSQLDeferral, error) {
	sqlExtensions, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		return "", nil, err
	}
	var b strings.Builder
	var deferred []extensionSQLDeferral
	for _, ext := range sqlExtensions {
		if ext.ExtName != "" && installed[ext.ExtName] {
			// Already present: the idempotent CREATE is a no-op and does not
			// depend on a pending setting or restart. Keep the baseline SQL.
			b.WriteString(ExtensionSQL(ext, spec.Name))
			continue
		}
		readiness, err := CheckPrimaryExtensionReadiness(cfg, spec, ext, loaded, pending, actual)
		if err != nil {
			return "", nil, err
		}
		if readiness.Deferred {
			deferred = append(deferred, extensionSQLDeferral{
				Name: ext.Name, Detail: readiness.Detail, RestartPending: readiness.RestartPending,
			})
			continue
		}
		b.WriteString(ExtensionSQL(ext, spec.Name))
	}
	return b.String(), deferred, nil
}

func sortedStringKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// extensionPendingMessage is the extension_pending_restart warning. It names
// the confirmed restart that loads the library and the retry that creates the
// deferred SQL.
func extensionPendingMessage(db string, names []string) string {
	return strings.Join(names, ", ") + " in " + db + " need their preload libraries first: the restart of " + db +
		" (vaire apply --db " + db + " --confirm " + db + "/restart) loads them; then vaire apply --db " + db + " creates them"
}
