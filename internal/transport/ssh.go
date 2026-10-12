package transport

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// maxOutput caps buffered command output.
const maxOutput = 4 << 20

// SSHTarget is user@host:port.
type SSHTarget struct {
	User string
	Addr string
}

func (t SSHTarget) String() string { return t.User + "@" + t.Addr }

// SSHOptions configure one connection.
type SSHOptions struct {
	Auth      []ssh.AuthMethod
	HostKeys  *KnownHosts
	Timeout   time.Duration
	ClientVer string
}

// SSHClient is one authenticated SSH connection. Each Run opens a session.
type SSHClient struct {
	client *ssh.Client
	Target SSHTarget
}

// DialSSH connects and authenticates through d. The host key must be pinned
// in opts.HostKeys for target.Addr.
func DialSSH(ctx context.Context, d Dialer, target SSHTarget, opts SSHOptions) (*SSHClient, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nc, err := d.DialContext(dialCtx, "tcp", target.Addr)
	if err != nil {
		return nil, unreachable(target, err)
	}
	var hostErr *HostKeyError
	callback := opts.HostKeys.Callback(target.Addr)
	cfg := &ssh.ClientConfig{
		User: target.User,
		Auth: opts.Auth,
		HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
			err := callback(host, remote, key)
			if errors.As(err, &hostErr) {
				return err
			}
			return err
		},
		HostKeyAlgorithms: opts.HostKeys.Algorithms(target.Addr),
		Timeout:           timeout,
		ClientVersion:     opts.ClientVer,
	}
	conn, chans, reqs, err := handshake(ctx, nc, target.Addr, cfg, timeout)
	if err != nil {
		_ = nc.Close()
		if hostErr != nil {
			return nil, hostKeyMismatch(target, hostErr)
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, output.NewError(
				output.CodeTransportFailed,
				"SSH authentication to "+target.String()+" failed",
				output.ExitTransport,
				"Check the SSH user and credential: --identity, an agent key, or --password-stdin.",
			).WithDetails(map[string]any{"target": target.String(), "reason": "auth_failed"})
		}
		return nil, output.WrapError(output.CodeTransportFailed, fmt.Errorf("SSH handshake with %s failed: %w", target, err), output.ExitTransport,
			"Check that sshd is running and reachable.").WithDetails(map[string]any{"target": target.String()})
	}
	return &SSHClient{client: ssh.NewClient(conn, chans, reqs), Target: target}, nil
}

func handshake(ctx context.Context, nc net.Conn, addr string, cfg *ssh.ClientConfig, timeout time.Duration) (ssh.Conn, <-chan ssh.NewChannel, <-chan *ssh.Request, error) {
	deadline := time.Now().Add(timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = nc.SetDeadline(deadline)
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = nc.Close()
		case <-finished:
		}
	}()
	conn, chans, reqs, err := ssh.NewClientConn(nc, addr, cfg)
	close(finished)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, nil, ctx.Err()
		}
		return nil, nil, nil, err
	}
	_ = nc.SetDeadline(time.Time{})
	return conn, chans, reqs, nil
}

func unreachable(target SSHTarget, err error) *output.CLIError {
	return output.WrapError(output.CodeRemoteUnreachable, fmt.Errorf("cannot reach %s: %w", target.Addr, err), output.ExitTransport,
		"Check the address, that the server is up, and that this machine can reach it.").
		WithDetails(map[string]any{"target": target.String()})
}

func hostKeyMismatch(target SSHTarget, e *HostKeyError) *output.CLIError {
	if e.Unknown {
		return output.NewError(
			output.CodeHostKeyMismatch,
			"no host key is pinned for "+e.Host,
			output.ExitTransport,
			"Pin it with --host-key-fingerprint, or confirm it on first contact.",
		).WithDetails(map[string]any{"target": target.String(), "got": e.Got})
	}
	return output.NewError(
		output.CodeHostKeyMismatch,
		"the host key of "+e.Host+" does not match the pinned key",
		output.ExitTransport,
		"Someone may be intercepting the connection, or the server was rebuilt. Do not continue until a human checks the fingerprint.",
	).WithDetails(map[string]any{"target": target.String(), "got": e.Got, "want": e.Want})
}

// SSH returns the underlying ssh.Client, for interactive sessions.
func (c *SSHClient) SSH() *ssh.Client {
	if c == nil {
		return nil
	}
	return c.client
}

// Close closes the connection.
func (c *SSHClient) Close() {
	if c != nil && c.client != nil {
		_ = c.client.Close()
	}
}

// Run runs command in a new session. A non-zero exit returns *ssh.ExitError.
// Cancelling ctx signals the remote process and closes the session.
func (c *SSHClient) Run(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	sess, err := c.client.NewSession()
	if err != nil {
		return output.WrapError(output.CodeTransportFailed, fmt.Errorf("open an SSH session to %s: %w", c.Target, err), output.ExitTransport, "")
	}
	defer func() { _ = sess.Close() }()
	if stdin != nil {
		sess.Stdin = stdin
	}
	sess.Stdout = stdout
	sess.Stderr = stderr
	done := make(chan error, 1)
	go func() { done <- sess.Run(command) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		_ = sess.Close()
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Output runs command and returns its stdout and stderr.
func (c *SSHClient) Output(ctx context.Context, command string, stdin io.Reader) ([]byte, []byte, error) {
	stdout := &capped{max: maxOutput}
	stderr := &capped{max: maxOutput}
	err := c.Run(ctx, command, stdin, stdout, stderr)
	return stdout.buf.Bytes(), stderr.buf.Bytes(), err
}

// ExitStatus returns the remote exit status of err, if it is one.
func ExitStatus(err error) (int, bool) {
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitStatus(), true
	}
	return 0, false
}

type capped struct {
	buf bytes.Buffer
	max int
}

func (c *capped) Write(p []byte) (int, error) {
	if room := c.max - c.buf.Len(); room > 0 {
		if len(p) > room {
			c.buf.Write(p[:room])
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// ProbeHostKey connects to addr, records the host key the server presents,
// and disconnects before authenticating. First contact uses it for TOFU.
func ProbeHostKey(ctx context.Context, d Dialer, addr string, timeout time.Duration) (ssh.PublicKey, error) {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	target := SSHTarget{User: "eru-host-key-probe", Addr: addr}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	nc, err := d.DialContext(dialCtx, "tcp", addr)
	if err != nil {
		return nil, unreachable(target, err)
	}
	defer func() { _ = nc.Close() }()
	var captured ssh.PublicKey
	errCaptured := errors.New("host key captured")
	cfg := &ssh.ClientConfig{
		User: target.User,
		Auth: []ssh.AuthMethod{ssh.Password("")},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			captured = key
			return errCaptured
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		Timeout:           timeout,
	}
	_, _, _, err = handshake(ctx, nc, addr, cfg, timeout)
	if captured == nil {
		return nil, output.WrapError(output.CodeTransportFailed, fmt.Errorf("read the SSH host key of %s: %w", addr, err), output.ExitTransport, "Check that sshd answers on that port.")
	}
	return captured, nil
}

// GenerateSSHKey returns a new ed25519 key as an OpenSSH private key PEM and
// its authorized_keys line.
func GenerateSSHKey(comment string) ([]byte, string, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, comment)
	if err != nil {
		return nil, "", err
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	if comment != "" {
		line += " " + comment
	}
	return pem.EncodeToMemory(block), line, nil
}

// AuthorizedKey returns the authorized_keys line of an OpenSSH private key.
func AuthorizedKey(privatePEM []byte, comment string) (string, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return "", err
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
	if comment != "" {
		line += " " + comment
	}
	return line, nil
}

// KeyAuth authenticates with an unencrypted private key.
func KeyAuth(privatePEM []byte) (ssh.AuthMethod, error) {
	signer, err := ssh.ParsePrivateKey(privatePEM)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(signer), nil
}

// KeyFileAuth authenticates with the key file at path. passphrase is asked
// for when the key is encrypted; nil means encrypted keys fail.
func KeyFileAuth(path string, passphrase func() ([]byte, error)) (ssh.AuthMethod, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this identity file.
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == nil {
			return nil, fmt.Errorf("%s is encrypted; load it into ssh-agent instead", path)
		}
		pw, perr := passphrase()
		if perr != nil {
			return nil, perr
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, pw)
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return ssh.PublicKeys(signer), nil
}

// AgentAuth returns the ssh-agent's keys when SSH_AUTH_SOCK is set. The
// returned close function releases the agent connection.
func AgentAuth() (ssh.AuthMethod, func(), bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}, false
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // G704: SSH_AUTH_SOCK is the operator's own agent socket.
	if err != nil {
		return nil, func() {}, false
	}
	client := agent.NewClient(conn)
	signers, err := client.Signers()
	if err != nil || len(signers) == 0 {
		_ = conn.Close()
		return nil, func() {}, false
	}
	return ssh.PublicKeysCallback(client.Signers), func() { _ = conn.Close() }, true
}

// PasswordAuth answers password and keyboard-interactive prompts with pw.
func PasswordAuth(pw string) []ssh.AuthMethod {
	return []ssh.AuthMethod{
		ssh.Password(pw),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, _ []bool) ([]string, error) {
			answers := make([]string, len(questions))
			for i := range answers {
				answers[i] = pw
			}
			return answers, nil
		}),
	}
}
