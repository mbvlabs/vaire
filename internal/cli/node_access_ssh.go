package cli

import (
	"errors"
	"os"
	"os/exec"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/spf13/cobra"
)

func nodeAccessSSHCommand() cmdSpec {
	return cmdSpec{
		use:    "access-ssh",
		hidden: true,
		run:    runNodeAccessSSH,
		meta: CommandMeta{
			Summary:   "Dispatch a restricted SSH login",
			WhenToUse: []string{"Only sshd invokes this forced command for an enrolled Vaire device."},
			Examples:  []string{"vaire node access-ssh"},
			LinuxOnly: true,
		},
	}
}

// SSH_ORIGINAL_COMMAND is untrusted. Compare the entire string and construct
// fresh argv for the two supported commands; never pass it to a shell.
func accessSSHArgs(original string) []string {
	switch original {
	case node.BinaryPath + " node version --output json":
		return []string{node.BinaryPath, "node", "version", "--output", "json"}
	case "sudo -n " + node.BinaryPath + " node rpc":
		return []string{"/usr/bin/sudo", "-n", node.BinaryPath, "node", "rpc"}
	default:
		return nil
	}
}

func runNodeAccessSSH(cmd *cobra.Command, _ []string) error {
	args := accessSSHArgs(os.Getenv("SSH_ORIGINAL_COMMAND"))
	if len(args) == 0 {
		return output.NewError(output.CodePrecondition, "this Vaire device login only accepts node RPC and version requests",
			output.ExitPrecondition, "")
	}

	child := exec.CommandContext(commandContext(cmd), args[0], args[1:]...) //nolint:gosec // Fixed argv selected by accessSSHArgs.
	child.Stdin = cmd.InOrStdin()
	child.Stdout = cmd.OutOrStdout()
	child.Stderr = cmd.ErrOrStderr()
	err := child.Run()

	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return &localenv.SilentExitError{Code: exitErr.ExitCode()}
	}

	return err
}
