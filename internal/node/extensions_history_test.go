package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

func useTempExtensionRecords(t *testing.T) {
	t.Helper()

	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
}

func planCfg(t *testing.T) config.Cluster {
	t.Helper()

	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", StatementTimeout: "60s"}}

	return cfg
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
	return slices.Contains(items, want)
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

func TestConfigSelectionReductionIsPure(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Extensions: []string{"pg_trgm", "pg_repack"}}}
	updated := cfg.Clone()
	updated.Clusters[0].Extensions = []string{"pg_trgm"}
	oldRecords, oldInstalled, oldRunning := extensionRecords, extensionInstalled, extensionClusterRunning
	extensionRecords = func() (backup.Records, error) {
		t.Fatal("pure selection reduction read records")
		return backup.Records{}, nil
	}
	extensionClusterRunning = func(config.ClusterSpec) bool {
		t.Fatal("pure selection reduction read runtime state")
		return true
	}
	extensionInstalled = func(context.Context, config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		t.Fatal("pure selection reduction read the installed SQL catalog")
		return nil, nil
	}

	t.Cleanup(func() {
		extensionRecords, extensionInstalled, extensionClusterRunning = oldRecords, oldInstalled, oldRunning
	})

	err := refuseExtensionSelectionReduction(cfg, updated)
	if err == nil || extReason(t, err) != "extension_remove_unsupported" {
		t.Fatalf("config removal = %v", err)
	}

	updated.Clusters[0].Extensions = []string{"pg_trgm", "pg_repack"}
	if err := refuseExtensionSelectionReduction(cfg, updated); err != nil {
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
			CopySafety: new(catalog.CopySafetySafe), StandbySafe: new(true)},
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
