package backup

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
)

func localCopyGuardWriter(t *testing.T) {
	t.Helper()
	old := writeCopyGuardFile
	writeCopyGuardFile = func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm) //nolint:gosec // G304: test-owned path
	}
	t.Cleanup(func() { writeCopyGuardFile = old })
}

// A custom pg_stat_kcache has no built-in catalogue entry or alias. Its native
// code needs pg_stat_statements loaded first, so the guard must keep the
// resolver's load order instead of sorting the library strings.
func customStatKcache() catalog.Extension {
	return catalog.Extension{
		Name: "pg_stat_kcache", ExtName: "pg_stat_kcache",
		Source: catalog.SourcePreinstalled, Preload: "pg_stat_kcache",
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}
}

func TestComposeCopyGuardKeepsDesiredLoadOrderAheadOfLegacyLexicalGuard(t *testing.T) {
	localCopyGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_stat_kcache'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The pre-fix guard sorted the same set lexically. No pg_stat_statements.max
	// setting masks the bad order: the guard itself must load stats first.
	legacy := []string{"auto_explain", "pg_stat_kcache", "pg_stat_statements"}
	if _, err := InstallCopyGuard(dataDir, CopyGuard{
		Preloads: legacy,
		Origins:  map[string]string{"auto_explain": "desired", "pg_stat_kcache": "desired", "pg_stat_statements": "desired"},
	}); err != nil {
		t.Fatal(err)
	}
	installed, present, err := ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("installed guard = %+v, %v, %v", installed, present, err)
	}
	if !slices.Equal(installed.Preloads, legacy) {
		t.Fatalf("legacy guard readback reordered preloads: %v", installed.Preloads)
	}
	desired := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache"}
	composed, err := ComposeCopyGuard(confDir, dataDir, CopyGuard{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(composed.Preloads, desired) {
		t.Fatalf("composed preloads = %v, want %v", composed.Preloads, desired)
	}
	if composed.Settings != nil {
		t.Fatalf("the preload-order repair added a settings workaround: %v", composed.Settings)
	}
	// The set is unchanged but the load order differs, so an existing
	// order-sensitive Check must see drift instead of normalizing the guard.
	if slices.Equal(installed.Preloads, composed.Preloads) {
		t.Fatal("the lexical legacy guard was normalized away instead of reported as changed")
	}
	if _, err := InstallCopyGuard(dataDir, composed); err != nil {
		t.Fatal(err)
	}
	got, present, err := ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", got, present, err)
	}
	if !slices.Equal(got.Preloads, desired) {
		t.Fatalf("guard readback reordered preloads: %v, want %v", got.Preloads, desired)
	}
	if got.Settings != nil {
		t.Fatalf("guard readback added settings: %v", got.Settings)
	}
}

func TestComposeCopyGuardKeepsRetainedOriginAndCompleteUnionOrder(t *testing.T) {
	localCopyGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "90-operator.conf"),
		[]byte("shared_preload_libraries = 'operator_lib'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.auto.conf"),
		[]byte("shared_preload_libraries = 'plain_auto_lib'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// auto_explain is both real retained evidence and current desired intent.
	// Desired is inserted first for load order; its origin must stay retained.
	base := CopyGuard{Preloads: []string{"auto_explain", "retained_lib"}}
	desired := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache"}
	got, err := ComposeCopyGuard(confDir, dataDir, base, desired)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache", "retained_lib", "operator_lib", "plain_auto_lib"}
	if !slices.Equal(got.Preloads, want) {
		t.Fatalf("preloads = %v, want %v", got.Preloads, want)
	}
	wantOrigins := map[string]string{
		"pg_stat_statements": "desired", "auto_explain": "retained",
		"pg_stat_kcache": "desired", "retained_lib": "retained",
		"operator_lib": "operator", "plain_auto_lib": "operator",
	}
	if !maps.Equal(got.Origins, wantOrigins) {
		t.Fatalf("origins = %v, want %v", got.Origins, wantOrigins)
	}
	if _, err := InstallCopyGuard(dataDir, got); err != nil {
		t.Fatal(err)
	}
	read, present, err := ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", read, present, err)
	}
	if !slices.Equal(read.Preloads, want) || !maps.Equal(read.Origins, wantOrigins) {
		t.Fatalf("guard readback = %+v, want preloads %v origins %v", read, want, wantOrigins)
	}
}

func TestComposeCopyGuardRetainedOriginSurvivesReadback(t *testing.T) {
	localCopyGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	desired := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache"}
	composed, err := ComposeCopyGuard(confDir, dataDir, CopyGuard{Preloads: []string{"pg_stat_kcache"}}, desired)
	if err != nil {
		t.Fatal(err)
	}
	if composed.Origins["pg_stat_kcache"] != "retained" {
		t.Fatalf("desired-first composition downgraded retained evidence: %v", composed.Origins)
	}
	if _, err := InstallCopyGuard(dataDir, composed); err != nil {
		t.Fatal(err)
	}
	// A later rewrite without the requirement must refuse to erase the
	// retained library. A downgraded desired origin would silently unload it.
	if _, err := ComposeCopyGuard(confDir, dataDir, CopyGuard{}, []string{"pg_stat_statements", "auto_explain"}); err == nil {
		t.Fatal("retained evidence was downgraded and silently unloaded")
	}
}

func TestCopyGuardForPreservesDescriptorLoadOrder(t *testing.T) {
	stats, ok := catalog.Lookup("pg_stat_statements")
	if !ok {
		t.Fatal("pg_stat_statements is missing from the catalogue")
	}
	auto, ok := catalog.Lookup("auto_explain")
	if !ok {
		t.Fatal("auto_explain is missing from the catalogue")
	}
	kcache := customStatKcache()
	exts := []catalog.Extension{stats, auto, kcache, stats, kcache}
	want := []string{"pg_stat_statements", "auto_explain", "pg_stat_kcache"}

	guard, err := copyGuardFor(exts, false, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(guard.Preloads, want) {
		t.Fatalf("guard preloads = %v, want descriptor order %v", guard.Preloads, want)
	}

	_, mergedGuard, err := MergeCopyRequirements(exts, nil, false, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(mergedGuard.Preloads, want) {
		t.Fatalf("merged guard preloads = %v, want descriptor order %v", mergedGuard.Preloads, want)
	}
}
