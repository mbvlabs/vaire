package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/state"
)

func useTempExtensionRecords(t *testing.T) {
	t.Helper()
	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
}

func planCfg(t *testing.T, exts ...string) config.Cluster {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s", Extensions: exts}}
	return cfg
}

func TestBuildCopyPlanDumpRequiresDeclaration(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	req := extensionCopyRequest{Intent: spec, CopyDB: "app", Dump: true, Historical: true, Standby: false}
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, req); err == nil || !strings.Contains(err.Error(), "recovery_extensions") {
		t.Fatalf("absent dump declaration = %v", err)
	}
	empty := []string{}
	req.Declaration = &empty
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, req)
	if err != nil || plan.Plan.Declaration != "empty" {
		t.Fatalf("explicit empty declaration = %+v, %v", plan, err)
	}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext", CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true)},
	}
	declared := []string{"custom"}
	req.Declaration = &declared
	plan, err = buildExtensionCopyPlan(context.Background(), cfg, req)
	if err != nil || !hasMerged(plan.Merged, "custom") {
		t.Fatalf("populated declaration = %+v, %v", plan, err)
	}
}

func hasMerged(exts []catalog.Extension, name string) bool {
	for _, ext := range exts {
		if ext.Name == name {
			return true
		}
	}
	return false
}

func TestBuildCopyPlanHistoricalCoverageUsesBaseBackupRoot(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	root := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	oldInfo := extensionBackupInfo
	extensionBackupInfo = func(context.Context, config.ClusterSpec) (backup.Info, error) {
		return backup.Info{Sets: []backup.Set{
			{Label: "f1", Type: backup.TypeFull, Start: root.Format(time.RFC3339)},
			{Label: "d1", Type: backup.TypeDiff, Prior: "f1", Start: root.Add(24 * time.Hour).Format(time.RFC3339)},
		}}, nil
	}
	t.Cleanup(func() { extensionBackupInfo = oldInfo })
	req := extensionCopyRequest{Intent: spec, Stanza: spec.Stanza, CopyDB: "app", Historical: true, Standby: true,
		Options: backup.RestoreOptions{Kind: backup.TargetSet, Set: "d1"}}
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, req); err == nil || !strings.Contains(err.Error(), "recovery_extensions") {
		t.Fatalf("uncovered point = %v", err)
	}
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{{
		Name: "pg_trgm", ExtName: "pg_trgm", Source: catalog.SourcePreinstalled,
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}, root.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, req)
	if err != nil || plan.Plan.CoverageSince == "" {
		t.Fatalf("covered point = %+v, %v", plan, err)
	}
	// A target time after the coverage start but whose selected base backup
	// started before it is still uncovered.
	if err := backup.RecordExtensionRequirements("app-late", "app", []catalog.Extension{{
		Name: "pg_trgm", ExtName: "pg_trgm", Source: catalog.SourcePreinstalled,
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}, root.Add(12*time.Hour)); err != nil {
		t.Fatal(err)
	}
	req.Stanza = "app-late"
	req.Options = backup.RestoreOptions{Kind: backup.TargetTime, Time: root.Add(30 * time.Hour)}
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, req); err == nil {
		t.Fatal("a target after coverage hid a base backup from before it")
	}
}

func TestBuildCopyPlanKeepsRetainedLibraryNotInIntent(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"old_ext": {Source: catalog.SourcePreinstalled, ExtName: "old_ext", Preload: "old_lib"},
	}
	ext := catalog.Extension{Name: "old_ext", ExtName: "old_ext", Source: catalog.SourcePreinstalled,
		Preload: "old_lib", CopySafety: catalog.CopySafetySafe, StandbySafe: true}
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{ext}, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, Stanza: spec.Stanza, CopyDB: "app", IncludeRetained: true, Standby: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasMerged(plan.Merged, "old_ext") || !contains(plan.Plan.Guard.Preloads, "old_lib") {
		t.Fatalf("retained library vanished: %+v %+v", plan.Merged, plan.Plan.Guard)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestFollowerIntentPropagationAndDirectEditRefusal(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Extensions: []string{"pg_trgm"}},
		{Name: "app_follower", Follows: "app", Extensions: []string{"pg_trgm"}},
	}
	updated := cfg.Clone()
	updated.Clusters[0].Extensions = []string{"pg_trgm", "citext"}
	propagated, changed, err := propagateFollowerExtensions(cfg, updated)
	if err != nil || !changed || !slicesEqual(propagated.Clusters[1].Extensions, []string{"pg_trgm", "citext"}) {
		t.Fatalf("propagation = %+v, %v, %v", propagated, changed, err)
	}
	direct := cfg.Clone()
	direct.Clusters[1].Extensions = []string{"pg_trgm", "citext"}
	if _, _, err := propagateFollowerExtensions(cfg, direct); err == nil {
		t.Fatal("a direct follower extension edit was accepted")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestConfigRemovalRequiresExtRemove(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Extensions: []string{"pg_trgm", "pg_repack"}}}
	updated := cfg.Clone()
	updated.Clusters[0].Extensions = []string{"pg_trgm"}
	oldRecords, oldInstalled, oldRunning := extensionRecords, extensionInstalled, extensionClusterRunning
	extensionRecords = func() (backup.Records, error) { return backup.Records{}, nil }
	extensionClusterRunning = func(config.ClusterSpec) bool { return true }
	extensionInstalled = func(context.Context, config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{"pg_repack": {Name: "pg_repack", Version: "1.5"}}, nil
	}
	t.Cleanup(func() {
		extensionRecords, extensionInstalled, extensionClusterRunning = oldRecords, oldInstalled, oldRunning
	})
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), cfg, updated)
	if err == nil || extReason(t, err) != "extension_remove_required" {
		t.Fatalf("config removal without DROP = %v", err)
	}
	updated.Clusters[0].Extensions = []string{"pg_trgm", "pg_repack"}
	if err := refuseUnsafeExtensionConfigRemoval(context.Background(), cfg, updated); err != nil {
		t.Fatalf("a no-op removal was refused: %v", err)
	}
}

func TestRestoreFilesKeepsExtensionRequirements(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abc123"}}
	r, _ := backup.LoadRecords()
	r.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{{
		Name: "pg_trgm", ExtName: "pg_trgm", Source: catalog.SourcePreinstalled,
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}, time.Now())
	raw, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	b := masterbackup.Bundle{Files: []masterbackup.File{
		masterbackup.NewFile(masterbackup.ClusterPath, "root", "root", 0o640, []byte("old\n")),
		masterbackup.NewFile(masterbackup.RecordsPath, "root", "root", 0o600, raw),
	}}
	files, _, err := restoreFiles(b, cfg, "new\n", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.Path != masterbackup.RecordsPath {
			continue
		}
		var got backup.Records
		if err := json.Unmarshal(f.Data, &got); err != nil {
			t.Fatal(err)
		}
		if _, ok := got.ExtensionRequirementsFor("app-abc123"); !ok {
			t.Fatal("the rebuild bundle lost the stanza's requirements")
		}
		return
	}
	t.Fatal("records.json is missing from the restored files")
}

func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }

func TestDumpPromoteRetryKeepsFixedStanzaIdentities(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	oldRaw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(t.TempDir(), "cluster.toml")
	store := state.FileStore{Path: storePath}
	if err := store.Save(oldRaw); err != nil {
		t.Fatal(err)
	}
	oldStore := switchStanzaStore
	switchStanzaStore = func() state.Store { return store }
	t.Cleanup(func() { switchStanzaStore = oldStore })
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{ID: "r1", Database: "app", Kind: targetDump, Target: "dump x", Phase: backup.PhaseReady}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	recs, _ := backup.LoadRecords()
	r := recs.Restores["app"]
	spec := cfg.Clusters[0]
	gotCfg, gotR, newStanza, oldStanza, err := prepareDumpPromoteStanza(cfg, "app", spec, r)
	if err != nil {
		t.Fatal(err)
	}
	if newStanza == "" || newStanza == oldStanza || oldStanza != spec.Stanza {
		t.Fatalf("identities = new %q old %q spec %q", newStanza, oldStanza, spec.Stanza)
	}
	if gotR.NewStanza != newStanza || gotR.OldStanza != oldStanza || gotCfg.Clusters[0].Stanza != newStanza {
		t.Fatalf("first promote = %+v %+v", gotR, gotCfg.Clusters[0])
	}
	// Interruption before the stanza save: the record is fixed but cluster.toml
	// still names the old stanza. The retry reuses the recorded new stanza.
	if err := store.Save(oldRaw); err != nil {
		t.Fatal(err)
	}
	_, gotR, retryNew, retryOld, err := prepareDumpPromoteStanza(cfg, "app", spec, gotR)
	if err != nil {
		t.Fatal(err)
	}
	if retryNew != newStanza || retryOld != oldStanza {
		t.Fatalf("retry before save = new %q old %q, want %q/%q", retryNew, retryOld, newStanza, oldStanza)
	}
	loaded, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Clusters[0].Stanza != newStanza {
		t.Fatalf("retry switched to %q, want recorded %q", loaded.Clusters[0].Stanza, newStanza)
	}
	// Interruption after the stanza save: the same identities are reused.
	_, gotR, retryNew, retryOld, err = prepareDumpPromoteStanza(loaded, "app", spec, gotR)
	if err != nil {
		t.Fatal(err)
	}
	if retryNew != newStanza || retryOld != oldStanza {
		t.Fatalf("retry after save = new %q old %q", retryNew, retryOld)
	}
	recs, err = backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if rec := recs.Restores["app"]; rec.ID != "r1" || rec.NewStanza != newStanza || rec.OldStanza != oldStanza {
		t.Fatalf("a retry rewrote the restore's fixed identities: %+v", rec)
	}
}

func TestBuildCopyPlanKeepsHistoricalOrdinarySetting(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	hist := catalog.Extension{Name: "hist", ExtName: "hist", Source: catalog.SourcePreinstalled,
		Settings: map[string]string{"hist.mode": "on"}, CopySafety: catalog.CopySafetySafe, StandbySafe: true}
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{hist}, time.Now()); err != nil {
		t.Fatal(err)
	}
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, Stanza: spec.Stanza, CopyDB: "app", IncludeRetained: true, Standby: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Plan.Guard.Settings["hist.mode"] != "on" {
		t.Fatalf("historical ordinary setting vanished: %+v", plan.Plan.Guard)
	}
}

func TestBuildCopyPlanRefusesConflictingHistoricalSetting(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"cur": {Source: catalog.SourcePreinstalled, ExtName: "cur", Settings: map[string]string{"x": "2"},
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true)},
	}
	cfg.Clusters[0].Extensions = []string{"cur"}
	spec := cfg.Clusters[0]
	hist := catalog.Extension{Name: "hist", ExtName: "hist", Source: catalog.SourcePreinstalled,
		Settings: map[string]string{"x": "1"}, CopySafety: catalog.CopySafetySafe, StandbySafe: true}
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{hist}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, Stanza: spec.Stanza, CopyDB: "app", IncludeRetained: true, Standby: true,
	}); err == nil {
		t.Fatal("conflicting old/current ordinary settings were accepted")
	}
}

func TestDumpPromoteDoesNotRequireStandbySafe(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(false)},
	}
	cfg.Clusters[0].Extensions = []string{"custom"}
	spec := cfg.Clusters[0]
	empty := []string{}
	req := extensionCopyRequest{Intent: spec, CopyDB: "app", Dump: true, Historical: true, Standby: false, Declaration: &empty}
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, req); err != nil {
		t.Fatalf("a logical dump required standby_safe: %v", err)
	}
	req.Standby = true
	if _, err := buildExtensionCopyPlan(context.Background(), cfg, req); err == nil {
		t.Fatal("a physical copy accepted standby_safe=false")
	}
}

func TestUnboundDumpPlanHasNoStanzaOrHistory(t *testing.T) {
	useTempExtensionRecords(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	if err := backup.RecordExtensionRequirements(spec.Stanza, "app", []catalog.Extension{{
		Name: "old", ExtName: "old", Source: catalog.SourcePreinstalled,
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	empty := []string{}
	plan, err := buildExtensionCopyPlan(context.Background(), cfg, extensionCopyRequest{
		Intent: spec, CopyDB: "app", Dump: true, Historical: true, Standby: false, Declaration: &empty,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Plan.Stanza != "" || hasMerged(plan.Plan.Requirements, "old") {
		t.Fatalf("an unbound dump inherited stanza history: %+v", plan.Plan)
	}
}
