package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// ErrInterrupted marks a step stopped by SIGTERM or SIGINT.
var ErrInterrupted = errors.New("interrupted")

// errDryRun is returned when a script would run in a dry run.
var errDryRun = errors.New("shell refuses to run scripts in a dry run")

var envKeyPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// scriptPath is the fixed PATH scripts run with. Scripts never inherit the
// caller's environment.
const scriptPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// tailLines is how many output lines a failed script keeps for its error.
const tailLines = 20

// ScriptError is a script that exited non-zero. Tail holds its last output
// lines, redacted, so a failed step shows why without --verbose.
type ScriptError struct {
	Step   string
	Status int
	Tail   []string
}

func (e *ScriptError) Error() string {
	return fmt.Sprintf("script for %s exited with status %d", e.Step, e.Status)
}

// killGrace is how long a cancelled script has between SIGTERM and SIGKILL.
const killGrace = 10 * time.Second

// Shell runs embedded bash scripts. Inputs travel only through the
// environment; output streams line by line as redacted log events.
type Shell struct {
	DryRun  bool
	Secrets *secrets.Registry
	Log     *RunLog
}

// Run feeds script to /bin/bash -s with env. Each output line becomes one log
// event for stepID. Cancelling ctx sends SIGTERM to the script's process group.
func (s Shell) Run(ctx context.Context, stepID, script string, env map[string]string, report Reporter) error {
	if s.DryRun {
		return errDryRun
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("script env key %q is not an upper-case identifier", key)
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	cmd := exec.Command("/bin/bash", "-s") //nolint:gosec // G204: fixed interpreter; the script comes from go:embed.
	cmd.Stdin = strings.NewReader(script)
	cmd.Env = []string{"PATH=" + scriptPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=/root", "DEBIAN_FRONTEND=noninteractive"}
	for _, key := range keys {
		cmd.Env = append(cmd.Env, key+"="+env[key])
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	_ = pw.Close()

	done := make(chan struct{})
	interrupted := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			close(interrupted)
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(killGrace):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		case <-done:
		}
	}()

	tail := s.stream(pr, stepID, report)
	_ = pr.Close()
	waitErr := cmd.Wait()
	close(done)
	select {
	case <-interrupted:
		return ErrInterrupted
	default:
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return &ScriptError{Step: stepID, Status: exitErr.ExitCode(), Tail: tail}
		}
		return fmt.Errorf("script for %s: %w", stepID, waitErr)
	}
	return nil
}

// stream turns each output line into a log event and returns the last
// tailLines lines.
func (s Shell) stream(r io.Reader, stepID string, report Reporter) []string {
	reader := bufio.NewReaderSize(r, 64*1024)
	var tail []string
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			line = strings.TrimRight(line, "\r\n")
			line = s.Secrets.Redact(secrets.Redact(line))
			tail = append(tail, line)
			if len(tail) > tailLines {
				tail = tail[1:]
			}
			if report != nil {
				report(output.Event{Type: "log", Step: stepID, Line: line})
			} else {
				s.Log.Line("log "+stepID, line)
			}
		}
		if err != nil {
			return tail
		}
	}
}
