//go:build e2e

package harness

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// VM is an Incus virtual machine created for one test.
type VM struct {
	t    *testing.T
	Name string
}

// NewVM launches an Incus VM and deletes it when the test ends.
func NewVM(t *testing.T, image string) *VM {
	t.Helper()
	if _, err := exec.LookPath("incus"); err != nil {
		t.Fatal("incus is required for the e2e harness")
	}
	if image == "" {
		image = "images:debian/13"
	}
	name, err := vmName()
	if err != nil {
		t.Fatal(err)
	}
	vm := &VM{t: t, Name: name}
	t.Cleanup(func() {
		out, err := exec.Command("incus", "delete", name, "--force").CombinedOutput() //nolint:gosec // G204: incus delete of this test VM.
		if err != nil {
			t.Logf("delete %s: %v\n%s", name, err, out)
		}
	})
	out, err := exec.Command( //nolint:gosec // G204: incus launch of the configured test image.
		"incus", "launch", image, name, "--vm",
		"-c", "limits.memory=2GiB",
		"-c", "limits.cpu=2",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("incus launch %s: %v\n%s", image, err, out)
	}
	vm.waitReady(3 * time.Minute)
	return vm
}

// Snapshot stores the VM as the baseline snapshot.
func Snapshot(vm *VM) {
	vm.t.Helper()
	vm.run("snapshot", "create", vm.Name, "baseline")
}

// Restore returns the VM to the baseline snapshot. Incus restarts a running
// VM to restore it, so this waits for the agent again.
func Restore(vm *VM) {
	vm.t.Helper()
	vm.run("snapshot", "restore", vm.Name, "baseline")
	vm.waitReady(3 * time.Minute)
}

// KillTunnel drops WireGuard UDP on the VM so a commit-confirm test can fail closed.
func KillTunnel(vm *VM) error {
	if vm == nil {
		return fmt.Errorf("no vm")
	}
	script := strings.Join([]string{
		"nft add table inet eru_e2e",
		"nft add chain inet eru_e2e input '{ type filter hook input priority 0 ; policy accept ; }'",
		"nft add rule inet eru_e2e input udp dport 51820 drop",
	}, "\n")
	_, err := vm.Exec("bash", "-lc", script)
	return err
}

// SQL runs eru sql against a context. Cluster tests call this after init.
func SQL(t *testing.T, contextName, query string) string {
	t.Helper()
	env := Eru(t, "--context", contextName, "sql", query)
	if env.Exit != 0 || !env.OK {
		t.Fatalf("sql %q failed: %s", query, env.Stdout)
	}
	return string(env.Data)
}

// Exec runs a command inside the VM.
func (vm *VM) Exec(args ...string) (string, error) {
	out, code, err := vm.ExecCode(args...)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("incus exec %s: exit %d\n%s", strings.Join(args, " "), code, out)
	}
	return out, nil
}

// ExecCode runs a command inside the VM and returns combined output and exit
// status. The error is set only when incus itself could not run the command.
func (vm *VM) ExecCode(args ...string) (string, int, error) {
	cmdArgs := append([]string{"exec", vm.Name, "--"}, args...)
	cmd := exec.Command("incus", cmdArgs...) //nolint:gosec // G204: incus exec on this test VM.
	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode(), nil
	}
	return string(out), -1, fmt.Errorf("incus exec %s: %w\n%s", strings.Join(args, " "), err, out)
}

// Push copies a local file into the VM.
func (vm *VM) Push(local, remote, mode string) error {
	vm.t.Helper()
	out, err := exec.Command( //nolint:gosec // G204: incus file push onto this test VM.
		"incus", "file", "push", local, vm.Name+remote,
		"--mode", mode, "--uid", "0", "--gid", "0",
	).CombinedOutput()
	if err != nil {
		return fmt.Errorf("incus file push: %w\n%s", err, out)
	}
	return nil
}

func (vm *VM) waitReady(timeout time.Duration) {
	vm.t.Helper()
	deadline := time.Now().Add(timeout)
	var last []byte
	for time.Now().Before(deadline) {
		out, err := exec.Command("incus", "exec", vm.Name, "--", "true").CombinedOutput() //nolint:gosec // G204: readiness probe for this test VM.
		if err == nil {
			return
		}
		last = out
		time.Sleep(2 * time.Second)
	}
	vm.t.Fatalf("vm %s agent did not become ready: %s", vm.Name, last)
}

func (vm *VM) run(args ...string) {
	vm.t.Helper()
	out, err := exec.Command("incus", args...).CombinedOutput() //nolint:gosec // G204: incus snapshot commands for this test VM.
	if err != nil {
		vm.t.Fatalf("incus %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func vmName() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "eru-e2e-" + hex.EncodeToString(buf), nil
}

// Image returns the VM image for this run.
func Image() string {
	if image := os.Getenv("ERU_E2E_IMAGE"); image != "" {
		return image
	}
	return "images:debian/13"
}

// ParseEnvelope unmarshals an eru JSON payload.
func ParseEnvelope(t *testing.T, raw string) Envelope {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("parse envelope: %v\n%s", err, raw)
	}
	return env
}
