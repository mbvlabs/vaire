package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// Command is one fixed program the shell runs without bash: the backup tools
// (pgbackrest, pg_dump, pg_restore, pg_ctl) whose output eru needs to read.
// Arguments never carry a secret; secrets travel in Env or Stdin.
type Command struct {
	Name string
	Args []string
	// Env is added to the fixed script environment.
	Env map[string]string
	// Stdin feeds the program; nil is empty.
	Stdin io.Reader
	// Stdout receives the raw standard output. When nil, stdout lines are
	// streamed as redacted log events like stderr.
	Stdout io.Writer
	// OnLine, when set, sees every redacted output line before it is
	// reported, for progress parsing.
	OnLine func(line string)
}

// Exec runs c the way Run runs a script: the fixed environment, its own
// process group, SIGTERM on cancel and SIGKILL killGrace later, and every
// output line redacted and reported as a log event for stepID. A non-zero
// exit is a *ScriptError carrying the last output lines.
func (s Shell) Exec(ctx context.Context, stepID string, c Command, report Reporter) error {
	if s.DryRun {
		return errDryRun
	}
	cmd := exec.Command(c.Name, c.Args...) //nolint:gosec // G204: fixed backup tools; arguments carry no secrets.
	cmd.Env = []string{"PATH=" + scriptPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=/root"}
	for key, value := range c.Env {
		if !envKeyPattern.MatchString(key) {
			return fmt.Errorf("command env key %q is not an upper-case identifier", key)
		}
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	cmd.Stdin = c.Stdin
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stderr = pw
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = pw
	}
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

	lineReport := report
	if c.OnLine != nil {
		lineReport = func(ev output.Event) {
			c.OnLine(ev.Line)
			if report != nil {
				report(ev)
				return
			}
			s.Log.Line("log "+stepID, ev.Line)
		}
	}
	tail := s.stream(pr, stepID, lineReport)
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
		return fmt.Errorf("%s for %s: %w", c.Name, stepID, waitErr)
	}
	return nil
}

// RunQuiet runs a read-only probe with the fixed environment and reports only
// whether it exited zero.
func RunQuiet(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed probe commands.
	cmd.Env = []string{"PATH=" + scriptPath, "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "HOME=/root"}
	return cmd.Run()
}
