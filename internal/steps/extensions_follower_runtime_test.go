package steps

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
)

func followerRuntimeTestConfig(t *testing.T) (config.Cluster, config.ClusterSpec) {
	t.Helper()
	testCopyGuardWriter(t)
	extensionTestDirs(t)
	tempGuardRecords(t)
	oldRoot := pgConfRoot
	pgConfRoot = t.TempDir()
	t.Cleanup(func() { pgConfRoot = oldRoot })
	for _, name := range []string{"pg_stat_statements", "pg_cron", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "pg_cron", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-abc123"},
		{Name: "app_follower", Port: 5434, Follows: "app"},
	}
	follower := cfg.Clusters[1]
	if err := os.MkdirAll(cfg.Server.DataDirectory(follower.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	return cfg, follower
}

func TestPlanFollowerRuntimeFailsClosedOnCatalogError(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
	}); err != nil {
		t.Fatal(err)
	}
	var catalogErr error
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		if catalogErr != nil {
			return nil, catalogErr
		}
		return map[string]bool{"plpgsql": true}, nil
	})
	defer restore()

	snapshot, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	catalogErr = errors.New("catalog is down")

	_, err = PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err == nil || refusalReason(err) != "follower_sql_unknown" {
		t.Fatalf("unreadable catalog = %v, want follower_sql_unknown", err)
	}
	_, err = PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, AllowUnknownSQL: true,
	})
	if err == nil || refusalReason(err) != "follower_runtime_snapshot_missing" {
		t.Fatalf("unreadable catalog without snapshot = %v, want follower_runtime_snapshot_missing", err)
	}

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, AllowUnknownSQL: true, Snapshot: &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SQLFactsComplete {
		t.Fatal("an unreadable catalog was reported as complete")
	}
	if !slices.Contains(plan.Preloads, "pg_cron") {
		t.Fatalf("an unknown catalog dropped the existing guard library: %v", plan.Preloads)
	}
	if plan.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("an unknown catalog dropped the existing suppression: %+v", plan.Settings)
	}
}

func TestPlanFollowerRuntimeSnapshotPreservesSQLOnlyMandatorySuppression(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	custom := catalog.Extension{
		Name: "custom", ExtName: "custom_ext", Source: catalog.SourcePreinstalled,
		Settings:     map[string]string{"custom.mode": "on"},
		CopySafety:   catalog.CopySafetyDisabled,
		CopySettings: map[string]string{"custom.worker": "off"},
		StandbySafe:  true,
	}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings:     map[string]string{"custom.mode": "on"},
			CopySafety:   stringPtr(catalog.CopySafetyDisabled),
			CopySettings: map[string]string{"custom.worker": "off"},
			StandbySafe:  boolPtr(true),
		},
	}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{custom}, time.Now()); err != nil {
		t.Fatal(err)
	}
	var catalogErr error
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		if catalogErr != nil {
			return nil, catalogErr
		}
		return map[string]bool{"plpgsql": true, "custom_ext": true}, nil
	})
	defer restore()

	snapshot, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Guard.Settings["custom.worker"] != "off" {
		t.Fatalf("preflight snapshot lost the unguarded mandatory suppression: %+v", snapshot.Guard.Settings)
	}
	catalogErr = errors.New("catalog went away before the guard refresh")
	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, AllowUnknownSQL: true, Snapshot: &snapshot,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.SQLFactsComplete {
		t.Fatal("the snapshot fallback claimed complete SQL facts")
	}
	if !slices.ContainsFunc(plan.Requirements, func(ext catalog.Extension) bool { return ext.Name == "custom" }) {
		t.Fatalf("the snapshot fallback lost the SQL-only retained descriptor: %+v", plan.Requirements)
	}
	if plan.ExpectedSettings["custom.worker"] != "off" || plan.Guard.Settings["custom.worker"] != "off" {
		t.Fatalf("the snapshot fallback lost the mandatory suppression: expected=%+v guard=%+v", plan.ExpectedSettings, plan.Guard.Settings)
	}

	id, err := FollowerRuntimeIdentity(cfg, follower, "op-snapshot")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithFollowerRuntimeOperation(context.Background(), "op-snapshot")
	ctx = WithFollowerRuntimePlans(ctx, map[FollowerRuntimePlanIdentity]FollowerRuntimePlan{id: snapshot})
	if err := ReconcileCopyGuard(ctx, cfg, follower); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(cfg.Server.DataDirectory(follower.Name))
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if guard.Settings["custom.worker"] != "off" {
		t.Fatalf("the post-effect refresh wrote an incomplete guard: %+v", guard)
	}
}

func TestFollowerRuntimeSnapshotWrongOperationRefusesGuardWrite(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	var catalogErr error
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		if catalogErr != nil {
			return nil, catalogErr
		}
		return map[string]bool{"plpgsql": true, "pg_cron": true}, nil
	})
	defer restore()
	snapshot, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{Class: FollowerRuntimeLive})
	if err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	before := backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
		Origins:  map[string]string{"pg_cron": "retained"},
	}
	if _, err := backup.InstallCopyGuard(dataDir, before); err != nil {
		t.Fatal(err)
	}
	catalogErr = errors.New("catalog is down")
	otherID, err := FollowerRuntimeIdentity(cfg, follower, "other-operation")
	if err != nil {
		t.Fatal(err)
	}
	ctx := WithFollowerRuntimeOperation(context.Background(), "current-operation")
	ctx = WithFollowerRuntimePlans(ctx, map[FollowerRuntimePlanIdentity]FollowerRuntimePlan{otherID: snapshot})
	err = ReconcileCopyGuard(ctx, cfg, follower)
	if err == nil || refusalReason(err) != "follower_runtime_snapshot_missing" {
		t.Fatalf("wrong-operation snapshot = %v", err)
	}
	after, present, readErr := backup.ReadCopyGuard(dataDir)
	if readErr != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", after, present, readErr)
	}
	if !slices.Equal(after.Preloads, before.Preloads) || after.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("an identity-mismatched snapshot changed the guard: %+v", after)
	}
}

func TestPlanFollowerRuntimeCollectsGuardOnlyRetainedMetadata(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	custom := catalog.Extension{
		Name: "custom", ExtName: "custom_ext", Source: catalog.SourcePreinstalled,
		Preload: "custom_lib",
		Settings: map[string]string{
			"custom.mode": "on",
		},
		CopySafety:   catalog.CopySafetyDisabled,
		CopySettings: map[string]string{"custom.worker": "off"},
		StandbySafe:  true,
	}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Preload: "custom_lib",
			Settings:     map[string]string{"custom.mode": "on"},
			CopySafety:   stringPtr(catalog.CopySafetyDisabled),
			CopySettings: map[string]string{"custom.worker": "off"},
			StandbySafe:  boolPtr(true),
		},
	}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{custom}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"custom_lib"},
		Settings: map[string]string{"custom.mode": "on", "custom.worker": "off"},
		Origins:  map[string]string{"custom_lib": "retained"},
	}); err != nil {
		t.Fatal(err)
	}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true, "custom_ext": true}, nil
	})
	defer restore()

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(plan.Requirements, func(ext catalog.Extension) bool { return ext.Name == "custom" }) {
		t.Fatalf("guard-only retained metadata was omitted: %+v", plan.Requirements)
	}
	if !slices.Contains(plan.Preloads, "custom_lib") {
		t.Fatalf("guard-only retained library was omitted: %v", plan.Preloads)
	}
	if plan.ExpectedSettings["custom.mode"] != "on" || plan.ExpectedSettings["custom.worker"] != "off" {
		t.Fatalf("complete expected settings lost retained metadata: %+v", plan.ExpectedSettings)
	}
	if plan.Guard.Settings["custom.worker"] != "off" {
		t.Fatalf("guard lost the retained mandatory suppression: %+v", plan.Guard.Settings)
	}
	if err := ReconcileCopyGuard(context.Background(), cfg, follower); err != nil {
		t.Fatal(err)
	}
	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
	if !slices.Contains(guard.Preloads, "custom_lib") || guard.Settings["custom.worker"] != "off" {
		t.Fatalf("refreshed guard lost retained metadata: %+v", guard)
	}
}

func TestPlanFollowerRuntimeCurrentDescriptorWinsHistory(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Clusters[0].Extensions = []string{"pg_cron"}
	oldExt := catalog.Extension{
		Name: "pg_cron", ExtName: "pg_cron", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		Settings: map[string]string{"cron.database_name": "'old'"}, CopySafety: catalog.CopySafetyDisabled,
		CopySettings: map[string]string{"cron.launch_active_jobs": "off"}, StandbySafe: true,
	}
	newExt := oldExt.Clone()
	newExt.Settings = map[string]string{"cron.database_name": "'new'"}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{oldExt, newExt}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{Preloads: []string{"pg_cron"}}); err != nil {
		t.Fatal(err)
	}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true, "pg_cron": true}, nil
	})
	defer restore()

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, RetireAbsent: true,
	})
	if err != nil {
		t.Fatalf("current descriptor did not win its historical variants: %v", err)
	}
	if got := plan.ExpectedSettings["cron.database_name"]; got != "app" {
		t.Fatalf("current ordinary setting = %q, want decoded current intent app", got)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if got := len(recs.RetainedExtensions("app-abc123")); got != 2 {
		t.Fatalf("history was deleted: %d entries", got)
	}
}

func TestPlanFollowerRuntimeExpectedOwnMandatoryOverlay(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"addon": {
			Source: catalog.SourcePreinstalled, ExtName: "addon_ext",
			Settings:     map[string]string{"addon.mode": "on", "addon.worker": "on"},
			CopySafety:   stringPtr(catalog.CopySafetyDisabled),
			CopySettings: map[string]string{"addon.worker": "off"},
			StandbySafe:  boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"addon"}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true, "addon_ext": true}, nil
	})
	defer restore()

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plan.ExpectedSettings["addon.mode"] != "on" {
		t.Fatalf("current ordinary setting was omitted: %+v", plan.ExpectedSettings)
	}
	if plan.ExpectedSettings["addon.worker"] != "off" {
		t.Fatalf("own mandatory suppression did not overlay its ordinary value: %+v", plan.ExpectedSettings)
	}
	if plan.Guard.Settings["addon.worker"] != "off" {
		t.Fatalf("guard lost the mandatory suppression: %+v", plan.Guard.Settings)
	}
}

func TestPlanFollowerRuntimeRetiresOutgoingOnlyAfterSQLAbsent(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	outgoing := catalog.Extension{
		Name: "pg_cron", ExtName: "pg_cron", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		Settings: map[string]string{"cron.database_name": "'app'"}, CopySafety: catalog.CopySafetyDisabled,
		CopySettings: map[string]string{"cron.launch_active_jobs": "off"}, StandbySafe: true,
	}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{outgoing}, time.Now()); err != nil {
		t.Fatal(err)
	}
	dataDir := cfg.Server.DataDirectory(follower.Name)
	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
		Origins:  map[string]string{"pg_cron": "retained"},
	}); err != nil {
		t.Fatal(err)
	}

	installed := map[string]bool{"plpgsql": true, "pg_cron": true}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return installed, nil
	})
	defer restore()

	preflight, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, Outgoing: []catalog.Extension{outgoing},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(preflight.Requirements, func(ext catalog.Extension) bool { return ext.Name == "pg_cron" }) {
		t.Fatalf("preflight lost outgoing evidence: %+v", preflight.Requirements)
	}
	if preflight.Settings["cron.launch_active_jobs"] != "off" {
		t.Fatalf("preflight did not keep the known old suppression: %+v", preflight.Settings)
	}

	present, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, Outgoing: []catalog.Extension{outgoing}, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(present.PendingSQL, []string{"pg_cron"}) {
		t.Fatalf("lagging follower SQL = %v, want pending pg_cron", present.PendingSQL)
	}
	if present.Settings["cron.launch_active_jobs"] != "off" || !slices.Contains(present.Preloads, "pg_cron") {
		t.Fatalf("lagging follower lost its old guard: %+v", present)
	}

	installed = map[string]bool{"plpgsql": true}
	absent, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, Outgoing: []catalog.Extension{outgoing}, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(absent.PendingSQL) != 0 {
		t.Fatalf("absent SQL still pending: %v", absent.PendingSQL)
	}
	if slices.Contains(absent.Preloads, "pg_cron") || absent.Settings["cron.launch_active_jobs"] != "" {
		t.Fatalf("absent SQL did not retire its guard obligations: %+v", absent)
	}
}

func TestPlanFollowerRuntimeRetirementKeepsSharedLibrary(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"shared": {
			Source: catalog.SourcePreinstalled, ExtName: "shared_ext", Preload: "pg_cron",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	cfg.Clusters[0].Extensions = []string{"shared"}
	outgoing := catalog.Extension{
		Name: "pg_cron", ExtName: "pg_cron", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		CopySafety: catalog.CopySafetyDisabled, CopySettings: map[string]string{"cron.launch_active_jobs": "off"},
		StandbySafe: true,
	}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{outgoing}, time.Now()); err != nil {
		t.Fatal(err)
	}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true}, nil
	})
	defer restore()

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, Outgoing: []catalog.Extension{outgoing}, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Preloads, "pg_cron") {
		t.Fatalf("a library shared by current intent was retired: %v", plan.Preloads)
	}
	if plan.Settings["cron.launch_active_jobs"] != "" {
		t.Fatalf("the removed extension's suppression was kept: %+v", plan.Settings)
	}
}

func TestPlanFollowerRuntimeRetirementKeepsRetainedSharedLibrary(t *testing.T) {
	cfg, follower := followerRuntimeTestConfig(t)
	outgoing := catalog.Extension{
		Name: "pg_cron", ExtName: "pg_cron", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		CopySafety: catalog.CopySafetyDisabled, CopySettings: map[string]string{"cron.launch_active_jobs": "off"},
		StandbySafe: true,
	}
	shared := catalog.Extension{
		Name: "shared", ExtName: "shared_ext", Source: catalog.SourcePreinstalled, Preload: "pg_cron",
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}
	if err := backup.RecordExtensionRequirements("app-abc123", "app", []catalog.Extension{outgoing, shared}, time.Now()); err != nil {
		t.Fatal(err)
	}
	restore := SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		return map[string]bool{"plpgsql": true, "shared_ext": true}, nil
	})
	defer restore()

	plan, err := PlanFollowerRuntime(context.Background(), cfg, follower, FollowerRuntimeRequest{
		Class: FollowerRuntimeLive, Outgoing: []catalog.Extension{outgoing}, RetireAbsent: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(plan.Preloads, "pg_cron") {
		t.Fatalf("a library shared by a retained extra was retired: %v", plan.Preloads)
	}
	if plan.Settings["cron.launch_active_jobs"] != "" {
		t.Fatalf("the removed extension's suppression was kept: %+v", plan.Settings)
	}
}

func TestCheckPrimaryExtensionReadinessDecodesActualOnce(t *testing.T) {
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}
	ext := catalog.Extension{
		Name: "custom", ExtName: "custom_ext", Source: catalog.SourcePreinstalled,
		Settings: map[string]string{"custom.worker": "'''off'''"},
	}
	readiness, err := CheckPrimaryExtensionReadiness(cfg, spec, ext, nil, nil, map[string]ExtensionSettingFact{"custom.worker": {Value: "off"}})
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.Deferred {
		t.Fatal("unquoted actual off matched a quote-content literal")
	}
	readiness, err = CheckPrimaryExtensionReadiness(cfg, spec, ext, nil, nil, map[string]ExtensionSettingFact{"custom.worker": {Value: "'off'"}})
	if err != nil {
		t.Fatal(err)
	}
	if readiness.Deferred {
		t.Fatalf("quote-content actual did not match: %+v", readiness)
	}
}

func TestValidateExtensionDescriptorSafetySeparatesCopyGate(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
		"copy_only": {
			Source: catalog.SourcePreinstalled, ExtName: "copy_only_ext",
			Settings:     map[string]string{"shared.mode": "on"},
			CopySafety:   stringPtr(catalog.CopySafetyDisabled),
			CopySettings: map[string]string{"shared.worker": "off"},
			StandbySafe:  boolPtr(true),
		},
		"ordinary": {
			Source: catalog.SourcePreinstalled, ExtName: "ordinary_ext",
			Settings: map[string]string{"shared.worker": "on"},
		},
	}
	spec := config.ClusterSpec{Name: "app", Extensions: []string{"custom", "copy_only", "ordinary"}}
	exts, err := ResolveExtensionSelection(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtensionDescriptorSafety(cfg, spec, exts); err != nil {
		t.Fatalf("ordinary validation rejected a copy-only setting or unknown copy safety: %v", err)
	}
	if err := ValidateExtensionMetadataSafety(cfg, spec, exts, false); err == nil {
		t.Fatal("the copy gate accepted an unknown copy safety or copy-only conflict")
	}
}

func TestExtensionCreationSQLDefersOnRequiredSettingPending(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings: map[string]string{"custom.worker": "on"},
		},
		"other": {
			Source: catalog.SourcePreinstalled, ExtName: "other_ext",
			Settings: map[string]string{"other.worker": "on"},
		},
	}
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Extensions: []string{"custom", "other"}}}
	spec := cfg.Clusters[0]
	loaded := map[string]bool{"pg_stat_statements": true, "auto_explain": true}

	sql, deferred, err := extensionCreationSQL(cfg, spec, loaded, map[string]bool{"custom.worker": true})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(deferred, []string{"custom"}) || strings.Contains(sql, `"custom_ext"`) || !strings.Contains(sql, `"other_ext"`) {
		t.Fatalf("required setting pending SQL = %q, deferred %v, %v", sql, deferred, err)
	}

	sql, deferred, err = extensionCreationSQL(cfg, spec, loaded, map[string]bool{"unrelated.setting": true})
	if err != nil {
		t.Fatal(err)
	}
	if len(deferred) != 0 || !strings.Contains(sql, `"custom_ext"`) || !strings.Contains(sql, `"other_ext"`) {
		t.Fatalf("unrelated pending setting deferred SQL = %q, %v, %v", sql, deferred, err)
	}

	// A reload-only effective prerequisite does not appear in pending_restart
	// and must not demand an unnecessary restart.
	sql, deferred, err = extensionCreationSQL(cfg, spec, loaded, nil)
	if err != nil || len(deferred) != 0 || !strings.Contains(sql, `"custom_ext"`) {
		t.Fatalf("ready reload-only SQL = %q, %v, %v", sql, deferred, err)
	}
}

func TestFollowerGuardDriftDetectsStaleRetiredEntries(t *testing.T) {
	plan := FollowerRuntimePlan{Guard: backup.CopyGuard{}}
	installed := backup.CopyGuard{
		Preloads: []string{"pg_cron"},
		Settings: map[string]string{"cron.launch_active_jobs": "off"},
	}
	if !FollowerGuardDrifted(installed, true, plan) {
		t.Fatal("a stale guard with retired obligations was reported complete")
	}
}
