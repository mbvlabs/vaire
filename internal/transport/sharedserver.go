//go:build unix

package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// SharedServerConfig configures the shared-tunnel daemon's socket server.
type SharedServerConfig struct {
	// Listener is the unix socket the daemon serves.
	Listener net.Listener
	// Dialer is the tunnel the daemon owns.
	Dialer Dialer
	// Allowed are the prefixes a client may dial into: the node /24.
	Allowed []netip.Prefix
	// Fingerprint identifies the tunnel's configuration (TunnelFingerprint).
	Fingerprint string
	// Build is the daemon's vaire build, shown by vaire tunnel status.
	Build string
	// Persist is how long the daemon stays up with no lease and no stream.
	// Zero exits as soon as the last one ends.
	Persist time.Duration
	// StartGrace keeps a new daemon up until its first client attaches,
	// whatever Persist is. Zero means DefaultSharedStartGrace.
	StartGrace time.Duration
	// DialTimeout bounds one dial into the tunnel. Zero means
	// DefaultTimeout.
	DialTimeout time.Duration
	// LastHandshake reports the tunnel's last WireGuard handshake, for
	// hello. Nil leaves it out.
	LastHandshake func() time.Time
	// CheckPeer accepts or rejects a client connection. Nil checks that
	// the peer runs as this process's uid.
	CheckPeer func(net.Conn) error
	// Logf receives one line per notable event. Nil discards them.
	Logf func(format string, args ...any)
}

// DefaultSharedStartGrace is how long a new daemon waits for its first
// client before the idle rule applies.
const DefaultSharedStartGrace = 30 * time.Second

// SharedServer serves the shared-tunnel protocol on a unix socket: each
// client action is one connection, one JSON request line and one JSON reply
// line, after which a dial carries raw bytes.
type SharedServer struct {
	cfg     SharedServerConfig
	started time.Time
	pid     int

	mu       sync.Mutex
	leases   map[net.Conn]SharedClient
	streams  int
	conns    map[net.Conn]struct{}
	draining bool
	idleGen  uint64
	idleAt   time.Time
	idle     *time.Timer
	done     chan struct{}
	doneOnce sync.Once
	reason   string
}

// NewSharedServer returns a server for cfg. Serve runs it.
func NewSharedServer(cfg SharedServerConfig) *SharedServer {
	if cfg.StartGrace <= 0 {
		cfg.StartGrace = DefaultSharedStartGrace
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = DefaultTimeout
	}
	if cfg.CheckPeer == nil {
		cfg.CheckPeer = checkPeerUID
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &SharedServer{
		cfg:     cfg,
		started: time.Now(),
		pid:     os.Getpid(),
		leases:  map[net.Conn]SharedClient{},
		conns:   map[net.Conn]struct{}{},
		done:    make(chan struct{}),
	}
}

// Serve accepts clients until the daemon goes idle, a stop arrives, or ctx
// ends. It closes the listener and every client connection before it
// returns, and reports why it stopped: "idle", "stopped" or "signal".
func (s *SharedServer) Serve(ctx context.Context) string {
	s.mu.Lock()
	s.armIdleLocked(s.cfg.StartGrace)
	s.mu.Unlock()
	go func() {
		for {
			conn, err := s.cfg.Listener.Accept()
			if err != nil {
				s.finish("stopped")
				return
			}
			go s.handle(ctx, conn)
		}
	}()
	select {
	case <-ctx.Done():
		s.finish("signal")
	case <-s.done:
	}
	_ = s.cfg.Listener.Close()
	s.mu.Lock()
	if s.idle != nil {
		s.idle.Stop()
	}
	for c := range s.conns {
		_ = c.Close()
	}
	reason := s.reason
	s.mu.Unlock()
	return reason
}

func (s *SharedServer) finish(reason string) {
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.reason = reason
		s.mu.Unlock()
		close(s.done)
	})
}

// armIdleLocked starts the idle countdown when nothing holds the daemon.
func (s *SharedServer) armIdleLocked(after time.Duration) {
	s.idleGen++
	if s.idle != nil {
		s.idle.Stop()
		s.idle = nil
	}
	if len(s.leases) > 0 || s.streams > 0 {
		s.idleAt = time.Time{}
		return
	}
	gen := s.idleGen
	s.idleAt = time.Now().Add(after)
	s.idle = time.AfterFunc(after, func() {
		s.mu.Lock()
		still := gen == s.idleGen && len(s.leases) == 0 && s.streams == 0
		s.mu.Unlock()
		if still {
			s.cfg.Logf("idle for %s with no client; exiting", after)
			s.finish("idle")
		}
	})
}

// activityChangedLocked re-arms the idle countdown after a lease or stream
// came or went.
func (s *SharedServer) activityChangedLocked() {
	if s.draining && len(s.leases) == 0 && s.streams == 0 {
		go s.finish("stopped")
		return
	}
	s.armIdleLocked(s.cfg.Persist)
}

func (s *SharedServer) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
		return false
	default:
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *SharedServer) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *SharedServer) handle(ctx context.Context, conn net.Conn) {
	if !s.track(conn) {
		_ = conn.Close()
		return
	}
	defer s.untrack(conn)
	defer func() { _ = conn.Close() }()
	if err := s.cfg.CheckPeer(conn); err != nil {
		s.cfg.Logf("refused a client: %v", err)
		return
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(conn)
	if err != nil {
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req sharedRequest
	if err := json.Unmarshal(line, &req); err != nil {
		_ = writeReply(conn, sharedReply{Code: "bad_request", Message: "the request is not JSON"})
		return
	}
	switch req.Op {
	case "hello":
		_ = writeReply(conn, s.hello())
	case "attach":
		s.attach(conn, req)
	case "dial":
		s.dial(ctx, conn, req)
	case "stop":
		s.stop(conn, req)
	default:
		_ = writeReply(conn, sharedReply{Code: "bad_request", Message: "unknown op " + req.Op})
	}
}

func (s *SharedServer) hello() sharedReply {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := sharedReply{
		OK:          true,
		Proto:       SharedProto,
		Build:       s.cfg.Build,
		PID:         s.pid,
		Fingerprint: s.cfg.Fingerprint,
		Attached:    len(s.leases),
		Streams:     s.streams,
		Draining:    s.draining,
		StartedAt:   s.started.UTC().Format(time.RFC3339),
	}
	for _, c := range s.leases {
		r.Clients = append(r.Clients, c)
	}
	if !s.idleAt.IsZero() {
		r.IdleDeadline = s.idleAt.UTC().Format(time.RFC3339)
	}
	if s.cfg.LastHandshake != nil {
		if t := s.cfg.LastHandshake(); !t.IsZero() {
			r.LastHandshake = t.UTC().Format(time.RFC3339)
		}
	}
	return r
}

// attach holds a lease for the connection's lifetime.
func (s *SharedServer) attach(conn net.Conn, req sharedRequest) {
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		_ = writeReply(conn, sharedReply{Code: "draining", Message: "the tunnel daemon is stopping"})
		return
	}
	s.leases[conn] = SharedClient{PID: req.PID, Command: req.Cmd, Since: time.Now().UTC().Format(time.RFC3339)}
	s.activityChangedLocked()
	s.mu.Unlock()
	s.cfg.Logf("client attached: pid %d (%s)", req.PID, req.Cmd)
	defer func() {
		s.mu.Lock()
		delete(s.leases, conn)
		s.activityChangedLocked()
		s.mu.Unlock()
		s.cfg.Logf("client detached: pid %d (%s)", req.PID, req.Cmd)
	}()
	if err := writeReply(conn, sharedReply{OK: true}); err != nil {
		return
	}
	// The client never writes on a lease; a read returns when it closes
	// or exits.
	_, _ = io.Copy(io.Discard, conn)
}

// dial connects into the tunnel and splices the client's connection to it.
func (s *SharedServer) dial(ctx context.Context, conn net.Conn, req sharedRequest) {
	if err := s.allowed(req.Addr); err != nil {
		_ = writeReply(conn, sharedReply{Code: "dial_refused", Message: err.Error()})
		return
	}
	s.mu.Lock()
	s.streams++
	s.activityChangedLocked()
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.streams--
		s.activityChangedLocked()
		s.mu.Unlock()
	}()
	dialCtx, cancel := context.WithTimeout(ctx, s.cfg.DialTimeout)
	upstream, err := s.cfg.Dialer.DialContext(dialCtx, "tcp", req.Addr)
	cancel()
	if err != nil {
		_ = writeReply(conn, sharedReply{Code: "dial_failed", Message: err.Error()})
		return
	}
	s.mu.Lock()
	s.conns[upstream] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, upstream)
		s.mu.Unlock()
		_ = upstream.Close()
	}()
	if err := writeReply(conn, sharedReply{OK: true}); err != nil {
		return
	}
	splice(conn, upstream)
}

// splice copies both ways until both directions end, passing a half-close
// on when the connection supports it.
func splice(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	pipe := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			if cw.CloseWrite() == nil {
				return
			}
		}
		_ = dst.Close()
		_ = src.Close()
	}
	go pipe(a, b)
	go pipe(b, a)
	wg.Wait()
}

func (s *SharedServer) allowed(addr string) error {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("%s is not an IP address and port", addr)
	}
	for _, p := range s.cfg.Allowed {
		if p.Contains(ap.Addr()) {
			return nil
		}
	}
	return fmt.Errorf("%s is outside the tunnel's node network", addr)
}

func (s *SharedServer) stop(conn net.Conn, req sharedRequest) {
	s.mu.Lock()
	s.draining = true
	idle := len(s.leases) == 0 && s.streams == 0
	s.mu.Unlock()
	_ = writeReply(conn, sharedReply{OK: true, Draining: !req.Force && !idle})
	if req.Force || idle {
		s.cfg.Logf("stop requested (force=%t); exiting", req.Force)
		s.finish("stopped")
		return
	}
	s.cfg.Logf("stop requested; refusing new clients and exiting once idle")
}

// checkPeerUID refuses a unix client that runs as another user.
func checkPeerUID(c net.Conn) error {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return errors.New("not a unix socket connection")
	}
	uid, err := peerUID(uc)
	if err != nil {
		return err
	}
	if int(uid) != os.Getuid() {
		return fmt.Errorf("peer uid %d is not %d", uid, os.Getuid())
	}
	return nil
}
