//go:build e2etest

package steps

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/secrets"
)

// Stub control files. Tests steer the stubs by creating these.
const (
	stubControlDir = "/etc/eru-e2e"
	stubStateDir   = "/var/lib/eru-e2e"
)

//go:embed scripts/stub-*.sh
var stubScripts embed.FS

// Catalog returns the e2e stub steps.
func Catalog() []engine.Step {
	return []engine.Step{stubFile{}, stubSecret{}, stubSlow{}, stubFail{}, stubRestart{}}
}

// Restarter returns the stub restarter, which writes a marker file.
func Restarter() engine.Restarter { return stubRestarter{} }

func script(name string) string {
	raw, err := stubScripts.ReadFile("scripts/" + name)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func presence(name string) []byte {
	return []byte(fmt.Sprintf("%s=%t", name, exists(filepath.Join(stubControlDir, name))))
}

type incomplete struct{}

func (incomplete) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{}, nil
}

// stubFile writes [e2e] value to a file.
type stubFile struct{}

func (stubFile) ID() string                     { return "stub-file-v1" }
func (stubFile) Describe(config.Cluster) string { return "Write the [e2e] value to " + stubStateDir + "/file" }
func (stubFile) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return []byte(cfg.E2E.Value), nil
}

func (stubFile) Check(_ context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	raw, err := os.ReadFile(filepath.Join(stubStateDir, "file"))
	if err != nil {
		return engine.CheckResult{Detail: "file missing"}, nil
	}
	return engine.CheckResult{Complete: string(raw) == cfg.E2E.Value, Detail: "file content"}, nil
}

func (stubFile) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if cfg.E2E.Value == "" {
		return engine.ApplyResult{}, errors.New("[e2e] value is empty")
	}
	err := rt.Shell.Run(ctx, "stub-file-v1", script("stub-file.sh"), map[string]string{
		"STUB_FILE":  filepath.Join(stubStateDir, "file"),
		"STUB_VALUE": cfg.E2E.Value,
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// stubSecret registers a secret and prints it from a script.
type stubSecret struct{ incomplete }

func (stubSecret) ID() string                     { return "stub-secret-v1" }
func (stubSecret) Describe(config.Cluster) string { return "Echo a registered secret from a script" }

func readSecret(rt engine.Runtime) (string, error) {
	raw, err := os.ReadFile(filepath.Join(stubControlDir, "secret"))
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if len(value) < secrets.MinLength {
		return "", errors.New("stub secret is shorter than the redaction minimum")
	}
	secrets.Add(value)
	rt.Secrets.Add(value)
	return value, nil
}

func (stubSecret) Inputs(_ config.Cluster, rt engine.Runtime) ([]byte, error) {
	value, err := readSecret(rt)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(value))
	return []byte(hex.EncodeToString(sum[:])), nil
}

func (stubSecret) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	value, err := readSecret(rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, "stub-secret-v1", script("stub-secret.sh"), map[string]string{
		"STUB_SECRET":     value,
		"STUB_REGISTERED": strings.Join(secrets.Default.Values(), "\n"),
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// stubSlow sleeps sleep_s seconds while holding the lock.
type stubSlow struct{ incomplete }

func (stubSlow) ID() string                     { return "stub-slow-v1" }
func (stubSlow) Describe(config.Cluster) string { return "Sleep for /etc/eru-e2e/sleep_s seconds" }

func sleepSeconds() (int, error) {
	raw, err := os.ReadFile(filepath.Join(stubControlDir, "sleep_s"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || n < 0 || n > 3600 {
		return 0, fmt.Errorf("sleep_s must be 0-3600")
	}
	return n, nil
}

func (stubSlow) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	n, err := sleepSeconds()
	if err != nil {
		return nil, err
	}
	return []byte("sleep_s=" + strconv.Itoa(n)), nil
}

func (stubSlow) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	n, err := sleepSeconds()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, "stub-slow-v1", script("stub-slow.sh"), map[string]string{"STUB_SLEEP_S": strconv.Itoa(n)}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// stubFail fails while /etc/eru-e2e/fail exists.
type stubFail struct{ incomplete }

func (stubFail) ID() string                     { return "stub-fail-v1" }
func (stubFail) Describe(config.Cluster) string { return "Fail while /etc/eru-e2e/fail exists" }
func (stubFail) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return presence("fail"), nil
}

func (stubFail) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	err := rt.Shell.Run(ctx, "stub-fail-v1", script("stub-fail.sh"), map[string]string{
		"STUB_FAIL_FILE": filepath.Join(stubControlDir, "fail"),
	}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

// stubRestart asks for a restart while /etc/eru-e2e/restart exists.
type stubRestart struct{ incomplete }

func (stubRestart) ID() string                     { return "stub-restart-v1" }
func (stubRestart) Describe(config.Cluster) string { return "Request a restart while /etc/eru-e2e/restart exists" }
func (stubRestart) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return presence("restart"), nil
}

func (stubRestart) Apply(ctx context.Context, _ config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	want := exists(filepath.Join(stubControlDir, "restart"))
	err := rt.Shell.Run(ctx, "stub-restart-v1", script("stub-restart.sh"), map[string]string{
		"STUB_RESTART": strconv.FormatBool(want),
	}, report)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if want {
		return engine.ApplyResult{Restart: engine.RestartRestart, Notes: []string{"restart requested"}}, nil
	}
	return engine.ApplyResult{Restart: engine.RestartNone}, nil
}

type stubRestarter struct{}

func (stubRestarter) Restart(context.Context) error {
	if err := os.MkdirAll(stubStateDir, 0o755); err != nil { //nolint:gosec // G301: test-only state directory.
		return err
	}
	return os.WriteFile(filepath.Join(stubStateDir, "restarted"), []byte("restarted\n"), 0o644) //nolint:gosec // G306: test-only marker.
}
