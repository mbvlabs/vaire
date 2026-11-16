package node

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/steps"
)

func writeExtTestFence(t *testing.T, cfg config.Cluster, spec config.ClusterSpec, opID, phase string) {
	t.Helper()
	if _, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: spec.Name, Kind: "fork", OperationID: opID, Spec: spec,
		TargetStanza: spec.Name + "-ancestor", OldStanza: spec.Name + "-old", NewStanza: spec.Name + "-new",
		Aside: spec.Name + "__pre", Registration: activationRegistration(spec),
		Port: spec.Port, DataDir: cfg.Server.DataDirectory(spec.Name), ConfDir: steps.PGConfDir(spec.Name),
	}); err != nil {
		t.Fatal(err)
	}
	if phase == fencePhasePrepared {
		return
	}
	if err := backup.UpdateActivationFence(spec.Name, opID, func(f *backup.ActivationFence) error {
		f.Phase = phase
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestInstalledExtensionFactsPreserveUnknownNamespace(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	oldPG := extensionPG
	extensionPG = func(config.ClusterSpec) sqlRunner {
		return &fakeExtPG{queryFn: func(sql string) ([]map[string]string, error) {
			if !strings.Contains(sql, "LEFT JOIN pg_namespace") || !strings.Contains(sql, "coalesce(n.nspname, '')") {
				t.Fatalf("installed extension query lost its unknown-namespace fact: %s", sql)
			}
			return []map[string]string{
				{"name": "pg_partman", "version": "5.0", "default_version": "5.0", "schema": ""},
			}, nil
		}}
	}
	t.Cleanup(func() { extensionPG = oldPG })
	got, err := extensionInstalledDefault(context.Background(), cfg.Clusters[0])
	if err != nil {
		t.Fatal(err)
	}
	fact, ok := got["pg_partman"]
	if !ok || fact.Schema != "" {
		t.Fatalf("installed unknown namespace fact = %+v, ok=%v", fact, ok)
	}
}

func TestExtAddDirectSchemaChangeOnInstalledSQLRefusedBeforeEffects(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Default: "5.0", Schema: "partman"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_partman", "db": "app", "schema": "public",
	})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("installed schema change: %v", err)
	}
	details := extDetails(t, err)
	if details["ext_name"] != "pg_partman" || details["actual_schema"] != "partman" || details["requested_schema"] != "public" {
		t.Fatalf("schema details = %+v", details)
	}
	assertNoExtEffects(t, h)
	if len(h.saveAttempts) != 0 {
		t.Fatalf("schema refusal attempted %d saves", len(h.saveAttempts))
	}
}

func TestExtAddDirectSchemaChangeWithUnknownActualSchemaRefused(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Default: "5.0"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_partman", "db": "app", "schema": "public",
	})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_schema_unknown" {
		t.Fatalf("unknown installed schema: %v", err)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddAllowsInitialSchemaOverrideBeforeFirstInstall(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	h := newExtHarness(t, cfg)
	calls := 0
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		calls++
		if calls <= 2 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Default: "5.0", Schema: "custom_partman"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pg_partman", "db": "app", "schema": "custom_partman",
	})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.Data.(extAddResult)
	if !ok || data.Schema != "custom_partman" || !data.DescriptorSaved || !data.Created {
		t.Fatalf("fresh schema result = %+v", res.Data)
	}
	if len(h.saves) != 1 {
		t.Fatalf("saves = %d, want one descriptor save", len(h.saves))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	if saved.Extensions["pg_partman"].Schema != "custom_partman" {
		t.Fatalf("saved schema = %+v", saved.Extensions["pg_partman"])
	}
	if h.prepareCalls != 1 {
		t.Fatalf("prepare calls = %d", h.prepareCalls)
	}
}

func TestExtAddAliasUsesCanonicalInstalledSchema(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"vector": {Name: "vector", Version: "0.9.0", Default: "0.9.0", Schema: "public"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "pgvector", "db": "app", "schema": "public",
	})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if data := mustAs[extAddResult](t, res.Data); data.Extension != "vector" || data.ExtName != "vector" || data.Schema != "public" {
		t.Fatalf("alias result = %+v", data)
	}
}

func TestConfigRefusesInstalledCustomExtNameChangeBeforePersistence(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small", Extensions: []string{"custom"},
	}}
	base.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "old_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	updated := base.Clone()
	updated.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "new_ext", Schema: "custom",
		CopySafety: &safe, StandbySafe: &standby,
	}

	h := newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"old_ext": {Name: "old_ext", Version: "1.0", Schema: "custom"},
		}, nil
	}
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated)
	if err == nil || extReason(t, err) != "extension_remove_required" {
		t.Fatalf("installed ext_name change: %v", err)
	}
	details := extDetails(t, err)
	if details["ext_name"] != "old_ext" || details["requested_ext_name"] != "new_ext" {
		t.Fatalf("ext_name details = %+v", details)
	}
	if len(h.saves) != 0 {
		t.Fatalf("config refusal saved %d times", len(h.saves))
	}

	// An already selected definition cannot change its SQL identity even when
	// the old SQL is absent; the safety declarations still describe the old
	// control identity until an explicit remove/re-add.
	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	err = refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated)
	if err == nil || extReason(t, err) != "extension_remove_required" {
		t.Fatalf("absent old SQL extension selected identity change: %v", err)
	}
	if details := extDetails(t, err); details["ext_name"] != "old_ext" || details["requested_ext_name"] != "new_ext" {
		t.Fatalf("absent old identity details = %+v", details)
	}
}

func TestConfigRefusesNewlySelectedExtNameInstalledAtOtherSchema(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small", Extensions: []string{"pg_stat_statements"},
	}}
	base.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "old_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	updated := base.Clone()
	updated.Clusters[0].Extensions = append(updated.Clusters[0].Extensions, "custom")
	updated.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "new_ext", Schema: "custom",
		CopySafety: &safe, StandbySafe: &standby,
	}

	h := newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"new_ext": {Name: "new_ext", Version: "1.0", Schema: "physical_zone"},
		}, nil
	}
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("newly selected ext_name schema mismatch: %v", err)
	}
	if details := extDetails(t, err); details["actual_schema"] != "physical_zone" || details["requested_schema"] != "custom" {
		t.Fatalf("newly selected ext_name details = %+v", details)
	}

	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"new_ext": {Name: "new_ext", Version: "1.0", Schema: "custom"},
		}, nil
	}
	if err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated); err != nil {
		t.Fatalf("newly selected ext_name at the declared schema refused: %v", err)
	}

	// The old definition was unused, so changing its control identity and then
	// selecting the new one does not carry a selected safety assertion across.
	h = newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"old_ext": {Name: "old_ext", Version: "1.0", Schema: "custom"},
		}, nil
	}
	if err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated); err != nil {
		t.Fatalf("unused definition edit/selection refused: %v", err)
	}
}

func TestConfigRefusesInitialSelectionInstalledSchemaMismatch(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_partman": {Schema: "public"},
	}
	updated := cfg.Clone()
	updated.Clusters[0].Extensions = append(updated.Clusters[0].Extensions, "pg_partman")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Schema: "partman"},
		}, nil
	}
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), cfg, updated)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("initial selection schema mismatch = %v", err)
	}
	if len(h.saves) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("initial selection mismatch persisted or ran SQL: saves=%d exec=%v", len(h.saves), h.pg.execs)
	}

	h = newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0"},
		}, nil
	}
	err = refuseUnsafeExtensionConfigRemoval(context.Background(), cfg, updated)
	if err == nil || extReason(t, err) != "extension_schema_unknown" {
		t.Fatalf("initial selection unknown installed schema = %v", err)
	}
	if len(h.saves) != 0 {
		t.Fatalf("unknown initial selection schema saved %d times", len(h.saves))
	}
}

func TestConfigRefusesSelectedExtNameChangeWithoutInstalledSQL(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small", Extensions: []string{"custom"},
	}}
	base.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "old_ext",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	updated := base.Clone()
	updated.Extensions["custom"] = config.ExtensionDefinition{
		Source: catalog.SourcePreinstalled, ExtName: "new_ext",
		CopySafety: &safe, StandbySafe: &standby,
	}
	h := newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{}, nil
	}
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated)
	if err == nil || extReason(t, err) != "extension_remove_required" {
		t.Fatalf("selected ext_name change with absent SQL = %v", err)
	}
	if details := extDetails(t, err); details["ext_name"] != "old_ext" || details["requested_ext_name"] != "new_ext" {
		t.Fatalf("selected identity details = %+v", details)
	}
	if len(h.saves) != 0 {
		t.Fatalf("selected identity change saved %d times", len(h.saves))
	}
}

func TestConfigRefusesFollowerInstalledSchemaMismatch(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	cfg := extTestCfg("pg_stat_statements")
	cfg.Clusters = append(cfg.Clusters, extFollowerCluster("app_follower_1", "app", 5434))
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	cfg.Clusters[0].Extensions = append(cfg.Clusters[0].Extensions, "custom")
	cfg.Clusters[1].Extensions = append(cfg.Clusters[1].Extensions, "custom")
	updated := cfg.Clone()
	h := newExtHarness(t, cfg)
	oldInstalled := extensionInstalled
	extensionInstalled = func(_ context.Context, spec config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		schema := "custom"
		if spec.Name == "app_follower_1" {
			schema = "public"
		}
		return map[string]extensionInstalledVersion{
			"custom_ext": {Name: "custom_ext", Version: "1.0", Schema: schema},
		}, nil
	}
	t.Cleanup(func() { extensionInstalled = oldInstalled })
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), cfg, updated)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("follower schema mismatch = %v", err)
	}
	if details := extDetails(t, err); details["database"] != "app_follower_1" {
		t.Fatalf("follower schema refusal target = %+v", details)
	}
	if len(h.saves) != 0 {
		t.Fatalf("follower schema mismatch saved %d times", len(h.saves))
	}
}

func TestExtAddRefusesFollowerInstalledSchemaMismatchBeforeEffects(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	cfg := extTestCfg("pg_stat_statements")
	cfg.Clusters = append(cfg.Clusters, extFollowerCluster("app_follower_1", "app", 5434))
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	cfg.Clusters[0].Extensions = append(cfg.Clusters[0].Extensions, "custom")
	cfg.Clusters[1].Extensions = append(cfg.Clusters[1].Extensions, "custom")
	h := newExtHarness(t, cfg)
	oldInstalled := extensionInstalled
	extensionInstalled = func(_ context.Context, spec config.ClusterSpec) (map[string]extensionInstalledVersion, error) {
		schema := "custom"
		if spec.Name == "app_follower_1" {
			schema = "public"
		}
		return map[string]extensionInstalledVersion{
			"custom_ext": {Name: "custom_ext", Version: "1.0", Schema: schema},
		}, nil
	}
	t.Cleanup(func() { extensionInstalled = oldInstalled })
	restoreSchemas := steps.SetFollowerInstalledExtensionSchemasForTest(func(_ context.Context, spec config.ClusterSpec) (map[string]string, error) {
		schema := "custom"
		if spec.Name == "app_follower_1" {
			schema = "public"
		}
		return map[string]string{"custom_ext": schema}, nil
	})
	t.Cleanup(restoreSchemas)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "custom", "db": "app"}, "app/restart", "app_follower_1/restart")}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("follower schema mismatch on direct add = %v", err)
	}
	if details := extDetails(t, err); details["database"] != "app_follower_1" {
		t.Fatalf("direct follower schema target = %+v", details)
	}
	assertNoExtEffects(t, h)
}

func TestExtAddConfiguredSchemaMismatchWithoutSchemaFlagRefused(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"pg_partman": {Schema: "public"},
	}
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Schema: "partman"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_partman", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_schema_mismatch" {
		t.Fatalf("configured schema mismatch without --schema = %v", err)
	}
	assertNoExtEffects(t, h)
}

func TestInstalledExtensionPreflightDoesNotForceOptionalSchema(t *testing.T) {
	cfg := extTestCfg("pg_cron")
	h := newExtHarness(t, cfg)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_cron": {Name: "pg_cron", Version: "1.6", Schema: "pg_catalog"},
		}, nil
	}
	if err := refuseInstalledExtensionSchemas(context.Background(), cfg, cfg.Clusters[0]); err != nil {
		t.Fatalf("pg_cron's optional descriptor schema was forced: %v", err)
	}
}

func TestConfigRefusesInstalledBuiltinSchemaChange(t *testing.T) {
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Stanza: "app-abc123", Size: "small", Extensions: []string{"pg_partman"},
	}}
	updated := base.Clone()
	updated.Extensions = map[string]config.ExtensionDefinition{"pg_partman": {Schema: "public"}}
	h := newExtHarness(t, base)
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		return map[string]extensionInstalledVersion{
			"pg_partman": {Name: "pg_partman", Version: "5.0", Schema: "partman"},
		}, nil
	}
	err := refuseUnsafeExtensionConfigRemoval(context.Background(), base, updated)
	if err == nil || extReason(t, err) != "extension_remove_required" {
		t.Fatalf("installed builtin schema change: %v", err)
	}
	if len(h.saves) != 0 {
		t.Fatalf("config refusal saved %d times", len(h.saves))
	}
}

func TestExtAddInstalledCustomDescriptorReplacementRefused(t *testing.T) {
	safe, standby := catalog.CopySafetySafe, true
	cfg := extTestCfg("pg_stat_statements", "custom")
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "old_ext", Schema: "custom",
			CopySafety: &safe, StandbySafe: &standby,
		},
	}
	h := newExtHarness(t, cfg)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{
		"name": "custom", "db": "app", "schema": "new_schema",
	})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "extension_definition_exists" {
		t.Fatalf("custom descriptor replacement: %v", err)
	}
	assertNoExtEffects(t, h)
}

func TestExtMutationsRefuseActiveActivationFencesBeforeEffects(t *testing.T) {
	for _, phase := range []string{fencePhasePrepared, fencePhaseAdopted, fencePhaseRouting} {
		t.Run("add/"+phase, func(t *testing.T) {
			cfg := extTestCfg("pg_stat_statements")
			h := newExtHarness(t, cfg)
			useTestInstance(t)
			writeExtTestFence(t, cfg, cfg.Clusters[0], "add-"+phase, phase)
			env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
			_, err := handleExtAdd(context.Background(), env)
			if err == nil || extReason(t, err) != "activation_fence_active" {
				t.Fatalf("fenced add: %v", err)
			}
			details := extDetails(t, err)
			if details["database"] != "app" || details["phase"] != phase || details["kind"] != "fork" {
				t.Fatalf("fence details = %+v", details)
			}
			assertNoExtEffects(t, h)
			if h.installedCalls != 0 {
				t.Fatalf("fenced add read installed facts %d times", h.installedCalls)
			}
		})
		t.Run("remove/"+phase, func(t *testing.T) {
			cfg := extTestCfg("pg_stat_statements", "pg_trgm")
			h := newExtHarness(t, cfg)
			useTestInstance(t)
			writeExtTestFence(t, cfg, cfg.Clusters[0], "remove-"+phase, phase)
			env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
			_, err := handleExtRemove(context.Background(), env)
			if err == nil || extReason(t, err) != "activation_fence_active" {
				t.Fatalf("fenced remove: %v", err)
			}
			assertNoExtEffects(t, h)
		})
		t.Run("dry-run/"+phase, func(t *testing.T) {
			cfg := extTestCfg("pg_stat_statements")
			h := newExtHarness(t, cfg)
			useTestInstance(t)
			writeExtTestFence(t, cfg, cfg.Clusters[0], "dry-"+phase, phase)
			env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
			env.Req.DryRun = true
			_, err := handleExtAdd(context.Background(), env)
			if err == nil || extReason(t, err) != "activation_fence_active" {
				t.Fatalf("fenced dry run: %v", err)
			}
			assertNoExtEffects(t, h)
		})
	}
}

func TestExtAddRefusesFollowerActivationFenceBeforeSourceEffects(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	follower := extFollowerCluster("app_follower_1", "app", 5434)
	cfg.Clusters = append(cfg.Clusters, follower)
	h := newExtHarness(t, cfg)
	useTestInstance(t)
	writeExtTestFence(t, cfg, follower, "follower-fork", fencePhaseAdopted)
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "app"})}
	_, err := handleExtAdd(context.Background(), env)
	if err == nil || extReason(t, err) != "activation_fence_active" {
		t.Fatalf("fenced follower target: %v", err)
	}
	if details := extDetails(t, err); details["database"] != follower.Name {
		t.Fatalf("fence target = %+v", details)
	}
	assertNoExtEffects(t, h)
}

func TestExtMutationGuardOrderPrecedesActivationFence(t *testing.T) {
	cases := []struct {
		name   string
		recs   backup.Records
		reason string
	}{
		{"awaiting", backup.Records{AwaitingRecovery: map[string]backup.Awaiting{"app": {}}}, "awaiting_recovery"},
		{"restore", backup.Records{Restores: map[string]backup.Restore{"app": {ID: "r1", Phase: "ready"}}}, "restore_in_progress"},
		{"unfollow", backup.Records{Unfollowing: map[string]backup.Unfollow{"app": {Source: "other"}}}, "unfollow_in_progress"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := extTestCfg("pg_stat_statements")
			newExtHarness(t, cfg)
			useTestInstance(t)
			writeExtTestFence(t, cfg, cfg.Clusters[0], "guard-"+tc.name, fencePhaseAdopted)
			err := extensionMutationRefusals(cfg, tc.recs, cfg.Clusters[0], "an extension change")
			if err == nil || extReason(t, err) != tc.reason {
				t.Fatalf("guard precedence = %v, want %s", err, tc.reason)
			}
		})
	}
}

func TestExtAddDoesNotBlockUnrelatedSafeDatabaseForFence(t *testing.T) {
	cfg := extTestCfg("pg_stat_statements")
	cfg.Clusters = append(cfg.Clusters, extAddCluster("safe", "pg_stat_statements"))
	h := newExtHarness(t, cfg)
	useTestInstance(t)
	writeExtTestFence(t, cfg, cfg.Clusters[0], "other-db-fork", fencePhaseAdopted)
	calls := 0
	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		calls++
		if calls == 1 {
			return map[string]extensionInstalledVersion{}, nil
		}
		return map[string]extensionInstalledVersion{
			"pg_trgm": {Name: "pg_trgm", Version: "1.6", Default: "1.6", Schema: "public"},
		}, nil
	}
	env := &Env{Cluster: cfg, Req: extReq(t, map[string]any{"name": "pg_trgm", "db": "safe"})}
	res, err := handleExtAdd(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if data := mustAs[extAddResult](t, res.Data); data.Database != "safe" || !data.Created {
		t.Fatalf("safe add result = %+v", data)
	}
	if len(h.saves) != 1 {
		t.Fatalf("safe add saves = %d", len(h.saves))
	}
	saved, err := config.ParseCluster(h.saves[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range saved.Clusters {
		if spec.Name == "safe" && !slices.Contains(spec.Extensions, "pg_trgm") {
			t.Fatalf("safe selection = %v", spec.Extensions)
		}
	}
}
