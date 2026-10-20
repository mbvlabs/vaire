//go:build unix

package cli

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
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/spf13/cobra"
)

// The context's tunnel daemon: vaire tunnel serve owns the only WireGuard
// device for the context and every vaire process dials through its unix socket
// (internal/transport/shared.go). The first command that needs the node
// spawns it; it exits after DefaultTunnelPersist with no command using it.

const (
	// tunnelDaemonCommand is how the daemon names itself in tunnel.lock.
	tunnelDaemonCommand = "vaire tunnel serve"
	// TunnelSocketFile, TunnelSpawnLockFile and TunnelLogFile live in the
	// context directory.
	TunnelSocketFile    = "tunnel.sock"
	TunnelSpawnLockFile = "tunnel.spawn.lock"
	TunnelLogFile       = "tunnel.log"
	// DefaultTunnelPersist is how long an unused daemon stays up.
	DefaultTunnelPersist = 10 * time.Minute
	// envTunnel selects the tunnel: "inprocess" runs one in this process
	// instead of using the daemon.
	envTunnel        = "VAIRE_TUNNEL"
	envTunnelPersist = "VAIRE_TUNNEL_PERSIST"
	// envTunnelReadyFD tells the daemon its readiness pipe's descriptor.
	envTunnelReadyFD = "VAIRE_TUNNEL_READY_FD"
	// maxSocketPath leaves room under sun_path (104 bytes on macOS, 108 on
	// Linux).
	maxSocketPath = 100
)

// openDialer connects the session to the tunnel: the context's shared
// daemon, or an in-process tunnel with VAIRE_TUNNEL=inprocess.
func (s *session) openDialer(ctx context.Context, cmd *cobra.Command, cfg transport.WireGuardConfig, p *progress) error {
	if os.Getenv(envTunnel) == "inprocess" {
		node, _ := s.ctx.Master()
		tun, err := upTunnel(ctx, s.ctx.Name, cfg.PrivateKey, s.ctx.Operator.WGAddress, node.WGPublicKey, node.PublicAddress, node.ListenPort, node.WGAddress, p)
		if err != nil {
			return err
		}
		s.dialer, s.closeDialer = tun, tun.Close
		return nil
	}
	d, err := sharedDialer(s.ctx.Name, cfg, cmd.CommandPath())
	if err != nil {
		return err
	}
	if err := d.Attach(ctx); err != nil {
		return err
	}
	if p != nil && p.opts.Verbose {
		p.note(fmt.Sprintf("tunnel daemon pid %d; its log is %s", d.DaemonPID(), contexts.Path(s.ctx.Name, TunnelLogFile)))
	}
	s.dialer, s.closeDialer = d, d.Close
	return nil
}

// sharedDialer is a client of the context's tunnel daemon.
func sharedDialer(name string, cfg transport.WireGuardConfig, command string) (*transport.SharedDialer, error) {
	socket, err := tunnelSocketPath(name)
	if err != nil {
		return nil, err
	}
	return transport.NewSharedDialer(transport.SharedDialerConfig{
		Socket:      socket,
		SpawnLock:   contexts.Path(name, TunnelSpawnLockFile),
		Fingerprint: transport.TunnelFingerprint(cfg),
		Build:       tunnelBuild(),
		PID:         os.Getpid(),
		Command:     command,
		Spawn:       func(ctx context.Context) error { return spawnTunnelDaemon(ctx, name) },
	}), nil
}

// tunnelBuild identifies this binary: its version and commit, plus the
// executable's size and modification time, so a rebuilt or upgraded binary
// replaces an idle daemon of the old one.
func tunnelBuild() string {
	build := buildinfo.Version + "/" + buildinfo.Commit
	if exe, err := os.Executable(); err == nil {
		if info, err := os.Stat(exe); err == nil {
			build += fmt.Sprintf("/%d/%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	return build
}

// tunnelSocketPath is the daemon's socket: tunnel.sock in the context
// directory, or, when that path is too long for a unix socket, a file named
// by a hash of the context directory in a private runtime directory.
func tunnelSocketPath(name string) (string, error) {
	path := contexts.Path(name, TunnelSocketFile)
	if len(path) <= maxSocketPath {
		return path, nil
	}
	sum := sha256.Sum256([]byte(contexts.Dir(name)))
	dir := filepath.Join(os.TempDir(), "vaire-"+strconv.Itoa(os.Getuid()))
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); runtime.GOOS == "linux" && xdg != "" {
		dir = filepath.Join(xdg, "vaire")
	}
	if err := privateDir(dir); err != nil {
		return "", output.WrapError(output.CodePrecondition, err, output.ExitPrecondition,
			"The context directory's path is too long for a unix socket, and the fallback directory is unsafe; use a shorter XDG_CONFIG_HOME.")
	}
	path = filepath.Join(dir, hex.EncodeToString(sum[:8])+".sock")
	if len(path) > maxSocketPath {
		return "", output.NewError(output.CodePrecondition, "no unix socket path short enough for the tunnel daemon: "+path, output.ExitPrecondition,
			"Use a shorter XDG_CONFIG_HOME or TMPDIR.")
	}
	return path, nil
}

// privateDir creates dir 0700, or checks that an existing one is a real
// directory owned by this user with no access for others.
func privateDir(dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil { //nolint:gosec // G703: the user's runtime or temp directory.
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) { //nolint:gosec // G703: checked below.
		return err
	}
	info, err := os.Lstat(dir) //nolint:gosec // G703: checked right here.
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to uid %d", dir, st.Uid)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %04o, want 0700", dir, info.Mode().Perm())
	}
	return nil
}

// tunnelPersist is how long an unused daemon stays up: VAIRE_TUNNEL_PERSIST,
// else the context's tunnel_persist, else DefaultTunnelPersist.
func tunnelPersist(c *contexts.Context) (time.Duration, error) {
	for _, v := range []string{os.Getenv(envTunnelPersist), c.TunnelPersist} {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < 0 {
			return DefaultTunnelPersist, fmt.Errorf("tunnel persist %q is not a duration like 10m", v)
		}
		return d, nil
	}
	return DefaultTunnelPersist, nil
}

// daemonReady is the line the daemon writes on its readiness pipe.
type daemonReady struct {
	OK      bool   `json:"ok"`
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
	Hint    string `json:"hint,omitempty"`
}

// spawnTunnelDaemon starts vaire tunnel serve for the context, detached in
// its own session, and waits for it to report on the readiness pipe.
func spawnTunnelDaemon(ctx context.Context, name string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// The daemon truncates the log once it holds the singleton lock, so a
	// spawn that loses the race doesn't wipe a running daemon's log.
	logPath := contexts.Path(name, TunnelLogFile)
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the context's own log.
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(exe, "tunnel", "serve", "--context", name) //nolint:gosec // G204: re-runs this binary.
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(), envTunnelReadyFD+"=3")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return err
	}
	_ = w.Close()
	// Reap the daemon if it exits while this process still runs.
	go func() { _ = cmd.Wait() }()

	type result struct {
		line []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := io.ReadAll(io.LimitReader(r, 64<<10))
		got <- result{line, err}
	}()
	var res result
	select {
	case res = <-got:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(60 * time.Second):
		return output.NewError(output.CodeTransportFailed, "the tunnel daemon did not start within 60s", output.ExitTransport,
			"See "+logPath+".")
	}
	var ready daemonReady
	if len(strings.TrimSpace(string(res.line))) == 0 || json.Unmarshal(res.line, &ready) != nil {
		return output.NewError(output.CodeTransportFailed, "the tunnel daemon exited during startup", output.ExitTransport,
			"See "+logPath+".").WithDetails(map[string]any{"log": logPath})
	}
	if ready.OK {
		return nil
	}
	if ready.Code == "already_running" {
		return transport.ErrDaemonRunning
	}
	code := ready.Code
	if !output.IsKnownCode(code) {
		code = output.CodeTransportFailed
	}
	return output.NewError(code, ready.Message, output.ExitFor(code), ready.Hint)
}

// runTunnelServe is the daemon: it takes the context's tunnel lock, brings
// up the tunnel, serves the socket until idle or stopped, then tears down.
func runTunnelServe(cmd *cobra.Command, _ []string) error {
	ready := func(daemonReady) {}
	if fd, err := strconv.Atoi(os.Getenv(envTunnelReadyFD)); err == nil && fd > 2 {
		pipe := os.NewFile(uintptr(fd), "ready")
		_ = os.Unsetenv(envTunnelReadyFD)
		ready = func(r daemonReady) {
			raw, _ := json.Marshal(r)
			_, _ = pipe.Write(append(raw, '\n'))
			_ = pipe.Close()
			ready = func(daemonReady) {}
		}
	}
	fail := func(err error) error {
		r := daemonReady{Code: output.CodeTransportFailed, Message: err.Error()}
		var cliErr *output.CLIError
		if errors.As(err, &cliErr) {
			r.Code, r.Hint = cliErr.Code, cliErr.Hint
		}
		ready(r)
		logLine("startup failed: %s", r.Message)
		return err
	}
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return fail(err)
	}
	release, held, err := tryTunnelLock(c.Name, tunnelDaemonCommand)
	if err != nil {
		return fail(err)
	}
	if !held {
		ready(daemonReady{Code: "already_running", Message: "a tunnel daemon is already running for " + c.Name})
		return nil
	}
	defer release()
	_ = os.Truncate(contexts.Path(c.Name, TunnelLogFile), 0)
	logLine("tunnel daemon for %s starting (pid %d, build %s)", c.Name, os.Getpid(), tunnelBuild())

	node, ok := c.Master()
	if !ok {
		return fail(output.NewError(output.CodeConfig, "context "+c.Name+" lists no node", output.ExitConfig, "Recreate it with vaire login."))
	}
	cfg, err := contextTunnelConfig(c, node)
	if err != nil {
		return fail(err)
	}
	persist, perr := tunnelPersist(c)
	if perr != nil {
		logLine("%v; using %s", perr, persist)
	}
	socket, err := tunnelSocketPath(c.Name)
	if err != nil {
		return fail(err)
	}
	// Only the lock holder may remove the socket: one left by a daemon that
	// died is stale.
	_ = os.Remove(socket)
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		return fail(output.WrapError(output.CodeTransportFailed, fmt.Errorf("listen on %s: %w", socket, err), output.ExitTransport, ""))
	}
	defer func() { _ = os.Remove(socket) }()
	_ = os.Chmod(socket, 0o600)

	cfg.Logf = func(format string, args ...any) { logLine("wireguard: "+format, args...) }
	tun, err := transport.UpWireGuard(cfg)
	if err != nil {
		_ = ln.Close()
		return fail(err)
	}
	defer tun.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	srv := transport.NewSharedServer(transport.SharedServerConfig{
		Listener:      ln,
		Dialer:        tun,
		Allowed:       cfg.AllowedIPs,
		Fingerprint:   transport.TunnelFingerprint(cfg),
		Build:         tunnelBuild(),
		Persist:       persist,
		DialTimeout:   transport.SharedDialTimeout,
		LastHandshake: tun.LastHandshake,
		Logf:          logLine,
	})
	logLine("serving %s; exits after %s with no client", socket, persist)
	ready(daemonReady{OK: true})
	reason := srv.Serve(ctx)
	logLine("exiting (%s)", reason)
	return nil
}

// logLine writes one timestamped, redacted line to the daemon's log.
func logLine(format string, args ...any) {
	line := secrets.Redact(fmt.Sprintf(format, args...))
	_, _ = fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}

// tunnelStatusData is vaire tunnel status's result.
type tunnelStatusData struct {
	Context string `json:"context"`
	Running bool   `json:"running"`
	Socket  string `json:"socket"`
	Log     string `json:"log"`
	*transport.SharedStatus
}

func runTunnelStatus(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	socket, err := tunnelSocketPath(c.Name)
	if err != nil {
		return err
	}
	data := tunnelStatusData{Context: c.Name, Socket: socket, Log: contexts.Path(c.Name, TunnelLogFile)}
	ctx, cancel := context.WithTimeout(commandContext(cmd), timeoutFlag(cmd))
	defer cancel()
	st, err := transport.SharedHello(ctx, socket)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return output.Success(cmd, output.Result{Data: data, Message: "no tunnel daemon runs for " + c.Name + "; the next command starts one"})
		}
		return output.WrapError(output.CodeTransportFailed, fmt.Errorf("ask the tunnel daemon of %s: %w", c.Name, err), output.ExitTransport,
			"See "+data.Log+".")
	}
	data.Running = true
	data.SharedStatus = &st
	return output.Success(cmd, output.Result{Data: data, Message: tunnelStatusMessage(data)})
}

func tunnelStatusMessage(d tunnelStatusData) string {
	st := d.SharedStatus
	lines := []string{fmt.Sprintf("tunnel daemon for %s: pid %d, up since %s", d.Context, st.PID, st.StartedAt)}
	if st.LastHandshake != "" {
		lines = append(lines, "last WireGuard handshake: "+st.LastHandshake)
	} else {
		lines = append(lines, "last WireGuard handshake: none yet")
	}
	lines = append(lines, fmt.Sprintf("%d attached, %d open streams", st.Attached, st.Streams))
	for _, c := range st.Clients {
		lines = append(lines, fmt.Sprintf("  pid %-7d %s (since %s)", c.PID, c.Command, c.Since))
	}
	switch {
	case st.Draining:
		lines = append(lines, "stopping: exits once its clients finish")
	case st.IdleDeadline != "":
		lines = append(lines, "idle; exits at "+st.IdleDeadline+" unless a command uses it")
	}
	lines = append(lines, "build "+st.Build, "log "+d.Log)
	return strings.Join(lines, "\n")
}

func runTunnelStop(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	force := flagTrue(cmd, "force")
	ctx, cancel := context.WithTimeout(commandContext(cmd), timeoutFlag(cmd))
	defer cancel()
	pid, draining, err := stopTunnelDaemon(ctx, c.Name, force)
	if err != nil {
		return output.WrapError(output.CodeTransportFailed, fmt.Errorf("stop the tunnel daemon of %s: %w", c.Name, err), output.ExitTransport,
			"See "+contexts.Path(c.Name, TunnelLogFile)+".")
	}
	data := map[string]any{"context": c.Name, "pid": pid, "stopped": pid != 0 && !draining, "draining": draining}
	switch {
	case pid == 0:
		return output.Success(cmd, output.Result{Data: data, Message: "no tunnel daemon runs for " + c.Name})
	case draining:
		return output.Success(cmd, output.Result{Data: data,
			Message: fmt.Sprintf("the tunnel daemon of %s (pid %d) takes no new commands and exits once the running ones finish", c.Name, pid),
			Next:    []string{"vaire tunnel status", "vaire tunnel stop --force"}})
	}
	return output.Success(cmd, output.Result{Data: data, Message: fmt.Sprintf("stopped the tunnel daemon of %s (pid %d)", c.Name, pid)})
}

// stopTunnelDaemon asks the context's daemon to exit and, unless it drains,
// waits for it to go. pid is 0 when none ran.
func stopTunnelDaemon(ctx context.Context, name string, force bool) (pid int, draining bool, err error) {
	socket, err := tunnelSocketPath(name)
	if err != nil {
		return 0, false, err
	}
	st, err := transport.SharedHello(ctx, socket)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	draining, err = transport.SharedStop(ctx, socket, force)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return st.PID, false, nil
		}
		return st.PID, false, err
	}
	if !draining && !transport.SharedWaitGone(ctx, socket, 10*time.Second) {
		return st.PID, false, fmt.Errorf("the tunnel daemon (pid %d) did not exit", st.PID)
	}
	return st.PID, draining, nil
}

// stopTunnelDaemonQuietly stops a context's daemon before something that
// would leave it stale (removing the context, an in-process tunnel), and
// ignores a failure: the daemon then exits on its own once idle.
func stopTunnelDaemonQuietly(name string, force bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_, _, _ = stopTunnelDaemon(ctx, name, force)
}

// stopIdleTunnelDaemons stops every context's daemon that no command uses,
// after the vaire binary changed.
func stopIdleTunnelDaemons() {
	names, err := contexts.List()
	if err != nil {
		return
	}
	for _, name := range names {
		socket, err := tunnelSocketPath(name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if st, err := transport.SharedHello(ctx, socket); err == nil && st.Attached == 0 && st.Streams == 0 {
			_, _ = transport.SharedStop(ctx, socket, true)
		}
		cancel()
	}
}
