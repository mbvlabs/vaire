# eru Phase 1: implementation spec

> Audience: the agent (or engineer) implementing eru Phase 0 + Phase 1.
> Source of truth for *what* and *why*: [`MASTER.md`](./MASTER.md). This document is the *how*.
> Status: ready to implement · 2026-10-09

---

## 0. How to use this document

1. Read `MASTER.md` in full first. This spec assumes its decisions (§3 there) and doesn't re-argue them.
2. Build the milestones in §24 **in order**. Each milestone ends with e2e scenarios that must pass before you move on.
3. Where this spec says **MUST**, it's fixed. Where it says **SHOULD**, deviate only with a note in `docs/DEVIATIONS.md` that gives the reason. Where it says **choose**, pick and record the choice in `docs/DEVIATIONS.md`.
4. Pin every external version and checksum when you implement (§23.3). Versions in this document are the floor known at writing time. Use the latest stable release at implementation time and record it in `internal/versions/versions.go`.
5. Re-read the reference code paths named here before copying patterns:
   - `/home/mbv/work/deploycrate-ce/internal/setup/` (step runner, embedded scripts, state, WireGuard, SSH, host, backup, node-exporter scripts)
   - `/home/mbv/work/deploycrate-ce/scripts/` (`install.sh`, `build-release.sh`, `sign-release.sh`, `upload-release.sh`)
   - `/home/mbv/work/andurel/cli/` (`output/output.go`, `command_meta.go`, `command_meta_catalog.go`, `discovery.go`, `agent_prompts.go`, `skill.go`)

### 0.1 Hard rules

- **No unit tests.** Behaviour is verified only by the end-to-end suite against real VMs (§22). Don't add `_test.go` files outside `e2e/`.
- Every host-mutating action is a step (§9) or a node RPC (§7.4). Nothing mutates a host through ad-hoc SSH commands.
- Secrets never appear in argv, logs, events, JSON output (unless explicitly requested by a `credentials` command), journal files or error messages. Pass them via stdin or 0600 files and redact on every output path (§9.6).
- Every command has `CommandMeta` (§5.3). The build-time check fails otherwise.
- Every bash script starts with `set -euo pipefail`, is idempotent, passes `shellcheck`, and receives inputs only via environment variables validated with `: "${VAR:?}"`.
- Supported server OS: Debian 12, Debian 13, Ubuntu 24.04, Ubuntu 26.04. amd64 and arm64.
- Supported operator OS: Linux and macOS, amd64 and arm64.

---

## 1. Scope

**In scope** (MASTER Phase 0 + Phase 1): single-node central command, operator CLI, WireGuard transport, secrets document, hardening, PostgreSQL 18, tuning, security, Caddy TLS, PgBouncer, pgBackRest DB backups and restores, restic VPS backups, DR rebuild, metrics exporters, maintenance, extensions v1, status/doctor/top, upgrade/rollback, `--local` install, agent guide and skill, e2e suite.

**Stretch** (do last; may slip to Phase 2 without blocking the release): Tailscale transport (M12).

**Out of scope:** multiple nodes, HA, replicas, operator add/revoke and scoped operators, provider snapshots, alerting, `eru mcp`, major PG upgrades.

---

## 2. Repository and toolchain

| Item | Value |
| --- | --- |
| Module | `github.com/mbvlabs/eru` |
| Go | 1.27.x (match andurel's `go` directive) |
| Task runner | `justfile` (`just build`, `just lint`, `just e2e`, `just release …`) |
| Tool pins | `mise.toml` (go, golangci-lint, shellcheck, cosign) |
| Lint | `golangci-lint` (default + `errorlint`, `gosec`), `shellcheck -x` on `internal/steps/scripts/*.sh` and `scripts/*.sh` |
| Build | `CGO_ENABLED=0`, `-trimpath`, `-ldflags "-s -w -X github.com/mbvlabs/eru/internal/buildinfo.Version=… -X …Commit=… -X …Channel=…"` |

### 2.1 Go dependencies (floors; use the latest stable)

| Purpose | Module |
| --- | --- |
| CLI | `github.com/spf13/cobra` (≥ 1.9.1), `github.com/spf13/pflag` |
| Prompts / TUI | `charm.land/huh/v2` (or the current huh major), `charm.land/bubbletea/v2`, `charm.land/lipgloss/v2`, `charm.land/bubbles/v2` (same majors as deploycrate) |
| TOML | `github.com/pelletier/go-toml/v2` |
| SSH | `golang.org/x/crypto/ssh`, `golang.org/x/crypto/ssh/agent`, `golang.org/x/crypto/ssh/knownhosts` |
| WireGuard (userspace) | `golang.zx2c4.com/wireguard` (`device`, `conn`, `tun/netstack`) |
| Postgres | `github.com/jackc/pgx/v5` |
| Secrets document | `filippo.io/age` (scrypt recipient) |
| Signature verification | `github.com/sigstore/sigstore-go` (verify cosign bundles in-process) |
| JQ filter | `github.com/itchyny/gojq` |
| Bcrypt (Caddy basic auth hash) | `golang.org/x/crypto/bcrypt` |

No other runtime dependencies without a `DEVIATIONS.md` entry.

### 2.2 Directory layout

```
cmd/eru/main.go
internal/buildinfo/          Version, Commit, Channel, ReleaseBaseURL
internal/versions/           pinned external versions + sha256 per arch
internal/cli/                cobra tree (one file per command group), CommandMeta registry, prompts
internal/cli/output/         output modes, envelope, error codes, exit codes (port of andurel/cli/output)
internal/localenv/           operator preflight, package-install offers, sudo escalation
internal/transport/          wg (userspace), ssh client, knownhosts, dialer abstraction
internal/remote/             binary push, version handshake, RPC client (NDJSON)
internal/rpc/                request/response/event types shared by operator and node
internal/node/               node-side RPC dispatcher + method handlers
internal/contexts/           ~/.config/eru layout, context files, pending-init state
internal/secretsdoc/         schema, age encrypt/decrypt, passphrase generation (EFF wordlist embedded)
internal/secrets/            secret generation (passwords, cipher passes), redaction registry
internal/config/             cluster.toml schema, defaults, validation, answers-file merge
internal/hostinfo/           OS, arch, RAM, vCPU, disks (rotational, size), kernel, public IP
internal/engine/             step interface, runner, journal, locks, plan, events
internal/steps/              step definitions (one file per step)
internal/steps/scripts/      embedded bash scripts
internal/render/             text/template renderers + templates/*.tmpl
internal/tune/               tuning calculator
internal/pg/                 pgx helpers: connect via socket/tunnel, roles, dbs, extensions, reports
internal/backup/             Driver interface, pgbackrest driver, restic (vps), drills, DR orchestration
internal/maint/              maintenance commands + SQL
internal/catalog/            extension catalog data
internal/doctor/             checks registry
internal/metrics/            textfile-collector writer
internal/upgrade/            release manifest fetch, sigstore verify, self-update, node upgrade/rollback
internal/guide/              agent guide markdown (embedded) + skill installer
e2e/                         end-to-end suite (build tag e2e)
scripts/                     install.sh, build-release.sh, sign-release.sh, upload-release.sh
.github/workflows/           publish-stable.yml, publish-edge.yml, e2e.yml
docs/                        MASTER.md, this file, DEVIATIONS.md, CLI.md (generated)
```

---

## 3. Binary modes

One binary. Mode is determined by the command path, not by detection:

- **Operator commands**: everything except `eru node …`. They run on a laptop, CI or a server in `--local` mode.
- **Node commands**: `eru node …` subtree, except `eru node upgrade`, which is operator-audience (§4.3). They run on a server, are mostly invoked over SSH by the operator, and are also called by systemd timers. `eru node` is hidden from top-level help but listed in `eru commands --json` with `"audience": "node"`.

An operator command against a remote cluster always runs the same thing:

1. Resolve the context (`--context` flag › `ERU_CONTEXT` › current context).
2. Bring up transport (§7).
3. Version handshake (§7.3).
4. Make a node RPC call (§7.4).
5. Render the result.

In `--local` mode (operator running on the server itself, §8.5), step 4 calls the node dispatcher in-process with root privileges and skips 2–3.

---

## 4. Command tree (Phase 1)

Legend: **M** = mutating, **D** = destructive (needs `--confirm <token>`), **DR** = supports `--dry-run`, **P** = may prompt in a TTY (every prompt has a flag).

### 4.1 Global flags (persistent)

`--context, -c <name>`, `--output human|json|agent|markdown`, `--json` (= `--output json`), `--jq <expr>`, `--quiet`, `--verbose`, `--stream` (NDJSON events), `--no-input` (never prompt; implied when stdin isn't a TTY), `--sudo` (pre-approve local escalation), `--install-missing` (pre-approve local package installs), `--timeout <dur>`.

### 4.2 Operator commands

| Command | Flags (beyond global) | M | D | DR | P | Confirm token |
| --- | --- | --- | --- | --- | --- | --- |
| `eru version` | | | | | | |
| `eru commands` | `--path <cmd path>` | | | | | |
| `eru guide agents` | | | | | | |
| `eru skill install` | `--harness <name>…`, `--dir` | ✓ | | ✓ | ✓ | |
| `eru login <secrets.age>` | `--name`, `--passphrase-stdin`, `--passphrase-file` | ✓ | | | ✓ | |
| `eru context list\|show\|use <n>\|remove <n>` | | (use/remove ✓) | remove | | | `<name>` |
| `eru cluster init` | §8.1 | ✓ | | ✓ | ✓ | `<cluster>` when `--no-input` |
| `eru plan` | | | | | | |
| `eru apply` | `--step <id>…` | ✓ | | ✓ | ✓ | `<cluster>` if any step needs a PG restart |
| `eru config show\|get <key>\|set <key> <value>\|edit\|validate` | `set`: `--apply` | set/edit ✓ | | ✓ | edit | |
| `eru status` | `--watch` | | | | | |
| `eru doctor` | `--only security\|backups\|host\|postgres\|tls` | | | | | |
| `eru top` | | | | | | |
| `eru queries` | `--limit`, `--order total\|mean\|calls`, `--reset` (D) | reset | reset | | | `<cluster>/stats` |
| `eru logs [unit]` | `--since`, `--follow`, `--lines` | | | | | |
| `eru sql <query>` | `--db`, `--file`, `--write`, `--role ro\|app\|admin` | `--write` | | | | |
| `eru psql` | `--db`, `--role` | | | | | |
| `eru tunnel` | `--local-port`, `--target pgbouncer\|postgres` | | | | | |
| `eru ssh` | `--command` | | | | | |
| `eru db list` / `create <name>` / `drop <name>` / `credentials <name>` | `create`: `--statement-timeout`, `--extensions`; `credentials`: `--rotate`, `--role` | create/drop/rotate | drop, rotate | ✓ | | `<db>` |
| `eru ext list` / `add <name>` / `remove <name>` | `--db` | add/remove | remove | ✓ | | `<cluster>/restart` if restart; `<db>` for remove |
| `eru tune show\|explain\|apply` | | apply | | ✓ | | `<cluster>/restart` if restart |
| `eru backup now` | `--type full\|diff\|incr` | ✓ | | ✓ | | |
| `eru backup list` / `verify` | | verify | | | | |
| `eru restore` | `--latest`, `--time`, `--set`, `--promote`, `--delta`, `--db <name>` | ✓ | ✓ | ✓ | ✓ | `<cluster>/restore` |
| `eru restore status\|promote\|abort\|cleanup` | | promote/abort/cleanup | abort, cleanup | ✓ | | `<cluster>/restore` |
| `eru restore drill` | `--time` | ✓ | | ✓ | | |
| `eru dump <db>` | `--to bucket\|local`, `--out <file>` | ✓ | | | | |
| `eru vps backup now\|list` | | now | | ✓ | | |
| `eru vps restore` | `--snapshot`, `--path…`, `--to <dir>` | ✓ | ✓ (in-place) | ✓ | | `<cluster>/vps-restore` |
| `eru maint vacuum\|analyze\|reindex\|repack` | §16 | ✓ | | ✓ | | repack: `<db>` |
| `eru maint report <kind>` | §16 | | | | | |
| `eru maint kill` | `--idle-in-tx`, `--pid`, `--older-than` | ✓ | ✓ | ✓ | | `<cluster>/kill` |
| `eru secrets export` | `--out`, `--passphrase-out` | ✓ | | | ✓ | |
| `eru wg export` | `--out`, `--install` | `--install` | | ✓ | ✓ | |
| `eru upgrade` | `--version`, `--channel`, `--skip-nodes` | ✓ | | ✓ | ✓ | |
| `eru node upgrade` | `--version`, `--rollback` | ✓ | rollback | ✓ | | `<cluster>/rollback` |
| `eru pg update` | `--now` | ✓ | | ✓ | | `<cluster>/restart` |

`eru node upgrade` is an **operator** command even though it lives under `eru node`. Audience is set per command in `CommandMeta`, not per subtree. Its node-side counterpart is the RPC method `upgrade.install`, so there's no name collision.

### 4.3 Node commands (`eru node …`)

| Command | Purpose |
| --- | --- |
| `eru node upgrade` | **Operator-audience**: see §4.2 and §19.2 |
| `eru node rpc` | Reads one JSON request on stdin, writes NDJSON events to stdout (§7.4). The only entry point the operator uses besides `version` |
| `eru node version` | Prints version JSON. Runs without sudo |
| `eru node selfcheck` | Binary can read config, the journal is readable, required tools are present |
| `eru node run <job>` | Timer entry points: `backup-full`, `backup-diff`, `backup-incr`, `backup-verify`, `restore-drill`, `vps-backup`, `maint-daily`, `maint-weekly`, `certs-sync`, `metrics-refresh` |
| `eru node ssh-revert` | Commit-confirm revert (§8.3), invoked by a transient systemd timer |
| `eru node unlock-ssh` | Break-glass: open public 22/tcp for `--minutes` (default 30) via a transient timer that re-closes it |

---

## 5. Output, errors and command metadata

Port `andurel/cli/output` (package `internal/cli/output`) and keep its API shape. The differences are below.

### 5.1 Envelope (`--output json|agent`)

Success:

```json
{
  "schema": "eru/v1",
  "ok": true,
  "command": "backup list",
  "context": "prod",
  "data": { },
  "warnings": [ { "code": "backup_stale", "message": "…" } ],
  "next": [ "eru backup verify" ]
}
```

Failure (also printed to stdout in structured modes; human mode prints to stderr):

```json
{
  "schema": "eru/v1",
  "ok": false,
  "command": "restore",
  "error": {
    "code": "unsafe_action_requires_confirmation",
    "message": "restore replaces the running cluster",
    "hint": "re-run with --confirm prod/restore",
    "details": { "confirm_token": "prod/restore" }
  }
}
```

- `agent` mode is `json`, plus each command's `CommandMeta.next` suggestions and terse `message`s.
- `markdown` renders tables for list-like data.
- `--jq` applies to the envelope.

### 5.2 Streaming (`--stream`, and always on the RPC wire)

NDJSON, one object per line:

```json
{"schema":"eru-event/v1","type":"step","step":"postgres-install-v1","state":"started","index":8,"total":18,"ts":"…"}
{"schema":"eru-event/v1","type":"log","step":"postgres-install-v1","line":"Setting up postgresql-18 …","ts":"…"}
{"schema":"eru-event/v1","type":"progress","phase":"restore","pct":42,"detail":"12.1/28.7 GiB","ts":"…"}
{"schema":"eru-event/v1","type":"result","envelope":{ "...": "§5.1 envelope" }}
```

The last line is always `type: result`. `type` ∈ `step|log|progress|warning|prompt-required|result`.

### 5.3 Error codes → exit codes

| Code | Exit |
| --- | --- |
| (success) | 0 |
| `usage_error`, `invalid_output_mode` | 1 |
| `context_not_found`, `cluster_not_initialized` | 2 |
| `missing_tool` | 3 |
| `unsafe_action_requires_confirmation` | 4 |
| `step_failed`, `partial_failure` | 5 |
| `external_command_failed` | 6 |
| `config_error`, `validation_failed` | 7 |
| `ambiguous_input`, `input_required` | 8 |
| `remote_unreachable`, `transport_failed`, `host_key_mismatch` | 9 |
| `node_version_mismatch`, `update_required` | 10 |
| `precondition_failed` (e.g. lock held, backup running, outside PITR window, insufficient disk) | 11 |
| `lockdown_failed` (commit-confirm reverted) | 12 |
| `secrets_error` (decrypt failed, bad passphrase) | 13 |
| `error` (generic) | 70 |

Codes are stable strings. Add new ones only at the end of a row group and list them in `eru guide agents`.

### 5.4 CommandMeta

Copy `andurel/cli/command_meta.go` and add these fields:

```go
type CommandMeta struct {
    // andurel fields: Group, Summary, Args, WhenToUse, WhenNotToUse, Prerequisites,
    // Next, Examples, Mutating, Destructive, SupportsDryRun, Prompts, InteractiveFlags
    Audience     string   // "operator" | "node"
    Remote       bool     // runs on a node via RPC
    ConfirmToken string   // template, e.g. "{cluster}/restore", "{db}"
    NeedsRestart string   // "never" | "maybe" | "always" (Postgres restart)
    ErrorCodes   []string // codes this command can return beyond the generic ones
}
```

- `eru commands --json` emits the catalog (as in andurel's `commandsDiscoveryPayload`).
- `just lint` runs `eru commands --check-meta` and fails on missing metadata or on mutating commands without `SupportsDryRun` (opt out with `SkipDryRunCheck` + reason).

### 5.5 Prompts and confirmation

Port `requireFlagInStructuredMode` and `confirmDestructiveAction` from `andurel/cli/agent_prompts.go`.

- If stdin isn't a TTY, `--no-input` is set, or the output mode is structured, a missing value returns `input_required`, naming the flag.
- Destructive commands require `--confirm <exact token>`. In a TTY without `--confirm`, prompt the user to type the token. `--yes` exists but **never** satisfies a destructive confirmation.

---

## 6. Configuration and files

### 6.1 `cluster.toml` (desired state; lives on the central command at `/etc/eru/cluster.toml`)

```toml
schema = 1
name = "prod"                       # [a-z][a-z0-9-]{1,30}; also pgBackRest stanza
domain = "data.example.com"         # FQDN; clients connect here
acme_email = "ops@example.com"
timezone = "UTC"

[tls]
mode = "acme"                       # acme | self-signed
acme_ca = ""                        # override directory URL (e2e: Pebble)

[transport]
kind = "wireguard"                  # wireguard (Phase 1); tailscale (M12)
subnet = "10.42.0.0/16"
listen_port = 51820

[[nodes]]
name = "prod-1"                     # defaults to "<name>-1"
role = "primary"
public_address = "203.0.113.10"     # IPv4 or IPv6 literal
wg_address = "10.42.0.1"

[server]
admin_user = "eru"                  # SSH + sudo user created on the node
swap_mb = 1024                      # 0 = don't create
data_directory = "/var/lib/postgresql/18/main"

[postgres]
version = 18
profile = "oltp"                    # oltp | mixed | analytics
client_connections = 1000           # PgBouncer max_client_conn
locale_provider = "builtin"         # builtin | icu | libc
locale = "C.UTF-8"
[postgres.settings]                 # explicit overrides, rendered last by eru (50-eru-settings.conf)
# "log_min_duration_statement" = "250ms"

[network]
client_allowlist = []               # CIDRs allowed to reach 6432 (and 5432 if expose_postgres)
expose_pgbouncer = true
expose_postgres = false

[[databases]]
name = "app"                        # [a-z_][a-z0-9_]{0,62}
statement_timeout = "60s"
extensions = ["pg_stat_statements"]

[backup]
provider = "r2"                     # r2 | s3 | b2 | hetzner | minio | custom
endpoint = "https://<account>.r2.cloudflarestorage.com"
region = "auto"
bucket = "eru-backups"
prefix = "prod"                     # no leading/trailing slash
uri_style = "path"                  # path | host (preset-derived)

[backup.database]
full_schedule = "Sun *-*-* 01:00:00"      # systemd OnCalendar syntax
diff_schedule = "Mon..Sat *-*-* 01:00:00"
incr_schedule = ""                        # e.g. "hourly"; empty = off
retention_full = 4
local_repo = false                        # repo2 on /var/lib/pgbackrest
verify_schedule = "Wed *-*-* 04:00:00"
drill_schedule = "*-*-01 05:00:00"

[backup.vps]
enabled = true
schedule = "Sun *-*-* 03:30:00"           # weekly by default, independent of DB
keep_weekly = 8
keep_monthly = 6
check_schedule = "*-*-15 04:30:00"        # monthly restic check
extra_paths = []

[maintenance]
window = "02:00-05:00"                    # local time per [timezone]
auto_repack = false
repack_bloat_threshold = 0.40

[metrics]
enabled = true
path_prefix = "/metrics"
```

**Validation** (`eru config validate`, run before every write and apply):

- Names and identifiers follow the regexes shown.
- `domain` is an FQDN. `acme_email` is valid when `tls.mode = "acme"`.
- Schedules parse with `systemd-analyze calendar`. Run it on the node. On the operator, use a Go parser for the subset and let the node have the final say.
- CIDRs parse. `0.0.0.0/0` in `client_allowlist` is allowed but produces a doctor `warn`.
- `retention_full ≥ 1`, `keep_weekly ≥ 1`.
- A database named `postgres`, `template0` or `template1` is rejected.
- Extensions exist in the catalog (§17).
- Phase 1 allows exactly one node with `role = "primary"`. More are rejected with `precondition_failed` and the hint "multi-node arrives in Phase 3".

`config set` uses dotted keys (`backup.vps.schedule`, `postgres.settings.work_mem`, `databases.app.statement_timeout`). Array-of-table entries are addressed by `name`.

**Answers file** (`--answers answers.toml`): the same schema as `cluster.toml`, plus a `[bootstrap]` table (`host`, `user`, `identity`, `host_key_fingerprint`) and a `[backup.credentials]` table (`access_key_id`, `secret_access_key`). The CLI strips credentials before persisting `cluster.toml` (they go to runtime secrets, §6.3).

### 6.2 Node files

| Path | Owner:mode | Content |
| --- | --- | --- |
| `/usr/local/bin/eru`, `/usr/local/bin/eru.prev` | root:root 0755 | Binary and rollback copy |
| `/etc/eru/node.toml` | root 0640 | `cluster`, `node_name`, `role`, `installed_at`. Marks a host as an eru node |
| `/etc/eru/cluster.toml` | root 0640 | Desired state |
| `/etc/eru/runtime/` | root 0700 | `backup.env` (S3 keys), `restic.env`, `pgbouncer_auth.pass`, `metrics.htpasswd` (bcrypt) |
| `/etc/eru/tls/` | root 0755 | `server.crt` (0644), `server.key` (root:ssl-cert 0640; `postgres` and `pgbouncer` are in `ssl-cert`) |
| `/var/lib/eru/journal.json` | root 0600 | §9.3 |
| `/var/lib/eru/state.json` | root 0600 | `secrets_confirmed_at`, `last_apply`, `restore` (in-progress restore record), `drills[]` (last 12) |
| `/var/lib/eru/drill/` | postgres 0700 | Scratch restores |
| `/var/lib/node_exporter/textfile/eru.prom` | root 0644 | §15.3 |
| `/var/log/eru/` | root 0750 | One log per RPC/job: `<UTC ts>-<method>-<request_id>.log`, rotated by logrotate (30 days) |
| `/run/lock/eru.lock` | | Global lock (§9.5) |

### 6.3 Operator files (`$XDG_CONFIG_HOME/eru`, default `~/.config/eru`, dir 0700)

```
config.toml                         current_context = "prod"
contexts/<name>/context.toml        cluster name, domain, nodes [{name, public_address, wg_address, wg_public_key, listen_port}],
                                    operator {name, wg_address, ssh_user}, created_at, eru_version
contexts/<name>/wg.key              operator WireGuard private key (0600)
contexts/<name>/id_ed25519          operator SSH private key (0600)
contexts/<name>/known_hosts         pinned server host keys (wg address entries)
contexts/<name>/credentials.json    DB + metrics credentials from the secrets document (0600)
pending/<name>/                     in-progress init: answers.toml, generated secrets (0600), progress.json
cache/releases/<version>/           downloaded artifacts + manifest + sigstore bundle
```

OS keychain storage is a later improvement. Files are 0600 like `~/.ssh`. Any file in `contexts/` with a looser mode triggers a warning and a `chmod` fix.

---

## 7. Transport and remote execution

### 7.1 Addressing

- Subnet `10.42.0.0/16`. Nodes use `10.42.0.0/24`, and the central command is `.1`. Operators use `10.42.255.0/24`. The first operator from `init` is `10.42.255.1`.
- Server: `wg0`, `ListenPort = 51820`. Each operator is a `[Peer]` with `AllowedIPs = <operator>/32` and no endpoint, because the operator initiates.

### 7.2 Operator-side userspace WireGuard

- Use `tun/netstack.CreateNetTUN([]netip.Addr{operatorAddr}, nil, 1420)` and `device.NewDevice(tun, conn.NewDefaultBind(), logger)`, configured via IPC (`private_key`, `public_key`, `endpoint`, `allowed_ip=10.42.0.0/24`, `persistent_keepalive_interval=25`).
- Expose a `transport.Dialer` interface with `DialContext(ctx, network, addr)`. The WireGuard implementation uses `netstack.Net.DialContext`. The local (`--local`) implementation uses `net.Dialer`.
- Bring up lazily per command. Tear down on exit. No persistent daemon.
- `eru tunnel` / `eru psql`: listen on `127.0.0.1:<port>` (default 6543 for pgbouncer and 6544 for postgres, or the first free port) and proxy accepted conns through the netstack dialer to `10.42.0.1:6432|5432`. `eru psql` execs the system `psql` with `PGHOST=127.0.0.1 PGPORT=<port> PGSSLMODE=verify-full PGSSLROOTCERT=system PGSSLSNI=1`. Set `PGHOSTADDR` to 127.0.0.1 and `PGHOST` to the domain so verify-full matches the cert. `PGPASSWORD` comes from `credentials.json` through the environment, never argv.

### 7.3 SSH and version handshake

- SSH client: `x/crypto/ssh`. Auth uses the operator `id_ed25519`, or for first contact `--identity`, `SSH_AUTH_SOCK`, or `--password-stdin`. Host key callback uses `knownhosts` from the context. First contact is TOFU: show the SHA256 fingerprint and require confirmation, or `--host-key-fingerprint SHA256:…`, or `--accept-new-host-key` (recorded as a warning). A mismatch returns `host_key_mismatch`.
- Handshake on every command:
  1. `ssh exec: /usr/local/bin/eru node version --output json`.
  2. Compare with `buildinfo.Version`. On mismatch, return `node_version_mismatch` with hint `eru node upgrade`. With `--auto-upgrade-nodes` or context `auto_upgrade_nodes = true`, perform §19.2 first.
  3. Dev builds (`Channel == "dev"`) skip the signature check and push the local `ERU_NODE_BINARY` (required env var when the operator OS/arch differs from the node).

### 7.4 Node RPC protocol

- Invocation: `ssh exec: sudo -n /usr/local/bin/eru node rpc`.
- **stdin**: one JSON line, then EOF:

  ```json
  {"schema":"eru-rpc/v1","request_id":"01J…","method":"backup.now","params":{"type":"full"},
   "secrets":{},"dry_run":false,"confirm":"","operator":"op-1","client_version":"1.0.0"}
  ```

- **stdout**: NDJSON events (§5.2). The final line is `result`. Exit status equals the envelope's exit code.
- **stderr**: free-form diagnostics. The operator surfaces it only with `--verbose`.
- **Methods** mirror operator commands, named `<group>.<verb>`: `cluster.bootstrap`, `apply`, `plan`, `config.get|set|validate`, `status`, `doctor`, `db.*`, `ext.*`, `tune.*`, `backup.*`, `restore.*`, `vps.*`, `maint.*`, `ssh.lockdown`, `ssh.confirm`, `certs.sync`, `logs`, `upgrade.install`, `upgrade.rollback`, `pg.update`, `sql`, `queries`, `top.sample`.
- **Confirmation is re-checked node-side.** The node recomputes the token and rejects mismatches, so a buggy operator can't skip it.
- **Read-only SQL** (`eru sql`, `top.sample`, `queries`, reports) runs over pgx through the tunnel, not via RPC, except where node-local access is needed (`postgres` peer for superuser-only views). RPC methods for these exist for `--local` mode.
- **Sudoers**: `/etc/sudoers.d/eru` contains `eru ALL=(root) NOPASSWD: /usr/local/bin/eru node *`. Validate it with `visudo -cf`.

### 7.5 Binary push

1. Operator obtains the node artifact for the node arch: from `cache/releases/<ver>/linux-<arch>/eru`, downloading and verifying it per §19.1 if missing, or from `ERU_NODE_BINARY` for dev.
2. Stream it via `ssh exec: cat > /tmp/eru-<ver>-<rand>` as the SSH user.
3. Run `sudo -n /usr/local/bin/eru node rpc` with `upgrade.install {path, sha256, version}`. The node verifies the sha256, then does `install -m 0755` to `/usr/local/bin/eru.new`, then `mv eru eru.prev && mv eru.new eru`.
4. **Bootstrap special case**: no node binary exists yet. As root (or `sudo -n`) the operator runs `install` directly after a `sha256sum` check executed remotely.

---

## 8. Cluster init and bootstrap

### 8.1 `eru cluster init` flags

`--host <user@ip[:port]>`, `--identity <path>`, `--password-stdin`, `--host-key-fingerprint`, `--accept-new-host-key`, `--answers <file>`, `--name`, `--domain`, `--acme-email`, `--profile`, `--client-connections`, `--database <name>` (repeatable), `--allow <cidr>` (repeatable), `--backup-provider`, `--backup-endpoint`, `--backup-region`, `--backup-bucket`, `--backup-prefix`, `--backup-key-id`, `--backup-secret-stdin` / `--backup-secret-file`, `--vps-backup-schedule`, `--db-full-schedule`, `--db-diff-schedule`, `--extensions`, `--tls-mode`, `--local`, `--restore <secrets.age>` (§13), `--restore-time <ts>`, `--secrets-out <path>`, `--passphrase-out <path|->`, `--confirm-secrets-stored`, `--resume`.

### 8.2 Remote init sequence

Each numbered phase emits `step` events and records progress in `pending/<name>/progress.json`. `--resume` continues from the last completed phase.

1. **Local preflight** (§20).
2. **Collect answers** from flags, the answers file, and prompts. Prompt only in a TTY, using `huh` forms grouped as in MASTER §6.1. Validate (§6.1).
3. **Generate secrets** on the operator (§10.2), stored in `pending/<name>/secrets.json` (0600). Generate the operator WireGuard keypair and SSH ed25519 key.
4. **Backup credentials check**: from the operator, do a signed `PUT`/`GET`/`DELETE` of `<prefix>/.eru-probe` using the S3 client in `internal/backup/s3probe.go` (minimal SigV4 over `net/http`; no SDK). This fails early on bad keys or a bad bucket. For a non-empty `<prefix>/pgbackrest` without `--restore`, return `precondition_failed` ("prefix already contains a backup repository").
5. **First contact** over public SSH. Detect arch and OS (`/etc/os-release`). Push the binary (§7.5, bootstrap case).
6. **RPC `cluster.bootstrap` phase=host** over public SSH. The node writes `cluster.toml` and `node.toml`, then runs hardening steps 1–6 (§11), which:
   - create the admin user with the operator SSH key and sudo password hash;
   - install WireGuard with the operator peer;
   - configure ufw with **public 22 still open**.
7. **Switch transport**: start userspace WireGuard to `public_address:51820` and SSH to `10.42.0.1:22` as `admin_user`. Pin the host key (same host keys, new address entry). Run the handshake.
8. **Commit-confirm lockdown** (§8.3).
9. **RPC `cluster.bootstrap` phase=database** over WireGuard: steps 7–18 (§11). Runtime secrets are passed in `secrets`.
10. **Secrets document** (§10): write to `--secrets-out` (default `./eru-<name>.secrets.age`, 0600) and display the passphrase once. In a TTY, require typing `stored`; otherwise require `--confirm-secrets-stored` together with `--passphrase-out`. Then RPC `cluster.confirm-secrets` sets `secrets_confirmed_at`.
11. **Create the local context** from the pending data and delete `pending/<name>`.
12. Print the summary: connection strings, doctor result, first DB and VPS backup labels.

### 8.3 Commit-confirm SSH lockdown

The node implements RPC `ssh.lockdown {timeout_s: 120}`:

1. Snapshot the current state to `/var/lib/eru/ssh-snapshot/`: `/etc/ssh/sshd_config.d/99-eru.conf` and `ufw status numbered` output (parsed to rule specs).
2. Arm the revert: `systemd-run --unit=eru-ssh-revert --on-active=<timeout> /usr/local/bin/eru node ssh-revert`.
3. Apply:
   - write the final sshd drop-in (§11 step 4) and run `sshd -t`, then `systemctl reload ssh`;
   - `ufw allow in on wg0 to any port 22 proto tcp`;
   - `ufw delete allow 22/tcp` (public).
4. Return `armed: true`.

The operator then **opens a new** SSH connection over WireGuard and calls `ssh.confirm`, which runs `systemctl stop eru-ssh-revert.timer` and deletes the snapshot.

- If the new connection fails within 60 s, the operator waits for `timeout + 15 s`, reconnects over public SSH, reads `ssh-revert`'s result from `/var/lib/eru/ssh-snapshot/reverted.json`, and returns `lockdown_failed` with diagnostics.
- `ssh-revert` restores the drop-in and ufw rules from the snapshot and reloads ssh.

The SSH restriction is enforced with **ufw** (22/tcp allowed only `in on wg0`), not `ListenAddress`. That avoids boot-ordering lockouts when wg0 comes up after sshd. This is the documented implementation of MASTER's "sshd listens only on wg0".

- **Ubuntu socket activation**: when `ssh.socket` is enabled (Ubuntu ≥ 22.10), the hardening step MUST `systemctl disable --now ssh.socket && systemctl enable --now ssh.service` so sshd config applies and doesn't rely on socket units.

### 8.4 Break-glass

`eru node unlock-ssh --minutes 30`, run from the provider console as root:

1. `ufw allow 22/tcp` and enable `PasswordAuthentication yes` for `admin_user` only (`Match User`).
2. Arm `systemd-run --on-active=30m eru node ssh-revert --lockdown-only`.

The secrets document's `break_glass` text describes this flow.

### 8.5 `--local` mode

`curl -fsSL <base>/install.sh | sudo bash` on the server installs `/usr/local/bin/eru`. It prompts "Initialise a cluster on this server now? [y/N]" when a TTY is present, which runs `eru cluster init --local`.

`eru cluster init --local`:

- requires root, or escalates via §20.3;
- skips first contact and binary push and uses the in-process dispatcher;
- generates secrets on the server and runs all steps;
- writes the secrets document to `/root/eru-<name>.secrets.age` (0600) and prints the passphrase once, plus a one-line `eru login` instruction: "copy the file to your machine, run `eru login eru-<name>.secrets.age`".

For lockdown in `--local`, the commit-confirm verification needs an operator over WireGuard. So `--local` arms the revert timer with `timeout = 30 min` and prints "run `eru login …` then any `eru status` from your machine within 30 minutes to confirm".

- The first successful remote RPC from an operator calls `ssh.confirm` automatically when a lockdown is pending. This is recorded in `state.json.lockdown_pending`.
- `--keep-public-ssh-until-confirm` is the default and only behaviour in `--local`.

---

## 9. Step engine

### 9.1 Interface

Adapt `deploycrate-ce/internal/setup/runner.go`:

```go
type Step interface {
    ID() string                          // versioned, e.g. "postgres-config-v1"
    Describe(cfg config.Cluster) string
    Inputs(cfg config.Cluster, rt Runtime) ([]byte, error) // canonical bytes of everything Apply depends on (rendered files, versions, env); secrets hashed, never raw
    Check(ctx context.Context, cfg config.Cluster, rt Runtime) (CheckResult, error)
    Apply(ctx context.Context, cfg config.Cluster, rt Runtime, report Reporter) (ApplyResult, error)
}
type CheckResult struct{ Complete bool; Detail string }
type ApplyResult struct{ Restart RestartNeed; Notes []string }       // RestartNeed: none|reload|restart
type Runtime struct {
    DryRun  bool
    Shell   Shell            // runs embedded scripts (deploycrate Shell + secrets redaction + per-run log)
    Secrets secrets.Runtime  // values available to this run
    Host    hostinfo.Info
    PG      *pg.Local        // lazily connects via unix socket as postgres
}
```

### 9.2 Runner semantics

For each step in order:

1. Compute `inputs_hash = sha256(ID || Inputs())`.
2. If the journal has `{id, status: completed, inputs_hash}` that matches, the step is skipped (`state: skipped`, reason `journal`).
3. Otherwise call `Check`. If complete **and** the hash matches the journal, skip (reason `check`). A complete check with a changed hash still runs `Apply`, because the files must be re-rendered.
4. Otherwise `Apply`, then journal `{status, inputs_hash, completed_at, restart}`.

Restart needs are aggregated. The runner performs at most **one** Postgres restart at the end, and only if `--confirm <cluster>/restart` was supplied (remote-validated). Otherwise it reloads and reports `restart_pending: [settings…]`. Reloads happen immediately per step.

`plan` runs steps 1–3 and lists those that would apply, plus file diffs from `Inputs()` against the on-disk files (unified diff, secrets redacted).

### 9.3 Journal (`/var/lib/eru/journal.json`)

```json
{"schema":1,"eru_version":"1.0.0","updated_at":"…",
 "steps":{"postgres-config-v1":{"status":"completed","inputs_hash":"…","started_at":"…","completed_at":"…","restart":"reload","error":""}}}
```

Write it atomically (temp file plus rename). Steps removed from the catalog are left as-is and ignored.

### 9.4 Embedded scripts

Use `//go:embed scripts/*.sh`. Run each script via `/bin/bash -s` with the env map, as in deploycrate `Shell.Run`. The combined output is streamed line by line as `log` events, redacted, and appended to the run log.

Rendered files are written by Go (`render` package) with an atomic write and owner/mode set, **not** by heredocs, for anything with a template. Scripts handle packages, services, users and ufw.

### 9.5 Locks

`/run/lock/eru.lock` uses `flock`:

- **exclusive**: `apply`, `restore.*` (except status), `upgrade.*`, `pg.update`, `ext.add/remove`, `tune.apply`, `vps.restore`, `restore-drill`;
- **shared**: `backup.*`, `vps.backup`, `maint.*`, `db.*`.

If a lock can't be acquired within `--timeout` (default 30 s; timers use 0), return `precondition_failed` with the holder (PID, method, start time, written to `/run/lock/eru.lock.info`).

Maintenance additionally refuses while a pgBackRest backup is running. Check `pgbackrest --stanza=<s> info --output=json`'s `status.lock.backup.held`.

### 9.6 Redaction

`secrets.Registry` collects every secret value loaded or generated in the process. All output paths (events, logs, errors, envelopes) pass through `Redact`, which replaces values of 6 or more characters with `«redacted»`.

---

## 10. Secrets

### 10.1 Secrets document

The file is `age` scrypt-encrypted (`age.NewScryptRecipient(passphrase)`, work factor 18). The plaintext is JSON:

```json
{
  "schema": "eru-secrets/v1",
  "created_at": "…", "eru_version": "1.0.0",
  "cluster": { "name": "prod", "domain": "data.example.com", "stanza": "prod" },
  "nodes": [ { "name": "prod-1", "role": "primary", "public_address": "203.0.113.10", "listen_port": 51820,
               "wg_address": "10.42.0.1", "wg_public_key": "…", "ssh_host_keys": ["ssh-ed25519 AAAA…"] } ],
  "operator": { "name": "op-1", "wg_address": "10.42.255.1", "wg_private_key": "…",
                "ssh_user": "eru", "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----…", "sudo_password": "…" },
  "database": {
    "admin": { "user": "eru_admin", "password": "…" },
    "databases": [ { "name": "app",
      "roles": { "migrate": {"user":"app_migrate","password":"…"}, "app": {"user":"app_app","password":"…"}, "ro": {"user":"app_ro","password":"…"} },
      "urls": { "app": "postgres://app_app:…@data.example.com:6432/app?sslmode=verify-full&sslrootcert=system",
                "migrate": "postgres://app_migrate:…@data.example.com:5432/app?sslmode=verify-full&sslrootcert=system" } } ]
  },
  "backup": { "provider": "r2", "endpoint": "…", "region": "auto", "bucket": "…", "prefix": "prod", "uri_style": "path",
              "access_key_id": "…", "secret_access_key": "…",
              "pgbackrest": { "stanza": "prod", "cipher_pass": "…" }, "restic": { "password": "…" } },
  "metrics": { "base_url": "https://data.example.com/metrics", "username": "metrics", "password": "…" },
  "break_glass": "…markdown text…"
}
```

- **Passphrase**: 8 words from the EFF large wordlist (embedded, about 103 bits), joined with `-`. It's displayed once and never stored by eru.
- **`eru login`**: decrypt (passphrase from TTY, `--passphrase-stdin` or `--passphrase-file`) and create `contexts/<name>/…`. Bad passphrase returns `secrets_error`.
- **`eru secrets export`**: rebuilds the document from the context. Credentials rotated since login require a re-login or the `credentials` command. It generates a new passphrase.

### 10.2 Generated secrets

| Secret | Generator | Where it lives after init |
| --- | --- | --- |
| Admin user sudo password | 24 chars base62 | Doc; node has `/etc/shadow` hash only |
| `eru_admin`, per-db `migrate`/`app`/`ro` passwords | 32 chars base62 | Doc; node has PG SCRAM verifiers only |
| `pgbouncer_auth` password | 32 chars base62 | Doc + node `/etc/pgbouncer/userlist.txt` (SCRAM verifier form `SCRAM-SHA-256$…`) |
| pgBackRest `cipher_pass` | 64 chars base62 | Doc + node `pgbackrest.conf` |
| restic password | 64 chars base62 | Doc + node `restic.env` |
| Metrics password | 32 chars base62 | Doc + node bcrypt hash in Caddyfile |
| `eru_monitor` password (PgBouncer stats only) | 32 chars base62 | Node only: `/etc/eru/runtime/monitor.pass`, `userlist.txt` SCRAM verifier; PG role keeps peer access |
| Bucket keys | user-provided | Doc + node `backup.env`, `pgbackrest.conf`, `restic.env` |

Use `crypto/rand` only.

---

## 11. Step catalogue

Order is fixed. IDs end in `-v1`; bump the suffix whenever behaviour changes in a way the input hash can't capture (for example, script logic changes). Columns: what it does, and how Check decides completeness.

| # | ID | Does | Check |
| --- | --- | --- | --- |
| 1 | `base-packages-v1` | Wait for `cloud-init status --wait`. apt update and install: `curl ca-certificates gnupg apt-transport-https sudo ufw fail2ban wireguard-tools chrony logrotate unattended-upgrades jq lsb-release` | All installed (dpkg-query) |
| 2 | `admin-user-v1` | Create `admin_user` (shell bash, groups `sudo`), set the password hash, install the operator SSH pubkey to `authorized_keys`, write `/etc/sudoers.d/eru` (§7.4) and validate it | User exists, key present, sudoers matches |
| 3 | `wireguard-v1` | Adapt deploycrate `wireguard.sh` (no ip_forward: not a gateway). Address `10.42.0.1/16`; operator peers rendered from `cluster.toml` + operator registry (`/etc/eru/operators.toml`: name, wg pubkey, address). `ufw allow 51820/udp` | `wg show wg0` lists all peers |
| 4 | `sshd-hardening-v1` | Write `/etc/ssh/sshd_config.d/99-eru.conf` (below). Handle `ssh.socket` (§8.3). `sshd -t`, reload. Public 22 stays open until lockdown | Drop-in matches render |
| 5 | `firewall-v1` | ufw: `default deny incoming`, `default allow outgoing`, allow 51820/udp, 80/tcp, 443/tcp, `in on wg0` 22/tcp, 6432/tcp from wg0 + allowlist (if `expose_pgbouncer`), 5432/tcp from wg0 + allowlist (if `expose_postgres`). Keep public 22 only if `lockdown_pending` or not yet locked. `ufw --force enable` | Parsed `ufw status` matches the expected rule set |
| 6 | `host-baseline-v1` | journald drop-in (copy deploycrate `host.sh`), swapfile (`swap_mb`), timezone, chrony enabled, unattended-upgrades security-only with `Package-Blacklist` `postgresql-`, `libpq`, `pgbouncer`, `pgbackrest`, `caddy` and `Automatic-Reboot "false"`, fail2ban sshd jail, logrotate for `/var/log/eru` | Files match render, services active |
| 7 | `kernel-tuning-v1` | `/etc/sysctl.d/90-eru.conf` and the `eru-thp.service` oneshot (below). `vm.nr_hugepages` from §12.3 | sysctl values equal render |
| 8 | `postgres-packages-v1` | Write `/etc/postgresql-common/createcluster.d/00-eru.conf` with `create_main_cluster = false` **before** installing. Install `postgresql-common`, run `/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y`, install `postgresql-18 postgresql-client-18 postgresql-contrib pgbouncer pgbackrest`, then `apt-mark hold postgresql-18 postgresql-client-18` | Packages installed and held |
| 9 | `postgres-cluster-v1` | If no cluster exists: `pg_createcluster 18 main --datadir=<data_directory> --locale-provider=<provider> --locale=<locale> --start-conf=auto -- --auth-local=peer --auth-host=scram-sha-256` (checksums are default-on in 18; assert `data_checksums = on` after start). systemd drop-in for `postgresql@18-main.service`: `LimitNOFILE=65536`, `OOMScoreAdjust=-900`, `Environment=PG_OOM_ADJUST_FILE=/proc/self/oom_score_adj PG_OOM_ADJUST_VALUE=0` | `pg_lsclusters` shows 18/main online |
| 10 | `postgres-config-v1` | Render `conf.d/10-eru-tuning.conf` (§12), `20-eru-security.conf`, `30-eru-extensions.conf` (§17), `40-eru-backup.conf` (archive settings, §13.1), `50-eru-settings.conf` (`[postgres.settings]`), and create `90-overrides.conf` (empty, comment header) if missing. Render `pg_hba.conf` and `pg_ident.conf`. Ensure `include_dir = 'conf.d'` is in `postgresql.conf`. Then `SELECT name FROM pg_file_settings WHERE error IS NOT NULL` must be empty before reload. Classify changes as reload or restart via `pg_settings.context` (`postmaster` means restart) | Rendered = on disk |
| 11 | `postgres-roles-v1` | §14 SQL (idempotent DO blocks); per-db roles, databases, default privileges, `pgbouncer_auth` function | All roles, dbs and grants exist |
| 12 | `tls-v1` | Install Caddy (copy deploycrate's install approach and pinned version). Render the Caddyfile (§15.2). Before the first ACME cert, generate a self-signed cert into `/etc/eru/tls` so PG/PgBouncer can start. Install `eru-certs-sync.path` (watches Caddy's cert dir) and `eru-certs-sync.timer` (daily); both run `eru node run certs-sync`, which copies the cert and key into `/etc/eru/tls`, sets modes, and reloads PG (`pg_ctlcluster 18 main reload`) and PgBouncer (`systemctl reload pgbouncer`) when they changed | Caddy active, `/etc/eru/tls/server.crt` exists |
| 13 | `pgbouncer-v1` | Render `/etc/pgbouncer/pgbouncer.ini` (§14.3) and `userlist.txt`. Enable and restart | Rendered = on disk, `SHOW VERSION` works |
| 14 | `pgbackrest-v1` | Render `/etc/pgbackrest/pgbackrest.conf` (§13.1). `pgbackrest --stanza=<s> stanza-create`, then `check`. If the journal has no `first-full`, run `backup --type=full` (streams progress) | `check` passes, first full exists |
| 15 | `restic-v1` | Install the pinned restic (copy deploycrate `backup.sh` checksum pattern). Render `restic.env`. `restic init` if needed (`restic cat config` decides). First snapshot (§13.6) | Repo reachable, ≥ 1 snapshot |
| 16 | `metrics-v1` | Install pinned `node_exporter` (copy deploycrate script; textfile collector dir `/var/lib/node_exporter/textfile`), `postgres_exporter`, `pgbouncer_exporter`, `pgbackrest_exporter` as systemd services bound to 127.0.0.1 (§15.1). The Caddy routes are already in the Caddyfile | All exporters return 200 locally |
| 17 | `timers-v1` | Render systemd `.service`/`.timer` pairs for every schedule in `cluster.toml` (§18). `systemctl daemon-reload`, enable timers, disable removed ones (`eru-*.timer` not in the render) | Timer set matches |
| 18 | `finalize-v1` | Run doctor (§21). Fail the step if any check is `fail`. Write metrics textfile. Upload `cluster.toml` as a restic snapshot tagged `eru-config` | Never complete (always runs; cheap) |

`cluster.bootstrap phase=host` runs steps 1–6. `phase=database` runs 7–18. `apply` runs 1–18.

### 11.1 Fixed file contents

`/etc/ssh/sshd_config.d/99-eru.conf`:

```
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
PermitEmptyPasswords no
X11Forwarding no
AllowTcpForwarding no
AllowAgentForwarding no
AllowUsers {{ .AdminUser }}
MaxAuthTries 3
MaxSessions 10
LoginGraceTime 30
ClientAliveInterval 300
ClientAliveCountMax 2
```

`MaxSessions 10` because operator commands open several sessions. `AllowTcpForwarding no` is fine since eru tunnels through WireGuard, not SSH.

`/etc/sysctl.d/90-eru.conf`:

```
vm.swappiness = 1
vm.overcommit_memory = {{ if ge .RAMMiB 4096 }}2{{ else }}0{{ end }}
{{ if ge .RAMMiB 4096 }}vm.overcommit_ratio = 90{{ end }}
vm.dirty_background_bytes = 67108864
vm.dirty_bytes = 536870912
vm.nr_hugepages = {{ .HugePages }}
kernel.sched_autogroup_enabled = 0
net.core.somaxconn = 1024
net.ipv4.tcp_keepalive_time = 300
```

`eru-thp.service` (oneshot, `WantedBy=multi-user.target`, `Before=postgresql.service`) writes `never` to `/sys/kernel/mm/transparent_hugepage/{enabled,defrag}`.

---

## 12. Tuning calculator (`internal/tune`)

`func Calculate(in Input) Result`, where

- `Input{RAMMiB, VCPU, DiskRotational, DataDiskGiB, Profile, ClientConnections, PGMajor, Extensions}`;
- `Result{Settings []Setting}`, with `Setting{Name, Value, Unit, Reason, Context}` (`Context` mirrors `pg_settings.context` for restart classification).

It's pure, with no I/O. `hostinfo` supplies the inputs; for the data disk, use the block device backing `data_directory` (`findmnt -no SOURCE`, then `/sys/block/<dev>/queue/rotational`, resolving partitions and device-mapper to the parent).

### 12.1 Formulas

Let `R = RAMMiB`, `C = VCPU`, and `P ∈ {oltp, mixed, analytics}`.

| Setting | Value |
| --- | --- |
| `shared_buffers` | `R < 2048 ? max(128, R*0.20) : min(R*0.25, 65536)` MiB |
| `effective_cache_size` | `R * (P==analytics ? 0.75 : 0.70)` MiB |
| `max_connections` | `clamp(4*C, 50, 400)` (PgBouncer fans in) |
| `superuser_reserved_connections` / `reserved_connections` | 3 / 3 (`eru_admin` gets `pg_use_reserved_connections`) |
| `work_mem` | `(R - shared_buffers) / (max_connections * f)` with `f = {oltp:3, mixed:2, analytics:1}`, clamped to `[4, P==analytics ? 1024 : 256]` MiB |
| `hash_mem_multiplier` | 2.0 |
| `maintenance_work_mem` | `clamp(R/16, 64, 2048)` MiB |
| `autovacuum_work_mem` | `min(maintenance_work_mem, 1024)` MiB |
| `huge_pages` | `try` if `R ≥ 8192`, else `off` |
| `wal_level` | `replica` |
| `wal_compression` | `zstd` |
| `wal_buffers` | `-1` |
| `min_wal_size` | `clamp(DataDiskGiB*0.02, 1, 8)` GiB |
| `max_wal_size` | `clamp(DataDiskGiB*0.10, 2, 64)` GiB |
| `checkpoint_timeout` | `15min` |
| `checkpoint_completion_target` | `0.9` |
| `random_page_cost` | SSD 1.1 / HDD 4.0 |
| `effective_io_concurrency` / `maintenance_io_concurrency` | SSD 200/200, HDD 2/10 |
| `io_method` | `worker` (PG18 default; `io_uring` is an explicit `[postgres.settings]` opt-in) |
| `io_workers` | `max(3, C/4)` |
| `max_worker_processes` | `C + 8` (min 8) |
| `max_parallel_workers` | `C` |
| `max_parallel_workers_per_gather` | oltp `min(2, C/2)`, mixed `min(4, C/2)`, analytics `min(8, C/2)` (min 0) |
| `max_parallel_maintenance_workers` | `min(4, max(1, C/2))` |
| `jit` | `off` unless analytics |
| `default_statistics_target` | analytics 500, else 100 |
| `autovacuum_worker_slots` | 16 |
| `autovacuum_max_workers` | `clamp(C/2, 3, 8)` |
| `autovacuum_naptime` | `30s` |
| `autovacuum_vacuum_scale_factor` / `autovacuum_vacuum_insert_scale_factor` / `autovacuum_analyze_scale_factor` | 0.05 / 0.05 / 0.02 |
| `autovacuum_vacuum_cost_limit` | SSD 2000 / HDD 400 |
| `track_io_timing` | `on` |
| `idle_in_transaction_session_timeout` | `10min` |
| `log_min_duration_statement` | oltp `500ms`, mixed `1s`, analytics `5s` |

### 12.2 Explanations

Every `Setting.Reason` is one sentence naming the inputs, for example "25% of 16384 MiB RAM". `eru tune explain` prints the inputs and then a table of name, value, previous value and reason.

### 12.3 Huge pages

- During initial cluster creation, and on restart-requiring retunes, stop PG and run `postgres -D <datadir> -c config_file=/etc/postgresql/18/main/postgresql.conf -C shared_memory_size_in_huge_pages`. Set `vm.nr_hugepages` to that value + 5%.
- Otherwise estimate `ceil(shared_buffers_MiB * 1.05 / 2) + 64`.
- When `huge_pages = off`, set it to 0.

---

## 13. Backups and restores

### 13.1 pgBackRest configuration

`/etc/pgbackrest/pgbackrest.conf` (postgres:postgres 0600):

```ini
[global]
repo1-type=s3
repo1-s3-endpoint={{ .EndpointHost }}
repo1-s3-bucket={{ .Bucket }}
repo1-s3-region={{ .Region }}
repo1-s3-uri-style={{ .URIStyle }}
repo1-s3-key={{ .KeyID }}
repo1-s3-key-secret={{ .Secret }}
repo1-path=/{{ .Prefix }}/pgbackrest
repo1-cipher-type=aes-256-cbc
repo1-cipher-pass={{ .CipherPass }}
repo1-retention-full={{ .RetentionFull }}
repo1-bundle=y
repo1-block=y
{{ if .LocalRepo }}repo2-path=/var/lib/pgbackrest
repo2-retention-full=1
{{ end }}compress-type=zst
process-max={{ max 1 (div .VCPU 2) }}
start-fast=y
archive-async=y
spool-path=/var/spool/pgbackrest
archive-push-queue-max=4GiB
log-level-console=info
log-level-file=detail
delta=y

[{{ .Stanza }}]
pg1-path={{ .DataDirectory }}
pg1-socket-path=/var/run/postgresql
```

`40-eru-backup.conf`:

```
archive_mode = on
archive_command = 'pgbackrest --stanza={{ .Stanza }} archive-push %p'
archive_timeout = 60s
max_wal_senders = 10
```

`archive_timeout = 60s` bounds RPO on idle systems.

**Provider presets**, used to derive defaults in the interview (`endpoint` and `region` are still editable):

| Provider | URI style | Region | Endpoint template |
| --- | --- | --- | --- |
| r2 | path | `auto` | `https://<account_id>.r2.cloudflarestorage.com` |
| s3 | host | asked | `s3.<region>.amazonaws.com` |
| b2 | host | asked | `s3.<region>.backblazeb2.com` |
| hetzner | host | asked (`fsn1`, `nbg1`, `hel1`) | `<region>.your-objectstorage.com` |
| minio | path | `us-east-1` | asked |
| custom | asked | asked | asked |

### 13.2 Backup commands

| RPC | Implementation |
| --- | --- |
| `backup.now` | `pgbackrest --stanza=s --type=<t> backup`. Parse progress from `--log-level-console=detail` lines into `progress` events. Afterwards, write textfile metrics |
| `backup.list` | `pgbackrest --stanza=s info --output=json` → `data.backups[] {label, type, start, stop, size, repo_size, wal_start, wal_stop, database_count}` and `data.pitr_window {from, to}` (`to` = last archived WAL time from `pg_stat_archiver.last_archived_time`) |
| `backup.verify` | `pgbackrest --stanza=s verify --output=json` (with `--set` latest for the weekly timer). Record the result in `state.json` + metrics |

### 13.3 In-place restore (`restore`)

**Preconditions:**

- the exclusive lock is held;
- no restore is already in progress (`state.json.restore`);
- the target lies within the PITR window (for `--time`), or the set exists (for `--set`);
- free space on the data filesystem ≥ (latest backup `size` × 1.2), unless `--delta`.

**Algorithm:**

1. Record `state.json.restore = {id, target, started_at, phase}`. Update `phase` after each step below.
2. Don't take a pre-restore backup. The current state is preserved by moving the data dir aside (step 4), which is faster and also covers a broken current state.
3. `systemctl stop pgbouncer`, then `pg_ctlcluster 18 main stop -m fast`.
4. Unless `--delta`, run `mv <datadir> <datadir>.pre-restore-<UTC ts>` and `install -d -o postgres -g postgres -m 0700 <datadir>`.
5. Run `pgbackrest --stanza=s restore` with `--type=time --target=<ts> --target-action=pause --target-timeline=current` (or `--type=default` for `--latest`, or `--set=<label> --type=immediate` for `--set`), plus `--delta` when requested. Stream progress.
6. `pg_ctlcluster 18 main start`. Poll `pg_is_in_recovery()` and `pg_get_wal_replay_pause_state()` until `paused`, or until recovery ends for `--latest`.
7. Report `{state: paused|promoted, replayed_to: pg_last_xact_replay_timestamp()}`. PgBouncer stays **stopped** while paused, so applications don't write to an inspection copy.
8. If `--promote` (or later `eru restore promote`): `SELECT pg_wal_replay_resume()` then `pg_promote(wait => true)`, start PgBouncer, run `backup --type=full` immediately (new timeline), and clear `state.json.restore` (keeping the `.pre-restore-*` path recorded under `restore_leftovers`).

`restore abort`:

- stop PG;
- move the restored datadir to `<datadir>.aborted-<ts>`;
- move `.pre-restore-*` back;
- start PG and PgBouncer.

This is only valid before `--delta` and only while a pre-restore dir exists.

`restore cleanup` (destructive) deletes `restore_leftovers` paths after confirmation.

### 13.4 Restore drill

1. `pgbackrest --stanza=s restore --pg1-path=/var/lib/eru/drill/<ts> --type=default --target-action=promote --archive-mode=off`. Check free space first; if it's insufficient, return `skipped` with reason `insufficient_space` (a `warn` in doctor).
2. Start a throwaway instance: `pg_ctl -D <dir> -o "-p 5499 -c listen_addresses='' -c unix_socket_directories='/var/lib/eru/drill/sock' -c archive_mode=off -c shared_buffers=128MB -c max_connections=20 -c huge_pages=off" -w start`. Run it as `postgres` with `PGDATA` config. Since Debian keeps config outside the datadir, pass `-c config_file=/dev/null -c hba_file=<generated minimal hba: local all postgres peer>`.
3. Checks:
   - `SELECT count(*) FROM pg_database`;
   - for each non-template database, `SELECT 1` and `pg_amcheck --database=<db> --heapallindexed=false --install-missing=false` (timeout 30 min; skip amcheck if the extension isn't installed);
   - compare the database list with live.
4. Stop and `rm -rf` the drill directory.
5. Write the result `{started_at, finished_at, backup_label, recovered_to, checks[], result: pass|fail|skipped}` to `state.json.drills` (keep 12) and metrics.

`archive_mode = off` is essential: a drill MUST never push WAL to the repo.

### 13.5 Single-database restore (`restore --db <name>`)

1. Run a drill-style restore to scratch with `--type=time` and the target. Keep the instance running.
2. `pg_dump -Fc -h <drill sock> -p 5499 <name>`, then `createdb <name>_restored_<yyyymmddhhmm> -O <name>_owner` on live, then `pg_restore --no-owner --role=<name>_owner -d <new>`.
3. Tear down scratch. Report the new database name.

This isn't destructive, so no `--confirm` is needed. It's exclusive-locked.

### 13.6 VPS backups (restic)

- `restic.env`:

  ```
  RESTIC_REPOSITORY=s3:<endpoint>/<bucket>/<prefix>/restic
  RESTIC_PASSWORD=…
  AWS_ACCESS_KEY_ID=…
  AWS_SECRET_ACCESS_KEY=…
  AWS_DEFAULT_REGION=<region>
  ```

  Add `RESTIC_COMPRESSION=auto`, and use `-o s3.bucket-lookup=dns|path` derived from `uri_style`.
- Include list: `/etc`, `/var/lib/eru`, `/var/lib/caddy`, `/root`, `/home`, `/usr/local/bin/eru`, plus `extra_paths`.
- Exclude list: `<data_directory>`, `/var/lib/postgresql`, `/var/lib/pgbackrest`, `/var/spool/pgbackrest`, `/var/lib/eru/drill`, `/var/cache`, `/var/log`, `/swapfile`, `/proc`, `/sys`, `/dev`, `/run`, `/tmp`.
- Job `vps-backup`: `restic backup --tag eru-vps --host <node> --files-from … --exclude-file …`, then `restic forget --tag eru-vps --keep-weekly N --keep-monthly M --prune`.
- Job `vps-check` (monthly): `restic check --read-data-subset=5%`.
- `eru vps backup list` → `restic snapshots --json --tag eru-vps`.
- `eru vps restore --snapshot <id> --path <p>… --to <dir>` restores into `--to` (default `/var/lib/eru/vps-restore/<id>`). Without `--to` and with `--in-place`, it's destructive and restores over the live paths given (requires `--confirm <cluster>/vps-restore`).
- Config snapshots: `restic backup --tag eru-config --stdin --stdin-filename cluster.toml` after each successful `apply`.
- `eru upgrade` and `eru pg update` take a `vps-backup` first.

### 13.7 DR: `eru cluster init --restore <secrets.age> --host root@<new-ip>`

1. Decrypt the document. Probe the bucket. Find the latest `eru-config` snapshot and load `cluster.toml` from it. Override `nodes[0].public_address` with the new host IP.
2. First contact and binary push (§8.2 steps 5–6), with these differences:
   - The operator keys come from the document, not newly generated.
   - Before step 3 (`wireguard-v1`), restore the allowlist paths from the latest `eru-vps` snapshot to their live locations: `/etc/wireguard`, `/etc/ssh/ssh_host_*`, `/etc/eru`, `/var/lib/caddy`, `/root`, `/home`, `extra_paths`. Use `restic restore --include` into a temp dir and copy with ownership. The server keeps its WireGuard identity and host keys, so existing contexts keep working once the public address is updated.
3. Lockdown (§8.3), then steps 7–13 with **`postgres-cluster-v1` in restore mode**. Create the cluster, stop it, empty the datadir, run `pgbackrest restore` (`--type=default`, or `--type=time --target=<--restore-time>` with `--target-action=promote`), and start it.
4. Steps 14–18 (stanza exists; `check`, then an immediate full backup on the new timeline).
5. Update the local context's `public_address`. Re-export the secrets document with the new address and a new passphrase. Print a DNS reminder if the A record of `domain` doesn't resolve to the new IP.

---

## 14. Roles, databases and PgBouncer

### 14.1 `20-eru-security.conf`

```
listen_addresses = '*'
port = 5432
ssl = on
ssl_cert_file = '/etc/eru/tls/server.crt'
ssl_key_file = '/etc/eru/tls/server.key'
ssl_min_protocol_version = 'TLSv1.2'
password_encryption = 'scram-sha-256'
allow_alter_system = off
log_line_prefix = '%m [%p] %q%u@%d %h '
log_connections = 'receipt,authentication,authorization'
log_disconnections = on
log_lock_waits = on
log_temp_files = '10MB'
log_checkpoints = on
log_autovacuum_min_duration = '10s'
```

Network exposure is controlled by ufw (§11 step 5) and pg_hba.

`pg_hba.conf`:

```
# managed by eru — do not edit; use cluster.toml
local   all             postgres                                peer
local   all             eru_monitor                             peer map=eru
local   all             eru_admin                               peer map=eru
local   all             all                                     scram-sha-256
hostssl all             postgres        0.0.0.0/0               reject
hostssl all             postgres        ::/0                    reject
hostssl all             all             127.0.0.1/32            scram-sha-256
hostssl all             all             {{ .WGSubnet }}         scram-sha-256
{{ range .Allowlist }}hostssl all       all             {{ . }}  scram-sha-256
{{ end }}host    all             all             0.0.0.0/0               reject
host    all             all             ::/0                    reject
```

`pg_ident.conf` (map `eru`): `<admin_user> → eru_admin`, `root → eru_admin`, `postgres_exporter → eru_monitor`, `postgres → eru_monitor` (pgbackrest_exporter runs as `postgres`).

pg_hba stops at the first line whose type, database and user match. If the peer check then fails, the connection is rejected; it doesn't fall through to the next line. That's why peer lines name specific users and the final `local all all scram-sha-256` line serves every other role (PgBouncer connecting app roles over the socket).

### 14.2 Role and database SQL (idempotent)

Cluster-level, in database `postgres`:

```sql
-- admin
CREATE ROLE eru_admin LOGIN CREATEDB CREATEROLE PASSWORD :'pw';            -- if not exists (DO block)
GRANT pg_monitor, pg_signal_backend, pg_maintain, pg_use_reserved_connections TO eru_admin;
-- monitor
CREATE ROLE eru_monitor LOGIN PASSWORD :'monitor_pw'; GRANT pg_monitor TO eru_monitor;  -- password only used by PgBouncer stats; PG access stays peer
-- pgbouncer auth
CREATE ROLE pgbouncer_auth LOGIN PASSWORD :'pw';
CREATE SCHEMA IF NOT EXISTS eru AUTHORIZATION postgres;
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
$$;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA eru TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth(text) TO pgbouncer_auth;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
GRANT CONNECT ON DATABASE postgres TO pgbouncer_auth, eru_admin, eru_monitor;
```

Per database `<d>`:

```sql
CREATE ROLE <d>_owner NOLOGIN;
CREATE ROLE <d>_migrate LOGIN PASSWORD :'pw' IN ROLE <d>_owner;
ALTER ROLE <d>_migrate SET role = '<d>_owner';              -- objects created by migrations are owned by <d>_owner
CREATE ROLE <d>_app LOGIN PASSWORD :'pw';
ALTER ROLE <d>_app SET statement_timeout = '<statement_timeout>';
CREATE ROLE <d>_ro LOGIN PASSWORD :'pw';
ALTER ROLE <d>_ro SET default_transaction_read_only = on;
CREATE DATABASE <d> OWNER <d>_owner;                         -- outside a transaction
REVOKE ALL ON DATABASE <d> FROM PUBLIC;
GRANT CONNECT, TEMPORARY ON DATABASE <d> TO <d>_migrate, <d>_app, <d>_ro, eru_admin, eru_monitor;
GRANT <d>_owner TO eru_admin WITH ADMIN OPTION;
-- inside <d>:
GRANT USAGE ON SCHEMA public TO <d>_app, <d>_ro;
ALTER DEFAULT PRIVILEGES FOR ROLE <d>_owner IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO <d>_app;
ALTER DEFAULT PRIVILEGES FOR ROLE <d>_owner IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO <d>_app;
ALTER DEFAULT PRIVILEGES FOR ROLE <d>_owner IN SCHEMA public GRANT EXECUTE ON FUNCTIONS TO <d>_app;
ALTER DEFAULT PRIVILEGES FOR ROLE <d>_owner IN SCHEMA public GRANT SELECT ON TABLES TO <d>_ro;
CREATE EXTENSION IF NOT EXISTS ... ;                         -- per [databases.extensions], as postgres
```

- **`eru db drop <d>`**: `DROP DATABASE <d> WITH (FORCE)`, then drop the roles. The confirm token is `<d>`. Take a `backup --type=incr` first.
- **`eru db credentials <d> --rotate --role app`**: generate a password, `ALTER ROLE … PASSWORD`, print once, and update `contexts/<name>/credentials.json`. Warn that the secrets document is stale and suggest `eru secrets export`.

Role names are validated by regex and also quoted with `pgx.Identifier{}.Sanitize()`. Passwords are always bind parameters or `format('%L')` inside DO blocks executed via pgx with parameters, never string-interpolated.

### 14.3 `pgbouncer.ini`

```ini
[databases]
* = host=/var/run/postgresql port=5432

[pgbouncer]
listen_addr = *
listen_port = 6432
unix_socket_dir = /var/run/postgresql
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
auth_user = pgbouncer_auth
auth_dbname = postgres
auth_query = SELECT usename, passwd FROM eru.pgbouncer_get_auth($1)
pool_mode = transaction
max_prepared_statements = 200
max_client_conn = {{ .ClientConnections }}
default_pool_size = {{ clamp (mul 2 .VCPU) 10 50 }}
min_pool_size = 0
reserve_pool_size = 5
max_db_connections = {{ sub .MaxConnections 20 }}
server_reset_query =
ignore_startup_parameters = extra_float_digits,search_path,options
client_tls_sslmode = require
client_tls_cert_file = /etc/eru/tls/server.crt
client_tls_key_file = /etc/eru/tls/server.key
client_tls_protocols = tlsv1.2,tlsv1.3
server_tls_sslmode = disable
admin_users = eru_admin
stats_users = eru_monitor
log_connections = 1
log_disconnections = 1
```

`userlist.txt` holds only `pgbouncer_auth` and `eru_monitor`, each with its SCRAM verifier read back from `pg_authid` after role creation.

---

## 15. TLS, metrics and Caddy

### 15.1 Exporters

| Exporter | Listen | Runs as | DB access |
| --- | --- | --- | --- |
| node_exporter | 127.0.0.1:9100 | `node_exporter` | `--collector.textfile.directory=/var/lib/node_exporter/textfile` |
| postgres_exporter | 127.0.0.1:9187 | `postgres_exporter` (system user) | `DATA_SOURCE_NAME=postgresql:///postgres?host=/var/run/postgresql&user=eru_monitor` (peer via ident map) |
| pgbouncer_exporter | 127.0.0.1:9127 | `pgbouncer_exporter` | `postgres://eru_monitor:<pw>@/pgbouncer?host=/var/run/postgresql&port=6432`. `eru_monitor` is in PgBouncer `stats_users` and authenticates against `userlist.txt` (it's excluded from `auth_query`). Its password lives in `/etc/eru/runtime/monitor.pass` and is passed via an `EnvironmentFile` |
| pgbackrest_exporter | 127.0.0.1:9854 | `postgres` (needs repo config) | `--backrest.config=/etc/pgbackrest/pgbackrest.conf`, collection interval 600 s |

Each exporter is a downloaded pinned binary with a sha256 per arch, following the `node-exporter.sh` pattern. Unit hardening: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ReadOnlyPaths=/`.

### 15.2 Caddyfile

```
{
    email {{ .ACMEEmail }}
    {{ if .ACMECA }}acme_ca {{ .ACMECA }}{{ end }}
    admin localhost:2019
}

{{ .Domain }} {
    {{ if eq .TLSMode "self-signed" }}tls internal{{ end }}
    handle_path {{ .MetricsPrefix }}/node*       { basic_auth { metrics {{ .MetricsHash }} } reverse_proxy 127.0.0.1:9100 }
    handle_path {{ .MetricsPrefix }}/postgres*   { basic_auth { metrics {{ .MetricsHash }} } reverse_proxy 127.0.0.1:9187 }
    handle_path {{ .MetricsPrefix }}/pgbouncer*  { basic_auth { metrics {{ .MetricsHash }} } reverse_proxy 127.0.0.1:9127 }
    handle_path {{ .MetricsPrefix }}/pgbackrest* { basic_auth { metrics {{ .MetricsHash }} } reverse_proxy 127.0.0.1:9854 }
    handle { respond 404 }
}
```

- Each path proxies to the exporter's `/metrics`. Use `rewrite * /metrics` inside each handle.
- The `certs-sync` source is Caddy's storage path for the domain, discovered via `find /var/lib/caddy/.local/share/caddy/certificates -path "*/<domain>/<domain>.crt"`.
- With `tls internal`, `certs-sync` copies Caddy's internal-CA leaf instead. Clients then need `sslmode=require` (the doc's URLs switch accordingly, and doctor warns).

### 15.3 eru textfile metrics (`eru.prom`)

```
eru_info{version,cluster,node} 1
eru_backup_last_success_timestamp_seconds{type="full|diff|incr"}
eru_backup_verify_last_success_timestamp_seconds
eru_restore_drill_last_run_timestamp_seconds
eru_restore_drill_last_result{result="pass|fail|skipped"} 1
eru_vps_backup_last_success_timestamp_seconds
eru_tls_cert_expiry_timestamp_seconds
eru_doctor_check{check,status} 1
eru_restore_in_progress 0|1
```

---

## 16. Maintenance (`internal/maint`)

Every maintenance session sets `SET lock_timeout = '5s'; SET statement_timeout = 0;` (vacuum and reindex can be long). Commands run as `eru_admin` (`pg_maintain`) over the tunnel, or via the socket in timers.

| Command | Flags | Implementation |
| --- | --- | --- |
| `maint vacuum` | `--db` (default all), `--table`, `--analyze`, `--freeze`, `--parallel N` | `VACUUM (VERBOSE false, ANALYZE x, FREEZE y, PARALLEL n) <table>`, or `vacuumdb --all --jobs=min(4,C)` via node RPC when there's no table |
| `maint analyze` | `--db`, `--table` | `ANALYZE` / `vacuumdb --analyze-only` |
| `maint reindex` | `--db`, `--table`/`--index`, `--concurrently` (required in Phase 1) | `REINDEX (CONCURRENTLY) …`; on failure, reports leftover `_ccnew` invalid indexes and offers `--cleanup-invalid` |
| `maint repack` | `--db`, `--table`, `--confirm <db>` | Requires the `pg_repack` extension in the db (otherwise `precondition_failed` with the hint `eru ext add pg_repack --db <db>`). Runs `pg_repack --no-superuser-check` as postgres over the socket. Respects `maintenance.window` unless `--now` |
| `maint report bloat` | `--db`, `--min-bytes` | Estimation query (the ioguix table + btree estimates, vendored with attribution) → `{table, est_bloat_bytes, ratio}` |
| `maint report wraparound` | | `datname, age(datfrozenxid), mxid_age(datminmxid)` + top-20 tables by `age(relfrozenxid)`. `warn` above 1.0e9, `fail` above 1.6e9 |
| `maint report unused-indexes` | `--db` | `pg_stat_user_indexes` with `idx_scan = 0`, not unique/PK/constraint-backing, size > 1 MiB, stats reset > 7 days ago |
| `maint report missing-indexes` | `--db` | `pg_stat_user_tables`: `seq_scan > 1000 AND seq_tup_read / greatest(seq_scan,1) > 10000 AND n_live_tup > 100000` |
| `maint report long-queries` | `--older-than 5m` | `pg_stat_activity` (state, duration, wait event, query truncated to 500 chars) |
| `maint report locks` | | Blocking tree via `pg_blocking_pids()` |
| `maint kill` | `--pid`, `--idle-in-tx`, `--older-than` | `pg_terminate_backend`. Never targets `postgres`/`eru_*` backends unless `--pid` is explicit |

**Timer jobs:**

- `maint-daily`: wraparound report → metrics + doctor state, then `ANALYZE` on tables with `n_mod_since_analyze > 0.1 * n_live_tup`, within the window.
- `maint-weekly`: bloat report → state. If `auto_repack` is set, repack tables above `repack_bloat_threshold` that have `pg_repack` installed, one table at a time, stopping at the window end.

---

## 17. Extension catalog v1 (`internal/catalog`)

```go
type Extension struct {
    Name, ExtName, Package, Repo string // Repo: "pgdg" | "contrib" | "timescale"
    Preload    string              // value added to shared_preload_libraries ("" if none)
    Settings   map[string]string   // rendered into 30-eru-extensions.conf
    Restart    bool                // derived: Preload != ""
    PerCluster bool                // e.g. pg_cron: lives in one database
    Default    bool                // enabled on init
    Note       string
}
```

| Name | Package | Preload | Notes |
| --- | --- | --- | --- |
| pg_stat_statements | contrib | `pg_stat_statements` | Default; `pg_stat_statements.max=10000`, `track=top` |
| auto_explain | contrib | `auto_explain` | Default; not a CREATE EXTENSION. `log_min_duration` = 2× `log_min_duration_statement`, `sample_rate=0.1`, `log_analyze=off` |
| pg_trgm, citext, btree_gin, btree_gist, pgstattuple, pg_buffercache, amcheck | contrib | | |
| vector | `postgresql-18-pgvector` | | |
| postgis | `postgresql-18-postgis-3` | | Large install |
| pg_cron | `postgresql-18-cron` | `pg_cron` | `cron.database_name` = first database; PerCluster |
| pgaudit | `postgresql-18-pgaudit` | `pgaudit` | `pgaudit.log='ddl,role'` default |
| pg_repack | `postgresql-18-repack` | | Required for `maint repack` |
| pg_partman | `postgresql-18-partman` | `pg_partman_bgw` (optional) | |
| hypopg | `postgresql-18-hypopg` | | |
| timescaledb | `timescaledb-2-postgresql-18` (Timescale apt repo) | `timescaledb` | Timescale licence; adds a repo; mark `Note` |

At implementation time, verify each package exists for PG18 on every supported OS/arch. Drop any that don't and record that in `DEVIATIONS.md`.

**`ext add <name> [--db <d>]`:**

1. Install the package (adding the repo if needed).
2. If there's a preload, re-render `30-eru-extensions.conf`. That requires a restart, so `--confirm <cluster>/restart` is needed; without it, stop after the package install and report `restart_required`.
3. Run `CREATE EXTENSION IF NOT EXISTS <ext>` in `--db` (default: all `[[databases]]`).
4. Persist to `cluster.toml`.

**`ext remove`**: `DROP EXTENSION` (no CASCADE unless `--cascade`; confirm `<db>`), then remove it from preload if no database uses it any more.

**`ext list`**: catalog × installed (`pg_available_extensions` and per-db `pg_extension`) → `{name, available, installed_in[], version, default_version, update_available}`.

---

## 18. Timers

Every job uses a pair of units named `eru-<job>.service` and `eru-<job>.timer`:

```
[Unit]
Description=eru {{ .Job }}
After=postgresql.service network-online.target
[Service]
Type=oneshot
ExecStart=/usr/local/bin/eru node run {{ .Job }}
Nice=10
IOSchedulingClass=best-effort
IOSchedulingPriority=7
```

```
[Timer]
OnCalendar={{ .Schedule }}
RandomizedDelaySec={{ .Jitter }}
Persistent=true
[Install]
WantedBy=timers.target
```

| Job | Schedule source | Jitter |
| --- | --- | --- |
| backup-full | `backup.database.full_schedule` | 5m |
| backup-diff | `backup.database.diff_schedule` | 5m |
| backup-incr | `backup.database.incr_schedule` (omit if empty) | 2m |
| backup-verify | `backup.database.verify_schedule` | 10m |
| restore-drill | `backup.database.drill_schedule` | 30m |
| vps-backup | `backup.vps.schedule` (omit if `enabled = false`) | 10m |
| vps-check | `backup.vps.check_schedule` | 30m |
| maint-daily | daily at window start | 10m |
| maint-weekly | Saturday at window start | 10m |
| certs-sync | daily 03:00 (+ the `.path` unit) | 30m |
| metrics-refresh | every 5 min | 0 |

`node run <job>` takes the right lock with `--timeout 0`. If the lock is busy it logs, exits 0 and writes a `skipped` metric. Busy isn't a failure, but repeated skips show up in doctor.

---

## 19. Upgrades

### 19.1 Releases and verification

- Copy the deploycrate release model: a manifest at `<ReleaseBaseURL>/manifest.json` with a sigstore bundle `manifest.json.sigstore.json`, signed keyless by GitHub Actions OIDC (`publish-stable.yml` on tags `vX.Y.Z`, `publish-edge.yml` on `main`). Channel layout, the default branch, and the live domain are recorded in [`docs/DEVIATIONS.md`](./DEVIATIONS.md).
- Manifest schema:

  ```json
  {"schemaVersion":1,"channel":"stable","version":"1.0.0","releaseBaseUrl":"…/releases/1.0.0",
   "artifacts":{"linux/amd64":{"url":"…","sha256":"…"},"linux/arm64":{},"darwin/amd64":{},"darwin/arm64":{}}}
  ```

- In-process verification uses `sigstore-go` against the Sigstore public-good trust root. The certificate identity regexp and OIDC issuer are exactly as in deploycrate `install.sh`, with the repository `mbvlabs/eru`.
- `ReleaseBaseURL` is a build-time constant. The production value is `https://eru.mbvlabs.com` (see [`docs/DEVIATIONS.md`](./DEVIATIONS.md)).

### 19.2 Commands

`eru upgrade`:

1. Fetch and verify the manifest, then download, verify (sha256) and replace the local binary atomically. On macOS, if the binary lives in a root-owned dir, escalate via §20.3.
2. Unless `--skip-nodes`, re-exec the new binary with `node upgrade`.

`eru node upgrade [--version v]`:

1. Run `vps-backup` (shared lock).
2. Push and install via §7.5.
3. Run `eru node selfcheck`. If it fails, roll back automatically and return `step_failed`.
4. Run `apply`, which re-runs only steps whose hash or ID changed. If any step needs a PG restart, report `restart_pending` and don't restart without `--confirm`.

`--rollback` swaps `eru.prev` back (confirm `<cluster>/rollback`). Only one previous version is kept.

`eru pg update`:

1. `vps-backup`, then `backup --type=incr`.
2. `apt-mark unhold`, `apt-get install --only-upgrade postgresql-18 postgresql-client-18 postgresql-contrib pgbouncer pgbackrest`, then `apt-mark hold`.
3. Restart PG at the next window start (scheduled via `systemd-run --on-calendar`), or now with `--now --confirm <cluster>/restart`.
4. Report versions before and after.

---

## 20. Local preflight and privilege escalation (`internal/localenv`)

### 20.1 Checks per command class

| Command class | Requirements |
| --- | --- |
| Any remote command | Context exists, key files are 0600, UDP egress (no check, just a hint on `transport_failed`) |
| `cluster init` (remote) | Target reachable on 22/tcp. An SSH credential: `--identity`, agent keys, or `--password-stdin` |
| `psql` | `psql` binary ≥ 16 on PATH |
| `wg export --install` | `wireguard-tools` (`wg`, `wg-quick`) and root |
| `cluster init --local` | Root (or escalation), a supported OS, no existing `/etc/eru/node.toml` |

### 20.2 Install offers

On a missing tool, eru detects the package manager (`apt-get`, `dnf`, `pacman`, `brew`) and builds the exact command, for example `sudo apt-get install -y postgresql-client` or `brew install libpq`.

- In a TTY: prompt "Install postgresql-client with `<cmd>`? [y/N]".
- Without a TTY: return `missing_tool` with `details.install_command`. With `--install-missing`, run it (escalating via §20.3 when needed).

### 20.3 Sudo escalation

When the current process isn't root and an operation needs root:

1. Try `sudo -n true`. If that works, proceed (passwordless).
2. In a TTY: prompt "eru needs root to <reason>. Escalate with sudo? [y/N]". On yes, run `sudo <self> <same args> --escalated` with stdio inherited. sudo prompts for the password itself.
3. Without a TTY: return `precondition_failed` (`details.reason`, `details.command`) unless `--sudo` is given. With `--sudo`, attempt only `sudo -n`, never an interactive password.
4. The escalated child sees `--escalated` and doesn't escalate again. Escalation applies to that single subprocess, never to the parent session.

---

## 21. Status and doctor

**`status` data:**

- `cluster`, `node`, `role`, `eru_version`, `pg_version`, `uptime`;
- `connections {used, max, by_state}` and `pgbouncer {cl_active, cl_waiting, sv_active, sv_idle}`;
- `cache_hit_ratio`, `tps` (delta over 1 s), `db_sizes[]`;
- `disk {data_free_pct, wal_bytes}`;
- `backup {last_full, last_diff, last_incr, pitr_window, archive_failed_count, last_archived_age_s}`;
- `vps_backup {last_snapshot}`, `drill {last_result, at}`, `tls {not_after}`;
- `restart_pending[]`, `restore_in_progress`.

**Doctor checks** (each returns `{id, status: pass|warn|fail, detail, fix}`; `fix` is an eru command when one exists):

| ID | Fail / warn when |
| --- | --- |
| `host.os_supported` | fail: unsupported |
| `host.reboot_required` | warn: `/var/run/reboot-required` exists |
| `host.disk_data` | warn < 20% free, fail < 10% |
| `host.unattended_upgrades` | warn: inactive |
| `host.time_sync` | fail: chrony unsynced |
| `ssh.public_closed` | fail: public 22 allowed and no lockdown pending |
| `ssh.config` | fail: drop-in differs |
| `fw.active` / `fw.rules` | fail: ufw inactive or rules differ |
| `pg.running` | fail |
| `pg.config_drift` | warn: rendered ≠ disk, or `90-overrides.conf` non-empty (lists the overridden settings) |
| `pg.restart_pending` | warn: `pg_settings.pending_restart` |
| `pg.alter_system_off` | fail |
| `pg.hba_no_trust` | fail: any `trust` / `password` / `md5` line |
| `pg.ssl` | fail: `ssl = off` |
| `pg.checksums` | warn: off |
| `pg.wraparound` | warn > 1.0e9, fail > 1.6e9 |
| `pg.connections` | warn > 80% of `max_connections` |
| `tls.expiry` | warn < 21 days, fail < 7 days |
| `tls.dns` | warn: A/AAAA of `domain` ≠ node public address |
| `tls.mode` | warn: self-signed |
| `backup.archiving` | fail: `pg_stat_archiver.failed_count` increased in the last hour, or last archived > 10 min with WAL activity |
| `backup.full_age` | warn > schedule interval + 1 day, fail > 2× |
| `backup.verify` | warn: last verify failed or older than 14 days |
| `backup.drill` | warn: no pass in 40 days; fail: last result fail |
| `vps.backup_age` | warn > 8 days, fail > 15 days (when enabled) |
| `secrets.confirmed` | fail: `secrets_confirmed_at` empty |
| `eru.version_skew` | warn: operator ≠ node (operator side) |
| `metrics.endpoint` | warn: any exporter not responding |

---

## 22. End-to-end test suite (`e2e/`, build tag `e2e`)

### 22.1 Harness

- **VMs**: `incus` VMs (`incus launch images:debian/13 <name> --vm -c limits.memory=2GiB -c limits.cpu=2`). The image matrix is Debian 12/13 and Ubuntu 24.04/26.04. arm64 runs nightly on arm64 runners.
- **Object storage**: a MinIO container on the CI host, with a bucket created per test run.
- **ACME**: Pebble (`ghcr.io/letsencrypt/pebble`) + `pebble-challtestsrv`. The test sets `tls.acme_ca` to Pebble's directory and resolves `domain` to the VM via challtestsrv's DNS. The VM trusts Pebble's root (test-only `ERU_TEST_EXTRA_CA` env read by the `tls` step, compiled in only with the `e2etest` build tag).
- **Operator**: the CI host runs `eru` built from the commit. `ERU_NODE_BINARY` points at the linux build. The operator connects to VM bridge IPs, which are "public" for the test.
- **Helpers**:
  - `harness.NewVM(t, image)` and `harness.Snapshot(vm)` / `Restore(vm)` (incus snapshots, to reuse an initialised node across scenarios);
  - `harness.Eru(t, args...) Envelope` (always `--output json`; asserts schema);
  - `harness.SQL(t, ctx, query)`;
  - `harness.KillTunnel()` (fault injection: block UDP 51820 on the VM via nft for commit-confirm tests).
- **CI**: `.github/workflows/e2e.yml` on Linux runners with KVM (`/dev/kvm` available on GitHub-hosted standard runners). The full matrix runs nightly. Per PR, run Debian 13 amd64 only.

### 22.2 Scenarios

Each scenario is a Go test asserting envelopes, exit codes and host state.

| ID | Scenario | Key assertions |
| --- | --- | --- |
| E01 | `cluster init` (answers file, `--no-input`) on a fresh VM | Exit 0; secrets doc decrypts; context exists; doctor has no `fail`; public 22 closed (TCP connect to public:22 times out); SSH over WG works; `eru sql "select 1"` works; app URL connects through PgBouncer with `verify-full` |
| E02 | Re-run `eru apply` | All steps skipped; no service restarts (PG `pg_postmaster_start_time` unchanged) |
| E03 | Commit-confirm failure: drop UDP 51820 during lockdown | `lockdown_failed`, exit 12; public SSH works again after revert; re-running init with `--resume` succeeds |
| E04 | `eru login` on a second operator home (`XDG_CONFIG_HOME` temp) | Works; wrong passphrase gives exit 13 |
| E05 | `--local` install via `install.sh` piped into the VM, then `eru login` on the operator | Context works, lockdown auto-confirms on the first RPC |
| E06 | Tuning: `incus config set limits.memory=8GiB`, reboot, `eru tune apply --confirm prod/restart` | `shared_buffers` = 2 GiB; `restart_pending` cleared |
| E07 | `config set postgres.settings.work_mem 64MB` + `apply` | Reload only; value active |
| E08 | Backups: `backup now --type full`, insert rows (record time T), `--type diff`, `backup list` | PITR window covers T; `verify` passes |
| E09 | PITR: insert rows after T, `restore --time T --confirm prod/restore` | Paused; rows after T absent; PgBouncer stopped; `restore promote` → writable; new full backup taken |
| E10 | `restore abort` | Original data back, including the post-T rows |
| E11 | `restore drill` | `pass`; repo WAL count unchanged by the drill (`archive_mode=off` proven) |
| E12 | `restore --db app` | `app_restored_*` exists with data at the target |
| E13 | DR: destroy the VM, new VM, `cluster init --restore doc --host root@new` | Data equals the pre-destroy checksum; WG pubkey and host keys preserved; old context (address updated) works |
| E14 | VPS backup: `vps backup now`, `vps backup list`, `vps restore --path /etc/eru --to /tmp/x` | Snapshot exists; restored file equals live; the weekly timer is `OnCalendar=Sun …` by default |
| E15 | Extensions: `ext add vector --db app` (no restart); `ext add pg_cron` without confirm → `restart_required`, then with confirm | `CREATE EXTENSION` visible; preload set |
| E16 | Maintenance: create bloat, `maint report bloat`, `maint repack --db app --table t --confirm app` (after adding pg_repack), `maint reindex --concurrently` | Size drops; no invalid indexes |
| E17 | Node upgrade: build version N+1 (dev), `node upgrade`, then `--rollback --confirm prod/rollback` | Versions switch; `eru.prev` present; `apply` re-ran only changed steps |
| E18 | Version skew: old operator against a new node | `node_version_mismatch`, exit 10 |
| E19 | Destructive guard: `db drop app` with `--yes` only | Exit 4, `unsafe_action_requires_confirmation` |
| E20 | Agent contract: run E01 + E08 + E09 using only `eru commands --json`, `--output agent`, and the `next` hints (scripted, no hardcoded flags beyond meta) | Completes; every envelope validates against the JSON schema in `e2e/schema/eru-v1.json` |
| E21 | Security: from the CI host, `psql` as `postgres` over TCP → rejected; `trust` absent; `ALTER SYSTEM` → error | All pass |
| E22 | Metrics: `curl -u metrics:… https://domain/metrics/postgres` (Pebble CA) → 200 with `pg_up 1`; without auth → 401 | Pass |
| E23 | `pg update` on a VM with an older pinned minor available (use the PGDG archive pin) | Version increased after `--now --confirm` |
| E24 | Local preflight: `eru psql` without psql installed, `--no-input` | `missing_tool` with `install_command` |

---

## 23. Release engineering

### 23.1 Scripts (copy and adapt from deploycrate)

- `scripts/build-release.sh VERSION CHANNEL BASE_URL`: builds `linux/{amd64,arm64}` and `darwin/{amd64,arm64}` and writes sha256 files, `manifest.json` and `install.sh` (channel-substituted).
- `scripts/sign-release.sh`: cosign keyless `sign-blob --bundle` for the manifest and each artifact.
- `scripts/upload-release.sh`: upload to the release bucket (R2) under `releases/<version>/…`, then the manifest last.

### 23.2 `install.sh`

Based on deploycrate's `install.sh`:

- Supports Linux and macOS: `uname -s` → `linux|darwin`.
- Downloads and verifies cosign (pinned checksums per OS/arch), the manifest and its signature, then the artifact, its signature and its sha256.
- Installs to `/usr/local/bin/eru` when root, else `~/.local/bin/eru` (with a PATH hint).
- When root on a supported Linux server with a TTY, asks "Initialise a cluster on this server now (`eru cluster init --local`)? [y/N]". Deferred to M3; see [`docs/DEVIATIONS.md`](./DEVIATIONS.md).
- `ERU_INSTALL_ONLY=1` skips the next-step text (and will skip the M3 question).

### 23.3 Pinned versions (`internal/versions`)

```go
const (
    PostgresMajor = 18
    CaddyVersion = "2.11.4"               // floor; update
    ResticVersion = "0.18.1"              // floor; update
    NodeExporterVersion = "1.11.1"        // floor; update
    PostgresExporterVersion = "…"         // latest stable at implementation
    PgbouncerExporterVersion = "…"
    PgbackrestExporterVersion = "…"
    CosignVersion = "3.1.3"               // install.sh; floor in this spec was 3.0.6, see DEVIATIONS.md
)
var Checksums = map[string]map[string]string{ /* artifact -> arch -> sha256 */ }
```

PGDG packages (postgresql-18, pgbouncer, pgbackrest, extensions) aren't version-pinned. They're held (`apt-mark hold`) and updated only via `eru pg update`.

---

## 24. Milestones

Each milestone is done when its listed scenarios pass on Debian 13 amd64, plus `just lint`.

| # | Milestone | Deliverables | Scenarios |
| --- | --- | --- | --- |
| M0 | Skeleton & contracts | Module, cobra root, `output` port, error/exit codes, CommandMeta + `--check-meta`, `version`, `commands`, `guide agents` (stub), `skill install`, justfile, lint, `e2e` harness bootstrapping a VM + MinIO + Pebble | Harness smoke (VM up, `eru version` in VM) |
| M1 | Release & install | **Done (code, 2026-10-09).** build/sign/upload scripts, `install.sh`, sigstore-go verification, `eru upgrade` (local only), edge workflow, E25. Maintainer first publish and M1-A/B/C remain (below). | E25; manual M1-A/B/C after the first edge publish |
| M2 | Engine & node runtime | Step engine, journal, locks, redaction, run logs, `node rpc` dispatcher, `node version/selfcheck`, binary push, version handshake | E18 (stubbed steps) |
| M3 | Transport, bootstrap, secrets | First contact (TOFU), userspace WireGuard, steps 1–6, commit-confirm, secrets document, contexts, `login`, `context *`, local preflight + sudo escalation, `--local`, `--resume` | E01 (host part only), E03, E04, E05, E24 |
| M4 | PostgreSQL core | Steps 7–11, tuning calculator, `tune *`, `config *`, `plan`/`apply`, `db *`, `sql`, `psql`, `tunnel`, `ssh`, `logs` | E01 (DB part), E02, E06, E07, E19, E21 |
| M5 | TLS & pooling | Steps 12–13, certs-sync | E01 (verify-full via PgBouncer), E22 (TLS part) |
| M6 | DB backups & restore | Step 14, `backup *`, `restore *`, drill, `--db` restore, `dump`, textfile metrics | E08–E12 |
| M7 | VPS backups & DR | Step 15, `vps *`, config snapshots, `cluster init --restore` | E13, E14 |
| M8 | Observability | Step 16, Caddy metrics routes, `status`, `top`, `queries`, `doctor` (all checks) | E22, doctor assertions in E01 |
| M9 | Maintenance & extensions | `maint *`, catalog, `ext *`, timers (step 17), finalize (step 18) | E15, E16 |
| M10 | Upgrades | `node upgrade`, rollback, `pg update` | E17, E23 |
| M11 | Agent polish & matrix | Full `eru guide agents` text, JSON schema file, `docs/CLI.md` generated from meta, full OS/arch matrix green | E20 + all scenarios on all images |
| M12 | (stretch) Tailscale transport | `transport.kind = "tailscale"` using `tsnet` in operator mode and `tailscale up --authkey` on the node; ufw allows 22 on `tailscale0` | E01 variant with Tailscale (manual/nightly; needs an auth key secret) |

M1 remaining (maintainer, not code):

1. Create the R2 bucket and the `eru.mbvlabs.com` custom domain (or change `internal/buildinfo.ReleaseBaseURL` and [`docs/DEVIATIONS.md`](./DEVIATIONS.md) together if the name differs).
2. Create GitHub environments `edge` and `stable` with vars `RELEASE_BUCKET`, `RELEASE_BASE_URL` and secrets `R2_ENDPOINT`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`.
3. Merge to `master` for the first edge publish. Check the bucket against the M1 layout and `curl -I` cache headers.
4. Run **M1-A** (Debian 13 VPS, root + user) and **M1-B** (macOS: install, `version`, `eru node version` → exit 11 `node_mode_requires_linux`).
5. Push a second commit → **M1-C**: `eru upgrade` on both machines, then a no-op re-run.
6. Record date, versions, and machines in the PR description.

---

## 25. The agent guide (`eru guide agents` / `eru skill install`)

The embedded markdown MUST cover:

1. what eru is (one paragraph);
2. always using `--output agent` or `--json` and reading `ok`, `error.code` and `next`;
3. discovering commands with `eru commands --json` and reading `meta` (`mutating`, `destructive`, `confirm_token`, `needs_restart`);
4. never inventing confirm tokens: obtain them from `error.details.confirm_token` and ask the human before passing one for a destructive command;
5. the exit code table;
6. common workflows: status check, backup now plus verify, PITR restore (paused → inspect → promote/abort), add a database, add an extension, run a maintenance report, upgrade;
7. what an agent must never do:
   - print the secrets document or passphrase;
   - run `restore cleanup` or `db drop` without explicit human approval;
   - pass `--sudo` or `--install-missing` without approval.

`eru skill install` writes this as a skill into the requested harness directories, using andurel's `skill.go` harness list and layout.

---

## 26. Choices left to the implementer

Record each of these in `docs/DEVIATIONS.md`:

- The release domain (`ReleaseBaseURL`).
- Exact exporter versions and whether `pgbackrest_exporter` is replaced by eru textfile metrics. Allowed if the same metrics are produced.
- The bloat estimation query source (vendored with licence and attribution).
- Whether `top` refreshes via repeated `top.sample` RPCs or direct pgx over the tunnel. Direct is preferred.
- Any catalog extension dropped because no PG18 package exists.
