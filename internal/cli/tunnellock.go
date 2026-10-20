package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/contexts"
)

// One WireGuard device per context: the node keeps one peer per operator
// key, and a second device with the same key moves that peer's endpoint and
// session to itself, so the first one's streams hang. tunnel.lock in the
// context directory guards that. The context's tunnel daemon (tunneld.go)
// holds it for its lifetime and every other vaire process dials through the
// daemon. The in-process tunnels (vaire server init, VAIRE_TUNNEL=inprocess)
// take it too, so they wait while a daemon or another in-process tunnel
// runs.

// TunnelLockFile is the per-context tunnel lock.
const TunnelLockFile = "tunnel.lock"

const tunnelLockPoll = 250 * time.Millisecond

// tunnelHolder is written into tunnel.lock by the process holding it.
type tunnelHolder struct {
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	StartedAt string `json:"started_at"`
}

// tunnelLocks counts this process's holds per context, so a process that
// opens a second tunnel to the same context doesn't wait on itself.
var tunnelLocks = struct {
	sync.Mutex
	held map[string]*heldTunnelLock
}{held: map[string]*heldTunnelLock{}}

type heldTunnelLock struct {
	f    *os.File
	refs int
}

// lockTunnel takes the context's tunnel lock, waiting while another vaire
// process holds it. It reports the wait once through p and gives up when
// ctx ends. The returned func releases the lock.
func lockTunnel(ctx context.Context, name string, p *progress) (func(), error) {
	tunnelLocks.Lock()
	defer tunnelLocks.Unlock()
	if h, ok := tunnelLocks.held[name]; ok {
		h.refs++
		return func() { releaseTunnelLock(name) }, nil
	}
	if err := os.MkdirAll(contexts.Dir(name), 0o700); err != nil {
		return nil, err
	}
	path := contexts.Path(name, TunnelLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the context's own lock file.
	if err != nil {
		return nil, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	told := false
	for {
		err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, err
		}
		if !told {
			told = true
			tunnelWaitNote(p, name, path)
			if p != nil {
				p.setPhase("waiting for the tunnel to " + name)
			}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-time.After(tunnelLockPoll):
		}
	}
	if told && p != nil {
		p.setPhase("")
	}
	command := "vaire"
	if p != nil && p.cmd != nil {
		command = p.cmd.CommandPath()
	}
	raw, _ := json.Marshal(tunnelHolder{PID: os.Getpid(), Command: command, StartedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(append(raw, '\n'), 0)
	}
	tunnelLocks.held[name] = &heldTunnelLock{f: f, refs: 1}
	return func() { releaseTunnelLock(name) }, nil
}

// tryTunnelLock takes the context's tunnel lock without waiting, for the
// tunnel daemon. held is false when another process has it. The returned
// func releases it.
func tryTunnelLock(name, command string) (release func(), held bool, err error) {
	path := contexts.Path(name, TunnelLockFile)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the context's own lock file.
	if err != nil {
		return nil, false, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, _ := json.Marshal(tunnelHolder{PID: os.Getpid(), Command: command, StartedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(append(raw, '\n'), 0)
	}
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

func releaseTunnelLock(name string) {
	tunnelLocks.Lock()
	defer tunnelLocks.Unlock()
	h, ok := tunnelLocks.held[name]
	if !ok {
		return
	}
	h.refs--
	if h.refs > 0 {
		return
	}
	delete(tunnelLocks.held, name)
	_ = h.f.Truncate(0)
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: file descriptors fit in int.
	_ = h.f.Close()
}

func tunnelWaitNote(p *progress, name, path string) {
	who := "another vaire command"
	if raw, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: the context's own lock file.
		var h tunnelHolder
		if json.Unmarshal(raw, &h) == nil && h.PID > 0 {
			cmd := h.Command
			if cmd == "" {
				cmd = "vaire"
			}
			who = fmt.Sprintf("%s (pid %d)", cmd, h.PID)
		}
	}
	line := fmt.Sprintf("%s is using the tunnel to %s; waiting for it to finish (close it, or press Ctrl-C to give up)…", who, name)
	if strings.Contains(who, tunnelDaemonCommand) {
		line = fmt.Sprintf("the tunnel daemon of %s (%s) holds the tunnel; waiting for it to exit (vaire tunnel stop -c %s ends it, or press Ctrl-C to give up)…", name, who, name)
	}
	if p == nil {
		_, _ = fmt.Fprintln(os.Stderr, line)
		return
	}
	p.note(line)
}
