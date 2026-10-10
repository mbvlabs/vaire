//go:build e2e

package harness

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// NodeBinaryPath is where the node binary lives in the VM.
const NodeBinaryPath = "/usr/local/bin/eru"

// Node is a VM prepared as an eru node with the e2etest stub catalogue.
type Node struct {
	VM     *VM
	Binary string // host path of the pushed build
	Commit string // buildinfo.Commit of the pushed build
	Secret string // content of /etc/eru-e2e/secret
}

// HeadCommit returns the short commit the justfile stamps into dev builds.
func HeadCommit(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("git", "-C", moduleRoot(t), "rev-parse", "--short=12", "HEAD").Output() //nolint:gosec // G204: fixed git query on this module.
	if err != nil {
		t.Fatalf("read commit: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// NodeBuild builds an e2etest linux binary for vm's architecture. The ldflags
// match the justfile's dev build except Commit, which is set to commit.
func NodeBuild(t *testing.T, vm *VM, commit string) string {
	t.Helper()
	arch := vmGoArch(t, vm)
	key := "e2etest/linux/" + arch + "/" + commit
	buildMu.Lock()
	defer buildMu.Unlock()
	if result, ok := builds[key]; ok {
		if result.err != nil {
			t.Fatal(result.err)
		}
		return result.path
	}
	root := moduleRoot(t)
	dir := filepath.Join(root, "dist", "e2e", "node-linux-"+arch+"-"+commit)
	result := buildResult{path: filepath.Join(dir, "eru")}
	result.err = func() error {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		pkg := "github.com/mbvlabs/eru/internal/buildinfo"
		ldflags := "-s -w -X " + pkg + ".Version=dev -X " + pkg + ".Commit=" + commit + " -X " + pkg + ".Channel=dev"
		cmd := exec.Command("go", "build", "-tags", "e2etest", "-trimpath", "-ldflags", ldflags, "-o", result.path, "./cmd/eru") //nolint:gosec // G204: fixed build of this module.
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build e2etest node %s: %w\n%s", key, err, out)
		}
		return nil
	}()
	builds[key] = result
	if result.err != nil {
		t.Fatal(result.err)
	}
	return result.path
}

func vmGoArch(t *testing.T, vm *VM) string {
	t.Helper()
	out, err := vm.Exec("uname", "-m")
	if err != nil {
		t.Fatal(err)
	}
	switch machine := strings.TrimSpace(out); machine {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	default:
		t.Fatalf("unsupported VM architecture %q", machine)
		return ""
	}
}

// PrepareNode pushes an e2etest build of HEAD to /usr/local/bin/eru and writes
// node.toml, cluster.toml ([cluster] name = "e2e", [e2e] value = "a") and the
// stub secret under /etc/eru-e2e.
func PrepareNode(t *testing.T, vm *VM) Node {
	t.Helper()
	commit := HeadCommit(t)
	bin := NodeBuild(t, vm, commit)
	if err := vm.Push(bin, NodeBinaryPath, "0755"); err != nil {
		t.Fatal(err)
	}
	secret, err := randomHex(12)
	if err != nil {
		t.Fatal(err)
	}
	secret = "stub-file-secret-" + secret
	script := strings.Join([]string{
		"set -euo pipefail",
		"install -d -m 0755 /etc/eru",
		"install -d -m 0700 /etc/eru-e2e",
		`printf 'cluster = "e2e"\nnode_name = "e2e-node"\nrole = "central"\ninstalled_at = 2026-10-09T12:00:00Z\n' > /etc/eru/node.toml`,
		`printf '[cluster]\nname = "e2e"\n\n[e2e]\nvalue = "a"\n' > /etc/eru/cluster.toml`,
		"chmod 0640 /etc/eru/node.toml /etc/eru/cluster.toml",
		"umask 077",
		"printf '%s\\n' \"$1\" > /etc/eru-e2e/secret",
	}, "\n")
	if out, err := vm.Exec("bash", "-c", script, "prepare", secret); err != nil {
		t.Fatalf("prepare node: %v\n%s", err, out)
	}
	return Node{VM: vm, Binary: bin, Commit: commit, Secret: secret}
}

// ExecStdin runs a command in the VM with stdin, closing it after the input
// like the M3 RPC client will. stdout and stderr stay separate.
func (vm *VM) ExecStdin(stdin string, args ...string) (stdout, stderr string, code int, err error) {
	cmdArgs := append([]string{"exec", vm.Name, "--"}, args...)
	cmd := exec.Command("incus", cmdArgs...) //nolint:gosec // G204: incus exec on this test VM.
	cmd.Stdin = strings.NewReader(stdin)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	runErr := cmd.Run()
	if runErr == nil {
		return outBuf.String(), errBuf.String(), 0, nil
	}
	if exitErr, ok := runErr.(*exec.ExitError); ok { //nolint:errorlint // exec returns *ExitError directly.
		return outBuf.String(), errBuf.String(), exitErr.ExitCode(), nil
	}
	return outBuf.String(), errBuf.String(), -1, fmt.Errorf("incus exec %s: %w", strings.Join(args, " "), runErr)
}

// RPCOptions are the request fields a test sets beyond method and params.
type RPCOptions struct {
	Secrets      map[string]string
	DryRun       bool
	Confirm      string
	LockTimeoutS *int
	// ClientCommit overrides the commit read from eru node version.
	ClientCommit string
	// User runs eru node rpc as this user instead of root.
	User string
}

// RPCEvent is one eru-event/v1 line.
type RPCEvent struct {
	Schema   string          `json:"schema"`
	Type     string          `json:"type"`
	Step     string          `json:"step,omitempty"`
	State    string          `json:"state,omitempty"`
	Index    *int            `json:"index,omitempty"`
	Total    *int            `json:"total,omitempty"`
	Line     string          `json:"line,omitempty"`
	Detail   string          `json:"detail,omitempty"`
	Code     string          `json:"code,omitempty"`
	Message  string          `json:"message,omitempty"`
	Envelope json.RawMessage `json:"envelope,omitempty"`
	TS       string          `json:"ts"`
}

// RPCResult is one node rpc call: every event, the final envelope, and the
// process result.
type RPCResult struct {
	Request  map[string]any
	Events   []RPCEvent
	Envelope Envelope
	Stdout   string
	Stderr   string
	Exit     int
}

// Steps returns the step events in order.
func (r RPCResult) Steps() []RPCEvent {
	var out []RPCEvent
	for _, ev := range r.Events {
		if ev.Type == "step" {
			out = append(out, ev)
		}
	}
	return out
}

// RPC sends one request to eru node rpc in vm. The client fields come from
// eru node version. It fails the test on any malformed line or a missing or
// non-final result.
func RPC(t *testing.T, vm *VM, method string, params any, opts RPCOptions) RPCResult {
	t.Helper()
	res, err := rpcCall(vm, method, params, opts)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// RPCAsync starts RPC in the background. The channel yields the result once
// the call ends; errors are reported with t.Errorf.
func RPCAsync(t *testing.T, vm *VM, method string, params any, opts RPCOptions) <-chan RPCResult {
	t.Helper()
	ch := make(chan RPCResult, 1)
	go func() {
		res, err := rpcCall(vm, method, params, opts)
		if err != nil {
			t.Errorf("async %s: %v", method, err)
		}
		ch <- res
	}()
	return ch
}

func rpcCall(vm *VM, method string, params any, opts RPCOptions) (RPCResult, error) {
	ver, code, err := vm.ExecCode(NodeBinaryPath, "node", "version", "--output", "json")
	if err != nil {
		return RPCResult{}, fmt.Errorf("eru node version: %w", err)
	}
	if code != 0 {
		return RPCResult{}, fmt.Errorf("eru node version: exit %d\n%s", code, ver)
	}
	var env struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal([]byte(ver), &env); err != nil {
		return RPCResult{}, fmt.Errorf("eru node version: %w\n%s", err, ver)
	}
	commit := env.Data["commit"]
	if opts.ClientCommit != "" {
		commit = opts.ClientCommit
	}
	if params == nil {
		params = map[string]any{}
	}
	secrets := opts.Secrets
	if secrets == nil {
		secrets = map[string]string{}
	}
	req := map[string]any{
		"schema":         "eru-rpc/v1",
		"method":         method,
		"params":         params,
		"secrets":        secrets,
		"dry_run":        opts.DryRun,
		"confirm":        opts.Confirm,
		"operator":       "e2e",
		"client_version": env.Data["version"],
		"client_commit":  commit,
		"client_channel": env.Data["channel"],
	}
	if opts.LockTimeoutS != nil {
		req["lock_timeout_s"] = *opts.LockTimeoutS
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return RPCResult{}, err
	}
	args := []string{NodeBinaryPath, "node", "rpc"}
	if opts.User != "" {
		args = append([]string{"runuser", "-u", opts.User, "--"}, args...)
	}
	stdout, stderr, exit, err := vm.ExecStdin(string(raw)+"\n", args...)
	if err != nil {
		return RPCResult{}, err
	}
	res := RPCResult{Request: req, Stdout: stdout, Stderr: stderr, Exit: exit}
	if err := parseEvents(&res); err != nil {
		return res, fmt.Errorf("%s: %w\nexit %d\nstdout:\n%s\nstderr:\n%s", method, err, exit, stdout, stderr)
	}
	return res, nil
}

var eventTypes = map[string]bool{"step": true, "log": true, "progress": true, "warning": true, "prompt-required": true, "result": true}

func parseEvents(res *RPCResult) error {
	scanner := bufio.NewScanner(strings.NewReader(res.Stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	results := 0
	for n := 1; scanner.Scan(); n++ {
		line := scanner.Text()
		var ev RPCEvent
		dec := json.NewDecoder(strings.NewReader(line))
		if err := dec.Decode(&ev); err != nil {
			return fmt.Errorf("line %d is not JSON: %w", n, err)
		}
		if ev.Schema != "eru-event/v1" {
			return fmt.Errorf("line %d has schema %q", n, ev.Schema)
		}
		if !eventTypes[ev.Type] {
			return fmt.Errorf("line %d has type %q", n, ev.Type)
		}
		if ev.TS == "" {
			return fmt.Errorf("line %d has no ts", n)
		}
		if results > 0 {
			return fmt.Errorf("line %d follows the result", n)
		}
		if ev.Type == "result" {
			results++
			if err := json.Unmarshal(ev.Envelope, &res.Envelope); err != nil {
				return fmt.Errorf("result envelope: %w", err)
			}
			if res.Envelope.Schema != "eru/v1" {
				return fmt.Errorf("result envelope schema %q", res.Envelope.Schema)
			}
			if res.Envelope.OK != (res.Exit == 0) {
				return fmt.Errorf("result ok=%t but exit %d", res.Envelope.OK, res.Exit)
			}
		}
		res.Events = append(res.Events, ev)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if results != 1 {
		return fmt.Errorf("want exactly one result event, got %d", results)
	}
	return nil
}
