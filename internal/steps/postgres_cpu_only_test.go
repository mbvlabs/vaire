package steps

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/render"
)

func TestCPUOnlyOverlayAndCommand(t *testing.T) {
	dir := t.TempDir()
	base := []byte("# managed by vaire - do not edit; use cluster.toml\n[Service]\nMemoryHigh=640M\nIOWeight=100\n")
	if err := os.WriteFile(filepath.Join(dir, "10-vaire.conf"), base, 0o600); err != nil {
		t.Fatal(err)
	}
	res := config.Resources{CPUMode: "spare", CPUWeight: 73, CPUQuota: "150%"}
	if err := writeCPUOverlay(dir, render.PGCPUOnlyDropin(res)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "10-vaire.conf")) //nolint:gosec // Test-owned temporary file.
	if err != nil || !bytes.Equal(raw, base) {
		t.Fatal("full drop-in changed", err)
	}
	exists, err := cpuOverlayExists(dir)
	if err != nil || !exists {
		t.Fatal(exists, err)
	}
	res.CPUMode, res.CPUQuota = "normal", ""
	if err := writeCPUOverlay(dir, render.PGCPUOnlyDropin(res)); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, cpuOverlayName)) //nolint:gosec // Test-owned temporary file.
	if err != nil || !strings.Contains(string(raw), "CPUWeight=73\nCPUQuota=\n") || strings.Contains(string(raw), "Memory") {
		t.Fatal(string(raw), err)
	}
	cmd := cpuOnlyCommand(config.ClusterSpec{Name: "app"}, res)
	if cmd.Name != "systemctl" || !slices.Equal(cmd.Args, []string{"set-property", "--runtime", "postgresql@18-app.service", "CPUWeight=73", "CPUQuota="}) {
		t.Fatal(cmd)
	}
	wrote := false
	if err := prepareFullCPUDropin(dir, func() error {
		wrote = true
		if exists, err := cpuOverlayExists(dir); err != nil || !exists {
			t.Fatal("overlay disappeared before full write", err)
		}
		return nil
	}); err != nil || !wrote {
		t.Fatal(err)
	}
	if exists, err := cpuOverlayExists(dir); err != nil || exists {
		t.Fatal(exists, err)
	}
	if err := os.WriteFile(filepath.Join(dir, cpuOverlayName), []byte("foreign"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeCPUOverlay(dir); err == nil {
		t.Fatal("foreign file was accepted")
	}
	if err := writeCPUOverlay(dir, render.PGCPUOnlyDropin(res)); err == nil {
		t.Fatal("foreign file was overwritten")
	}
}

func TestCPUOverlayInterruptedTempDoesNotBlockUpdate(t *testing.T) {
	dir := t.TempDir()
	base := []byte("# managed by vaire - do not edit; use cluster.toml\n[Service]\nMemoryHigh=640M\n")
	oldName := ".20-vaire-cpu.conf.tmp"
	oldRandom := ".20-vaire-cpu.conf.abcdef0123456789abcdef01"
	for name, content := range map[string][]byte{
		"10-vaire.conf": base,
		oldName:         []byte("old interrupted write"),
		oldRandom:       []byte("other interrupted write"),
	} {
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	content := render.PGCPUOnlyDropin(config.Resources{CPUMode: "spare", CPUWeight: 73, CPUQuota: "150%"})
	if err := writeCPUOverlay(dir, content); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{
		"10-vaire.conf": base,
		oldName:         []byte("old interrupted write"),
		oldRandom:       []byte("other interrupted write"),
		cpuOverlayName:  content,
	} {
		got, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // Test-owned temporary file.
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s: %q, %v", name, got, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 4 {
		t.Fatalf("unexpected files %v, %v", entries, err)
	}
}

func TestCPUOverlayOwnTempCleanupAndCollision(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	content := render.PGCPUOnlyDropin(config.Resources{CPUMode: "normal", CPUWeight: 73})
	suffix := bytes.Repeat([]byte{7}, 12)
	name := ".20-vaire-cpu.conf.070707070707070707070707"
	renameErr := errors.New("simulated rename failure")
	err = writeCPUOverlayAtomic(root, content, bytes.NewReader(suffix), func(r *os.Root, source, target string) error {
		if source != name || target != cpuOverlayName {
			t.Fatalf("unexpected rename %q -> %q", source, target)
		}
		if _, err := r.Stat(source); err != nil {
			t.Fatal(err)
		}
		return renameErr
	})
	if !errors.Is(err, renameErr) {
		t.Fatal(err)
	}
	if _, err := root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("own failed temporary file remains", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("foreign residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = writeCPUOverlayAtomic(root, content, bytes.NewReader(suffix), (*os.Root).Rename)
	if !errors.Is(err, os.ErrExist) {
		t.Fatal("collision did not fail closed", err)
	}
	got, err := root.ReadFile(name)
	if err != nil || string(got) != "foreign residue" {
		t.Fatalf("foreign residue changed: %q %v", got, err)
	}
	replacementSuffix := bytes.Repeat([]byte{9}, 12)
	replacement := ".20-vaire-cpu.conf.090909090909090909090909"
	err = writeCPUOverlayAtomic(root, content, bytes.NewReader(replacementSuffix), func(r *os.Root, source, target string) error {
		if err := r.Rename(source, target); err != nil {
			return err
		}
		f, err := r.OpenFile(source, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := f.WriteString("new foreign file")
		return errors.Join(writeErr, f.Close())
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = root.ReadFile(replacement)
	if err != nil || string(got) != "new foreign file" {
		t.Fatalf("replacement was removed: %q %v", got, err)
	}
}

type cpuObservedStep struct {
	result  engine.CheckResult
	applied *bool
}

func (cpuObservedStep) ID() string                     { return "cpuprobe-v1" }
func (cpuObservedStep) Describe(config.Cluster) string { return "CPU probe" }
func (cpuObservedStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte("same"), nil
}
func (cpuObservedStep) Recheck() bool { return true }
func (s cpuObservedStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return s.result, nil
}
func (s cpuObservedStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	*s.applied = true
	return engine.ApplyResult{}, nil
}

func TestCPUUnknownCompletedEngineSkipsWithoutMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.json")
	id := "cpuprobe-v1"
	j := &engine.Journal{Steps: map[string]engine.JournalEntry{id: {Status: engine.StatusCompleted, InputsHash: engine.InputsHash(id, []byte("same"))}}}
	if err := j.Save(path); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // Test-owned journal.
	if err != nil {
		t.Fatal(err)
	}
	applied := false
	step := cpuObservedStep{result: checkCPUObserved("app", config.Resources{CPUMode: "normal", CPUWeight: 100}, hostinfo.CPUObservation{}), applied: &applied}
	data, warnings, err := engine.Apply(context.Background(), []engine.Step{step}, config.Cluster{}, engine.Runtime{}, engine.Options{JournalPath: path})
	after, readErr := os.ReadFile(path) //nolint:gosec // Test-owned journal.
	if err != nil || readErr != nil || applied || !bytes.Equal(before, after) || data.Skipped != 1 || len(warnings) != 1 {
		t.Fatal(data, warnings, err, readErr)
	}
}

func TestCPUOnlyAdmissionLoadedVersusActual(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Size: "small"}}
	line := "18 app 5433 online postgres " + cfg.Server.DataDirectory("app") + " /log\n"
	probe := func(context.Context, string, ...string) (string, error) { return line, nil }
	controls := hostinfo.NonCPUControls{MemoryHigh: hostinfo.ControlLimit{Bytes: 123}, MemoryMax: hostinfo.ControlLimit{Unlimited: true}, IOWeight: 100}
	facts := hostinfo.NonCPUObservation{Known: true, Loaded: controls, Actual: controls}
	observe := func(context.Context, string) hostinfo.NonCPUObservation { return facts }
	screen := func(context.Context, string) hostinfo.CPUScreen {
		return hostinfo.CPUScreen{Capability: hostinfo.CPUCapable}
	}
	target := func(string) error { return nil }
	// The loaded/actual values deliberately differ from saved size-derived
	// memory. That is not a reason to mutate the full resource drop-in.
	if _, err := cpuOnlyAdmission(context.Background(), cfg, "app", probe, observe, screen, target); err != nil {
		t.Fatal(err)
	}
	facts.Actual.MemoryHigh.Bytes++
	if _, err := cpuOnlyAdmission(context.Background(), cfg, "app", probe, observe, screen, target); err == nil {
		t.Fatal("mismatch accepted")
	}
	facts.Known = false
	if _, err := cpuOnlyAdmission(context.Background(), cfg, "app", probe, observe, screen, target); err == nil {
		t.Fatal("unknown accepted")
	}
	line = "18 app 5433 down postgres " + cfg.Server.DataDirectory("app") + " /log\n"
	if _, err := cpuOnlyAdmission(context.Background(), cfg, "app", probe, observe, screen, target); err == nil {
		t.Fatal("stopped service accepted")
	}
}

func TestCPUOnlyPreflightAndVerification(t *testing.T) {
	ctx := context.Background()
	res := config.Resources{CPUMode: "normal", CPUWeight: 73}
	spec := config.ClusterSpec{Name: "app"}
	idle, weight := false, uint64(73)
	quota := hostinfo.CPUQuota{Unlimited: true, PeriodUsec: 100000}
	cpu := hostinfo.CPUObservation{Screen: hostinfo.CPUScreen{Capability: hostinfo.CPUCapable}, Idle: &idle, Weight: &weight, Quota: &quota}
	writes, sets := 0, 0
	ops := cpuOnlyOps{
		admit: func(context.Context) (hostinfo.NonCPUObservation, error) {
			return hostinfo.NonCPUObservation{}, errors.New("unavailable")
		},
		write:     func() error { writes++; return nil },
		set:       func(context.Context) error { sets++; return nil },
		nonCPU:    func(context.Context) hostinfo.NonCPUObservation { return hostinfo.NonCPUObservation{} },
		cpu:       func(context.Context) hostinfo.CPUObservation { return cpu },
		unchanged: func(hostinfo.NonCPUObservation, hostinfo.NonCPUObservation) error { return nil },
	}
	if err := runCPUOnly(ctx, config.Cluster{}, spec, res, ops); err == nil || writes != 0 || sets != 0 {
		t.Fatal(err, writes, sets)
	}
	ops.admit = func(context.Context) (hostinfo.NonCPUObservation, error) { return hostinfo.NonCPUObservation{}, nil }
	if err := runCPUOnly(ctx, config.Cluster{}, spec, res, ops); err != nil || writes != 1 || sets != 1 {
		t.Fatal(err, writes, sets)
	}
	cpu.Quota = nil
	if err := runCPUOnly(ctx, config.Cluster{}, spec, res, ops); err == nil {
		t.Fatal("unknown post-apply quota accepted")
	}
	beforeSets := sets
	ops.unchanged = func(hostinfo.NonCPUObservation, hostinfo.NonCPUObservation) error {
		return errors.New("non_cpu_controls_changed")
	}
	if err := runCPUOnly(ctx, config.Cluster{}, spec, res, ops); err == nil || sets != beforeSets {
		t.Fatal("changed pre-runtime controls did not block set-property", err, sets)
	}
	checks := 0
	ops.unchanged = func(hostinfo.NonCPUObservation, hostinfo.NonCPUObservation) error {
		checks++
		if checks == 2 {
			return errors.New("non_cpu_controls_changed")
		}
		return nil
	}
	if err := runCPUOnly(ctx, config.Cluster{}, spec, res, ops); err == nil || sets != beforeSets+1 {
		t.Fatal("changed post-runtime controls were hidden", err, sets)
	}
}

func TestCPUOnlyForcesDistinctInputs(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Port: 5433, Size: "small"}}
	catalog, err := CPUOnlyCatalog([]engine.Step{postgresCluster{}})
	if err != nil {
		t.Fatal(err)
	}
	normal := postgresCluster{clusterStep{cluster: "app"}}
	only := catalog[0].(engine.Template).Instance("app")
	a, err := normal.Inputs(cfg, engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := only.Inputs(cfg, engine.Runtime{})
	if err != nil || bytes.Equal(a, b) || normal.ID() != only.ID() {
		t.Fatal("CPU-only input identity", err)
	}
	if f, ok := only.(engine.Forcer); !ok || f.Force(cfg, engine.Runtime{}) == "" {
		t.Fatal("explicit CPU-only apply is not forced")
	}
	opts := CPUOnlyOptions("app")
	if opts.DB != "app" || !opts.LeaveRestartsPending || !slices.Equal(opts.Only, []string{"postgres-cluster-v1@app"}) {
		t.Fatal(opts)
	}
}

func TestCPUCheckKnownDriftOutranksUnknown(t *testing.T) {
	idle := false
	res := config.Resources{CPUMode: "spare", CPUWeight: 100}
	got := checkCPUObserved("app", res, hostinfo.CPUObservation{Idle: &idle})
	if got.Inconclusive || got.Complete {
		t.Fatal(got)
	}
	got = checkCPUObserved("app", res, hostinfo.CPUObservation{})
	if !got.Inconclusive || got.Complete {
		t.Fatal(got)
	}
	got = checkCPUWithOverlay("app", res, true, hostinfo.CPUObservation{})
	if got.Inconclusive || got.Complete || !strings.Contains(got.Detail, "overlay") {
		t.Fatal(got)
	}
}
