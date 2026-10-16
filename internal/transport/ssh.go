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
	"sync"
	"sync/atomic"
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

// Keepalive defaults: a keepalive@openssh.com request every interval while
// none is in flight, and the connection is closed only after
// DefaultKeepaliveTimeout in which nothing at all arrived from the node.
const (
	DefaultKeepaliveInterval = 15 * time.Second
	DefaultKeepaliveTimeout  = 3 * time.Minute
)

// SSHOptions configure one connection.
type SSHOptions struct {
	Auth      []ssh.AuthMethod
	HostKeys  *KnownHosts
	Timeout   time.Duration
	ClientVer string
	// KeepaliveInterval and KeepaliveTimeout default to
	// DefaultKeepaliveInterval and DefaultKeepaliveTimeout.
	KeepaliveInterval time.Duration
	KeepaliveTimeout  time.Duration
}

// SSHClient is one authenticated SSH connection. Each Run opens a session.
type SSHClient struct {
	client *ssh.Client
	Target SSHTarget

	stop      chan struct{}
	closeOnce sync.Once
	lost      atomic.Bool
	silence   time.Duration
}

// readTracker records when bytes last arrived on the connection. Any
// inbound byte, a keepalive reply or channel data, shows the node is alive.
type readTracker struct {
	net.Conn
	last atomic.Int64
}

func newReadTracker(nc net.Conn) *readTracker {
	t := &readTracker{Conn: nc}
	t.last.Store(time.Now().UnixNano())
	return t
}

func (t *readTracker) Read(p []byte) (int, error) {
	n, err := t.Conn.Read(p)
	if n > 0 {
		t.last.Store(time.Now().UnixNano())
	}
	return n, err
}

func (t *readTracker) lastRead() time.Time { return time.Unix(0, t.last.Load()) }

// DialSSH connects and authenticates through d. The host key must be pinned
// in opts.HostKeys for target.Addr.
func DialSSH(ctx context.Context, d Dialer, target SSHTarget, opts SSHOptions) (*SSHClient, error) {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// The dial may include a WireGuard handshake, which takes longer than an
	// SSH handshake; a dialer that knows its own budget (the shared tunnel)
	// sets it. The SSH handshake itself keeps the normal timeout.
	dialTimeout := timeout
	if b, ok := d.(interface{ DialBudget() time.Duration }); ok {
		if bt := b.DialBudget(); bt > dialTimeout {
			dialTimeout = bt
		}
	}
	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := d.DialContext(dialCtx, "tcp", target.Addr)
	if err != nil {
		return nil, unreachable(target, err)
	}
	nc := newReadTracker(raw)
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
	interval, silence := opts.KeepaliveInterval, opts.KeepaliveTimeout
	if interval <= 0 {
		interval = DefaultKeepaliveInterval
	}
	if silence <= 0 {
		silence = DefaultKeepaliveTimeout
	}
	c := &SSHClient{client: ssh.NewClient(conn, chans, reqs), Target: target, stop: make(chan struct{}), silence: silence}
	go func() {
		probe := func() error {
			_, _, err := c.client.SendRequest("keepalive@openssh.com", true, nil)
			return err
		}
		if keepalive(c.stop, interval, silence, probe, nc.lastRead) {
			c.lost.Store(true)
			_ = c.client.Close()
		}
	}()
	return c, nil
}

// keepalive probes the connection with probe every interval and reports true
// once a probe is in flight and nothing at all has arrived (lastRead) for
// silence. Without it a stalled stream (a lost WireGuard peer, a dead NAT
// mapping) blocks readers forever; the probe's traffic also prompts
// WireGuard to rehandshake. It returns false when stop closes or a probe
// fails because the connection is already gone (readers see that
// themselves).
//
// At most one probe is in flight: x/crypto/ssh serialises global requests
// with wantReply on one mutex for the whole round trip, so a second probe
// would only queue behind the first. A late reply still counts, as does any
// channel data: a busy node whose replies are slow (TCP retransmits over the
// tunnel, a starved sshd) is alive, not lost.
func keepalive(stop <-chan struct{}, interval, silence time.Duration, probe func() error, lastRead func() time.Time) bool {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var inFlight chan error
	for {
		select {
		case <-stop:
			return false
		case err := <-inFlight:
			inFlight = nil
			if err != nil {
				return false
			}
		case <-ticker.C:
			if inFlight == nil {
				inFlight = make(chan error, 1)
				go func(reply chan<- error) { reply <- probe() }(inFlight)
				continue
			}
			if time.Since(lastRead()) >= silence {
				return true
			}
		}
	}
}

// Lost reports whether keepalive closed the connection because nothing
// arrived from the node for Silence.
func (c *SSHClient) Lost() bool {
	return c != nil && c.lost.Load()
}

// Silence is how long the connection may stay silent, with a keepalive
// unanswered, before keepalive closes it.
func (c *SSHClient) Silence() time.Duration {
	if c == nil {
		return 0
	}
	return c.silence
}

// StreamLost reports whether err from Run means the connection went away
// before the remote command reported an exit status, as opposed to the
// command exiting or the session being refused.
func (c *SSHClient) StreamLost(err error) bool {
	if c.Lost() {
		return true
	}
	var missing *ssh.ExitMissingError
	return errors.As(err, &missing)
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

// Close stops the keepalive and closes the connection.
func (c *SSHClient) Close() {
	if c == nil {
		return
	}
	c.closeOnce.Do(func() {
		if c.stop != nil {
			close(c.stop)
		}
	})
	if c.client != nil {
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
	signer, _, err := KeyFileSigner(path, passphrase)
	if err != nil {
		return nil, err
	}
	return ssh.PublicKeys(signer), nil
}

// KeyFileSigner parses the key file at path. encrypted reports whether the
// key needs a passphrase; with a nil passphrase func an encrypted key fails.
func KeyFileSigner(path string, passphrase func() ([]byte, error)) (signer ssh.Signer, encrypted bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this identity file.
	if err != nil {
		return nil, false, err
	}
	signer, err = ssh.ParsePrivateKey(raw)
	var missing *ssh.PassphraseMissingError
	if errors.As(err, &missing) {
		if passphrase == nil {
			return nil, true, fmt.Errorf("%s is encrypted; load it into ssh-agent instead", path)
		}
		pw, perr := passphrase()
		if perr != nil {
			return nil, true, perr
		}
		signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, pw)
		encrypted = true
	}
	if err != nil {
		return nil, encrypted, fmt.Errorf("parse %s: %w", path, err)
	}
	return signer, encrypted, nil
}

// AgentSigners returns the ssh-agent's signers when SSH_AUTH_SOCK is set and
// the agent holds at least one key.
func AgentSigners() ([]ssh.Signer, func(), bool) {
	sock := os.Getenv("SSH_AUTH_SOCK")
	if sock == "" {
		return nil, func() {}, false
	}
	conn, err := net.Dial("unix", sock) //nolint:gosec // G704: SSH_AUTH_SOCK is the operator's own agent socket.
	if err != nil {
		return nil, func() {}, false
	}
	signers, err := agent.NewClient(conn).Signers()
	if err != nil || len(signers) == 0 {
		_ = conn.Close()
		return nil, func() {}, false
	}
	return signers, func() { _ = conn.Close() }, true
}

// AgentHolds reports whether one of signers has the public key in pubPath.
func AgentHolds(signers []ssh.Signer, pubPath string) bool {
	raw, err := os.ReadFile(pubPath) //nolint:gosec // G304: the .pub next to a default key.
	if err != nil {
		return false
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		return false
	}
	return ContainsKey(publicKeys(signers), pub)
}

func publicKeys(signers []ssh.Signer) []ssh.PublicKey {
	out := make([]ssh.PublicKey, 0, len(signers))
	for _, s := range signers {
		out = append(out, s.PublicKey())
	}
	return out
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
