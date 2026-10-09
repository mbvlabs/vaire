package localenv

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/spf13/cobra"
)

// SilentExit is a child process exit that the parent must propagate without
// printing a second envelope.
type SilentExit struct {
	Code int
}

func (e *SilentExit) Error() string {
	if e == nil {
		return ""
	}
	return "exit " + strconv.Itoa(e.Code)
}

// Prompter asks a yes/no question without importing the cli package.
type Prompter interface {
	YesNo(cmd *cobra.Command, question string) (bool, error)
}

var prompter Prompter

// SetPrompter installs the TTY prompt used for interactive sudo.
func SetPrompter(p Prompter) {
	prompter = p
}

// Escalate re-executes the current command under sudo when the process is not
// root. handled is true when a child ran; the parent must exit with err.
func Escalate(cmd *cobra.Command, reason string, extraArgs ...string) (bool, error) {
	if os.Geteuid() == 0 || flagBool(cmd, "escalated") {
		return false, nil
	}
	if err := exec.Command("sudo", "-n", "true").Run(); err == nil { //nolint:gosec // G204: fixed sudo probe.
		return reexec(true, extraArgs)
	}
	if canPrompt(cmd) {
		ok, err := ask(cmd, "eru needs root to "+reason+". Escalate with sudo?")
		if err != nil {
			return false, err
		}
		if !ok {
			return false, rootRequired(extraArgs)
		}
		return reexec(false, extraArgs)
	}
	if flagBool(cmd, "sudo") {
		return false, output.NewError(
			output.CodePrecondition,
			"sudo needs a password",
			output.ExitPrecondition,
			"Run from a terminal, or configure passwordless sudo.",
		).WithDetails(map[string]any{
			"reason":  "sudo_password_required",
			"command": sudoLine(extraArgs),
		})
	}
	return false, rootRequired(extraArgs)
}

func canPrompt(cmd *cobra.Command) bool {
	if cmd != nil && cmd.Flags() != nil {
		if flag := cmd.Flags().Lookup("no-input"); flag != nil && flag.Value.String() == "true" {
			return false
		}
	}
	opts, err := output.ParseOptions(cmd)
	if err == nil && output.UsesStructuredOutput(opts) {
		return false
	}
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func ask(cmd *cobra.Command, question string) (bool, error) {
	if prompter == nil {
		return false, errors.New("sudo prompt is not configured")
	}
	return prompter.YesNo(cmd, question)
}

func reexec(noninteractive bool, extraArgs []string) (bool, error) {
	self, err := os.Executable()
	if err != nil {
		return false, err
	}
	sudoArgs := make([]string, 0, 4+len(os.Args)+len(extraArgs))
	if noninteractive {
		sudoArgs = append(sudoArgs, "-n")
	}
	sudoArgs = append(sudoArgs, self)
	sudoArgs = append(sudoArgs, filteredArgs()...)
	sudoArgs = append(sudoArgs, "--escalated")
	sudoArgs = append(sudoArgs, extraArgs...)
	child := exec.Command("sudo", sudoArgs...) //nolint:gosec // G204: re-exec of this binary under sudo.
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	err = child.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return true, &SilentExit{Code: exitErr.ExitCode()}
	}
	return true, err
}

func filteredArgs() []string {
	out := make([]string, 0, len(os.Args))
	for _, arg := range os.Args[1:] {
		if arg == "--escalated" || strings.HasPrefix(arg, "--escalated=") {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func sudoLine(extraArgs []string) string {
	self, err := os.Executable()
	if err != nil {
		self = "eru"
	}
	parts := []string{"sudo", self}
	parts = append(parts, filteredArgs()...)
	parts = append(parts, extraArgs...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.ContainsAny(part, " \t\"'") {
			quoted = append(quoted, strconv.Quote(part))
		} else {
			quoted = append(quoted, part)
		}
	}
	return strings.Join(quoted, " ")
}

func rootRequired(extraArgs []string) error {
	line := sudoLine(extraArgs)
	return output.NewError(
		output.CodePrecondition,
		"this command needs root",
		output.ExitPrecondition,
		"Re-run as root, or: "+line,
	).WithDetails(map[string]any{
		"reason":  "root_required",
		"command": line,
	})
}

func flagBool(cmd *cobra.Command, name string) bool {
	if cmd == nil || cmd.Flags() == nil {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	if flag == nil {
		return false
	}
	value, err := strconv.ParseBool(flag.Value.String())
	return err == nil && value
}
