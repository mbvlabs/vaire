//go:build e2e

package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Envelope is the JSON body of an eru invocation, plus the process result.
type Envelope struct {
	Schema  string          `json:"schema"`
	OK      bool            `json:"ok"`
	Command string          `json:"command"`
	Message string          `json:"message,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`
	Next    []string        `json:"next,omitempty"`
	Error   *ErrorBody      `json:"error,omitempty"`
	Stdout  string          `json:"-"`
	Stderr  string          `json:"-"`
	Exit    int             `json:"-"`
}

// ErrorBody is the failure object inside an envelope.
type ErrorBody struct {
	Code    string          `json:"code"`
	Message string          `json:"message"`
	Hint    string          `json:"hint,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

type buildResult struct {
	path string
	err  error
}

var (
	buildMu sync.Mutex
	builds  = map[string]buildResult{}
)

// Binary returns the operator binary under test: ERU_E2E_BINARY when set,
// otherwise a host build in dist/e2e. It never touches bin/eru.
func Binary(t *testing.T) string {
	t.Helper()
	if path := os.Getenv("ERU_E2E_BINARY"); path != "" {
		return absFromRoot(t, path)
	}
	return build(t, "", "")
}

// NodeBinary returns the linux binary to push onto vm: ERU_NODE_BINARY when
// set, otherwise a build for the VM's architecture.
func NodeBinary(t *testing.T, vm *VM) string {
	t.Helper()
	if path := os.Getenv("ERU_NODE_BINARY"); path != "" {
		return absFromRoot(t, path)
	}
	out, err := vm.Exec("uname", "-m")
	if err != nil {
		t.Fatal(err)
	}
	var arch string
	switch machine := strings.TrimSpace(out); machine {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	default:
		t.Fatalf("unsupported VM architecture %q", machine)
	}
	return build(t, "linux", arch)
}

// build compiles ./cmd/eru once per target with the justfile's dev ldflags.
// Empty goos and goarch mean the host.
func build(t *testing.T, goos, goarch string) string {
	t.Helper()
	key := goos + "/" + goarch
	buildMu.Lock()
	defer buildMu.Unlock()
	if result, ok := builds[key]; ok {
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.path
	}
	root := moduleRoot(t)
	dir := filepath.Join(root, "dist", "e2e", "host")
	if goos != "" {
		dir = filepath.Join(root, "dist", "e2e", goos+"-"+goarch)
	}
	result := buildResult{path: filepath.Join(dir, "eru")}
	result.err = func() error {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		commit, err := exec.Command("git", "-C", root, "rev-parse", "--short=12", "HEAD").Output() //nolint:gosec // G204: fixed git query on this module.
		if err != nil {
			return fmt.Errorf("read commit: %w", err)
		}
		pkg := "github.com/mbvlabs/eru/internal/buildinfo"
		ldflags := "-s -w -X " + pkg + ".Version=dev -X " + pkg + ".Commit=" + strings.TrimSpace(string(commit)) + " -X " + pkg + ".Channel=dev"
		cmd := exec.Command("go", "build", "-trimpath", "-ldflags", ldflags, "-o", result.path, "./cmd/eru") //nolint:gosec // G204: fixed build of this module.
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if goos != "" {
			cmd.Env = append(cmd.Env, "GOOS="+goos, "GOARCH="+goarch)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build eru %s: %w\n%s", key, err, out)
		}
		return nil
	}()
	builds[key] = result
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.path
}

func absFromRoot(t *testing.T, path string) string {
	t.Helper()
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(moduleRoot(t), path)
}

// Run executes the eru binary and returns its streams and exit status.
func Run(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(Binary(t), args...) //nolint:gosec // G204: tests invoke the eru binary under test.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), stderr.String(), 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("run eru %v: %v", args, err)
	}
	return stdout.String(), stderr.String(), exitErr.ExitCode()
}

// Eru runs eru with --output json and checks the envelope schema.
func Eru(t *testing.T, args ...string) Envelope {
	t.Helper()
	full := append([]string{}, args...)
	if !hasOutputMode(full) {
		full = append(full, "--output", "json")
	}
	stdout, stderr, code := Run(t, full...)
	var env Envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("eru %v exit %d: invalid JSON: %v\nstdout: %s\nstderr: %s", args, code, err, stdout, stderr)
	}
	if env.Schema != "eru/v1" {
		t.Fatalf("eru %v: schema %q\n%s", args, env.Schema, stdout)
	}
	env.Stdout = stdout
	env.Stderr = stderr
	env.Exit = code
	return env
}

func hasOutputMode(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "--output" || strings.HasPrefix(arg, "--output=") {
			return true
		}
	}
	return false
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
