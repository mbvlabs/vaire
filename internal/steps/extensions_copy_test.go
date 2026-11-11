package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func testCopyGuardWriter(t *testing.T) {
	t.Helper()
	old := backup.CopyGuardWriterForTest()
	backup.RestoreCopyGuardWriterForTest(func(path string, data []byte, perm os.FileMode) error {
		return os.WriteFile(path, data, perm)
	})
	oldInstance := guardInstance
	guardInstance = func() (string, error) { return "test-instance", nil }
	t.Cleanup(func() {
		backup.RestoreCopyGuardWriterForTest(old)
		guardInstance = oldInstance
	})
}

func tempGuardRecords(t *testing.T) {
	t.Helper()
	old := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = old })
}

func TestDesiredExtensionSQLOnlyMissing(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"custom"}}
	sql, created, err := DesiredExtensionSQL(cfg, spec, map[string]bool{"pg_stat_statements": true}, "app")
	if err != nil || !strings.Contains(sql, `CREATE EXTENSION IF NOT EXISTS "custom_ext"`) || len(created) != 1 {
		t.Fatalf("missing SQL = %q, %v, %v", sql, created, err)
	}
	sql, created, err = DesiredExtensionSQL(cfg, spec, map[string]bool{"custom_ext": true, "pg_stat_statements": true}, "app")
	if err != nil || sql != "" || len(created) != 0 {
		t.Fatalf("installed extension was created again: %q, %v, %v", sql, created, err)
	}
}

func TestReconcileCopyGuardRemovesWhenCovered(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionControl(t, "custom_ext")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "custom_lib")
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	confDir := PGConfDir("app")
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'custom_lib,pg_stat_statements,auto_explain'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "50-vaire-settings.conf"),
		[]byte("custom.mode = on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true)},
	}
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"custom"}, Settings: map[string]string{"custom.mode": "on"}}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"custom_lib"}, Settings: map[string]string{"custom.mode": "on"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	if _, present, _ := backup.ReadCopyGuard(dir); present {
		t.Fatal("a covered guard stayed")
	}
}

func TestReconcileCopyGuardKeepsRetainedPreload(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "old_lib")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Stanza: "app-abc123", Extensions: []string{"pg_trgm"}}
	if err := backup.RecordExtensionRequirements(spec.Stanza, spec.Name, []catalog.Extension{{
		Name: "old", Source: catalog.SourcePreinstalled,
		Preload: "old_lib", CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{Preloads: []string{"old_lib"}}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	found := false
	for _, p := range guard.Preloads {
		if p == "old_lib" {
			found = true
		}
	}
	if !found {
		t.Fatalf("retained preload was removed: %+v", guard)
	}
}

func TestRetainedMandatorySuppressionSurvivesLaterApply(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "hist_lib")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Stanza: "app-abc123"}
	if err := backup.RecordExtensionRequirements(spec.Stanza, spec.Name, []catalog.Extension{{
		Name: "hist", Source: catalog.SourcePreinstalled, Preload: "hist_lib",
		CopySafety: catalog.CopySafetyDisabled, StandbySafe: true,
		CopySettings: map[string]string{"hist.worker": "off"},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	confDir := PGConfDir(spec.Name)
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"),
		[]byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writePermanent := func(value string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(confDir, "conf.d", "50-vaire-settings.conf"),
			[]byte("hist.worker = "+value+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"hist_lib"}, Settings: map[string]string{"hist.worker": "off"},
	}); err != nil {
		t.Fatal(err)
	}
	// A later ordinary config apply keeps the historical-only mandatory
	// suppression because the permanent configuration does not supply it.
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if guard.Settings["hist.worker"] != "off" {
		t.Fatalf("later apply dropped the retained suppression: %+v", guard)
	}
	// The operator supplies the equivalent permanent value: the guard hands
	// it off and releases it. The retained preload keeps the guard present.
	spec.Settings = map[string]string{"hist.worker": "off"}
	writePermanent("off")
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	if guard, present, _ := backup.ReadCopyGuard(dir); present && guard.Settings["hist.worker"] != "" {
		t.Fatalf("equivalent permanent suppression was not handed off: %+v", guard)
	} else if present && !slices.Contains(guard.Preloads, "hist_lib") {
		t.Fatalf("the required preload was lost on handoff: %+v", guard)
	}
	// A deliberate later change is not overridden by a reconstructed guard.
	spec.Settings["hist.worker"] = "on"
	writePermanent("on")
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	if guard, present, _ := backup.ReadCopyGuard(dir); present && guard.Settings["hist.worker"] != "" {
		t.Fatalf("a deliberate later change was overridden: %+v", guard)
	}
}

func TestValidateCopyGuardRefusesUnknownOrAmbiguousPreloads(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}
	unknown := backup.CopyGuard{Preloads: []string{"unknown_lib"}}
	err := ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, nil, unknown)
	if err == nil || !strings.Contains(err.Error(), "no extension definition explains") {
		t.Fatalf("unknown operator preload = %v", err)
	}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"known": {
			Source: catalog.SourcePreinstalled, Preload: "unknown_lib",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	touchExtensionLibrary(t, "unknown_lib")
	touchExtensionControl(t, "known")
	if err := ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, nil, unknown); err != nil {
		t.Fatalf("explicit compatible metadata refused: %v", err)
	}
	cfg.Extensions["known"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "unknown_lib",
		CopySafety: stringPtr(catalog.CopySafetyDisabled), StandbySafe: boolPtr(true),
		CopySettings: map[string]string{"known.worker": "off"},
	}
	err = ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, nil, unknown)
	if err == nil || !strings.Contains(err.Error(), "does not enforce") {
		t.Fatalf("missing mandatory copy setting = %v", err)
	}
	withSetting := backup.CopyGuard{Preloads: []string{"unknown_lib"}, Settings: map[string]string{"known.worker": "off"}}
	if err := ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, nil, withSetting); err != nil {
		t.Fatalf("explicit mandatory metadata refused: %v", err)
	}
	cfg.Extensions["known"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "unknown_lib",
		CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(false),
	}
	err = ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, true, nil, unknown)
	if err == nil || !strings.Contains(err.Error(), "not declared standby_safe") {
		t.Fatalf("standby-unsafe preload = %v", err)
	}
	// A settled, verified handoff releases the key: the validator must not
	// recreate the suppression the guard deliberately handed off.
	cfg.Extensions["known"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, Preload: "unknown_lib",
		CopySafety: stringPtr(catalog.CopySafetyDisabled), StandbySafe: boolPtr(true),
		CopySettings: map[string]string{"known.worker": "off"},
	}
	released := map[string]string{"known.worker": "off"}
	if err := ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, released, unknown); err != nil {
		t.Fatalf("a verified settled handoff was rejected: %v", err)
	}
	if err := ValidateCopyGuardPreloads(cfg, spec, "app", nil, nil, false, nil, unknown); err == nil {
		t.Fatal("a pending suppression without the guard setting was accepted")
	}
}

func TestPoolerRenderWithholdsActivationAliasUntilRouting(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	recs := backup.Records{Activation: map[string]backup.ActivationFence{
		"app": {OperationID: "op", Kind: "restore-promote", Phase: "promoted"},
	}}
	files := pgbouncerFilesFrom(cfg, engine.Runtime{}, recs)
	for _, f := range files {
		if f.path == poolerFilePaths.ini || f.path == poolerFilePaths.sessionIni {
			if strings.Contains(f.content, "app = host=") {
				t.Fatalf("%s served the alias before routing: %s", f.path, f.content)
			}
		}
	}
	recs.Activation["app"] = backup.ActivationFence{OperationID: "op", Kind: "restore-promote", Phase: "routing"}
	files = pgbouncerFilesFrom(cfg, engine.Runtime{}, recs)
	seen := 0
	for _, f := range files {
		if f.path == poolerFilePaths.ini || f.path == poolerFilePaths.sessionIni {
			if strings.Contains(f.content, "app = host=/var/run/postgresql port=5433") {
				seen++
			}
		}
	}
	if seen != 2 {
		t.Fatalf("routing eligibility did not publish both aliases: %d", seen)
	}
}

func TestPoolerRenderWithdrawsRestoreInspectionAlias(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	recs := backup.Records{
		Restores: map[string]backup.Restore{"app": {ID: "r", Database: "app", Phase: backup.PhaseReady, Port: 6001}},
		Activation: map[string]backup.ActivationFence{
			"app": {OperationID: "op", Kind: "restore-promote", Phase: "prepared"},
		},
	}
	files := pgbouncerFilesFrom(cfg, engine.Runtime{}, recs)
	for _, f := range files {
		if strings.Contains(f.content, "app__restoring") {
			t.Fatalf("%s kept the restore-inspection alias during promotion: %s", f.path, f.content)
		}
	}
}

func TestPoolerRoutingEligibilityIsProvenBeforeRender(t *testing.T) {
	oldRecords, oldInstance := backup.RecordsPath, poolerInstance
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	poolerInstance = func() (string, error) { return "test-instance", nil }
	backupRoot := t.TempDir()
	oldRoot := backup.PGConfRootForTest(backupRoot)
	t.Cleanup(func() {
		backup.RecordsPath, poolerInstance = oldRecords, oldInstance
		oldRoot()
	})
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Port: 5433, Stanza: "app-abc123"}
	cfg.Clusters = []config.ClusterSpec{spec}
	dataDir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(backupRoot, spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backupRoot, spec.Name, "postgresql.conf"),
		[]byte("data_directory = '"+dataDir+"'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Activation[spec.Name] = backup.ActivationFence{
			OperationID: "op", Kind: "fork", Database: spec.Name, Instance: "test-instance",
			TargetStanza: spec.Stanza, OldStanza: "source-abc123", NewStanza: spec.Stanza,
			Registration: spec.Name + ":5433:", Port: spec.Port, DataDir: dataDir,
			ConfDir: PGConfDir(spec.Name), Phase: "routing",
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pgbouncerFiles(cfg, engine.Runtime{}); err != nil {
		t.Fatalf("a valid routing checkpoint was refused: %v", err)
	}
	if err := backup.UpdateActivationFence(spec.Name, "op", func(f *backup.ActivationFence) error {
		f.Instance = "another-instance"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pgbouncerFiles(cfg, engine.Runtime{}); err == nil {
		t.Fatal("a stale instance routing checkpoint was rendered")
	}
	if err := backup.UpdateActivationFence(spec.Name, "op", func(f *backup.ActivationFence) error {
		f.Instance = "test-instance"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(backupRoot, spec.Name, "postgresql.conf")); err != nil {
		t.Fatal(err)
	}
	if _, err := pgbouncerFiles(cfg, engine.Runtime{}); err == nil {
		t.Fatal("a routing checkpoint without actual registration was rendered")
	}
	writes := 0
	oldWrite := writePoolerFile
	writePoolerFile = func(string, string, os.FileMode, string, string) error { writes++; return nil }
	t.Cleanup(func() { writePoolerFile = oldWrite })
	if err := ApplyPoolerFence(context.Background(), cfg, engine.Runtime{}, nil); err == nil {
		t.Fatal("ApplyPoolerFence accepted a stale routing checkpoint")
	}
	if writes != 0 {
		t.Fatalf("invalid routing eligibility wrote %d pooler files", writes)
	}
}

func TestEffectivePermanentOverrideRetainsGuard(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "hist_lib")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Stanza: "app-abc123"}
	if err := backup.RecordExtensionRequirements(spec.Stanza, spec.Name, []catalog.Extension{{
		Name: "hist", Source: catalog.SourcePreinstalled, Preload: "hist_lib",
		CopySafety: catalog.CopySafetyDisabled, StandbySafe: true,
		CopySettings: map[string]string{"hist.worker": "off"},
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	confDir := PGConfDir(spec.Name)
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"),
		[]byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("hist.worker = off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "90-overrides.conf"),
		[]byte("hist.worker = on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"hist_lib"}, Settings: map[string]string{"hist.worker": "off"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if guard.Settings["hist.worker"] != "off" {
		t.Fatalf("a higher-precedence operator value released the suppression: %+v", guard)
	}
}

func TestPlanFollowerRuntimeCompleteWinningGuard(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	for _, name := range []string{"pg_stat_statements", "pg_cron"} {
		touchExtensionControl(t, name)
		touchExtensionLibrary(t, name)
	}
	touchExtensionLibrary(t, "auto_explain")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-abc123", Extensions: []string{"pg_cron"}},
		{Name: "app_follower", Port: 5434, Follows: "app"},
	}
	follower := cfg.Clusters[1]
	confDir := PGConfDir(follower.Name)
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"),
		[]byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_cron'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	restoreSQL := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	defer restoreSQL()
	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"auto_explain", "pg_cron", "pg_stat_statements"}
	if !slices.Equal(plan.Preloads, want) {
		t.Fatalf("plan preloads = %v, want %v", plan.Preloads, want)
	}
	if plan.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("plan lost the mandatory copy setting: %+v", plan.Settings)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, follower); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if !slices.Equal(guard.Preloads, want) || guard.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("follower guard = %+v", guard)
	}
	// Guard-only drift is visible to Check's readonly calculation.
	drifted := guard.Clone()
	drifted.Settings["cron.launch_active_jobs"] = "on"
	if !FollowerGuardDrifted(drifted, true, plan) {
		t.Fatal("guard-only drift was not detected")
	}
	if _, err := backup.InstallCopyGuard(dataDir, drifted); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, follower); err != nil {
		t.Fatal(err)
	}
	guard, _, err = backup.ReadCopyGuard(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("the follower guard suppression was released: %+v", guard)
	}
}

func TestPlanFollowerRuntimeKeepsRetainedExtraAndCurrentSetting(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	for _, name := range []string{"pg_stat_statements", "pg_cron"} {
		touchExtensionControl(t, name)
		touchExtensionLibrary(t, name)
	}
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "retained_lib")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-abc123", Extensions: []string{"pg_cron"}},
		{Name: "app_follower", Port: 5434, Follows: "app"},
	}
	follower := cfg.Clusters[1]
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{
		{Name: "retained", Source: catalog.SourcePreinstalled, Preload: "retained_lib",
			CopySafety: catalog.CopySafetySafe, StandbySafe: true},
		{Name: "pg_cron", ExtName: "pg_cron", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
			Settings: map[string]string{"cron.database_name": "'old'"}, CopySafety: catalog.CopySafetyDisabled,
			CopySettings: map[string]string{"cron.launch_active_jobs": "off"}, StandbySafe: true},
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"retained_lib", "pg_cron"},
		Origins:  map[string]string{"retained_lib": "retained", "pg_cron": "retained"},
	}); err != nil {
		t.Fatal(err)
	}
	restoreSQL := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{}, nil
	})
	defer restoreSQL()
	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Preloads, "retained_lib") {
		t.Fatalf("actual retained extra was dropped: %v", plan.Preloads)
	}
	if plan.Settings["cron.database_name"] == "'old'" || plan.Settings["cron.database_name"] == "old" {
		t.Fatalf("an obsolete historical setting replaced current intent: %+v", plan.Settings)
	}
}

func TestFollowerUnsafeSQLOnlyMetadataRefuses(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"unsafe_sql": {Source: catalog.SourcePreinstalled, ExtName: "unsafe_sql"},
	}
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"unsafe_sql"}}
	exts, err := ResolveExtensionSelection(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtensionMetadataSafety(cfg, spec, exts, true); err == nil {
		t.Fatal("an SQL-only extension with unknown copy/standby safety was accepted")
	}
}

func TestEffectivePermanentSettingsReadsPrecedence(t *testing.T) {
	testCopyGuardWriter(t)
	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"),
		[]byte("include_dir = 'conf.d'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("hist.worker = 'off'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "90-overrides.conf"),
		[]byte("hist.worker = on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.auto.conf"),
		[]byte("hist.worker = off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := backup.EffectivePermanentSettings(confDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got["hist.worker"] != "off" {
		t.Fatalf("auto.conf did not win: %v", got)
	}
	// The generated guard block is not permanent operator configuration.
	if err := os.WriteFile(filepath.Join(dataDir, "postgresql.auto.conf"),
		[]byte("hist.worker = on\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{Settings: map[string]string{"hist.worker": "off"}}); err != nil {
		t.Fatal(err)
	}
	got, err = backup.EffectivePermanentSettings(confDir, dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if got["hist.worker"] != "on" {
		t.Fatalf("the generated guard was treated as permanent: %v", got)
	}
}

func TestFenceRestorePoolersCutsBothAliasesWithoutResume(t *testing.T) {
	oldNames, oldAdmin := poolerDatabaseNames, poolerAdminAt
	t.Cleanup(func() { poolerDatabaseNames, poolerAdminAt = oldNames, oldAdmin })
	poolerDatabaseNames = func(context.Context, string, int) (map[string]bool, error) {
		return map[string]bool{"app": true, "app__restoring": true, "other": true}, nil
	}
	var calls []string
	poolerAdminAt = func(_ context.Context, dir string, _ int, command string) error {
		calls = append(calls, dir+":"+command)
		return nil
	}
	if err := FenceRestorePoolers(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		PGSocketDir + ":KILL app",
		PGSocketDir + ":PAUSE app",
		PGSocketDir + ":KILL app__restoring",
		PGSocketDir + ":PAUSE app__restoring",
		SessionSocketDir + ":KILL app",
		SessionSocketDir + ":PAUSE app",
		SessionSocketDir + ":KILL app__restoring",
		SessionSocketDir + ":PAUSE app__restoring",
	}
	if !slices.Equal(calls, want) {
		t.Fatalf("commands = %v, want %v", calls, want)
	}
	for _, call := range calls {
		if strings.Contains(call, "RESUME") || strings.Contains(call, "other") {
			t.Fatalf("strict fence resumed or touched an unrelated alias: %v", calls)
		}
	}
	calls = nil
	for failAt := 0; failAt < len(want); failAt++ {
		calls = nil
		index := 0
		poolerAdminAt = func(_ context.Context, dir string, _ int, command string) error {
			calls = append(calls, dir+":"+command)
			index++
			if index-1 == failAt {
				return errors.New("command failed")
			}
			return nil
		}
		if err := FenceRestorePoolers(context.Background(), "app"); err == nil {
			t.Fatalf("failure at command %d was reported as success", failAt)
		}
		for _, call := range calls {
			if strings.Contains(call, "RESUME") {
				t.Fatalf("failure at command %d resumed an alias: %v", failAt, calls)
			}
		}
	}
}

func TestReconcileCopyGuardNeverRemovesFollower(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abc123"}}
	spec := config.ClusterSpec{Name: "app_follower", Port: 5434, Follows: "app"}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{Preloads: []string{"pg_stat_statements"}}); err != nil {
		t.Fatal(err)
	}
	err := ReconcileCopyGuard(context.Background(), cfg, spec)
	if err == nil || refusalReason(err) != "follower_runtime_snapshot_missing" {
		t.Fatalf("unreadable catalog without a preflight plan = %v", err)
	}
	guard, present, readErr := backup.ReadCopyGuard(dir)
	if readErr != nil || !present || !slices.Contains(guard.Preloads, "pg_stat_statements") {
		t.Fatalf("an unproven refresh changed the follower guard: %+v, %v, %v", guard, present, readErr)
	}
}

func TestRefuseExtensionUnloadStoppedFailsClosed(t *testing.T) {
	oldPath := managedExtensionsConfPath
	path := filepath.Join(t.TempDir(), "30-vaire-extensions.conf")
	managedExtensionsConfPath = func(string) string { return path }
	t.Cleanup(func() { managedExtensionsConfPath = oldPath })
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "stopped_ext_test"}

	// A new cluster has no old configuration to unload.
	if err := RefuseExtensionUnload(context.Background(), cfg, spec, false); err != nil {
		t.Fatalf("a new stopped cluster was blocked: %v", err)
	}
	// An unchanged stopped cluster is not blocked.
	if err := os.WriteFile(path, []byte("shared_preload_libraries = 'pg_stat_statements,auto_explain'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RefuseExtensionUnload(context.Background(), cfg, spec, false); err != nil {
		t.Fatalf("an unchanged stopped cluster was blocked: %v", err)
	}
	// Removing a last managed preload cannot be proven safe.
	if err := os.WriteFile(path, []byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,old_lib'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := RefuseExtensionUnload(context.Background(), cfg, spec, false)
	if err == nil || !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("a stopped preload removal = %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a typed refusal: %T", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != "extension_unload_unproven" {
		t.Fatalf("refusal details = %v", details)
	}
	// Unreadable old evidence is unknown, not safe.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RefuseExtensionUnload(context.Background(), cfg, spec, false); err == nil {
		t.Fatal("unreadable managed evidence was treated as safe")
	}
}

func TestStoppedUnsafeSavedIntentHasNoEffects(t *testing.T) {
	oldPath := managedExtensionsConfPath
	path := filepath.Join(t.TempDir(), "30-vaire-extensions.conf")
	managedExtensionsConfPath = func(string) string { return path }
	t.Cleanup(func() { managedExtensionsConfPath = oldPath })
	if err := os.WriteFile(path, []byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,old_lib'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "stoppedunsafe", Size: "small", Port: 5433}}
	oldWrite, oldScript := configWriteFileOwned, runExtensionPackageScript
	writes, installs := 0, 0
	configWriteFileOwned = func(string, string, os.FileMode, string, string) error {
		writes++
		return nil
	}
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		installs++
		return nil
	}
	t.Cleanup(func() { configWriteFileOwned, runExtensionPackageScript = oldWrite, oldScript })
	_, err := (postgresConfig{}).Instance("stoppedunsafe").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot prove") {
		t.Fatalf("already-saved unsafe intent = %v", err)
	}
	if writes != 0 || installs != 0 {
		t.Fatalf("unsafe stopped apply had effects: writes=%d installs=%d", writes, installs)
	}
	if _, statErr := os.Stat(PGConfDir("stoppedunsafe")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("a generated file was written before the refusal: %v", statErr)
	}
}

func TestReconcileCopyGuardDropsObsoleteManagedCurrent(t *testing.T) {
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	touchExtensionLibrary(t, "auto_explain")
	touchExtensionLibrary(t, "retained_lib")
	oldRoot, oldRecords := pgConfRoot, backup.RecordsPath
	pgConfRoot = t.TempDir()
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { pgConfRoot, backup.RecordsPath = oldRoot, oldRecords })
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	spec := config.ClusterSpec{Name: "app", Stanza: "app-abc123", Extensions: []string{"pg_stat_statements"}}
	if err := backup.RecordExtensionRequirements(spec.Stanza, spec.Name, []catalog.Extension{{
		Name: "retained", Source: catalog.SourcePreinstalled,
		Preload: "retained_lib", CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	confDir := PGConfDir(spec.Name)
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'old_current,pg_stat_statements'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := backup.InstallCopyGuard(dir, backup.CopyGuard{
		Preloads: []string{"old_current", "retained_lib"},
		Origins:  map[string]string{"old_current": "desired", "retained_lib": "retained"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, spec); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if !strings.Contains(strings.Join(guard.Preloads, ","), "retained_lib") {
		t.Fatalf("retained preload vanished: %v", guard.Preloads)
	}
	if strings.Contains(strings.Join(guard.Preloads, ","), "old_current") {
		t.Fatalf("obsolete managed current stayed: %v", guard.Preloads)
	}
	if !strings.Contains(strings.Join(guard.Preloads, ","), "pg_stat_statements") {
		t.Fatalf("current desired preload vanished: %v", guard.Preloads)
	}
}

func TestActivationFenceWithholdsPoolerAlias(t *testing.T) {
	recs := backup.Records{Activation: map[string]backup.ActivationFence{
		"app": {OperationID: "op", Phase: "promoted"},
	}}
	if !fencedPoolerAliases(recs)["app"] {
		t.Fatal("an active activation fence did not withhold the pooler alias")
	}
	recs.Activation["app"] = backup.ActivationFence{OperationID: "op", Phase: "routing"}
	if fencedPoolerAliases(recs)["app"] {
		t.Fatal("a routing-only fence kept the alias withheld")
	}
}
