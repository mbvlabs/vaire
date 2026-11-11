package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/pg"
)

func TestPrepareExtensionStartTargetIsolation(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Extensions: []string{"app_ext"}},
		{Name: "other", Extensions: []string{"other_ext"}},
	}
	cfg.Awaiting = []config.ClusterSpec{{Name: "awaiting", Extensions: []string{"awaiting_ext"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"app_ext":      {Source: catalog.SourceAPT, Package: "postgresql-18-app"},
		"other_ext":    {Source: catalog.SourceAPT, Package: "postgresql-18-other"},
		"awaiting_ext": {Source: catalog.SourceAPT, Package: "postgresql-18-awaiting"},
	}
	for _, name := range []string{"pg_stat_statements", "app_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "app_lib"} {
		touchExtensionLibrary(t, name)
	}
	// app_ext has no preload, so only its control/module matters.
	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	var got map[string]string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		got = env
		return nil
	}
	if err := PrepareExtensionStart(context.Background(), cfg, cfg.Clusters[0], nil, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if got["EXTENSION_PACKAGES"] != "postgresql-18-app" {
		t.Fatalf("target isolation package env = %#v", got)
	}
	if strings.Contains(got["EXTENSION_PACKAGES"], "other") || strings.Contains(got["EXTENSION_PACKAGES"], "awaiting") {
		t.Fatalf("target preflight leaked another cluster: %#v", got)
	}
}

func TestExtensionRuntimeRequirementsIncludePreloadOnlyDefaults(t *testing.T) {
	cfg := config.DefaultCluster()
	spec := config.ClusterSpec{Name: "app", Port: 5433}
	reqs, err := ExtensionPrerequisitesForSpec(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	var auto *ExtensionPrerequisite
	for i := range reqs {
		if reqs[i].Name == "auto_explain" {
			auto = &reqs[i]
		}
	}
	if auto == nil {
		t.Fatalf("preload-only default omitted from runtime requirements: %+v", reqs)
	}
	if auto.ExtName != "" || auto.Preload != "auto_explain" || auto.Source != catalog.SourcePreinstalled {
		t.Fatalf("auto_explain runtime requirement = %+v", *auto)
	}

	// SQL creation is a separate projection and must never include the
	// preload-only default.
	sqlEntries, err := extensionSQLSelection(cfg, spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range sqlEntries {
		if entry.Name == "auto_explain" {
			t.Fatalf("auto_explain entered SQL creation: %+v", entry)
		}
	}
	missing, err := missingExtensionNames(cfg, spec, map[string]bool{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(missing, []string{"pg_stat_statements"}) {
		t.Fatalf("missing SQL extensions = %v, want only pg_stat_statements", missing)
	}
	if sql := ExtensionSQL(catalog.Extension{Name: "auto_explain", Preload: "auto_explain"}, "app"); sql != "" {
		t.Fatalf("preload-only module produced CREATE EXTENSION SQL: %q", sql)
	}
}

func TestExtensionRuntimeRequirementsAutoExplainAPTOverride(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {
			Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	reqs, err := ExtensionPrerequisitesForSpec(cfg, cfg.Clusters[0])
	if err != nil {
		t.Fatal(err)
	}
	packages := extensionPackagesFrom(reqs)
	if !slices.Contains(packages, "postgresql-18-auto-explain") {
		t.Fatalf("auto_explain APT override omitted from target packages: %v", packages)
	}
	sqlEntries, err := extensionSQLSelection(cfg, cfg.Clusters[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range sqlEntries {
		if entry.Name == "auto_explain" {
			t.Fatalf("auto_explain APT override entered SQL creation: %+v", entry)
		}
	}
}

func TestExtensionPackagesIncludeAutoExplainAPTOverride(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {
			Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	packages, err := ExtensionPackages(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(packages, []string{"postgresql-18-auto-explain"}) {
		t.Fatalf("global auto_explain package union = %v", packages)
	}
}

func TestConfigInstallsAutoExplainOverrideBeforeFirstWrite(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "autoexplaincfg", Size: "small", Port: 5433}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {
			Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	touchExtensionControl(t, "pg_stat_statements")
	for _, name := range []string{"pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}
	oldScript, oldWrite := runExtensionPackageScript, configWriteFileOwned
	t.Cleanup(func() {
		runExtensionPackageScript, configWriteFileOwned = oldScript, oldWrite
	})
	var order []string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		if env["EXTENSION_PACKAGES"] != "postgresql-18-auto-explain" {
			t.Fatalf("auto_explain package not installed/held alone: %#v", env)
		}
		order = append(order, "install")
		return nil
	}
	configWriteFileOwned = func(string, string, os.FileMode, string, string) error {
		order = append(order, "write")
		return errors.New("stop after first write")
	}
	_, err := (postgresConfig{}).Instance("autoexplaincfg").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "stop after first write") {
		t.Fatalf("config write sentinel = %v", err)
	}
	if !slices.Equal(order, []string{"install", "write"}) {
		t.Fatalf("config order = %v, want auto_explain install before write", order)
	}
}

func TestRestarterInstallsAutoExplainOverrideBeforePause(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"auto_explain": {
			Source: catalog.SourceAPT, Package: "postgresql-18-auto-explain",
			CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true),
		},
	}
	touchExtensionControl(t, "pg_stat_statements")
	for _, name := range []string{"pg_stat_statements", "auto_explain"} {
		touchExtensionLibrary(t, name)
	}
	oldScript := runExtensionPackageScript
	oldPause, oldRun := restarterPoolerAdmin, restarterRunCmd
	oldWait, oldPending := restarterWaitReady, restarterPendingCheck
	t.Cleanup(func() {
		runExtensionPackageScript = oldScript
		restarterPoolerAdmin, restarterRunCmd = oldPause, oldRun
		restarterWaitReady, restarterPendingCheck = oldWait, oldPending
	})
	var order []string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		if env["EXTENSION_PACKAGES"] != "postgresql-18-auto-explain" {
			t.Fatalf("auto_explain package not installed/held alone: %#v", env)
		}
		order = append(order, "install")
		return nil
	}
	restarterPoolerAdmin = func(context.Context, int, string) error { order = append(order, "pause"); return nil }
	restarterRunCmd = func(context.Context, string, ...string) error { order = append(order, "restart"); return nil }
	restarterWaitReady = func(context.Context, int) error { return nil }
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) { return nil, nil }
	if _, err := RestarterFor(cfg, engine.Runtime{}, nil).Restart(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "install" || order[1] != "pause" {
		t.Fatalf("restart order = %v, want auto_explain install before pause", order)
	}
}

func TestMissingAutoExplainRefusesBeforeConfigWrite(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "autoexplainmissing", Size: "small", Port: 5433}}
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		t.Fatal("a preinstalled-only requirement ran the package script")
		return nil
	}
	_, err := (postgresConfig{}).Instance("autoexplainmissing").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "auto_explain.so") ||
		!strings.Contains(err.Error(), `preinstalled extension "auto_explain"`) {
		t.Fatalf("missing auto_explain = %v", err)
	}
	if _, statErr := os.Stat(PGConfDir("autoexplainmissing")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("config wrote a file before the auto_explain prerequisite failed: %v", statErr)
	}
}

func TestRolesCheckReportsMissingAutoExplain(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	useFakeRolesPG(t, fake)
	if err := os.Remove(filepath.Join(extensionLibDir, "auto_explain.so")); err != nil {
		t.Fatal(err)
	}
	written := 0
	rolesWriteRuntimePass = func(engine.Runtime, string, string) error {
		written++
		return nil
	}
	step := postgresRoles{}.Instance("app")
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete ||
		!strings.Contains(res.Detail, "auto_explain.so") {
		t.Fatalf("roles auto_explain check = %+v, %v", res, err)
	}
	if _, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil); err == nil ||
		!strings.Contains(err.Error(), "auto_explain.so") {
		t.Fatalf("roles auto_explain apply = %v", err)
	}
	if len(fake.execs) != 0 || written != 0 {
		t.Fatalf("roles wrote effects before the auto_explain prerequisite failed: execs=%d pass=%d", len(fake.execs), written)
	}
}

// TestTuneApplyCommandSelectionStopsBeforeConfigWrite runs the exact step
// selection tune.apply passes (TuneStepIDs) through the engine, with a test
// journal so the kernel step is already complete. The config step's guard must
// stop the run before its first write. The node handler itself is not called:
// it hardcodes the node journal path, so this is the hermetic command-boundary
// trace.
func TestTuneApplyCommandSelectionStopsBeforeConfigWrite(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "tunecfgtest", Size: "small", Port: 5433,
		Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	kernelInputs, err := (kernelTuning{}).Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		kernelTuningID: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(kernelTuningID, kernelInputs)},
	}}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	_, _, err = engine.Apply(context.Background(), Catalog(), cfg, engine.Runtime{}, engine.Options{
		Only: TuneStepIDs(), JournalPath: journal,
	})
	if err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("tune apply selection = %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("tune apply failure is not a step error: %v", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["step"] != ConfigStepID("tunecfgtest") {
		t.Fatalf("tune apply failed in %v, want %s", details["step"], ConfigStepID("tunecfgtest"))
	}
	if _, statErr := os.Stat(PGConfDir("tunecfgtest")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("tune apply wrote a config file before the prerequisite failed: %v", statErr)
	}
}

func TestPrerequisiteFailureStopsRolesEffects(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	useFakeRolesPG(t, fake)
	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	written := 0
	rolesWriteRuntimePass = func(engine.Runtime, string, string) error {
		written++
		return nil
	}
	_, err := (postgresRoles{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("roles prerequisite failure = %v", err)
	}
	if len(fake.execs) != 0 || written != 0 {
		t.Fatalf("roles wrote effects before the prerequisite failed: execs=%d pass=%d", len(fake.execs), written)
	}
}

type guardLaterStep struct{ applied *int }

func (s guardLaterStep) ID() string                     { return "later-v1" }
func (s guardLaterStep) Describe(config.Cluster) string { return "later-v1" }
func (s guardLaterStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte("later-v1"), nil
}
func (s guardLaterStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: true}, nil
}
func (s guardLaterStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	*s.applied++
	return engine.ApplyResult{}, nil
}

func TestPrerequisiteFailureStopsLaterApply(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	useFakeRolesPG(t, fake)
	if err := os.Remove(filepath.Join(extensionControlDir, "custom_ext.control")); err != nil {
		t.Fatal(err)
	}
	applied := 0
	journal := filepath.Join(t.TempDir(), "journal.json")
	_, _, err := engine.Apply(context.Background(), []engine.Step{postgresRoles{}, guardLaterStep{applied: &applied}}, cfg, engine.Runtime{}, engine.Options{
		JournalPath: journal,
	})
	if err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("apply prerequisite failure = %v", err)
	}
	if applied != 0 {
		t.Fatalf("a later step ran after the prerequisite failure: %d", applied)
	}
}

func TestConfigInstallsBeforeFirstWrite(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Size: "small", Port: 5433,
		Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom_ext", Preload: "custom_lib"},
	}
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	oldScript, oldWrite := runExtensionPackageScript, configWriteFileOwned
	t.Cleanup(func() {
		runExtensionPackageScript, configWriteFileOwned = oldScript, oldWrite
	})
	var order []string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		if env["EXTENSION_PACKAGES"] != "postgresql-18-custom" {
			t.Fatalf("install env = %#v", env)
		}
		order = append(order, "install")
		return nil
	}
	configWriteFileOwned = func(string, string, os.FileMode, string, string) error {
		order = append(order, "write")
		return errors.New("stop after first write")
	}
	_, err := (postgresConfig{}).Instance("app").Apply(context.Background(), cfg, engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "stop after first write") {
		t.Fatalf("config write sentinel = %v", err)
	}
	if !slices.Equal(order, []string{"install", "write"}) {
		t.Fatalf("config order = %v, want install before write", order)
	}
}

func TestPrerequisiteFailureStopsConfigAndClusterFiles(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "guardcfgtest", Size: "small", Port: 5433,
		Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	if _, err := (postgresConfig{}).Instance("guardcfgtest").Apply(context.Background(), cfg, engine.Runtime{}, nil); err == nil ||
		!strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("config prerequisite failure = %v", err)
	}
	if _, err := os.Stat(PGConfDir("guardcfgtest")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config wrote a file before the prerequisite failed: %v", err)
	}
	if _, err := (postgresCluster{}).Instance("guardcfgtest").Apply(context.Background(), cfg, engine.Runtime{}, nil); err == nil ||
		!strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("cluster prerequisite failure = %v", err)
	}
	if _, err := os.Stat(PGServiceDropIn("guardcfgtest")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cluster wrote a file before the prerequisite failed: %v", err)
	}
}

func TestPrerequisiteFailureStopsRestarterEffects(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	oldPause, oldRun := restarterPoolerAdmin, restarterRunCmd
	oldWait, oldPending := restarterWaitReady, restarterPendingCheck
	t.Cleanup(func() {
		restarterPoolerAdmin, restarterRunCmd = oldPause, oldRun
		restarterWaitReady, restarterPendingCheck = oldWait, oldPending
	})
	var calls []string
	restarterPoolerAdmin = func(context.Context, int, string) error { calls = append(calls, "pause"); return nil }
	restarterRunCmd = func(context.Context, string, ...string) error { calls = append(calls, "restart"); return nil }
	restarterWaitReady = func(context.Context, int) error { calls = append(calls, "wait"); return nil }
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) { calls = append(calls, "pending"); return nil, nil }
	r := postgresRestarter{cfg: cfg, guarded: true}
	if _, err := r.Restart(context.Background(), "app"); err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("restart prerequisite failure = %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("restart acted before the prerequisite failed: %v", calls)
	}
}

func TestRestarterInstallsBeforePause(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom", ExtName: "custom_ext", Preload: "custom_lib"},
	}
	for _, name := range []string{"pg_stat_statements", "custom_ext"} {
		touchExtensionControl(t, name)
	}
	for _, name := range []string{"pg_stat_statements", "auto_explain", "custom_lib"} {
		touchExtensionLibrary(t, name)
	}
	oldScript := runExtensionPackageScript
	oldPause, oldRun := restarterPoolerAdmin, restarterRunCmd
	oldWait, oldPending := restarterWaitReady, restarterPendingCheck
	t.Cleanup(func() {
		runExtensionPackageScript = oldScript
		restarterPoolerAdmin, restarterRunCmd = oldPause, oldRun
		restarterWaitReady, restarterPendingCheck = oldWait, oldPending
	})
	var order []string
	runExtensionPackageScript = func(_ context.Context, _ string, env map[string]string, _ engine.Runtime, _ engine.Reporter) error {
		if env["EXTENSION_PACKAGES"] != "postgresql-18-custom" {
			t.Fatalf("install env = %#v", env)
		}
		order = append(order, "install")
		return nil
	}
	restarterPoolerAdmin = func(context.Context, int, string) error { order = append(order, "pause"); return nil }
	restarterRunCmd = func(context.Context, string, ...string) error { order = append(order, "restart"); return nil }
	restarterWaitReady = func(context.Context, int) error { return nil }
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) { return nil, nil }
	if _, err := RestarterFor(cfg, engine.Runtime{}, nil).Restart(context.Background(), "app"); err != nil {
		t.Fatal(err)
	}
	if len(order) < 2 || order[0] != "install" || order[1] != "pause" {
		t.Fatalf("restart order = %v, want install before pause", order)
	}
}

func TestRestartOnlyGuardFailureKeepsPending(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Port: 5433, Extensions: []string{"custom"},
	}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourcePreinstalled, ExtName: "custom_ext"},
	}
	step := rolesRestartTemplate{}.Instance("app")
	inputs, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{
		Steps: map[string]engine.JournalEntry{
			step.ID(): {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(step.ID(), inputs)},
		},
		RestartPending: []string{step.ID()},
	}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	oldPending := restarterPendingCheck
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) {
		return []string{"shared_preload_libraries"}, nil
	}
	t.Cleanup(func() { restarterPendingCheck = oldPending })
	r := postgresRestarter{cfg: cfg, guarded: true}
	_, _, err = engine.Apply(context.Background(), []engine.Step{rolesRestartTemplate{}}, cfg, engine.Runtime{}, engine.Options{
		JournalPath: journal, Restarter: r, Confirm: engine.Confirmation{Clusters: []string{"app"}},
	})
	if err == nil || !strings.Contains(err.Error(), "custom_ext.control") {
		t.Fatalf("restart-only failure = %v", err)
	}
	j, err = engine.LoadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(j.RestartPending, []string{step.ID()}) {
		t.Fatalf("failed restart guard cleared pending: %v", j.RestartPending)
	}
}

func TestRestartOnlyMissingAutoExplainKeepsPending(t *testing.T) {
	extensionTestDirs(t)
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433}}
	touchExtensionControl(t, "pg_stat_statements")
	touchExtensionLibrary(t, "pg_stat_statements")
	step := rolesRestartTemplate{}.Instance("app")
	inputs, err := step.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{
		Steps: map[string]engine.JournalEntry{
			step.ID(): {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(step.ID(), inputs)},
		},
		RestartPending: []string{step.ID()},
	}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	oldPending := restarterPendingCheck
	oldPause, oldRun := restarterPoolerAdmin, restarterRunCmd
	oldWait := restarterWaitReady
	t.Cleanup(func() {
		restarterPendingCheck = oldPending
		restarterPoolerAdmin, restarterRunCmd = oldPause, oldRun
		restarterWaitReady = oldWait
	})
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) {
		return []string{"shared_preload_libraries"}, nil
	}
	restarterPoolerAdmin = func(context.Context, int, string) error {
		t.Fatal("the restart paused the pooler before auto_explain was verified")
		return nil
	}
	restarterRunCmd = func(context.Context, string, ...string) error {
		t.Fatal("the restart ran before auto_explain was verified")
		return nil
	}
	restarterWaitReady = func(context.Context, int) error {
		t.Fatal("the restart waited for readiness before auto_explain was verified")
		return nil
	}
	_, _, err = engine.Apply(context.Background(), []engine.Step{rolesRestartTemplate{}}, cfg, engine.Runtime{}, engine.Options{
		JournalPath: journal, Restarter: RestarterFor(cfg, engine.Runtime{}, nil),
		Confirm: engine.Confirmation{Clusters: []string{"app"}},
	})
	if err == nil || !strings.Contains(err.Error(), "auto_explain.so") {
		t.Fatalf("restart-only missing auto_explain = %v", err)
	}
	j, err = engine.LoadJournal(journal)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(j.RestartPending, []string{step.ID()}) {
		t.Fatalf("failed auto_explain guard cleared pending: %v", j.RestartPending)
	}
}

func TestReadOnlyPathsDoNotInstall(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	useFakeRolesPG(t, fake)
	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		t.Fatal("a read-only path ran the package script")
		return nil
	}
	oldProbe := packageProbe
	packageProbe = func(_ context.Context, name string, args ...string) (string, error) {
		switch name {
		case "dpkg-query":
			return "install ok installed", nil
		case "apt-mark":
			return strings.Join(append(append([]string(nil), pgHold...), "postgresql-18-custom"), "\n"), nil
		}
		return "", nil
	}
	t.Cleanup(func() { packageProbe = oldProbe })
	if _, err := (postgresPackages{}).Inputs(cfg, engine.Runtime{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (postgresConfig{}).Instance("app").Inputs(cfg, engine.Runtime{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (postgresCluster{}).Instance("app").Inputs(cfg, engine.Runtime{}); err != nil {
		t.Fatal(err)
	}
	if _, err := (postgresRoles{}).Instance("app").Inputs(cfg, engine.Runtime{}); err != nil {
		t.Fatal(err)
	}
	if res, err := (postgresPackages{}).Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("package read-only check = %+v, %v", res, err)
	}
	if res, err := (postgresRoles{}).Instance("app").Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("roles read-only check = %+v, %v", res, err)
	}
	oldPending := restarterPendingCheck
	restarterPendingCheck = func(context.Context, pg.Local) ([]string, error) { return nil, nil }
	t.Cleanup(func() { restarterPendingCheck = oldPending })
	if pending, err := (postgresRestarter{cfg: cfg}).NeedsRestart(context.Background(), "app"); err != nil || pending {
		t.Fatalf("NeedsRestart = %v, %v", pending, err)
	}

	// Plan forces the two Recheckers' Check and still performs no install.
	packagesInputs, err := (postgresPackages{}).Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	rolesInputs, err := (postgresRoles{}).Instance("app").Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "journal.json")
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		postgresPackagesID: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(postgresPackagesID, packagesInputs)},
		RolesStepID("app"): {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(RolesStepID("app"), rolesInputs)},
	}}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	plan, err := engine.Plan(context.Background(), []engine.Step{postgresPackages{}, postgresRoles{}}, cfg, engine.Runtime{}, engine.Options{JournalPath: journal})
	if err != nil {
		t.Fatal(err)
	}
	if plan.WouldApply != 0 {
		t.Fatalf("read-only plan would apply: %+v", plan)
	}
}

func TestInvalidOrExcludedSelectionHasNoPackageEffects(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Port: 5433, Extensions: []string{"custom"}},
		{Name: "other", Port: 5434},
	}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {Source: catalog.SourceAPT, Package: "postgresql-18-custom"},
	}
	oldScript := runExtensionPackageScript
	t.Cleanup(func() { runExtensionPackageScript = oldScript })
	runExtensionPackageScript = func(context.Context, string, map[string]string, engine.Runtime, engine.Reporter) error {
		t.Fatal("an invalid selection ran the package script")
		return nil
	}
	if _, _, err := engine.Apply(context.Background(), Catalog(), cfg, engine.Runtime{}, engine.Options{DB: "nope"}); err == nil {
		t.Fatal("invalid db was accepted")
	}
	if _, _, err := engine.Apply(context.Background(), Catalog(), cfg, engine.Runtime{}, engine.Options{Only: []string{"nope-v1"}}); err == nil {
		t.Fatal("unknown step was accepted")
	}
	if _, err := engine.Plan(context.Background(), Catalog(), cfg, engine.Runtime{}, engine.Options{Only: []string{"nope-v1"}}); err == nil {
		t.Fatal("unknown step plan was accepted")
	}

	oldView := engine.ClusterView
	engine.ClusterView = func(c config.Cluster, _ string) config.Cluster {
		out := c.Clone()
		out.Clusters = nil
		for _, spec := range c.Clusters {
			if spec.Name != "app" {
				out.Clusters = append(out.Clusters, spec)
			}
		}
		return out
	}
	t.Cleanup(func() { engine.ClusterView = oldView })
	if _, _, err := engine.Apply(context.Background(), Catalog(), cfg, engine.Runtime{}, engine.Options{
		Only: []string{ConfigStepID("app"), ConfigStepID("other")},
	}); err == nil {
		t.Fatal("a mixed valid+excluded selection was accepted")
	}
}

func TestRolesCheckReportsPGQueryErrors(t *testing.T) {
	cfg := rolesPipelineConfig()
	fake := completeRolesPG()
	useFakeRolesPG(t, fake)
	oldQuery := rolesQueryColumn
	t.Cleanup(func() { rolesQueryColumn = oldQuery })
	rolesQueryColumn = func(_ context.Context, _ pg.Local, _ string, sql string) ([]string, error) {
		if strings.Contains(sql, "FROM pg_roles") {
			return nil, errors.New("roles unavailable")
		}
		return fake.query(context.Background(), pg.Local{}, "", sql)
	}
	step := postgresRoles{}.Instance("app")
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "cannot read pg_roles") {
		t.Fatalf("pg_roles error = %+v, %v", res, err)
	}

	rolesQueryColumn = func(_ context.Context, _ pg.Local, _ string, sql string) ([]string, error) {
		if strings.Contains(sql, "FROM pg_extension") {
			return nil, errors.New("extensions unavailable")
		}
		return fake.query(context.Background(), pg.Local{}, "", sql)
	}
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || res.Complete || !strings.Contains(res.Detail, "cannot read extensions") {
		t.Fatalf("pg_extension error = %+v, %v", res, err)
	}
}

func TestRolesDefaultsProtectedNoAutoExplainSQL(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Protected: true}}
	fake := completeRolesPG()
	fake.extensions = []string{"pg_stat_statements"}
	fake.loaded = map[string]bool{"pg_stat_statements": true, "auto_explain": true}
	fake.adminOwner, fake.signalBack, fake.maintain = "f", "f", "f"
	useFakeRolesPG(t, fake)
	step := postgresRoles{}.Instance("app")
	if res, err := step.Check(context.Background(), cfg, engine.Runtime{}); err != nil || !res.Complete {
		t.Fatalf("protected defaults check = %+v, %v", res, err)
	}
	if res, err := step.Apply(context.Background(), cfg, engine.Runtime{}, nil); err != nil || res.Incomplete {
		t.Fatalf("protected defaults apply = %+v, %v", res, err)
	}
	var sql strings.Builder
	for _, exec := range fake.execs {
		sql.WriteString(exec.sql)
	}
	if strings.Contains(sql.String(), `CREATE EXTENSION IF NOT EXISTS "auto_explain"`) {
		t.Fatalf("auto_explain was created:\n%s", sql.String())
	}
	if !strings.Contains(sql.String(), `CREATE EXTENSION IF NOT EXISTS "pg_stat_statements"`) {
		t.Fatalf("default SQL missing:\n%s", sql.String())
	}
}
