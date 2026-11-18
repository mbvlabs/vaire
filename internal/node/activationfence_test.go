package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

func useTestInstance(t *testing.T) {
	t.Helper()

	old := followHostInstance
	followHostInstance = func() (string, error) { return "test-instance", nil }

	t.Cleanup(func() { followHostInstance = old })
}

func writeTestFence(t *testing.T, cfg config.Cluster, kind, opID string) backup.ActivationFence {
	t.Helper()

	spec := cfg.Clusters[0]

	f, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: spec.Name, Kind: kind, OperationID: opID, Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Aside: "app__pre", Registration: activationRegistration(spec),
		Port: spec.Port, DataDir: cfg.Server.DataDirectory(spec.Name),
		ConfDir: steps.PGConfDir(spec.Name),
	})
	if err != nil {
		t.Fatal(err)
	}

	return f
}

// installActivationExtensionTree creates the test-owned extension control and
// library files a real config or roles step inspects.
func installActivationExtensionTree(t *testing.T, controlFiles, libFiles []string) {
	t.Helper()
	extRoot := t.TempDir()

	controlDir, libDir := filepath.Join(extRoot, "extension"), filepath.Join(extRoot, "lib")
	for _, dir := range []string{controlDir, libDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	t.Cleanup(steps.ExtensionDirsForTest(controlDir, libDir))

	for _, name := range controlFiles {
		if err := os.WriteFile(filepath.Join(controlDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, name := range libFiles {
		if err := os.WriteFile(filepath.Join(libDir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// useEncodedFollowerStore persists cfg behind the follower state seam so the
// real config and roles steps read the same cluster the fence describes.
func useEncodedFollowerStore(t *testing.T, cfg config.Cluster) {
	t.Helper()
	store := state.FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Save(raw); err != nil {
		t.Fatal(err)
	}

	oldState := followerState
	followerState = func() state.Store { return store }

	t.Cleanup(func() { followerState = oldState })
}

// useFollowRegistrationHooks reports every name as registered at dataDir.
func useFollowRegistrationHooks(t *testing.T, dataDir string) {
	t.Helper()

	oldConf, oldData := followConfRegistered, followRegisteredData
	followConfRegistered = func(string) bool { return true }
	followRegisteredData = func(string) (string, error) { return dataDir, nil }

	t.Cleanup(func() { followConfRegistered, followRegisteredData = oldConf, oldData })
}

// writeActivationConfFixture writes the PostgreSQL configuration a real config
// step reads, rewrites and verifies.
func writeActivationConfFixture(t *testing.T, database string) {
	t.Helper()

	confDir := steps.PGConfDir(database)
	for _, dir := range []string{confDir, filepath.Join(confDir, "conf.d")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	files := map[string]string{
		"postgresql.conf":                 "include_dir = 'conf.d'\n",
		"conf.d/90-overrides.conf":        "",
		"conf.d/10-vaire-tuning.conf":     "# existing\n",
		"conf.d/30-vaire-extensions.conf": "shared_preload_libraries = 'pg_stat_statements,auto_explain'\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(confDir, filepath.FromSlash(name)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// stubActivationPublication models route publication and resumes the database
// once those routes exist.
func stubActivationPublication(t *testing.T, spec config.ClusterSpec, published *bool) {
	t.Helper()

	oldPublish, oldRoutes, oldResume := publishRouting, poolerRuntimeRoutes, poolerResumeDatabase
	publishRouting = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		*published = true
		return nil
	}
	poolerResumeDatabase = func(context.Context, string) {}
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		if !*published {
			return map[string]steps.PoolerRoute{}, nil
		}

		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}

	t.Cleanup(func() { publishRouting, poolerRuntimeRoutes, poolerResumeDatabase = oldPublish, oldRoutes, oldResume })
}

// adoptForkFence records a fork fence, advances it to adopted and derives the
// copy plan from the same durable identity.
func adoptForkFence(t *testing.T, cfg config.Cluster, spec config.ClusterSpec, identity activationFenceIdentity) (backup.ActivationFence, *extensionCopyPlan) {
	t.Helper()

	f, err := writeActivationFence(cfg, identity)
	if err != nil {
		t.Fatal(err)
	}

	if err := backup.UpdateActivationFence(spec.Name, f.OperationID, func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseAdopted
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	plan, err := copyPlanFromFence(cfg, spec, f)
	if err != nil {
		t.Fatal(err)
	}

	return f, plan
}

// assertActivationFencePhase checks the durable fence for database.
func assertActivationFencePhase(t *testing.T, database, phase string) {
	t.Helper()

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if got, ok := recs.ActivationFenceFor(database); !ok || got.Phase != phase {
		t.Fatalf("activation fence = %+v, present=%v", got, ok)
	}
}

// assertCompleteForkActivationFails proves a continuation failure was reported
// and left the adopted checkpoint intact.
func assertCompleteForkActivationFails(t *testing.T, spec config.ClusterSpec, f backup.ActivationFence, plan *extensionCopyPlan, rt engine.Runtime, what string) {
	t.Helper()

	if _, _, err := completeForkActivation(context.Background(), spec, f, plan, rt, nil); err == nil {
		t.Fatalf("%s was reported as success", what)
	}

	assertActivationFencePhase(t, spec.Name, fencePhaseAdopted)
}

// installActivationConfigSteps supplies the real config and roles step hooks
// while configFail makes the next configuration write fail.
func installActivationConfigSteps(t *testing.T, spec config.ClusterSpec, configFail *bool) {
	t.Helper()

	restoreConfig := steps.SetConfigTestHooks(steps.ConfigTestHooks{
		Write: func(path, content string, mode os.FileMode, owner, group string) error {
			if *configFail {
				return errors.New("config write failed")
			}

			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}

			return os.WriteFile(path, []byte(content), mode)
		},
		Validate: func(context.Context, config.Cluster, config.ClusterSpec) error { return nil },
		Script: func(context.Context, engine.Runtime, string, string, map[string]string, engine.Reporter) error {
			return nil
		},
	})
	t.Cleanup(restoreConfig)

	restoreRoles := steps.SetRolesTestHooks(steps.RolesTestHooks{
		QueryColumn: func(_ context.Context, _ pg.Local, _, sql string) ([]string, error) {
			switch {
			case strings.Contains(sql, "pg_roles"):
				return []string{"vaire_admin", "vaire_monitor", "postgres"}, nil
			case strings.Contains(sql, "pg_database"):
				return []string{"postgres", spec.DatabaseName()}, nil
			case strings.Contains(sql, "pg_proc"):
				return nil, nil
			case strings.Contains(sql, "pg_extension"):
				return []string{"pg_stat_statements"}, nil
			}

			return nil, nil
		},
		Exec: func(context.Context, pg.Local, string, string, map[string]string) error { return nil },
		LoadedPreloads: func(context.Context, pg.Local) (map[string]bool, error) {
			return map[string]bool{"pg_stat_statements": true, "auto_explain": true}, nil
		},
		PendingRestart:   func(context.Context, pg.Local) ([]string, error) { return nil, nil },
		WriteRuntimePass: func(engine.Runtime, string, string) error { return nil },
	})
	t.Cleanup(restoreRoles)
}

func TestApplyRefusesActiveActivationFenceBeforeEffects(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "restore-promote", "op-1")

	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }

	t.Cleanup(func() { applyReloadCluster = oldReload })

	_, err := handleApply(context.Background(), &Env{Req: request("apply", nil), Cluster: cfg})
	if err == nil || extReason(t, err) != "activation_fence_active" {
		t.Fatalf("fenced apply = %v", err)
	}
}

func TestApplyRefusesChangedLockClassAfterReload(t *testing.T) {
	old := planCfg(t)
	reloaded := extFollowerCfg(t)
	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return reloaded, nil }

	t.Cleanup(func() { applyReloadCluster = oldReload })

	_, err := handleApply(context.Background(), &Env{Req: request("apply", map[string]any{"db": "app"}), Cluster: old})
	if err == nil || extReason(t, err) != "apply_lock_class_changed" {
		t.Fatalf("changed lock class = %v", err)
	}
}

func TestActivationFenceBlocksSQLAndForeignInstance(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "restore-promote", "op-2")

	if err := refuseUnsettledSQL("app"); err == nil || extReason(t, err) != "activation_fence_active" {
		t.Fatalf("fenced SQL = %v", err)
	}

	if err := backup.UpdateActivationFence("app", "op-2", func(f *backup.ActivationFence) error {
		f.Instance = "other-instance"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := refuseActiveActivationFence(cfg, "app"); err == nil || !strings.Contains(err.Error(), "another installation instance") {
		t.Fatalf("foreign fence = %v", err)
	}
}

func TestUnprotectedSQLChecksFenceBeforeConnection(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "restore-promote", "op-sql")

	env := &Env{Req: request("sql", map[string]any{"db": "app", "query": "SELECT 1"}), Cluster: cfg}
	if _, err := handleSQL(context.Background(), env); err == nil || extReason(t, err) != "activation_fence_active" {
		t.Fatalf("unprotected SQL reached the cluster behind a fence: %v", err)
	}
}

func TestRoutingFinalizerVerifiesBothPoolersAndClearsExactFence(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "restore-promote", "op-3")

	if err := backup.UpdateActivationFence("app", "op-3", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldRoutes, oldResume := poolerRuntimeRoutes, poolerResumeDatabase

	t.Cleanup(func() { poolerRuntimeRoutes, poolerResumeDatabase = oldRoutes, oldResume })

	resumed := 0
	poolerResumeDatabase = func(context.Context, string) { resumed++ }
	poolerRuntimeRoutes = func(_ context.Context, dir string, _ int) (map[string]steps.PoolerRoute, error) {
		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}

	if err := verifyRoutingFence(context.Background(), cfg, spec, "op-3", "restore-promote"); err != nil {
		t.Fatal(err)
	}

	if resumed != 1 {
		t.Fatalf("pause releases = %d, want 1 (both poolers are resumed together)", resumed)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.ActivationFenceFor("app"); ok {
		t.Fatal("the exact routing fence was not cleared")
	}
}

func TestRoutingFinalizerFailureLeavesRoutingFence(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "restore-promote", "op-4")

	if err := backup.UpdateActivationFence("app", "op-4", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldRoutes, oldResume := poolerRuntimeRoutes, poolerResumeDatabase

	t.Cleanup(func() { poolerRuntimeRoutes, poolerResumeDatabase = oldRoutes, oldResume })

	poolerResumeDatabase = func(context.Context, string) {}
	poolerRuntimeRoutes = func(_ context.Context, dir string, _ int) (map[string]steps.PoolerRoute, error) {
		if dir == steps.SessionSocketDir {
			return map[string]steps.PoolerRoute{}, nil
		}

		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}

	if err := verifyRoutingFence(context.Background(), cfg, spec, "op-4", "restore-promote"); err == nil {
		t.Fatal("a missing session-pooler alias was accepted")
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Phase != fencePhaseRouting {
		t.Fatalf("routing failure did not leave the fence in routing: %+v", f)
	}
}

func TestApplyRefusesAdoptedForkWithoutRetainedOwnership(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "fork", "fork-1")

	if err := backup.UpdateActivationFence("app", "fork-1", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseAdopted
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldReload, oldResume := applyReloadCluster, resumeForkSettlementAt
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	called := false
	resumeForkSettlementAt = func(context.Context, *Env, backup.ActivationFence) (Result, error) {
		called = true
		return Result{}, nil
	}

	t.Cleanup(func() { applyReloadCluster, resumeForkSettlementAt = oldReload, oldResume })

	_, err := handleApply(context.Background(), &Env{Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg})
	if err == nil || extReason(t, err) != "materialisation_incomplete" {
		t.Fatalf("adopted fence without a retained record = %v", err)
	}

	if called {
		t.Fatal("an unowned adopted fence dispatched a continuation")
	}
}

func TestCompleteForkActivationRunsNonSecretHandoff(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	useTempGuardWriter(t)
	restoreRoot := steps.PGConfRootForTest(t.TempDir())
	t.Cleanup(restoreRoot)
	installActivationExtensionTree(t,
		[]string{"custom.control", "pg_stat_statements.control"},
		[]string{"custom_lib.so", "pg_stat_statements.so", "auto_explain.so"})
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]
	spec.Extensions = []string{"custom"}
	cfg.Clusters[0] = spec
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, Preload: "custom_lib",
			CopySafety: new(catalog.CopySafetyDisabled), StandbySafe: new(true),
			CopySettings: map[string]string{"custom.worker": "off"},
		},
	}
	useEncodedFollowerStore(t, cfg)

	oldPrune := pruneCopyGuard
	pruneCopyGuard = pruneCopyGuardForSettlement

	t.Cleanup(func() { pruneCopyGuard = oldPrune })

	oldStart := forkStartCluster

	t.Cleanup(func() { forkStartCluster = oldStart })

	started := false
	forkStartCluster = func(context.Context, config.Cluster, engine.Runtime, config.ClusterSpec, engine.Reporter) error {
		started = true
		return nil
	}
	published := false
	stubActivationPublication(t, spec, &published)

	dataDir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	useFollowRegistrationHooks(t, dataDir)
	f, plan := adoptForkFence(t, cfg, spec, activationFenceIdentity{
		Database: spec.Name, Kind: "fork", OperationID: "fork-full", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: "source-abc123", NewStanza: spec.Stanza,
		Registration: activationRegistration(spec), Port: spec.Port,
		DataDir: dataDir, ConfDir: steps.PGConfDir(spec.Name),
		Requirements: []catalog.Extension{{
			Name: "custom", ExtName: "custom", Source: catalog.SourcePreinstalled, Preload: "custom_lib",
			CopySafety: catalog.CopySafetyDisabled, StandbySafe: true,
			CopySettings: map[string]string{"custom.worker": "off"},
		}},
		Renames: map[string]string{"app_source": spec.Name},
	})

	updated, warnings, err := completeForkActivation(context.Background(), spec, f, plan, engine.Runtime{}, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !started || !published {
		t.Fatalf("continuation did not start/publish: started=%v published=%v warnings=%v", started, published, warnings)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.ActivationFenceFor(spec.Name); ok {
		t.Fatal("the completed fork left its activation fence")
	}

	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}

	if !strings.Contains(strings.Join(guard.Preloads, ","), "custom_lib") {
		t.Fatalf("guard lost the required library: %+v", guard)
	}

	if _, ok := updated.ClusterByName(spec.Name); !ok {
		t.Fatal("the continuation lost the database entry")
	}
}

func TestRetireRestoreAndRouteIsAtomic(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "restore-promote", "op-retire")

	if err := backup.UpdateActivationFence("app", "op-retire", func(f *backup.ActivationFence) error {
		f.Phase = fencePhasePromoted
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	r := backup.Restore{ID: "op-retire", Database: "app", Aside: "app__pre", Kind: backup.TargetLatest, Port: spec.Port, Phase: backup.PhaseReady}

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = r
		s.Timelines["app"] = backup.TimelineSwitch{Stanza: spec.Stanza, Timeline: "2"}

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A changed restore identity leaves both the restore and the fence whole.
	changed := r

	changed.ID = "other-op"
	if err := retireRestoreAndRoute("app", changed, spec, "app__pre", spec.Stanza, spec.Stanza, spec.Stanza); err == nil {
		t.Fatal("a changed restore was retired")
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("the failed retirement removed the restore record")
	}

	if f, ok := recs.ActivationFenceFor("app"); !ok || f.Phase != fencePhasePromoted {
		t.Fatalf("the failed retirement advanced the fence: %+v", f)
	}
	// The exact identity retires the restore, timeline and leftover and sets
	// routing in one update.
	if err := retireRestoreAndRoute("app", r, spec, "app__pre", spec.Stanza, spec.Stanza, spec.Stanza); err != nil {
		t.Fatal(err)
	}

	recs, err = backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.Restores["app"]; ok {
		t.Fatal("the retired restore record stayed")
	}

	if _, ok := recs.Timelines["app"]; ok {
		t.Fatal("the retired restore timeline stayed")
	}

	if len(recs.Leftovers) != 1 || recs.Leftovers[0].Name != "app__pre" {
		t.Fatalf("leftovers = %+v", recs.Leftovers)
	}

	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Phase != fencePhaseRouting {
		t.Fatalf("routing eligibility was not set atomically: %+v", f)
	}
}

func TestForkRoutingFailureRetriesThroughApply(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]

	dataDir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	confRoot := t.TempDir()
	restoreSteps, oldMatConf := steps.PGConfRootForTest(confRoot), materialisationConfigRoot
	materialisationConfigRoot = confRoot

	t.Cleanup(func() { restoreSteps(); materialisationConfigRoot = oldMatConf })

	confDir := filepath.Join(confRoot, spec.Name)
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}

	registration := "data_directory = '" + dataDir + "'\ncluster_name = '18/" + spec.Name + "'\nport = 5433\n"
	if err := os.WriteFile(filepath.Join(confDir, "postgresql.conf"), []byte(registration), 0o600); err != nil {
		t.Fatal(err)
	}

	oldConf, oldData := followConfRegistered, followRegisteredData
	followConfRegistered = func(string) bool { return true }
	followRegisteredData = func(string) (string, error) { return dataDir, nil }

	t.Cleanup(func() { followConfRegistered, followRegisteredData = oldConf, oldData })
	writeTestFence(t, cfg, "fork", "fork-route")

	if err := backup.UpdateActivationFence("app", "fork-route", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldPublish, oldRoutes, oldResume := publishRouting, poolerRuntimeRoutes, poolerResumeDatabase

	t.Cleanup(func() { publishRouting, poolerRuntimeRoutes, poolerResumeDatabase = oldPublish, oldRoutes, oldResume })

	fail := true
	publishRouting = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		if fail {
			return errors.New("pooler reload failed")
		}

		return nil
	}
	poolerResumeDatabase = func(context.Context, string) {}
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}
	env := &Env{Cluster: cfg}

	res, err := resumeForkSettlement(context.Background(), env, backup.ActivationFence{
		OperationID: "fork-route", Kind: "fork", Database: "app", Phase: fencePhaseRouting,
	})
	if err != nil || len(res.Warnings) == 0 {
		t.Fatalf("a routing failure was not reported as a warning: %+v %v", res, err)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if f, ok := recs.ActivationFenceFor("app"); !ok || f.Phase != fencePhaseRouting {
		t.Fatalf("routing failure did not leave the fence: %+v", f)
	}

	fail = false

	res, err = resumeForkSettlement(context.Background(), env, backup.ActivationFence{
		OperationID: "fork-route", Kind: "fork", Database: "app", Phase: fencePhaseRouting,
	})
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("routing retry = %+v %v", res, err)
	}

	recs, err = backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.ActivationFenceFor("app"); ok {
		t.Fatal("the verified routing retry did not clear the fence")
	}
}

func TestFenceIdentityRejectsReusedStanzaPortAndOperation(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "fork", "op-a")
	reusedStanza := cfg.Clone()

	reusedStanza.Clusters[0].Stanza = "app-new999"
	if _, _, err := verifyActivationFence(reusedStanza, "app", "op-a", "fork", fencePhasePrepared); err == nil {
		t.Fatal("a reused name with a new stanza inherited the old fence")
	}

	reusedPort := cfg.Clone()

	reusedPort.Clusters[0].Port = 5444
	if _, _, err := verifyActivationFence(reusedPort, "app", "op-a", "fork", fencePhasePrepared); err == nil {
		t.Fatal("a reused name with a new port inherited the old fence")
	}

	if _, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: spec.Name, Kind: "fork", OperationID: "op-b", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: "source-abc123", NewStanza: spec.Stanza,
		Registration: activationRegistration(spec), Port: spec.Port,
		DataDir: cfg.Server.DataDirectory(spec.Name), ConfDir: steps.PGConfDir(spec.Name),
	}); err == nil || extReason(t, err) != "activation_fence_changed" {
		t.Fatalf("another operation overwrote the fence: %v", err)
	}
}

func TestAnyFenceRefusesDropAndReidentity(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "restore-promote", "op-drop")

	if err := backup.UpdateActivationFence("app", "op-drop", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := refuseAnyActivationFence(cfg, "app"); err == nil || extReason(t, err) != "activation_fence_active" {
		t.Fatalf("a routing fence did not block a destructive change: %v", err)
	}
}

func TestRestoreFenceFailureLeavesDataUnswapped(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]
	empty := []string{}
	spec.RecoveryExtensions = &empty
	cfg.Clusters[0] = spec
	liveData := cfg.Server.DataDirectory(spec.Name)

	restoringData := cfg.Server.DataDirectory(backup.RestoringName(spec.Name))
	for _, dir := range []string{liveData, restoringData} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{
			ID: "restore-fence", Database: "app", Kind: backup.TargetLatest,
			Phase: backup.PhaseReady, StartedAt: "2026-01-01T00:00:00Z", Port: 6001,
			Aside: "app__pre_restore_20260101T000000",
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldEnsure := ensureCopyGuardAt
	ensureCopyGuardAt = func(context.Context, string, *extensionCopyPlan, copyGuardEndpoint, func() error) ([]string, error) {
		return nil, nil
	}
	oldFence := fenceRestorePoolers
	oldApplyFence := applyPoolerFence
	marker := errors.New("pooler fence failed")
	fenceRestorePoolers = func(context.Context, string) error { return marker }

	t.Cleanup(func() { ensureCopyGuardAt, fenceRestorePoolers, applyPoolerFence = oldEnsure, oldFence, oldApplyFence })

	_, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {})
	if !errors.Is(err, marker) {
		t.Fatalf("promote fence failure = %v", err)
	}

	for _, dir := range []string{liveData, restoringData} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("the failed fence changed %s: %v", dir, statErr)
		}
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Phase != fencePhasePrepared {
		t.Fatalf("the failed fence did not leave its prepared checkpoint: %+v", f)
	}
	// A render/reload failure after the cut has the same property: no resume
	// and no swap.
	fenceRestorePoolers = func(context.Context, string) error { return nil }
	renderMarker := errors.New("pooler render failed")
	applyPoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error { return renderMarker }
	resumed := 0
	oldResume := poolerResumeDatabase
	poolerResumeDatabase = func(context.Context, string) { resumed++ }

	t.Cleanup(func() { poolerResumeDatabase = oldResume })

	if _, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {}); !errors.Is(err, renderMarker) {
		t.Fatalf("promote render failure = %v", err)
	}

	if resumed != 0 {
		t.Fatalf("a render failure resumed the poolers %d times", resumed)
	}

	for _, dir := range []string{liveData, restoringData} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("the failed render changed %s: %v", dir, statErr)
		}
	}
}

func TestWriteActivationFenceKeepsAdvancedCheckpoint(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "restore-promote", "op-advanced")

	if err := backup.UpdateActivationFence("app", "op-advanced", func(f *backup.ActivationFence) error {
		f.Phase = fencePhasePromoted
		f.Registration = activationRegistration(spec)
		f.Port = spec.Port
		f.DataDir = cfg.Server.DataDirectory(spec.Name)
		f.ConfDir = steps.PGConfDir(spec.Name)

		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A retry proposes the original prepared temporary endpoint; the durable
	// promoted checkpoint owns the live endpoint instead.
	got, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: spec.Name, Kind: "restore-promote", OperationID: "op-advanced", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Registration: "app__restoring:6001:", Port: 6001,
		DataDir: cfg.Server.DataDirectory(backup.RestoringName(spec.Name)),
		ConfDir: steps.PGConfDir(backup.RestoringName(spec.Name)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.Phase != fencePhasePromoted || got.Port != spec.Port || got.Registration != activationRegistration(spec) {
		t.Fatalf("the retry rewrote the advanced checkpoint: %+v", got)
	}
}

func TestRestorePromoteRetryAfterSwappedCompletes(t *testing.T) {
	cfg, _ := swappedPromotionFixture(t)
	if _, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {}); err != nil {
		t.Fatal(err)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.Restores["app"]; ok {
		t.Fatal("swapped retry left the restore record")
	}

	if _, ok := recs.ActivationFenceFor("app"); ok {
		t.Fatal("swapped retry left the activation fence")
	}
}

func swappedPromotionFixture(t *testing.T) (config.Cluster, *bool) {
	t.Helper()
	stubRestoreCredentialLifecycle(t)
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]
	empty := []string{}
	spec.RecoveryExtensions = &empty
	cfg.Clusters[0] = spec
	aside := "app__pre_restore_20260101T000000"

	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores["app"] = backup.Restore{
			ID: "swapped-retry", Database: "app", Kind: backup.TargetLatest,
			Phase: backup.PhaseReady, StartedAt: "2026-01-01T00:00:00Z", Port: 6001, Aside: aside,
		}

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: "app", Kind: "restore-promote", OperationID: "swapped-retry", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza, Aside: aside,
		Registration: activationRegistration(spec), Port: spec.Port,
		DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
	}); err != nil {
		t.Fatal(err)
	}

	if err := backup.UpdateActivationFence("app", "swapped-retry", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseSwapped
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldEnsure := ensureCopyGuardAt
	ensureCopyGuardAt = func(context.Context, string, *extensionCopyPlan, copyGuardEndpoint, func() error) ([]string, error) {
		return nil, nil
	}
	oldFenceRestore, oldApplyFence := fenceRestorePoolers, applyPoolerFence
	fenceRestorePoolers = func(context.Context, string) error { return nil }
	applyPoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error { return nil }
	oldSwap, oldStart, oldPromote, oldSettle := swapCluster, startCluster, promoteCluster, settlePromoted
	swapCluster = func(context.Context, engine.Shell, string, engine.Reporter, string, string, string, string, int) error {
		return nil
	}
	startCluster = func(context.Context, engine.Shell, string, engine.Reporter, string, string, int, string) error {
		return nil
	}
	promoteCluster = func(context.Context, pg.Local, string) ([]string, error) { return nil, nil }
	settlePromoted = func(context.Context, engine.Shell, string, engine.Reporter, pg.Local, string, string, int, []string) error {
		return nil
	}
	oldPublish, oldRoutes, oldResume := publishRouting, poolerRuntimeRoutes, poolerResumeDatabase
	published := false
	publishRouting = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		published = true
		return nil
	}
	poolerResumeDatabase = func(context.Context, string) {}
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		if !published {
			return map[string]steps.PoolerRoute{}, nil
		}

		return map[string]steps.PoolerRoute{
			"app": {Name: "app", Host: steps.PGSocketDir, Port: spec.Port, Database: "app"},
		}, nil
	}

	t.Cleanup(func() {
		ensureCopyGuardAt, fenceRestorePoolers, applyPoolerFence = oldEnsure, oldFenceRestore, oldApplyFence
		swapCluster, startCluster, promoteCluster, settlePromoted = oldSwap, oldStart, oldPromote, oldSettle
		publishRouting, poolerRuntimeRoutes, poolerResumeDatabase = oldPublish, oldRoutes, oldResume
	})

	return cfg, &published
}

func TestInstallCopyPlanGuardRefusesUnknownPriorGuard(t *testing.T) {
	useTempGuardWriter(t)
	extRoot := t.TempDir()

	controlDir, libDir := filepath.Join(extRoot, "extension"), filepath.Join(extRoot, "lib")
	for _, dir := range []string{controlDir, libDir} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	restoreExt := steps.ExtensionDirsForTest(controlDir, libDir)
	t.Cleanup(restoreExt)

	if err := os.WriteFile(filepath.Join(libDir, "unknown_lib.so"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	confDir, dataDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := backup.InstallCopyGuard(dataDir, backup.CopyGuard{Preloads: []string{"unknown_lib"}}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dataDir, "postgresql.auto.conf")

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app"}

	plan := &backup.CopyPlan{}
	if _, _, err := backup.InstallCopyPlanGuard(confDir, dataDir, plan); err == nil {
		t.Fatal("an unexplained prior guard was overwritten")
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(before) != string(after) {
		t.Fatalf("the refused guard changed the file:\n%s\n%s", before, after)
	}
	// Explicit compatible metadata explains the same library: it becomes a
	// retained requirement and the guard is written.
	known := []catalog.Extension{{
		Name: "unknown", Source: catalog.SourcePreinstalled, Preload: "unknown_lib",
		CopySafety: catalog.CopySafetySafe, StandbySafe: true,
	}}

	plan = &backup.CopyPlan{
		Guard:         backup.CopyGuard{Preloads: []string{"unknown_lib"}},
		ValidateGuard: finalPreloadValidator(cfg, spec, "app", known, nil, false, nil),
	}
	if _, _, err := backup.InstallCopyPlanGuard(confDir, dataDir, plan); err != nil {
		t.Fatalf("explicit compatible metadata was refused: %v", err)
	}

	guard, present, err := backup.ReadCopyGuard(dataDir)
	if err != nil || !present || !strings.Contains(strings.Join(guard.Preloads, ","), "unknown_lib") {
		t.Fatalf("guard = %+v, %v, %v", guard, present, err)
	}
}

func TestForkRoutingStaleInstancePublishesNothing(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	writeTestFence(t, cfg, "fork", "fork-stale")

	if err := backup.UpdateActivationFence("app", "fork-stale", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		f.Instance = "another-instance"

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldPublish, oldRoutes := publishRouting, poolerRuntimeRoutes

	t.Cleanup(func() { publishRouting, poolerRuntimeRoutes = oldPublish, oldRoutes })

	published := 0
	publishRouting = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error { published++; return nil }
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}

	env := &Env{Cluster: cfg}
	if _, err := resumeForkSettlement(context.Background(), env, backup.ActivationFence{
		OperationID: "fork-stale", Kind: "fork", Database: "app", Phase: fencePhaseRouting,
	}); err == nil {
		t.Fatal("a stale-instance routing fence was accepted")
	}

	if published != 0 {
		t.Fatalf("a stale routing fence published %d times", published)
	}
}

func TestForkRoutingInvalidRegistrationPublishesNothing(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	writeTestFence(t, cfg, "fork", "fork-bad-registration")

	if err := backup.UpdateActivationFence("app", "fork-bad-registration", func(f *backup.ActivationFence) error {
		f.Phase = fencePhaseRouting
		f.Registration = "wrong:5433:"

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	oldPublish := publishRouting

	t.Cleanup(func() { publishRouting = oldPublish })

	published := 0
	publishRouting = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		published++
		return nil
	}

	env := &Env{Cluster: cfg}
	if _, err := resumeForkSettlement(context.Background(), env, backup.ActivationFence{
		OperationID: "fork-bad-registration", Kind: "fork", Database: "app", Phase: fencePhaseRouting,
	}); err == nil {
		t.Fatal("an invalid routing registration was accepted")
	}

	if published != 0 {
		t.Fatalf("an invalid routing registration published %d times", published)
	}
}

func TestForkContinuationEnvelopeSerializesSettings(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{Name: "app2", Node: "db1", Port: 5434, Stanza: "app2-def456", Size: "small", StatementTimeout: "60s"})
	store := state.FileStore{Path: filepath.Join(t.TempDir(), "cluster.toml")}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	if err := store.Save(raw); err != nil {
		t.Fatal(err)
	}

	oldState := followerState
	followerState = func() state.Store { return store }

	t.Cleanup(func() { followerState = oldState })
	writeTestFence(t, cfg, "fork", "op-envelope")

	if got := applyLockWithConfig(cfg, request("apply", map[string]any{"db": "app"})); got != engine.LockExclusive {
		t.Fatalf("a pending fork apply envelope = %s", got)
	}

	restoreLock := engine.LockPathForTest(filepath.Join(t.TempDir(), "vaire.lock"))
	t.Cleanup(restoreLock)

	plan := func(name, setting string) *backup.CopyPlan {
		return &backup.CopyPlan{Requirements: []catalog.Extension{{
			Name: name, Source: catalog.SourcePreinstalled,
			CopySafety: catalog.CopySafetyDisabled, StandbySafe: true,
			CopySettings: map[string]string{setting: "off"},
		}}}
	}
	started := make(chan struct{})
	attempted := make(chan struct{})

	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()

		release, err := engine.Acquire(context.Background(), engine.LockExclusive, "fork-a", "a", 5*time.Second)
		if err != nil {
			t.Error(err)
			return
		}

		close(started)
		<-attempted

		if _, _, err := persistCopySettings("app", plan("a", "a.worker"), "app"); err != nil {
			t.Error(err)
		}

		release()
	}()

	<-started

	go func() {
		defer wg.Done()

		close(attempted)

		release, err := engine.Acquire(context.Background(), engine.LockExclusive, "fork-b", "b", 5*time.Second)
		if err != nil {
			t.Error(err)
			return
		}

		if _, _, err := persistCopySettings("app2", plan("b", "b.worker"), "app2"); err != nil {
			t.Error(err)
		}

		release()
	}()

	wg.Wait()

	loaded, _, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	one, _ := loaded.ClusterByName("app")

	two, _ := loaded.ClusterByName("app2")
	if one.Settings["a.worker"] != "off" || two.Settings["b.worker"] != "off" {
		t.Fatalf("concurrent continuations lost settings: app=%v app2=%v", one.Settings, two.Settings)
	}

	if one.Stanza != "app-abc123" || two.Stanza != "app2-def456" || one.Port != 5433 || two.Port != 5434 {
		t.Fatalf("concurrent continuations lost identity: app=%+v app2=%+v", one, two)
	}
}

func TestApplyEnvelopeUnreadableRecordsNeverShared(t *testing.T) {
	useTempExtensionRecords(t)

	cfg := planCfg(t)
	if err := os.WriteFile(backup.RecordsPath, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := applyLockWithConfig(cfg, request("apply", map[string]any{"db": "app"})); got != engine.LockExclusive {
		t.Fatalf("unreadable records envelope = %s, want exclusive", got)
	}
}

func TestApplyEnvelopeClassChangeRefusedBeforeEffects(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }

	t.Cleanup(func() { applyReloadCluster = oldReload })

	env := &Env{Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockShared}
	writeTestFence(t, cfg, "fork", "op-class")

	if _, err := handleApply(context.Background(), env); err == nil || extReason(t, err) != "apply_lock_class_changed" {
		t.Fatalf("fence appearance while waiting = %v", err)
	}

	if err := backup.ClearActivationFence("app", "op-class"); err != nil {
		t.Fatal(err)
	}

	env.LockMode = engine.LockExclusive
	if _, err := handleApply(context.Background(), env); err == nil || extReason(t, err) != "apply_lock_class_changed" {
		t.Fatalf("fence disappearance while waiting = %v", err)
	}
}

func TestCompleteForkActivationRealEngineConfigRoles(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	useTempGuardWriter(t)
	installActivationExtensionTree(t,
		[]string{"pg_stat_statements.control"},
		[]string{"pg_stat_statements.so", "auto_explain.so"})
	restoreConf := steps.PGConfRootForTest(t.TempDir())
	t.Cleanup(restoreConf)

	restoreInstance := steps.SetGuardInstanceForTest(func() (string, error) { return "test-instance", nil })
	t.Cleanup(restoreInstance)
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]

	dataDir := cfg.Server.DataDirectory(spec.Name)
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		t.Fatal(err)
	}

	writeActivationConfFixture(t, spec.Name)
	useEncodedFollowerStore(t, cfg)
	useFollowRegistrationHooks(t, dataDir)

	configFail := false
	installActivationConfigSteps(t, spec, &configFail)

	oldOnly, oldJournal := forkStartOnly, forkStartJournal
	forkStartOnly = []string{steps.ConfigStepID(spec.Name), steps.RolesStepID(spec.Name)}
	journalPath := filepath.Join(t.TempDir(), "journal.json")
	forkStartJournal = journalPath

	t.Cleanup(func() { forkStartOnly, forkStartJournal = oldOnly, oldJournal })

	published := false
	stubActivationPublication(t, spec, &published)
	f, plan := adoptForkFence(t, cfg, spec, activationFenceIdentity{
		Database: spec.Name, Kind: "fork", OperationID: "fork-engine", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: "source-abc123", NewStanza: spec.Stanza,
		Registration: activationRegistration(spec), Port: spec.Port,
		DataDir: dataDir, ConfDir: steps.PGConfDir(spec.Name),
		Renames: map[string]string{"app_source": spec.Name},
	})

	secretValues := map[string]string{
		steps.SecretPgbouncerAuth: "test-pw",
		steps.SecretMonitor:       "test-pw",
	}
	for _, role := range steps.DatabaseRoles {
		secretValues[steps.SecretDatabaseRole(spec.Name, role)] = "test-pw"
	}

	rt := engine.Runtime{SecretValues: secretValues}
	configFail = true

	assertCompleteForkActivationFails(t, spec, f, plan, rt, "a config failure after adoption")

	configFail = false
	oldSQL := reconcileCopySQL
	sqlFail := true
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		if sqlFail {
			return nil, errors.New("SQL settlement failed")
		}

		return nil, nil
	}

	t.Cleanup(func() { reconcileCopySQL = oldSQL })
	assertCompleteForkActivationFails(t, spec, f, plan, rt, "an SQL failure after adoption")

	sqlFail = false

	updated, warnings, err := completeForkActivation(context.Background(), spec, f, plan, rt, nil)
	if err != nil {
		t.Fatal(err)
	}

	if !published {
		t.Fatalf("real engine continuation did not publish routing: %v", warnings)
	}

	if _, err := os.Stat(filepath.Join(steps.PGConfD(spec.Name), "10-vaire-tuning.conf")); err != nil {
		t.Fatalf("real config step did not write its files: %v", err)
	}

	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("real engine did not write its journal: %v", err)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if _, ok := recs.ActivationFenceFor(spec.Name); ok {
		t.Fatal("real engine continuation left its activation fence")
	}

	if _, ok := updated.ClusterByName(spec.Name); !ok {
		t.Fatal("real engine continuation lost the database entry")
	}
}

func TestPendingConfigChangeRefusedWhileFenceActive(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	old := planCfg(t)
	writeTestFence(t, old, "fork", "fork-cfg")
	updated := old.Clone()

	updated.Clusters[0].Port = 5440
	if err := refusePendingConfigChanges(old, updated); err == nil || extReason(t, err) != "activation_fence_pending" {
		t.Fatalf("pending config re-identity = %v", err)
	}

	updated = old.Clone()

	updated.Clusters = nil
	if err := refusePendingConfigChanges(old, updated); err == nil || extReason(t, err) != "activation_fence_pending" {
		t.Fatalf("pending config removal = %v", err)
	}
}
