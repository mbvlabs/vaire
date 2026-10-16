//go:build unix

package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// The shared tunnel: one daemon per context owns the only WireGuard device
// for the operator key and proxies TCP dials into it over a unix socket.
// Every eru process, the one that spawned the daemon included, dials through
// it, so two commands no longer steal the node's single peer from each other.

// SharedProto is the socket protocol version.
const SharedProto = 1

// maxSharedLine caps one protocol line.
const maxSharedLine = 64 << 10

// SharedDialTimeout bounds one dial the daemon makes into the tunnel. It must
// allow for a WireGuard handshake: after the tunnel has been idle the session
// expired, and the first packet triggers a re-handshake that wireguard-go
// retries every REKEY_TIMEOUT (5s). A per-command client waits
// sharedDialBudget for the daemon's reply, a little longer than this so the
// daemon's own failure reply wins the race.
const SharedDialTimeout = 2 * time.Minute

// sharedDialBudget is how long a client waits for a dial reply from the
// daemon: SharedDialTimeout plus a margin.
const sharedDialBudget = SharedDialTimeout + 15*time.Second

// sharedControlTimeout bounds the control ops (hello, attach, stop), which
// the daemon answers at once.
const sharedControlTimeout = 30 * time.Second

// ErrDaemonNoAnswer is what a client read from the daemon returns when it
// timed out: the daemon is up but did not reply.
var ErrDaemonNoAnswer = errors.New("the tunnel daemon did not answer")

// TunnelDialError is the daemon's dial_failed reply: the tunnel could not
// reach the address within SharedDialTimeout, usually a WireGuard handshake
// that did not complete.
type TunnelDialError struct {
	Addr    string
	Message string
}

func (e *TunnelDialError) Error() string {
	return "the tunnel could not reach " + e.Addr + ": " + e.Message
}

type sharedRequest struct {
	V     int    `json:"v"`
	Op    string `json:"op"`
	Addr  string `json:"addr,omitempty"`
	PID   int    `json:"pid,omitempty"`
	Cmd   string `json:"cmd,omitempty"`
	Force bool   `json:"force,omitempty"`
}

// SharedClient is one eru process attached to the daemon.
type SharedClient struct {
	PID     int    `json:"pid"`
	Command string `json:"command"`
	Since   string `json:"since"`
}

// SharedStatus is the daemon's hello reply.
type SharedStatus struct {
	Proto         int            `json:"proto,omitempty"`
	Build         string         `json:"build,omitempty"`
	PID           int            `json:"pid,omitempty"`
	Fingerprint   string         `json:"fingerprint,omitempty"`
	Attached      int            `json:"attached"`
	Streams       int            `json:"streams"`
	Clients       []SharedClient `json:"clients,omitempty"`
	Draining      bool           `json:"draining,omitempty"`
	StartedAt     string         `json:"started_at,omitempty"`
	IdleDeadline  string         `json:"idle_deadline,omitempty"`
	LastHandshake string         `json:"last_handshake,omitempty"`
}

type sharedReply struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	SharedStatus
}

// ErrDaemonRunning is what a spawn returns when another daemon (or an
// in-process tunnel) holds the context's singleton lock.
var ErrDaemonRunning = errors.New("a tunnel daemon is already running")

// errRestart asks ensure to look again after it stopped a stale daemon.
var errRestart = errors.New("restart the tunnel daemon")

// IsNoDaemon reports whether a connect error means nothing serves the
// socket: it is missing, or left behind by a daemon that died.
func IsNoDaemon(err error) bool {
	return errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED)
}

// TunnelFingerprint identifies a tunnel configuration: a daemon serves only
// clients whose context computes the same value. The private key enters
// only through its public half.
func TunnelFingerprint(cfg WireGuardConfig) string {
	pub, _ := WGPublicKey(cfg.PrivateKey)
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "proto=%d\noperator=%s\naddress=%s\npeer=%s\nendpoint=%s\nmtu=%d\n",
		SharedProto, pub, cfg.Address, strings.TrimSpace(cfg.PeerPublicKey), cfg.Endpoint, TunnelMTU)
	for _, p := range cfg.AllowedIPs {
		_, _ = fmt.Fprintf(h, "allowed=%s\n", p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// readLine reads one newline-terminated line a byte at a time, so nothing
// after it (a dial's raw bytes) is consumed.
func readLine(r io.Reader) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for {
		n, err := r.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return line, nil
			}
			line = append(line, b[0])
			if len(line) > maxSharedLine {
				return nil, errors.New("protocol line too long")
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(line) > 0 {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
	}
}

func writeReply(w io.Writer, r sharedReply) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = w.Write(append(raw, '\n'))
	return err
}

// roundTrip connects to the socket, sends req and reads the reply line. The
// connection stays open for an attach or a dial; the caller closes it.
func roundTrip(ctx context.Context, socket string, req sharedRequest) (net.Conn, sharedReply, error) {
	return roundTripWithin(ctx, socket, req, sharedControlTimeout)
}

// roundTripWithin is roundTrip with the budget bounding the wait when ctx has
// no deadline of its own.
func roundTripWithin(ctx context.Context, socket string, req sharedRequest, budget time.Duration) (net.Conn, sharedReply, error) {
	req.V = SharedProto
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, sharedReply{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Now()) })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(budget))
	}
	raw, _ := json.Marshal(req)
	if _, err := conn.Write(append(raw, '\n')); err != nil {
		_ = conn.Close()
		return nil, sharedReply{}, err
	}
	line, err := readLine(conn)
	if err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, sharedReply{}, ctx.Err()
		}
		return nil, sharedReply{}, fmt.Errorf("%w: %w", ErrDaemonNoAnswer, err)
	}
	var reply sharedReply
	if err := json.Unmarshal(line, &reply); err != nil {
		_ = conn.Close()
		return nil, sharedReply{}, fmt.Errorf("the tunnel daemon's reply is not JSON: %w", err)
	}
	if !stop() {
		// ctx ended while the reply came in; the deadline already fired.
		_ = conn.Close()
		return nil, sharedReply{}, ctx.Err()
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, reply, nil
}

// SharedHello asks the daemon on socket for its status.
func SharedHello(ctx context.Context, socket string) (SharedStatus, error) {
	conn, reply, err := roundTrip(ctx, socket, sharedRequest{Op: "hello"})
	if err != nil {
		return SharedStatus{}, err
	}
	_ = conn.Close()
	if !reply.OK {
		return SharedStatus{}, errors.New(reply.Message)
	}
	return reply.SharedStatus, nil
}

// SharedStop asks the daemon on socket to exit: at once with force, else
// once its last client is gone. draining reports that it is still serving
// clients and will exit after them.
func SharedStop(ctx context.Context, socket string, force bool) (draining bool, err error) {
	conn, reply, err := roundTrip(ctx, socket, sharedRequest{Op: "stop", Force: force})
	if err != nil {
		return false, err
	}
	_ = conn.Close()
	if !reply.OK {
		return false, errors.New(reply.Message)
	}
	return reply.Draining, nil
}

// SharedWaitGone waits up to within for the daemon on socket to stop
// answering.
func SharedWaitGone(ctx context.Context, socket string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		hctx, cancel := context.WithTimeout(ctx, time.Second)
		_, err := SharedHello(hctx, socket)
		cancel()
		if err != nil && IsNoDaemon(err) {
			return true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// SharedDialerConfig configures a client of the shared tunnel.
type SharedDialerConfig struct {
	// Socket is the daemon's unix socket.
	Socket string
	// SpawnLock is a file locked while a client checks for and spawns the
	// daemon, so concurrent clients start exactly one.
	SpawnLock string
	// Fingerprint is TunnelFingerprint of this client's context.
	Fingerprint string
	// Build is this client's eru build. An idle daemon of another build is
	// replaced; a busy one is used as it is.
	Build string
	// PID and Command describe this client in eru tunnel status.
	PID     int
	Command string
	// Spawn starts a daemon and returns once it serves Socket, or with its
	// startup error (ErrDaemonRunning when the singleton lock is taken).
	Spawn func(ctx context.Context) error
	// Retry is the wait between spawn attempts while another daemon holds
	// the singleton lock. Zero means 250ms.
	Retry time.Duration
}

// SharedDialer dials through the context's tunnel daemon, starting it when
// none runs. It holds a lease for its lifetime, which keeps the daemon up.
// When the daemon dies, the next dial starts a new one.
type SharedDialer struct {
	cfg SharedDialerConfig

	mu        sync.Mutex
	lease     net.Conn
	daemonPID int
	closed    bool
}

// NewSharedDialer returns a dialer for cfg. Attach connects it.
func NewSharedDialer(cfg SharedDialerConfig) *SharedDialer {
	if cfg.Retry <= 0 {
		cfg.Retry = 250 * time.Millisecond
	}
	return &SharedDialer{cfg: cfg}
}

// Attach makes sure the daemon runs and holds a lease on it.
func (d *SharedDialer) Attach(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.attachLocked(ctx)
}

// DaemonPID is the pid of the daemon the lease is on, 0 before Attach.
func (d *SharedDialer) DaemonPID() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.daemonPID
}

func (d *SharedDialer) attachLocked(ctx context.Context) error {
	if d.closed {
		return errors.New("the shared tunnel dialer is closed")
	}
	if d.lease != nil {
		return nil
	}
	st, err := d.ensure(ctx)
	if err != nil {
		return err
	}
	conn, reply, err := roundTrip(ctx, d.cfg.Socket, sharedRequest{Op: "attach", PID: d.cfg.PID, Cmd: d.cfg.Command})
	if err != nil {
		return sharedError(err)
	}
	if !reply.OK {
		_ = conn.Close()
		if reply.Code == "draining" {
			return stoppingError(st)
		}
		return sharedError(errors.New(reply.Message))
	}
	d.lease = conn
	d.daemonPID = st.PID
	go func() {
		// The daemon never writes on a lease: a read returns when it
		// exits or the lease is released.
		_, _ = io.Copy(io.Discard, conn)
		d.mu.Lock()
		if d.lease == conn {
			d.lease = nil
		}
		d.mu.Unlock()
		_ = conn.Close()
	}()
	return nil
}

// ensure returns the status of a daemon that serves this client's tunnel,
// spawning one when none runs and replacing an idle stale one.
func (d *SharedDialer) ensure(ctx context.Context) (SharedStatus, error) {
	unlock, err := lockFile(ctx, d.cfg.SpawnLock)
	if err != nil {
		return SharedStatus{}, sharedError(err)
	}
	defer unlock()
	var lastErr error
	for range 40 {
		hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		st, err := SharedHello(hctx, d.cfg.Socket)
		cancel()
		if err == nil {
			st, err = d.check(ctx, st)
			if !errors.Is(err, errRestart) {
				return st, err
			}
			continue
		}
		if ctx.Err() != nil {
			return SharedStatus{}, ctx.Err()
		}
		if !IsNoDaemon(err) {
			return SharedStatus{}, sharedError(err)
		}
		err = d.cfg.Spawn(ctx)
		if err == nil {
			continue
		}
		if !errors.Is(err, ErrDaemonRunning) {
			return SharedStatus{}, err
		}
		// A daemon that is exiting, or an in-process tunnel (server init),
		// holds the singleton lock; wait for it.
		lastErr = err
		select {
		case <-ctx.Done():
			return SharedStatus{}, ctx.Err()
		case <-time.After(d.cfg.Retry):
		}
	}
	return SharedStatus{}, output.WrapError(output.CodePrecondition, lastErr, output.ExitPrecondition,
		"Another eru process holds the tunnel lock without serving the shared tunnel (eru server init uses its own); wait for it, then retry.").
		WithDetails(map[string]any{"reason": "tunnel_busy"})
}

// check accepts a running daemon, or stops an idle one that serves another
// configuration or build and returns errRestart.
func (d *SharedDialer) check(ctx context.Context, st SharedStatus) (SharedStatus, error) {
	if st.Draining {
		return st, stoppingError(st)
	}
	if st.Fingerprint == d.cfg.Fingerprint && st.Build == d.cfg.Build {
		return st, nil
	}
	if st.Attached == 0 && st.Streams == 0 {
		if _, err := SharedStop(ctx, d.cfg.Socket, true); err != nil && !IsNoDaemon(err) {
			return st, sharedError(err)
		}
		if !SharedWaitGone(ctx, d.cfg.Socket, 10*time.Second) {
			return st, sharedError(fmt.Errorf("the tunnel daemon (pid %d) did not exit after a stop", st.PID))
		}
		return st, errRestart
	}
	if st.Fingerprint == d.cfg.Fingerprint {
		// Another build, same tunnel: the dial protocol doesn't depend on
		// the build, so use it until it goes idle.
		return st, nil
	}
	return st, output.NewError(output.CodeTransportFailed,
		"the running tunnel daemon serves another configuration of this context", output.ExitTransport,
		"Another eru command ("+describeClients(st.Clients)+") is using the old tunnel; wait for it, or run eru tunnel stop --force.").
		WithDetails(map[string]any{"reason": "tunnel_config_changed", "daemon_pid": st.PID})
}

func stoppingError(st SharedStatus) error {
	return output.NewError(output.CodePrecondition, "the tunnel daemon is stopping and takes no new commands", output.ExitPrecondition,
		"It exits once "+describeClients(st.Clients)+" finishes; wait, or run eru tunnel stop --force.").
		WithDetails(map[string]any{"reason": "tunnel_stopping", "daemon_pid": st.PID})
}

func describeClients(clients []SharedClient) string {
	if len(clients) == 0 {
		return "its last stream"
	}
	parts := make([]string, 0, len(clients))
	for _, c := range clients {
		parts = append(parts, fmt.Sprintf("pid %d, %s", c.PID, c.Command))
	}
	return strings.Join(parts, "; ")
}

func sharedError(err error) error {
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		return err
	}
	return output.WrapError(output.CodeTransportFailed, fmt.Errorf("shared tunnel: %w", err), output.ExitTransport,
		"See tunnel.log in the context directory; eru tunnel stop --force restarts the tunnel daemon.")
}

// DialBudget is how long a dial the daemon makes may take, including the
// WireGuard re-handshake after the tunnel has idled. DialSSH gives a
// shared-tunnel dial this long instead of its own shorter timeout.
func (d *SharedDialer) DialBudget() time.Duration { return sharedDialBudget }

// DialContext dials addr inside the tunnel through the daemon. When the
// daemon is gone it starts a new one and dials again. The wait for the
// daemon's reply allows for a WireGuard handshake (sharedDialBudget), not the
// caller's shorter timeout: the daemon owns the tunnel and knows how long a
// handshake takes.
func (d *SharedDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, fmt.Errorf("the shared tunnel dials tcp, not %s", network)
	}
	for attempt := 0; ; attempt++ {
		if err := d.Attach(ctx); err != nil {
			return nil, err
		}
		conn, reply, err := roundTripWithin(ctx, d.cfg.Socket, sharedRequest{Op: "dial", Addr: addr}, sharedDialBudget)
		if err != nil {
			if IsNoDaemon(err) && attempt == 0 {
				d.dropLease()
				continue
			}
			return nil, err
		}
		if !reply.OK {
			_ = conn.Close()
			return nil, &TunnelDialError{Addr: addr, Message: reply.Message}
		}
		return conn, nil
	}
}

func (d *SharedDialer) dropLease() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lease != nil {
		_ = d.lease.Close()
		d.lease = nil
	}
}

// Close releases the lease. The daemon keeps running for its persist
// window.
func (d *SharedDialer) Close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	if d.lease != nil {
		_ = d.lease.Close()
		d.lease = nil
	}
}

// lockFile takes an exclusive flock on path, waiting until ctx ends.
func lockFile(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: a lock file in the context directory.
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return func() {
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
