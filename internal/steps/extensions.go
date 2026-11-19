package steps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
// against its immutable stanza before a backup is taken.
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

	for _, specs := range [][]config.ClusterSpec{cfg.Clusters, cfg.Awaiting} {
		for _, spec := range specs {
			if err := addSpecExtensionPrerequisites(resolver, spec, seen, &out); err != nil {
				return nil, err
			}
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

// addSpecExtensionPrerequisites collects one cluster's declared and recovery
// selections into the host-wide requirement set.
func addSpecExtensionPrerequisites(resolver config.ExtensionResolver, spec config.ClusterSpec, seen map[ExtensionPrerequisite]bool, out *[]ExtensionPrerequisite) error {
	selections := [][]string{spec.Extensions}
	if spec.RecoveryExtensions != nil {
		selections = append(selections, *spec.RecoveryExtensions)
	}

	for _, declared := range selections {
		if err := addDeclaredExtensionPrerequisites(resolver, declared, seen, out); err != nil {
			return err
		}
	}

	return nil
}

// addDeclaredExtensionPrerequisites resolves one declared selection and adds
// only the entries that have a control file or preload library.
func addDeclaredExtensionPrerequisites(resolver config.ExtensionResolver, declared []string, seen map[ExtensionPrerequisite]bool, out *[]ExtensionPrerequisite) error {
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
		*out = append(*out, req)
	}

	return nil
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

// ClusterStepID is cluster's postgres-cluster-v1 instance. The extension
// transition uses it to let the engine start a stopped target before it reads
// that target's pg_extension catalog.
func ClusterStepID(cluster string) string { return engine.InstanceID(postgresClusterID, cluster) }

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

// SetExtensionDirectoriesForTest supplies a rootless file tree to cross-package
// lifecycle tests. Call the returned function to restore the host paths.
func SetExtensionDirectoriesForTest(control, libraries string) func() {
	oldControl, oldLibraries := extensionControlDir, extensionLibDir
	extensionControlDir, extensionLibDir = control, libraries

	return func() { extensionControlDir, extensionLibDir = oldControl, oldLibraries }
}

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
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("cannot read control file: %w", err)
	}

	for line := range strings.SplitSeq(string(raw), "\n") {
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

	defs, err := copyGuardPreloadDefinitions(cfg, known)
	if err != nil {
		return err
	}

	transitionNames := extensionNameSet(transition)

	reqs := make([]ExtensionPrerequisite, 0, len(g.Preloads))
	for _, library := range g.Preloads {
		req, err := validateCopyGuardPreload(spec, copyDB, library, defs[library], transitionNames, permanent, standby, released, g)
		if err != nil {
			return err
		}

		reqs = append(reqs, req)
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

// copyGuardPreloadDefinitions indexes every known and configured descriptor
// that declares a library, keyed by that library.
func copyGuardPreloadDefinitions(cfg config.Cluster, known []catalog.Extension) (map[string][]catalog.Extension, error) {
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
		return nil, config.ValidationError(err)
	}

	for _, ext := range list {
		add(ext)
	}

	return defs, nil
}

func extensionNameSet(exts []catalog.Extension) map[string]bool {
	out := make(map[string]bool, len(exts))
	for _, ext := range exts {
		out[ext.Name] = true
	}

	return out
}

// validateCopyGuardPreload binds one final preload library to its single
// compatible descriptor and its copy/standby obligations.
func validateCopyGuardPreload(spec config.ClusterSpec, copyDB, library string, candidates []catalog.Extension, transitionNames map[string]bool, permanent map[string]string, standby bool, released map[string]string, g backup.CopyGuard) (ExtensionPrerequisite, error) {
	base, transitionOnly, err := selectCopyGuardPreloadDefinition(spec, library, candidates, transitionNames)
	if err != nil {
		return ExtensionPrerequisite{}, err
	}

	if !transitionOnly {
		if err := validateCopyGuardPreloadSafety(base, spec, copyDB, library, permanent, standby, released, g); err != nil {
			return ExtensionPrerequisite{}, err
		}
	}

	return ExtensionPrerequisite{
		Name: base.Name, Source: base.Source, Package: base.Package,
		ExtName: base.ExtName, Preload: base.Preload,
	}, nil
}

// selectCopyGuardPreloadDefinition wins the non-transition descriptor for one
// library. A library explained only by outgoing transition descriptors is
// accepted as transition-only.
func selectCopyGuardPreloadDefinition(spec config.ClusterSpec, library string, candidates []catalog.Extension, transitionNames map[string]bool) (catalog.Extension, bool, error) {
	allMatches := compatiblePreloadDefinitions(candidates)
	if len(allMatches) == 0 {
		return catalog.Extension{}, false, output.NewError(output.CodePrecondition,
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
		return catalog.Extension{}, false, output.NewError(output.CodePrecondition,
			"the copy's preload library "+library+" matches definitions with contradictory safety metadata: "+preloadDefinitionNames(matches),
			output.ExitPrecondition,
			"Keep one compatible definition for that library; the copy was not started.").
			WithDetails(map[string]any{"reason": "extension_preload_ambiguous", "library": library, "database": spec.Name})
	}

	return base, transitionOnly, nil
}

// validateCopyGuardPreloadSafety proves the winning descriptor's copy and
// standby declarations against the final guard and permanent configuration.
func validateCopyGuardPreloadSafety(base catalog.Extension, spec config.ClusterSpec, copyDB, library string, permanent map[string]string, standby bool, released map[string]string, g backup.CopyGuard) error {
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
		return reconcileFollowerCopyGuard(ctx, cfg, spec)
	}

	refresh, present, err := prepareCopyGuardRefresh(cfg, spec, dataDir)
	if err != nil || !present {
		return err
	}

	desiredPreloads, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return config.ValidationError(err)
	}

	plan := &backup.CopyPlan{
		Guard: refresh.guard, DesiredPreloads: desiredPreloads,
		ValidateGuard: func(g backup.CopyGuard) error {
			return ValidateCopyGuardPreloads(cfg, spec, spec.DatabaseName(), refresh.known, refresh.permanent, false, refresh.handoff, g)
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
	if backup.CopyGuardCovered(composed, permanentPreloads, refresh.actualPermanent) {
		_, err = backup.RemoveCopyGuard(dataDir)
		return err
	}

	_, err = backup.InstallCopyGuard(dataDir, composed)

	return err
}

// reconcileFollowerCopyGuard refreshes a follower's guard from its complete
// runtime plan. A post-effect preflight snapshot is used only when the live
// catalog cannot be read; the plan is still the complete winning union.
func reconcileFollowerCopyGuard(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	dataDir := cfg.Server.DataDirectory(spec.Name)
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

type copyGuardRefresh struct {
	guard           backup.CopyGuard
	handoff         map[string]string
	known           []catalog.Extension
	permanent       map[string]string
	actualPermanent map[string]string
}

// prepareCopyGuardRefresh reads and updates the durable guard handoff for a
// non-follower's permanent configuration. It writes only the handoff record;
// the guard itself is installed by the caller after validation. An absent
// guard is reported as not present so the caller no-ops, exactly as before.
func prepareCopyGuardRefresh(cfg config.Cluster, spec config.ClusterSpec, dataDir string) (copyGuardRefresh, bool, error) {
	_, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		return copyGuardRefresh{}, false, err
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		return copyGuardRefresh{}, false, err
	}

	current, err := ResolveExtensionSelection(cfg, spec)
	if err != nil {
		return copyGuardRefresh{}, false, err
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
		return copyGuardRefresh{}, false, err
	}

	permanent := copyGuardPermanentSettings(cfg, spec, current)

	actualPermanent, err := backup.EffectivePermanentSettings(PGConfDir(spec.Name), dataDir)
	if err != nil {
		return copyGuardRefresh{}, false, output.NewError(output.CodePrecondition,
			"the permanent configuration's effective precedence for "+spec.Name+" cannot be proven: "+err.Error(),
			output.ExitPrecondition,
			"Fix the cluster's configuration files; vaire will not release a copy suppression without proof.").
			WithDetails(map[string]any{"reason": "extension_permanent_unproven", "database": spec.Name})
	}

	instance, err := guardInstanceIdentity()
	if err != nil {
		return copyGuardRefresh{}, false, output.NewError(output.CodePrecondition,
			"the node instance identity is missing, so "+spec.Name+"'s guard handoff cannot be scoped",
			output.ExitPrecondition, "Repair the node's instance file deliberately; vaire will not inherit unscoped handoff authority.").
			WithDetails(map[string]any{"reason": "guard_instance_unknown", "database": spec.Name})
	}

	known := append(append([]catalog.Extension(nil), current...), retained...)

	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: known, Current: current, Permanent: permanent,
		Explicit: spec.Settings, CopyDB: spec.DatabaseName(),
		Pending: copyGuardFenceActive(recs, spec), Handoff: recs.GuardHandoffFor(spec.Name, instance, spec.Stanza),
		HandoffPermanent: actualPermanent,
	})
	if err != nil {
		return copyGuardRefresh{}, false, err
	}

	base.Settings = decision.Settings

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.SetGuardHandoff(spec.Name, instance, spec.Stanza, decision.Handoff)
		return nil
	}); err != nil {
		return copyGuardRefresh{}, false, err
	}

	return copyGuardRefresh{
		guard: base, handoff: decision.Handoff, known: known,
		permanent: permanent, actualPermanent: actualPermanent,
	}, true, nil
}

func copyGuardPermanentSettings(cfg config.Cluster, spec config.ClusterSpec, current []catalog.Extension) map[string]string {
	permanent := map[string]string{}
	maps.Copy(permanent, cfg.Postgres.Settings)

	for _, ext := range current {
		maps.Copy(permanent, ext.ResolvedSettings(spec.DatabaseName()))
	}

	maps.Copy(permanent, spec.Settings)

	return permanent
}

func copyGuardFenceActive(recs backup.Records, spec config.ClusterSpec) bool {
	f, ok := recs.ActivationFenceFor(spec.Name)
	return ok && f.Active()
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
	// FollowerRuntimeStopped means the target is materialized but stopped. Its
	// pg_extension is unknown, not empty: only persisted guard/history and
	// descriptor metadata are authoritative until the caller's engine-owned
	// start makes the catalog readable. No SQL absence is inferred.
	FollowerRuntimeStopped FollowerRuntimeClass = "stopped"
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
	// follower must have effective: exactly the ordinary settings the renderer
	// writes for the follower's declared extension selection, retained/outgoing
	// obligations, and every mandatory copy overlay. Global/follower settings
	// join only for a key one of those obligations owns. Values are decoded
	// runtime values (descriptor literals decoded once, raw global/spec/
	// follower overrides unchanged). Verification checks every key before any
	// primary configuration or SQL runs. The serialized Guard.Settings map
	// remains config literals.
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

// followerInstalledExtensionSchemas reads the follower's actual installed
// namespaces. The shared LEFT JOIN keeps a known installed row with an
// unreadable namespace visible as unknown instead of absent.
var followerInstalledExtensionSchemas = func(ctx context.Context, spec config.ClusterSpec) (map[string]string, error) {
	return QueryInstalledExtensionSchemas(ctx, ClusterPG(spec), spec.DatabaseName())
}

// SetFollowerInstalledExtensionsForTest replaces the live catalog reader for a
// test in another package and returns the restore function.
func SetFollowerInstalledExtensionsForTest(f func(context.Context, config.ClusterSpec) (map[string]bool, error)) func() {
	old := followerInstalledExtensions
	followerInstalledExtensions = f

	return func() { followerInstalledExtensions = old }
}

// SetFollowerInstalledExtensionSchemasForTest replaces the live namespace
// reader for a test in another package and returns the restore function.
func SetFollowerInstalledExtensionSchemasForTest(f func(context.Context, config.ClusterSpec) (map[string]string, error)) func() {
	old := followerInstalledExtensionSchemas
	followerInstalledExtensionSchemas = f

	return func() { followerInstalledExtensionSchemas = old }
}

// PlanFollowerRuntime resolves one continuously following runtime without
// feeding every historical stanza variant into it. It collects current intent,
// outgoing transition evidence, actual installed SQL and existing guard
// obligations first, then derives one complete runtime union. Current
// inherited descriptors win their previous ordinary runtime variants; only
// genuine retained extras keep strict historical ambiguity checks. Unknown or
// unreadable required facts refuse before any effect.
func PlanFollowerRuntime(ctx context.Context, cfg config.Cluster, follower config.ClusterSpec, req FollowerRuntimeRequest) (FollowerRuntimePlan, error) {
	st, err := newFollowerRuntimeState(cfg, follower, req)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	snapshot, err := st.loadRuntimeFacts(ctx)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	if snapshot != nil {
		return *snapshot, nil
	}

	st.indexCurrentDescriptors()

	if err := st.indexOutgoingDescriptors(); err != nil {
		return FollowerRuntimePlan{}, err
	}

	if err := st.collectRetainedDescriptors(); err != nil {
		return FollowerRuntimePlan{}, err
	}

	kept, retired, err := st.keptExtensions()
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	baseKeys := st.baseKeys()

	decision, permanent, actualPermanent, err := st.guardDecision(kept, baseKeys)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	expected, err := st.expectedSettings(kept, baseKeys, actualPermanent)
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	return st.buildPlan(kept, retired, decision, permanent, expected)
}

// followerRuntimeState stages one follower plan. Its methods follow the
// original fact order: current intent, outgoing transition, installed SQL,
// guard history, then the winning runtime union.
type followerRuntimeState struct {
	cfg      config.Cluster
	follower config.ClusterSpec
	req      FollowerRuntimeRequest
	source   config.ClusterSpec
	class    FollowerRuntimeClass
	db       string
	resolver config.ExtensionResolver
	current  []catalog.Extension

	currentByName    map[string]catalog.Extension
	currentBySQL     map[string]catalog.Extension
	currentByPreload map[string]catalog.Extension
	currentBySetting map[string]catalog.Extension

	outgoingByName    map[string]catalog.Extension
	outgoingBySQL     map[string]catalog.Extension
	outgoingByPreload map[string]catalog.Extension
	outgoingBySetting map[string]catalog.Extension

	retained    map[string]catalog.Extension
	recs        backup.Records
	existing    backup.CopyGuard
	present     bool
	installed   map[string]bool
	sqlComplete bool
	dataDir     string
}

func newFollowerRuntimeState(cfg config.Cluster, follower config.ClusterSpec, req FollowerRuntimeRequest) (*followerRuntimeState, error) {
	if !follower.IsFollower() {
		return nil, output.NewError(output.CodePrecondition,
			follower.Name+" is not a follower, so it has no inherited runtime plan",
			output.ExitPrecondition, "Use the ordinary database configuration path.")
	}

	source, ok := cfg.ClusterByName(follower.Follows)
	if !ok {
		return nil, output.NewError(output.CodePrecondition,
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
	case FollowerRuntimeLive, FollowerRuntimeAwaiting, FollowerRuntimeStopped:
	default:
		return nil, output.NewError(output.CodePrecondition,
			"unknown follower runtime evidence class "+string(class),
			output.ExitPrecondition, "This is a vaire bug; report it with the run log.")
	}

	db := source.DatabaseName()
	resolver := cfg.ExtensionResolver()

	currentNames, err := resolver.EffectiveNames(source.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}

	current, err := resolveExtensionDescriptors(resolver, currentNames)
	if err != nil {
		return nil, config.ValidationError(err)
	}

	return &followerRuntimeState{
		cfg: cfg, follower: follower, req: req, source: source,
		class: class, db: db, resolver: resolver, current: current,
		retained: map[string]catalog.Extension{},
	}, nil
}

// loadRuntimeFacts collects the live catalog and persisted guard evidence. A
// post-effect refresh without a readable catalog returns its complete preflight
// snapshot; any other unknown state refuses before effects.
func (st *followerRuntimeState) loadRuntimeFacts(ctx context.Context) (*FollowerRuntimePlan, error) {
	if err := st.validateLiveSchemas(ctx); err != nil {
		return nil, err
	}
	// Current inherited metadata safety is independent of the preload list and
	// files: every selected descriptor, SQL-only included, must declare
	// compatible copy/standby safety before history, save, package or DROP.
	if _, _, err := backup.MergeCopyRequirements(st.current, nil, true, st.db); err != nil {
		return nil, err
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}

	st.recs = recs
	st.dataDir = st.cfg.Server.DataDirectory(st.follower.Name)

	st.existing, st.present, err = backup.ReadCopyGuard(st.dataDir)
	if err != nil {
		return nil, err
	}

	return st.loadInstalledFacts(ctx)
}

func (st *followerRuntimeState) validateLiveSchemas(ctx context.Context) error {
	if st.class != FollowerRuntimeLive || st.req.AllowUnknownSQL {
		return nil
	}
	// A live follower's actual metadata namespace must agree with every
	// explicitly targeted descriptor schema before history, save,
	// configuration or package effects. Absent SQL may still arrive by primary
	// WAL replay; a known installed row with no readable namespace fails
	// closed. A stopped/awaiting target has no readable catalog here and is
	// rechecked before its own SQL; a post-effect refresh never claims a new
	// preflight.
	schemaEntries := make([]catalog.Extension, 0, len(st.current))
	for _, ext := range st.current {
		if ext.ExtName != "" && ext.Schema != "" {
			schemaEntries = append(schemaEntries, ext)
		}
	}

	if len(schemaEntries) == 0 {
		return nil
	}

	schemas, err := followerInstalledExtensionSchemas(ctx, st.follower)
	if err != nil {
		return output.NewError(output.CodePrecondition,
			"cannot read "+st.follower.Name+"'s installed extension schemas: "+err.Error(),
			output.ExitPrecondition, "Start the follower and retry when its catalog answers; nothing was changed.").
			WithDetails(map[string]any{"reason": "follower_schema_unknown", "database": st.follower.Name})
	}

	return ValidateInstalledExtensionSchemas(st.follower.Name, schemaEntries, schemas)
}

func (st *followerRuntimeState) loadInstalledFacts(ctx context.Context) (*FollowerRuntimePlan, error) {
	if st.class != FollowerRuntimeLive {
		return nil, nil
	}
	// Actual installed SQL facts. A live catalog error is unknown state, not
	// an empty set; only an explicit post-effect refresh may keep the existing
	// obligations without claiming completeness. An awaiting target is
	// metadata-only and never pretends the catalog was queried.
	installed, factsErr := followerInstalledExtensions(ctx, st.follower)
	if factsErr == nil {
		st.installed = installed
		st.sqlComplete = true

		return nil, nil
	}

	if !st.req.AllowUnknownSQL {
		return nil, output.NewError(output.CodePrecondition,
			"cannot read "+st.follower.Name+"'s installed SQL extensions: "+factsErr.Error(),
			output.ExitPrecondition, "Start the follower and retry when its catalog answers; nothing was changed.").
			WithDetails(map[string]any{"reason": "follower_sql_unknown", "database": st.follower.Name})
	}

	if st.req.Snapshot == nil {
		// Reconstructing from the preload guard alone would silently
		// lose a SQL-only retained descriptor or an unguarded
		// mandatory suppression found by preflight. Stop before any
		// guard write, reload, start or restart.
		return nil, output.NewError(output.CodePrecondition,
			"cannot read "+st.follower.Name+"'s installed SQL extensions and no complete preflight runtime plan is available",
			output.ExitPrecondition, "Start the follower and retry when its catalog answers; nothing was changed.").
			WithDetails(map[string]any{"reason": "follower_runtime_snapshot_missing", "database": st.follower.Name})
	}

	plan := followerRuntimePlanFromSnapshot(*st.req.Snapshot)

	return &plan, nil
}

func (st *followerRuntimeState) indexCurrentDescriptors() {
	st.currentByName = map[string]catalog.Extension{}
	st.currentBySQL = map[string]catalog.Extension{}
	st.currentByPreload = map[string]catalog.Extension{}

	st.currentBySetting = map[string]catalog.Extension{}
	for _, ext := range st.current {
		st.currentByName[ext.Name] = ext
		if ext.ExtName != "" {
			st.currentBySQL[ext.ExtName] = ext
		}

		if ext.Preload != "" {
			st.currentByPreload[ext.Preload] = ext
		}

		for k := range ext.ResolvedSettings(st.db) {
			st.currentBySetting[k] = ext
		}

		for k := range ext.ResolvedCopySettings(st.db) {
			st.currentBySetting[k] = ext
		}
	}
}

// indexOutgoingDescriptors keeps the immediate pre-change definitions a
// removal still needs to explain existing guard entries. Current intent always
// wins the same name, SQL extension or library; outgoing is not retained
// history and is retired only after actual SQL absence.
func (st *followerRuntimeState) indexOutgoingDescriptors() error {
	st.outgoingByName = map[string]catalog.Extension{}
	st.outgoingBySQL = map[string]catalog.Extension{}
	st.outgoingByPreload = map[string]catalog.Extension{}

	st.outgoingBySetting = map[string]catalog.Extension{}
	for _, ext := range st.req.Outgoing {
		if ext.Name == "" || st.outgoingSuperseded(ext) {
			continue
		}

		if prev, ok := st.outgoingByName[ext.Name]; ok && !prev.SemanticEqual(ext) {
			return output.NewError(output.CodePrecondition,
				"the outgoing transition has ambiguous metadata for "+ext.Name,
				output.ExitPrecondition, "Keep one compatible outgoing definition; the follower was not changed.").
				WithDetails(map[string]any{"reason": "follower_outgoing_ambiguous", "database": st.follower.Name, "extension": ext.Name})
		}

		ext = ext.Clone()

		st.outgoingByName[ext.Name] = ext
		if ext.ExtName != "" {
			st.outgoingBySQL[ext.ExtName] = ext
		}

		if ext.Preload != "" {
			st.outgoingByPreload[ext.Preload] = ext
		}

		for k := range ext.ResolvedSettings(st.db) {
			st.outgoingBySetting[k] = ext
		}

		for k := range ext.ResolvedCopySettings(st.db) {
			st.outgoingBySetting[k] = ext
		}
	}

	return nil
}

// outgoingSuperseded reports whether current intent already owns this
// descriptor's name or SQL extension.
func (st *followerRuntimeState) outgoingSuperseded(ext catalog.Extension) bool {
	if _, ok := st.currentByName[ext.Name]; ok {
		return true
	}

	if ext.ExtName == "" {
		return false
	}

	_, ok := st.currentBySQL[ext.ExtName]

	return ok
}

// collectRetainedDescriptors gathers obligations from actual installed SQL and
// existing guard metadata. Current or outgoing evidence wins before history is
// consulted, so an ordinary update cannot become permanently ambiguous.
func (st *followerRuntimeState) collectRetainedDescriptors() error {
	if err := st.retainInstalledSQL(); err != nil {
		return err
	}

	return st.retainGuardObligations()
}

func (st *followerRuntimeState) retainInstalledSQL() error {
	if !st.sqlComplete {
		return nil
	}

	for sqlName := range st.installed {
		if sqlName == "plpgsql" {
			continue
		}

		if _, ok := st.currentBySQL[sqlName]; ok {
			continue
		}

		if _, ok := st.outgoingBySQL[sqlName]; ok {
			continue
		}

		ext, err := retainedDescriptor(st.resolver, st.recs, st.source.Stanza, sqlName, "", "")
		if err != nil {
			return err
		}

		if err := st.addRetained(ext); err != nil {
			return err
		}
	}

	return nil
}

func (st *followerRuntimeState) retainGuardObligations() error {
	if !st.present {
		return nil
	}

	for _, lib := range st.existing.Preloads {
		if _, ok := st.currentByPreload[lib]; ok {
			continue
		}

		if _, ok := st.outgoingByPreload[lib]; ok {
			continue
		}

		ext, err := retainedDescriptor(st.resolver, st.recs, st.source.Stanza, "", lib, "")
		if err != nil {
			return err
		}

		if err := st.addRetained(ext); err != nil {
			return err
		}
	}

	for k := range st.existing.Settings {
		if _, ok := st.currentBySetting[k]; ok {
			continue
		}

		if _, ok := st.outgoingBySetting[k]; ok {
			continue
		}

		ext, err := retainedDescriptor(st.resolver, st.recs, st.source.Stanza, "", "", k)
		if err != nil {
			return err
		}

		if err := st.addRetained(ext); err != nil {
			return err
		}
	}

	return nil
}

func (st *followerRuntimeState) addRetained(ext catalog.Extension) error {
	if ext.Name == "" {
		return nil
	}

	if _, ok := st.currentByName[ext.Name]; ok {
		return nil
	}

	if ext.ExtName != "" {
		if _, ok := st.currentBySQL[ext.ExtName]; ok {
			return nil
		}
	}

	if _, ok := st.outgoingByName[ext.Name]; ok {
		return nil
	}

	if ext.ExtName != "" {
		if _, ok := st.outgoingBySQL[ext.ExtName]; ok {
			return nil
		}
	}

	if prev, ok := st.retained[ext.Name]; ok {
		if !prev.SemanticEqual(ext) {
			return output.NewError(output.CodePrecondition,
				"follower "+st.follower.Name+" has ambiguous retained metadata for "+ext.Name,
				output.ExitPrecondition, "Keep one compatible retained definition; the follower was not changed.").
				WithDetails(map[string]any{"reason": "follower_retained_ambiguous", "database": st.follower.Name, "extension": ext.Name})
		}

		return nil
	}

	st.retained[ext.Name] = ext.Clone()

	return nil
}

// keptExtensions retires a non-current SQL descriptor only when the actual
// follower catalog proves its SQL absent. A preload-only descriptor has no SQL
// absence proof and stays. Retired libraries/settings remain if another kept
// descriptor still requires them.
func (st *followerRuntimeState) keptExtensions() ([]catalog.Extension, map[string]catalog.Extension, error) {
	all := append(append([]catalog.Extension(nil), st.current...), valuesOf(st.retained)...)
	all = append(all, valuesOf(st.outgoingByName)...)
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].Name != all[j].Name {
			return all[i].Name < all[j].Name
		}

		return all[i].ExtName < all[j].ExtName
	})
	kept := make([]catalog.Extension, 0, len(all))
	retired := map[string]catalog.Extension{}

	for _, ext := range all {
		_, isCurrent := st.currentByName[ext.Name]
		if st.req.RetireAbsent && st.sqlComplete && !isCurrent && ext.ExtName != "" && !st.installed[ext.ExtName] {
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
		if _, isOutgoing := st.outgoingByName[ext.Name]; isOutgoing {
			continue
		}

		safety = append(safety, ext)
	}

	_, _, err := backup.MergeCopyRequirements(safety, nil, true, st.db)

	return kept, retired, err
}

// baseKeys are the ordinary runtime keys current intent or an explicit
// operator setting already owns.
func (st *followerRuntimeState) baseKeys() map[string]bool {
	baseKeys := map[string]bool{}
	for k := range st.cfg.Postgres.Settings {
		baseKeys[k] = true
	}

	for _, ext := range st.current {
		for k := range ext.ResolvedSettings(st.db) {
			baseKeys[k] = true
		}

		for k := range ext.ResolvedCopySettings(st.db) {
			baseKeys[k] = true
		}
	}

	for k := range st.follower.Settings {
		baseKeys[k] = true
	}

	return baseKeys
}

// guardDecision overlays the kept descriptors and the actual permanent
// configuration into one guard assignment set.
func (st *followerRuntimeState) guardDecision(kept []catalog.Extension, baseKeys map[string]bool) (backup.CopyGuardSettingsDecision, map[string]string, map[string]string, error) {
	guardKnown := make([]catalog.Extension, 0, len(kept))
	for _, ext := range kept {
		if _, isOutgoing := st.outgoingByName[ext.Name]; isOutgoing {
			ext = filterTransitionOrdinary(ext, baseKeys)
		}

		guardKnown = append(guardKnown, ext)
	}

	actualPermanent, err := backup.EffectivePermanentSettings(PGConfDir(st.follower.Name), st.dataDir)
	if err != nil {
		return backup.CopyGuardSettingsDecision{}, nil, nil, output.NewError(output.CodePrecondition,
			"follower "+st.follower.Name+"'s permanent configuration cannot be proven: "+err.Error(),
			output.ExitPrecondition, "Fix its configuration; the follower was not changed.").
			WithDetails(map[string]any{"reason": "follower_permanent_unproven", "database": st.follower.Name})
	}

	permanent := map[string]string{}
	maps.Copy(permanent, actualPermanent)

	for _, ext := range st.current {
		for k, v := range ext.ResolvedSettings(st.db) {
			if _, ok := permanent[k]; !ok {
				permanent[k] = v
			}
		}
	}

	decision, err := backup.CopyGuardSettings(backup.CopyGuardSettingsOptions{
		Known: guardKnown, Current: st.current, Permanent: permanent,
		Explicit: st.follower.Settings, CopyDB: st.db, Pending: true,
	})
	if err != nil {
		return backup.CopyGuardSettingsDecision{}, nil, nil, err
	}

	return decision, permanent, actualPermanent, nil
}

// expectedSettings is the complete ordinary-plus-mandatory-copy overlay the
// follower must have effective. Only explicitly declared descriptors write
// ordinary settings into 30-vaire-extensions.conf; retained/outgoing
// obligations add their values after rendered current intent.
func (st *followerRuntimeState) expectedSettings(kept []catalog.Extension, baseKeys map[string]bool, actualPermanent map[string]string) (map[string]string, error) {
	rendered, err := st.resolver.Enabled(st.follower.Extensions)
	if err != nil {
		return nil, config.ValidationError(err)
	}

	expected := map[string]string{}

	for _, ext := range rendered {
		for k, literal := range ext.ResolvedSettings(st.db) {
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}

	permanentDecoded := st.decodedPermanent(actualPermanent)
	st.addRetainedExpectedSettings(expected, permanentDecoded, kept, baseKeys)
	// Renderer order after 30-vaire-extensions.conf is 50-vaire-settings.conf:
	// setup-wide settings followed by the database's explicit settings. Apply
	// that precedence only to keys an extension obligation owns.
	for k := range expected {
		if raw, ok := st.cfg.Postgres.Settings[k]; ok {
			expected[k] = raw
		}
	}

	for k := range expected {
		if raw, ok := st.follower.Settings[k]; ok {
			expected[k] = raw
		}
	}
	// Mandatory copy settings live in postgresql.auto.conf, after the managed
	// configuration, so they overlay every ordinary or explicit value.
	for _, ext := range kept {
		if ext.CopySafety != catalog.CopySafetyDisabled {
			continue
		}

		for k, literal := range ext.ResolvedCopySettings(st.db) {
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}

	return expected, nil
}

// decodedPermanent is permanent precedence in decoded runtime form. The
// guard's Permanent map may still carry literals for its own decisions.
func (st *followerRuntimeState) decodedPermanent(actualPermanent map[string]string) map[string]string {
	permanentDecoded := map[string]string{}
	maps.Copy(permanentDecoded, actualPermanent)

	for _, ext := range st.current {
		for k, literal := range ext.ResolvedSettings(st.db) {
			if _, ok := permanentDecoded[k]; !ok {
				permanentDecoded[k] = catalog.NormalizedSettingValue(literal)
			}
		}
	}

	return permanentDecoded
}

func (st *followerRuntimeState) addRetainedExpectedSettings(expected, permanentDecoded map[string]string, kept []catalog.Extension, baseKeys map[string]bool) {
	for _, ext := range kept {
		if _, isCurrent := st.currentByName[ext.Name]; isCurrent {
			continue
		}

		st.addRetainedExtensionSettings(expected, permanentDecoded, ext, baseKeys)
	}
}

func (st *followerRuntimeState) addRetainedExtensionSettings(expected, permanentDecoded map[string]string, ext catalog.Extension, baseKeys map[string]bool) {
	view := ext
	if _, isOutgoing := st.outgoingByName[ext.Name]; isOutgoing {
		view = filterTransitionOrdinary(ext, baseKeys)
	}

	mandatory := map[string]string{}

	if view.CopySafety == catalog.CopySafetyDisabled {
		for k, literal := range view.ResolvedCopySettings(st.db) {
			mandatory[k] = catalog.NormalizedSettingValue(literal)
		}
	}

	for k, literal := range view.ResolvedSettings(st.db) {
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

func (st *followerRuntimeState) buildPlan(kept []catalog.Extension, retired map[string]catalog.Extension, decision backup.CopyGuardSettingsDecision, permanent, expected map[string]string) (FollowerRuntimePlan, error) {
	currentPreloads, err := st.resolver.Preloads(st.source.Extensions)
	if err != nil {
		return FollowerRuntimePlan{}, config.ValidationError(err)
	}

	base := backup.CopyGuard{Preloads: st.retainedPreloads(kept), Settings: decision.Settings}

	composed, err := backup.ComposeCopyGuardDropping(PGConfDir(st.follower.Name), st.dataDir, base, currentPreloads, st.droppedPreloads(kept, retired))
	if err != nil {
		return FollowerRuntimePlan{}, err
	}

	keptNonCurrent, keptOutgoing := st.keptTransitions(kept)

	return FollowerRuntimePlan{
		Guard: composed, Requirements: kept, Preloads: composed.Preloads,
		Settings: composed.Settings, ExpectedSettings: expected, Cron: hasExtensionCron(kept),
		Retained: keptNonCurrent, Outgoing: keptOutgoing, Permanent: permanent,
		SQLFactsComplete: st.sqlComplete, PendingSQL: sortedKeys(st.pendingSQL(kept)),
	}, nil
}

func (st *followerRuntimeState) retainedPreloads(kept []catalog.Extension) []string {
	retainedPreloads := make([]string, 0, len(kept))
	seen := map[string]bool{}

	for _, ext := range kept {
		if _, isCurrent := st.currentByName[ext.Name]; isCurrent {
			continue
		}

		if ext.Preload != "" && !seen[ext.Preload] {
			seen[ext.Preload] = true
			retainedPreloads = append(retainedPreloads, ext.Preload)
		}
	}

	return retainedPreloads
}

func (st *followerRuntimeState) droppedPreloads(kept []catalog.Extension, retired map[string]catalog.Extension) map[string]bool {
	keptPreloads := map[string]bool{}

	for _, ext := range kept {
		if ext.Preload != "" {
			keptPreloads[ext.Preload] = true
		}
	}

	dropped := map[string]bool{}

	for _, ext := range retired {
		if ext.Preload != "" && !keptPreloads[ext.Preload] {
			dropped[ext.Preload] = true
		}
	}

	return dropped
}

func (st *followerRuntimeState) pendingSQL(kept []catalog.Extension) map[string]bool {
	pending := map[string]bool{}
	if !st.req.RetireAbsent || !st.sqlComplete {
		return pending
	}

	for _, ext := range kept {
		if _, isOutgoing := st.outgoingByName[ext.Name]; !isOutgoing {
			// A genuinely retained extra is not a pending removal. Only
			// the explicitly outgoing descriptor may hold a transition
			// open while its SQL still replays.
			continue
		}

		if ext.ExtName != "" && st.installed[ext.ExtName] {
			pending[ext.ExtName] = true
		}
	}

	return pending
}

func hasExtensionCron(kept []catalog.Extension) bool {
	for _, ext := range kept {
		if ext.Name == "pg_cron" || ext.ExtName == "pg_cron" {
			return true
		}
	}

	return false
}

func (st *followerRuntimeState) keptTransitions(kept []catalog.Extension) ([]catalog.Extension, []catalog.Extension) {
	keptNonCurrent := make([]catalog.Extension, 0, len(kept))

	keptOutgoing := make([]catalog.Extension, 0, len(st.outgoingByName))
	for _, ext := range kept {
		if _, isCurrent := st.currentByName[ext.Name]; isCurrent {
			continue
		}

		keptNonCurrent = append(keptNonCurrent, ext)
		if _, isOutgoing := st.outgoingByName[ext.Name]; isOutgoing {
			keptOutgoing = append(keptOutgoing, ext)
		}
	}

	return keptNonCurrent, keptOutgoing
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
	maps.Copy(out, m)

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
	found := retainedDescriptorMatches(recs.RetainedExtensions(stanza), sqlName, preload, setting)
	if len(found) == 0 {
		list, err := resolver.List()
		if err != nil {
			return catalog.Extension{}, config.ValidationError(err)
		}

		found = retainedDescriptorMatches(list, sqlName, preload, setting)
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
		what := retainedDescriptorName(sqlName, preload, setting)

		return catalog.Extension{}, output.NewError(output.CodePrecondition,
			"the follower's retained data requires "+what+", which no definition explains",
			output.ExitPrecondition, "Declare it in [extensions.<name>] with copy_safety and standby_safe; the follower was not changed.").
			WithDetails(map[string]any{"reason": "follower_retained_unknown", "extension": what})
	}

	if len(unique) > 1 {
		what := retainedDescriptorName(sqlName, preload, setting)

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

func retainedDescriptorMatches(exts []catalog.Extension, sqlName, preload, setting string) []catalog.Extension {
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

func retainedDescriptorName(sqlName, preload, setting string) string {
	if sqlName != "" {
		return sqlName
	}

	if preload != "" {
		return "library " + preload
	}

	return "setting " + setting
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

	if !slices.Equal(installed.Preloads, plan.Guard.Preloads) {
		return true
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

// guardInstanceIdentity reads the trimmed installation identity and refuses
// an empty one.
func guardInstanceIdentity() (string, error) {
	instance, err := guardInstance()
	if err != nil {
		return "", err
	}

	instance = strings.TrimSpace(instance)
	if instance == "" {
		return "", errors.New("empty instance identity")
	}

	return instance, nil
}

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
		return refuseUnloadWithoutCatalog(cfg, spec, firstConfig)
	}

	installed := map[string]bool{}
	for _, row := range installedRows {
		installed[row["name"]] = true
	}

	desired, err := unloadDesiredExtensions(cfg, spec)
	if err != nil {
		return err
	}

	loaded, err := LoadedPreloads(ctx, ClusterPG(spec))
	if err != nil {
		return refuseUnloadWithoutCatalog(cfg, spec, firstConfig)
	}

	keep, defs, err := unloadRenderedDefinitions(cfg, spec)
	if err != nil {
		return err
	}

	return refuseLoadedExtensionUnload(spec, installed, desired, keep, loaded, defs)
}

// refuseUnloadWithoutCatalog allows a first managed configuration and otherwise
// judges the stopped or unreadable cluster from its last managed files.
func refuseUnloadWithoutCatalog(cfg config.Cluster, spec config.ClusterSpec, firstConfig bool) error {
	if firstConfig {
		return nil
	}

	return refuseUnloadFromManagedFiles(cfg, spec)
}

func unloadDesiredExtensions(cfg config.Cluster, spec config.ClusterSpec) (map[string]bool, error) {
	entries, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		return nil, err
	}

	desired := map[string]bool{}
	for _, ext := range entries {
		desired[ext.ExtName] = true
	}

	return desired, nil
}

func unloadRenderedDefinitions(cfg config.Cluster, spec config.ClusterSpec) (map[string]bool, map[string]catalog.Extension, error) {
	rendered, err := cfg.ExtensionResolver().Preloads(spec.Extensions)
	if err != nil {
		return nil, nil, err
	}

	keep := map[string]bool{}
	for _, p := range rendered {
		keep[p] = true
	}

	defs := map[string]catalog.Extension{}

	list, err := cfg.ExtensionResolver().List()
	if err != nil {
		return nil, nil, err
	}

	for _, ext := range list {
		if ext.ExtName != "" {
			defs[ext.ExtName] = ext
		}
	}

	return keep, defs, nil
}

// refuseLoadedExtensionUnload refuses to unload a library that still backs an
// installed SQL extension the new configuration no longer desires.
func refuseLoadedExtensionUnload(spec config.ClusterSpec, installed, desired, keep, loaded map[string]bool, defs map[string]catalog.Extension) error {
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
						"Declare it in [extensions.<name>] with copy/standby safety and keep its library configured; this release does not remove extensions. This configuration was not applied; earlier steps may have run.").
						WithDetails(map[string]any{"reason": "extension_remove_required", "database": spec.Name, "extension": name, "library": p})
				}
			}

			continue
		}

		if ext.Preload != "" && loaded[ext.Preload] && !keep[ext.Preload] {
			return output.NewError(output.CodePrecondition,
				"applying this configuration would unload "+ext.Preload+" while "+name+"'s SQL is still installed in "+spec.Name,
				output.ExitPrecondition,
				"Add it to extensions or keep its preload configured; this release does not remove extensions. This configuration was not applied; earlier steps may have run.").
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
					"Restore "+managedExtensionsConfPath(spec.Name)+", or start the cluster so vaire can read pg_extension. This configuration was not applied; earlier steps may have run.").
					WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": spec.Name})
			}
			// A new cluster has no last configuration to unload.
			return nil
		}

		return output.NewError(output.CodePrecondition,
			"vaire cannot read "+spec.Name+"'s last managed extension configuration to prove its preloads safe: "+err.Error(),
			output.ExitPrecondition,
			"Fix access to "+managedExtensionsConfPath(spec.Name)+", or start the cluster so vaire can read pg_extension. This configuration was not applied; earlier steps may have run.").
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
			"Start "+spec.Name+" and retry so vaire can read pg_extension, or keep its preload configured; this release does not remove extensions. This configuration was not applied; earlier steps may have run.").
			WithDetails(map[string]any{"reason": "extension_unload_unproven", "database": spec.Name, "library": p})
	}

	return nil
}

// managedPreloads reads the shared_preload_libraries value from a managed
// 30-vaire-extensions.conf. A missing setting is an empty list.
func managedPreloads(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	value := ""

	for line := range strings.SplitSeq(string(raw), "\n") {
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

	for part := range strings.SplitSeq(value, ",") {
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
	if err := CheckExtensionPackageAuthority(reqs, rt.TrustedHostAdmin); err != nil {
		return err
	}

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

	var (
		b       strings.Builder
		created []string
	)

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

	for part := range strings.SplitSeq(value, ",") {
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

	if slices.Contains(pending, "shared_preload_libraries") {
		return true, nil
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

	var (
		b        strings.Builder
		deferred []string
	)

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
// extension's own setting keys on a primary, matching the renderer's load
// order. An explicitly selected extension contributes its descriptor defaults
// from 30-vaire-extensions.conf, then the setup-wide [postgres.settings], then
// the database's explicit [clusters.settings] from 50-vaire-settings.conf.
// Copy overlays do not apply to a primary.
//
// Only an extension explicitly selected for the database, by canonical name
// or built-in alias, has its descriptor settings rendered. The implicit
// default modules still contribute their preloads, packages and SQL, but their
// unrendered descriptor defaults are not effective expectations. A setup-wide
// or database setting is still rendered by 50-vaire-settings.conf, so it
// overrides an extension-owned key even when the extension is only implicit.
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

	resolver := cfg.ExtensionResolver()
	explicit := extensionExplicitlySelected(resolver, spec, ext.Name)

	expected := make(map[string]string, len(ordinary))
	if explicit {
		for k, literal := range ordinary {
			expected[k] = catalog.NormalizedSettingValue(literal)
		}
	}

	for k := range ordinary {
		if _, ok := expected[k]; ok {
			continue
		}

		if raw, ok := cfg.Postgres.Settings[k]; ok {
			expected[k] = raw
			continue
		}

		if raw, ok := spec.Settings[k]; ok {
			expected[k] = raw
		}
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

// extensionExplicitlySelected reports whether name is selected by canonical
// name or built-in alias. Only an explicit selection renders descriptor
// settings.
func extensionExplicitlySelected(resolver config.ExtensionResolver, spec config.ClusterSpec, name string) bool {
	for _, selected := range spec.Extensions {
		if canonical, ok := resolver.Canonical(selected); ok && canonical == name {
			return true
		}
	}

	return false
}

// ExtensionSettingFact is one effective PostgreSQL setting value together with
// its pg_settings type and unit. An empty Type means PostgreSQL did not report
// a type (a custom placeholder), so the value is compared exactly. An empty
// Unit means the setting has no unit or the caller did not read it; integer
// and real comparisons then use the plain value.
type ExtensionSettingFact struct {
	Value string
	Type  string
	Unit  string
}

// ExtensionSettingFactSQL reads one required setting's effective value, type
// and unit. current_setting is the authority for the value; pg_settings
// supplies the type and unit only when PostgreSQL knows the setting. A
// placeholder that is not in pg_settings still reports its current_setting
// value with empty type and unit.
func ExtensionSettingFactSQL(key string) string {
	k := literal(key)
	return "SELECT current_setting(" + k + ", true) AS v, coalesce((SELECT vartype FROM pg_settings WHERE name = " + k + "), '') AS vartype, coalesce((SELECT unit FROM pg_settings WHERE name = " + k + "), '') AS unit"
}

// ParseExtensionSettingFact reads ExtensionSettingFactSQL's one row.
func ParseExtensionSettingFact(row map[string]string) ExtensionSettingFact {
	return ExtensionSettingFact{Value: row["v"], Type: row["vartype"], Unit: row["unit"]}
}

// ExtensionSettingMatches compares an expected decoded setting value with an
// actual fact. PostgreSQL's own type determines the comparison:
//
//   - bool uses PostgreSQL's boolean input forms and their unique prefixes;
//   - enum is case-insensitive, because PostgreSQL canonicalises GUC enum
//     values to lowercase;
//   - integer and real compare numerically, including PostgreSQL unit
//     suffixes (for example 1MB and 1024kB) when pg_settings reports the unit;
//   - string, an unknown type and an unrecognised unit are compared exactly,
//     so quote content and arbitrary text are never normalized.
//
// The expected value is already decoded once by the caller. This function
// never strips another quote layer, trims whitespace or guesses a type.
func ExtensionSettingMatches(want string, fact ExtensionSettingFact) bool {
	switch fact.Type {
	case "bool":
		w, ok := postgresBool(want)
		if !ok {
			return false
		}

		got, ok := postgresBool(fact.Value)
		if !ok {
			return false
		}

		return w == got
	case "enum":
		return strings.EqualFold(want, fact.Value)
	case "integer":
		return extensionNumericMatches(want, fact, true)
	case "real":
		return extensionNumericMatches(want, fact, false)
	}

	return fact.Value == want
}

func extensionNumericMatches(want string, fact ExtensionSettingFact, integer bool) bool {
	if fact.Unit != "" {
		w, wantOK := postgresUnitValue(want, fact.Unit)

		got, gotOK := postgresUnitValue(fact.Value, fact.Unit)
		if wantOK && gotOK {
			return w == got
		}
		// An unknown unit or an unparseable value is compared exactly rather
		// than guessed.
		return fact.Value == want
	}

	if integer {
		w, wantErr := strconv.ParseInt(want, 10, 64)

		got, gotErr := strconv.ParseInt(fact.Value, 10, 64)
		if wantErr == nil && gotErr == nil {
			return w == got
		}

		return fact.Value == want
	}

	w, wantErr := strconv.ParseFloat(want, 64)

	got, gotErr := strconv.ParseFloat(fact.Value, 64)
	if wantErr == nil && gotErr == nil {
		return w == got
	}

	return fact.Value == want
}

// postgresUnitValue converts a PostgreSQL GUC value to the base unit named by
// the pg_settings unit column. A bare number is in that base unit; a suffix
// selects its own unit and must belong to the same category. Unknown units
// return false instead of guessing.
func postgresUnitValue(value, unit string) (float64, bool) {
	base, category, ok := postgresUnitBase(unit)
	if !ok {
		return 0, false
	}

	number, suffix, ok := splitPostgresUnitValue(value)
	if !ok {
		return 0, false
	}

	if suffix == "" {
		return number * base, true
	}

	factor, suffixCategory, ok := postgresUnitSuffix(suffix)
	if !ok || suffixCategory != category {
		return 0, false
	}

	return number * factor, true
}

func splitPostgresUnitValue(value string) (float64, string, bool) {
	end := len(value)
	for end > 0 {
		c := value[end-1]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			end--
			continue
		}

		break
	}

	number, err := strconv.ParseFloat(value[:end], 64)
	if err != nil {
		return 0, "", false
	}

	return number, strings.ToLower(value[end:]), true
}

type postgresUnitCategory int

const (
	postgresUnitBytes postgresUnitCategory = iota + 1
	postgresUnitTime
)

func postgresUnitBase(unit string) (float64, postgresUnitCategory, bool) {
	switch strings.ToLower(unit) {
	case "b":
		return 1, postgresUnitBytes, true
	case "kb":
		return 1 << 10, postgresUnitBytes, true
	case "mb":
		return 1 << 20, postgresUnitBytes, true
	case "gb":
		return 1 << 30, postgresUnitBytes, true
	case "tb":
		return 1 << 40, postgresUnitBytes, true
	case "8kb":
		return 8 << 10, postgresUnitBytes, true
	case "16kb":
		return 16 << 10, postgresUnitBytes, true
	case "32kb":
		return 32 << 10, postgresUnitBytes, true
	case "64kb":
		return 64 << 10, postgresUnitBytes, true
	case "us":
		return 1, postgresUnitTime, true
	case "ms":
		return 1_000, postgresUnitTime, true
	case "s":
		return 1_000_000, postgresUnitTime, true
	case "min":
		return 60_000_000, postgresUnitTime, true
	case "h":
		return 3_600_000_000, postgresUnitTime, true
	case "d":
		return 86_400_000_000, postgresUnitTime, true
	}

	return 0, 0, false
}

func postgresUnitSuffix(suffix string) (float64, postgresUnitCategory, bool) {
	switch suffix {
	case "b":
		return 1, postgresUnitBytes, true
	case "kb":
		return 1 << 10, postgresUnitBytes, true
	case "mb":
		return 1 << 20, postgresUnitBytes, true
	case "gb":
		return 1 << 30, postgresUnitBytes, true
	case "tb":
		return 1 << 40, postgresUnitBytes, true
	case "us":
		return 1, postgresUnitTime, true
	case "ms":
		return 1_000, postgresUnitTime, true
	case "s":
		return 1_000_000, postgresUnitTime, true
	case "min":
		return 60_000_000, postgresUnitTime, true
	case "h":
		return 3_600_000_000, postgresUnitTime, true
	case "d":
		return 86_400_000_000, postgresUnitTime, true
	}

	return 0, 0, false
}

// postgresBool accepts PostgreSQL's boolean input forms and their unique
// prefixes: true (t, tr, tru, true), yes (y, ye, yes), on, 1, false (f, fa,
// fal, fals, false), no (n, no), off (of, off) and 0. "o" is not a boolean
// because it prefixes both on and off. It does not trim or strip quotes: GUC
// input does not tolerate surrounding whitespace, and malformed or
// quote-content values are not booleans.
func postgresBool(value string) (bool, bool) {
	switch strings.ToLower(value) {
	case "t", "tr", "tru", "true":
		return true, true
	case "y", "ye", "yes":
		return true, true
	case "on":
		return true, true
	case "1":
		return true, true
	case "f", "fa", "fal", "fals", "false":
		return false, true
	case "n", "no":
		return false, true
	case "of", "off":
		return false, true
	case "0":
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

	var (
		b        strings.Builder
		deferred []extensionSQLDeferral
	)

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
