package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/state"
)

type cpuRuntimeFixture struct {
	id, cluster string
	apply       func() error
}

func (s cpuRuntimeFixture) ID() string {
	if s.cluster == "" {
		return s.id
	}
	return engine.InstanceID(s.id, s.cluster)
}
func (s cpuRuntimeFixture) Instance(cluster string) engine.Step {
	s.cluster = cluster
	return s
}
func (cpuRuntimeFixture) Describe(config.Cluster) string { return "test CPU-only runtime" }
func (s cpuRuntimeFixture) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte("fixture-" + s.ID()), nil
}
func (cpuRuntimeFixture) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Inconclusive: true, Detail: "test CPU facts unknown"}, nil
}
func (cpuRuntimeFixture) Recheck() bool { return true }
func (s cpuRuntimeFixture) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	if s.apply != nil {
		return engine.ApplyResult{Restart: engine.RestartNone}, s.apply()
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, nil
}

type saveFailureStore struct{ state.Store }

func (saveFailureStore) Save([]byte) error { return errors.New("simulated crash before save") }

type fullResizeFixture struct{ cpuRuntimeFixture }

func (s fullResizeFixture) Instance(cluster string) engine.Step {
	s.cluster = cluster
	return s
}

func (s fullResizeFixture) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	spec, _ := cfg.ClusterByName(s.cluster)
	return []byte("fixture-" + s.ID() + "-" + spec.Size), nil
}

func TestCPUOnlyRealEnginePreservesPending(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", false)
	env.Req = request("db.resize", map[string]any{"name": "app", "cpu_mode": "normal", "cpu_quota": "none"})
	env.Req.Confirm = "app/restart"
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
	oldAdmission, oldCatalog, oldView, oldApply := checkCPUOnlyResources, cpuOnlyCatalog, engine.ClusterView, applyResizeStep
	t.Cleanup(func() {
		checkCPUOnlyResources, cpuOnlyCatalog, engine.ClusterView, applyResizeStep = oldAdmission, oldCatalog, oldView, oldApply
	})
	engine.ClusterView, applyResizeStep = nil, engine.Apply
	checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { return nil }
	dir := t.TempDir()
	full := filepath.Join(dir, "10-vaire.conf")
	fullBytes := []byte("MemoryHigh=old\nIOWeight=old\n")
	if err := os.WriteFile(full, fullBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	var commands []string
	runs := 0
	cpu := cpuRuntimeFixture{id: "postgres-cluster-v1", apply: func() error {
		runs++
		commands = append(commands, "systemctl set-property --runtime postgresql@18-app.service CPUWeight=200 CPUQuota=")
		return os.WriteFile(filepath.Join(dir, "20-vaire-cpu.conf"), []byte("CPUWeight=200\nCPUQuota=\n"), 0o600)
	}}
	catalog := []engine.Step{cpu, cpuRuntimeFixture{id: "postgres-config-v1"}}
	cpuOnlyCatalog = func([]engine.Step) ([]engine.Step, error) { return catalog, nil }
	path := cpuJournalPath()
	previousCPU := "postgres-cluster-v1@app"
	pending := []string{"postgres-config-v1@app", "postgres-config-v1@other"}
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{
		previousCPU:              {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(previousCPU, []byte("fixture-"+previousCPU))},
		"postgres-config-v1@app": {Status: engine.StatusCompleted, InputsHash: "pending"},
	}, RestartPending: append([]string(nil), pending...)}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	result, err := handleDBResize(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	got := result.Data.(map[string]any)["apply"].(engine.ApplyData)
	if runs != 1 || !slices.Equal(got.RestartPending, pending[:1]) || !slices.Equal(got.ConfirmTokens, []string{"app/restart"}) ||
		len(result.Warnings) != 1 || result.Warnings[0].Code != "restart_pending" {
		t.Fatalf("runs %d result %+v warnings %+v", runs, got, result.Warnings)
	}
	current, err := engine.LoadJournal(path)
	if err != nil || !slices.Equal(current.RestartPending, pending) {
		t.Fatalf("pending %+v error %v", current.RestartPending, err)
	}
	if len(commands) != 1 || strings.Contains(commands[0], "Memory") || strings.Contains(commands[0], "IOWeight") ||
		strings.Contains(commands[0], "daemon-reload") || strings.Contains(commands[0], " start ") {
		t.Fatal(commands)
	}
	gotFull, err := os.ReadFile(full) //nolint:gosec // Test-owned temporary file.
	if err != nil || !reflect.DeepEqual(gotFull, fullBytes) {
		t.Fatal("full resource file changed", err)
	}
	saved, _, err := store.Load()
	if err != nil || saved.Clusters[0].CPUMode != "normal" || saved.Clusters[0].CPUQuota != "" {
		t.Fatal(saved.Clusters, err)
	}
}

func TestCPUOnlyAdmissionFailureNoWrites(t *testing.T) {
	for _, reason := range []string{"non_cpu_controls_unknown", "non_cpu_controls_mismatch"} {
		t.Run(reason, func(t *testing.T) { testCPUOnlyAdmissionFailure(t, reason) })
	}
}

func testCPUOnlyAdmissionFailure(t *testing.T, reason string) {
	t.Helper()
	cfg := followerLifecycleCfg(t)
	env, store := resizeFixture(t, cfg, "", false)
	env.Req = request("db.resize", map[string]any{"name": "app", "cpu_quota": "none"})
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
	oldAdmission, oldInvalidate, oldApply := checkCPUOnlyResources, invalidateCPUCompletion, applyResizeStep
	t.Cleanup(func() {
		checkCPUOnlyResources, invalidateCPUCompletion, applyResizeStep = oldAdmission, oldInvalidate, oldApply
	})
	checkCPUOnlyResources = func(context.Context, config.Cluster, string) error {
		return output.NewError(output.CodePrecondition, "non-CPU controls unavailable", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": reason})
	}
	invalidates, applies := 0, 0
	invalidateCPUCompletion = func(string, string) error { invalidates++; return nil }
	applyResizeStep = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		applies++
		return engine.ApplyData{}, nil, nil
	}
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleDBResize(context.Background(), env); err == nil || reasonOfCPUError(err) != reason {
		t.Fatal("preflight accepted or lost reason", err)
	}
	_, after, err := store.Load()
	if err != nil || !reflect.DeepEqual(before, after) || invalidates != 0 || applies != 0 {
		t.Fatal("mutation before admission", err, invalidates, applies)
	}
}

func reasonOfCPUError(err error) string {
	var cli *output.CLIError
	if errors.As(err, &cli) {
		if details, ok := cli.Details.(map[string]any); ok {
			if reason, ok := details["reason"].(string); ok {
				return reason
			}
		}
	}
	return ""
}

func TestCPUOnlyInvalidationSurvivesSaveFailure(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	cfg.Clusters[0].CPUMode = "normal"
	env, store := resizeFixture(t, cfg, "", false)
	env.Req = request("db.resize", map[string]any{"name": "app", "cpu_mode": "normal", "cpu_quota": "none"})
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
	oldAdmission, oldStore, oldView := checkCPUOnlyResources, followerState, engine.ClusterView
	t.Cleanup(func() { checkCPUOnlyResources, followerState, engine.ClusterView = oldAdmission, oldStore, oldView })
	checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { return nil }
	followerState = func() state.Store { return saveFailureStore{store} }
	engine.ClusterView = nil
	path := cpuJournalPath()
	id := "postgres-cluster-v1@app"
	pending := []string{"postgres-config-v1@app"}
	stepRuns := 0
	step := cpuRuntimeFixture{id: "postgres-cluster-v1", apply: func() error { stepRuns++; return nil }}
	hash := engine.InputsHash(id, []byte("fixture-"+id))
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{id: {Status: engine.StatusCompleted, InputsHash: hash}}, RestartPending: pending}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handleDBResize(context.Background(), env); err == nil || !strings.Contains(err.Error(), "simulated crash") {
		t.Fatal(err)
	}
	_, after, err := store.Load()
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("failed save changed config", err)
	}
	got, err := engine.LoadJournal(path)
	if err != nil || !slices.Equal(got.RestartPending, pending) {
		t.Fatal(got, err)
	}
	if _, exists := got.Steps[id]; exists {
		t.Fatal("completed proof survived the save gap")
	}
	data, _, err := engine.Apply(context.Background(), []engine.Step{step}, cfg, engine.Runtime{}, engine.Options{JournalPath: path, LeaveRestartsPending: true})
	if err != nil || data.Applied != 1 || stepRuns != 1 {
		t.Fatalf("retry incorrectly skipped: %+v %v", data, err)
	}
}

func TestCPUOnlySameValueDryRunIsForcedAndReadOnly(t *testing.T) {
	cfg := followerLifecycleCfg(t)
	cfg.Clusters[0].CPUMode = "normal"
	env, store := resizeFixture(t, cfg, "", true)
	env.Req = request("db.resize", map[string]any{"name": "app", "cpu_mode": "normal", "cpu_quota": "none"})
	env.Req.DryRun = true
	fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
	oldAdmission, oldView, oldInvalidate := checkCPUOnlyResources, engine.ClusterView, invalidateCPUCompletion
	t.Cleanup(func() {
		checkCPUOnlyResources, engine.ClusterView, invalidateCPUCompletion = oldAdmission, oldView, oldInvalidate
	})
	checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { return nil }
	engine.ClusterView = nil
	invalidates := 0
	invalidateCPUCompletion = func(string, string) error { invalidates++; return nil }
	_, before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	result, err := handleDBResize(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	plan := result.Data.(map[string]any)["plan"].(engine.PlanData)
	_, after, err := store.Load()
	if err != nil || !reflect.DeepEqual(before, after) || invalidates != 0 || plan.WouldApply != 1 ||
		len(plan.Steps) != 1 || plan.Steps[0].Reason != "cpu_only_explicit" {
		t.Fatal(plan, invalidates, err)
	}
}

func TestFullResizeSameCPUPreviewMatchesExecution(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields map[string]any
		size   string
		forced bool
	}{
		{"mode", map[string]any{"cpu_mode": "normal"}, "large", true},
		{"quota", map[string]any{"cpu_quota": "none"}, "large", true},
		{"both", map[string]any{"cpu_mode": "normal", "cpu_quota": "none"}, "large", true},
		{"size-only", nil, "large", false},
		{"changed-size", nil, "medium", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t)
			cfg.Clusters[0].CPUMode = "normal"
			env, store := resizeFixture(t, cfg, "", true)
			fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
				hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, true)
			params := map[string]any{"name": "app", "size": tc.size}
			for key, value := range tc.fields {
				params[key] = value
			}
			env.Req = request("db.resize", params)
			env.Req.DryRun = true
			oldCatalog, oldApply, oldView, oldGuard := resizeCatalog, applyResizeStep, engine.ClusterView, engine.GuardView
			t.Cleanup(func() {
				resizeCatalog, applyResizeStep, engine.ClusterView, engine.GuardView = oldCatalog, oldApply, oldView, oldGuard
			})
			engine.ClusterView, engine.GuardView = nil, nil
			runs := 0
			step := fullResizeFixture{cpuRuntimeFixture{id: "postgres-cluster-v1", apply: func() error { runs++; return nil }}}
			catalog := []engine.Step{step}
			resizeCatalog = func() []engine.Step { return catalog }
			id := "postgres-cluster-v1@app"
			path := cpuJournalPath()
			journal := &engine.Journal{Steps: map[string]engine.JournalEntry{
				id: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(id, []byte("fixture-"+id+"-"+cfg.Clusters[0].Size))},
			}, RestartPending: []string{"postgres-config-v1@app"}}
			if err := journal.Save(path); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path) //nolint:gosec // Test-owned journal.
			if err != nil {
				t.Fatal(err)
			}
			loadedBefore, configBefore, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			preview, err := handleDBResize(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			plan := preview.Data.(map[string]any)["plan"].(engine.PlanData)
			want := "skip"
			if tc.forced {
				want = "apply"
			}
			if len(plan.Steps) != 1 || plan.Steps[0].Action != want {
				t.Fatalf("preview = %+v, want %s", plan, want)
			}
			after, err := os.ReadFile(path) //nolint:gosec // Test-owned journal.
			_, configAfter, loadErr := store.Load()
			if err != nil || loadErr != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(configBefore, configAfter) {
				t.Fatalf("preview changed journal or configuration: %v %v", err, loadErr)
			}
			applyResizeStep = func(ctx context.Context, _ []engine.Step, cfg config.Cluster, rt engine.Runtime, opts engine.Options) (engine.ApplyData, []output.Warning, error) {
				if opts.DB == "" {
					return engine.ApplyData{}, nil, nil
				}
				opts.JournalPath, opts.Restarter = path, nil
				return engine.Apply(ctx, catalog, cfg, rt, opts)
			}
			env.Req.DryRun = false
			result, err := handleDBResize(context.Background(), env)
			if err != nil {
				t.Fatal(err)
			}
			applied := result.Data.(map[string]any)["apply"].(engine.ApplyData)
			if len(applied.Steps) != 1 || (runs == 1) != tc.forced {
				t.Fatalf("execution runs=%d result=%+v", runs, applied)
			}
			saved, _, err := store.Load()
			expected := loadedBefore.Clusters[0]
			expected.Size = tc.size
			if err != nil || !reflect.DeepEqual(saved.Clusters[0], expected) {
				t.Fatalf("unrelated fields or size changed: %+v %v", saved.Clusters, err)
			}
			current, err := engine.LoadJournal(path)
			if err != nil || !slices.Equal(current.RestartPending, journal.RestartPending) {
				t.Fatalf("pending restarts changed: %+v %v", current, err)
			}
		})
	}
}

func TestConfigCPUProofInvalidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		full bool
		dry  bool
	}{{"same-dotted", false, false}, {"changed-full", true, false}, {"same-dry", false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := followerLifecycleCfg(t)
			cfg.Clusters[0].CPUMode, cfg.Clusters[0].CPUQuota = "normal", "150%"
			env, store := resizeFixture(t, cfg, "", tc.dry)
			fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
			oldStore, oldSchedules, oldHostname, oldPlan := configSetStore, configSetSchedules, configSetHostname, configSetPlan
			t.Cleanup(func() {
				configSetStore, configSetSchedules, configSetHostname, configSetPlan = oldStore, oldSchedules, oldHostname, oldPlan
			})
			configSetStore = func() state.FileStore { return store }
			configSetSchedules = func(context.Context, config.Cluster) error { return nil }
			configSetHostname = func(config.Cluster) error { return nil }
			configSetPlan = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.PlanData, error) {
				return engine.PlanData{}, nil
			}
			id := "postgres-cluster-v1@app"
			pending := []string{"postgres-config-v1@app"}
			j := &engine.Journal{Steps: map[string]engine.JournalEntry{id: {Status: engine.StatusCompleted, InputsHash: "prior"}}, RestartPending: pending}
			path := cpuJournalPath()
			if err := j.Save(path); err != nil {
				t.Fatal(err)
			}
			revision, err := store.Revision()
			if err != nil {
				t.Fatal(err)
			}
			_, before, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			params := map[string]any{"base_revision": revision}
			if tc.full {
				cfg.Clusters[0].CPUQuota = ""
				raw, err := cfg.Encode()
				if err != nil {
					t.Fatal(err)
				}
				params["toml"] = string(raw)
			} else {
				params["set"] = map[string]string{"clusters.app.cpu_quota": "150%"}
			}
			env.Req = request("config.set", params)
			env.Req.DryRun = tc.dry
			if _, err := handleConfigSet(context.Background(), env); err != nil {
				t.Fatal(err)
			}
			after, err := engine.LoadJournal(path)
			if err != nil || !slices.Equal(after.RestartPending, pending) {
				t.Fatal(after, err)
			}
			_, exists := after.Steps[id]
			if exists != tc.dry {
				t.Fatalf("proof exists=%v for dry=%v", exists, tc.dry)
			}
			if tc.dry {
				_, raw, err := store.Load()
				if err != nil || !reflect.DeepEqual(before, raw) {
					t.Fatal("dry run saved config", err)
				}
			}
		})
	}
}

func TestCPUInvalidExplicitValuesRefusedBeforeCallbacks(t *testing.T) {
	for _, fields := range []map[string]any{
		{"cpu_mode": nil}, {"cpu_quota": nil}, {"cpu_mode": ""}, {"cpu_mode": "idle"},
		{"cpu_quota": ""}, {"cpu_quota": "0%"}, {"cpu_quota": "-1%"}, {"cpu_quota": "NaN%"},
	} {
		t.Run(fmt.Sprint(fields), func(t *testing.T) {
			cfg := followerLifecycleCfg(t)
			env, _ := resizeFixture(t, cfg, "", false)
			fixedResizeHost(t, hostinfo.Info{VCPU: 4, RAMMiB: 8192}, hostinfo.Memory{}, true)
			fields["name"] = "app"
			env.Req = request("db.resize", fields)
			oldAdmission, oldInvalidate, oldApply, oldSpare := checkCPUOnlyResources, invalidateCPUCompletion, applyResizeStep, checkSpareCPU
			t.Cleanup(func() {
				checkCPUOnlyResources, invalidateCPUCompletion, applyResizeStep, checkSpareCPU = oldAdmission, oldInvalidate, oldApply, oldSpare
			})
			calls := 0
			checkCPUOnlyResources = func(context.Context, config.Cluster, string) error { calls++; return nil }
			checkSpareCPU = func(context.Context, string, string) error { calls++; return nil }
			invalidateCPUCompletion = func(string, string) error { calls++; return nil }
			applyResizeStep = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
				calls++
				return engine.ApplyData{}, nil, nil
			}
			if _, err := handleDBResize(context.Background(), env); err == nil || calls != 0 {
				t.Fatal("invalid field accepted or callback invoked", err, calls)
			}
		})
	}
}

func TestFollowCPUInvalidValuesBeforeProbes(t *testing.T) {
	for _, fields := range []map[string]any{
		{"cpu_mode": nil}, {"cpu_quota": nil}, {"cpu_mode": ""}, {"cpu_mode": "idle"},
		{"cpu_quota": ""}, {"cpu_quota": "0%"}, {"cpu_quota": "-1%"}, {"cpu_quota": "NaN%"},
	} {
		t.Run(fmt.Sprint(fields), func(t *testing.T) {
			cfg := followerLifecycleCfg(t)
			oldRunning, oldExists, oldCheck := clusterRunning, followClusterExists, checkSpareCPU
			t.Cleanup(func() { clusterRunning, followClusterExists, checkSpareCPU = oldRunning, oldExists, oldCheck })
			probes := 0
			clusterRunning = func(config.ClusterSpec) bool { probes++; return true }
			followClusterExists = func(context.Context, string) bool { probes++; return false }
			checkSpareCPU = func(context.Context, string, string) error { probes++; return nil }
			fields["source"], fields["name"] = "app", "app_follower"
			req := request("db.follow", fields)
			if _, err := handleDBFollow(context.Background(), &Env{Req: req, Cluster: cfg}); err == nil || probes != 0 {
				t.Fatal("invalid field accepted or probe invoked", err, probes)
			}
		})
	}
}
