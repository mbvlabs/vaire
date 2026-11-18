package backup

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

// Historical extension requirements and copy guards (Phase 7). A stanza keeps
// every resolved descriptor its data has needed, so a rebuild or a restore can
// reproduce the preloads, packages and copy settings that current intent no
// longer names. Coverage starts when vaire first observed a stanza's
// requirements; it is never backdated.

// ExtensionRequirement is one resolved descriptor a stanza needed, first seen
// at Since.
type ExtensionRequirement struct {
	Name      string            `json:"name"`
	Since     string            `json:"since"`
	Extension catalog.Extension `json:"extension"`
}

// ExtensionRequirements is one stanza's retained requirement history.
type ExtensionRequirements struct {
	// Database is diagnostic only. The map key (the immutable primary
	// stanza) is the binding; a database name is never one.
	Database      string                 `json:"database,omitempty"`
	CoverageSince string                 `json:"coverage_since,omitempty"`
	Requirements  []ExtensionRequirement `json:"requirements,omitempty"`
}

// ExtensionRequirementsFor returns stanza's history.
func (s Records) ExtensionRequirementsFor(stanza string) (ExtensionRequirements, bool) {
	if stanza == "" {
		return ExtensionRequirements{}, false
	}

	r, ok := s.ExtensionRequirements[stanza]

	return r, ok
}

// RecordExtensionRequirements adds the descriptors that are not already
// retained for stanza and sets CoverageSince once. It returns whether records
// changed. It is safe to call from inside UpdateRecords and never removes a
// snapshot: conservative retention is deliberate.
func (s *Records) RecordExtensionRequirements(stanza, database string, exts []catalog.Extension, at time.Time) bool {
	if stanza == "" {
		return false
	}

	if s.ExtensionRequirements == nil {
		s.ExtensionRequirements = map[string]ExtensionRequirements{}
	}

	entry, ok := s.ExtensionRequirements[stanza]

	changed := !ok
	if entry.Database == "" && database != "" {
		entry.Database, changed = database, true
	}

	if entry.CoverageSince == "" {
		entry.CoverageSince = at.UTC().Format(time.RFC3339)
		changed = true
	}

	since := at.UTC().Format(time.RFC3339)

	for _, ext := range exts {
		if ext.Name == "" {
			continue
		}

		ext = ext.Clone()
		if requirementsContain(entry.Requirements, ext) {
			continue
		}

		entry.Requirements = append(entry.Requirements, ExtensionRequirement{
			Name: ext.Name, Since: since, Extension: ext,
		})
		changed = true
	}

	if changed {
		sortExtensionRequirements(entry.Requirements)
		s.ExtensionRequirements[stanza] = entry
	}

	return changed
}

func requirementsContain(reqs []ExtensionRequirement, ext catalog.Extension) bool {
	for _, have := range reqs {
		if have.Extension.SemanticEqual(ext) {
			return true
		}
	}

	return false
}

func sortExtensionRequirements(reqs []ExtensionRequirement) {
	sort.SliceStable(reqs, func(i, j int) bool {
		a, b := reqs[i], reqs[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}

		if a.Since != b.Since {
			return a.Since < b.Since
		}

		return requirementKey(a.Extension) < requirementKey(b.Extension)
	})
}

func requirementKey(ext catalog.Extension) string {
	return strings.Join([]string{ext.Name, ext.ExtName, ext.Source, ext.Package, ext.Preload, ext.Schema}, "\x00")
}

// RecordExtensionRequirements is the durable wrapper around the Records
// method. A failure stops the dependent action; it never falls back to today's
// intent as proof for an older point.
func RecordExtensionRequirements(stanza, database string, exts []catalog.Extension, at time.Time) error {
	if stanza == "" {
		return nil
	}

	return UpdateRecords(func(s *Records) error {
		s.RecordExtensionRequirements(stanza, database, exts, at)
		return nil
	})
}

// RetainedExtensions returns the resolved snapshots retained for stanza, in
// their recorded order.
func (s Records) RetainedExtensions(stanza string) []catalog.Extension {
	r, ok := s.ExtensionRequirementsFor(stanza)
	if !ok {
		return nil
	}

	out := make([]catalog.Extension, 0, len(r.Requirements))
	for _, req := range r.Requirements {
		out = append(out, req.Extension.Clone())
	}

	return out
}

// CopyGuard is what must win in a copy's postgresql.auto.conf: the libraries
// the data needs and the settings that keep copied background work idle. It is
// deliberately separate from ordinary desired settings: an independent copy
// persists those settings explicitly, and a same-database restore drops this
// guard once copied jobs are deactivated.
type CopyGuard struct {
	// Preloads is the shared_preload_libraries value in load order, not a
	// set: one library may require another to load before it.
	Preloads []string          `json:"preloads,omitempty"`
	Settings map[string]string `json:"settings,omitempty"`
	// Origins records why each preload is in the guard: retained, desired or
	// operator. A prior guard without this provenance is not independent
	// safety evidence and must be explained before it is overwritten.
	Origins map[string]string `json:"origins,omitempty"`
}

// Empty reports whether g has nothing to enforce.
func (g CopyGuard) Empty() bool { return len(g.Preloads) == 0 && len(g.Settings) == 0 }

// Clone returns a deep copy.
func (g CopyGuard) Clone() CopyGuard {
	out := CopyGuard{Preloads: append([]string(nil), g.Preloads...)}
	if g.Settings != nil {
		out.Settings = make(map[string]string, len(g.Settings))
		maps.Copy(out.Settings, g.Settings)
	}

	if g.Origins != nil {
		out.Origins = make(map[string]string, len(g.Origins))
		maps.Copy(out.Origins, g.Origins)
	}

	return out
}

// SettingKeys are the guard's managed setting keys, sorted.
func (g CopyGuard) SettingKeys() []string {
	out := make([]string, 0, len(g.Settings))
	for k := range g.Settings {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}

// copyGuardBegin and copyGuardEnd delimit the block. The block is written at
// the end of postgresql.auto.conf, which PostgreSQL reads after
// postgresql.conf and its conf.d, so its values win over a copied
// configuration and a copied auto.conf.
const (
	copyGuardBegin = "# vaire copy guard begin: background work stays idle until this copy is settled; managed by vaire, do not edit"
	copyGuardEnd   = "# vaire copy guard end"
	// copyGuardOriginsPrefix records why each preload is in the block. A
	// previous guard alone is not safety evidence; an unexplained entry
	// refuses instead of being silently erased.
	copyGuardOriginsPrefix = "# vaire guard origins: "
)

// writeCopyGuardFile is injectable for tests: production writes as postgres.
var writeCopyGuardFile = func(path string, data []byte, perm os.FileMode) error {
	return engine.WriteFileAtomicOwned(path, data, perm, "postgres", "postgres")
}

// CopyGuardWriterForTest returns the current guard writer so a test in another
// package can replace it and restore it.
func CopyGuardWriterForTest() func(string, []byte, os.FileMode) error { return writeCopyGuardFile }

// RestoreCopyGuardWriterForTest restores a writer returned by
// CopyGuardWriterForTest.
func RestoreCopyGuardWriterForTest(f func(string, []byte, os.FileMode) error) { writeCopyGuardFile = f }

func autoConfPath(dataDir string) string { return filepath.Join(dataDir, "postgresql.auto.conf") }

// InstallCopyGuard writes g at the end of the copy's postgresql.auto.conf,
// removing any earlier vaire guard block. It returns whether the file changed.
// The recovery settings pgBackRest wrote and every other line are preserved,
// including a plain shared_preload_libraries or setting assignment: it is the
// operator's own provenance and wins again once the guard block is removed.
// The guard is appended last, so PostgreSQL reads it after that line.
func InstallCopyGuard(dataDir string, g CopyGuard) (bool, error) {
	path := autoConfPath(dataDir)

	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}

	if _, _, err := parseCopyGuard(string(raw)); err != nil {
		return false, fmt.Errorf("the existing vaire copy guard is unreadable, so vaire will not overwrite it: %w", err)
	}

	kept := stripCopyGuard(string(raw))
	if g.Empty() {
		if kept == string(raw) {
			return false, nil
		}

		return true, writeCopyGuardFile(path, []byte(kept), 0o600)
	}

	var b strings.Builder
	b.WriteString(strings.TrimRight(kept, "\n"))

	if b.Len() > 0 {
		b.WriteString("\n")
	}

	b.WriteString(copyGuardBegin + "\n")

	if len(g.Origins) > 0 {
		libs := make([]string, 0, len(g.Origins))
		for lib := range g.Origins {
			libs = append(libs, lib)
		}

		sort.Strings(libs)

		parts := make([]string, 0, len(libs))
		for _, lib := range libs {
			parts = append(parts, lib+"="+g.Origins[lib])
		}

		b.WriteString(copyGuardOriginsPrefix + strings.Join(parts, ",") + "\n")
	}

	if len(g.Preloads) > 0 {
		b.WriteString("shared_preload_libraries = '" + strings.Join(g.Preloads, ",") + "'\n")
	}

	for _, key := range g.SettingKeys() {
		b.WriteString(key + " = " + g.Settings[key] + "\n")
	}

	b.WriteString(copyGuardEnd + "\n")

	return true, writeCopyGuardFile(path, []byte(b.String()), 0o600)
}

// RemoveCopyGuard removes the vaire block from the copy's
// postgresql.auto.conf. It returns whether the file changed. Plain assignments
// outside the block are the operator's provenance and stay: a deliberate
// later operator change is not hidden by a guard that was already released.
func RemoveCopyGuard(dataDir string) (bool, error) {
	path := autoConfPath(dataDir)

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}

		return false, err
	}

	if _, present, err := parseCopyGuard(string(raw)); err != nil || !present {
		return false, err
	}

	kept := stripCopyGuard(string(raw))
	if kept == string(raw) {
		return false, nil
	}

	return true, writeCopyGuardFile(path, []byte(kept), 0o600)
}

// ReadCopyGuard returns the guard currently in dataDir.
func ReadCopyGuard(dataDir string) (CopyGuard, bool, error) {
	raw, err := os.ReadFile(autoConfPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return CopyGuard{}, false, nil
		}

		return CopyGuard{}, false, err
	}

	g, present, err := parseCopyGuard(string(raw))

	return g, present, err
}

// CopyGuardCovered reports whether the permanent configuration supplies every
// value the guard enforces. Preloads must be a superset; settings must have the
// same normalized effective value.
func CopyGuardCovered(g CopyGuard, preloads []string, settings map[string]string) bool {
	have := map[string]bool{}
	for _, p := range preloads {
		have[p] = true
	}

	for _, p := range g.Preloads {
		if !have[p] {
			return false
		}
	}

	for k, want := range g.Settings {
		got, ok := settings[k]
		if !ok || got != catalog.NormalizedSettingValue(want) {
			return false
		}
	}

	return true
}

// managedConfPreloads reads the preloads in the cluster's managed extension
// file. These are old current intent, not an operator's own list: a guard
// rewrite may replace them. A missing file is an empty list; an unreadable one
// is an error.
func managedConfPreloads(confDir string) ([]string, error) {
	raw, err := os.ReadFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	tokens, err := lexConfigLines(string(raw))
	if err != nil {
		return nil, err
	}

	value := ""

	for _, tok := range tokens {
		if tok.Name == "shared_preload_libraries" {
			value = tok.Value
		}
	}

	return splitSettingList(value), nil
}

// PlainAutoPreloads reads the shared_preload_libraries assignment the operator
// (or a copied configuration) wrote directly in postgresql.auto.conf, ignoring
// the vaire guard block. It is the provenance beneath the winning block: the
// guard is appended last, so it wins while present and this value applies again
// once the guard is deliberately released.
func PlainAutoPreloads(dataDir string) ([]string, error) {
	raw, err := os.ReadFile(autoConfPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, err
	}

	tokens, err := lexConfigLines(stripCopyGuard(string(raw)))
	if err != nil {
		return nil, err
	}

	value := ""

	for _, tok := range tokens {
		if tok.Name == "shared_preload_libraries" {
			value = tok.Value
		}
	}

	return splitSettingList(value), nil
}

// OperatorPreloads is the current operator configuration's preloads: conf.d
// values excluding vaire's generated current-intent file, plus the plain
// postgresql.auto.conf assignment. The generated guard block is not operator
// configuration.
func OperatorPreloads(confDir, dataDir string) ([]string, error) {
	confEffective, err := EffectiveConfPreloads(confDir)
	if err != nil {
		return nil, err
	}

	autoPlain, err := PlainAutoPreloads(dataDir)
	if err != nil {
		return nil, err
	}

	oldManaged, err := managedConfPreloads(confDir)
	if err != nil {
		return nil, err
	}

	oldCurrent := map[string]bool{}
	for _, p := range oldManaged {
		oldCurrent[p] = true
	}

	seen := map[string]bool{}

	var out []string

	add := func(p string) {
		if p != "" && !oldCurrent[p] && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, p := range confEffective {
		add(p)
	}

	for _, p := range autoPlain {
		add(p)
	}

	sort.Strings(out)

	return out, nil
}

// ComposeCopyGuard returns the complete guard a copy must carry: current
// desired preloads, then retained requirements, then the current underlying
// operator configuration (postgresql.conf, conf.d and a plain
// postgresql.auto.conf assignment) excluding the generated guard and vaire's
// obsolete managed current intent. Preload order is the load order: current
// desired preloads come first because a library may require another to load
// before it, and each remaining source keeps its own order. A prior guard block
// is not independent evidence: only the requirements and the operator's current
// configuration survive, so a deliberate removal of a library takes effect.
// Settings are base.Settings only: the caller recomputes them from current
// intent and retained history, never by accumulating an old guard.
func ComposeCopyGuard(confDir, dataDir string, base CopyGuard, desired []string) (CopyGuard, error) {
	return ComposeCopyGuardDropping(confDir, dataDir, base, desired, nil)
}

// ComposeCopyGuardDropping is ComposeCopyGuard with explicitly retired
// preloads. A planned primary SQL DROP may retire a library that a prior
// guard recorded as retained; it is dropped only because the caller named the
// exact planned drop.
func ComposeCopyGuardDropping(confDir, dataDir string, base CopyGuard, desired []string, dropped map[string]bool) (CopyGuard, error) {
	existing, present, err := ReadCopyGuard(dataDir)
	if err != nil {
		return CopyGuard{}, err
	}

	confEffective, err := EffectiveConfPreloads(confDir)
	if err != nil {
		return CopyGuard{}, err
	}

	autoPlain, err := PlainAutoPreloads(dataDir)
	if err != nil {
		return CopyGuard{}, err
	}

	oldManaged, err := managedConfPreloads(confDir)
	if err != nil {
		return CopyGuard{}, err
	}

	oldCurrent := map[string]bool{}
	for _, p := range oldManaged {
		oldCurrent[p] = true
	}

	out := base.Clone()
	out.Preloads = nil
	out.Origins = nil
	preloads := copyGuardPreloadSet{origins: map[string]string{}, seen: map[string]bool{}}
	// Origin precedence: retained evidence outranks current intent, which
	// outranks the operator configuration. Desired preloads are inserted first
	// to keep their load order; that insertion must not downgrade a library
	// also backed by retained history, or a later rewrite could unload it
	// while the data still needs it.
	for _, p := range desired {
		preloads.add(p, "desired")
	}

	for _, p := range base.Preloads {
		preloads.add(p, "retained")
	}

	for _, p := range confEffective {
		if !oldCurrent[p] {
			preloads.add(p, "operator")
		}
	}

	for _, p := range autoPlain {
		if !oldCurrent[p] {
			preloads.add(p, "operator")
		}
	}

	if present {
		if err := rejectUnexplainedCopyGuardPreloads(existing, preloads.seen, dropped); err != nil {
			return CopyGuard{}, err
		}
	}

	out.Preloads = preloads.preloads
	if len(out.Preloads) == 0 {
		out.Preloads = nil
	}

	out.Origins = preloads.origins
	if len(out.Origins) == 0 {
		out.Origins = nil
	}

	return out, nil
}

var copyGuardOriginRank = map[string]int{"operator": 0, "desired": 1, "retained": 2}

type copyGuardPreloadSet struct {
	preloads []string
	origins  map[string]string
	seen     map[string]bool
}

func (s *copyGuardPreloadSet) add(preload, origin string) {
	if preload == "" {
		return
	}

	if s.seen[preload] {
		if copyGuardOriginRank[origin] > copyGuardOriginRank[s.origins[preload]] {
			s.origins[preload] = origin
		}

		return
	}

	s.seen[preload] = true
	s.preloads = append(s.preloads, preload)
	s.origins[preload] = origin
}

func rejectUnexplainedCopyGuardPreloads(existing CopyGuard, seen, dropped map[string]bool) error {
	for _, p := range existing.Preloads {
		if seen[p] || dropped[p] {
			continue
		}

		switch existing.Origins[p] {
		case "operator", "desired":
			// The previous guard recorded an operator or current-intent
			// origin, and that source is now gone. The deliberate removal
			// is proven and the entry is dropped.
		default:
			// A previous guard alone is not safety metadata. An
			// unexplained requirement, including a valid pre-provenance
			// guard, is never silently erased or loaded: refuse before
			// any write or start.
			return output.NewError(output.CodePrecondition,
				"the copy's existing guard contains "+p+", whose origin is not proven",
				output.ExitPrecondition,
				"Keep it in current intent, retained requirements or the operator's own configuration, or add explicit compatible metadata; the copy was not started.").
				WithDetails(map[string]any{"reason": "extension_previous_guard_unknown", "library": p})
		}
	}

	return nil
}

// CopyGuardSettingsOptions is one guard settings decision. Known is the
// compatible union of current and retained descriptors; Current is today's
// intent; Permanent is the permanent configuration (server, database and
// current descriptors' ordinary settings, explicit database settings last);
// Explicit is the database's own explicit [clusters.settings]; Handoff is the
// durable per-key provenance already released to equivalent permanent
// configuration. Pending says the copy has not settled yet, so a mandatory
// suppression still wins over a different permanent value.
type CopyGuardSettingsOptions struct {
	Known     []catalog.Extension
	Current   []catalog.Extension
	Permanent map[string]string
	Explicit  map[string]string
	CopyDB    string
	Pending   bool
	Handoff   map[string]string
	// HandoffPermanent is the actual underlying permanent effective value
	// (postgresql.conf, conf.d and plain postgresql.auto.conf, excluding the
	// generated guard) used to prove a mandatory key may be released. Nil
	// falls back to Permanent for tests; production callers pass real files.
	HandoffPermanent map[string]string
}

// CopyGuardSettingsDecision is the guard's settings and the handoff provenance
// after this decision. The caller persists the handoff map with the records.
type CopyGuardSettingsDecision struct {
	Settings map[string]string
	Handoff  map[string]string
}

type copySettingOwner struct {
	name      string
	mandatory bool
}

// CopyGuardSettings computes the settings a copy guard must enforce. Each
// descriptor's own mandatory copy settings overlay its own ordinary settings
// (a Timescale 2→0 suppression is not a conflict with itself); two different
// descriptors with different effective values refuse. An inherited ordinary
// destination value never beats a mandatory suppression: the guard wins until
// the permanent configuration supplies the equivalent value, and then the key
// is recorded as handed off so a deliberate later supported change is not
// overridden by a reconstructed guard. Historical-only ordinary settings stay
// until the permanent configuration supplies them.
func CopyGuardSettings(opts CopyGuardSettingsOptions) (CopyGuardSettingsDecision, error) {
	known := opts.Known
	if len(known) == 0 {
		known = opts.Current
	}

	effective, owners, err := effectiveCopySettings(known, opts.CopyDB)
	if err != nil {
		return CopyGuardSettingsDecision{}, err
	}

	guard, handoff := copyGuardSettingDecisions(effective, owners, opts)

	return CopyGuardSettingsDecision{Settings: guard, Handoff: handoff}, nil
}

func effectiveCopySettings(known []catalog.Extension, copyDB string) (map[string]string, map[string]copySettingOwner, error) {
	effective := map[string]string{}
	owners := map[string]copySettingOwner{}

	for _, ext := range known {
		ordinary := ext.ResolvedSettings(copyDB)

		mandatory := map[string]string{}
		if ext.CopySafety == catalog.CopySafetyDisabled {
			mandatory = ext.ResolvedCopySettings(copyDB)
		}

		for k, v := range ordinary {
			if _, own := mandatory[k]; own {
				// The descriptor's own suppression overlays its own ordinary
				// setting. This is the built-in Timescale 2→0 case.
				continue
			}

			if err := mergeEffective(effective, owners, k, v, copySettingOwner{name: ext.Name}); err != nil {
				return nil, nil, err
			}
		}

		for k, v := range mandatory {
			if err := mergeEffective(effective, owners, k, v, copySettingOwner{name: ext.Name, mandatory: true}); err != nil {
				return nil, nil, err
			}
		}
	}

	return effective, owners, nil
}

func copyGuardSettingDecisions(effective map[string]string, owners map[string]copySettingOwner, opts CopyGuardSettingsOptions) (guard, handoff map[string]string) {
	guard = map[string]string{}
	handoff = map[string]string{}

	for k, v := range effective {
		owner := owners[k]
		if !owner.mandatory {
			if _, supplied := opts.Permanent[k]; supplied {
				continue
			}

			guard[k] = v

			continue
		}

		if value, ok := handedOffCopySetting(k, v, opts); ok {
			handoff[k] = value
			continue
		}

		guard[k] = v
	}

	if len(guard) == 0 {
		guard = nil
	}

	if len(handoff) == 0 {
		handoff = nil
	}

	return guard, handoff
}

// handedOffCopySetting returns the recorded handoff value when a mandatory
// setting is already supplied by verified equivalent permanent configuration.
func handedOffCopySetting(key, value string, opts CopyGuardSettingsOptions) (string, bool) {
	if opts.Pending {
		return "", false
	}

	want := catalog.NormalizedSettingValue(value)
	if prev, ok := opts.Handoff[key]; ok && prev == want {
		// Already handed off to verified equivalent permanent
		// configuration: a later deliberate change is respected.
		return prev, true
	}

	proof := opts.HandoffPermanent
	if proof == nil {
		proof = opts.Permanent
	}

	if permanent, supplied := proof[key]; supplied && permanent == want {
		return want, true
	}

	return "", false
}

func mergeEffective(effective map[string]string, owners map[string]copySettingOwner, key, value string, owner copySettingOwner) error {
	if prev, ok := effective[key]; ok {
		prevOwner := owners[key]
		if catalog.NormalizedSettingValue(prev) != catalog.NormalizedSettingValue(value) {
			return output.NewError(output.CodePrecondition,
				"extension settings conflict on "+key+": "+prev+" ("+prevOwner.name+") and "+value+" ("+owner.name+")",
				output.ExitPrecondition,
				"Keep one effective value in the current intent and retained history; the copy was not started.").
				WithDetails(map[string]any{"reason": "extension_setting_conflict", "setting": key, "extensions": []string{prevOwner.name, owner.name}})
		}

		if owner.mandatory {
			owners[key] = owner
		}

		return nil
	}

	effective[key], owners[key] = value, owner

	return nil
}

func parseCopyGuard(raw string) (CopyGuard, bool, error) {
	block, closed, present := copyGuardBlock(strings.Split(raw, "\n"))
	if !present {
		return CopyGuard{}, false, nil
	}

	g := CopyGuard{Settings: map[string]string{}}
	for _, line := range block {
		if err := applyCopyGuardLine(&g, line); err != nil {
			return CopyGuard{}, false, err
		}
	}

	if !closed {
		return CopyGuard{}, false, fmt.Errorf("the vaire copy guard block has no end marker")
	}

	if len(g.Settings) == 0 {
		g.Settings = nil
	}

	return g, true, nil
}

// copyGuardBlock returns the lines inside the first guard block. present is
// false when there is no block; closed is false when the end marker is missing.
func copyGuardBlock(lines []string) (block []string, closed, present bool) {
	begin := -1

	for i, line := range lines {
		if strings.TrimSpace(line) == copyGuardBegin {
			begin = i
			break
		}
	}

	if begin < 0 {
		return nil, false, false
	}

	for i := begin + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == copyGuardEnd {
			return lines[begin+1 : i], true, true
		}
	}

	return lines[begin+1:], false, true
}

func applyCopyGuardLine(g *CopyGuard, line string) error {
	trimmed := strings.TrimSpace(line)
	if after, ok := strings.CutPrefix(trimmed, copyGuardOriginsPrefix); ok {
		origins, err := parseCopyGuardOrigins(strings.TrimSpace(after))
		if err != nil {
			return err
		}

		g.Origins = origins

		return nil
	}

	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil
	}

	tok, ok, err := lexConfigLine(trimmed)
	if err != nil {
		return fmt.Errorf("the vaire copy guard line %q is not a setting: %w", trimmed, err)
	}

	if !ok {
		return nil
	}

	if tok.Name == "shared_preload_libraries" {
		g.Preloads = splitSettingList(tok.Value)
		return nil
	}

	g.Settings[tok.Name] = tok.RawValue

	return nil
}

func parseCopyGuardOrigins(raw string) (map[string]string, error) {
	if raw == "" {
		return nil, nil
	}

	out := map[string]string{}

	for part := range strings.SplitSeq(raw, ",") {
		lib, origin, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok || lib == "" || origin == "" {
			return nil, fmt.Errorf("the vaire copy guard origin %q is malformed", part)
		}

		switch origin {
		case "retained", "desired", "operator":
		default:
			return nil, fmt.Errorf("the vaire copy guard origin %q is unknown", origin)
		}

		out[lib] = origin
	}

	return out, nil
}

// stripCopyGuard removes the block, leaving every other line in order.
func stripCopyGuard(raw string) string {
	lines := strings.SplitAfter(raw, "\n")
	out := make([]string, 0, len(lines))
	in := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == copyGuardBegin:
			in = true
			continue
		case in && trimmed == copyGuardEnd:
			in = false
			continue
		case in:
			continue
		}

		out = append(out, line)
	}

	return strings.Join(out, "")
}

// stripSettings removes assignments of managed keys from raw.
func splitSettingList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}

	var out []string

	for part := range strings.SplitSeq(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}

	return out
}

// EffectivePreloads reads the shared_preload_libraries a cluster would load
// from postgresql.conf, then conf.d in name order, then postgresql.auto.conf,
// which is the order PostgreSQL reads them. A missing file is empty.
func EffectivePreloads(confDir, dataDir string) ([]string, error) {
	return effectivePreloads(confDir, true, dataDir)
}

// EffectiveConfPreloads is EffectivePreloads without postgresql.auto.conf: the
// preloads the permanent configuration (postgresql.conf and conf.d) supplies.
func EffectiveConfPreloads(confDir string) ([]string, error) {
	return effectivePreloads(confDir, false, "")
}

func effectivePreloads(confDir string, withAuto bool, dataDir string) ([]string, error) {
	value := ""
	if err := readPreloadValue(filepath.Join(confDir, "postgresql.conf"), &value); err != nil {
		return nil, err
	}

	names, err := confDConfigNames(confDir)
	if err != nil {
		return nil, err
	}

	for _, name := range names {
		if err := readPreloadValue(filepath.Join(confDir, "conf.d", name), &value); err != nil {
			return nil, err
		}
	}

	if withAuto {
		if err := readPreloadValue(autoConfPath(dataDir), &value); err != nil {
			return nil, err
		}
	}

	return splitSettingList(value), nil
}

// readPreloadValue replaces value with the last shared_preload_libraries
// assignment in path. A missing file leaves value unchanged.
func readPreloadValue(path string, value *string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	tokens, err := lexConfigLines(string(raw))
	if err != nil {
		return err
	}

	for _, tok := range tokens {
		if tok.Name == "shared_preload_libraries" {
			*value = tok.Value
		}
	}

	return nil
}

// confDConfigNames lists confDir/conf.d's named configuration files in the
// order PostgreSQL reads them.
func confDConfigNames(confDir string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(confDir, "conf.d"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".conf") {
			names = append(names, e.Name())
		}
	}

	sort.Strings(names)

	return names, nil
}

// EffectivePermanentSettings reads the underlying permanent configuration a
// cluster would start with: postgresql.conf, conf.d in name order, then the
// plain assignments in postgresql.auto.conf, excluding the generated vaire
// guard block. It is the proof used before releasing a mandatory suppression;
// an include form it cannot resolve is unknown state, never an empty value.
func EffectivePermanentSettings(confDir, dataDir string) (map[string]string, error) {
	out := map[string]string{}
	if err := readPermanentSettings(filepath.Join(confDir, "postgresql.conf"), out, confDir, true); err != nil {
		return nil, err
	}

	raw, err := os.ReadFile(autoConfPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}

		return nil, err
	}

	if err := parsePermanentSettings(stripCopyGuard(string(raw)), out); err != nil {
		return nil, fmt.Errorf("%s: %w", autoConfPath(dataDir), err)
	}

	return out, nil
}

func readPermanentSettings(path string, out map[string]string, confDir string, allowIncludeDir bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return err
	}

	tokens, err := lexConfigLines(string(raw))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}

	for _, tok := range tokens {
		switch tok.Name {
		case "include", "include_if_exists":
			return fmt.Errorf("%s: configuration include %q cannot be proven", path, tok.RawValue)
		case "include_dir":
			if !allowIncludeDir || tok.Value != "conf.d" {
				return fmt.Errorf("%s: configuration include_dir %q cannot be proven", path, tok.Value)
			}

			if err := readConfDSettings(confDir, out); err != nil {
				return err
			}

			continue
		}

		out[tok.Name] = tok.Value
	}

	return nil
}

func readConfDSettings(confDir string, out map[string]string) error {
	names, err := confDConfigNames(confDir)
	if err != nil {
		return err
	}

	for _, name := range names {
		if err := readPermanentSettings(filepath.Join(confDir, "conf.d", name), out, "", false); err != nil {
			return err
		}
	}

	return nil
}

func parsePermanentSettings(raw string, out map[string]string) error {
	tokens, err := lexConfigLines(raw)
	if err != nil {
		return err
	}

	for _, tok := range tokens {
		switch tok.Name {
		case "include", "include_if_exists":
			return fmt.Errorf("configuration include %q cannot be proven", tok.RawValue)
		case "include_dir":
			return fmt.Errorf("configuration include_dir %q cannot be proven in postgresql.auto.conf", tok.Value)
		}

		out[tok.Name] = tok.Value
	}

	return nil
}

// configToken is one decoded assignment from a PostgreSQL configuration
// file. Name is ASCII-lowercased; Value is decoded exactly once and preserves
// its case and quoted whitespace; RawValue keeps the token text for callers
// that must serialize PostgreSQL text.
type configToken struct {
	Name     string
	Value    string
	RawValue string
}

// lexConfigLine parses one PostgreSQL configuration line: optional '=', an
// ASCII case-insensitive name, a single-quoted value with doubled quotes and
// supported backslash escapes, or a bare value, followed only by whitespace
// and a comment outside quotes. Unsupported syntax fails closed.
func lexConfigLine(line string) (configToken, bool, error) {
	s := strings.TrimSpace(line)
	if s == "" || strings.HasPrefix(s, "#") {
		return configToken{}, false, nil
	}

	name, i, err := configLineName(s, line)
	if err != nil {
		return configToken{}, false, err
	}

	i = skipConfigSpace(s, i)
	if i < len(s) && s[i] == '=' {
		i = skipConfigSpace(s, i+1)
	}

	if i >= len(s) || s[i] == '#' {
		return configToken{}, false, fmt.Errorf("configuration line %q has no value", line)
	}

	rawStart := i

	var value string
	if s[i] == '\'' {
		value, i, err = decodeQuotedConfigValue(s, line, i)
	} else {
		value, i, err = decodeBareConfigValue(s, line, i)
	}

	if err != nil {
		return configToken{}, false, err
	}

	rawEnd := i

	i = skipConfigSpace(s, i)
	if i < len(s) && s[i] != '#' {
		return configToken{}, false, fmt.Errorf("configuration line %q has trailing text %q", line, s[i:])
	}

	return configToken{Name: name, Value: value, RawValue: s[rawStart:rawEnd]}, true, nil
}

func configLineName(s, line string) (string, int, error) {
	i := 0
	for i < len(s) && s[i] != '=' && !isConfigSpace(s[i]) {
		i++
	}

	if i == 0 {
		return "", 0, fmt.Errorf("configuration line %q has no setting name", line)
	}

	return asciiLower(s[:i]), i, nil
}

func skipConfigSpace(s string, i int) int {
	for i < len(s) && isConfigSpace(s[i]) {
		i++
	}

	return i
}

func decodeQuotedConfigValue(s, line string, i int) (string, int, error) {
	i++

	var b strings.Builder

	for i < len(s) {
		c := s[i]
		switch {
		case c == '\'' && i+1 < len(s) && s[i+1] == '\'':
			b.WriteByte('\'')

			i += 2
		case c == '\'':
			return b.String(), i + 1, nil
		case c == '\\':
			decoded, next, err := decodeConfigEscape(s, i)
			if err != nil {
				return "", i, fmt.Errorf("configuration line %q: %w", line, err)
			}

			b.WriteByte(decoded)

			i = next
		default:
			b.WriteByte(c)

			i++
		}
	}

	return "", i, fmt.Errorf("configuration line %q has an unterminated quoted value", line)
}

func decodeBareConfigValue(s, line string, i int) (string, int, error) {
	start := i
	for i < len(s) && !isConfigSpace(s[i]) && s[i] != '#' {
		i++
	}

	if i == start {
		return "", i, fmt.Errorf("configuration line %q has no value", line)
	}

	return s[start:i], i, nil
}

func isConfigSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}

	return string(b)
}

func decodeConfigEscape(s string, i int) (byte, int, error) {
	if i+1 >= len(s) {
		return 0, i, fmt.Errorf("trailing backslash in quoted value")
	}

	switch s[i+1] {
	case '\\':
		return '\\', i + 2, nil
	case '\'':
		return '\'', i + 2, nil
	case '"':
		return '"', i + 2, nil
	case 'n':
		return '\n', i + 2, nil
	case 'r':
		return '\r', i + 2, nil
	case 't':
		return '\t', i + 2, nil
	case 'b':
		return '\b', i + 2, nil
	case 'f':
		return '\f', i + 2, nil
	case 'v':
		return '\v', i + 2, nil
	default:
		return 0, i, fmt.Errorf("unsupported backslash escape %q", s[i:i+2])
	}
}

func lexConfigLines(raw string) ([]configToken, error) {
	var out []configToken

	for line := range strings.SplitSeq(raw, "\n") {
		tok, ok, err := lexConfigLine(line)
		if err != nil {
			return nil, err
		}

		if ok {
			out = append(out, tok)
		}
	}

	return out, nil
}

// SelectedBackupStart is the start of the full backup at the root of the set a
// restore would use: the base backup whose WAL and replay history the restored
// data spans. ok is false when the selection or its prior chain is unknown;
// callers then treat historical coverage as unproven.
func SelectedBackupStart(info Info, opts RestoreOptions) (time.Time, bool) {
	selected, ok := selectedBackupSet(info, opts)
	if !ok {
		return time.Time{}, false
	}

	return fullBackupStart(info, selected)
}

func selectedBackupSet(info Info, opts RestoreOptions) (Set, bool) {
	switch opts.Kind {
	case TargetTime:
		for _, set := range slices.Backward(info.Sets) {
			if set.Error {
				continue
			}

			start, err := time.Parse(time.RFC3339, set.Start)
			if err != nil {
				return Set{}, false
			}

			if !start.After(opts.Time) {
				return set, true
			}
		}

		return Set{}, false
	case TargetSet:
		return info.Find(opts.Set)
	case TargetLatest, "":
		return info.Latest()
	default:
		return Set{}, false
	}
}

func fullBackupStart(info Info, selected Set) (time.Time, bool) {
	seen := map[string]bool{}

	for range 100 {
		if selected.Type == TypeFull {
			start, err := time.Parse(time.RFC3339, selected.Start)
			if err != nil {
				return time.Time{}, false
			}

			return start, true
		}

		if selected.Prior == "" || seen[selected.Label] {
			return time.Time{}, false
		}

		seen[selected.Label] = true

		prior, found := info.Find(selected.Prior)
		if !found || prior.Error {
			return time.Time{}, false
		}

		selected = prior
	}

	return time.Time{}, false
}

// MergeExtensionRequirements resolves the compatible union of known (current
// intent and retained history) and declared descriptors without requiring a
// copy safety declaration. It is the ordinary provider/settings validation a
// standalone primary uses; the copy/standby gate is separate and applies only
// when a copy or follower will actually load the data.
func MergeExtensionRequirements(known, declared []catalog.Extension) ([]catalog.Extension, error) {
	merged, err := mergeKnownRequirements(known)
	if err != nil {
		return nil, err
	}

	for _, ext := range declared {
		next, err := mergeOneRequirement(merged, ext, true)
		if err != nil {
			return nil, fmt.Errorf("declared recovery requirement %s conflicts with known history: %w", ext.Name, err)
		}

		merged = next
	}

	return merged, nil
}

// MergeCopyRequirements resolves the compatible union of known (current intent
// and retained history) and declared requirements for one copy. Known history
// is authoritative: a declaration cannot override or arbitrate a conflict in
// it. The returned guard is what must win in the copy's auto.conf.
func MergeCopyRequirements(known, declared []catalog.Extension, standby bool, copyDB string) ([]catalog.Extension, CopyGuard, error) {
	merged, err := MergeExtensionRequirements(known, declared)
	if err != nil {
		return nil, CopyGuard{}, err
	}

	guard, err := copyGuardFor(merged, standby, copyDB)
	if err != nil {
		return nil, CopyGuard{}, err
	}

	return merged, guard, nil
}

func mergeKnownRequirements(exts []catalog.Extension) ([]catalog.Extension, error) {
	var out []catalog.Extension
	for _, ext := range exts {
		next, err := mergeOneRequirement(out, ext, false)
		if err != nil {
			return nil, err
		}

		out = next
	}

	return out, nil
}

func mergeOneRequirement(out []catalog.Extension, ext catalog.Extension, declared bool) ([]catalog.Extension, error) {
	ext = ext.Clone()

	for i := range out {
		have := out[i]

		done, err := mergeSameExtName(out, i, ext, declared)
		if err != nil {
			return nil, err
		}

		if done {
			return out, nil
		}

		if ext.Preload != "" && have.Preload == ext.Preload && !sameProvider(have, ext) {
			return nil, providerConflict(have, ext)
		}
	}

	if containsSemanticExtension(out, ext) {
		return out, nil
	}

	return append(out, ext), nil
}

// mergeSameExtName merges ext into the descriptor at out[i] when both name the
// same extension. done says the existing descriptor already satisfies ext.
func mergeSameExtName(out []catalog.Extension, i int, ext catalog.Extension, declared bool) (done bool, err error) {
	have := out[i]
	if ext.ExtName == "" || have.ExtName != ext.ExtName {
		return false, nil
	}

	if have.Source != ext.Source || have.Package != ext.Package {
		return false, providerConflict(have, ext)
	}

	if have.Preload != "" && ext.Preload != "" && have.Preload != ext.Preload {
		return false, providerConflict(have, ext)
	}

	if have.Preload == "" {
		out[i].Preload = ext.Preload
	}

	return !declared && have.SemanticEqual(ext), nil
}

func sameProvider(a, b catalog.Extension) bool {
	return a.Name == b.Name && a.Source == b.Source && a.Package == b.Package
}

func containsSemanticExtension(out []catalog.Extension, ext catalog.Extension) bool {
	for _, have := range out {
		if have.SemanticEqual(ext) {
			return true
		}
	}

	return false
}

func providerConflict(a, b catalog.Extension) error {
	return output.NewError(output.CodePrecondition,
		"extension requirements conflict: "+a.Name+" ("+providerText(a)+") and "+b.Name+" ("+providerText(b)+")",
		output.ExitPrecondition,
		"A copy cannot load two providers of the same extension or library. Keep one provider in the history, or choose a restore point before the provider changed.").
		WithDetails(map[string]any{"reason": "extension_provider_conflict", "extensions": []string{a.Name, b.Name}})
}

func providerText(ext catalog.Extension) string {
	switch {
	case ext.Package != "":
		return ext.Source + " package " + ext.Package + " preload " + ext.Preload
	case ext.Preload != "":
		return ext.Source + " preload " + ext.Preload
	default:
		return ext.Source
	}
}

func copyGuardFor(exts []catalog.Extension, standby bool, copyDB string) (CopyGuard, error) {
	guard := CopyGuard{Settings: map[string]string{}}

	seenPreloads := map[string]bool{}
	for _, ext := range exts {
		// Descriptor order is runtime load order; a set plus a later sort would
		// erase a native dependency (for example pg_stat_statements before a
		// custom pg_stat_kcache).
		if ext.Preload != "" && !seenPreloads[ext.Preload] {
			seenPreloads[ext.Preload] = true
			guard.Preloads = append(guard.Preloads, ext.Preload)
		}

		if err := addCopySafetySettings(&guard, ext, copyDB); err != nil {
			return CopyGuard{}, err
		}

		if standby && !ext.StandbySafe {
			return CopyGuard{}, output.NewError(output.CodePrecondition,
				"extension "+ext.Name+" is not declared standby_safe, and this copy starts as a physical standby",
				output.ExitPrecondition,
				"Declare standby_safe in [extensions."+ext.Name+"] only when its native code is safe on a standby; otherwise remove it from the restore or follow.").
				WithDetails(map[string]any{"reason": "extension_standby_unsafe", "extension": ext.Name})
		}
	}

	if len(guard.Settings) == 0 {
		guard.Settings = nil
	}

	return guard, nil
}

func addCopySafetySettings(guard *CopyGuard, ext catalog.Extension, copyDB string) error {
	switch ext.CopySafety {
	case catalog.CopySafetySafe:
		return nil
	case catalog.CopySafetyDisabled:
		settings := ext.ResolvedCopySettings(copyDB)
		if len(settings) == 0 {
			return output.NewError(output.CodePrecondition,
				"extension "+ext.Name+" declares copy_safety disabled without copy_settings",
				output.ExitPrecondition,
				"Add the settings that suppress its background work, or declare it safe.").
				WithDetails(map[string]any{"reason": "extension_copy_safety_incomplete", "extension": ext.Name})
		}

		for k, v := range settings {
			if prev, ok := guard.Settings[k]; ok && catalog.NormalizedSettingValue(prev) != catalog.NormalizedSettingValue(v) {
				return output.NewError(output.CodePrecondition,
					"extension copy settings conflict on "+k+": "+prev+" and "+v,
					output.ExitPrecondition,
					"Two required extensions cannot suppress the same setting differently. Keep one provider or one safe definition.").
					WithDetails(map[string]any{"reason": "extension_copy_setting_conflict", "setting": k, "extensions": []string{ext.Name}})
			}

			guard.Settings[k] = v
		}

		return nil
	default:
		return output.NewError(output.CodePrecondition,
			"extension "+ext.Name+" has no copy safety declaration",
			output.ExitPrecondition,
			"Declare copy_safety safe or disabled with copy_settings in its [extensions."+ext.Name+"] definition; an unknown native extension cannot be started in a copy.").
			WithDetails(map[string]any{"reason": "extension_copy_safety_unknown", "extension": ext.Name})
	}
}

// CopyPlan is the historical and copy-safety decision for one materialised
// cluster: the guard that must win in its auto.conf, the resolved requirements
// for package/file verification, and the evidence the caller reports. A nil
// plan means today's behavior (no historical requirements, no guard).
type CopyPlan struct {
	Guard         CopyGuard
	Requirements  []catalog.Extension
	Stanza        string
	CoverageSince string
	// DesiredPreloads are current intent's preloads. They are not retained
	// history: the guard composer refreshes them on every rewrite.
	DesiredPreloads []string
	// Historical records that the data came from a stanza point, not from a
	// live base backup.
	Historical bool
	// Cron says the built-in pg_cron launch guard and settlement apply.
	Cron bool
	// Declaration is "absent", "empty" or "populated" for diagnostics.
	Declaration string
	// ValidateGuard checks the final composed guard (preloads and settings)
	// against explicit resolved copy/standby metadata before any guard is
	// installed and the copy first starts. It sees the complete union,
	// including the operator's own configuration; an unknown or ambiguous
	// library refuses.
	ValidateGuard func(CopyGuard) error
}

// InstallCopyPlanGuard composes the plan's complete guard, validates the final
// union, then installs it. The validation happens after composition, so it
// covers a library the operator's own configuration adds.
func InstallCopyPlanGuard(confDir, dataDir string, plan *CopyPlan) (CopyGuard, bool, error) {
	if plan == nil {
		return CopyGuard{}, false, nil
	}

	g, err := ComposeCopyGuard(confDir, dataDir, plan.Guard, plan.DesiredPreloads)
	if err != nil {
		return CopyGuard{}, false, err
	}

	if plan.ValidateGuard != nil {
		if err := plan.ValidateGuard(g); err != nil {
			return CopyGuard{}, false, err
		}
	}

	changed, err := InstallCopyGuard(dataDir, g)

	return g, changed, err
}

// InstallCopyPlan installs the plan's complete guard: retained requirements,
// current desired preloads and the operator's own effective list. The guard
// never writes a partial replacement that shadows a current or default library.
func (m *Materialised) InstallCopyPlan() error {
	if m.Plan == nil {
		return nil
	}

	_, _, err := InstallCopyPlanGuard(m.ConfDir(), m.Data, m.Plan)

	return err
}
