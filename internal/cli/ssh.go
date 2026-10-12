package cli

import (
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func runSSH(cmd *cobra.Command, _ []string) error {
	command := stringFlag(cmd, "command")
	opts, _ := output.ParseOptions(cmd)
	if command == "" && (output.UsesStructuredOutput(opts) || MustNotPrompt(cmd)) {
		return output.NewError(output.CodeUsage, "eru ssh needs a terminal or --command", output.ExitUsage,
			"Pass --command to run one command, or use a TTY for a shell.")
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	client := s.node.SSH.SSH()
	if client == nil {
		return output.NewError(output.CodeError, "the SSH client is not available", output.ExitGeneric, "")
	}
	sess, err := client.NewSession()
	if err != nil {
		return output.WrapError(output.CodeTransportFailed, err, output.ExitTransport, "")
	}
	defer func() { _ = sess.Close() }()

	// A command run from a terminal gets a PTY too (like ssh -t), so sudo can
	// ask for the admin password.
	if command != "" && (!stdinIsTTY() || MustNotPrompt(cmd)) {
		sess.Stdin = cmd.InOrStdin()
		sess.Stdout = cmd.OutOrStdout()
		sess.Stderr = cmd.ErrOrStderr()
		return sshExit(sess.Run(command))
	}

	fd := int(os.Stdin.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	width, height, err := term.GetSize(fd)
	if err != nil {
		width, height = 80, 24
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := sess.RequestPty("xterm-256color", height, width, modes); err != nil {
		return output.WrapError(output.CodeTransportFailed, err, output.ExitTransport, "The node refused a PTY.")
	}
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Run eru ssh from a terminal.")
	}
	defer func() { _ = term.Restore(fd, oldState) }()
	sess.Stdin = os.Stdin
	sess.Stdout = os.Stdout
	sess.Stderr = os.Stderr
	go forwardWindowSize(fd, sess)
	if command != "" {
		return sshExit(sess.Run(command))
	}
	// Shell only starts the remote shell; Wait blocks until it exits.
	if err := sess.Shell(); err != nil {
		return sshExit(err)
	}
	return sshExit(sess.Wait())
}

func sshExit(err error) error {
	if err == nil {
		return nil
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return &localenv.SilentExit{Code: exitErr.ExitStatus()}
	}
	return output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
}

func forwardWindowSize(fd int, sess *ssh.Session) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	for range ch {
		if width, height, err := term.GetSize(fd); err == nil {
			_ = sess.WindowChange(height, width)
		}
	}
}
