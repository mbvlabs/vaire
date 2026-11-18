package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// cancelTestDir tells cancellationScript where to record its state. The name
// matches the shell environment's upper-case key rule.
const cancelTestDir = "VAIRE_CANCEL_TEST_DIR"

// cancellationScript starts a background child in the script's process group
// and prints ready only once that child has installed its TERM trap and is
// running. The child records the trap in child.term, so a test can prove that
// cancelling signalled the whole group and not just the script process. The
// script runs until it is signalled.
const cancellationScript = `(
	trap 'printf term > "$` + cancelTestDir + `/child.term"; exit 0' TERM
	printf '%s' "$BASHPID" > "$` + cancelTestDir + `/child.pid"
	: > "$` + cancelTestDir + `/child.ready"
	sleep 300
) &
printf '%s' "$$" > "$` + cancelTestDir + `/group.pid"
while [ ! -e "$` + cancelTestDir + `/child.ready" ]; do sleep 0.01; done
echo ready
wait
`

// TestShellRunCancelsProcessGroup pins Shell.Run's cancellation contract: the
// script's whole process group gets SIGTERM, so a background child stops too.
func TestShellRunCancelsProcessGroup(t *testing.T) {
	shell := Shell{}

	testShellCancelsProcessGroup(t, func(ctx context.Context, dir string, report Reporter) error {
		return shell.Run(ctx, "cancel-test-v1", cancellationScript, map[string]string{cancelTestDir: dir}, report)
	})
}

// TestShellExecCancelsProcessGroup pins the same contract for Shell.Exec.
func TestShellExecCancelsProcessGroup(t *testing.T) {
	shell := Shell{}

	testShellCancelsProcessGroup(t, func(ctx context.Context, dir string, report Reporter) error {
		return shell.Exec(ctx, "cancel-test-v1", Command{
			Name: "/bin/bash",
			Args: []string{"-c", cancellationScript},
			Env:  map[string]string{cancelTestDir: dir},
		}, report)
	})
}

// testShellCancelsProcessGroup starts cancellationScript through start and
// cancels it only after the script reports ready, so the signal lands after its
// background child is running. It then requires ErrInterrupted and the child's
// TERM record. Waiting for the run is bounded by killGrace, so a broken group
// signal fails the test instead of hanging it, and killScriptGroup cleans up
// whatever the failure left behind.
func testShellCancelsProcessGroup(t *testing.T, start func(context.Context, string, Reporter) error) {
	t.Helper()
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())

	t.Cleanup(func() {
		cancel()
		killScriptGroup(dir)
	})
	// The script prints ready exactly once, after its readiness handshake, so
	// cancellation is triggered by observed output rather than by a delay.
	ready := make(chan struct{})

	var once sync.Once

	report := func(ev output.Event) {
		if ev.Line != "ready" {
			return
		}

		once.Do(func() {
			close(ready)
			cancel()
		})
	}

	result := make(chan error, 1)
	go func() { result <- start(ctx, dir, report) }()

	select {
	case err := <-result:
		if !errors.Is(err, ErrInterrupted) {
			t.Fatalf("cancelled run = %v, want ErrInterrupted", err)
		}
	case <-time.After(killGrace + 5*time.Second):
		// killGrace covers the SIGKILL escalation, so the working path is far
		// below this bound and a regression fails here instead of hanging.
		select {
		case <-ready:
			t.Fatal("cancelled run did not return; its process group was not signalled")
		default:
			t.Fatal("run never printed ready")
		}
	}

	term, err := os.ReadFile(filepath.Join(dir, "child.term"))
	if err != nil || string(term) != "term" {
		t.Fatalf("background child did not record the group SIGTERM: %q, %v", term, err)
	}
}

// killScriptGroup SIGKILLs the process group cancellationScript recorded, so a
// failed assertion cannot leave its background child running. It only signals;
// nothing is waited on.
func killScriptGroup(dir string) {
	if pid := recordedPID(filepath.Join(dir, "group.pid")); pid > 0 {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}

	if pid := recordedPID(filepath.Join(dir, "child.pid")); pid > 0 {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// recordedPID returns the pid in path, or 0 when it is missing or malformed.
func recordedPID(path string) int {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0
	}

	return pid
}
