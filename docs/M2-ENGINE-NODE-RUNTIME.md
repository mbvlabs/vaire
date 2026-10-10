# eru M2: Engine & node runtime — implementation spec

> Audience: the agent (or engineer) implementing milestone M2 of [`PHASE-1-IMPLEMENTATION.md`](./PHASE-1-IMPLEMENTATION.md) (§24).
> Builds on M0 (`9d969fb`) and M1 (release, install, `eru upgrade`).
> Parent sections: §3 (binary modes), §5.2 (streaming), §6.2 (node files), §7.4 (RPC), §7.5 (binary install), §9 (step engine), §22 (e2e).
> Status: approved · 2026-10-09

---

## Decisions

Settled at review (2026-10-09):

- **No operator path in M2.** M2 is the node side only. Tests drive the node through `incus exec`. Contexts, the transport, the version handshake, the RPC client and E18 move to M3, which needs them for bootstrap anyway.
- **`plan` and `apply` are the engine's commands.** M2 builds them as node RPC methods. M3 wires the operator `eru plan` / `eru apply` end to end.
- **Binary install as primitives only.** M2 builds the node-side `upgrade.install` method. The operator push, the bootstrap install and `--auto-upgrade-nodes` move to M3, and E27 moves with them.

The operator-side design worked out during review is kept in [Appendix A](#appendix-a-carried-to-m3) as input for the M3 spec.

---

## 0. Goal and exit criteria

M2 builds the node runtime every later milestone runs on: `eru node rpc` reads one request, checks it, takes the lock, runs the method through the step engine, and streams redacted NDJSON events ending in one result. The node keeps a journal and a run log per request.

M2 ships **no real host steps**. The production step catalogue is empty until M3 adds steps 1–6. A stub catalogue compiled only into `e2etest` builds exercises the engine.

**Done when** (Debian 13 amd64 + `just lint`, per §24):

1. **E26** (node runtime, new) passes (§9).
2. `eru node version`, `eru node selfcheck` and `eru node rpc` work on a VM. `rpc` and `selfcheck` run only as root.
3. A production build contains no stub steps and no `ERU_TEST_` strings.

**Not in M2:** anything that runs on the operator against a node: contexts, SSH, WireGuard, the handshake, RPC client, binary push, `--auto-upgrade-nodes` (M3). Real steps, the `cluster.toml` schema, file diffs in `plan`, Postgres restarts (M4). `eru node upgrade` and `--rollback` (M10).

**Change to §24.** M2's scenario becomes E26 instead of E18. M3 gains E18, E27, the version handshake and the binary push.

---

## 1. Deliverables

| Path | What |
| --- | --- |
| `internal/secrets/` | New. `Registry` and `Redact` (§6) |
| `internal/rpc/` | New. Request, event and result types, request-ID generation, protocol constants (§4) |
| `internal/engine/` | New. `Step` interface, runner, plan, journal, locks, run log, `Shell` (§5) |
| `internal/steps/` | New. `Catalog()`, empty in production. `catalog_e2etest.go` holds the stub steps (§5.7) |
| `internal/steps/scripts/` | New. `go:embed` home for step scripts. M2 has only the stub scripts, behind `e2etest` |
| `internal/node/` | New. RPC dispatcher, method table, `selfcheck`, `upgrade.install` (§4, §7) |
| `internal/config/` | New. Minimal `node.toml` and `cluster.toml` loaders (§8) |
| `internal/cli/node.go` | New. `node rpc`, `node selfcheck` |
| `internal/cli/tree.go` | `node rpc` and `node selfcheck` wired. `plan` and `apply` stay `notImplemented` until M3 |
| `e2e/` | E26; harness helpers for stdin over `incus exec`, node preparation and request building |
| `docs/DEVIATIONS.md` | Entries in §10 |

No new Go dependencies. TOML uses `pelletier/go-toml/v2`, which §2.1 already allows.

---

## 2. Build tags

Same pattern as M1's `verify_e2etest.go`:

- `//go:build e2etest` files add the stub catalogue and the stub restarter.
- `//go:build !e2etest` files hold the production stand-ins: an empty catalogue and no restarter.
- A production binary contains no `ERU_TEST_` strings and no `stub-` step IDs. `just lint` gains a check: `go tool nm bin/eru | grep -c ERU_TEST` must be 0, and `bin/eru commands --check-steps` must report 0 steps.
- The harness builds the node binary with `-tags e2etest` for E26. E25 keeps its own build.

---

## 3. Operator side

Not in M2. See [Appendix A](#appendix-a-carried-to-m3).

---

## 4. RPC protocol (§7.4)

### 4.1 Request

```json
{"schema":"eru-rpc/v1","request_id":"01JA…","method":"apply","params":{"steps":[]},
 "secrets":{},"dry_run":false,"confirm":"","operator":"op-1",
 "client_version":"dev","client_commit":"2eead45474ca","client_channel":"dev","lock_timeout_s":30}
```

- `request_id` is a ULID: 48-bit ms timestamp plus 80 random bits, Crockford base32, implemented in `internal/rpc` with no dependency. A request without one gets one from the node.
- `client_commit`, `client_channel` and `lock_timeout_s` are additions to §7.4. `lock_timeout_s` defaults to 30; timers will pass 0.
- Stdin is capped at 1 MiB and decoded strictly: exactly one JSON object, then EOF.

### 4.2 Node dispatcher

`eru node rpc`:

1. Not root → `precondition_failed`, `details.reason = "root_required"`.
2. Decode the request. Bad schema or JSON → `usage_error`.
3. Register every `secrets` value with the registry (§6) **before anything else can print**.
4. **Version check.** The request's client build must equal the node's:
   - release builds: `client_version` and `client_channel` equal;
   - dev builds (`channel == "dev"`): `client_commit` must match too. Every dev build reports `version = "dev"`, so comparing only the version would let two different dev builds talk to each other.

   A mismatch → `node_version_mismatch` (exit 10), `details{client, node}`. **`upgrade.install` is the only exempt method**, since its whole purpose is to cross versions. The operator's handshake (M3) applies the same rule before it ever sends a request; this check is the backstop.
5. Look up the method. Unknown → `usage_error`, `details.methods` lists the known ones.
6. When the method needs a confirm token, recompute it from `cluster.toml` and the params and compare it with `confirm`. A mismatch → `unsafe_action_requires_confirmation` (exit 4), `details.confirm_token`.
7. Take the lock in the method's mode with `lock_timeout_s` (§5.4).
8. Open the run log (§5.6), run the handler with an event sink, and write the `result` event. Exit with the envelope's exit code.

### 4.3 Methods in M2

| Method | Lock | Confirm | Handler |
| --- | --- | --- | --- |
| `plan` | shared | — | Engine plan over the catalogue (§5.3) |
| `apply` | exclusive (shared with `dry_run`) | `{cluster}/restart`, checked only when a step needs a restart and `confirm` is set | Engine apply. Params: `steps` (IDs, optional) |
| `selfcheck` | none | — | §7.1 |
| `upgrade.install` | exclusive | — | §7.2 |

M3+ add methods to the same table. Each entry is `{name, lock, confirm func(cfg, params) string, handler}`. `eru commands --json` doesn't list RPC methods; they're wire names, not commands.

### 4.4 Events

Use the existing `output.Event` (§5.2). M2 emits:

- `step`: `state` ∈ `started|skipped|completed|failed`, plus `index`, `total`, and `detail` (skip reason or failure message).
- `log`: one per script output line, with `step` set.
- `warning`: `code` + `message`.
- `result`: always last, exactly once, including when the handler fails or panics. The dispatcher recovers, emits `error` / exit 70, and writes the stack only to the run log.

stdout is flushed after every event so the operator (M3) sees progress as it happens.

**Protocol stability.** `eru-rpc/v1` request fields and the `upgrade.install` params and result are frozen once M2 ships. From M3 on, a new operator calls `upgrade.install` on an **old** node binary, so changing them would break every later upgrade. Additions are optional fields only.

---

## 5. Step engine (§9)

### 5.1 Interface

As §9.1, with fields that don't exist yet left out until their milestone:

```go
type Step interface {
    ID() string
    Describe(cfg config.Cluster) string
    Inputs(cfg config.Cluster, rt Runtime) ([]byte, error)
    Check(ctx context.Context, cfg config.Cluster, rt Runtime) (CheckResult, error)
    Apply(ctx context.Context, cfg config.Cluster, rt Runtime, report Reporter) (ApplyResult, error)
}
type Runtime struct {
    DryRun  bool
    Shell   Shell
    Secrets *secrets.Registry
    Log     *RunLog
    // Host hostinfo.Info — M3. PG *pg.Local — M4.
}
```

`ID()` must match `^[a-z0-9]+(-[a-z0-9]+)*-v[0-9]+$`. The runner refuses duplicate IDs, and so does `just lint` through a hidden `--check-steps` mode on `eru commands`.

### 5.2 Runner (§9.2)

For each step, in catalogue order:

1. `inputs_hash = sha256(ID || 0x00 || Inputs())`.
2. Journal has `{status: completed, inputs_hash}` equal → `skipped`, reason `journal`.
3. `Check` complete **and** journal hash equal → `skipped`, reason `check`. Complete with a different hash still applies.
4. Otherwise mark `running` in the journal, `Apply`, then write `completed` with `inputs_hash`, `completed_at` and `restart`, or `failed` with the redacted error.

- The first failure stops the run: `step_failed` (exit 5), `details{step, error, index}`. The next `apply` resumes at that step, because earlier ones skip on the journal.
- `params.steps` runs only those steps, in catalogue order. An unknown ID → `validation_failed` listing the valid IDs.
- **Restarts.** Aggregate `ApplyResult.Restart` (`none|reload|restart`) across the run. `reload` is the step's job. If any step needs `restart`:
  - with the correct confirm, call `Restarter.Restart(ctx)` once at the end;
  - without a confirm, finish with `data.restart_pending: [step IDs]` and warning `restart_pending`;
  - with no restarter, as in production M2, that's an engine bug → `error`. M4 provides the Postgres restarter.
- `ctx` is cancelled on SIGTERM/SIGINT. The current step's script gets SIGTERM and the step is journaled `failed` with `interrupted`.

### 5.3 Plan and dry-run

`plan` runs steps 1–3 of the runner for every step and returns:

```json
{"steps":[{"id":"stub-file-v1","index":1,"action":"apply","reason":"inputs_changed"},
          {"id":"stub-secret-v1","index":2,"action":"skip","reason":"journal"}],
 "would_apply":1,"total":2}
```

`reason` ∈ `new|inputs_changed|check_incomplete|previous_failure|journal|check`. File diffs arrive with `render` in M4.

`apply` with `dry_run: true` returns the same data as `plan`, takes the shared lock, and writes nothing (no journal, no run log, no scripts). `Shell` refuses to execute when `DryRun` is set, as a second guard.

### 5.4 Locks (§9.5)

- `flock(2)` on `/run/lock/eru.lock` through `syscall.Flock`, so there's no dependency on the `flock` binary. Shared = `LOCK_SH`, exclusive = `LOCK_EX`, polled with `LOCK_NB` every 200 ms until the timeout. A timeout of 0 tries once.
- On acquiring, write `/run/lock/eru.lock.info` atomically: `{pid, method, request_id, mode, started_at}`.
- On timeout → `precondition_failed`, `details{reason: "lock_held", holder}`, where `holder` is the info file. When the holder's PID is no longer alive, `holder` is `null`.
- With several shared holders the info file names the most recent one. That's enough to tell the operator what's running. Accepted limitation, recorded in DEVIATIONS.

### 5.5 Journal (§9.3)

Unchanged from §9.3. `/var/lib/eru/journal.json`, 0600, written atomically with temp + fsync + rename + dir fsync, under the exclusive lock. A corrupt journal → `precondition_failed`, `details.reason = "journal_corrupt"`. It is never silently reset.

### 5.6 Run log

`/var/log/eru/<UTC 20261009T120000Z>-<method>-<request_id>.log`, dir 0750, file 0640, both root. It holds the request (secrets dropped), every event, and full script output, all through `Redact`. Rotation (`logrotate`, 30 days) is a host step in M3. `selfcheck` writes no run log.

### 5.7 Shell and the stub catalogue

`Shell.Run(ctx, stepID, script, env, report)` follows deploycrate's `Shell.Run`, plus:

- Line streaming instead of `CombinedOutput`: one `log` event per line as it arrives.
- Redaction on every line.
- `env` keys must match `^[A-Z][A-Z0-9_]*$`. Values go through the environment, never argv.
- Scripts come from `//go:embed scripts/*.sh` and start with `set -euo pipefail` (§0.1).

**Stub catalogue** (`e2etest` only), steered by files under `/etc/eru-e2e/`:

| ID | Behaviour |
| --- | --- |
| `stub-file-v1` | Writes `/var/lib/eru-e2e/file` from `cluster.toml` `[e2e] value`. Inputs = that value. Check = file content matches |
| `stub-secret-v1` | Reads `/etc/eru-e2e/secret`, registers it, `echo`es it from a script. Proves redaction |
| `stub-slow-v1` | Sleeps `/etc/eru-e2e/sleep_s` seconds (default 0). Inputs include the value, so changing it re-runs the step. Holds the lock for contention tests |
| `stub-fail-v1` | Fails while `/etc/eru-e2e/fail` exists |
| `stub-restart-v1` | Returns `Restart: restart` while `/etc/eru-e2e/restart` exists; that file's presence is part of its inputs. The stub restarter writes `/var/lib/eru-e2e/restarted` |

---

## 6. Redaction (§9.6)

- `secrets.Registry`: a process-wide, mutex-guarded set. `Add(value)` ignores values shorter than 6 characters.
- `Redact(s)` replaces each registered value with `«redacted»`, longest first, so a secret that contains another one is fully hidden.
- Applied on every output path in node mode: event writer, run log, journal `error`, envelope `message`/`hint`/`details` (walked recursively), and stderr diagnostics. The node wraps its stdout in a redacting event writer, so new code can't forget.

---

## 7. Node maintenance methods

### 7.1 `selfcheck`

`eru node selfcheck` (root, local) and RPC `selfcheck` return:

```json
{"checks":[{"name":"root","status":"pass"},{"name":"node_toml","status":"warn","detail":"not an eru node yet"}]}
```

| Check | Fail when |
| --- | --- |
| `root` | Not root |
| `binary` | `/usr/local/bin/eru` is not this executable, or not 0755 root |
| `node_toml` | Present but unparseable. Absent = `warn` until M3 writes it |
| `cluster_toml` | Present but unparseable. Absent = `warn` |
| `journal` | Present but corrupt or not 0600 |
| `dirs` | `/var/lib/eru`, `/var/log/eru` or `/run/lock` can't be created or written |
| `tools` | Missing any of `bash`, `systemd-run`, `systemctl`, `sha256sum`, `install` |

Any `fail` → `precondition_failed`, `details.checks`. Only `warn` → `ok` with a `selfcheck_warn` warning.

### 7.2 `upgrade.install` (§7.5 step 3)

Params `{path, sha256, version, commit, channel}`. The node:

1. Requires `path` to match `^/tmp/eru-[A-Za-z0-9._-]+$`, be a regular file (`Lstat`, no symlink), and be owned by the SSH user (`SUDO_UID`) or by root.
2. Checks the sha256 with a constant-time compare. A mismatch → `release_verification_failed` (exit 14), and the file is deleted.
3. Runs `<path> version --output json` (10 s). Version, commit and channel must equal the params, or → `release_verification_failed`.
4. `install -m 0755 -o root -g root` to `/usr/local/bin/eru.new`, hard-links `eru` → `eru.prev` (replacing it), renames `eru.new` → `eru`, and fsyncs the dir.
5. Runs the **new** binary's `node selfcheck --output json`. On `fail`, renames `eru.prev` back and returns `step_failed`, `details{reason: "selfcheck_failed_rolled_back", checks}`.
6. Deletes `path`. Returns `{previous, installed, rolled_back: false}`.

Nothing calls it from the operator until M3.

---

## 8. Config files in M2

- `node.toml` (§6.2): `cluster`, `node_name`, `role`, `installed_at`. Read by `selfcheck`. M3 writes it during bootstrap; M2 tests write it by hand.
- `cluster.toml`: M2 decodes `[cluster] name` and, in `e2etest` builds, `[e2e] value`. Unknown keys are **allowed** in M2 because the schema doesn't exist yet. M4 adds the full schema and switches to a strict decode.
- Confirm tokens use `[cluster] name`. A missing `cluster.toml` → `cluster_not_initialized` (exit 2) for `plan` and `apply`.

---

## 9. E2E scenario E26: node runtime

Debian 13 amd64. The harness pushes an `e2etest` node build to `/usr/local/bin/eru`, writes `node.toml`, `cluster.toml` (`name = "e2e"`, `[e2e] value = "a"`) and `/etc/eru-e2e/secret`, then snapshots the VM. Requests are piped into `incus exec <vm> -- /usr/local/bin/eru node rpc` with the client fields read from `eru node version`. Every response is checked line by line: valid `eru-event/v1`, exactly one `result`, last.

1. `eru node selfcheck --json` → `ok`; `warn` only for items M3 provides.
2. `plan` → 5 steps, all `apply`, reason `new`.
3. `apply` → `step` events in order with `index/total`; journal has 5 `completed` entries, mode 0600; one run log, mode 0640.
4. The secret from `stub-secret-v1`, and a second secret sent in the request's `secrets`, are absent from stdout, the run log and the journal. `«redacted»` is present in stdout and the log.
5. Re-run `apply` → 5 × `skipped`/`journal`.
6. Set `[e2e] value = "b"` → `plan` shows only `stub-file-v1` with `inputs_changed`; `apply` re-runs only it.
7. `touch /etc/eru-e2e/fail` → `apply` exits 5, `step_failed`, `details.step = "stub-fail-v1"`, journal `failed`. Remove it → `apply` resumes at `stub-fail-v1`.
8. `sleep_s = 20`, start `apply` in the background, then `apply` with `lock_timeout_s: 2` → exit 11, `lock_held`, with the first run's PID and `apply`.
9. `touch /etc/eru-e2e/restart` → `apply` succeeds with `restart_pending` and no marker. With `confirm: "e2e/restart"` → marker written. With `confirm: "wrong/restart"` → exit 4.
10. `apply` with `dry_run: true` → `plan` data; journal byte-identical; no new run log.
11. Unknown method → exit 1 with `details.methods`. `node rpc` as `nobody` → exit 11 `root_required`. A request with a different `client_commit` → exit 10, journal unchanged.
12. `upgrade.install`, with a second `e2etest` build B pushed to `/tmp/eru-b` as root:
    - wrong sha256 → exit 14; `/usr/local/bin/eru` unchanged; the temp file is gone;
    - correct sha256 → `/usr/local/bin/eru` reports B; `eru.prev` reports the original build;
    - a mismatched `client_commit` doesn't block it (the method is exempt).

The §22.2 table gains E26. E18 stays in the table, assigned to M3.

---

## 10. `docs/DEVIATIONS.md` entries

- Dev builds compare `commit` in the node-side version check (§4.2). The M3 handshake will use the same rule.
- RPC request adds `client_commit`, `client_channel`, `lock_timeout_s` (§4.1).
- `eru-rpc/v1` and `upgrade.install` are frozen (§4.4).
- `cluster.toml` is decoded leniently until M4 (§8).
- `eru.lock.info` names only the most recent shared holder (§5.4).
- §24 milestone split: M2 is node-only with E26. The handshake, binary push, E18 and E27 move to M3.

---

## 11. Implementation order

1. `internal/secrets`, `internal/rpc`, `internal/config`.
2. `internal/engine`: journal, locks, run log, `Shell`, runner, plan. `internal/steps` with the empty production catalogue and the e2etest stubs.
3. `internal/node`: dispatcher, `plan`, `apply`, `selfcheck`; `node rpc` / `node selfcheck` commands.
4. `upgrade.install`.
5. Harness + E26.
6. DEVIATIONS, §22.2/§24 updates, `just lint` + `just e2e` green.

---

## Appendix A: carried to M3

Worked out while reviewing M2. Not binding; the M3 spec takes it as a starting point.

**Contexts** (read side). Resolve `--context` › `ERU_CONTEXT` › `config.toml` `current_context`. Load `contexts/<name>/context.toml` (§6.3 fields plus `auto_upgrade_nodes`), `id_ed25519`, `known_hosts`. Strict decode. Missing context → `context_not_found`. One node per context until Phase 3 (`nodes[0]`).

**Transport.** A `Dialer` interface (`DialContext`). SSH with `x/crypto/ssh`, user `operator.ssh_user`, key `id_ed25519`, `knownhosts` callback. A key mismatch → `host_key_mismatch` with the fingerprint; dial/auth failures → `remote_unreachable` / `transport_failed`.

**Handshake.** `ssh exec: /usr/local/bin/eru node version --output json` without sudo. A missing binary → `precondition_failed` / `node_binary_missing`. Compare with the §4.2 rule. A mismatch → `node_version_mismatch` with `direction` ∈ `operator_older|operator_newer|unknown` (edge and dev have no order). Hint: `eru upgrade` when older, `--auto-upgrade-nodes` otherwise. Auto-upgrade never downgrades a node.

**RPC client.** `sudo -n /usr/local/bin/eru node rpc` (no sudo as root), write the request, close stdin, decode events. A malformed line or EOF before `result` → `transport_failed` / `rpc_stream_truncated`. stderr only with `--verbose`; `sudo: a password is required` → `precondition_failed` / `sudo_not_configured`. The final envelope is the node's result with the operator's `command` and `context`. Rendering: human = one line per `step` event, `log` lines with `--verbose`; `--stream` = node events verbatim plus the operator's result last; json/agent/markdown = final envelope only.

**Push and auto-upgrade.** Artifact for the node's arch: `release.Resolve` + `release.Fetch` for release builds; `ERU_NODE_BINARY` or `os.Executable()` (linux, same arch) for dev. Stream to `/tmp/eru-<version>-<rand>` with `umask 077`. With a node binary → `upgrade.install`. Without one (bootstrap) → `sha256sum -c` then `install` as root or `sudo -n`; a sudo password → `precondition_failed` / `root_required`. Re-run the handshake; still mismatched → `node_version_mismatch` / `install_did_not_take`. `--auto-upgrade-nodes` (flag or context) runs this on a mismatch or missing binary, continues the command, and adds a `node_upgraded{from, to}` warning.

**Scenarios.** E18 (version skew via `eru plan`, plus the node-side backstop) and E27 (auto-upgrade, tampered sha, bootstrap as root, bootstrap without sudo → `root_required`).
