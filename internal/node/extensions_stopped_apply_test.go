package node

import (
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/steps"
)

func stoppedApplyCfg(t *testing.T) config.Cluster {
	t.Helper()

	cfg := extFollowerCfg(t)
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"pg_cron"}
	}

	return cfg
}

// stoppedApplyRecorder models the engine's start, configuration and SQL effects
// for stopped-target transitions, recording their order.
type stoppedApplyRecorder struct {
	running          map[string]bool
	effects          []string
	primaryPending   bool
	failStart        string
	failStartErr     error
	afterStartEffect bool
}

func (r *stoppedApplyRecorder) apply(opts engine.Options) (engine.ApplyData, error) {
	step := opts.Only[0]
	if strings.HasPrefix(step, "postgres-cluster-v1") {
		if opts.DB == r.failStart {
			return engine.ApplyData{}, r.failStartErr
		}

		r.effects = append(r.effects, "start:"+opts.DB)
		r.running[opts.DB] = true

		return engine.ApplyData{}, nil
	}

	if r.afterStartEffect {
		r.effects = append(r.effects, "after-start:"+opts.DB+":"+step)
		return engine.ApplyData{}, nil
	}

	if strings.HasPrefix(step, "postgres-config-v1") {
		r.effects = append(r.effects, "config:"+opts.DB)
		if r.primaryPending && opts.DB == "app" {
			return engine.ApplyData{RestartPending: []string{step}}, nil
		}

		return engine.ApplyData{}, nil
	}

	r.effects = append(r.effects, "other:"+opts.DB)

	return engine.ApplyData{}, nil
}

func TestStoppedApplyRecorderCatchesEveryPostStartStep(t *testing.T) {
	for _, step := range []string{"postgres-config-v1@app", "postgres-roles-v1@app"} {
		t.Run(step, func(t *testing.T) {
			recorder := stoppedApplyRecorder{afterStartEffect: true}
			if _, err := recorder.apply(engine.Options{DB: "app", Only: []string{step}}); err != nil {
				t.Fatal(err)
			}

			if want := []string{"after-start:app:" + step}; !slices.Equal(recorder.effects, want) {
				t.Fatalf("post-start step escaped the failure-boundary recorder: got %v, want %v", recorder.effects, want)
			}
		})
	}
}

func assertStoppedTargetEffects(t *testing.T, effects []string) {
	t.Helper()

	firstStart := slices.IndexFunc(effects, func(effect string) bool { return strings.HasPrefix(effect, "start:") })
	if firstStart < 0 {
		t.Fatalf("no engine-owned start was reached: %v", effects)
	}

	for _, guard := range []string{"guard:app_follower_1", "guard:app_follower_2"} {
		i := slices.Index(effects, guard)
		if i < 0 || i > firstStart {
			t.Fatalf("follower guard %q was not established before a start: %v", guard, effects)
		}
	}

	wantStarts := []string{"start:app", "start:app_follower_1", "start:app_follower_2"}

	starts := make([]string, 0, len(effects))
	for _, effect := range effects {
		if strings.HasPrefix(effect, "start:") {
			starts = append(starts, effect)
		}
	}

	if !slices.Equal(starts, wantStarts) {
		t.Fatalf("starts = %v, want %v (effects %v)", starts, wantStarts, effects)
	}

	for _, follower := range []string{"config:app_follower_1", "config:app_follower_2"} {
		if !slices.Contains(effects, follower) {
			t.Fatalf("follower %q was not verified after start: %v", follower, effects)
		}
	}

	if sql := slices.Index(effects, "other:app"); sql >= 0 {
		for _, follower := range []string{"config:app_follower_1", "config:app_follower_2"} {
			if sql < slices.Index(effects, follower) {
				t.Fatalf("primary SQL ran before follower %q was verified: %v", follower, effects)
			}
		}
	}
}

func assertStoppedUnloadProof(t *testing.T, effects, proofs []string, stoppedStart string) {
	t.Helper()

	if slices.Index(effects, "start:"+stoppedStart) < 0 {
		t.Fatalf("%s was never started: %v", stoppedStart, effects)
	}

	for _, proof := range proofs {
		if proof == stoppedStart+":stopped" {
			t.Fatalf("stopped target's unload proof ran before its start: %v (effects %v)", proofs, effects)
		}
	}

	if !slices.Contains(proofs, stoppedStart+":running") {
		t.Fatalf("stopped target was not unload-proven after start: %v (effects %v)", proofs, effects)
	}
}

func assertFollowersVerifiedBeforePrimary(t *testing.T, effects []string) {
	t.Helper()

	for _, follower := range []string{"app_follower_1", "app_follower_2"} {
		verified := slices.Index(effects, "config:"+follower)
		if verified < 0 {
			t.Fatalf("follower %s was not verified: %v", follower, effects)
		}

		if primary := slices.Index(effects, "config:app"); primary >= 0 && verified > primary {
			t.Fatalf("follower %s was verified after primary configuration: %v", follower, effects)
		}

		if sql := slices.Index(effects, "other:app"); sql >= 0 && verified > sql {
			t.Fatalf("follower %s was verified after primary SQL: %v", follower, effects)
		}
	}
}

func TestHandleApplyStartsStoppedSourceAndFollowersBeforeCatalog(t *testing.T) {
	cfg := stoppedApplyCfg(t)
	h := newExtHarness(t, cfg)
	recorder := &stoppedApplyRecorder{running: map[string]bool{}, primaryPending: true}
	h.runningFn = func(spec config.ClusterSpec) bool { return recorder.running[spec.Name] }

	oldValidate, oldPlan, oldReload := extensionValidateStopped, extensionPlan, applyReloadCluster
	extensionValidateStopped = func(_ context.Context, _ config.Cluster, spec config.ClusterSpec) error {
		recorder.effects = append(recorder.effects, "guard:"+spec.Name)
		return nil
	}
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }

	t.Cleanup(func() {
		extensionValidateStopped, extensionPlan, applyReloadCluster = oldValidate, oldPlan, oldReload
	})

	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		for _, spec := range cfg.Clusters {
			if !recorder.running[spec.Name] {
				return nil, errors.New("catalog is down")
			}
		}

		return map[string]extensionInstalledVersion{}, nil
	}
	h.preloadLoaded["pg_cron"] = true
	cronSettingFake(h)
	h.applyFn = recorder.apply

	res, err := handleApply(context.Background(), &Env{
		Req: request("apply", map[string]any{"db": "app"}), Cluster: cfg, LockMode: engine.LockExclusive,
	})
	if err != nil {
		t.Fatalf("stopped source/follower apply = %v; effects %v", err, recorder.effects)
	}

	data, ok := res.Data.(engine.ApplyData)
	if !ok || len(data.RestartPending) == 0 {
		t.Fatalf("staged result = %+v, want a pending primary restart after follower verification", res.Data)
	}

	assertStoppedTargetEffects(t, recorder.effects)
}

func TestApplyStoppedFollowerPreflightDoesNotReadCatalog(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	running := map[string]bool{"app": true}
	h.runningFn = func(spec config.ClusterSpec) bool { return running[spec.Name] }

	failFollowerCatalog(t)

	var validateCalls, startCalls int

	oldValidate := extensionValidateStopped
	extensionValidateStopped = func(_ context.Context, _ config.Cluster, spec config.ClusterSpec) error {
		if spec.Name != "app_follower_1" {
			t.Fatalf("unexpected guard for %s", spec.Name)
		}

		validateCalls++

		return nil
	}

	t.Cleanup(func() { extensionValidateStopped = oldValidate })

	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		if strings.HasPrefix(opts.Only[0], "postgres-cluster-v1") {
			startCalls++
			running[opts.DB] = true
		}

		return engine.ApplyData{}, nil
	}

	req := request("apply", map[string]any{"db": "app_follower_1", "steps": []string{"postgres-cluster-v1@app_follower_1"}})

	warnings, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{
		DB: "app_follower_1", Steps: []string{"postgres-cluster-v1@app_follower_1"},
	})
	if release != nil {
		release()
	}

	if err != nil {
		t.Fatalf("stopped follower start = %v", err)
	}

	_ = warnings

	if validateCalls != 1 || startCalls != 1 {
		t.Fatalf("guard/start calls = %d/%d, want 1/1", validateCalls, startCalls)
	}
}

func TestSelectedClusterStartDoesNotEnterExtensionTransition(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.runningFn = func(config.ClusterSpec) bool { return false }
	catalogCalls := 0
	restore := steps.SetFollowerInstalledExtensionsForTest(func(context.Context, config.ClusterSpec) (map[string]bool, error) {
		catalogCalls++
		return nil, errors.New("catalog must not be read")
	})
	t.Cleanup(restore)

	req := request("apply", map[string]any{"steps": []string{"postgres-cluster-v1"}})

	warnings, release, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{
		Steps: []string{"postgres-cluster-v1"},
	})
	if release != nil {
		release()
	}

	if err != nil {
		t.Fatalf("selected cluster start = %v", err)
	}

	_ = warnings

	if catalogCalls != 0 || len(h.applyCalls) != 0 {
		t.Fatalf("selected start entered the extension transition: catalog=%d applies=%v", catalogCalls, h.applyCalls)
	}
}

func writeStoppedManagedPreloads(t *testing.T, cluster string) {
	t.Helper()

	confDir := steps.PGConfDir(cluster)
	if err := os.MkdirAll(filepath.Join(confDir, "conf.d"), 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(confDir, "conf.d", "30-vaire-extensions.conf"),
		[]byte("shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_cron'\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

type stoppedUnloadProofCase struct {
	name         string
	running      map[string]bool
	stoppedStart string
}

func runStoppedUnloadProofCase(t *testing.T, tc stoppedUnloadProofCase) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()

	restoreConf := steps.PGConfRootForTest(t.TempDir())
	defer restoreConf()

	writeStoppedManagedPreloads(t, "app")
	writeStoppedManagedPreloads(t, "app_follower_1")
	writeStoppedManagedPreloads(t, "app_follower_2")

	recorder := &stoppedApplyRecorder{running: maps.Clone(tc.running), primaryPending: true}
	h := newExtHarness(t, cfg)
	h.runningFn = func(spec config.ClusterSpec) bool { return recorder.running[spec.Name] }

	var unloadProofs []string

	oldValidate, oldUnload, oldPlan, oldReload := extensionValidateStopped, extensionRefuseUnload, extensionPlan, applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }
	extensionValidateStopped = func(_ context.Context, _ config.Cluster, spec config.ClusterSpec) error {
		recorder.effects = append(recorder.effects, "guard:"+spec.Name)
		return nil
	}
	extensionRefuseUnload = func(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, first bool) error {
		unloadProofs = append(unloadProofs, spec.Name+":"+map[bool]string{true: "running", false: "stopped"}[recorder.running[spec.Name]])
		if recorder.running[spec.Name] {
			return nil
		}
		// Prove the managed-file fallback really refuses a stopped
		// preload removal in this fixture.
		return steps.RefuseExtensionUnload(ctx, cfg, spec, first)
	}
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}

	t.Cleanup(func() {
		extensionValidateStopped, extensionRefuseUnload, extensionPlan, applyReloadCluster = oldValidate, oldUnload, oldPlan, oldReload
	})

	h.installedFn = func(int) (map[string]extensionInstalledVersion, error) {
		for _, isRunning := range recorder.running {
			if !isRunning {
				return nil, errors.New("catalog is down")
			}
		}

		return map[string]extensionInstalledVersion{}, nil
	}
	h.applyFn = recorder.apply

	req := request("apply", map[string]any{"db": "app"})

	_, err := handleApply(context.Background(), &Env{Req: req, Cluster: cfg, LockMode: engine.LockExclusive})
	if err != nil {
		t.Fatalf("stopped unload transition = %v; effects=%v proofs=%v", err, recorder.effects, unloadProofs)
	}

	assertStoppedUnloadProof(t, recorder.effects, unloadProofs, tc.stoppedStart)
	assertFollowersVerifiedBeforePrimary(t, recorder.effects)
}

func TestStoppedApplyDefersUnloadProofUntilAfterStart(t *testing.T) {
	for _, tc := range []stoppedUnloadProofCase{
		{name: "stopped source", running: map[string]bool{"app_follower_1": true, "app_follower_2": true}, stoppedStart: "app"},
		{name: "stopped follower", running: map[string]bool{"app": true, "app_follower_2": true}, stoppedStart: "app_follower_1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runStoppedUnloadProofCase(t, tc)
		})
	}
}

func writeFollowerFence(t *testing.T, cfg config.Cluster, follower string, phase string) {
	t.Helper()

	spec, ok := cfg.ClusterByName(follower)
	if !ok {
		t.Fatalf("missing follower %s", follower)
	}

	spec.Stanza = follower + "-abc123"

	fence, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: spec.Name, Kind: "fork", OperationID: "op-fence", Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Aside: follower + "__pre", Registration: activationRegistration(spec),
		Port: spec.Port, DataDir: cfg.Server.DataDirectory(spec.Name),
		ConfDir: steps.PGConfDir(spec.Name),
	})
	if err != nil {
		t.Fatal(err)
	}

	if phase != fencePhasePrepared {
		if err := backup.UpdateActivationFence(spec.Name, fence.OperationID, func(f *backup.ActivationFence) error {
			f.Phase = phase
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestApplyStoppedFollowerActivationFenceRefusesBeforeEffects(t *testing.T) {
	for _, phase := range []string{fencePhasePrepared, fencePhaseAdopted, fencePhaseRouting} {
		t.Run(phase, func(t *testing.T) {
			cfg := extFollowerCfg(t)
			cfg.Server.DataRoot = t.TempDir()
			h := newExtHarness(t, cfg)
			useTempExtensionRecords(t)
			useTestInstance(t)
			writeFollowerFence(t, cfg, "app_follower_1", phase)

			running := map[string]bool{"app": true, "app_follower_2": true}
			h.runningFn = func(spec config.ClusterSpec) bool { return running[spec.Name] }

			var validateCalls, startCalls int

			oldValidate, oldPlan := extensionValidateStopped, extensionPlan
			extensionValidateStopped = func(context.Context, config.Cluster, config.ClusterSpec) error {
				validateCalls++
				return nil
			}
			extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
				return engine.PlanData{WouldApply: 1}, nil
			}

			t.Cleanup(func() { extensionValidateStopped, extensionPlan = oldValidate, oldPlan })

			h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
				startCalls++
				running[opts.DB] = true

				return engine.ApplyData{}, nil
			}

			req := request("apply", map[string]any{"db": "app"})

			_, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
			if err == nil || extReason(t, err) != "activation_fence_active" {
				t.Fatalf("phase %s fence = %v", phase, err)
			}

			if validateCalls != 0 || startCalls != 0 || len(h.saves) != 0 || len(h.pg.execs) != 0 {
				t.Fatalf("phase %s fence had effects: guard=%d start=%d saves=%d exec=%d",
					phase, validateCalls, startCalls, len(h.saves), len(h.pg.execs))
			}
		})
	}
}

func TestApplyTargetSetPreflightWinsBeforeActivationFence(t *testing.T) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	h := newExtHarness(t, cfg)
	useTempExtensionRecords(t)
	useTestInstance(t)
	writeFollowerFence(t, cfg, "app_follower_1", fencePhaseRouting)

	h.records = backup.Records{Unfollowing: map[string]backup.Unfollow{
		"app_follower_1": {Source: "app"},
	}}
	running := map[string]bool{"app": true, "app_follower_2": true}
	h.runningFn = func(spec config.ClusterSpec) bool { return running[spec.Name] }
	validateCalls, startCalls := 0, 0
	oldValidate, oldPlan := extensionValidateStopped, extensionPlan
	extensionValidateStopped = func(context.Context, config.Cluster, config.ClusterSpec) error {
		validateCalls++
		return nil
	}
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}

	t.Cleanup(func() { extensionValidateStopped, extensionPlan = oldValidate, oldPlan })

	h.applyFn = func(opts engine.Options) (engine.ApplyData, error) {
		startCalls++
		running[opts.DB] = true

		return engine.ApplyData{}, nil
	}

	req := request("apply", map[string]any{"db": "app"})

	_, _, err := applyExtensionTransitions(context.Background(), &Env{Req: req, Cluster: cfg}, stepParams{DB: "app"})
	if err == nil || extReason(t, err) != "unfollow_in_progress" {
		t.Fatalf("target-set preflight = %v, want unfollow_in_progress", err)
	}

	if validateCalls != 0 || startCalls != 0 || len(h.saves) != 0 || len(h.pg.execs) != 0 {
		t.Fatalf("target-set preflight lost precedence: validate=%d start=%d saves=%d exec=%d",
			validateCalls, startCalls, len(h.saves), len(h.pg.execs))
	}
}

func followerCustomDefinition() map[string]config.ExtensionDefinition {
	return map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom_ext",
			Settings:    map[string]string{"custom.mode": "on"},
			CopySafety:  new(catalog.CopySafetySafe),
			StandbySafe: new(true),
		},
	}
}

func TestVerifyFollowerRuntimeIgnoresUnrelatedSettings(t *testing.T) {
	cfg := extFollowerCfg(t)

	cfg.Extensions = followerCustomDefinition()
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"custom"}
	}

	cfg.Postgres.Settings = map[string]string{
		"random_page_cost":    "1.10",
		"hash_mem_multiplier": "2.0",
		"client_min_messages": "WARNING",
	}
	cfg.Clusters[1].Settings = map[string]string{"work_mem": "8MB"}
	h := newExtHarness(t, cfg)
	h.pendingSettings = []string{"work_mem", "random_page_cost"}
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		if strings.Contains(sql, "custom.mode") {
			return []map[string]string{{"v": "on", "vartype": "enum"}}, nil
		}

		return nil, errors.New("unrelated setting was read as an extension dependency: " + sql)
	}

	if err := verifyFollowerRuntime(context.Background(), cfg, cfg.Clusters[1]); err != nil {
		t.Fatalf("unrelated settings blocked the follower runtime: %v", err)
	}
}

func TestVerifyFollowerRuntimeOwnedTypesCanonical(t *testing.T) {
	cfg := extFollowerCfg(t)

	cfg.Extensions = map[string]config.ExtensionDefinition{
		"typed": {
			Source: catalog.SourcePreinstalled, ExtName: "typed_ext",
			Settings: map[string]string{
				"custom.ratio": "'1.10'",
				"custom.mode":  "'ON'",
				"custom.size":  "'1MB'",
			},
			CopySafety:  new(catalog.CopySafetySafe),
			StandbySafe: new(true),
		},
	}
	for i := range cfg.Clusters {
		cfg.Clusters[i].Extensions = []string{"typed"}
	}

	h := newExtHarness(t, cfg)
	ratio := "1.1"
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		switch {
		case strings.Contains(sql, "custom.ratio"):
			return []map[string]string{{"v": ratio, "vartype": "real"}}, nil
		case strings.Contains(sql, "custom.mode"):
			return []map[string]string{{"v": "on", "vartype": "enum"}}, nil
		case strings.Contains(sql, "custom.size"):
			return []map[string]string{{"v": "1024kB", "vartype": "integer", "unit": "kB"}}, nil
		default:
			return nil, errors.New("unexpected setting query: " + sql)
		}
	}

	if err := verifyFollowerRuntime(context.Background(), cfg, cfg.Clusters[1]); err != nil {
		t.Fatalf("canonical extension-owned values were refused: %v", err)
	}

	ratio = "1.2"

	if err := verifyFollowerRuntime(context.Background(), cfg, cfg.Clusters[1]); err == nil || extReason(t, err) != "extension_settings_missing" {
		t.Fatalf("a real mismatch was accepted: %v", err)
	}
}

func TestVerifyFollowerRuntimeImplicitDefaultsAreNotExpected(t *testing.T) {
	cfg := extFollowerCfg(t)
	h := newExtHarness(t, cfg)
	h.pg.queryFn = func(sql string) ([]map[string]string, error) {
		return nil, errors.New("implicit default became an expected dependency: " + sql)
	}

	if err := verifyFollowerRuntime(context.Background(), cfg, cfg.Clusters[1]); err != nil {
		t.Fatalf("implicit default settings blocked an empty extension selection: %v", err)
	}
}

type stoppedStartBoundaryCase struct {
	name         string
	running      map[string]bool
	failProof    string
	failStart    string
	wantStarted  []string
	wantPending  []string
	failedTarget string
	failedStage  string
	wantReason   string
	startUnknown bool
}

func assertStoppedStartBoundary(t *testing.T, err error, tc stoppedStartBoundaryCase, effects []string) {
	t.Helper()

	details := extDetails(t, err)
	if details["started"] != (len(tc.wantStarted) > 0) ||
		details["failed_target"] != tc.failedTarget ||
		details["failed_stage"] != tc.failedStage ||
		details["reason"] != tc.wantReason ||
		details["start_unknown"] != tc.startUnknown ||
		details["configuration_staged"] != false ||
		details["sql_run"] != false ||
		details["packages_may_remain"] != true {
		t.Fatalf("partial details = %v", details)
	}

	if got, _ := details["started_targets"].([]string); !slices.Equal(got, tc.wantStarted) {
		t.Fatalf("started_targets = %v, want %v (details %v)", got, tc.wantStarted, details)
	}

	if got, _ := details["pending_targets"].([]string); !slices.Equal(got, tc.wantPending) {
		t.Fatalf("pending_targets = %v, want %v (details %v)", got, tc.wantPending, details)
	}

	for _, effect := range effects {
		if strings.HasPrefix(effect, "after-start:") {
			t.Fatalf("configuration/SQL ran after a start-boundary failure: %v", effects)
		}
	}

	failure := output.Fail(nil, err)
	if tc.failedStage == "unload_proof" {
		if !strings.HasPrefix(failure.Error.Hint, tc.failedTarget+" was started on its old configuration;") ||
			!strings.Contains(failure.Error.Hint, "--db app, then retry apply") {
			t.Fatalf("post-start hint does not identify the started target and source retry: %+v", failure.Error)
		}
	}

	if !slices.ContainsFunc(failure.Warnings, func(w output.Warning) bool { return w.Code == "start_warning" }) {
		t.Fatalf("start warnings were discarded: %+v", failure.Warnings)
	}
}

func runStoppedStartBoundaryCase(t *testing.T, tc stoppedStartBoundaryCase) {
	cfg := extFollowerCfg(t)
	cfg.Server.DataRoot = t.TempDir()

	restoreConf := steps.PGConfRootForTest(t.TempDir())
	defer restoreConf()

	for _, cluster := range []string{"app", "app_follower_1", "app_follower_2"} {
		writeStoppedManagedPreloads(t, cluster)
	}

	h := newExtHarness(t, cfg)
	recorder := &stoppedApplyRecorder{
		running:   maps.Clone(tc.running),
		failStart: tc.failStart,
		failStartErr: output.NewError(output.CodePartialFailure,
			"installing extension packages failed before the cluster answered", output.ExitStep,
			"packages may remain").
			WithDetails(map[string]any{"reason": "package_install_failed", "database": tc.failStart}),
		afterStartEffect: true,
	}
	h.runningFn = func(spec config.ClusterSpec) bool { return recorder.running[spec.Name] }

	oldValidate, oldUnload, oldPlan, oldReload := extensionValidateStopped, extensionRefuseUnload, extensionPlan, applyReloadCluster
	extensionValidateStopped = func(context.Context, config.Cluster, config.ClusterSpec) error { return nil }
	extensionRefuseUnload = func(_ context.Context, _ config.Cluster, spec config.ClusterSpec, _ bool) error {
		if spec.Name == tc.failProof {
			return output.NewError(output.CodePrecondition,
				"installed extension still needs the old library", output.ExitPrecondition, "").
				WithDetails(map[string]any{"reason": "extension_unload_unproven"})
		}

		return nil
	}
	extensionPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
		return engine.PlanData{WouldApply: 1}, nil
	}
	applyReloadCluster = func() (config.Cluster, error) { return cfg, nil }

	t.Cleanup(func() {
		extensionValidateStopped, extensionRefuseUnload, extensionPlan, applyReloadCluster = oldValidate, oldUnload, oldPlan, oldReload
	})

	h.applyWarnings = []output.Warning{{Code: "start_warning", Message: "start observed"}}
	h.applyFn = recorder.apply

	req := request("apply", map[string]any{"db": "app"})

	_, err := handleApply(context.Background(), &Env{Req: req, Cluster: cfg, LockMode: engine.LockExclusive})
	if err == nil {
		t.Fatal("start-boundary failure was reported as success")
	}

	assertStoppedStartBoundary(t, err, tc, recorder.effects)
}

func TestStoppedApplyStartBoundaryFailureReturnsPartialState(t *testing.T) {
	for _, tc := range []stoppedStartBoundaryCase{
		{
			name: "source proof after start", running: map[string]bool{},
			failProof: "app", wantStarted: []string{"app"}, wantPending: []string{"app_follower_1", "app_follower_2"},
			failedTarget: "app", failedStage: "unload_proof", wantReason: "extension_unload_unproven",
		},
		{
			name: "follower proof after start", running: map[string]bool{"app": true, "app_follower_2": true},
			failProof: "app_follower_1", wantStarted: []string{"app_follower_1"}, wantPending: []string{},
			failedTarget: "app_follower_1", failedStage: "unload_proof", wantReason: "extension_unload_unproven",
		},
		{
			name: "later target start failure", running: map[string]bool{},
			failStart: "app_follower_2", wantStarted: []string{"app", "app_follower_1"}, wantPending: []string{},
			failedTarget: "app_follower_2", failedStage: "start", wantReason: "package_install_failed", startUnknown: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runStoppedStartBoundaryCase(t, tc)
		})
	}
}
