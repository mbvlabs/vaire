package steps

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestApplyCPUChecked(t *testing.T) {
	ctx := context.Background()
	res := config.Resources{CPUMode: "spare", CPUWeight: 73, CPUQuota: "150%"}
	screenFact := hostinfo.CPUScreen{Capability: hostinfo.CPUCapable}
	screen := func(_ context.Context, unit string) hostinfo.CPUScreen {
		if unit != "postgresql@18-app" {
			t.Fatal(unit)
		}
		return screenFact
	}
	idle := true
	weight := uint64(73)
	quota := hostinfo.CPUQuota{QuotaUsec: 150000, PeriodUsec: 100000}
	facts := hostinfo.CPUObservation{Screen: screenFact, Idle: &idle, Weight: &weight, Quota: &quota}
	observe := func(context.Context, string) hostinfo.CPUObservation { return facts }
	runs := 0
	run := func() error { runs++; return nil }
	for _, tc := range []struct {
		capability hostinfo.Capability
		reason     string
	}{{hostinfo.CPUUnsupported, "spare_cpu_unsupported"}, {hostinfo.CPUUnknown, "spare_cpu_capability_unknown"}} {
		screenFact.Capability = tc.capability
		err := applyCPUChecked(ctx, "app", res, screen, run, observe)
		var cli *output.CLIError
		if !errors.As(err, &cli) || cli.Details.(map[string]any)["reason"] != tc.reason || runs != 0 {
			t.Fatalf("%v, runs %d", err, runs)
		}
	}
	screenFact.Capability = hostinfo.CPUCapable
	if err := applyCPUChecked(ctx, "app", res, screen, run, observe); err != nil || runs != 1 {
		t.Fatalf("%v, runs %d", err, runs)
	}
	// A native mismatch after successful script execution is a step failure.
	idle = false
	err := applyCPUChecked(ctx, "app", res, screen, run, observe)
	var cli *output.CLIError
	if !errors.As(err, &cli) || cli.ExitCode != output.ExitStep || cli.Details.(map[string]any)["reason"] != "cpu_policy_not_enforced" {
		t.Fatal(err)
	}
	res.CPUMode = "normal"
	if err := applyCPUChecked(ctx, "app", res, screen, run, observe); err != nil {
		t.Fatal(err)
	}
	weight = 72
	if err := applyCPUChecked(ctx, "app", res, screen, run, observe); err == nil {
		t.Fatal("normal weight mismatch accepted")
	}
	facts.Idle = nil
	if err := applyCPUChecked(ctx, "app", res, screen, run, observe); !errors.As(err, &cli) || cli.Details.(map[string]any)["effective_known"] != false {
		t.Fatal(err)
	}
	failed := func() error {
		return &engine.ScriptError{Tail: []string{"vaire_cpu_policy_set_failed"}}
	}
	if err := applyCPUChecked(ctx, "app", res, screen, failed, observe); !errors.As(err, &cli) ||
		cli.Details.(map[string]any)["mismatch"] != "runtime_property_update_failed" {
		t.Fatal(err)
	}
	startErr := &engine.ScriptError{Status: 1, Tail: []string{"vaire_cpu_start_failed"}}
	err = applyCPUChecked(ctx, "app", res, screen, func() error { return startErr }, observe)
	if !errors.As(err, &cli) || cli.Details.(map[string]any)["mismatch"] != "startup_cpu_attribute_failed" || !errors.Is(err, startErr) {
		t.Fatal(err)
	}
	unrelated := &engine.ScriptError{Status: 1, Tail: []string{"postgres failed to start"}}
	if err := applyCPUChecked(ctx, "app", res, screen, func() error { return unrelated }, observe); !errors.Is(err, unrelated) {
		t.Fatalf("unrelated start error changed: %v", err)
	}
}

func TestCPUInactiveStartDiagnostic(t *testing.T) {
	dir := t.TempDir()
	commands := map[string]string{
		"pg_lsclusters": "#!/bin/sh\nprintf '18 app 5433 down postgres /data/app /log\\n'\n",
		"systemctl":     "#!/bin/sh\ncase \"$1\" in is-active|start) exit 1;; *) exit 0;; esac\n",
		"journalctl":    "#!/bin/sh\ncase \"$*\" in *--show-cursor*) echo '-- cursor: s=123;i=456';; *) if [ \"$CPU_FAILURE\" = yes ]; then echo \"Failed to set 'cpu.idle' attribute on '/system.slice/example' to '1': Operation not permitted\"; else echo 'PostgreSQL startup failed'; fi;; esac\n",
		"timeout":       "#!/bin/sh\nshift\nexec \"$@\"\n",
	}
	for name, script := range commands {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil { //nolint:gosec // Executable test-only command mocks in a private temp directory.
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name, failure string
		marker        bool
	}{{"cpu", "yes", true}, {"postgres", "no", false}} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/bash", "-s") //nolint:gosec // fixed interpreter, mocked commands.
			cmd.Stdin = strings.NewReader(Script("postgres-cluster"))
			cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "CLUSTER=app", "PORT=5433", "DATA_DIR=/data/app",
				"LOCALE_PROVIDER=builtin", "LOCALE=C", "MEMORY_HIGH=640M", "MEMORY_MAX=infinity",
				"CPU_WEIGHT=idle", "CPU_QUOTA=150%", "IO_WEIGHT=100", "CPU_FAILURE=" + tc.failure}
			out, err := cmd.CombinedOutput()
			if err == nil || strings.Contains(string(out), "vaire_cpu_start_failed") != tc.marker {
				t.Fatalf("output %q error %v", out, err)
			}
		})
	}
}

func TestCPUClusterScriptUsesOneRuntimeUpdate(t *testing.T) {
	script := Script("postgres-cluster")
	if !strings.Contains(script, `"CPUWeight=${CPU_WEIGHT}" "CPUQuota=${CPU_QUOTA}"`) ||
		!strings.Contains(script, "set -euo pipefail") ||
		!strings.Contains(script, "systemctl set-property --runtime") {
		t.Fatal("runtime CPU properties are not applied together")
	}
	res := config.Resources{CPUMode: "spare", CPUWeight: 73, CPUQuota: "150%"}
	env := clusterScriptEnv(config.Cluster{}, config.ClusterSpec{Name: "app"}, res)
	if env["CPU_WEIGHT"] != "idle" || env["CPU_QUOTA"] != "150%" {
		t.Fatal(env)
	}
	res.CPUMode = "normal"
	res.CPUQuota = ""
	env = clusterScriptEnv(config.Cluster{}, config.ClusterSpec{Name: "app"}, res)
	if env["CPU_WEIGHT"] != "73" || env["CPU_QUOTA"] != "" {
		t.Fatal(env)
	}
}
