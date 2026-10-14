# Plan: one shared WireGuard tunnel per context (ControlMaster-style), plus stream hardening

## Status (2026-10-14)

- **Implemented, not yet tested live:** the shared-tunnel daemon (Phases 4–6, all at once). It is the default; `ERU_TUNNEL=inprocess` keeps the old in-process tunnel under the tunnel lock.
  - `internal/transport/shared.go` (client, protocol, fingerprint), `internal/transport/sharedserver.go` (server), `internal/transport/peercred_{linux,darwin}.go`, tests in `internal/transport/shared_test.go`.
  - `internal/cli/tunneld.go`: spawn, hidden `eru tunnel serve`, `eru tunnel status`, `eru tunnel stop [--force]`, socket path with the short-path fallback, persist window.
  - `internal/cli/session.go`: `session.dialer`/`closeDialer`; `tunnelConfig` split out of `upTunnel`.
  - Stop hooks: `eru context remove` (forced), `eru server init` before its in-process tunnel (forced), `eru upgrade` (idle daemons only).
- **Implemented earlier:** node stdout decoupling and SIGPIPE (Phase 1), SSH keepalive and MTU 1380 (Phase 2), re-dial and re-attach in `callJob` (Phase 3; it replays `job.follow` and skips the events already shown instead of a journal cursor), the stopgap tunnel lock (Phase 0).
- **Dropped:** compatibility with older CLIs and nodes (version skew). Every machine runs matching builds.
- **Differences from the design below:**
  - No `tunnel.addr` file: the fallback socket path is derived from a hash of the context directory, so every client computes the same path.
  - `eru login` gets no stop hook: it refuses an existing context name, and `eru context remove` already stops the daemon.
  - The daemon compares builds as well as the fingerprint. A client of another build (version, commit, and the executable's size and modification time) replaces an idle daemon; a busy daemon with the same fingerprint keeps serving it.
  - A non-forced stop refuses new clients (`precondition_failed`, `reason: tunnel_stopping`) until it exits.
  - A new daemon waits up to 30s for its first client before the idle rule applies, so `ERU_TUNNEL_PERSIST=0` can't exit before the spawner attaches.

## 0. Where things stand (grounded in the code)

- **Tunnel bring-up.** `openSession` (`internal/cli/session.go`) calls `upTunnel` → `transport.UpWireGuard` (`internal/transport/wireguard.go`) for every command. That builds a fresh `device.NewDevice(..., conn.NewDefaultBind(), ...)`, which binds a random UDP port, with the same `private_key` and the same operator address (`c.Operator.WGAddress`, 10.42.255.1). The node has one `[Peer]`, so whichever process handshook last owns the session keys and endpoint.
- **Why the loser hangs forever.** `persistent_keepalive_interval=25` sends empty keepalives. Those don't arm wireguard-go's "data sent, no reply in 15s → rehandshake" timer. While a CLI waits for a result it sends no TCP data, and `transport.SSHClient` has no SSH keepalive. So `readEvents` in `internal/remote/remote.go` blocks on a socket that will never get another packet.
- **The node side makes it worse (new finding).** `steps/sshd.go` sets `ClientAliveInterval 300` / `ClientAliveCountMax 2`. About 10 minutes after the client vanishes, sshd tears down the session, and `eru node rpc`'s stdout becomes a broken pipe. `runNodeRPC` (`internal/cli/node.go`) → `node.Serve` never calls `signal.Notify`/`Ignore` for `SIGPIPE` (grep finds no SIGPIPE anywhere). Go's rule is that a write to a broken pipe on fd 1 kills the process with SIGPIPE.
  - `eventWriter.writeLocked` (`internal/node/dispatch.go`) writes to stdout before `e.log.Line(...)`. So the run log never gets the line that killed the process, and the handler dies mid-step.
  - This matches the Part 8 report: "run log stops at stanza-create ..., no node process left".
  - Before sshd gives up, a stalled SSH channel window blocks `e.w.Write` while `e.mu` is held. Every `env.Report` (handlers, `streamJournal`) then blocks too, so the node's work stalls along with the client.
- **Who uses the tunnel.**
  - Directly, as a `transport.Dialer`: `psql.go:43` (`proxy`), `console.go:59` (`pg.ServeTLSProxy`), `sql.go:122` (`pg.Connect`), `tunnel.go:98` (`proxy`), and SSH in `openSession`.
  - `server init` calls `upTunnel` itself (`serverinit.go:748`), before a context exists.
  - Every consumer only needs a `Dialer`. That is the seam the design uses.
- **Stopgap (done).** `Tunnel.OnClose` releases a per-context flock (`contexts/<name>/tunnel.lock`) when the tunnel closes; `upTunnel` takes it.
- **Supported OSes.** `scripts/build-release.sh` and `scripts/sign-release.sh` build only `linux/{amd64,arm64}` and `darwin/{amd64,arm64}`. `internal/cli/ssh.go` uses `syscall.SIGWINCH`, so the CLI doesn't build for Windows today. **Windows is out of scope.** Everything new is `//go:build unix`, with Linux and macOS as first-class targets.
- **A gap this design does not close.** `eru login` on a second machine reuses the same operator key and address. Two laptops will still steal the peer from each other. The real fix is a per-machine peer (`eru operator add`, a later-phase command). Phase 2's keepalives make the loser recover instead of hang.

---

## 1. Design decisions

### 1.1 A detached daemon, not "first process becomes master"
The first eru process that needs the node spawns a background daemon: the hidden command `eru tunnel serve --context <name>`. The daemon owns the only wireguard-go device for that context and serves a unix socket in the context directory. Every eru process, including the one that spawned it, is a client.

Rejected alternative: the first CLI process acts as master. Its lifetime is the first command's lifetime. An `eru ssh` that became master would kill every other command's streams when the shell exits, and handing mastership over is complex and racy. OpenSSH's `ControlPersist` solves the same problem by forking a persistent master, and so does this design.

### 1.2 Share the tunnel's TCP dials, not the SSH connection
The daemon proxies **TCP dials** into the netstack. Each eru process still runs its own SSH client over its proxied stream: `DialSSH`, `Handshake`, `remote.Node.Call` and `eru ssh` stay as they are.

The bug is about WireGuard peer identity, not SSH. Multiplexing SSH sessions through the daemon would need a session protocol over the socket for small gain, about one SSH handshake RTT. It can be a later optimisation.

### 1.3 Socket protocol (versioned, small)
Each client action is one unix connection. The client sends one JSON line, the daemon answers one JSON line, and for `dial` the connection then turns into raw bytes.

```
→ {"v":1,"op":"hello"}                 ← {"ok":true,"proto":1,"build":"<ver/commit>","pid":123,"fingerprint":"<sha256>","attached":2,"streams":5,"last_handshake":"..."}
→ {"v":1,"op":"attach","pid":456,"cmd":"restore"}   ← {"ok":true}   (held open for the client's lifetime = lease)
→ {"v":1,"op":"dial","addr":"10.42.0.1:22"}          ← {"ok":true} then bytes | {"ok":false,"code":"...","message":"..."}
→ {"v":1,"op":"stop","force":false}    ← {"ok":true,"draining":true}
```

- **Fingerprint.** sha256 over the operator WG public key (derived), operator address, node public key, endpoint, AllowedIPs, MTU and proto. The client computes the same value from the context and compares. A mismatch means a re-login, a node rebuild or a new build changed something.
- **Dial policy.** The daemon only dials `tcp` to addresses inside the AllowedIPs prefix (the node /24 built in `upTunnel`). It is not a general proxy.
- **Lease.** The `attach` connection counts toward liveness. That is what keeps an idle `eru tunnel` listener, or an `eru console` between queries, from losing its daemon. EOF on it also tells the client right away that the daemon died.

### 1.4 Who starts it, and the startup race
New code lives in `internal/transport/shared.go` (client) and `internal/transport/sharedserver.go` (server). Spawning lives in `internal/cli/tunneld.go`.

1. `SharedDialer.ensure()` connects to the socket and sends `hello`.
2. If that fails with `ENOENT` or `ECONNREFUSED`, take `contexts/<name>/tunnel.spawn.lock` (blocking flock, short timeout) and retry the connect. Another process may have just spawned it.
3. If it still fails, spawn the daemon:
   - `exec.Command(os.Executable(), "tunnel", "serve", "--context", name)`
   - `SysProcAttr{Setsid: true}`; stdin `/dev/null`; stdout/stderr → `contexts/<name>/tunnel.log` (0600, truncated at start)
   - one `ExtraFiles` pipe as fd 3 for readiness. The daemon writes `{"ok":true}` or a CLIError body, then closes it.
   - The client relays a startup error, for example an invalid key or endpoint (`wgError`), as the same CLIError the in-process path gives today.
4. The daemon's own startup order:
   - take the **singleton lock** (exclusive, non-blocking flock). This is the same lock file the stopgap uses; see §1.9.
   - if it's held, exit with `already_running`, and the client goes back to step 1.
   - with the lock held, `os.Remove` any stale socket. This is safe because only the lock holder may remove it.
   - `syscall.Umask(0o077)`, `net.Listen("unix", path)`, `os.Chmod(path, 0o600)`.
   - `upTunnel`, then signal readiness.

The "who brings up the device" logic moves out of `upTunnel`'s callers into the daemon. The daemon reuses `upTunnel` and `transport.UpWireGuard` as they are.

### 1.5 Lifetime
- **Idle exit.** The daemon exits when there are **no attached leases and no open streams** for the persist window.
  - Default 10m. Override with `ERU_TUNNEL_PERSIST` (a Go duration; `0` = exit as soon as the last lease drops) or a context.toml key `tunnel_persist`.
- **Explicit stop.** `eru tunnel stop [--force]` (new child of the existing `tunnel` spec in `internal/cli/tree.go`; cobra resolves children before the parent's `runTunnel`).
  - Without `--force`, the daemon refuses new leases, drains, and exits when idle.
  - `--force` closes everything now.
  - `eru tunnel status` prints the `hello` reply as JSON or human output: pid, build, attached clients with their pid and command, streams, last handshake (from `dev.IpcGet()` → `last_handshake_time_sec`), and the idle deadline.
- **Other exits.**
  - `runContextRemove` (`internal/cli/login.go`) sends a forced `stop` before `contexts.Remove`.
  - `eru login` over an existing context sends a non-forced `stop`.
  - `eru upgrade` (self-upgrade) sends a non-forced `stop` to idle daemons.
  - SIGTERM or SIGINT to the daemon shuts it down cleanly: close the listener, `Tunnel.Close`, release the lock.
- **Config or build mismatch on `hello`** (the fingerprint or the build differs):
  - if `attached == 0`, send `stop{force:true}` and respawn;
  - otherwise fail with `transport_failed`, `reason: tunnel_config_changed`, and the hint "another eru command (pid X, `restore`) is using the old tunnel; wait for it or run eru tunnel stop --force".
  - A busy daemon of another build with the same fingerprint is used as it is: the dial protocol doesn't depend on the build.

### 1.6 What clients look like
- `session.tunnel *transport.Tunnel` becomes `session.dialer transport.Dialer` plus `closeDialer func()`.
- `openSession` picks the dialer:
  - `transport.SharedDialer` by default (after Phase 6);
  - `upTunnel` in-process when `ERU_TUNNEL=inprocess` is set, still under the stopgap lock.
- The call sites only change field names: `psql.go:43`, `console.go:59`, `sql.go:122`, `tunnel.go:98` and the `DialSSH` call in `openSession`. All of them already take a `transport.Dialer`.
- `session.Close` closes the SSH client and releases the lease. It never closes the device.
- **`eru tunnel` and `eru console`.** Each local connection already dials on its own (`proxy`, `pg.ServeTLSProxy`), so each becomes one proxied stream. While those commands run, the lease keeps the daemon alive.
- **`server init`.** Stays in-process (`serverinit.go:748`). No context exists yet, and the keys are in memory as `r.sec.WGPrivateKey` with `config.DefaultFirstOperatorWG`. A `--resume` against an existing context name first sends `stop{force:true}` to that context's daemon, then takes the singleton lock for its in-process tunnel. Init is the only remaining in-process user.

### 1.7 When the daemon dies mid-command
- **What fails.** Every proxied stream resets. SSH reads return errors instead of hanging, so `remote.Node.Call` returns "ended without a result". The lease connection hits EOF.
- **The node.** After Phase 1 it keeps running the RPC to completion. Stdout errors are ignored, and the run log holds everything.
- **Respawn.** The next `SharedDialer.DialContext` sees `ECONNREFUSED`, goes through `ensure()` and respawns. The kernel released the dead daemon's flock, and the stale socket gets unlinked. The new device is the only one, so its handshake wins cleanly.
- **Recovery by command type:**
  - **Job commands** (`callJob` in `internal/cli/jobs.go`: backup now/verify, restore, drill, fork, dump): re-dial SSH and resume with `job.follow` from the last cursor (Phase 3). The user sees "connection lost; re-attached to job X" and the command finishes.
  - **Non-job RPCs** (`apply`, `db create`, `db drop` ...): no transparent resume. Fail with `transport_failed`, `reason: stream_lost`, `request_id`, and the hint "the node keeps running it; its run log is /var/log/eru/runs/...<request_id>; check with eru logs / eru plan".
    - A later `run.status <request_id>` RPC could read the run log result. Not in this plan.
  - **`eru ssh`**: the shell ends, like an OpenSSH ControlMaster death.
  - **`eru tunnel` and `eru console`**: in-flight client connections drop, and the next connection respawns the daemon.

### 1.8 Security
- **Location.** The socket lives in `contexts/<name>/` (0700, enforced by `contexts.FixModes`). The socket file is created under umask 077 and then chmodded to 0600.
- **Peer credentials, checked on every accepted connection.** Reject when the peer uid ≠ `os.Getuid()`.
  - Linux: `unix.GetsockoptUcred(fd, SOL_SOCKET, SO_PEERCRED)`.
  - macOS: `unix.GetsockoptXucred(fd, SOL_LOCAL, LOCAL_PEERCRED)`.
  - Both via `golang.org/x/sys/unix`, which is already an indirect dependency; promote it to direct.
  - Files: `peercred_linux.go`, `peercred_darwin.go`.
- **Dial policy.** TCP only, inside the node prefix only (§1.3).
- **Secrets.** The daemon reads `wg.key` itself through `contexts.ReadFile`; it is never on argv or in the environment. It calls `secrets.Add` so `tunnel.log` is redacted. WireGuard `Errorf` goes to `tunnel.log` (what `--verbose`'s `logf` used to print).
- **No new exposure.** Any same-uid process can already read `wg.key`.
- **Socket path length.** `sun_path` is 104 bytes on macOS and 108 on Linux, and a long `XDG_CONFIG_HOME` or context name can exceed that.
  - When `len(path) > 100`, use `$XDG_RUNTIME_DIR/eru/<sha256(ctxdir)[:16]>.sock` on Linux, or `os.TempDir()/eru-<uid>/<hash>.sock` on macOS (macOS `$TMPDIR` is per-user).
  - Create the directory 0700 and refuse to use it if it already exists with another owner or looser mode (`lstat`, no symlinks).
  - Write the chosen path to `contexts/<name>/tunnel.addr` (0600).

### 1.9 How it replaces the stopgap lock
- **Today (stopgap).** An exclusive per-context flock is held for the tunnel's lifetime (`Tunnel.OnClose` releases it), so eru processes run one at a time.
- **With the daemon.** The **same lock file** becomes the daemon's singleton lock: the daemon holds it, clients never do.
- **An in-process holder.** A client whose spawn fails with `already_running` but gets no `hello` reply (`eru server init` holds the lock in-process, or a daemon is exiting) retries for about 10s, then fails with `precondition_failed`, `reason: tunnel_busy`.
- **Holder info.** The holder writes its pid, command and start time into the lock file, so the in-process wait message names it.
- After Phase 6, the in-process path (`ERU_TUNNEL=inprocess`, `server init`) still takes the lock exclusively. That rules out two devices on one machine.

---

## 2. Hardening that ships independently

### 2.1 Node: a blocked or broken stdout must never hold back the run log or result (`internal/node/dispatch.go`, `internal/cli/node.go`)
1. In `runNodeRPC`, call `signal.Ignore(syscall.SIGPIPE)` before `node.Serve`. Writes then return `EPIPE` instead of killing the process. Put it in `Serve` itself or in the node path only; `remote.Local` runs `Serve` in-process for `--local` and doesn't need it.
2. `eventWriter`: write `e.log.Line("event", ...)` **before** stdout. Keep marshalling and logging under `e.mu`, so ordering in the log is preserved and cheap.
3. Decouple stdout. `eventWriter` gets a writer goroutine fed by a buffered channel of lines (say 4096 entries). `writeLocked` enqueues without blocking.
   - On queue overflow, set `stdoutLagging`, drop log-type events and keep step, warning and result events. Once a slot frees, emit one `warning{code:"events_dropped",n}`.
   - On the first write error, set `stdoutDead` and stop writing. The handler keeps running.
   - `ok`/`fail` enqueue the result, then wait up to about 5s for the queue to drain before `Serve` returns. The result is already in the run log either way.
4. In `followJob` and `streamJournal` (`internal/node/jobs.go`), `report` no longer blocks, so the journal reader can't stall. The existing 2s guards stay.
5. Optional, same unit: filter `pam_unix(runuser:session)` lines in `streamJournal`, which the MANUAL-TESTING note lists.
6. Tests: `internal/node` has none today. Add `dispatch_test.go` with an `eventWriter` over a writer that blocks forever or returns `EPIPE`. Assert that `emit` returns, the run log gets every line, and `ok` returns within the drain timeout.

### 2.2 Client: SSH keepalive (`internal/transport/ssh.go`)
- `SSHOptions.KeepaliveInterval` (default 15s) and `KeepaliveTimeout` (default 3m).
- After `ssh.NewClient` in `DialSSH`, a goroutine sends `keepalive@openssh.com` with `wantReply` every interval, with **at most one probe in flight**: x/crypto/ssh serialises global requests for the whole round trip, so a second probe would only queue behind the first.
- **Any inbound byte counts as life.** The connection is wrapped in a read tracker; a keepalive reply, channel data or a late reply all reset the clock. A busy node whose replies are slow is alive, not lost.
- The client closes the connection only when a probe is in flight and **nothing at all has arrived for 3 minutes**. `SSHClient.Lost` then reports it, and `SSHClient.Close` stops the goroutine.
- Effect: a stalled stream becomes a transport error within about 3 minutes instead of hanging. In the shared-tunnel world that triggers re-dial or resume. In the in-process or second-machine world, the keepalive's real data also triggers a WireGuard rehandshake, so the peer can win back.
- Applies to every `DialSSH` caller: `openSession`, `dialRetryReport`, serverinit.

### 2.3 Re-dial with `job.follow` resume
- **Node side** (`followJob` / `streamJournal`):
  - switch journalctl to `-o json` and take `MESSAGE` as the line;
  - carry `__CURSOR` on each re-emitted event in a new optional `output.Event.Cursor` field (`json:"cursor,omitempty"`); update `schema/eru-v1.json`;
  - `jobFollowParams` gains `AfterCursor string`, passed as `--after-cursor=` instead of `-n all`.
- **Client side** (`callJob`):
  - track the last cursor and the `jobID` (already captured from the `progress/job` event);
  - on an error that is a transport loss (not `ctx` cancel, not a node error envelope) with a known `jobID`:
    1. note "connection lost; re-attaching to job X";
    2. close `s.node.SSH` and re-dial it (a new `reconnect` method on `session` that reuses the dialer and runs `Handshake`);
    3. call `job.follow{id, after_cursor}`;
    4. retry with backoff (2s, 4s, ... capped at 30s) for up to `--reattach-timeout` (default 10m), then fall back to the existing `detachedResult`.
  - `runJobFollow` gets the same loop for free, since it calls `callJob`.
- Related, from Part 8: `job follow` on a finished job printed no result line. Fix it in the same unit, in the `runJobFollow` message.

### 2.4 `TunnelMTU` 1420 → 1380 (`internal/transport/wireguard.go`)
- The measured path MTU is 1492 (PPPoE). Over IPv6, WireGuard adds 80 bytes, and 1420 + 80 = 1500 > 1492.
- Netstack advertises an MSS derived from its MTU, so TCP from the node respects it without changing the node's wg0.
- One-line change. Document it in MASTER §7.2. It becomes part of the daemon fingerprint.

---

## 3. Phases (each one ships on its own)

**Phase 0: stopgap per-context flock (done).** It settled on:
- the lock file name (suggest `contexts/<name>/tunnel.lock`);
- a holder `.info` file;
- the lock being exclusive and taken only around `upTunnel` in `openSession` and `serverinit`;
- `Tunnel.OnClose` releasing it.

**Phase 1: node stdout decoupling and SIGPIPE** (§2.1; done). The node binary goes out through the existing auto-upgrade handshake. This alone stops the "node process gone, credentials lost" failures (`db create big`).

**Phase 2: SSH keepalive and MTU 1380** (§2.2, §2.4; done). Client only. Hangs become errors.

**Phase 3: `job.follow` resume and re-dial in `callJob`** (§2.3; done, with event skipping instead of a cursor). Needs a matching node build, which the handshake enforces. Restore and backup commands then survive a dropped stream even with only the stopgap.

**Phase 4: shared-tunnel daemon** (implemented together with Phases 5 and 6, so there is no opt-in `ERU_TUNNEL=shared` stage).
- `internal/transport/sharedserver.go`: listener, peer-cred check, protocol, dial policy, stream and lease accounting, idle timer, fingerprint, `hello`/`attach`/`dial`/`stop`.
- `internal/transport/shared.go`: `SharedDialer` (implements `Dialer`; `ensure`, respawn on `ECONNREFUSED`, lease).
- `peercred_{linux,darwin}.go`; `//go:build unix` throughout.
- `internal/cli/tunneld.go`: `spawnDaemon` (Setsid, fd 3 readiness, `tunnel.log`), hidden `tunnel serve` (reads the context and calls the existing `upTunnel`), socket path resolution with the short-path fallback.
- `internal/cli/session.go`: the `session.dialer` refactor and dialer choice.
- Field renames in `psql.go`, `console.go`, `sql.go`, `tunnel.go`.
- Unit tests in `internal/transport`, which has none today:
  - the protocol round-trip through a fake `Dialer` (loopback TCP echo);
  - refusing an address outside the prefix;
  - idle exit with an injectable clock;
  - the lease keeping the daemon alive;
  - stale-socket recovery;
  - concurrent `ensure()` from N goroutines spawning exactly one daemon (spawn func injected);
  - fingerprint mismatch handling.

**Phase 5: control surface and lifecycle hooks** (implemented; the MASTER doc edit is still open).
- `eru tunnel status|stop [--force]` and the hidden `tunnel serve` in `tree.go`, with `CommandMeta` (error codes `transport_failed`, `context_not_found`; stop counts as `Mutating`, with a `SkipDryRunReason`).
- Stop hooks in `runContextRemove`, `eru login` and `eru upgrade`.
- `--verbose` prints the daemon pid and the `tunnel.log` path.
- Docs:
  - MASTER §7.2: "a userspace tunnel per context, shared by eru processes";
  - MANUAL-TESTING: a new part "concurrent commands", and remove "Practical rule until the tunnel fix: run one eru process at a time";
  - `eru guide agents` text: agents may run commands in parallel.

**Phase 6: default on** (implemented). Shared becomes the default. `ERU_TUNNEL=inprocess` is the escape hatch. The stopgap lock is now the daemon singleton plus the in-process guard (§1.9), with no serialisation of clients.

Manual test before flipping the default:
1. A `restore` running while `eru ssh` is open completes.
2. `eru tunnel --db app` stays usable across 15 minutes of idleness.
3. `kill -9` of the daemon during `eru backup now` → the client re-attaches and prints the result.
4. `kill -9` during `eru apply` → `stream_lost` with `request_id`, and the node finishes; check the run log.
5. On macOS: a long `$XDG_CONFIG_HOME` exercises the short-path fallback, plus sleep/wake and a Wi-Fi switch mid-command (WireGuard roaming plus keepalive).
6. Two contexts at once each get their own daemon.
7. `eru context remove` stops the daemon.

---

## 4. Risks and open points
- **Daemon on an old binary after `eru upgrade` or a rebuild.** `eru upgrade` stops idle daemons, and a client of another build replaces an idle daemon on `hello`. A busy one keeps serving until it goes idle. The binary path can change under a running daemon; that's harmless because the daemon never re-execs.
- **Non-job RPCs can't resume** (`apply`, `db create`). Phase 1 makes them finish on the node. Showing the outcome later needs a small `run.status` RPC; it's worth adding next because `db create` loses one-time credentials when its stream dies (an open MANUAL-TESTING item).
- **Second machine with the same key.** Unchanged (§0); needs per-machine operators.
- **Windows.** Not supported by the CLI today (`SIGWINCH`, no release target). The unix build tags keep it that way explicitly.

### Critical Files for Implementation
- /home/mbv/work/eru/internal/cli/session.go
- /home/mbv/work/eru/internal/transport/wireguard.go (plus new /home/mbv/work/eru/internal/transport/shared.go and sharedserver.go)
- /home/mbv/work/eru/internal/transport/ssh.go
- /home/mbv/work/eru/internal/node/dispatch.go (and /home/mbv/work/eru/internal/cli/node.go for SIGPIPE)
- /home/mbv/work/eru/internal/cli/jobs.go and /home/mbv/work/eru/internal/node/jobs.go (job.follow resume)
