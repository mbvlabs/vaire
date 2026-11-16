package config

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/mbvlabs/vaire/internal/catalog"
)

// ExtensionDefinition is one [extensions.<name>] entry: an operator-supplied
// descriptor for an extension the built-in catalogue does not know, or an
// explicit provider override of a built-in entry. The key is the canonical
// name. A descriptor never carries SQL hooks, grants, shell commands or
// repository credentials.
type ExtensionDefinition struct {
	// ExtName is the CREATE EXTENSION name; empty defaults to the key. A
	// built-in override inherits the built-in name.
	ExtName string `toml:"ext_name,omitempty" json:"ext_name,omitempty"`
	// Source is apt or preinstalled. Required for a new definition.
	Source string `toml:"source,omitempty" json:"source,omitempty"`
	// Package is required for apt and must be empty for preinstalled.
	Package string `toml:"package,omitempty" json:"package,omitempty"`
	// Preload is the shared_preload_libraries value; empty means none.
	Preload string `toml:"preload,omitempty" json:"preload,omitempty"`
	// Settings are rendered into the cluster's extension configuration.
	Settings map[string]string `toml:"settings,omitempty" json:"settings,omitempty"`
	// Schema is the schema the extension is created in; empty means public.
	Schema string `toml:"schema,omitempty" json:"schema,omitempty"`
	// Note is human documentation and never changes runtime intent.
	Note string `toml:"note,omitempty" json:"note,omitempty"`
	// CopySafety is unknown, safe or disabled. Nil inherits a built-in
	// entry's declaration; a new definition defaults to unknown.
	CopySafety *string `toml:"copy_safety,omitempty" json:"copy_safety,omitempty"`
	// CopySettings suppress background effects in a copy when copy_safety
	// is disabled.
	CopySettings map[string]string `toml:"copy_settings,omitempty" json:"copy_settings,omitempty"`
	// StandbySafe declares that the extension may load on a physical
	// standby. Nil inherits a built-in entry's declaration; a new
	// definition defaults to false.
	StandbySafe *bool `toml:"standby_safe,omitempty" json:"standby_safe,omitempty"`
}

// Clone returns a deep copy of the descriptor.
func (d ExtensionDefinition) Clone() ExtensionDefinition {
	out := d
	out.Settings = cloneStringMap(d.Settings)
	out.CopySettings = cloneStringMap(d.CopySettings)
	if d.CopySafety != nil {
		v := *d.CopySafety
		out.CopySafety = &v
	}
	if d.StandbySafe != nil {
		v := *d.StandbySafe
		out.StandbySafe = &v
	}
	return out
}

// ExtensionResolver resolves canonical extension names against the built-in
// catalogue and the cluster's [extensions] definitions. It is the one
// resolver every configuration, rendering, roles, operations and recovery path
// uses; unknown names are errors, never silently skipped.
type ExtensionResolver struct{ cluster Cluster }

// ExtensionResolver returns a resolver for c's built-in and custom entries.
func (c Cluster) ExtensionResolver() ExtensionResolver { return ExtensionResolver{cluster: c} }

// Resolve returns the canonical descriptor called name, merging a custom
// override with its built-in defaults. An alias resolves to its canonical
// built-in name. Invalid descriptors and unknown names are errors.
func (r ExtensionResolver) Resolve(name string) (catalog.Extension, error) {
	resolved, problems := r.resolve(name)
	if len(problems) > 0 {
		return catalog.Extension{}, errors.New(strings.Join(problems, "; "))
	}
	return resolved, nil
}

// Lookup resolves name when it is valid.
func (r ExtensionResolver) Lookup(name string) (catalog.Extension, bool) {
	resolved, err := r.Resolve(name)
	return resolved, err == nil
}

// Canonical returns name's canonical name: a built-in alias resolves to the
// built-in Name, a custom key is already canonical. Unknown names come back
// unchanged with ok false.
func (r ExtensionResolver) Canonical(name string) (string, bool) {
	if builtin, ok := catalog.Lookup(name); ok {
		return builtin.Name, true
	}
	if _, ok := r.cluster.Extensions[name]; ok {
		return name, true
	}
	return name, false
}

// List returns every built-in entry (with overrides applied) in catalogue
// order, then every new custom definition sorted by name.
func (r ExtensionResolver) List() ([]catalog.Extension, error) {
	var out []catalog.Extension
	for _, name := range catalog.Names() {
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, ext)
	}
	for _, name := range r.customNames() {
		if _, builtin := catalog.Lookup(name); builtin {
			continue
		}
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		out = append(out, ext)
	}
	return out, nil
}

// Defaults returns the built-in names enabled on init.
func (r ExtensionResolver) Defaults() []string { return catalog.Defaults() }

// Enabled resolves names in order, canonicalised and without duplicates.
func (r ExtensionResolver) Enabled(names []string) ([]catalog.Extension, error) {
	var out []catalog.Extension
	seen := map[string]bool{}
	for _, name := range names {
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		if seen[ext.Name] {
			continue
		}
		seen[ext.Name] = true
		out = append(out, ext)
	}
	return out, nil
}

// EffectiveNames is the selection every runtime path uses: the implicit
// default modules followed by the named extensions, canonicalised and without
// duplicates. Preloads, Packages and step inputs must not resolve different
// selections.
func (r ExtensionResolver) EffectiveNames(names []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, name := range append(catalog.Defaults(), names...) {
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		if seen[ext.Name] {
			continue
		}
		seen[ext.Name] = true
		out = append(out, ext.Name)
	}
	return out, nil
}

// Preloads returns the shared_preload_libraries values for the enabled
// extensions. Built-in entries keep catalogue order, new custom definitions
// follow sorted by name, and the default modules are always preloaded.
func (r ExtensionResolver) Preloads(names []string) ([]string, error) {
	effective, err := r.EffectiveNames(names)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, name := range effective {
		want[name] = true
	}
	order := append(catalog.Names(), r.customNames()...)
	var out []string
	seen := map[string]bool{}
	for _, name := range order {
		builtin, isBuiltin := catalog.Lookup(name)
		if !isBuiltin || builtin.Name != name {
			// A new custom entry is considered only when enabled.
			if !want[name] {
				continue
			}
		}
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		if ext.Preload == "" || (!ext.Default && !want[ext.Name]) || seen[ext.Preload] {
			continue
		}
		seen[ext.Preload] = true
		out = append(out, ext.Preload)
	}
	return out, nil
}

// PreloadChanges reports whether going from before to after changes
// shared_preload_libraries.
func (r ExtensionResolver) PreloadChanges(before, after []string) (bool, error) {
	old, err := r.Preloads(before)
	if err != nil {
		return false, err
	}
	next, err := r.Preloads(after)
	if err != nil {
		return false, err
	}
	return !slices.Equal(old, next), nil
}

// Packages returns the APT packages the effective selection needs, sorted and
// without duplicates. The selection includes the implicit default modules, so
// an APT provider override of auto_explain or pg_stat_statements contributes
// its package even when no database names it. Preinstalled entries need none,
// which keeps an unchanged empty package union empty.
func (r ExtensionResolver) Packages(names []string) ([]string, error) {
	effective, err := r.EffectiveNames(names)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var out []string
	for _, name := range effective {
		ext, err := r.Resolve(name)
		if err != nil {
			return nil, err
		}
		if ext.Source != catalog.SourceAPT || ext.Package == "" || seen[ext.Package] {
			continue
		}
		seen[ext.Package] = true
		out = append(out, ext.Package)
	}
	sort.Strings(out)
	return out, nil
}

// customNames returns the custom definition keys sorted.
func (r ExtensionResolver) customNames() []string {
	out := make([]string, 0, len(r.cluster.Extensions))
	for name := range r.cluster.Extensions {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// resolve merges one name and returns all descriptor problems instead of the
// first, so validation can report a whole file at once.
func (r ExtensionResolver) resolve(name string) (catalog.Extension, []string) {
	builtin, isBuiltin := catalog.Lookup(name)
	if isBuiltin {
		name = builtin.Name
	}
	def, hasDef := r.cluster.Extensions[name]
	switch {
	case isBuiltin && hasDef:
		return mergeExtension(name, &def, &builtin)
	case isBuiltin:
		return builtin, nil
	case hasDef:
		return mergeExtension(name, &def, nil)
	default:
		return catalog.Extension{}, []string{fmt.Sprintf("extension %q is not built in and has no [extensions.%s] definition", name, name)}
	}
}

// mergeExtension applies a custom definition over a built-in entry, or builds
// a new custom entry. It returns every validation problem it finds.
func mergeExtension(name string, def *ExtensionDefinition, builtin *catalog.Extension) (catalog.Extension, []string) {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	if !validExtensionKey(name) {
		add("extensions.%s name must match %s without leading/trailing hyphen or --", name, extensionKeyPattern)
	}
	out := newExtensionDefinition(name, def)
	if builtin != nil {
		out = mergeBuiltinExtension(name, def, builtin, add)
	}
	validateExtensionProvider(add, name, out)
	validateExtensionIdentity(add, name, out)
	out.Restart = out.Preload != ""
	validateExtensionSettings(add, name, out)
	return out, problems
}

// newExtensionDefinition builds the descriptor a new custom definition states.
func newExtensionDefinition(name string, def *ExtensionDefinition) catalog.Extension {
	out := catalog.Extension{
		Name:         name,
		Source:       def.Source,
		Package:      def.Package,
		CopySafety:   catalog.CopySafetyUnknown,
		CopySettings: cloneStringMap(def.CopySettings),
		Settings:     cloneStringMap(def.Settings),
		ExtName:      def.ExtName,
		Preload:      def.Preload,
		Schema:       def.Schema,
		Note:         def.Note,
	}
	if out.ExtName == "" {
		out.ExtName = name
	}
	if def.CopySafety != nil {
		out.CopySafety = *def.CopySafety
	}
	if def.StandbySafe != nil {
		out.StandbySafe = *def.StandbySafe
	}
	return out
}

// mergeBuiltinExtension applies an override over the built-in entry. Provider
// metadata and the default modules keep the built-in contract; safety
// declarations must be renewed when the provider changes.
func mergeBuiltinExtension(name string, def *ExtensionDefinition, builtin *catalog.Extension, add func(string, ...any)) catalog.Extension {
	out := builtin.Clone()
	out.Settings = mergeStringMap(builtin.Settings, def.Settings)
	out.CopySettings = cloneStringMap(builtin.CopySettings)
	out.Note = builtin.Note
	mergeBuiltinProvider(&out, name, def, builtin, add)
	applyExtensionOverrides(&out, def)
	validateRenewedSafety(&out, name, def, builtin, add)
	applyBuiltinDefault(&out, name, def, builtin, add)
	return out
}

// mergeBuiltinProvider applies the provider fields an override may change.
func mergeBuiltinProvider(out *catalog.Extension, name string, def *ExtensionDefinition, builtin *catalog.Extension, add func(string, ...any)) {
	if def.ExtName != "" {
		if def.ExtName != builtin.ExtName {
			add("extensions.%s.ext_name %q cannot change built-in %q", name, def.ExtName, builtin.ExtName)
		}
		out.ExtName = builtin.ExtName
	}
	if def.Source != "" {
		out.Source = def.Source
	}
	switch out.Source {
	case catalog.SourceAPT:
		if def.Package != "" {
			out.Package = def.Package
		}
	case catalog.SourcePreinstalled:
		if def.Package != "" {
			add("extensions.%s.package must be empty for source preinstalled", name)
		}
		out.Package = ""
	}
}

// applyExtensionOverrides applies the remaining scalar descriptor fields.
func applyExtensionOverrides(out *catalog.Extension, def *ExtensionDefinition) {
	if def.Preload != "" {
		out.Preload = def.Preload
	}
	if def.Schema != "" {
		out.Schema = def.Schema
	}
	if def.Note != "" {
		out.Note = def.Note
	}
	if def.CopySafety != nil {
		out.CopySafety = *def.CopySafety
	}
	if def.CopySettings != nil {
		out.CopySettings = cloneStringMap(def.CopySettings)
	}
	if def.StandbySafe != nil {
		out.StandbySafe = *def.StandbySafe
	}
}

// validateRenewedSafety requires fresh safety declarations when an override
// changes the provider contract.
func validateRenewedSafety(out *catalog.Extension, name string, def *ExtensionDefinition, builtin *catalog.Extension, add func(string, ...any)) {
	providerChanged := out.Source != builtin.Source || out.Package != builtin.Package ||
		out.Preload != builtin.Preload || out.ExtName != builtin.ExtName
	if !providerChanged {
		return
	}
	if def.CopySafety == nil {
		add("extensions.%s.copy_safety must be renewed when source, package, preload or ext_name changes", name)
	}
	if def.StandbySafe == nil {
		add("extensions.%s.standby_safe must be renewed when source, package, preload or ext_name changes", name)
	}
}

// applyBuiltinDefault keeps the implicit default modules' preload managed by
// vaire.
func applyBuiltinDefault(out *catalog.Extension, name string, def *ExtensionDefinition, builtin *catalog.Extension, add func(string, ...any)) {
	if !builtin.Default {
		return
	}
	if def.Preload != "" && def.Preload != builtin.Preload {
		add("extensions.%s.preload is managed by vaire for the default module %q", name, builtin.Name)
	}
	out.Preload = builtin.Preload
	out.Default = true
}

func validateExtensionProvider(add func(string, ...any), name string, ext catalog.Extension) {
	if ext.Source == "" {
		add("extensions.%s.source is required: apt or preinstalled", name)
	} else if ext.Source != catalog.SourceAPT && ext.Source != catalog.SourcePreinstalled {
		add("extensions.%s.source %q must be apt or preinstalled", name, ext.Source)
	}
	switch ext.Source {
	case catalog.SourceAPT:
		if ext.Package == "" {
			add("extensions.%s.package is required for source apt", name)
		}
	case catalog.SourcePreinstalled:
		if ext.Package != "" {
			add("extensions.%s.package must be empty for source preinstalled", name)
		}
	}
	if ext.Package != "" && !debianPackagePattern.MatchString(ext.Package) {
		add("extensions.%s.package %q is not a Debian package name", name, ext.Package)
	}
}

func validateExtensionIdentity(add func(string, ...any), name string, ext catalog.Extension) {
	if ext.ExtName != "" && !validSQLExtensionName(ext.ExtName) {
		add("extensions.%s.ext_name %q is not a safe PostgreSQL extension name", name, ext.ExtName)
	}
	if ext.Preload != "" && !libraryPattern.MatchString(ext.Preload) {
		add("extensions.%s.preload %q is not a library name", name, ext.Preload)
	}
	for _, defaultName := range catalog.Defaults() {
		defaultExt, _ := catalog.Lookup(defaultName)
		if defaultExt.Preload != "" && ext.Preload == defaultExt.Preload && name != defaultExt.Name {
			add("extensions.%s.preload %q is managed by vaire for the default module %q", name, ext.Preload, defaultExt.Name)
		}
	}
	if ext.Schema != "" && !schemaPattern.MatchString(ext.Schema) {
		add("extensions.%s.schema %q must match %s", name, ext.Schema, schemaPattern)
	}
}

func validateExtensionSettings(add func(string, ...any), name string, ext catalog.Extension) {
	for key, value := range ext.Settings {
		validateExtensionSetting(add, "extensions."+name+".settings", key, value)
	}
	for key, value := range ext.CopySettings {
		validateExtensionSetting(add, "extensions."+name+".copy_settings", key, value)
	}
	switch ext.CopySafety {
	case catalog.CopySafetyUnknown, catalog.CopySafetySafe, catalog.CopySafetyDisabled:
	default:
		add("extensions.%s.copy_safety %q must be unknown, safe or disabled", name, ext.CopySafety)
	}
	if ext.CopySafety == catalog.CopySafetyDisabled && len(ext.CopySettings) == 0 {
		add("extensions.%s.copy_settings must suppress background effects when copy_safety is disabled", name)
	}
	if ext.CopySafety != catalog.CopySafetyDisabled && len(ext.CopySettings) > 0 {
		add("extensions.%s.copy_settings requires copy_safety disabled", name)
	}
}

// validateExtensionSetting applies the renderer's safety rules to one
// extension setting. Reserved vaire settings may never be overridden.
func validateExtensionSetting(add func(string, ...any), at, key, value string) {
	if !SettingKeyPattern.MatchString(key) {
		add("%s key %q must match %s", at, key, SettingKeyPattern)
	}
	if isReservedSetting(key) {
		add("%s key %q is managed by vaire and cannot be overridden", at, key)
	}
	if value == "" {
		add("%s key %q needs a value", at, key)
	} else if !extensionSettingValuePattern.MatchString(value) {
		add("%s key %q value %q is not a safe PostgreSQL setting literal", at, key, value)
	}
	if len(value) > 1024 {
		add("%s key %q value is too long", at, key)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			add("%s key %q value contains a control character", at, key)
			break
		}
	}
}

// validateExtensionDefinitions checks every [extensions] descriptor, including
// unused ones, so an invalid definition cannot be activated later unnoticed.
func (c Cluster) validateExtensionDefinitions(add func(string, ...any)) {
	names := make([]string, 0, len(c.Extensions))
	for name := range c.Extensions {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := c.Extensions[name]
		builtin, isBuiltin := catalog.Lookup(name)
		if isBuiltin && builtin.Name != name {
			add("extensions.%s must be written as extensions.%s", name, builtin.Name)
			continue
		}
		if !validExtensionKey(name) {
			add("extensions.%s name must match %s without leading/trailing hyphen or --", name, extensionKeyPattern)
			continue
		}
		var problems []string
		if isBuiltin {
			_, problems = mergeExtension(name, &def, &builtin)
		} else {
			_, problems = mergeExtension(name, &def, nil)
		}
		for _, problem := range problems {
			add("%s", problem)
		}
	}
}

// validateExtensionSelection checks one database's enabled and recovery
// definitions: canonical names, no duplicates, and no two definitions that
// would create the same SQL extension in the database.
func (c Cluster) validateExtensionSelection(add func(string, ...any), at string, s ClusterSpec) {
	c.validateExtensionNames(add, at+".extensions", s.Extensions)
	c.validateExtensionSettingUnion(add, at+".extensions", s.Extensions, s.DatabaseName(), false)
	c.validateExtensionSettingUnion(add, at+".extensions", s.Extensions, s.DatabaseName(), true)
	if s.RecoveryExtensions != nil {
		c.validateExtensionNames(add, at+".recovery_extensions", *s.RecoveryExtensions)
		// A recovery declaration is a complete selection: the implicit
		// default modules are part of it too. It is validated on its own,
		// never merged with today's enabled extensions.
		if effective, err := c.ExtensionResolver().EffectiveNames(*s.RecoveryExtensions); err == nil {
			c.validateExtensionSettingUnion(add, at+".recovery_extensions", effective, s.DatabaseName(), false)
			c.validateExtensionSettingUnion(add, at+".recovery_extensions", effective, s.DatabaseName(), true)
		}
	}
}

type extensionSettingUse struct {
	value string
	owner string
}

// validateExtensionSettingUnion rejects two extensions in one database that
// assign the same setting different values. Render order must never silently
// choose one. For the copy union a descriptor's own copy settings intentionally
// override its normal value; all other descriptors must still agree.
func (c Cluster) validateExtensionSettingUnion(add func(string, ...any), at string, names []string, db string, copyUnion bool) {
	seen := map[string]extensionSettingUse{}
	mode := "normal"
	if copyUnion {
		mode = "copy"
	}
	for _, name := range names {
		ext, err := c.ExtensionResolver().Resolve(name)
		if err != nil {
			continue
		}
		addExtensionSettingConflicts(add, at, db, mode, seen, ext)
	}
}

// addExtensionSettingConflicts records one extension's settings in the union
// and reports a key two extensions assign different effective values.
func addExtensionSettingConflicts(add func(string, ...any), at, db, mode string, seen map[string]extensionSettingUse, ext catalog.Extension) {
	settings := ext.Settings
	if mode == "copy" {
		settings = mergeStringMap(ext.Settings, ext.CopySettings)
	}
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := strings.ReplaceAll(normalizedSettingValue(settings[key]), catalog.DBPlaceholder, db)
		prev, ok := seen[key]
		if !ok {
			seen[key] = extensionSettingUse{value: value, owner: ext.Name}
			continue
		}
		if prev.value != value {
			add("%s setting %q conflicts between %q and %q in the %s union (%q vs %q)",
				at, key, prev.owner, ext.Name, mode, prev.value, value)
		}
	}
}

func (c Cluster) validateExtensionNames(add func(string, ...any), at string, names []string) {
	seenName := map[string]bool{}
	seenExt := map[string]string{}
	for i, name := range names {
		builtin, isBuiltin := catalog.Lookup(name)
		switch {
		case isBuiltin && builtin.Name != name:
			add("%s[%d] %q must be written as %q", at, i, name, builtin.Name)
			continue
		case isBuiltin:
			name = builtin.Name
		default:
			if _, ok := c.Extensions[name]; !ok {
				add("%s[%d] %q is not built in and has no [extensions.%s] definition", at, i, name, name)
				continue
			}
		}
		if seenName[name] {
			add("%s %q is listed twice", at, name)
			continue
		}
		seenName[name] = true
		ext, err := c.ExtensionResolver().Resolve(name)
		if err != nil {
			add("%s %q: %v", at, name, err)
			continue
		}
		if ext.ExtName == "" {
			continue
		}
		if other, ok := seenExt[ext.ExtName]; ok {
			add("%s %q and %q both create SQL extension %q", at, other, name, ext.ExtName)
			continue
		}
		seenExt[ext.ExtName] = name
	}
}

// hostSettingRequirement is one effective setting value a host-wide selection
// requires, with the selection that first stated it.
type hostSettingRequirement struct {
	value string
	where string
}

// hostRequirement accumulates the provider, preload and setting requirements
// for one SQL extension or preload library across the host's selections. An
// empty preload or an absent setting key is absence of a requirement, not a
// contradictory value, so a sparse descriptor merges with a richer one without
// hiding a later conflict.
type hostRequirement struct {
	where        string
	source       string
	pkg          string
	preload      string
	preloadWhere string
	settings     map[string]hostSettingRequirement
}

func newHostRequirement(where string, ext catalog.Extension) hostRequirement {
	settings := make(map[string]hostSettingRequirement, len(ext.Settings))
	for key, value := range ext.Settings {
		settings[key] = hostSettingRequirement{value: normalizedSettingValue(value), where: where}
	}
	return hostRequirement{
		where: where, source: ext.Source, pkg: ext.Package,
		preload: ext.Preload, preloadWhere: where, settings: settings,
	}
}

func sortedHostSettingKeys(settings map[string]hostSettingRequirement) []string {
	keys := make([]string, 0, len(settings))
	for key := range settings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// mergeSQL adds next's requirements to a SQL extension aggregate. A sparse
// descriptor contributes nothing for an omitted preload or setting; only two
// nonempty, different requirements conflict.
func (r *hostRequirement) mergeSQL(add func(string, ...any), extName string, next hostRequirement) {
	if r.source != next.source || r.pkg != next.pkg {
		add("%s and %s resolve SQL extension %q with conflicting providers (%s vs %s)",
			r.where, next.where, extName, providerText(r.source, r.pkg, r.preload), providerText(next.source, next.pkg, next.preload))
	}
	if next.preload != "" {
		switch {
		case r.preload == "":
			r.preload = next.preload
			r.preloadWhere = next.where
		case r.preload != next.preload:
			add("%s and %s resolve SQL extension %q with conflicting preload libraries (%q vs %q)",
				r.preloadWhere, next.where, extName, r.preload, next.preload)
		}
	}
	for _, key := range sortedHostSettingKeys(next.settings) {
		nextUse := next.settings[key]
		if prev, ok := r.settings[key]; ok {
			if prev.value != nextUse.value {
				add("%s and %s resolve SQL extension %q with conflicting setting %q (%q vs %q)",
					prev.where, nextUse.where, extName, key, prev.value, nextUse.value)
			}
			continue
		}
		r.settings[key] = nextUse
	}
}

// mergeLibrary adds next's requirements to a preload-library aggregate. The
// existing contract keeps one provider identity and one effective value per
// overlapping library setting across the host.
func (r *hostRequirement) mergeLibrary(add func(string, ...any), library string, next hostRequirement) {
	conflict := r.source != next.source || r.pkg != next.pkg
	for _, key := range sortedHostSettingKeys(next.settings) {
		nextUse := next.settings[key]
		if prev, ok := r.settings[key]; ok {
			if prev.value != nextUse.value {
				conflict = true
			}
			continue
		}
		r.settings[key] = nextUse
	}
	if conflict {
		add("%s and %s load library %q with conflicting providers or settings", r.where, next.where, library)
	}
}

// extensionHostSelection is one effective selection on the host and the names
// its declaration stated explicitly.
type extensionHostSelection struct {
	label    string
	names    []string
	declared map[string]bool
}

// validateExtensionHost rejects conflicting providers and runtime metadata
// for the same SQL extension or preload library across the host's databases.
// It validates the effective selection, including implicit defaults, for both
// current and present recovery declarations.
func (c Cluster) validateExtensionHost(add func(string, ...any)) {
	resolver := c.ExtensionResolver()
	byExt := map[string]hostRequirement{}
	byPreload := map[string]hostRequirement{}
	for _, spec := range c.Clusters {
		for _, sel := range extensionHostSelections(resolver, spec) {
			for _, name := range sel.names {
				ext, err := resolver.Resolve(name)
				if err != nil {
					continue
				}
				requirement := newHostRequirement(extensionHostSelectionLabel(spec.Name, sel, name), ext)
				if ext.ExtName != "" {
					addHostSQLRequirement(byExt, add, ext.ExtName, requirement)
				}
				if ext.Preload != "" {
					addHostLibraryRequirement(byPreload, add, ext.Preload, requirement)
				}
			}
		}
	}
}

// extensionHostSelections returns the current selection and, when declared,
// the recovery selection. An absent recovery declaration is unknown, not empty.
func extensionHostSelections(resolver ExtensionResolver, spec ClusterSpec) []extensionHostSelection {
	selections := []extensionHostSelection{newExtensionHostSelection(resolver, "extensions", spec.Extensions)}
	if spec.RecoveryExtensions != nil {
		selections = append(selections, newExtensionHostSelection(resolver, "recovery_extensions", *spec.RecoveryExtensions))
	}
	return selections
}

func newExtensionHostSelection(resolver ExtensionResolver, label string, declaredNames []string) extensionHostSelection {
	effective, err := resolver.EffectiveNames(declaredNames)
	if err != nil {
		return extensionHostSelection{label: label, declared: map[string]bool{}}
	}
	declared := map[string]bool{}
	for _, name := range declaredNames {
		canonical, _ := resolver.Canonical(name)
		declared[canonical] = true
	}
	return extensionHostSelection{label: label, names: effective, declared: declared}
}

func extensionHostSelectionLabel(specName string, sel extensionHostSelection, name string) string {
	at := "clusters." + specName + "." + sel.label
	if sel.declared[name] {
		return at + " " + fmt.Sprintf("%q", name)
	}
	return at + " implicit " + fmt.Sprintf("%q", name)
}

func addHostSQLRequirement(by map[string]hostRequirement, add func(string, ...any), key string, requirement hostRequirement) {
	prev, ok := by[key]
	if !ok {
		by[key] = requirement
		return
	}
	prev.mergeSQL(add, key, requirement)
	by[key] = prev
}

func addHostLibraryRequirement(by map[string]hostRequirement, add func(string, ...any), key string, requirement hostRequirement) {
	prev, ok := by[key]
	if !ok {
		by[key] = requirement
		return
	}
	prev.mergeLibrary(add, key, requirement)
	by[key] = prev
}

// normalizedSettingValue removes one PostgreSQL string-literal quoting layer
// for conflict comparison: on and 'on' are the same effective setting value.
func normalizedSettingValue(value string) string {
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "''", "'")
	}
	return value
}

func providerText(source, pkg, preload string) string {
	switch {
	case pkg != "":
		return source + " package " + pkg + " preload " + preload
	case preload != "":
		return source + " preload " + preload
	default:
		return source
	}
}

// ExtensionIntentChanges returns the sorted cluster keys whose extension
// intent differs between old and updated. It compares only definitions used by
// a database or recovery declaration, and ignores human notes. An absent
// recovery_extensions is different from an explicitly empty one.
func ExtensionIntentChanges(old, updated Cluster) []string {
	used := extensionIntentUsed(old)
	for name := range extensionIntentUsed(updated) {
		used[name] = true
	}
	for _, name := range catalog.Defaults() {
		used[name] = true
	}

	changes := extensionDefinitionChanges(used, old, updated)
	changes = append(changes, clusterExtensionIntentChanges(old, updated)...)
	sort.Strings(changes)
	return slices.Compact(changes)
}

// extensionIntentUsed collects the canonical names any database or recovery
// declaration in c uses.
func extensionIntentUsed(c Cluster) map[string]bool {
	used := map[string]bool{}
	for _, spec := range c.Clusters {
		for _, name := range spec.Extensions {
			canonical, _ := c.ExtensionResolver().Canonical(name)
			used[canonical] = true
		}
		if spec.RecoveryExtensions != nil {
			for _, name := range *spec.RecoveryExtensions {
				canonical, _ := c.ExtensionResolver().Canonical(name)
				used[canonical] = true
			}
		}
	}
	return used
}

func extensionDefinitionChanges(used map[string]bool, old, updated Cluster) []string {
	var changes []string
	for name := range used {
		before, beforeOK := old.ExtensionResolver().Lookup(name)
		after, afterOK := updated.ExtensionResolver().Lookup(name)
		if beforeOK != afterOK || (beforeOK && !before.SemanticEqual(after)) {
			changes = append(changes, "extensions."+name)
		}
	}
	return changes
}

func clusterExtensionIntentChanges(old, updated Cluster) []string {
	var changes []string
	for _, beforeSpec := range old.Clusters {
		afterSpec, ok := updated.ClusterByName(beforeSpec.Name)
		if !ok {
			continue
		}
		if !slices.Equal(canonicalListFor(old, beforeSpec.Extensions), canonicalListFor(updated, afterSpec.Extensions)) {
			changes = append(changes, "clusters."+beforeSpec.Name+".extensions")
		}
		if recoveryChanged(old, updated, beforeSpec.RecoveryExtensions, afterSpec.RecoveryExtensions) {
			changes = append(changes, "clusters."+beforeSpec.Name+".recovery_extensions")
		}
	}
	return changes
}

func canonicalListFor(c Cluster, names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		canonical, _ := c.ExtensionResolver().Canonical(name)
		out = append(out, canonical)
	}
	sort.Strings(out)
	return slices.Compact(out)
}

func recoveryChanged(old, updated Cluster, before, after *[]string) bool {
	if (before == nil) != (after == nil) {
		return true
	}
	if before == nil {
		return false
	}
	return !slices.Equal(canonicalListFor(old, *before), canonicalListFor(updated, *after))
}

// Clone returns a deep copy of the cluster: no nested slice or map is shared
// with the receiver. Views and copy construction use it so a resolved
// descriptor can never be mutated through another database's snapshot.
func (c Cluster) Clone() Cluster {
	out := c
	out.Nodes = slices.Clone(c.Nodes)
	if c.Clusters != nil {
		out.Clusters = make([]ClusterSpec, len(c.Clusters))
		for i := range c.Clusters {
			out.Clusters[i] = c.Clusters[i].Clone()
		}
	}
	if c.Awaiting != nil {
		out.Awaiting = make([]ClusterSpec, len(c.Awaiting))
		for i := range c.Awaiting {
			out.Awaiting[i] = c.Awaiting[i].Clone()
		}
	}
	out.Network.ClientAllowlist = slices.Clone(c.Network.ClientAllowlist)
	out.Metrics.Allowlist = slices.Clone(c.Metrics.Allowlist)
	out.Postgres.Settings = cloneStringMap(c.Postgres.Settings)
	if c.Extensions != nil {
		out.Extensions = make(map[string]ExtensionDefinition, len(c.Extensions))
		for name, def := range c.Extensions {
			out.Extensions[name] = def.Clone()
		}
	}
	return out
}

// Clone returns a deep copy of the cluster entry.
func (s ClusterSpec) Clone() ClusterSpec {
	out := s
	out.Extensions = slices.Clone(s.Extensions)
	out.Settings = cloneStringMap(s.Settings)
	out.RecoveryExtensions = cloneStringList(s.RecoveryExtensions)
	return out
}

func cloneStringList(in *[]string) *[]string {
	if in == nil {
		return nil
	}
	out := make([]string, len(*in))
	copy(out, *in)
	return &out
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mergeStringMap(base, override map[string]string) map[string]string {
	if len(base) == 0 && len(override) == 0 {
		return nil
	}
	out := make(map[string]string, len(base)+len(override))
	maps.Copy(out, base)
	maps.Copy(out, override)
	return out
}

var (
	// extensionKeyPattern is the canonical cluster.toml key format. It
	// allows internal hyphens so uuid-ossp can be a key directly; the SQL
	// extension name is validated separately.
	extensionKeyPattern  = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,62}$`)
	debianPackagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]{1,127}$`)
	libraryPattern       = regexp.MustCompile(`^[a-z0-9_][a-z0-9_.-]{0,127}$`)
	schemaPattern        = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	// A setting value is a single-quoted string literal, a bare identifier,
	// or a signed decimal integer. Floats, units, lists and punctuation
	// must be quoted, matching PostgreSQL's config-file token rules
	// conservatively. It is never a free-form config fragment.
	extensionSettingValuePattern = regexp.MustCompile(`^('([^'\\\r\n]|'')*'|[A-Za-z_][A-Za-z0-9_]*|[+-]?[0-9]+)$`)
)

// validExtensionKey is the canonical key rule: lower-case identifier with
// optional internal hyphens, but no leading/trailing hyphen or "--".
func validExtensionKey(name string) bool {
	return extensionKeyPattern.MatchString(name) && !strings.Contains(name, "--") && !strings.HasSuffix(name, "-")
}

// validSQLExtensionName follows PostgreSQL 18's extension-name limits:
// nonempty, at most 63 bytes, no "--", no leading/trailing hyphen, no
// directory separators and no control characters. Other punctuation and
// mixed case are safe because CREATE EXTENSION quotes the identifier.
func validSQLExtensionName(name string) bool {
	if name == "" || name == "." || len(name) > 63 {
		return false
	}
	if strings.Contains(name, "--") || strings.Contains(name, "..") {
		return false
	}
	if strings.HasPrefix(name, "-") || strings.HasSuffix(name, "-") {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}
