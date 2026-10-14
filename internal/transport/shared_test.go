//go:build unix

package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// echoServer accepts loopback TCP connections and echoes them, standing in
// for the node behind the tunnel.
func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// daemon is one fake tunnel daemon, run in-process the way eru tunnel serve
// runs: singleton lock, stale socket removed, listen, serve.
type daemon struct {
	srv    *SharedServer
	ln     *net.UnixListener
	cancel context.CancelFunc
	done   chan string
}

// daemons spawns fake daemons for one context.
type daemons struct {
	t           *testing.T
	socket      string
	spawnLock   string
	singleton   string
	persist     time.Duration
	fingerprint string
	build       string
	spawns      atomic.Int32

	mu  sync.Mutex
	all []*daemon
}

func newDaemons(t *testing.T) *daemons {
	t.Helper()
	// Unix socket paths are short; t.TempDir can be too long on macOS.
	dir, err := os.MkdirTemp("", "eru-shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	f := &daemons{
		t:           t,
		socket:      filepath.Join(dir, "tunnel.sock"),
		spawnLock:   filepath.Join(dir, "tunnel.spawn.lock"),
		singleton:   filepath.Join(dir, "tunnel.lock"),
		persist:     time.Minute,
		fingerprint: "fp-1",
		build:       "build-1",
	}
	t.Cleanup(func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for _, d := range f.all {
			d.cancel()
			<-d.done
		}
	})
	return f
}

func (f *daemons) spawn(context.Context) error {
	f.spawns.Add(1)
	lock, err := os.OpenFile(f.singleton, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return ErrDaemonRunning
	}
	_ = os.Remove(f.socket)
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		_ = lock.Close()
		return err
	}
	srv := NewSharedServer(SharedServerConfig{
		Listener:    ln,
		Dialer:      Direct{},
		Allowed:     []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		Fingerprint: f.fingerprint,
		Build:       f.build,
		Persist:     f.persist,
		StartGrace:  f.persist,
	})
	ctx, cancel := context.WithCancel(context.Background())
	d := &daemon{srv: srv, ln: ln.(*net.UnixListener), cancel: cancel, done: make(chan string, 1)}
	go func() {
		reason := srv.Serve(ctx)
		_ = lock.Close() // releases the singleton lock, as process exit does
		d.done <- reason
		close(d.done)
	}()
	f.mu.Lock()
	f.all = append(f.all, d)
	f.mu.Unlock()
	return nil
}

func (f *daemons) last() *daemon {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.all) == 0 {
		f.t.Fatal("no daemon was spawned")
	}
	return f.all[len(f.all)-1]
}

// kill stops the last daemon like kill -9: the socket file stays behind.
func (f *daemons) kill() {
	d := f.last()
	d.ln.SetUnlinkOnClose(false)
	d.cancel()
	<-d.done
}

func (f *daemons) dialer(cmd string) *SharedDialer {
	return NewSharedDialer(SharedDialerConfig{
		Socket:      f.socket,
		SpawnLock:   f.spawnLock,
		Fingerprint: f.fingerprint,
		Build:       f.build,
		PID:         os.Getpid(),
		Command:     cmd,
		Spawn:       f.spawn,
		Retry:       10 * time.Millisecond,
	})
}

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func echoOnce(t *testing.T, d Dialer, addr, msg string) {
	t.Helper()
	conn, err := d.DialContext(ctxT(t), "tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != msg {
		t.Fatalf("echo = %q, want %q", buf, msg)
	}
}

func waitDone(t *testing.T, d *daemon, within time.Duration) string {
	t.Helper()
	select {
	case reason := <-d.done:
		return reason
	case <-time.After(within):
		t.Fatalf("the daemon did not exit within %s", within)
		return ""
	}
}

func TestSharedStartsDaemonAndDials(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	d := f.dialer("eru test")
	defer d.Close()
	echoOnce(t, d, addr, "hello through the daemon")
	echoOnce(t, d, addr, "and again")
	if n := f.spawns.Load(); n != 1 {
		t.Fatalf("spawned %d daemons, want 1", n)
	}
	st, err := SharedHello(ctxT(t), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attached != 1 || len(st.Clients) != 1 || st.Clients[0].Command != "eru test" || st.PID != os.Getpid() {
		t.Fatalf("hello = %+v, want one attached client", st)
	}
}

func TestSharedReusesRunningDaemon(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	first := f.dialer("eru first")
	defer first.Close()
	echoOnce(t, first, addr, "one")
	second := f.dialer("eru second")
	defer second.Close()
	echoOnce(t, second, addr, "two")
	if n := f.spawns.Load(); n != 1 {
		t.Fatalf("spawned %d daemons, want 1", n)
	}
	st, err := SharedHello(ctxT(t), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attached != 2 {
		t.Fatalf("attached = %d, want 2", st.Attached)
	}
}

func TestSharedRefusesAddressOutsidePrefix(t *testing.T) {
	f := newDaemons(t)
	d := f.dialer("eru test")
	defer d.Close()
	_, err := d.DialContext(ctxT(t), "tcp", "10.9.9.9:22")
	if err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("err = %v, want a refusal outside the prefix", err)
	}
	_, err = d.DialContext(ctxT(t), "tcp", "localhost:22")
	if err == nil || !strings.Contains(err.Error(), "not an IP") {
		t.Fatalf("err = %v, want a refusal of a hostname", err)
	}
}

func TestSharedIdleExit(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	f.persist = 50 * time.Millisecond
	d := f.dialer("eru test")
	echoOnce(t, d, addr, "x")
	d.Close()
	if reason := waitDone(t, f.last(), 2*time.Second); reason != "idle" {
		t.Fatalf("exit reason = %q, want idle", reason)
	}
	if _, err := os.Stat(f.socket); !os.IsNotExist(err) {
		t.Fatalf("socket still exists after an idle exit: %v", err)
	}
}

func TestSharedLeaseKeepsDaemonAlive(t *testing.T) {
	f := newDaemons(t)
	f.persist = 30 * time.Millisecond
	d := f.dialer("eru tunnel")
	if err := d.Attach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.last().done:
		t.Fatal("the daemon exited while a lease was held")
	case <-time.After(300 * time.Millisecond):
	}
	d.Close()
	waitDone(t, f.last(), 2*time.Second)
}

func TestSharedOpenStreamKeepsDaemonAlive(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	f.persist = 30 * time.Millisecond
	d := f.dialer("eru test")
	conn, err := d.DialContext(ctxT(t), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	d.Close() // the lease is gone; the stream still counts
	select {
	case <-f.last().done:
		t.Fatal("the daemon exited with a stream open")
	case <-time.After(300 * time.Millisecond):
	}
	_ = conn.Close()
	waitDone(t, f.last(), 2*time.Second)
}

func TestSharedRecoversFromStaleSocket(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	d := f.dialer("eru test")
	defer d.Close()
	echoOnce(t, d, addr, "before")
	f.kill()
	if _, err := os.Stat(f.socket); err != nil {
		t.Fatalf("the killed daemon's socket should be left behind: %v", err)
	}
	// The next dial finds a socket nobody serves, spawns a new daemon and
	// re-attaches.
	echoOnce(t, d, addr, "after")
	if n := f.spawns.Load(); n != 2 {
		t.Fatalf("spawned %d daemons, want 2", n)
	}
	st, err := SharedHello(ctxT(t), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attached != 1 {
		t.Fatalf("attached = %d after respawn, want 1", st.Attached)
	}
}

func TestSharedConcurrentClientsSpawnOneDaemon(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	const n = 12
	dialers := make([]*SharedDialer, n)
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		dialers[i] = f.dialer("eru parallel")
		wg.Add(1)
		go func(d *SharedDialer) {
			defer wg.Done()
			conn, err := d.DialContext(ctxT(t), "tcp", addr)
			if err != nil {
				errs <- err
				return
			}
			_, _ = conn.Write([]byte("ping"))
			buf := make([]byte, 4)
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := io.ReadFull(conn, buf); err != nil {
				errs <- err
			}
			_ = conn.Close()
		}(dialers[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if got := f.spawns.Load(); got != 1 {
		t.Fatalf("spawned %d daemons, want exactly 1", got)
	}
	st, err := SharedHello(ctxT(t), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Attached != n {
		t.Fatalf("attached = %d, want %d", st.Attached, n)
	}
	for _, d := range dialers {
		d.Close()
	}
}

func TestSharedReplacesIdleStaleDaemon(t *testing.T) {
	f := newDaemons(t)
	if err := f.spawn(context.Background()); err != nil {
		t.Fatal(err)
	}
	old := f.last()
	f.fingerprint = "fp-2" // a re-login changed the context
	d := f.dialer("eru test")
	defer d.Close()
	if err := d.Attach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if reason := waitDone(t, old, 2*time.Second); reason != "stopped" {
		t.Fatalf("old daemon exit = %q, want stopped", reason)
	}
	st, err := SharedHello(ctxT(t), f.socket)
	if err != nil {
		t.Fatal(err)
	}
	if st.Fingerprint != "fp-2" {
		t.Fatalf("fingerprint = %q, want the new one", st.Fingerprint)
	}
}

func TestSharedBusyStaleDaemonIsNotKilled(t *testing.T) {
	f := newDaemons(t)
	busy := f.dialer("eru restore")
	defer busy.Close()
	if err := busy.Attach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	f.fingerprint = "fp-2"
	d := f.dialer("eru plan")
	defer d.Close()
	err := d.Attach(ctxT(t))
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Details.(map[string]any)["reason"] != "tunnel_config_changed" {
		t.Fatalf("err = %v, want tunnel_config_changed", err)
	}
	if !strings.Contains(cliErr.Hint, "eru restore") {
		t.Fatalf("hint %q should name the busy command", cliErr.Hint)
	}
	// Another build with the same configuration is used as it is.
	f.fingerprint, f.build = "fp-1", "build-2"
	other := f.dialer("eru plan")
	defer other.Close()
	if err := other.Attach(ctxT(t)); err != nil {
		t.Fatalf("a busy daemon of another build should serve: %v", err)
	}
	if n := f.spawns.Load(); n != 1 {
		t.Fatalf("spawned %d daemons, want 1", n)
	}
}

func TestSharedStopDrainsThenExits(t *testing.T) {
	f := newDaemons(t)
	d := f.dialer("eru restore")
	if err := d.Attach(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	draining, err := SharedStop(ctxT(t), f.socket, false)
	if err != nil || !draining {
		t.Fatalf("stop = %v, %v; want draining", draining, err)
	}
	late := f.dialer("eru plan")
	defer late.Close()
	err = late.Attach(ctxT(t))
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Details.(map[string]any)["reason"] != "tunnel_stopping" {
		t.Fatalf("err = %v, want tunnel_stopping", err)
	}
	d.Close()
	if reason := waitDone(t, f.last(), 2*time.Second); reason != "stopped" {
		t.Fatalf("exit = %q, want stopped", reason)
	}
}

func TestSharedForceStopDropsStreams(t *testing.T) {
	addr := echoServer(t)
	f := newDaemons(t)
	d := f.dialer("eru backup now")
	defer d.Close()
	conn, err := d.DialContext(ctxT(t), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := SharedStop(ctxT(t), f.socket, true); err != nil {
		t.Fatal(err)
	}
	waitDone(t, f.last(), 2*time.Second)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("a stream survived a forced stop")
	}
	// The client recovers on its next dial.
	echoOnce(t, d, addr, "re-attached")
}

func TestReadLineLeavesTheRest(t *testing.T) {
	a, b := net.Pipe()
	go func() { _, _ = a.Write([]byte("{\"ok\":true}\nRAW")); _ = a.Close() }()
	line, err := readLine(b)
	if err != nil || string(line) != `{"ok":true}` {
		t.Fatalf("line = %q, %v", line, err)
	}
	rest, _ := io.ReadAll(b)
	if string(rest) != "RAW" {
		t.Fatalf("rest = %q, want RAW", rest)
	}
}

func TestTunnelFingerprint(t *testing.T) {
	priv, _, err := GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}
	_, peer, _ := GenerateWGKey()
	cfg := WireGuardConfig{
		PrivateKey:    priv,
		Address:       netip.MustParseAddr("10.42.255.1"),
		PeerPublicKey: peer,
		Endpoint:      netip.MustParseAddrPort("203.0.113.5:51820"),
		AllowedIPs:    []netip.Prefix{netip.MustParsePrefix("10.42.0.0/24")},
	}
	a := TunnelFingerprint(cfg)
	if a != TunnelFingerprint(cfg) {
		t.Fatal("fingerprint is not stable")
	}
	if strings.Contains(a, priv) {
		t.Fatal("fingerprint leaks the private key")
	}
	cfg.Endpoint = netip.MustParseAddrPort("203.0.113.6:51820")
	if a == TunnelFingerprint(cfg) {
		t.Fatal("a new endpoint should change the fingerprint")
	}
}
