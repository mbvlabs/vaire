package cli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/localenv"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func runSSH(cmd *cobra.Command, _ []string) error {
	command := stringFlag(cmd, "command")
	opts, _ := output.ParseOptions(cmd)
	if command == "" && (output.UsesStructuredOutput(opts) || MustNotPrompt(cmd)) {
		return output.NewError(output.CodeUsage, "vaire ssh needs a terminal or --command", output.ExitUsage,
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
	// ask for the admin password. With stdout piped or redirected it runs
	// without one: a PTY merges the remote stderr (sudo's prompt) into stdout
	// and turns every newline into \r\n, which breaks vaire ssh --command … | jq.
	if command != "" && (!stdinIsTTY() || !stdoutIsTTY() || MustNotPrompt(cmd)) {
		stderr := &sudoPasswordWatch{w: cmd.ErrOrStderr()}
		sess.Stdin = cmd.InOrStdin()
		sess.Stdout = cmd.OutOrStdout()
		sess.Stderr = stderr
		err := sess.Run(command)
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && stderr.seen {
			return output.NewError(output.CodeInputRequired,
				"sudo on the node needs a password, and without a terminal there is none to ask for", output.ExitInput,
				"Run the command from a terminal with stdout not piped so sudo can prompt, "+
					"or have sudo write the output to a file and read it back with vaire ssh --no-input --command 'cat <file>'.").
				WithDetails(map[string]any{"exit_status": exitErr.ExitStatus()})
		}
		return sshExit(err)
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
	// The remote shell owns the terminal: nothing of vaire's live area may draw.
	defer ui.Suspend()()
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return output.WrapError(output.CodePrecondition, err, output.ExitPrecondition, "Run vaire ssh from a terminal.")
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

// stdoutIsTTY reports whether stdout is a terminal.
func stdoutIsTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd())) //nolint:gosec // G115: file descriptors fit in int.
}

// sudoPasswordWatch passes the remote stderr through and notes whether sudo
// refused for want of a terminal to read the password from.
type sudoPasswordWatch struct {
	w    io.Writer
	tail []byte
	seen bool
}

// sudoNoTTY are the lines sudo prints when it needs a password but has no
// terminal.
var sudoNoTTY = [][]byte{[]byte("sudo: a terminal is required"), []byte("sudo: a password is required")}

func (s *sudoPasswordWatch) Write(b []byte) (int, error) {
	if !s.seen {
		// Keep a short tail so a message split across writes still matches.
		s.tail = append(s.tail, b...)
		for _, m := range sudoNoTTY {
			if bytes.Contains(s.tail, m) {
				s.seen = true
			}
		}
		if keep := 64; len(s.tail) > keep {
			s.tail = append(s.tail[:0], s.tail[len(s.tail)-keep:]...)
		}
	}
	return s.w.Write(b)
}
