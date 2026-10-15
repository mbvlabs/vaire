# eru: implementation spec

> Audience: the agent (or engineer) implementing eru on its master node: one server with a PostgreSQL cluster per database.
> Phase 3 follows [`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md) (approved). Where this spec and the RFC disagree about a later phase, the RFC wins until that phase's design updates this document.
> Source of truth for *what* and *why*, and for the roadmap and status: [`MASTER.md`](./MASTER.md). This document is the *how* and carries no status.

---

## 0. How to use this document

1. Read `MASTER.md` in full first. This spec assumes its decisions (§3 there) and doesn't re-argue them.
2. Build the phases in MASTER §7 **in order**. From Phase 2 on, each phase ends with a manual test on a real VPS that must pass before you move on.
3. Where this spec says **MUST**, it's fixed. Where it says **SHOULD** or **choose** and you decide something, change this document so it describes what was built. If the decision matters beyond implementation detail, also add it to the decisions table in `MASTER.md`. There's no separate deviations log.
4. Pin every external version and checksum when you implement (§23.3). Versions in this document are the floor known at writing time. Use the latest stable release at implementation time and record it in `internal/versions/versions.go`.
5. Re-read the reference code paths named here before copying patterns:
   - `/home/mbv/work/deploycrate-ce/internal/setup/` (step runner, embedded scripts, state, WireGuard, SSH, host, backup, node-exporter scripts)
   - `/home/mbv/work/deploycrate-ce/scripts/` (`install.sh`, `build-release.sh`, `sign-release.sh`, `upload-release.sh`)
   - `/home/mbv/work/andurel/cli/` (`output/output.go`, `command_meta.go`, `command_meta_catalog.go`, `discovery.go`, `agent_prompts.go`, `skill.go`)

### 0.1 Hard rules

- **No automated tests.** Don't add `_test.go` files. Behaviour is verified by manual tests on real VPSes (§22).
- Every host-mutating action is a step (§9) or a node RPC (§7.4). Nothing mutates a host through ad-hoc SSH commands.
- Secrets never appear in argv, logs, events, JSON output (unless explicitly requested by a `credentials` command), journal files or error messages. Pass them via stdin or 0600 files and redact on every output path (§9.6).
- Every command has `CommandMeta` (§5.3). The build-time check fails otherwise.
- Every bash script starts with `set -euo pipefail`, is idempotent, passes `shellcheck`, and receives inputs only via environment variables validated with `: "${VAR:?}"`.
- Supported server OS: the latest Debian and Ubuntu LTS only, today Debian 13 and Ubuntu 26.04. amd64 and arm64. Older releases are refused by preflight.
- Supported operator OS: Linux and macOS, amd64 and arm64.

---

## 1. Scope

**In scope** (MASTER roadmap Phases 0–5, 7 and 8, plus the agent guide from Phase 9): the master node, operator CLI, WireGuard transport, secrets document, hardening, PostgreSQL 18 with a cluster per database (sizes, the memory budget, cgroup limits), tuning, security, nginx TLS (eru-issued ACME certificates), the transaction and session PgBouncers, pgBackRest backups with a stanza per database, native restores, forks and drills, the master backup and DR rebuild (Phase 5, designed in the RFC), metrics exporters, maintenance, extensions v1, status/doctor/top, upgrade/rollback, `--local` install, agent guide and skill.

**Out of scope:** followers (Phase 6, designed in the RFC), worker nodes, `db move`, replicas and the Tailscale transport (Phase 10), HA (Phase 11), operator add/revoke and scoped operators, provider snapshots and alerting (Phase 9), `eru mcp` and major PG upgrades (backlog).

---

## 2. Repository and toolchain

| Item | Value |
| --- | --- |
| Module | `github.com/mbvlabs/eru` |
| Go | 1.27.x (match andurel's `go` directive) |
| Task runner | `justfile` (`just build`, `just lint`, `just release-local …`) |
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
| Secrets document, long-retention dumps | `filippo.io/age` (scrypt recipient for the bundle; X25519 for dumps), v1.3.2 |
| S3 (bucket probe, dumps) | `github.com/minio/minio-go/v7`, v7.3.0 |
| Signature verification | `github.com/sigstore/sigstore-go` (verify cosign bundles in-process) |
| JQ filter | `github.com/itchyny/gojq` |
| Bcrypt (nginx basic auth hash) | `golang.org/x/crypto/bcrypt` |
| ACME client (certificates) | `golang.org/x/crypto/acme` |
| `~/.ssh/config` parsing | `github.com/kevinburke/ssh_config` |
| Passphrase prompts without echo | `golang.org/x/term` |

`tun/netstack` pulls in `gvisor.dev/gvisor` at the version the WireGuard module pins; don't upgrade it on its own.

Add any other runtime dependency to this table first.

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
internal/config/             cluster.toml schema ([[clusters]], sizes), defaults, validation, answers-file merge
internal/hostinfo/           OS, arch, RAM, vCPU, disks (rotational, size), kernel, public IP
internal/engine/             step interface, runner, journal, locks, plan, events
internal/steps/              step definitions (one file per step)
internal/steps/scripts/      embedded bash scripts
internal/render/             text/template renderers + templates/*.tmpl
internal/tune/               tuning calculator, memory budget and oversubscription sheet
internal/pg/                 pgx helpers: connect via socket/tunnel, roles, dbs, extensions, reports
internal/backup/             Driver interface, pgbackrest driver, materialise (temporary clusters), dumps, records.json, metrics
internal/maint/              maintenance commands + SQL
internal/catalog/            extension catalog data
internal/doctor/             checks registry
internal/metrics/            textfile-collector writer
internal/upgrade/            release manifest fetch, sigstore verify, self-update, node upgrade/rollback
internal/guide/              agent guide markdown (embedded) + skill installer
schema/eru-v1.json           agent JSON contract
scripts/                     install.sh, build-release.sh, sign-release.sh, upload-release.sh
.github/workflows/           publish-stable.yml, publish-edge.yml
docs/                        MASTER.md, IMPLEMENTATION.md (this file), CLI.md (generated)
```

---

## 3. Binary modes

One binary. Mode is determined by the command path, not by detection:

- **Operator commands**: everything except `eru node …`. They run on a laptop, CI or a server in `--local` mode.
- **Node commands**: `eru node …` subtree, except `eru node upgrade`, which is operator-audience (§4.3). They run on a server, are mostly invoked over SSH by the operator, and are also called by systemd timers. `eru node` is hidden from top-level help but listed in `eru commands --json` with `"audience": "node"`.

An operator command against a remote server always runs the same thing:

1. Resolve the context (`--context` flag › `ERU_CONTEXT` › current context).
2. Bring up transport (§7).
3. Version handshake (§7.3).
4. Make a node RPC call (§7.4).
5. Render the result.

In `--local` mode (operator running on the server itself, §8.5), step 4 calls the node dispatcher in-process with root privileges and skips 2–3.

---

## 4. Command tree

Legend: **M** = mutating, **D** = destructive (needs `--confirm <token>`), **DR** = supports `--dry-run`, **P** = may prompt in a TTY (every prompt has a flag).

### 4.1 Global flags (persistent)

`--context, -c <name>`, `--output human|json|agent|markdown`, `--json` (= `--output json`), `--jq <expr>`, `--quiet`, `--verbose`, `--stream` (NDJSON events), `--no-input` (never prompt; implied when stdin isn't a TTY), `--sudo` (pre-approve local escalation), `--install-missing` (pre-approve local package installs), `--auto-upgrade-nodes` (install this CLI's build on a mismatched node instead of failing, §7.3), `--timeout <dur>`.

### 4.2 Operator commands

| Command | Flags (beyond global) | M | D | DR | P | Confirm token |
| --- | --- | --- | --- | --- | --- | --- |
| `eru version` | | | | | | |
| `eru commands` | `--path <cmd path>` | | | | | |
| `eru guide agents` | | | | | | |
| `eru skill install` | `--harness <name>…`, `--dir` | ✓ | | ✓ | ✓ | |
| `eru login [bundle-file\|-]` | `--name`, `--passphrase-stdin`, `--passphrase-file` | ✓ | | | ✓ | |
| `eru context list\|show\|use <n>\|remove <n>` | | (use/remove ✓) | remove | | | `<name>` |
| `eru server init` | §8.1 | ✓ | | ✓ | ✓ | `<server>` when `--no-input` |
| `eru plan` | `--db` | | | | | |
| `eru apply` | `--step <id>…` (`<step>` or `<step>@<db>`), `--db`, `--confirm` (repeatable), `--skip-doctor` | ✓ | | ✓ | ✓ | `<db>/restart` per database that needs a restart; `<server>/restart` for all of them, one at a time |
| `eru config show\|get <key>\|set <key> <value>\|edit\|validate` | `set`, `edit`: `--apply` | set/edit ✓ | | ✓ | edit | |
| `eru status` | `--watch`, `--db` | | | | | |
| `eru doctor` | `--only security\|backups\|host\|postgres\|tls\|metrics`, `--db` | | | | | |
| `eru top` | `--db`, `--once` | | | | | |
| `eru queries` | `--db`, `--limit`, `--order total\|mean\|calls`, `--reset` (D) | reset | reset | | | `<db>/stats`, `<server>/stats` without `--db` |
| `eru metrics show\|rotate` | `show`: `--reveal` | rotate | rotate | ✓ | | `<server>/metrics` |
| `eru logs [unit]` | `--db`, `--since`, `--follow`, `--lines` | | | | | |
| `eru sql <query>` | `--db`, `--file`, `--write` | `--write` | | | | |
| `eru psql` | `--db`, `--write`, `-- <psql args>` | | | | | |
| `eru console [db]` | `--db`, `--write` | | | | | |
| `eru tunnel` | `--db`, `--local-port`, `--target pgbouncer\|postgres` | | | | | |
| `eru ssh` | `--command` | | | | | |
| `eru db list` / `create <name>` / `resize <name>` / `drop <name>` / `credentials <name>` | `create`: `--size`, `--memory`, `--profile`, `--statement-timeout`, `--extensions`; `resize`: `--size`, `--memory`, `--defer-restart`; `drop`: `--no-final-dump`; `credentials`: `--rotate`, `--role` | create/resize/drop/rotate | drop, rotate | ✓ | | `<db>` (drop, rotate); `<db>/restart` (resize) |
| `eru db protect <db>…\|--all` / `unprotect <db>…` | `protect`: `--all` | ✓ | | ✓ | unprotect (TOTP code) | |
| `eru protect init\|status` | `init`: `--rotate`, `--db <db>` (repeatable), `--all` | init | | | init | |
| `eru ext list` / `add <name>` / `remove <name>` | `--db` | add/remove | remove | ✓ | | `<db>/restart` if restart; `<db>` for remove |
| `eru tune show\|explain\|apply` | `show`, `explain`: `--db` | apply | | ✓ | | `<db>/restart` if restart |
| `eru backup setup` | `--provider`, `--endpoint`, `--region`, `--bucket`, `--prefix`, `--uri-style`, `--key-id`, `--secret-stdin\|--secret-file`, `--full-schedule`, `--diff-schedule`, `--incr-schedule`, `--retention-full` | ✓ | | ✓ | ✓ | |
| `eru backup now` | `--db`, `--type full\|diff\|incr` (a job per database) | ✓ | | ✓ | | |
| `eru backup list` / `verify` | `--db` | verify | | | | |
| `eru job list` / `follow <id>` | | | | | | |
| `eru restore <db>` | `--time`, `--latest`, `--set`, `--dump`, `--promote` (job) | ✓ | ✓ | ✓ | ✓ | `<db>/restore` |
| `eru restore status [db]\|promote <db>\|undo <db>\|abort <db>` | | promote/undo/abort | promote, undo | ✓ | | `<db>/restore` (promote, undo) |
| `eru restore cleanup [db]` | | ✓ | ✓ | ✓ | ✓ | `<db>/cleanup` |
| `eru restore drill` | `--db`, `--time` (a job per database) | ✓ | | ✓ | | |
| `eru db fork <db>` | `--name`, `--at` (job) | ✓ | | ✓ | | |
| `eru dump <db>` | `--to bucket\|local`, `--out` | ✓ | | | | |
| `eru dump list [db]` / `get <db> <id>` | `get`: `--out` | get (local file) | | | `get` | |
| `eru server backup now` | `--force` | ✓ | | ✓ | | |
| `eru server backup list` | `--local`, `--bucket` | | | | | |
| `eru server backup verify` | `--bundle`, `--bundle-file` | | | | | |
| `eru server backup export` | `--bundle`, `--out` | local file | | | | |
| `eru server init --restore` | `--host`, `--bundle <name\|time>`, `--bundle-file <file\|->`, `--old-master-unreachable`, `--resume` | ✓ | ✓ | ✓ | ✓ | `<server>/replace` |
| `eru db recover <db>…\|--all` | `--time`, `--no-promote`, `--no-full` (jobs) | ✓ | | ✓ | | `<db>/recover` with `--time` |
| `eru context set-address <ip[:port]>` | `--name` | ✓ | | ✓ | | |
| `eru maint vacuum\|analyze\|reindex\|repack` | §16 | ✓ | | ✓ | | repack: `<db>` |
| `eru maint report <kind>` | §16 | | | | | |
| `eru maint kill` | `--db` (required), `--idle-in-tx`, `--pid`, `--older-than` | ✓ | ✓ | ✓ | | `<db>/kill` |
| `eru secrets export` | `--out`, `--reveal-passphrase` | ✓ | | | ✓ | |
| `eru wg export` | `--out`, `--install` | `--install` | | ✓ | ✓ | |
| `eru upgrade` | `--version`, `--channel`, `--skip-nodes` | ✓ | | ✓ | ✓ | |
| `eru node upgrade` | `--version`, `--rollback` | ✓ | rollback | ✓ | | `<server>/rollback` |
| `eru pg update` | `--now` | ✓ | | ✓ | | `<server>/restart` |

Phase 5 added `server backup now|list|verify|export`, `server init --restore`, `db recover` and `context set-address`, and removed `vps backup|restore` (the master backup section below); Phase 6 adds `db follow` and `db unfollow`; Phase 10 adds `node add|list|remove`, `db move` and `server takeover`. Their design is in the RFC.

`eru node upgrade` is an **operator** command even though it lives under `eru node`. Audience is set per command in `CommandMeta`, not per subtree. Its node-side counterpart is the RPC method `upgrade.install`, so there's no name collision.

### 4.3 Node commands (`eru node …`)

| Command | Purpose |
| --- | --- |
| `eru node upgrade` | **Operator-audience**: see §4.2 and §19.2 |
| `eru node rpc` | Reads one JSON request on stdin, writes NDJSON events to stdout (§7.4). The only entry point the operator uses besides `version` |
| `eru node version` | Prints version JSON. Runs without sudo |
| `eru node selfcheck` | Binary can read config, the journal is readable, required tools are present. Creates missing `/var/lib/eru` (0700), `/var/log/eru` (0750) and `/run/lock` (1777), and fails if the owner lacks `rwx` on them, since root can write to a mode-000 directory |
| `eru node run <job>` | Timer and job entry points: `backup-full`, `backup-diff`, `backup-incr`, `backup-verify`, `restore-drill` and `db-dump` (each needs `--db <db>`), `stanza-expiry`, `restore` and `db-fork` (only with `--job-id`, from their RPC), `maint-daily`, `maint-weekly`, `certs-renew`, `metrics-refresh`. `--job-id <id>` runs the detachable job an RPC prepared (§13.4) |
| `eru node dump <db>` | Writes `pg_dump -Fc` of one database to stdout, over that database's cluster socket; `eru dump --to local` runs it over SSH. Besides `rpc` and `version`, the only node command the operator calls |
| `eru node ssh-revert` | Commit-confirm revert (§8.3), invoked by a transient systemd timer |
| `eru node unlock-ssh` | Break-glass: open public 22/tcp for `--minutes` (default 30) via a transient timer that re-closes it |
| `eru node unfence --confirm <server>/unfence [--reclaim]` | Phase 5: lift the epoch fence on this node (break-glass, a human decision), then `eru apply` |

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
    "message": "restore replaces the running database",
    "hint": "re-run with --confirm app/restore",
    "details": { "confirm_token": "app/restore" }
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
| `step_failed`, `partial_failure`, `doctor_failed` | 5 |
| `external_command_failed` | 6 |
| `config_error`, `validation_failed` | 7 |
| `ambiguous_input`, `input_required` | 8 |
| `remote_unreachable`, `transport_failed`, `host_key_mismatch`, `endpoint_unreachable`, `bucket_not_found`, `backup_auth_failed` | 9 |
| `node_version_mismatch`, `update_required` | 10 |
| `precondition_failed` (e.g. lock held, backup running, outside PITR window, insufficient disk), `bundle_not_found`, `node_fenced` (Phase 5) | 11 |
| `lockdown_failed` (commit-confirm reverted) | 12 |
| `secrets_error` (decrypt failed, bad passphrase), `bundle_corrupt` (a master bundle failed its checks, Phase 5) | 13 |
| `release_verification_failed` (signature, identity, sha256 or preflight of a downloaded binary) | 14 |
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
    ConfirmToken string   // template, e.g. "{db}/restore", "{db}", "{server}/restart"
    NeedsRestart string   // "never" | "maybe" | "always" (Postgres restart)
    ErrorCodes   []string // codes this command can return beyond the generic ones
    MutatingFlags    []string // flags that make an otherwise read-only command mutate
    DestructiveFlags []string // flags that make it destructive, e.g. queries --reset, db credentials --rotate, node upgrade --rollback
    LinuxOnly      bool     // emitted as "platforms": ["linux"]; every node-audience command sets it
    LinuxOnlyFlags []string // flags that are Linux-only, e.g. server init --local, wg export --install
}
```

- A Linux-only command or flag on another OS returns `precondition_failed` with `details.reason = "node_mode_requires_linux"`.

- `eru commands --json` emits the catalog (as in andurel's `commandsDiscoveryPayload`).
- `just lint` runs `eru commands --check-meta` and fails on missing metadata or on mutating commands without `SupportsDryRun` (opt out with `SkipDryRunCheck` + reason). It also requires every listed flag to exist, and a `ConfirmToken` when `DestructiveFlags` is set.

### 5.5 Prompts and confirmation

Port `requireFlagInStructuredMode` and `confirmDestructiveAction` from `andurel/cli/agent_prompts.go`.

- If stdin isn't a TTY, `--no-input` is set, or the output mode is structured, a missing value returns `input_required`, naming the flag.
- Destructive commands require `--confirm <exact token>`. In a TTY without `--confirm`, prompt the user to type the token. `--yes` exists but **never** satisfies a destructive confirmation.
- A word the operator must type is always quoted in the prompt: `Type 'prod' to confirm`, `Type 'confirmed' once the records exist`, `Type 'stored' once both are stored safely`.

### 5.6 Terminal UI

`internal/cli/ui` draws human output on a terminal. It is used when the output mode is human, stderr is a terminal, `TERM` isn't `dumb`, and none of `--no-input`, `--quiet`, `--jq`, `--stream` or a structured `--output` is set. Everything else keeps the plain lines (and `NO_COLOR` strips colors from the terminal UI).

- **Live area:** every running local phase and node step shows a `bubbles` spinner with its title, elapsed time and, for waits, the time left. The latest script output line is shown dim under it, so long installs visibly move. Node steps show a progress bar (`step 3 of 7`), and the binary upload shows bytes sent. A finished task prints one `✓`/`✗`/`!`/`–` line; a finished step always shows its duration (`<1s` under a second); a skipped step, or one eru never saw start, shows none. `--verbose` still prints every log line.
- **Layout:** `lipgloss` section headers (Prepare → Host → Lockdown → DNS → Database → TLS → Secrets), boxed panels for the DNS records (plain `TYPE NAME VALUE` lines inside so they copy cleanly), the DNS answers and the secrets passphrase, warnings in their own style, and a summary card with connection URLs, warnings and next steps at the end of init.
- **Errors:** a boxed error with the code, the failed step, `details.output_tail` and the hint. The step line is left out when the message already names the step. A step that fails in Go code rather than a script has no output tail, so its message says which action failed (e.g. "validating the rendered configuration"), and `pg.Local` errors carry psql's own message without an extra `psql:` prefix.
- **Prompts:** every prompt (the interview, typed words, yes/no, host-key trust, passphrases) is a `huh` field with eru's theme. The theme picks its accent and the focused button colours per terminal background so the selected button stays readable on dark and light terminals; headers keep the terminal's default foreground with the accent only as a marker, and dim text is used only for secondary hints. The live area is suspended while a prompt is open. The renderer never reads stdin, so Ctrl+C keeps its default behaviour.

---

## 6. Configuration and files

### 6.1 `cluster.toml` (desired state; lives on the master at `/etc/eru/cluster.toml`)

```toml
schema = 1
name = "prod"                       # [a-z][a-z0-9-]{1,30}; the server (setup) and context name
domain = "example.com"              # the setup's base; each node is <node>.<domain>, the master db1.<domain>
acme_email = "ops@example.com"
timezone = "UTC"

[tls]
mode = "acme"                       # acme | self-signed
acme_ca = ""                        # override directory URL (e.g. Let's Encrypt staging)

[transport]
kind = "wireguard"                  # wireguard; tailscale (Phase 10)
subnet = "10.42.0.0/16"
listen_port = 51820

[[nodes]]
name = "db1"                        # server init --node-name; the hostname is <name>.<domain>
role = "master"                     # exactly one master until workers arrive in Phase 10
public_address = "203.0.113.10"     # IPv4 or IPv6 literal
wg_address = "10.42.0.1"

[server]
admin_user = "eru"                  # SSH + sudo user created on the node
swap_mb = 1024                      # 0 = don't create
data_root = "/var/lib/postgresql/18" # each database's cluster is <data_root>/<db>
memory_reserve = ""                 # empty = max(512MiB, 25% of RAM); the clusters share the rest

[postgres]                          # setup-wide defaults for every cluster
version = 18
profile = "oltp"                    # oltp | mixed | analytics
client_connections = 1000           # transaction pooler max_client_conn
session_client_connections = 100    # session pooler max_client_conn
locale_provider = "builtin"         # builtin | icu | libc
locale = "C.UTF-8"
default_size = "medium"             # db create without --size or --memory
[postgres.settings]                 # explicit overrides for every cluster (50-eru-settings.conf)
# "log_min_duration_statement" = "250ms"

[network]
client_allowlist = []               # CIDRs allowed to reach 6432 (and 5432 if expose_postgres)
expose_pgbouncer = true             # the transaction pooler (and 443's direct TLS) to the allowlist
expose_postgres = false             # the session pooler to the allowlist

[[clusters]]                        # one database in its own PostgreSQL cluster; written by db create
name = "app"                        # database, cluster, pooler entry and URL path: [a-z_][a-z0-9_]{0,62}
node = "db1"                        # placement; Phase 3 accepts only the master
port = 5433                         # 5433-5479, names the socket; assigned at create, immutable
stanza = "app-3f9a1c"               # <name>-<6 hex>; assigned at create, immutable (a dump restore and its undo switch it)
size = "small"                      # nano | small | medium | large | xlarge | custom
memory = ""                         # empty = from size; KiB/MiB/GiB/TiB or kB/MB/GB/TB
memory_max = ""                     # opt-in hard cap (MemoryMax)
cpu_weight = 0                      # 0 = from size
cpu_quota = ""                      # opt-in, e.g. "150%"
io_weight = 100
profile = "oltp"                    # optional; empty = [postgres] profile
statement_timeout = "60s"
extensions = ["pg_stat_statements"]
pool_size = 0                       # transaction pooler; 0 = max_connections - session_pool_size - 3
session_pool_size = 10              # session pooler
[clusters.settings]                 # this cluster's overrides, after [postgres.settings]
work_mem = "32MB"
[clusters.backup]                   # optional; each key defaults to [backup.database]; left out while empty
retention_full = 1                  # this database's restore window in weeks
full_schedule = ""
diff_schedule = ""
incr_schedule = ""
verify_schedule = ""
drill_schedule = ""
dump_schedule = "*-*-* 03:00:00"    # long-retention dumps (§13.8); empty = none
dump_retention = 90

[backup]
provider = "r2"                     # r2 | s3 | b2 | hetzner | minio | custom; empty = no bucket
endpoint = "https://<account>.r2.cloudflarestorage.com"
region = "auto"
bucket = "eru-backups"
prefix = "prod"                     # no leading/trailing slash
uri_style = "path"                  # path | host (preset-derived)

[backup.database]                   # defaults for every database's schedules and window
full_schedule = "Sun *-*-* 01:00:00"      # systemd OnCalendar syntax
diff_schedule = "Mon..Sat *-*-* 01:00:00"
incr_schedule = ""                        # e.g. "hourly"; empty = off
retention_full = 4                        # restore window in weeks (time-based retention)
local_repo = false                        # repo2 on /var/lib/pgbackrest
verify_schedule = "Wed *-*-* 04:00:00"
drill_schedule = "*-*-01 05:00:00"        # the day is staggered per database

[master_backup]                           # Phase 5; the bucket and prefix come from [backup]
retention_days = 30
retention_weeks = 52
local_keep = 10                           # bundles kept on the master itself

[maintenance]
window = "02:00-05:00"                    # local time per [timezone]; also the backup stagger spread
auto_repack = false
repack_bloat_threshold = 0.40

[metrics]
enabled = true
path_prefix = "/metrics"
```

**Sizes** (`internal/config/sizes.go`, `ResolveResources`):

| Size | `memory` | `cpu_weight` | `max_connections` |
| --- | --- | --- | --- |
| `nano` | 256 MiB | 25 | 20 |
| `small` | 512 MiB | 50 | 30 |
| `medium` | 1 GiB | 100 | 50 |
| `large` | 2 GiB | 200 | 100 |
| `xlarge` | 4 GiB | 400 | 200 |

Explicit values win: `size = "small"` with `memory = "768MiB"` is a small with more memory. `custom` needs `memory`; its `max_connections` comes from the largest named size whose memory fits (nano below 256 MiB), and its CPU weight defaults to 100. `settings.max_connections` overrides either. An empty size means `[postgres] default_size`.

**Validation** (`eru config validate`, run before every write and apply):

- Names and identifiers follow the regexes shown.
- `domain` is an FQDN. `acme_email` is optional; when set it must be an email address. It is only the ACME account contact, used in `acme` mode.
- Schedules parse with `systemd-analyze calendar`. Run it on the node. On the operator, use a Go parser for the subset and let the node have the final say.
- CIDRs parse. `0.0.0.0/0` in `client_allowlist` is allowed but produces a doctor `warn`.
- `retention_full ≥ 1`, `keep_weekly ≥ 1`; `[master_backup]` needs `retention_days` and `local_keep` ≥ 1.
- Exactly one node with `role = "master"`; any other role is rejected with "workers arrive in Phase 10".
- `[[clusters]]`: names unique and valid database names (`postgres`, `template0` and `template1` are refused; `<name>_migrate` etc. fit in 63 characters); `node` is the master; ports unique in 5433–5479; stanzas unique and `<name>-<6 hex>`; `profile ∈ {oltp, mixed, analytics}`; `statement_timeout` is `0` or matches `^\d+(ms|s|min|h)$`; extensions are catalog entries (§17; until Phase 7 only the contrib entries exist); weights 1–10000 (0 = default); `cpu_quota` a percentage; `memory_max` not below `memory`; `pool_size + session_pool_size + 3 ≤ max_connections`; `[clusters.backup].dump_schedule` needs `[backup]` and `dump_retention ≥ 1`.

- **Strict decode.** `ParseCluster` refuses unknown keys with `config_error` naming them. There's no compatibility for the shared-cluster layout (`[[databases]]`, `data_directory`): a server from before Phase 3 is rebuilt. `schema` stays `1`.
- A table's fields are validated only when its feature is in use: `[backup]` only when `provider != ""` (empty means not configured). Then `provider` is a preset, `endpoint` an `https://` URL with only a host (pgBackRest talks to S3 over TLS only), `bucket` an S3 bucket name, `prefix` non-empty without leading or trailing slash, `region` set, `uri_style ∈ {path, host}`, `full_schedule` set, and every schedule (the database's own too) passes a coarse character filter; `config.set` (and so `config edit` and `backup setup`) then runs `systemd-analyze calendar` on each on the node, which has the final say.
- Name collisions with temporary clusters are existence checks, not name rules: `db create` and `db fork` refuse a name whose `18/<name>` cluster exists, and a restore refuses when `18/<db>__restoring` is in the way (§13.5).
- `tls.mode ∈ {acme, self-signed}`; `acme_ca` empty or an `https://` URL; `postgres.version == 18`; `profile ∈ {oltp, mixed, analytics}`; `client_connections` and `session_client_connections` 10–100000; `default_size` a named size; `memory_reserve` empty or a memory value; `locale_provider ∈ {builtin, icu, libc}`; `locale` matches `^[A-Za-z0-9_.@-]+$`.
- `[postgres.settings]` and `[clusters.settings]` keys match `^[a-z_][a-z0-9_.]*$` and may not name a setting eru owns: `allow_alter_system`, `ssl*`, `listen_addresses`, `port`, `password_encryption`, `shared_preload_libraries`, `archive_*`, `data_directory`, `hba_file`, `ident_file`, `include*`.
- `ValidateFor(scope)` has scopes `host` and `full`. `cluster.bootstrap phase=host` uses `host`, which allows a missing `domain`. Everything else uses `full`, where a missing domain is `validation_failed` with hint `eru config set domain <fqdn>`.
- `DefaultCluster()` has no clusters, and init never creates one: it has no `--database` flag, the interview does not ask, and an answers file with `[[clusters]]` is rejected. Databases are added after init with `eru db create`, which places each one (the master, the lowest free port, a new stanza, the default size).

`config set` uses dotted keys (`backup.database.full_schedule`, `postgres.settings.work_mem`, `clusters.app.size`, `clusters.app.settings.work_mem`, `clusters.app.backup.retention_full`). Array-of-table entries are addressed by `name`. Values parse by the target field's type (bool, int, float, string, `[]string` as a comma list or TOML array literal). `schema`, `name`, `nodes.*.wg_address`, `transport.*` and `clusters.*.name|port|stanza|follows` are immutable: `config set` refuses them, and so does a whole-file `config.set` (`config edit`), with `precondition_failed`, reason `immutable_key`. Changing a cgroup key (`memory`, `memory_max`, `cpu_weight`, `cpu_quota`, `io_weight`, or `size`) is a `daemon-reload` and `set-property` on the next `apply`, with no restart; `eru db resize` is the checked path (§12.4). Every `config set` carries the oversubscription warnings.

**Protected databases.** `clusters.<db>.protected` is checked live by the node, so changing it needs no apply. `eru db protect <db>…|--all` and `eru db unprotect <db>…` write it through `config.set` with one `set` key per database, exactly as `config set clusters.<db>.protected true|false` does (which stays supported). Protecting is refused until `eru protect init` has confirmed a TOTP secret (`precondition_failed`, reason `protect_not_initialized`). Unprotecting asks once for a TOTP code for every database named (`unprotectCode`), and is refused with `--yes`, without a terminal and in structured output. `eru protect init` checks `--db` names before showing the QR code; once the code is confirmed it protects the `--db`/`--all` databases, or asks which of the unprotected ones to protect (a multi-select with all picked), in the same run. Its next steps name the databases (`eru db protect app billing`), never a placeholder. `eru protect status` lists every database with protected yes or no (`data.databases`), and `eru db list` has a PROTECTED column.

**Desired-state store** (`internal/state`): a `Store` interface (`Load`, `Save(raw)` that validates with `full` and writes atomically, `Revision()` = sha256 of the file). `FileStore` at `/etc/eru/cluster.toml`; Phase 5's bundle copies it off the master. `config.set`, `db.create`, `db.resize`, `db.fork`, `db.drop`, restore promote and undo (a dump restore's stanza) and `cluster.bootstrap` write through it. `config.set` takes `base_revision` and refuses a stale one (`precondition_failed`, reason `config_changed`); with `dry_run` it returns the plan for the new file without saving.

**Answers file** (`--answers answers.toml`): the same schema as `cluster.toml`, plus a `[bootstrap]` table (`host`, `user`, `identity`, `host_key_fingerprint`) and a `[backup.credentials]` table (`access_key_id`, `secret_access_key`). The CLI strips credentials before persisting `cluster.toml` (they go to runtime secrets, §6.3).

### 6.2 Node files

| Path | Owner:mode | Content |
| --- | --- | --- |
| `/usr/local/bin/eru`, `/usr/local/bin/eru.prev` | root:root 0755 | Binary and rollback copy |
| `/etc/eru/node.toml` | root 0640 | `server`, `node_name`, `hostname` (`<node>.<domain>`, rewritten whenever `cluster.toml` is saved), `role = "master"`, `installed_at`. Marks a host as an eru node |
| `/etc/eru/cluster.toml` | root 0640 | Desired state |
| `/etc/eru/operators.toml` | root 0640 | Operator registry: `name`, `wg_address`, `wg_public_key`, `ssh_public_key` per operator. Steps 2 and 3 render `authorized_keys` and the wg0 peers from it |
| `/etc/eru/runtime/` | root 0700 | `pgbouncer_auth.pass` and `monitor.pass` (0600, written only from operator-supplied secrets), `backup.env` (0600: S3 keys and the dump age recipient, §13.3), `pgbouncer_exporter{,-session}.env` (0600). The `/metrics` bcrypt hash is `/etc/nginx/eru-metrics.htpasswd` (`root:www-data 0640`), since nginx's workers read it |
| `/var/lib/eru/doctor.json`, `/var/lib/eru/maint.json` | root 0600 | The last doctor results and maintenance numbers (Phase 4); regenerable, not in the master backup |
| `/var/lib/eru/master-backup/` | root 0700 | Phase 5: local bundles `<name>.tar.age` and sidecars `<name>.json` (the newest `local_keep`), the dirty flag `dirty`, `last.json` (last bundle, upload, verify, prune, export), `epoch.json` (the node's copy of the bucket's) |
| `/var/lib/eru/instance` | root 0600 | Phase 5: this install's random id; the bucket's epoch names it. Never bundled |
| `/etc/eru/fenced` | root 0644 | Phase 5: written when the bucket's epoch names another instance; the archive wrapper (as postgres) stats it |
| `/etc/eru/runtime/recovery.recipient` | root 0600 | Phase 5: the recovery kit's age recipient, with or without a bucket; the master bundles are encrypted to it |
| `/usr/local/lib/eru/exporters/` | root 0755 | Pinned exporter binaries, one directory per version (Phase 4) |
| `/etc/pgbackrest/pgbackrest.conf` | postgres 0600 | §13.1; one repository, a section per stanza; holds the S3 keys and the repository cipher |
| `/etc/postgresql/18/<db>/` | postgres | The database's cluster configuration (`postgresql.conf` from `pg_createcluster`, `conf.d/`, `pg_hba.conf`, `pg_ident.conf`, `start.conf`) |
| `<data_root>/<db>/` | postgres 0700 | The database's data directory; also `<db>__restoring`, `<db>__pre_restore_<ts>` and `<db>__drill` for temporary clusters and restore leftovers |
| `/etc/systemd/system/postgresql@18-<db>.service.d/10-eru.conf` | root 0644 | The cluster's cgroup limits and `OOMScoreAdjust` (§11) |
| `/etc/pgbouncer/pgbouncer.ini`, `pgbouncer_hba.conf` | postgres 0640 | The transaction pooler (Debian's `pgbouncer.service`) |
| `/etc/pgbouncer/session.ini`, `session_hba.conf` | postgres 0640 | The session pooler (`eru-pgbouncer-session.service`, runtime dir `/run/eru-pgbouncer-session`) |
| `/etc/pgbouncer/userlist.txt` | postgres 0640 | Shared by both poolers: `pgbouncer_auth` and `eru_monitor` verifiers |
| `/var/lib/eru/jobs/` | root 0700 | Detachable job specs (0600), deleted when the job starts (§13.4) |
| `/etc/eru/tls/` | root 0755 | `server.crt` (0644), `server.key` (root:ssl-cert 0640; `postgres` and `pgbouncer` are in `ssl-cert`) for the node's hostname |
| `/var/lib/eru/journal.json` | root 0600 | §9.3 |
| `/var/lib/eru/state.json` | root 0600 | Ephemeral only: `secrets_confirmed_at`, `lockdown_pending`, `locked_down_at`, `jobs[]` (last 50). Updated under an flock on `state.json.lock` |
| `/var/lib/eru/records.json` | root 0600 | Durable records owned by `internal/backup` (§13.9): restores per database, leftovers, timeline switches, dropped stanzas, drills, last backup, verify and dump per database. Updated under an flock on `records.json.lock`; a corrupt file is `records_corrupt`, never reset |
| `/var/lib/eru/ssh-snapshot/` | root 0700 | Commit-confirm snapshot (`snapshot.json`, `sshd-dropin.conf`, `ufw-rules.txt`) and `reverted.json` (§8.3) |
| `/run/eru-ports/<owner>` | root | Port reservations of temporary clusters in 5480–5499, under `ports.lock` (§13.5) |
| `/var/lib/node_exporter/textfile/eru.prom` | root 0644 | §15.4 |
| `/var/log/eru/` | root 0750 | One log per RPC/job: `<UTC ts>-<method>-<request_id>.log`, rotated by logrotate (30 days) |
| `/var/log/postgresql/postgresql-18-<db>.log` | postgres | The cluster's log (`eru logs postgres --db <db>`) |
| `/run/lock/eru.lock`, `/run/lock/eru-<db>.lock`, `/run/lock/eru-heavy-io.lock` | | The global lock, each database's cluster lock and the heavy-I/O lock (§9.5) |

### 6.3 Operator files (`$XDG_CONFIG_HOME/eru`, default `~/.config/eru`, dir 0700)

```
config.toml                         current_context = "prod"
contexts/<name>/context.toml        server, domain, tls_mode, nodes [{name, role, hostname, public_address, wg_address,
                                    wg_public_key, listen_port}] (a cache of the master's), databases {<db> = <node>}
                                    (the placement cache), operator {name, wg_address, ssh_user}, auto_upgrade_nodes,
                                    lockdown_confirmed_at, created_at, eru_version
contexts/<name>/wg.key              operator WireGuard private key (0600)
contexts/<name>/id_ed25519          operator SSH private key (0600)
contexts/<name>/known_hosts         pinned server host keys (wg address entries)
contexts/<name>/credentials.json    sudo password, setup-wide database credentials (eru_admin, services
                                    pgbouncer_auth / eru_monitor), the recovery kit (the age identity from
                                    server init; with a bucket its keys and the pgBackRest cipher pass) and later
                                    metrics credentials
                                    (0600); never application database passwords
pending/<name>/                     in-progress init: answers.toml, secrets.json (0600), progress.json, known_hosts
cache/releases/<version>/           downloaded artifacts + manifest + sigstore bundle
```

`domain`, `tls_mode` and each node's `hostname` are what `eru sql`, `psql`, `console`, `tunnel` and `secrets export` verify a node's certificate against. They come from the secrets document at init and `eru login`. After `config set`, `config edit` and `apply`, the operator refreshes the context from the node's `cluster.toml` (`syncContext`): the node cache and the database placement map, plus `domain` and `tls_mode`, warning `context_tls_updated` when a hostname or the TLS mode changed, so a domain change (or one made by another operator) doesn't leave connections failing with a hostname mismatch. A database missing from the placement map triggers one refresh from the master before a connection; one still unknown (a restore copy, a typo) goes to the master, whose pooler refuses it if it doesn't exist. A failure to update the context is the warning `context_sync_failed`; the command itself still succeeds.

OS keychain storage is a later improvement. Files are 0600 like `~/.ssh`. Any file in `contexts/` with a looser mode triggers a warning and a `chmod` fix.

---

## 7. Transport and remote execution

### 7.1 Addressing

- Subnet `10.42.0.0/16`. Nodes use `10.42.0.0/24`, and the master is `.1`. Operators use `10.42.255.0/24`. The first operator from `init` is `10.42.255.1`.
- Server: `wg0`, `ListenPort = 51820`. Each operator is a `[Peer]` with `AllowedIPs = <operator>/32` and no endpoint, because the operator initiates.

### 7.2 Operator-side userspace WireGuard

- Use `tun/netstack.CreateNetTUN([]netip.Addr{operatorAddr}, nil, 1380)` (1380 so WireGuard over IPv6 fits a 1492-byte PPPoE path) and `device.NewDevice(tun, conn.NewDefaultBind(), logger)`, configured via IPC (`private_key`, `public_key`, `endpoint`, `allowed_ip=10.42.0.0/24`, `persistent_keepalive_interval=25`).
- Expose a `transport.Dialer` interface with `DialContext(ctx, network, addr)`. The WireGuard implementation uses `netstack.Net.DialContext`. The local (`--local`) implementation uses `net.Dialer`.
- Bring up lazily per command. Tear down on exit. No persistent daemon.
- **Which node.** Every operator connection goes to the poolers of the node that holds the database: `contexts.DatabaseNode(db)` reads the placement cache for its WireGuard address and hostname (§6.3). Port 5432 is the session pooler, 6432 the transaction pooler; both route by the database name. Without `--db`, `tunnel` targets the master.
- `eru tunnel` / `eru psql`: listen on `127.0.0.1:<port>` (default 6543 for pgbouncer and 6544 for postgres, or the first free port) and proxy accepted conns through the netstack dialer to `<node wg address>:6432|5432`. `tunnel --db <db>` picks the node and puts the database in the printed DSN's path. `eru psql` execs the system `psql` with `PGHOST=127.0.0.1 PGPORT=<port> PGSSLMODE=verify-full PGSSLROOTCERT=system PGSSLSNI=1`. Set `PGHOSTADDR` to 127.0.0.1 and `PGHOST` to the node's hostname so verify-full matches the cert. It connects as `eru_admin` (`PGPASSWORD` from `credentials.json` through the environment, never argv) with `PGOPTIONS=--default_transaction_read_only=on`, or `--role=<db>_owner` with `--write` so new objects belong to the database owner and get its default privileges. The session pooler passes these through: it tracks `IntervalStyle, search_path, default_transaction_read_only, role` (`track_extra_parameters`) and refuses anything else in `options`. If PgBouncer turns out not to apply the `--name=value` form, `psql` and `console` fail to connect, which is safe; the fallback is `-c name=value` or a `PSQLRC` with the `SET`s. `eru sql` does the same per call (`SET default_transaction_read_only = on` or `SET ROLE <db>_owner`). eru keeps no application database passwords, so there is no `--role`.
- `eru console [db]` (`--write`, same session options as `eru psql`, in the `--name=value` form because usql turns the URL into a `key=value` conninfo without quoting, which would cut `-c name=value` at the space): opens usql instead of psql, so the operator needs no PostgreSQL client. usql can't verify a certificate against a loopback address, so eru terminates TLS itself: `pg.ServeTLSProxy` listens on `127.0.0.1:<port>` (from 6544), answers the client's `SSLRequest`/`GSSENCRequest` with `N`, then sends its own `SSLRequest` upstream over the tunnel and does the handshake with `pg.TLSConfig` (verify-full against system roots with `ServerName` = the node's hostname; `require` in `self-signed` mode, with a warning) before forwarding the startup message. usql connects with `postgres://eru_admin@127.0.0.1:<port>/<db>?sslmode=disable&application_name=eru-console&options=…`, with the password in `PGPASSWORD`, never argv. eru ignores SIGINT while usql runs, so Ctrl-C cancels the query, not the console. Refused (`usage`) when eru must not prompt; agents use `eru sql`.
- usql comes from PATH, else from `$XDG_CACHE_HOME/eru/tools/usql/<version>/usql`. When neither exists eru offers to download the pinned release (`localenv.UsqlVersion`, v0.21.4) from `github.com/xo/usql/releases`, or does so with `--install-missing`. The archive is checked against a SHA-256 pinned per platform (linux and darwin, amd64 and arm64) before the binary is extracted. Without approval it fails `missing_tool` with `details.download_url`.

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
   "secrets":{},"dry_run":false,"confirm":"","confirms":[],"operator":"op-1","client_version":"1.0.0"}
  ```

- The request also carries `client_commit`, `client_channel` and `lock_timeout_s` (default 30; timers pass 0).
- **Version check on the node:** `client_version` and `client_channel` must equal the node's. For dev builds (`channel = "dev"`) `client_commit` must match too, because every dev build reports `version = "dev"`. The operator-side handshake uses the same rule.
- **Decoding:** the request must be exactly one JSON object within 1 MiB, then EOF, with `schema = "eru-rpc/v1"`, a well-formed `method` and a ULID `request_id` (it names the run log). Unknown top-level fields are allowed so a newer operator can add optional ones. Method params decode strictly, except `upgrade.install`, which crosses versions and decodes leniently.
- **Frozen:** a newer operator calls `upgrade.install` on an older node, so the request fields and `upgrade.install`'s params (`path`, `sha256`, `version`, `commit`, `channel`) and result (`previous`, `installed`, `rolled_back`) only ever gain optional fields.
- **Confirm tokens for `apply`:** `confirms` lists the tokens (`ConfirmTokens()` merges it with the older single `confirm`). A token that is neither `<db>/restart` for a configured database nor `<server>/restart` is refused (exit 4) before any step runs, because whether a restart is needed is only known afterwards. No token never fails; it leaves the restart pending, and `PlanData`/`ApplyData` return `confirm_tokens` for the restarts still pending. A database named like the server makes `<name>/restart` the server token.
- **stdout**: NDJSON events (§5.2). The final line is `result`. Exit status equals the envelope's exit code.
- **stderr**: free-form diagnostics. The operator surfaces it only with `--verbose`.
- **Methods** mirror operator commands, named `<group>.<verb>` (each word may contain inner hyphens, as in `cluster.confirm-secrets`): `cluster.bootstrap`, `cluster.confirm-secrets` (internal names that predate `server init`), `apply`, `plan`, `config.get|set|validate`, `status`, `doctor`, `db.*`, `ext.*`, `tune.*`, `backup.*`, `restore.*`, `db.recover`, `master-backup.*`, `maint.*`, `ssh.lockdown`, `ssh.confirm`, `ssh.status`, `tls.status`, `logs`, `upgrade.install`, `upgrade.rollback`, `pg.update`, `sql`, `queries`, `top.sample`.
- **Confirmation is re-checked node-side.** The node recomputes the token and rejects mismatches, so a buggy operator can't skip it. Destructive methods (`db.drop`, `db.credentials` with `rotate`) also refuse an *empty* token with `unsafe_action_requires_confirmation`; for `apply`, `tune.apply` and `db.resize` an empty token only leaves the restart pending.
- **Phase 2 methods** (as they are after Phase 3): `cluster.bootstrap phase=database`; `config.get` (raw TOML + revision), `config.set`, `config.validate`; `db.list` (every configured cluster plus orphans from `pg_lsclusters`); `db.create` (needs `db.<name>.*` and the service secrets; places the cluster, checks `would_not_start`, saves `cluster.toml`, then applies `kernel-tuning-v1`, the database's step instances, both poolers, and with backups `pgbackrest-v1` and `timers-v1`; §14.2), `db.drop`, `db.resize`, `db.credentials` (rotate only; eru keeps no database passwords to read); `tune.show` (with `db`), `tune.apply`; `tls.status` (served certificate state, polled by init); `logs` (unit allowlist `postgres` with `db`, `pgbouncer`, `pgbouncer-session`, `nginx`, `eru-certs`, `eru`; `follow` streams until the client disconnects); `sql` (for `--local` only: one transaction as `postgres` with `SET LOCAL ROLE`, `READ ONLY` unless `write`).
- **Phase 3 methods:** `backup.now` and `backup.verify` (one database per call in `params.database`; the CLI loops over the databases one at a time; a dry run without one plans every database), `backup.list`; `restore.start` (confirm `<db>/restore`), `restore.status`, `restore.promote` and `restore.undo` (confirm `<db>/restore`), `restore.abort`, `restore.cleanup` (confirm `<db>/cleanup` when it drops), `restore.drill` (one database per call, like `backup.now`); `db.fork` (needs `db.<new>.*`); `dump.create`; `job.list`, `job.follow`. `backup.now`, `backup.verify`, `restore.start`, `restore.drill`, `db.fork` and `dump.create` run their work as detachable jobs and take no dispatcher lock (§13.4). `apply` takes `rotate_backup_secrets` and `db`; `db.drop` takes `no_final_dump`; `db.resize` takes `size`, `memory` and `defer_restart`; `config.set` checks schedules with `systemd-analyze calendar` and refuses moving an existing repository (`repo_in_use`).
- **Read-only SQL** (`eru sql`, `top.sample`, `queries`, reports) runs over pgx through the tunnel, not via RPC, except where node-local access is needed (`postgres` peer for superuser-only views). RPC methods for these exist for `--local` mode.
- **Sudoers**: `/etc/sudoers.d/eru` contains `eru ALL=(root) NOPASSWD: /usr/local/bin/eru node *`. Validate it with `visudo -cf`.

### 7.5 Binary push

1. Operator obtains the node artifact for the node arch: from `cache/releases/<ver>/linux-<arch>/eru`, downloading and verifying it per §19.1 if missing, or from `ERU_NODE_BINARY` for dev.
2. Stream it via `ssh exec: cat > /tmp/eru-<ver>-<rand>` as the SSH user.
3. Run `sudo -n /usr/local/bin/eru node rpc` with `upgrade.install {path, sha256, version, commit, channel}`. The `/tmp` file is owned by the SSH user, who could change it after a check, so the node first copies it into a root-only `/usr/local/bin/eru.new` (0700) while hashing it, verifies and preflights that copy, then makes it 0755, keeps the old binary as `eru.prev` and renames the new one into place. It runs `selfcheck` and rolls back if that fails (or removes the new binary if there was no old one). The `/tmp` file is deleted on every outcome.
4. **Bootstrap special case**: no node binary exists yet. As root (or `sudo -n`) the operator runs `install` directly after a `sha256sum` check executed remotely.

---

## 8. Server init and bootstrap

### 8.1 `eru server init` flags

`--host <user@ip[:port]>`, `--identity <path>`, `--password-stdin`, `--host-key-fingerprint`, `--accept-new-host-key`, `--answers <file>`, `--name` (the server), `--node-name` (default `db1`), `--domain` (the base domain), `--admin-user`, `--timezone`, `--data-root`, `--acme-email`, `--profile`, `--default-size`, `--client-connections`, `--allow <cidr>` (repeatable), `--backup-provider`, `--backup-endpoint`, `--backup-region`, `--backup-bucket`, `--backup-prefix`, `--backup-key-id`, `--backup-secret-stdin` / `--backup-secret-file`, `--db-full-schedule`, `--db-diff-schedule`, `--tls-mode`, `--local`, `--restore`, `--bundle <name|time>`, `--bundle-file <file|->`, `--old-master-unreachable` (Phase 5, §13.11), `--secrets-out <path>`, `--reveal-passphrase`, `--confirm-secrets-stored`, `--dns-ready`, `--skip-dns-check` (deprecated, no effect), `--resume`, `--discard`. The database backup flags (`--backup-*`, `--db-full-schedule`, `--db-diff-schedule`) work from Phase 3 on (§13.3).

- `--host` also takes a `Host` alias from `~/.ssh/config`. First contact reads the matching block for `HostName`, `User`, `Port` and `IdentityFile`; explicit `user@`/`:port` and `--identity` win. `--identity` (or the resolved `IdentityFile`) is checked to be a readable private key while the answers are collected, before anything is prompted or generated.
- Without `--identity`, first contact offers ssh-agent keys and `~/.ssh/id_{ed25519,ecdsa,rsa}` as one `publickey` method. An encrypted default key that the agent doesn't hold gets a passphrase prompt in a TTY; otherwise it is skipped and named in `details.skipped_keys`.
- `--discard --name <n>` deletes `pending/<n>` after a typed `discard` (or `--confirm discard`). When the host phase already ran it warns `host_already_bootstrapped`: the discarded pending state holds the only credentials for that server. The `init_pending` error names both `--resume` and `--discard` and carries `details.pending_path`.
- During init, a `step_failed` hint says `eru server init --resume --name <n>` instead of `eru apply`, adding `--local` only when the init is local, and every `step_failed` carries `details.output_tail`: the last 20 redacted output lines of the failed script.

### 8.2 Remote init sequence

Each numbered phase emits `step` events and records progress in `pending/<name>/progress.json`. `--resume` continues from the last completed phase.

1. **Local preflight** (§20).
2. **Collect answers** from flags, the answers file, and prompts. Prompt only in a TTY. Validate (§6.1). The `huh` interview has groups *Server* (server name, node name, admin user, timezone, data root), *Endpoint* (base domain, TLS mode, client allowlist, then ACME email in `acme` mode), and *Workload* (profile, default database size, client connections). It does not ask for databases: they are created after init with `eru db create`. Each question has a flag; a question already answered by a flag or the answers file is skipped. Backup, VPS and extension questions arrive with their phases. Non-interactive runs check `--confirm <server>`, `--confirm-secrets-stored`, `--secrets-out` and `--reveal-passphrase` before anything changes. In `acme` mode, init then shows the `A db1.<domain> → <public IPv4>` record (the master's hostname) so the operator can create it while the host phase runs.
3. **Generate secrets** on the operator (§10.2), stored in `pending/<name>/secrets.json` (0600): the operator WireGuard keypair, SSH ed25519 key, sudo password, `eru_admin` and the service passwords, and the age recovery identity (`ensureRecoveryIdentity`, with or without a bucket), so `--resume` reuses them.
4. **Backup credentials check** (when backups are configured): from the operator, `backup.Probe` (`minio-go`): read the bucket, put/get/delete `<prefix>/.eru-probe`, and refuse a prefix holding another server's stanzas (§13.3). This fails early on bad keys or a bad bucket, before first contact. The cipher pass is generated here and saved in `pending/<name>/secrets.json` beside the age identity.
5. **First contact** over public SSH. Detect arch and OS (`/etc/os-release`). Push the binary (§7.5, bootstrap case).
6. **RPC `cluster.bootstrap` phase=host** over public SSH. The node writes `cluster.toml` and `node.toml`, then runs hardening steps 1–6 (§11), which:
   - create the admin user with the operator SSH key and sudo password hash;
   - install WireGuard with the operator peer;
   - configure ufw with **public 22 still open**.
7. **Switch transport**: start userspace WireGuard to `public_address:51820` and SSH to `10.42.0.1:22` as `admin_user`. Pin the host key (same host keys, new address entry). Run the handshake.
8. **Commit-confirm lockdown** (§8.3).
9. **DNS** (`dns_confirmed`, `acme` mode only). Show the records in one panel (`A`, plus `AAAA → <public IPv6>` when the server has one; when it has none, the panel says there is no AAAA record), with the note that they must be DNS only (not proxied). The panel appears right after first contact, which reads the IPv6 address over SSH (the source of `ip -6 route get 2606:4700:4700::1111`; `--local` reads it directly), so the records propagate while the host phase runs. In a TTY the operator types `confirmed` once, right there (`Type 'confirmed' once the A and AAAA records exist`, or `… the A record exists`); `--dns-ready` skips that pause, and non-interactive runs don't pause. Host bootstrap also reports the IPv6 address, kept in `progress.json`; only when it differs from what first contact read, or on a resume that skipped first contact, does this step show the panel and ask again. eru then queries 1.1.1.1 and 8.8.8.8 once, directly (a Go `net.Resolver` dialing them, so local caches can't lie), and continues whatever they answer. A mismatch is a `dns_mismatch` warning listing each resolver's answers against the expected addresses. Answers inside Cloudflare's published ranges are a `dns_proxied` warning ("proxied by Cloudflare: database clients can't connect through it; set the record to DNS only"). DNS never blocks init: the certificate wait (step 11) shows whether the certificate was issued. `--skip-dns-check` is accepted with a `flag_deprecated` warning. `self-signed` mode skips this phase.
10. **RPC `cluster.bootstrap` phase=database** (`database_bootstrapped`) over WireGuard: it requires `node.toml` of the same server and a completed host phase, writes the new `cluster_toml` through `state.Store`, and runs the database steps. With no `[[clusters]]` yet, that is kernel tuning, the packages, `tls-v1` and `timers-v1` (the certificate timer); the per-cluster steps have no instances, `pgbouncer-v1` skips with `no_databases` and `pgbackrest-v1` with `backups_not_configured`. All database and service passwords travel in `secrets`. Result: `{phase, steps, tls: {state, not_after}, restart_performed}`.
11. **Certificate wait** (`tls_checked`): poll `tls.status` for up to 120 s until the placeholder is replaced. `tls.state` is `acme`, `pending` or `self-signed`; `pending` is a `tls_pending` warning, never a failure. The warning carries the resolver answers (or the Cloudflare hint) from a fresh DNS check, and says that `eru-certs.timer` retries every hour.
11a. **Backups** (`backups_configured`, only when configured): `config.set` with `[backup]` and `apply --step postgres-config-v1 --step pgbackrest-v1 --step timers-v1`, the same code as `eru backup setup` (§13.3), with no restart. It runs here, between the certificate wait and the handoff, so the handoff carries the full recovery kit. Without a bucket, init ends with the `no_offsite_backup` warning (fix: `eru backup setup`), which every `apply` repeats.
12. **Secrets handoff** (`secrets_written`, §10): show the handoff (§10.1) in the terminal and write nothing, unless `--secrets-out <path>` asks for the armored transfer bundle to be written, or the handoff's save action asks for the whole record to be written (both 0600). In a TTY, require typing `stored` (the prompt reads `Type 'stored' once the record is in your password manager`), then offer to clear the screen (default yes). Without a TTY, require `--secrets-out`, `--reveal-passphrase` and `--confirm-secrets-stored`; the passphrase is in `data.passphrase` and the plain secrets are never in structured output. A resumed init that stopped before the confirmation shows a new handoff with a new passphrase. Then RPC `cluster.confirm-secrets` sets `secrets_confirmed_at`.
13. **Create the local context** from the pending data (with `lockdown_confirmed_at` set), make it current, and delete `pending/<name>`.
14. Print the summary: the master's hostname, TLS state, the Backups row ("none off the server: run eru backup setup" when skipped), and `next: ["eru db create <name>", "eru guide agents"]`, plus `eru backup setup` without a bucket. Doctor results join with Phase 4.

### 8.3 Commit-confirm SSH lockdown

The node implements RPC `ssh.lockdown {timeout_s: 120, keep_public: false}`:

1. Snapshot the current state to `/var/lib/eru/ssh-snapshot/`: `/etc/ssh/sshd_config.d/00-eru.conf` and the `ufw show added` rule specs. A re-armed lockdown keeps the first snapshot. Set `state.json.lockdown_pending`.
2. Arm the revert: `systemd-run --unit=eru-ssh-revert --on-active=<timeout> /usr/local/bin/eru node ssh-revert`.
3. Apply:
   - write the final sshd drop-in (§11 step 4) and run `sshd -t`, then `systemctl reload ssh`;
   - `ufw allow in on wg0 to any port 22 proto tcp`;
   - `ufw delete allow 22/tcp` (public), unless `keep_public` (used by `--local`, §8.5).
4. Return `armed: true`. A node already locked down returns `already_locked: true` and changes nothing.

The operator then **opens a new** SSH connection over WireGuard and calls `ssh.confirm`, which runs `systemctl stop eru-ssh-revert.timer`, deletes public 22/tcp if it is still there, deletes the snapshot, clears `lockdown_pending` and sets `locked_down_at`. `ssh.confirm {auto: true}` also locks down a node that has no pending lockdown and was never locked (the caller is on WireGuard, which proves the tunnel). `ssh.status` reports the state, the timers and `reverted.json` for diagnostics.

- If the new connection fails within 60 s, the operator waits for `timeout + 15 s`, reconnects over public SSH, reads `ssh-revert`'s result from `/var/lib/eru/ssh-snapshot/reverted.json`, and returns `lockdown_failed` with diagnostics.
- `ssh-revert` takes the exclusive lock (waiting up to 60 s), does nothing when no lockdown is pending (a confirm won the race), otherwise restores the ufw rules and then the drop-in from the snapshot, reloads ssh, clears `lockdown_pending` and writes `reverted.json`.
- The bootstrap preflight requires sshd to serve port 22 (`sshd -T`), because the lockdown moves SSH to `<wg_address>:22` and a revert reopens public 22/tcp. After a failed lockdown the operator reconnects on public port 22 as the admin user with the operator key.

The SSH restriction is enforced with **ufw** (22/tcp allowed only `in on wg0`), not `ListenAddress`. That avoids boot-ordering lockouts when wg0 comes up after sshd. This is the documented implementation of MASTER's "sshd listens only on wg0".

- **Ubuntu socket activation**: when `ssh.socket` is enabled (Ubuntu ≥ 22.10), the hardening step MUST `systemctl disable --now ssh.socket && systemctl enable --now ssh.service` so sshd config applies and doesn't rely on socket units.

### 8.4 Break-glass

`eru node unlock-ssh --minutes 30`, run from the provider console as root:

1. `ufw allow 22/tcp` and enable `PasswordAuthentication yes` for `admin_user` only: `/etc/ssh/sshd_config.d/01-eru-break-glass.conf` holds `Match User <admin_user>` / `PasswordAuthentication yes` / `Match all`, validated with `sshd -t`.
2. Arm `systemd-run --unit=eru-ssh-unlock --on-active=30m eru node ssh-revert --lockdown-only`.

`ssh-revert --lockdown-only` removes the break-glass drop-in and, when the node was locked down, deletes public 22/tcp again.

The secrets document's `break_glass` text describes this flow.

### 8.5 `--local` mode

`curl -fsSL <base>/install.sh | sudo bash` on the server installs `/usr/local/bin/eru`. It prompts "Initialise this server now (eru server init --local)? [y/N]" when a TTY is present, which runs `eru server init --local`.

`eru server init --local`:

- requires root, or escalates via §20.3, and must run as `/usr/local/bin/eru` (the revert timer calls that path);
- takes the node's public address from the answers file, then `--host <public-ip>`, then the default-route source address (with a warning when it is private);
- skips first contact and binary push and uses the in-process dispatcher;
- generates secrets on the server and runs the same phases as a remote init: host steps, lockdown arming, the DNS step (`acme` mode), the database phase and the certificate wait;
- shows the secrets handoff (§10.1) on the server console without the save action, writes nothing to `/root`, and prints a one-line instruction: "On your machine run `eru login` and paste the transfer bundle".

For lockdown in `--local`, the commit-confirm verification needs an operator over WireGuard. So `--local` arms the revert timer with `timeout = 30 min` and prints "run `eru login …` then any `eru status` from your machine within 30 minutes to confirm".

- The lockdown is `ssh.lockdown {timeout_s: 1800, keep_public: true}`: public SSH stays open until confirmed, which is the only behaviour in `--local`.
- The first time a context reaches the node over WireGuard (its `lockdown_confirmed_at` is empty), the operator calls `ssh.confirm {auto: true}` and records the time. That confirms a pending lockdown, or locks down a node whose 30-minute timer reverted.
- No context is created on the server; the pending state is removed after the document is confirmed.

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
    DryRun        bool
    Shell         Shell             // runs embedded scripts (deploycrate Shell + secrets redaction + per-run log)
    Secrets       *secrets.Registry // every secret value, for redaction
    SecretValues  map[string]string // the request's secrets by name, for steps that need a raw value
    Log           *RunLog
    Host          hostinfo.Info
    RotateSecrets bool              // replace runtime secrets already on disk (eru backup setup)
}
```

There's no shared PostgreSQL connection in the runtime: a step that talks to a cluster opens `steps.ClusterPG(spec)` on that cluster's socket, so nothing can fall back to a default socket.

**Per-cluster instances.** A step that runs once per database implements `engine.Template` (`Instance(cluster) Step`). `engine.Expand` turns the catalog into one step per `[[clusters]]` entry for each template, in catalog order; an instance's ID is `<id>@<cluster>` (`InstanceID`, `SplitID`) with its own inputs hash and journal entry, so changing `app` re-runs nothing for `billing`. The templates are `postgres-cluster-v1`, `postgres-config-v1` and `postgres-roles-v1`. `Options.DB` (`apply --db`, `plan --db`) keeps only that database's instances; `--step postgres-config-v1` runs every instance and `--step postgres-config-v1@app` one.

Optional interfaces: `engine.FileRenderer` (plan diffs); `engine.Skipper` (`SkipReason(cfg)`: skip without a journal entry, e.g. `pgbackrest-v1` without `[backup]`, reason `backups_not_configured`, or `pgbouncer-v1` without databases, `no_databases`); `engine.Forcer` (`Force(cfg, rt)`: run although the journal and hash say current, with the returned reason, e.g. `backup_credentials_rotated` when `Runtime.RotateSecrets` brings new bucket keys that are never in the inputs). `engine.Shell.Exec` runs a fixed program (no bash) with the same environment, process-group and redaction rules as scripts, for tools whose output eru reads (pgbackrest, pg_dump, pg_restore, pg_ctl, pg_basebackup).

### 9.2 Runner semantics

For each step in order:

1. Compute `inputs_hash = sha256(ID || Inputs())`.
2. If the journal has `{id, status: completed, inputs_hash}` that matches, the step is skipped (`state: skipped`, reason `journal`).
3. Otherwise call `Check`. If complete **and** the hash matches the journal, skip (reason `check`). A complete check with a changed hash still runs `Apply`, because the files must be re-rendered.
4. Otherwise `Apply`, then journal `{status, inputs_hash, completed_at, restart}`.

Restart needs are aggregated **per cluster**. At the end the runner restarts each cluster with a pending restart whose token was supplied (`--confirm <db>/restart`, or `--confirm <server>/restart` for all of them), one cluster at a time through `Restarter.Restart(ctx, cluster)`, and `ApplyData.Restarts` reports each cluster and its downtime. Otherwise the restart stays pending and is reported with the tokens that would perform it (`confirm_tokens`). Reloads happen immediately per step.

A step that fails with `precondition_failed` or `input_required` keeps that code, its hint and its reason in the `step_failed`-shaped error (with `details.step`), so agents can tell a refusal from a failed script.

Pending restarts are saved in the journal (as instance IDs), so a later confirmed `apply` still performs the restart even though the steps themselves now skip. That apply restarts each confirmed cluster once and clears its entries; `plan` reports them too. If a step needs a restart and the build has no restarter, `apply` fails with `error` rather than promising a restart that can't happen.

`plan` runs steps 1–3 and lists those that would apply, plus file diffs from `Inputs()` against the on-disk files (unified diff, secrets redacted).

### 9.3 Journal (`/var/lib/eru/journal.json`)

```json
{"schema":1,"eru_version":"1.0.0","updated_at":"…","restart_pending":["postgres-config-v1@app"],
 "steps":{"postgres-config-v1@app":{"status":"completed","inputs_hash":"…","started_at":"…","completed_at":"…","restart":"restart","error":""},
          "tls-v1":{"status":"completed","inputs_hash":"…","started_at":"…","completed_at":"…","restart":"none","error":""}}}
```

Write it atomically (temp file plus rename). Updates merge under a file lock, because an `apply --db` holds the global lock only shared and two of them may write at once. Steps removed from the catalog, and instances of a dropped database, are left as-is and ignored.

### 9.4 Embedded scripts

Use `//go:embed scripts/*.sh`. Run each script via `/bin/bash -s` with the env map, as in deploycrate `Shell.Run`. Scripts get a fixed `PATH`, `LANG`, `HOME` and `DEBIAN_FRONTEND` plus the step's validated keys, and don't inherit the caller's environment. Each runs in its own process group, which gets SIGTERM on cancel and SIGKILL 10 s later. The combined output is streamed line by line as `log` events, redacted, and appended to the run log.

Run logs live in `/var/log/eru/`. `plan` and `apply` write one; `selfcheck` and dry-run `apply` don't. Lock and version refusals happen before the log opens, so they leave none.

Rendered files are written by Go (`render` package) with an atomic write and owner/mode set, **not** by heredocs, for anything with a template. Scripts handle packages, services, users and ufw.

### 9.5 Locks

Three kinds of `flock`, always taken in this order: the global lock, the heavy-I/O lock, then cluster locks by name.

- **Global** (`/run/lock/eru.lock`): **exclusive** for server-level changes (`apply` without `--db`, `cluster.bootstrap`, `ssh.*`, `upgrade.*`, `pg.update`, `config.set`, `db.create`, `db.drop`, `db.resize`, `tune.apply`, the create part of `db.fork`); **shared** for everything that touches one database or only reads (`plan`, `apply --db`, `db.list`, `db.credentials`, `backup.list`, `sql`, `restore.promote|undo|abort|cleanup`, and every job: backup, verify, drill, dump, restore, fork).
- **Per cluster** (`/run/lock/eru-<db>.lock`, with its own `.info`): methods declare it with `clusterLock`. `apply --db`, `db.resize`, `db.credentials --rotate` and `restore.promote|undo|abort|cleanup` take it **exclusive**; every database job (backups, verify, drill, dump, restore, and fork for its source and new name) takes it **shared** with no wait, so a busy lock records `skipped` with `lock_busy`. An unknown database is refused. So an `apply` that restarts `billing` never waits for a promote on `app`.
- **Heavy I/O** (`/run/lock/eru-heavy-io.lock`, exclusive, timeout 0): every database job except incrementals (full, diff, verify, drill, dump) takes it after the global lock, so only one runs on a server at a time; a busy lock records `skipped` with `lock_busy` and `lock: heavy_io` (§13.4).

If a lock can't be acquired within `--timeout` (default 30 s; timers use 0), return `precondition_failed` (`reason: lock_held`) with the holder (PID, method, start time, written to `<lock>.info`) and, for a cluster or the heavy-I/O lock, which one. Exclusive holders remove the info file on release; shared holders leave it, and it names only the most recent shared holder. A holder whose PID is gone is reported as `null`.

Maintenance additionally refuses while a pgBackRest backup of that database is running. Check `pgbackrest --stanza=<s> info --output=json`'s `status.lock.backup.held`.

### 9.6 Redaction

`secrets.Registry` collects every secret value loaded or generated in the process. All output paths (events, logs, errors, envelopes) pass through `Redact`, which replaces values of 6 or more characters with `«redacted»`.

---

## 10. Secrets

### 10.1 Secrets handoff and transfer bundle

eru never writes the secrets document to disk by default. At the end of init (and on `eru secrets export`) it shows the **secrets handoff**, the server's master record. The terminal shows only a two-line note on what the record is and the bundle passphrase, then a menu:

1. **Copy everything to the clipboard** (always offered): sends the plain-text record to the terminal clipboard with OSC 52, which also works from a server console over SSH. The record has every value grouped like password-manager entries (cluster, each node, the operator with its SSH and WireGuard private keys, the cluster admin `eru_admin`, the service roles, then backups and metrics from their phases, then the break-glass text; groups with nothing yet are left out). It holds cluster access only: application database passwords are never in it (§10.2), then the **transfer bundle**: its passphrase and the document below as an ASCII-armored `age` block (`-----BEGIN AGE ENCRYPTED FILE-----`).
2. **Save everything to a file** (only on the operator's machine, so not in `server init --local`): asks for a path relative to the current directory, which the prompt names (default `./<server>-secrets.txt`; absolute and `~/` paths work too), refuses an existing file, and writes the whole record, the same text the copy action sends (every value, the passphrase and the armored bundle), with mode 0600. It is offered whether or not `--secrets-out`/`--out` wrote the bundle.
3. **Show everything on screen** (always offered): prints the same record in the terminal, for terminals that block OSC 52 (such as tmux without `set-clipboard on`).
4. **Done**: leaves the menu for the `stored` confirmation.

`eru login` takes the bundle from a paste, stdin or a file. A paste may be the whole copied record: lines before the BEGIN line are skipped.

The bundle is `age` scrypt-encrypted (`age.NewScryptRecipient(passphrase)`, work factor 18). The plaintext is JSON:

```json
{
  "schema": "eru-secrets/v1",
  "created_at": "…", "eru_version": "1.0.0",
  "server": { "name": "prod", "domain": "example.com", "tls_mode": "acme" },
  "nodes": [ { "name": "db1", "role": "master", "hostname": "db1.example.com", "public_address": "203.0.113.10",
               "listen_port": 51820, "wg_address": "10.42.0.1", "wg_public_key": "…", "ssh_host_keys": ["ssh-ed25519 AAAA…"] } ],
  "operator": { "name": "op-1", "wg_address": "10.42.255.1", "wg_private_key": "…",
                "ssh_user": "eru", "ssh_private_key": "-----BEGIN OPENSSH PRIVATE KEY-----…", "sudo_password": "…" },
  "database": {
    "admin": { "user": "eru_admin", "password": "…" },
    "services": { "pgbouncer_auth": {"user":"pgbouncer_auth","password":"…"}, "eru_monitor": {"user":"eru_monitor","password":"…"} }
  },
  "backup": { "provider": "r2", "endpoint": "…", "region": "auto", "bucket": "…", "prefix": "prod", "uri_style": "path",
              "access_key_id": "…", "secret_access_key": "…",
              "pgbackrest": { "cipher_pass": "…" },
              "dumps": { "age_identity": "AGE-SECRET-KEY-1…", "age_recipient": "age1…" } },
  "metrics": { "base_url": "https://db1.example.com/metrics", "username": "metrics", "password": "…" },
  "break_glass": "…markdown text…"
}
```

- **Passphrase**: 8 words from the EFF large wordlist (embedded, about 103 bits), joined with `-`. It's displayed once in the handoff and never stored by eru.
- **`eru login [bundle-file|-]`**: with no argument in a TTY, prompt to paste the bundle and read up to its END line; `-` reads it from stdin (not together with `--passphrase-stdin`); a path reads a file. Armored and binary `age` both work, so old `.secrets.age` files still open. Decrypt (passphrase from TTY, `--passphrase-stdin` or `--passphrase-file`) and create `contexts/<name>/…`. Bad passphrase returns `secrets_error`.
- **`eru secrets export`**: rebuilds the document from the context and shows a new handoff with a new passphrase, with the same `stored` confirmation. `--out <path>` also writes the armored bundle. Without a TTY it needs `--out` and `--reveal-passphrase`, like init. Credentials rotated since login require a re-login or the `credentials` command. The previous bundle still opens with its own passphrase.

### 10.2 Generated secrets

| Secret | Generator | Where it lives after init |
| --- | --- | --- |
| Admin user sudo password | 24 chars base62 | Doc; node has `/etc/shadow` hash only |
| `eru_admin` password | 32 chars base62 | Doc; node has PG SCRAM verifier only |
| Per-db `migrate`/`app`/`ro` passwords | 32 chars base62 | Shown once by `eru db create` / `db credentials --rotate`; eru keeps none; node has PG SCRAM verifiers only |
| `pgbouncer_auth` password | 32 chars base62 | Doc (`database.services`) + node `/etc/eru/runtime/pgbouncer_auth.pass` (0600) and its SCRAM verifier in `userlist.txt` |
| pgBackRest `cipher_pass` | 64 chars base62, on the operator | Doc (recovery kit) + `credentials.json` + node `pgbackrest.conf` |
| Recovery (dump) age identity | `age.GenerateX25519Identity`, on the operator at `server init`, with or without a bucket; `backup setup` reuses it | Doc (recovery kit) + `credentials.json` only; the node gets the recipient (`backup.env`), and the identity only for the length of a `restore --dump` job. the master bundles are encrypted to the same recipient, which the node keeps in `/etc/eru/runtime/recovery.recipient` with or without a bucket (Phase 5) |
| Metrics password | 32 chars base62 | Doc + node bcrypt hash in an nginx `auth_basic_user_file` |
| `eru_monitor` password | 32 chars base62 | Doc (`database.services`) + node `/etc/eru/runtime/monitor.pass` (0600) and its SCRAM verifier in `userlist.txt` |
| Bucket keys | user-provided | Doc (recovery kit) + `credentials.json` + node `backup.env`, `pgbackrest.conf` |

Use `crypto/rand` only.

**The secrets document holds server access credentials only**: every secret needed to reach, run and rebuild the server. Application database passwords are not server access and are never stored by eru: `eru db create` generates them, sends them to the node, and prints them once (with the connection URLs and notes) for the human or the app's secret store; a lost one is replaced with `eru db credentials <db> --rotate`. eru's own access to a database goes through `eru_admin`. The **operator** generates all database passwords: `eru_admin`, the service roles `pgbouncer_auth` and `eru_monitor`, and each database's `migrate`/`app`/`ro`. The node never generates one. It keeps SCRAM verifiers only, except the two service passwords that PgBouncer and the exporters need at runtime.

- **RPC secret names:** `db.admin`, `db.<db>.migrate`, `db.<db>.app`, `db.<db>.ro`, `svc.pgbouncer_auth`, `svc.eru_monitor`, `backup.s3_key_secret`, `backup.cipher_pass`, `backup.age_identity` (restore from a dump only), plus the non-secret `backup.s3_key_id` and `backup.age_recipient` in the same map (constants/helpers in `internal/steps/secrets.go`). The node registers every received secret for redaction before anything logs.
- **Verifiers, not plaintext:** `pg.SCRAMVerifier` (SCRAM-SHA-256, 4096 iterations, random 16-byte salt) is computed in Go and sent as `ALTER ROLE … PASSWORD '<verifier>'` on psql's stdin, so the plaintext never reaches psql, server logs or `pg_stat_statements`. Verifiers are redacted too.
- **Set once:** a password is set only when its role is created, or by `db credentials --rotate`. `postgres-roles-v1` never re-sets an existing role's password, and passwords are not part of its inputs. A login role that must be created without a supplied secret fails with `input_required` naming the role. The same goes for a missing `/etc/eru/runtime/*.pass`.
- **Operator top-up of the recovery kit:** `apply` also sends the kit from `credentials.json`; the node writes it only where missing (§13.3). Backups on and no kit is `input_required`.
- **Operator top-up:** before `apply` and init's database phase, the operator compares `eru_admin` and the service roles with `credentials.json` (never `[[clusters]]`), generates anything missing, saves it to `credentials.json`, sends it, and warns `secrets_document_stale` ("run `eru secrets export`"). It saves before sending: a role created with a password the operator then lost would be unrecoverable.
- The document's `database` section holds `admin` and `services` (`pgbouncer_auth`, `eru_monitor`). The per-database URLs and connection notes (5432 is reachable only over WireGuard or with `expose_postgres = true`) are printed by `eru db create`, `db fork` and `db credentials --rotate` (`secretsdoc.DatabaseURLs`). Their host is the hostname of the node that holds the database (`config.DatabaseHostname`, `db1.<domain>` today) and their path is the database. URLs use `sslmode=verify-full&sslrootcert=system` in `acme` mode and `sslmode=require` in `self-signed` mode. `app` is on 6432 (the transaction pooler), `app_direct_tls` on 443 with `sslnegotiation=direct` (PG17+ libpq, pgx 5.7+), and `migrate` on 5432 (the session pooler: session features for migrations). `<d>_migrate` is a member of `eru_direct_only`, so the transaction pooler refuses it on 6432 and 443.

---

## 11. Step catalogue

Order is fixed. IDs end in `-v1`. Every step stays `-v1` until the first release; after that, bump the suffix whenever behaviour changes in a way the input hash can't capture (for example, script logic changes). Columns: what it does, and how Check decides completeness.

| # | ID | Does | Check |
| --- | --- | --- | --- |
| 1 | `base-packages-v1` | Wait for `cloud-init status --wait`. apt update and install: `curl ca-certificates gnupg sudo ufw fail2ban python3-systemd wireguard-tools chrony logrotate unattended-upgrades jq lsb-release`, plus `apt-transport-https` when the release still has it (it is transitional). `python3-systemd` lets the fail2ban sshd jail read the journal | All installed (dpkg-query) |
| 2 | `admin-user-v1` | Create `admin_user` (shell bash, groups `sudo`), set the password hash, install the operator SSH pubkey to `authorized_keys`, write `/etc/sudoers.d/eru` (§7.4) and validate it | User exists, key present, sudoers matches |
| 3 | `wireguard-v1` | Adapt deploycrate `wireguard.sh` (no ip_forward: not a gateway). Address `10.42.0.1/16`; operator peers rendered from `cluster.toml` + operator registry (`/etc/eru/operators.toml`: name, wg pubkey, address). `ufw allow 51820/udp`. The node key lives in `/etc/wireguard/eru.key` (`eru.pub`); a running wg0 is updated with `wg syncconf` | `wg show wg0` lists all peers |
| 4 | `sshd-hardening-v1` | Write `/etc/ssh/sshd_config.d/00-eru.conf` (below). It sorts first because sshd keeps the first value it reads, so provider drop-ins such as `50-cloud-init.conf` cannot re-enable passwords. Handle `ssh.socket` (§8.3). `sshd -t` and check that `sshd -T` shows `permitrootlogin no` and `passwordauthentication no` (roll back otherwise), reload. Public 22 stays open until lockdown | Drop-in matches render |
| 5 | `firewall-v1` | ufw: `default deny incoming`, `default allow outgoing`, allow 51820/udp, 80/tcp, 443/tcp, `in on wg0` 22/tcp, 6432/tcp (the transaction pooler) and 5432/tcp (the session pooler); the allowlist rules for 6432 (if `expose_pgbouncer`) and 5432 (if `expose_postgres`). Nothing per database: clusters listen on sockets only. WireGuard peers always reach both ports; the expose flags gate only the allowlist. Keep public 22 only if `lockdown_pending` or not yet locked. Rules not in the set are deleted (expected rules are added before and after, so SSH always has an allow rule). `ufw --force enable` | `ufw status` active and `ufw show added` equals the expected rule set |
| 6 | `host-baseline-v1` | journald drop-in (copy deploycrate `host.sh`), swapfile (`swap_mb`), timezone, chrony enabled, unattended-upgrades security-only with `Package-Blacklist` `postgresql-`, `libpq`, `pgbouncer`, `pgbackrest`, `nginx`, `libnginx-mod-` and `Automatic-Reboot "false"` (via `#clear` of the distribution's origins), fail2ban sshd jail (`backend = systemd`, iptables ban actions so bans stay out of the ufw rule set), logrotate for `/var/log/eru` | Files match render, services active |
| 7 | `kernel-tuning-v1` | `/etc/sysctl.d/90-eru.conf` and the `eru-thp.service` oneshot (below), `sysctl --system`, `daemon-reload`, `enable --now eru-thp.service`. `vm.nr_hugepages` is the **sum** of each cluster's estimate for the clusters with `huge_pages = try` (§12.3); 0 when none. `db create` and `db resize` run it before a cluster's own steps, so the pool grows before a 2 GiB+ cluster starts | `sysctl -n` values equal render, THP `enabled` = `[never]` |
| 8 | `postgres-packages-v1` | Write `/etc/postgresql-common/createcluster.d/00-eru.conf` with `create_main_cluster = false` **before** installing. Install `postgresql-common`, run `/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y`, install `postgresql-18 postgresql-client-18 postgresql-contrib pgbouncer pgbackrest`, then `apt-mark hold postgresql-18 postgresql-client-18 pgbackrest`. pgBackRest older than `versions.PgBackRestMinVersion` (2.59.3) is refused before it is installed: the step fails with `precondition_failed`, `reason: pgbackrest_too_old`. Debian's pgbouncer auto-starts with its own config, so `systemctl stop pgbouncer` right after install until step 13 renders it. | Packages installed and held |
| 9 | `postgres-cluster-v1@<db>` | If the cluster doesn't exist: `pg_createcluster 18 <db> --port=<port> --datadir=<data_root>/<db> --locale=<locale> --start-conf=auto -- --auth-local=peer --auth-host=scram-sha-256 --locale-provider=<provider> …` (checksums are default-on in 18; assert `data_checksums = on` on its socket and fail the step otherwise). A missing `data_root` is created `postgres:postgres 0700`. An existing `18/<db>` with a **different** data directory or port is `precondition_failed` (`cluster_mismatch`); eru never re-creates a cluster. Writes the systemd drop-in `postgresql@18-<db>.service.d/10-eru.conf`: `LimitNOFILE=65536`, `OOMScoreAdjust=-900`, `Environment=PG_OOM_ADJUST_FILE=/proc/self/oom_score_adj PG_OOM_ADJUST_VALUE=0`, `MemoryHigh=` (memory + 25%), `CPUWeight=`, `IOWeight=`, and `MemoryMax=` / `CPUQuota=` when set. Then `daemon-reload`, and `systemctl set-property --runtime` on a running cluster (limits change live, no restart) or `systemctl start` | `pg_lsclusters` shows `18/<db>` online with its data directory and port, and the drop-in matches |
| 10 | `tls-v1` | Refuse a server that has `caddy` installed (an older eru used Caddy; there is no migration, rebuild the server). Install `nginx` and `libnginx-mod-stream` from the distribution (at least `NginxMinVersion`), `apt-mark hold` them, remove the default site. Add `postgres` and `pgbouncer` to `ssl-cert`. If no certificate exists, write a self-signed P-256 placeholder (30 days, `CN`/SAN = the node's hostname, `steps.NodeHostname(cfg)`) into `/etc/eru/tls` so the poolers and nginx can start; TLS never blocks on ACME. It runs before `postgres-config-v1`, which points every cluster's `ssl_cert_file` at it. Render `/etc/nginx/nginx.conf` (§15.2) with the node's hostname as `server_name`, `nginx -t`, `systemctl reload-or-restart nginx`. Install `eru-certs.service` and `systemctl start --no-block eru-certs.service` so issuance starts at once (§15.3); `timers-v1` owns the hourly `eru-certs.timer` | nginx active, `/etc/eru/tls/server.crt` exists |
| 11 | `postgres-config-v1@<db>` | Render into `/etc/postgresql/18/<db>/conf.d/`: `10-eru-tuning.conf` (§12, from the cluster's size), `20-eru-security.conf` (§14.1: socket only, its port), `30-eru-extensions.conf` (§17), `40-eru-backup.conf` (§13.1: `archive_mode = on` always, `archive-push` to the database's stanza with backups on, `/bin/true` without), `50-eru-settings.conf` (`[postgres.settings]`, then `[clusters.settings]`, values single-quoted with `''` escaping), and create `90-overrides.conf` (empty, comment header) if missing. Render `pg_hba.conf` and `pg_ident.conf` (§14.1). Every file is written owned by `postgres:postgres` with `0640` for `pg_hba.conf` and `pg_ident.conf` and `0644` for `conf.d`, so the server can read them before the reload. Ensure `include_dir = 'conf.d'` is in `postgresql.conf`. Before reload, `pg_file_settings` and `pg_hba_file_rules` must have no errors; otherwise the previous files are restored and the step fails with the errors. Classify changes via `pg_settings.pending_restart` (a restart of this cluster, listed in the notes). A cluster's **first configuration** (none of the managed files existed) restarts it unconditionally and without confirmation, because there is no traffic yet. When huge pages change, `shared_memory_size_in_huge_pages` is measured while the cluster is stopped during that restart, and `vm.nr_hugepages` is re-applied through step 7's helper | Rendered = on disk, with owner and mode |
| 12 | `postgres-roles-v1@<db>` | §14.2 SQL through `steps.ClusterPG(spec)` (psql as the `postgres` OS user on the cluster's socket, SQL on stdin): the service roles with setup-wide verifiers, the `eru` schema with both auth functions, `eru_direct_only`, then that database with its four roles, default privileges and extensions. Passwords follow §10.2 (set once, verifiers only). Writes `/etc/eru/runtime/{pgbouncer_auth,monitor}.pass` from the request secrets when missing. Inputs are names, timeouts and extensions, never passwords | All roles, the database, grants, extensions and both auth functions exist; the transaction pooler's function refuses `eru_direct_only`, and `<db>_migrate` is a member |
| 13 | `pgbouncer-v1` | Skipped (`no_databases`) without `[[clusters]]`. Render both poolers (§14.3) from one template: `/etc/pgbouncer/pgbouncer.ini` + `pgbouncer_hba.conf` (transaction, 6432) and `/etc/pgbouncer/session.ini` + `session_hba.conf` (session, 5432), the `eru-pgbouncer-session.service` unit, and the shared `userlist.txt` (verifiers read back from `pg_authid` for `pgbouncer_auth` and `eru_monitor`), `postgres:postgres 0640`, overwriting Debian's files. `pgbouncer.sh` enables both and reloads a running one or starts it: adding or removing a database is a reload | Rendered = on disk (owner and mode included), and `SHOW VERSION` as `eru_monitor` works on both admin consoles (via a 0600 `PGPASSFILE`) |
| 14 | `pgbackrest-v1` | Skipped (`backups_not_configured`, no journal entry) without `[backup]`. Otherwise `install -d` `/etc/pgbackrest`, `/var/spool/pgbackrest`, `/var/log/pgbackrest` (and `/var/lib/pgbackrest` with `local_repo`), write `backup.env` and `/etc/pgbackrest/pgbackrest.conf` (§13.1, §13.3: a section per stanza). With no clusters yet it stops there, so `server init` and `backup setup` on an empty server leave the bucket keys and cipher pass on the node for the first `db create`. Then, for each cluster without a full backup (one `pgbackrest info` over every stanza decides): `stanza-create`, `check` and a full backup (`--stanza`, `--pg1-port`; streams progress). Clusters that already have a full are skipped unless the request rotates the keys, so `db create` only touches the new stanza. The repository, not the journal, is the source of truth for the first full | Conf on disk equals the render, `backup.env` exists, `info` lists a full backup in every stanza (no `info` without clusters) |
| 15 | `master-backup-v1` | Phase 5 (§13.11). Create `/var/lib/eru/master-backup` (root 0700), write `/etc/eru/runtime/recovery.recipient` from `backup.age_recipient` when missing (else from `backup.env`; `input_required` with neither), make `/var/lib/eru/instance` when missing, and mark the master backup dirty so the first bundle follows. `timers-v1` renders `eru-master-backup.{service,path,timer}` and, with a bucket, `eru-fence.{service,timer}`. Rechecked on every apply | The directory, the recipient and the instance id exist |
| 16 | `metrics-v1` | Phase 4. Create the exporters' system users, fetch the pinned binaries (sha256-checked), write the units, `eru-exporters.slice`, the htpasswd (only when missing), `/etc/nginx/eru-metrics.conf`, `targets.json` and the fail2ban jail, `nginx -t` (the previous include comes back on failure), then start the exporters and reload nginx and fail2ban (§15.1, §15.2). The pgbouncer_exporters run only while there are databases. With `[metrics] enabled = false` it stops the exporters and removes the include. `db create`, `db fork` and `db drop` run it (`metrics_reload_failed` on failure) | Binaries at the pinned version, files on disk, units active, node_exporter answers 200 |
| 17 | `timers-v1` | Pulled forward from Phase 4. Render `eru-certs.timer`; with backups on, the template services and a timer per database and job (§18), and `eru-stanza-expiry.service`/`.timer`. Disable and remove `eru-*.timer` (and the services it owns) not in the render, `daemon-reload`, `enable --now` the timers. A dropped database's timers go as stale units | Files match, every timer enabled, no stale units |
| 18 | (doctor gate) | Not a catalogue step, so a second `apply` still skips everything: a full `apply` runs doctor (§21) after the steps and returns `data.doctor` (failing checks are `doctor_check_failed` warnings); `server init`'s database phase fails with `doctor_failed`. `--skip-doctor` skips it | — |

`cluster.bootstrap phase=host` runs steps 1–6 (`steps.HostStepIDs()`). `phase=database` runs the database steps (`steps.DatabaseStepIDs()`), and `apply` runs everything, with each template expanded into one instance per `[[clusters]]` entry. Each phase appends its own steps: Phase 2 has 1–13, Phase 3 adds 14 and 17 (both in `DatabaseStepIDs()`, after `pgbouncer-v1`), and Phase 4 adds 16 and 18 (step numbers are run order, not phase order). `eru backup setup` runs `steps.BackupStepIDs()`: `postgres-config-v1` (every instance: `archive_command` changes with a reload), `pgbackrest-v1`, `timers-v1`. It doesn't install packages; before writing `[backup]` it plans `postgres-packages-v1` and refuses with `precondition_failed` (`reason: node_outdated`, hint `eru apply`) if it would still run. The database commands run subsets: `db create` runs `kernel-tuning-v1`, the new database's instances, `pgbouncer-v1`, and with backups `pgbackrest-v1` and `timers-v1`; `db drop` runs `pgbouncer-v1` and `timers-v1` after the cluster is gone; `db resize` runs `kernel-tuning-v1` and the database's instances.

**Restarter** (`steps.Restarter(cfg)`): per cluster, `PAUSE <db>` on the transaction pooler's admin console (as `pgbouncer` over the socket, as the postgres OS user; a failed or timed-out PAUSE doesn't stop the restart) → `pg_ctlcluster 18 <db> restart -m fast` → wait for `pg_isready` on its socket (60 s) → `RESUME <db>`. Every other database keeps serving. The session pooler isn't paused: in session mode PAUSE waits for every client to leave. A restart needs `--confirm <db>/restart` (or `<server>/restart`) except on a cluster's first configuration.

**Plan diffs:** steps that write managed files implement `engine.FileRenderer`. For each step that would apply, `plan` (and `apply --dry-run`, `config set --dry-run`, `config edit`) shows a unified diff of each rendered file against disk, with secrets redacted. `userlist.txt` holds verifiers and is never diffed.

### 11.1 Fixed file contents

`/etc/ssh/sshd_config.d/00-eru.conf` (plus a `# managed by eru` header):

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

- `Input{RAMMiB, VCPU, Clusters, DiskRotational, DataDiskGiB, Profile, MaxConnections, ClientConnections, PGMajor, Extensions}`;
- `Result{Settings []Setting, SharedBuffersMiB, HugePages}`, with `Setting{Name, Value, Unit, Reason, Context}` (`Context` mirrors `pg_settings.context`).

It's pure, with no I/O, and runs **once per cluster**. `steps.tuningFor(cfg, spec, host)` fills the input: `RAMMiB` is the cluster's resolved `memory` (from its size), `VCPU` the server's with `Clusters` the number of clusters, `MaxConnections` from its size, `Profile` its own or `[postgres] profile`, and `DataDiskGiB` its share of the data disk in proportion to its memory. `hostinfo` measures the disk under `data_root` (`findmnt -no SOURCE`, then `/sys/block/<dev>/queue/rotational`, resolving partitions and device-mapper to the parent). `[postgres.settings]` and then `[clusters.settings]` override the result.

### 12.1 Formulas

Let `R` = the cluster's memory in MiB, `C = CPUShare(VCPU, Clusters)` = `min(VCPU, max(2, VCPU / Clusters))`, and `P ∈ {oltp, mixed, analytics}`.

| Setting | Value |
| --- | --- |
| `shared_buffers` | `R < 2048 ? max(128, R*0.20) : min(R*0.25, 65536)` MiB |
| `effective_cache_size` | `R * (P==analytics ? 0.75 : 0.70)` MiB |
| `max_connections` | From the size (20 / 30 / 50 / 100 / 200); `clamp(4*C, 50, 400)` only without one. Both pools plus 3 must fit (§6.1) |
| `superuser_reserved_connections` / `reserved_connections` | 0 / 3, the 3 slots both pools leave free (`eru_admin` and, from Phase 4, `eru_monitor` get `pg_use_reserved_connections`; superusers may use them too) |
| `work_mem` | `(R - shared_buffers) / (max_connections * f)` with `f = {oltp:3, mixed:2, analytics:1}`, clamped to `[4, P==analytics ? 1024 : 256]` MiB |
| `hash_mem_multiplier` | 2.0 |
| `maintenance_work_mem` | `clamp(R/16, 64, 2048)` MiB |
| `autovacuum_work_mem` | `min(maintenance_work_mem, 1024)` MiB |
| `huge_pages` | `try` if `R ≥ 2048`, else `off`, so a fragmented pool can't leave a small cluster's start-up at the mercy of another's |
| `wal_level` | `logical` (Phase 6's physical followers drop it to `replica`) |
| `wal_compression` | `zstd` |
| `wal_buffers` | `-1` |
| `min_wal_size` | `clamp(DataDiskGiB*0.02, 1, 8)` GiB, of the cluster's disk share |
| `max_wal_size` | `clamp(DataDiskGiB*0.10, 2, 64)` GiB, of the cluster's disk share |
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

Every `Setting.Reason` is one sentence naming the inputs, for example "25% of the cluster's 2048 MiB". `eru tune explain --db <db>` prints the server sheet (§12.4), the cluster's inputs and then a table of name, value, previous value and reason. Without `--db` it works only with one database, else `input_required` listing them.

### 12.3 Huge pages

- Per cluster with `huge_pages = try`: during its first configuration, and on restart-requiring retunes, stop the cluster and run `postgres -D <data_root>/<db> -c config_file=/etc/postgresql/18/<db>/postgresql.conf -C shared_memory_size_in_huge_pages`; use that value + 5%.
- Otherwise estimate `ceil(shared_buffers_MiB * 1.05 / 2) + 64`.
- `vm.nr_hugepages` is the **sum** over the clusters with `huge_pages = try` (`steps.hugePagesFor`); 0 when none.

### 12.4 Memory budget and oversubscription

- **Reserve and budget:** `ServerReserve` = `max(512 MiB, 25% of RAM)`, or `[server] memory_reserve`. `ClusterBudget` = RAM − reserve. eru never rebalances existing clusters.
- **The sheet** (`internal/tune/budget.go`): a `Demand` per cluster (size, memory, CPU weight and quota, start-up shared memory = the huge-page estimate × 2 MiB, huge pages, `max_wal_size`) and a `Host` (RAM, vCPU, reserve, `CommitLimit`, `Committed_AS`, `MemAvailable`, free huge pages, disk size and use) make a `Sheet`: allocated, free or overcommitted. `eru tune show` prints it with each cluster's size, memory, CPU and IO weight, connections, opt-in caps, current use (`memory.current` of `system.slice/system-postgresql.slice/postgresql@18-<db>.service`) and state, the commit limit, the huge pages, and whether `IOWeight` is enforced (`bfq`, or `io.cost.qos` `enable=1` for the disk; otherwise "not enforced").
- **Warnings** (`Sheet.Warnings`): `memory_overcommitted` (the sizes add up to more than the budget, with the numbers and the options: `db resize`, a node, a bigger VPS), `cpu_crowded` (more clusters than 2 × vCPU, or `cpu_quota`s over 100% × vCPU), `disk_crowded` (data-root usage + every `max_wal_size` + the 4 GiB archive spool over 80% of the disk). They come back from `plan`, `apply` (and its dry run), `config set`, `tune show|explain|apply`, `db create` and `db resize`; `status` and `doctor` add them in Phase 4.
- **Refused:** `would_not_start` (`WouldNotStart`): the cluster's shared memory doesn't fit in `CommitLimit − Committed_AS` (plus what a restart frees), unless its huge pages are already free in the pool; with `settings.huge_pages = on`, the free pages plus what `MemAvailable` could add must cover it. An unreadable `/proc/meminfo` refuses nothing. The hint and `details.smallest_size` name the smallest size that would start (`SmallestThatStarts`).
- **Refused unless allowed:** `memory_overcommitted` (`checkBudget` in `internal/node/budget.go`, `LargestThatFits`): `db create` (with or without `--size`; without it the size is `default_size`), `db fork` and a growing `db resize` are refused before `cluster.toml` is saved when the new or larger size takes the sum past the budget and adds memory: `precondition_failed`, `details.reason = "memory_overcommitted"`, with the numbers in the message and details (`memory_mib`, `allocated_mib`, `budget_mib`, `ram_mib`, `reserve_mib`, `left_mib`). The hint names the largest size that still fits in what the other databases leave (for a resize, only sizes above its current memory) with the complete command (`details.largest_size`, `details.suggested_command`, for example `eru db create big2 --size small`); when none fits it says so and names the largest other database to resize down, or a bigger server. Nothing steps down by itself. `--allow-overcommit` (param `allow_overcommit`, sent only when set) goes ahead with the `memory_overcommitted` warning. A shrink is never refused, and an unknown budget (no RAM detected) refuses nothing.
- **The other databases' configuration:** each cluster's tuning depends on the others (its share of the data disk sizes `min_wal_size`/`max_wal_size`, the cluster count its CPU share), so `db create`, `db fork` and `db drop` plan `postgres-config-v1@<other>` for every other database and apply those whose inputs changed (`otherConfigs`, `applyOtherConfigs`) in the same run, without a confirm token: a reload, and a change that needs a restart stays pending with the `restart_pending` warning and its token. A database never configured is left to `eru apply`; a failure is the warning `other_config_failed`. A following `eru apply` has nothing to run.
- **Step numbering:** a request that runs several applies (db create, db fork, db drop, restore promote and undo) counts them up front (`engine.NewNumbering`, on the context with `engine.WithNumbering`), so the step events number across the whole run ([1/6] to [6/6]) instead of restarting per apply.
- **`eru db resize <db> --size … | --memory … [--defer-restart] [--confirm <db>/restart]`:** `--size` replaces the size and drops a `memory` override unless `--memory` is given too; `--memory` alone keeps the size. Refused before saving: `would_not_start`, counting the running cluster's shared memory as freed, and `memory_overcommitted` for a larger size past the budget unless `--allow-overcommit`. Warned: `shrink_below_use` when `memory.current` is above the new memory, plus the oversubscription warnings. It saves `cluster.toml`, runs `kernel-tuning-v1` (the huge-page sum) and the database's instances: the drop-in changes with `set-property` and reload-only settings with a reload; `shared_buffers`, `max_connections` and `huge_pages` restart that cluster only with the token. Without it, or with `--defer-restart`, the restart stays pending (`next`: `eru apply --db <db> --confirm <db>/restart`); Phase 4's maintenance window will run deferred restarts. A dry run returns the plan for the resized file.

## 13. Backups and restores

### 13.1 pgBackRest configuration

One repository and one cipher for the setup, a stanza per database. `/etc/pgbackrest/pgbackrest.conf` (postgres:postgres 0600, `render.PgBackRestConf`):

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
repo1-retention-full-type=time
repo1-bundle=y
repo1-block=y
{{ if .LocalRepo }}repo2-path=/var/lib/pgbackrest
repo2-retention-full=1
{{ end }}compress-type=zst
process-max={{ .ProcessMax }}
start-fast=y
archive-async=y
spool-path=/var/spool/pgbackrest
log-level-console=info
log-level-file=detail
delta=y

[app-3f9a1c]                          # one section per [[clusters]] entry
pg1-path=/var/lib/postgresql/18/app
pg1-port=5433
pg1-socket-path=/var/run/postgresql
repo1-retention-full=28               # the database's retention_full weeks × 7
archive-push-queue-max=2048MiB       # 4 GiB split over the stanzas, at least 512 MiB each
```

`40-eru-backup.conf`, in every cluster from creation:

```
archive_mode = on
archive_command = 'pgbackrest --stanza={{ .Stanza }} archive-push %p'   # with backups on; '/bin/true' without
archive_timeout = 60s
```

`archive_mode = on` from the start means turning backups on or off is a reload: without a bucket every WAL segment is discarded instead of piling up. `max_wal_senders` is already in the tuning file. `repo1-s3-endpoint` is the endpoint's host; a port in the endpoint URL becomes `repo1-s3-port`. `process-max` is `max(1, vCPU/2)`. `archive_timeout = 60s` bounds RPO on idle clusters. `repo1-retention-full` comes from `config.Cluster.RetentionFull(spec)`: the cluster's `backup.retention_full`, else `[backup.database]`'s. `archive-push-queue-max` is `tune.ArchiveSpoolGiB` (4 GiB) split over the clusters with a 512 MiB floor (`render.ArchiveQueueMaxMiB`); past eight clusters the floors add up to more than the 4 GiB that `disk_crowded` counts. Dropped stanzas get no section (§13.8).

**Provider presets**, used to derive defaults in the interview (`endpoint` and `region` are still editable):

| Provider | URI style | Region | Endpoint template |
| --- | --- | --- | --- |
| r2 | path | `auto` | `https://<account_id>.r2.cloudflarestorage.com` |
| s3 | host | asked | `s3.<region>.amazonaws.com` |
| b2 | host | asked | `s3.<region>.backblazeb2.com` |
| hetzner | host | asked (`fsn1`, `nbg1`, `hel1`) | `<region>.your-objectstorage.com` |
| minio | path | `us-east-1` | asked |
| custom | asked | asked | asked |

### 13.2 The per-database model

Each database is its own cluster with its own WAL stream, so every backup operation is per database: its stanza (`<db>-<6 hex>`, from `config.NewStanza` at create, immutable in `config set`), its restore window (from just after the end of its oldest backup set to its last archived WAL segment), its schedules, its verify and its drill. A database dropped and created again under the same name gets a new stanza, because the old one has a different system identifier and still holds the dropped database's window. The recovery kit doesn't grow: one bucket, one cipher, one age identity; the stanzas follow from `cluster.toml`, or from listing `<prefix>/pgbackrest/backup/` during DR. A database that must be kept longer than its window gets scheduled long-retention dumps (§13.8). There is no in-place restore of a whole server: rebuilding the master is Phase 5's `server init --restore`.

Code lives in `internal/backup`: `driver.go` (the `Driver` interface: `StanzaCreate`, `Check`, `Backup`, `Info`, `Verify`, `StanzaDelete`, `Materialise`, `ArchiveStatus`; `steps.BackupDriver(spec, …)` builds one for a cluster), `pgbackrest.go` (the implementation; every command runs as `postgres` through `engine.Shell.Exec`, so its output is redacted and no argument carries a secret; `--stanza` always, `--pg1-port` for the commands that connect to the cluster: `stanza-create`, `check`, `backup`; an empty stanza, used by `InfoAll`, lists every stanza in one `info` call), `materialise.go` (temporary clusters, the cut-over swap, promote and timelines, §13.5), `progress.go` (detail log lines → `progress` events, one per whole percent), `scratch.go` (the temporary-port reservation, `LoadDump`), `dumps.go` (age-encrypted dumps), `s3.go` (the `minio-go` client, presets and the bucket probe), `state.go` (`records.json`), `metrics.go` and `space.go`. WAL-G would be a second `Driver`; it is not built.

### 13.3 Turning backups on: `eru backup setup` and init

Backups can be added to an existing server as well as at init: every cluster already archives to `/bin/true`.

`eru backup setup` (flags `--provider --endpoint --region --bucket --prefix --uri-style --key-id --secret-stdin|--secret-file --full-schedule --diff-schedule --incr-schedule --retention-full`; in a terminal a `huh` form asks the same, with the retention phrased as a restore window "about N weeks"):

1. Reads `cluster.toml` and the recovery kit in `credentials.json`. Moving backups to another endpoint, bucket or prefix once any stanza has backups is refused (`precondition_failed`, `reason: repo_in_use`); the node refuses it again in `config.set`.
2. Probes the bucket **from the operator** with `minio-go`: `BucketExists`, then put, get and delete `<prefix>/.eru-probe`, then lists `<prefix>/pgbackrest/backup/` and refuses a prefix holding another server's stanzas (`repo_in_use`; `stanza-create` catches a system-ID mismatch again). Errors name the cause: `backup_auth_failed`, `bucket_not_found`, `endpoint_unreachable` (all exit 9).
3. Keeps the age recovery identity `server init` made (`prepareKit`), generates `cipher_pass` (64 chars base62) when this bucket/prefix has none in the kit, and **saves the kit to `credentials.json` before sending anything**. A server whose backups are already on and whose context has no kit for that bucket is `input_required` (`reason: recovery_kit_missing`): eru never makes a new cipher for an existing repository; log in from a current bundle.
4. `config.set` with the dotted `backup.*` keys and `base_revision`, then `apply --step postgres-config-v1 --step pgbackrest-v1 --step timers-v1` with `rotate_backup_secrets: true`. Every cluster's `archive_command` switches with a **reload**; there is no restart and no confirm token. `pgbackrest-v1` creates, checks and fully backs up each stanza that has no full yet.
5. Calls `backup.list` to print each database's latest backup and window, warns `secrets_document_stale` with the hint `eru secrets export`, and in a terminal offers the secrets handoff right away.

Re-running setup with the same bucket rotates the access keys (the cipher pass and age identity are kept). `db create` and `db fork` with backups on run `pgbackrest-v1` and `timers-v1` for the new database (`firstBackup`); a failure is the warning `first_backup_failed`, not an error, because the database already works and its passwords are printed only on success. Its hint is `eru apply` for a failed script, and the step's own hint when it refused with `input_required` or `precondition_failed` (keys neither on the node nor in the operator's kit), where a bare `apply` would refuse the same way.

**`server init`** accepts the `--backup-*`, `--db-full-schedule` and `--db-diff-schedule` flags and the answers file's `[backup]` and `[backup.credentials]`. In a terminal the interview gets a *Backups* page ("Configure an S3-compatible bucket now" or "Skip for now"). When configured, init probes the bucket and generates the cipher before first contact, so bad keys fail before the server is touched. The host and database phases send `cluster.toml` **without** `[backup]`; a separate phase `backups_configured` runs after the certificate wait and before the handoff, through the same code as `backup setup`. With no databases yet it only writes the configuration. Skipped, the summary's *Backups* row and `next` say `eru backup setup`, and the `no_offsite_backup` warning ends init and comes back from every `apply` and dry run (`status` adds it in Phase 4).

**Secrets on the node.** The RPC secret names are `backup.s3_key_secret` and `backup.cipher_pass`; `backup.s3_key_id` and `backup.age_recipient` travel in the same map (the only per-request channel a step has) but are not secret. None of them enters an input hash. `pgbackrest-v1` writes `/etc/pgbackrest/pgbackrest.conf` (postgres 0600) and `/etc/eru/runtime/backup.env` (root 0600: `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `ERU_DUMP_AGE_RECIPIENT`) from the request when a value is missing on disk, or when the request sets `rotate_backup_secrets` (only `backup setup` does), and otherwise reuses what is on disk, so an operator with stale credentials never overwrites newer keys. A different cipher pass under `rotate_backup_secrets` is refused (`cipher_mismatch`). A value missing on disk and in the request is `input_required`. Plan diffs of `pgbackrest.conf` show the keys and cipher as `«redacted»`. The operator's top-up before `apply` and `db create` sends the kit when it has a bucket in it (`BackupCreds.HasBucket()`), so a node that lost its copy, or never got one, heals on the next of either; with `[backup]` configured and no kit in `credentials.json` it is `input_required` with the hint to `eru login` from a current bundle.

### 13.4 Backup commands, timers and detachable jobs

| Command / RPC | Implementation |
| --- | --- |
| `backup now [--db] [--type]` / `backup.now` | `backup.now` takes one database (`params.database`; a usage error without one, except in a dry run, which plans every database). Without `--db` the CLI lists the databases and calls it once per database, **one after another** (`eachDatabase`), so each job takes the heavy-I/O lock in turn; a failed or skipped database doesn't stop the others, and the command then fails with the first failure's code, `details.failed` and every outcome. Ctrl-C detaches from the running job and starts no more. Each call is job `backup-<type>` for that database: without `--type`, `incr`, or `full` when its stanza has none (one `InfoAll` decides). The job skips when pgBackRest's own lock is held. `pgbackrest --stanza=<s> --pg1-port=<p> --type=<t> --log-level-console=detail backup`; progress events from the detail lines; the set goes to `records.json` (`last_backups[<db>][<type>]`) and `eru.prom`. Result: `data.backups`, one entry per database |
| `backup list [--db]` / `backup.list` | Without `--db`: one row per database (stanza, status, window, complete sets, latest set, last verify, drill and dump), then a row per dropped stanza still recorded (dropped and expiry times, window). One `pgbackrest info` over every stanza (`InfoAll`) plus each live cluster's `pg_stat_archiver` for the window's end; an unreadable one is the warning `archive_status_unavailable` and that row's window ends at its newest set. `--db` names a live database (its sets `{label, type, start, stop, size, delta, repo_size, wal_start, wal_stop}`, `restore_window {from, to, archive_lag_seconds}`, last verify, last drill, open restore, dump policy and dumps listed by the node with `backup.env`) or a dropped one still in its window (`data.dropped`; its window ends at its newest set) |
| `backup verify [--db]` / `backup.verify` | Per stanza: `pgbackrest --stanza=<s> verify --process-max=4`, as job `backup-verify`, since it downloads every file and mostly waits on the bucket; restores use the same process-max. Without `--db` the CLI loops one database at a time, like `backup now`. The result goes to `records.json` (`last_verifies[<db>]`) and `eru.prom`; a failed verify is recorded, then returned as `external_command_failed` with the output tail. The weekly timer runs the same code |

**Timers.** `timers-v1` renders, with backups on, a template service per job, `eru-<job>@.service` (`ExecStart=/usr/local/bin/eru node run <job> --db %i`), and a timer per database and job: `eru-backup-full@<db>.timer`, `eru-backup-diff@<db>`, `eru-backup-incr@<db>`, `eru-backup-verify@<db>`, `eru-restore-drill@<db>`, `eru-db-dump@<db>` (§18). Schedules come from `[clusters.backup]`, defaulting to `[backup.database]` (dumps have no default).

**Staggering.** N fulls at Sunday 01:00 would fight for the disk and the uplink. `StaggerOffset(stanza, spread)` is an FNV hash of the stanza id modulo the spread in whole minutes; the spread (`StaggerSpread`) is the length of `[maintenance] window` (3 hours by default, also when the window doesn't parse). `Stagger` rewrites the `OnCalendar` time (`HH:MM[:SS]` or `*:MM[:SS]`) later by the offset, wrapping within what is left of the day or hour, so the date and weekday fields never move; a schedule without a fixed minute (`daily`, `*:0/15`, lists) keeps its time. A database's full, diff, verify, drill and dump share its offset, and `RandomizedDelaySec` stays on top. The drill also moves its day of the month (`StaggerDay`: a fixed day `DD` ≤ 28 moves later by a hash of the stanza, wrapping within days 1–28), so the default `*-*-01 05:00:00` becomes a day of its own per database. systemd has no offset for `OnCalendar`, so the time is rewritten rather than delayed. Offsets are hashed, not slotted: two databases can land a few minutes apart, and the second job is then skipped by the heavy-I/O lock, not queued (`Persistent=` doesn't retry it). Evenly spaced slots, or a wait on the lock for timer runs, would fix that if it bites.

**Heavy I/O.** Database jobs (`backup-full|diff|incr`, `backup-verify`, `restore-drill`, `db-dump`) need a database, from `--db` or the RPC's `params.database` (missing is a usage error, unknown `database_not_found`), and take that cluster's lock shared (timeout 0). All of them except incrementals first take the server's heavy-I/O lock (`engine.AcquireHeavyIO`, `/run/lock/eru-heavy-io.lock`, exclusive, timeout 0): busy records `skipped` with `lock_busy` and `lock: heavy_io`. Incrementals are small and frequent, and a skipped one is caught up by the next; pgBackRest's own lock still keeps two backups of one stanza apart.

**Detachable jobs.** `backup.now`, `backup.verify`, `restore.start`, `restore.drill`, `db.fork` and `dump.create` start their work as `systemd-run --unit=eru-job-<request_id> --collect … eru node run <job> --job-id <request_id>`. The RPC first writes `/var/lib/eru/jobs/<id>.json` (0600, root 0700 directory) with the job's params and secrets; the job reads it and deletes it before doing anything else. The node and cluster locks are taken inside the unit (shared, timeout 0; busy records `skipped`). The RPC follows the unit's journal (`journalctl -u eru-job-<id>.service -o cat -f`): the job writes its events as NDJSON to stdout, which the RPC re-emits; it ends when the job's record in `state.json` is final and returns the job's result. Ctrl-C or a dropped tunnel only ends the RPC: the operator prints the job ID and `eru job follow <id>` re-attaches (a finished job replays its log and result). `eru job list` shows the last 50 records (`state.json.jobs`) with a DB column (`JobRecord.DB`). Timer runs are recorded the same way with systemd's `INVOCATION_ID`, which `job follow` uses to read their journal. A unit that ends without recording a result is marked failed by the follower.

### 13.5 `eru restore <db>`: materialise, inspect, cut over

```
eru restore <db> --time <ts> | --latest | --set <label> | --dump <id> [--promote] --confirm <db>/restore
eru restore status [<db>]
eru restore promote <db> --confirm <db>/restore
eru restore undo <db> --confirm <db>/restore
eru restore abort <db>
eru restore cleanup [<db>] [--confirm <db>/cleanup]
```

**Materialise** (`backup.Materialised`, `materialise.go`) is the one primitive behind restores, forks and drills: build a new cluster at a point in time, registered as `18/<name>` on a temporary port.

- `NewMaterialised(data_root, name, source, sourcePort, …)` refuses an existing `/etc/postgresql/18/<name>` or `<data_root>/<name>` (`cluster_exists`) and reserves a port in 5480–5499 under `/run/eru-ports/ports.lock` (skipping ports in `pg_lsclusters`, live socket lock files and other reservations; `no_temporary_port` when all are taken). The reservation is a file in `/run/eru-ports/`, freed with the cluster.
- `Restore(ctx, driver, opts)` runs `pgbackrest restore --stanza=<source's> --pg1-path=<data_root>/<name>` with `Pause`: time → `--type=time --target=<ts> --target-action=pause`, set → `--type=immediate --target-action=pause`, latest → `--type=standby` (keeps following the archive until promoted); without `Pause` the same targets with `--target-action=promote`, latest `--type=default` (forks and drills). `Timeline` adds `--target-timeline=<n>` (the timeline `restore undo` recorded). It **never** passes `--archive-mode=off`: pgBackRest writes that into `postgresql.auto.conf`, which travels with the data directory when it is swapped in and would leave the live cluster not archiving. `BaseBackup(ctx)` copies the live source instead with `pg_basebackup -X stream -c fast` over its socket (`local replication postgres peer` in `pg_hba.conf`).
- **Registration** writes `/etc/postgresql/18/<name>/` itself (a pgBackRest restore of a Debian cluster has no `postgresql.conf` in the data directory): a fresh `postgresql.conf` (data directory, hba and ident files, pid file, the temporary port, socket directory, `cluster_name`, `include_dir`), `start.conf = manual`, the source's `pg_hba.conf`, `pg_ident.conf`, `pg_ctl.conf`, `environment` and every `conf.d/*.conf`, plus `conf.d/99-materialise.conf`: the port, `listen_addresses = ''`, `archive_mode = off`, `hot_standby = on`, `shared_buffers = 128MB`, small `effective_cache_size` and `maintenance_work_mem`, `huge_pages = off`. Copying the source's conf.d is what carries the **hot-standby floors** (`max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`, `max_locks_per_transaction` at least the source's). It's started and stopped with `pg_ctlcluster 18 <name> start|stop`, with no cgroup drop-in; its log is `/var/log/postgresql/postgresql-18-<name>.log`.
- **Readiness:** `pg_isready`, then for a paused target `pg_get_wal_replay_pause_state() = 'paused'` (or already out of recovery), for promote `NOT pg_is_in_recovery()`. A postmaster that dies (a target outside the window ends recovery with a FATAL) returns the log tail. `RecoveredTo` reads `pg_last_xact_replay_timestamp()`.
- `Drop()` / `DropTemporaryCluster` run `pg_dropcluster 18 <name> --stop`, or remove the directories when the cluster was never registered, and free the port, on a context of their own.

**The restore:**

1. **Preflight** (`restore.start`): backups configured; `<db>` in `cluster.toml` (a dropped database is never restored in place; its stanza is reached with `backup list --db` or a fork); no open restore of `<db>`; nothing in the way (`restoring_exists` with `details.cluster` when `18/<db>__restoring` or its data directory exists, saying so when it is a real database of that name); the target in this stanza's window (`outside_window`; when an older stanza of the database is still recorded as dropped, after a dump restore, the hint names it, its drop time and expiry), the set, or the dump exists; free space for **one** copy of the database (its size plus 1 GiB) on the data root's filesystem, keeping 2 GiB free (`insufficient_space`). `--time` takes RFC 3339 or relative (`-15m`); the operator echoes it in UTC and the server timezone before the confirm. A timeline recorded by `restore undo` for this stanza (`Records.TargetTimeline(db, stanza)`) goes into the restore record; the dry run shows it.
2. **Materialise** (job `restore`, holding the cluster lock shared): `NewMaterialised(data_root, <db>__restoring, <db>, …)` and `Restore` with `Pause: true` and the recorded timeline. A failure drops the copy at once, so a failed restore holds no disk; a failed restore never blocks the next one.
3. **Ready:** `records.json` `restores[<db>] = {id, target, kind, phase: ready, port, timeline, …}`, and `pgbouncer-v1` adds `<db>__restoring = host=/var/run/postgresql port=<copy> dbname=<db> auth_dbname=<db>__restoring__auth` and its `__auth` alias (max 5 connections) to the **session pooler only** (`steps.poolerDatabases(cfg, true)`). `eru sql --db <db>__restoring` (and `psql`, `console`) reach the paused, read-only copy as `eru_admin`. A failed reload is the warning `pooler_reload_failed`. `restore status` shows the target, a per-table comparison of `pg_class.reltuples` read from the live cluster and from the copy (`compareRows`), and what promote will do. A `--latest` copy keeps replaying the archive while it waits.
4. **Promote** (`restore.promote`, global lock shared and the database's cluster lock **exclusive**; or inside the job with `--promote`): `steps.PauseDatabase` (PAUSE on the transaction pooler, RESUME after), then `backup.Swap`: `systemctl stop postgresql@18-<db>`, stop the copy, `rename(<data_root>/<db>, <data_root>/<db>__pre_restore_<UTC yyyymmddThhmmss>)`, `rename(<data_root>/<db>__restoring, <data_root>/<db>)` (same filesystem), re-register the old data as the stopped cluster `<db>__pre_restore_<ts>` on the copy's temporary port (with the overrides file and `start.conf = manual`), and remove `/etc/postgresql/18/<db>__restoring`. `repointAutoConf` rewrites the copy's data path to the live one in the moved `postgresql.auto.conf` (pgBackRest's `restore_command` carries `--pg1-path`, and archive-get reads `pg_control` there). `backup.StartCluster` starts `postgresql@18-<db>` (its own configuration, cgroup and `archive_command`) and waits until it is paused at the target (open for latest and dumps); `backup.Promote` runs `SELECT pg_promote() WHERE pg_is_in_recovery()`, then `ALTER SYSTEM RESET` of `restore_command` and `recovery_target*` and a reload. The cluster opens on a new timeline and archives to the same stanza. Then `records.json` closes the restore, clears the database's `timelines` entry (a promote makes the newest timeline the right one again) and adds a `restore_leftovers` entry `{database, name, created_at, restore_id, port, stanza}`; `pgbouncer-v1` drops the `__restoring` entry; and `systemctl start --no-block eru-backup-full@<db>.service` starts a full backup. Clients of the transaction pooler wait through the pause; the downtime is a few seconds plus the replay to the target. Roles and passwords are unchanged. A failure after the cut-over is the warning `after_cutover_failed`, not an error.
5. **Undo** (`restore.undo`, same locks): read the restored cluster's timeline (`backup.Timeline`: the first 8 hex digits of `pg_walfile_name(pg_current_wal_lsn())`, in decimal), then the same `Swap` with the **newest** leftover as the source (the restored data becomes a new `<db>__pre_restore_<ts>` on the leftover's port), and `StartCluster` until open. It reads the timeline the old cluster resumed on and records `timelines[<db>] = {stanza, timeline, switched_at, abandoned}`, so later restores, forks and drills of that stanza pass `--target-timeline=<n>` instead of `latest`, which would follow the abandoned branch. Then a full backup. When the leftover's stanza differs from the database's current one (the undo of a dump restore), undo puts that stanza back in `cluster.toml`, removes its `dropped_stanzas` record and records the dump's stanza as dropped (normal expiry), then applies the database's steps (a reload of `archive_command`), `pgbackrest-v1` and `timers-v1`. Undo of an undo works the same way.
6. **Abort and cleanup:** `abort <db>` drops `<db>__restoring` (refused with `restore_running` while it is still materialising) and reloads the poolers without its entry; the live database was never touched. `cleanup [<db>]` lists the leftovers with whether the cluster exists and its data directory's size; with a database and `--confirm <db>/cleanup` it drops them (`pg_dropcluster 18 <name> --stop`, then the directories) and their records. Until then the leftover is a one-command undo. `db drop` drops a database's leftovers (and a recorded drill leftover) with it.

- While a restore of `<db>` is open, `db drop <db>`, `db fork <db>` and `db credentials <db> --rotate` refuse with `reason: restore_in_progress`. Other databases, `apply` and backups carry on.
- `db list` skips clusters a restore or drill owns (`Records.IsTemporary`: an open restore's `<db>__restoring`, a recorded leftover or a recorded drill copy), so a real database called `app__restoring` lists normally and a temporary cluster isn't reported as orphaned.

**`--dump <id>`** (points older than the window): `Materialised.Initdb` (initdb into `<data_root>/<db>__restoring` with checksums, `--auth-local=peer --auth-host=scram-sha-256` and `[postgres] locale`/`locale_provider`; initdb's own config files are removed from the data directory, then the same registration, started open), `CopyRoles` (every role of the live cluster from `pg_dumpall --roles-only`, so verifiers, memberships and per-role settings carry over and credentials don't change; `CREATE ROLE postgres;` is filtered out; the output goes to a buffer, never the log), the pooler auth functions, `CREATE DATABASE <db> OWNER <db>_owner` with the connect grants, `LoadDump` as `<db>_owner` (the operator sends the age identity as the job's secret `backup.age_identity`; pg_restore's own errors are the warning `restore_dump_errors`), then the grants again and `ANALYZE`. The copy is ready, open rather than paused. On promote (no `pg_promote()`: it is a primary) the database **gets a new stanza**: its system identifier is new, so it can't keep pushing to the old one, and `stanza-upgrade` is meant for major upgrades. `switchStanza` writes `NewStanza(<db>)` into `cluster.toml` (the only stanza change, made by eru, never by `config set`), the old stanza becomes a `dropped_stanzas` record with its normal expiry (it still holds the PITR history from before the dump), the leftover records the old stanza, and `applyStanza` runs the database's steps (until that reload, archive-push to the old stanza fails on the system identifier and WAL waits in `pg_wal`), `pgbackrest-v1` (section, `stanza-create`, `check`, first full) and `timers-v1`. A later `restore --time` before the dump gets the hint naming the older stanza rather than searching both.

### 13.6 `eru db fork <src> [--name <new>] [--at <time>]`

A new database from a physical copy of the source. `--name` defaults to `<src>_fork_<yyyymmddhhmm>` and must pass `CheckDatabaseName`. The operator generates the four role passwords and prints them once with the URLs, like `db create`. Not destructive, no confirm token.

1. **Preflight** (`db.fork`): the source in `cluster.toml`, the name free (no `18/<new>`), no open restore of the source, with `--at` backups on and the time in the window; space for one copy. Then, under the exclusive lock, `addDatabase`: the first half of `db create` (passwords present, the master, a free port, a new stanza, validation, `would_not_start`) saves the `[[clusters]]` entry without creating the cluster. The entry is the source's (`forkSpec`): its size, `memory`, limits, pool sizes, profile, `statement_timeout`, extensions and `[clusters.settings]`, with no step-down, as `db create --size` does; a fork that overcommits the server warns `memory_overcommitted` and goes ahead, and only what `db create` refuses is refused. Backup schedules are the defaults. The passwords go to the job's secrets. If the job can't start, or is skipped on a busy lock, the entry is removed again (`forgetDatabase`).
2. **Materialise** (job `db-fork`): `NewMaterialised(data_root, <new>, <src>, …)`; with `--at`, `Restore` with `Pause: false` (promotes at the target) and the timeline recorded for the source's stanza; without it, `BaseBackup` of the live source (no backups needed).
3. **Promote and rename:** `backup.Promote` (a no-op `pg_promote()` for a base backup, and the reset of pgBackRest's recovery settings), then `ALTER DATABASE <src> RENAME TO <new>`, `ALTER ROLE <src>_owner|_migrate|_app|_ro RENAME TO <new>_…` (ownership and default privileges follow the role OIDs), `ALTER ROLE <new>_migrate SET role = '<new>_owner'`, and `ALTER ROLE … PASSWORD '<verifier>'` for the three login roles. Any failure until here drops the copy and the `cluster.toml` entry.
4. **Adopt:** `Materialised.Adopt(port)` stops the temporary cluster, writes `/etc/postgresql/18/<new>/postgresql.conf` as the source's own (Debian's, from `pg_createcluster`) with `data_directory`, `hba_file`, `ident_file`, `external_pid_file`, `port` (the placed port) and `cluster_name` set for `<new>`, sets `start.conf = auto`, empties `conf.d` except the source's `90-overrides.conf` (copied at registration; the source's eru files and `99-materialise.conf` go) and frees the temporary port. Then, as `db create` does, `kernel-tuning-v1` and the new database's instances: `postgres-cluster-v1@<new>` finds the cluster with the right data directory and port, writes its drop-in and starts it; `postgres-config-v1@<new>` sees no managed files, renders its first configuration (archiving to the new stanza, its own tuning, the copied `[clusters.settings]`), keeps the source's `90-overrides.conf` (preloads set there included) and restarts it; `postgres-roles-v1@<new>` finds every role and re-runs grants and settings. A failure here keeps the cluster and hints `eru apply --db <new>`.
5. **Route and back up:** `pgbouncer-v1` (failure: `pooler_reload_failed`) and `firstBackup` (`stanza-create` of `<new>-<id>`, `check`, first full, `timers-v1`; failure: `first_backup_failed`). The fork's system identifier equals the source's; pgBackRest accepts that in a separate stanza (to verify on the VPS). The job's warnings come back with its result.

### 13.7 Restore drill

`eru restore drill [--db] [--time]` and each database's monthly timer (job `restore-drill`, one database per job; without `--db` the CLI runs one job per database, one after another): `NewMaterialised(data_root, <db>__drill, <db>, …)` and `Restore` with `Pause: false` (`--type=default` for latest) and the timeline `restore undo` recorded for the stanza. Checks: `restore` (with the recovered-to time; a failure copies the copy's log tail into the job log, since `pg_dropcluster` removes the log), `open <db>`, `amcheck <db>` (`pg_amcheck --install-missing --database=<db>` on the copy's port, 30-minute bound; amcheck is installed in the throwaway copy only) and `tables` (`compareRows` against live: the table count and the tables only one side has, at most ten names each; informational, always `ok`, since tables created or dropped after the target are expected). The copy is always dropped. Skipped with `no_backups` (the stanza has no backup) or `insufficient_space` (one copy of the database). `archive_mode = off` in `99-materialise.conf` is what guarantees a drill never pushes WAL.

`records.json` keeps `drilling[<db>] = <db>__drill` from the moment the copy's port is reserved until it is dropped; the next drill of the database drops a recorded leftover first (a `<db>__drill` the records don't name is refused with `cluster_exists`). The result `{id, database, started_at, finished_at, backup_label, target, recovered_to, checks[], result: pass|fail|skipped, reason}` goes to `records.json` `drills` (the last 12 **per database**), `eru.prom` and `backup list`. A failed drill also fails the job. Doctor integration arrives in Phase 4.

### 13.8 Per-database policies: long-retention dumps and dropped stanzas

The restore window is per database (`[clusters.backup].retention_full`). A database that must be kept longer gets scheduled dumps on top:

```toml
[[clusters]]
name = "app"
# …
[clusters.backup]
dump_schedule = "*-*-* 03:00:00"   # empty = no dumps (default; the table is left out of the file)
dump_retention = 90                  # dumps kept
```

- `config set clusters.app.backup.dump_schedule …` works through the dotted-key path. A schedule needs `[backup]` and a `dump_retention ≥ 1`.
- `timers-v1` renders `eru-db-dump@<db>.timer` per database with a schedule (jitter 10 min, staggered like its backups).
- Job `db-dump` streams `pg_dump -Fc` from the database's socket through `age` (X25519, to the recipient in `backup.env`) into `<prefix>/dumps/<db>/<UTC yyyymmddThhmmssZ>.dump.age` with `minio-go` (64 MiB parts; nothing touches the node's disk), then deletes the oldest scheduled dumps beyond `dump_retention`. The identity lives only in `credentials.json` and the recovery kit, so a compromised node can't read old dumps.
- `eru dump <db> --to bucket` takes one on demand (job `db-dump`); `--to local` (the default) streams `eru node dump <db>` (raw `pg_dump -Fc` on stdout) over SSH into `--out` (default `./<db>-<time>.dump`, 0600).
- `eru dump list [<db>]` and `eru dump get <db> <id> --out <file>` run on the operator only, with the recovery kit: list the bucket, download and decrypt.
- `eru restore <db> --dump <id>` restores from a dump (§13.5).

**`db drop <db>`:** a final dump (`<ts>-final`, never pruned) first when backups are configured, refusing to drop when it fails (`final_dump_failed`; `--no-final-dump` skips it with a `no_backup_before_drop` warning, which is also the warning without backups); then `pg_dropcluster 18 <db> --stop`, the drop-in directory and a systemd reload, the database's restore and drill leftovers, the `cluster.toml` entry, and `dropRouting`: `pgbouncer-v1` drops its pooler entries and `timers-v1` removes its timers as stale units. Dropping the **last** database runs `systemctl disable --now pgbouncer eru-pgbouncer-session` instead, because `pgbouncer-v1` skips without databases; the next `db create` starts both again. **The stanza is kept:** `dropped_stanzas` gets `{database, stanza, dropped_at, expires_at, final_dump}` with `expires_at` = the drop time plus the database's `retention_full` weeks, and the database's last backup and verify results are removed so its metrics stop. A failed record write is the warning `records_not_updated`, not a failure, because the cluster is already gone. An orphaned cluster (in `pg_lsclusters`, not in `cluster.toml`) can be dropped the same way; it has no stanza to keep.

**Expiry.** With backups on, `timers-v1` renders `eru-stanza-expiry.timer` (daily 06:00, after the default window, 30 min jitter) running `eru node run stanza-expiry` (`jobStanzaExpiry`, global lock only). For each expired record (`Records.Expired`) it skips a stanza whose database has an open restore; for a stanza the repository holds it runs `pgbackrest stop`, `stanza-delete --force --pg1-path=<old data dir>` and `start` (`PgBackRest.StanzaDelete`), then drops the record. A stanza not in the repository only loses its record; a failed delete keeps it for the next run and fails the job. Dropped stanzas get **no** `pgbackrest.conf` section: `stanza-delete` takes `--pg1-path` on the command line, and nothing expires inside a stanza that gets no new backups. `db drop` doesn't re-render `pgbackrest.conf`; the leftover section is harmless and goes with the next `apply`.

### 13.9 Records, state and locks

`state.json` keeps only the ephemeral parts: `jobs` (§13.4) and the lockdown flags. Everything durable that exists nowhere else lives in `/var/lib/eru/records.json` (`internal/backup/state.go`, `LoadRecords`, `UpdateRecords` under an flock on `records.json.lock`, written atomically 0600; a corrupt file is the precondition `records_corrupt` and is never reset):

| Key | Content |
| --- | --- |
| `restores` | Open restore per database: `{id, database, target, kind, phase, started_at, ready_at, error, warnings, promote, port, timeline}` |
| `restore_leftovers` | `{database, name, created_at, restore_id, port, stanza}`: stopped `<db>__pre_restore_<ts>` clusters, with the port they're registered on and the stanza their data belongs to |
| `timelines` | Per database: `{stanza, timeline, switched_at, abandoned}`, the branch `restore undo` went back to; cleared by a promote and by `db drop` |
| `dropped_stanzas` | `{database, stanza, dropped_at, expires_at, final_dump}` |
| `drills` | The last 12 drill results per database |
| `drilling` | `<db>` → `<db>__drill` while a drill's copy exists |
| `last_backups` | Per database, then per type: `{label, finished_at, size}` |
| `last_verifies` | Per database |
| `last_dump` | Per database |

The old `last_backup` and `last_verify` keys are dropped on the next write (no migration). Phase 5's bundle carries `records.json` off the master.

Locks (§9.5): backup, verify, drill, dump, restore and fork jobs take the global lock and their cluster's lock **shared**, so a long restore or drill never blocks `apply`; full, diff, verify, drill and dump also take the heavy-I/O lock. `restore.promote|undo|abort|cleanup` take the database's cluster lock exclusive briefly. pgBackRest's own lock keeps two backups of one stanza apart (the job records `skipped`).

### 13.10 VPS backups (restic): removed

The master backup (§13.11) replaced the restic VPS backup in Phase 5. `vps backup|restore`, `--vps-backup-schedule` and `[backup.vps]` are gone; `ParseCluster` drops a `[backup.vps]` table from a file written before Phase 5 (the one exception to strict decoding, to remove after the first alpha). restic is not a dependency.

### 13.11 DR: the master backup (Phase 5)

The decisions and their reasons are in [`PHASE-5-WORK-UNITS.md`](./PHASE-5-WORK-UNITS.md); this is the spec as built.

- **What** (`internal/masterbackup`): an uncompressed tar, `manifest.json` first (schema `eru-master-bundle/v1`: eru version, server, node, epoch, desired-state revision, created at, content hash, and per file path, owner, group, mode, size, sha256), then `/etc/eru/cluster.toml`, `node.toml`, `operators.toml`, `/var/lib/eru/records.json`, `/etc/eru/protect.json`, `/etc/wireguard/eru.key`, `/etc/ssh/ssh_host_*_key{,.pub}`, `/etc/eru/tls/server.{crt,key}` and `/etc/eru/acme/*`. Fixed mtimes and order, so the same files give the same bytes. Sealed with age X25519 to `/etc/eru/runtime/recovery.recipient` (the recovery kit's recipient; `master-backup-v1` writes it from `backup.age_recipient`, which the operator sends whenever its kit has the identity). Opening refuses anything but regular files the set allows, each once, matching the manifest.
- **Names:** `<UTC ts>-e<epoch>-r<12 hex of the revision>`; in the bucket `<prefix>/master/<name>.tar.age`, then the sidecar `<name>.json` (schema `eru-master-sidecar/v1`: the manifest's metadata, file paths and sizes, the ciphertext's size and sha256; no plaintext hashes). A bundle without a sidecar is incomplete.
- **When:** the writers of bundled files call `masterbackup.MarkDirty` (`state.FileStore.Save`, `writeBootstrapFiles`, `syncNodeHostname`, the certificate install, the ACME account, the protect store, and `backup.UpdateRecords` when dropped stanzas, timelines or awaiting recovery change), which touches `/var/lib/eru/master-backup/dirty`. `eru-master-backup.path` (`PathExists=`) starts `eru-master-backup.service` (`eru node run master-backup`): it waits for 5 s of quiet (at most 60 s), removes the flag, snapshots, and builds only when the content hash differs from `last.json`'s. It takes its own lock (`/run/lock/eru-master-backup.lock`), never the node lock. `eru-master-backup.timer` runs it daily at the window's start as the safety net. Local bundles: the newest `[master_backup] local_keep`.
- **Upload** (with `[backup]`): check the fence, write `epoch.json` (epoch 1, this instance) when the bucket has none, put the bundle then the sidecar (single PUTs), confirm once every 20 h that the newest sidecar is in the bucket (re-uploading when not), prune once a day: every complete bundle younger than `retention_days`, the newest of each ISO week younger than `retention_weeks`, always the newest three; incomplete uploads older than a day go.
- **The epoch** (`<prefix>/master/epoch.json`, schema `eru-master-epoch/v1`: server, epoch, instance id, written at and by, reason): `/var/lib/eru/instance` is this install's id. Only positive proof fences a node: a well-formed epoch of this server naming another instance at an epoch newer than the one the node last confirmed. A bucket that can't be read, an object that isn't a valid epoch, one of another server, or one not newer than the node's own is unknown, never fenced (and the master backup then refuses to upload beside it, `epoch_conflict`). The bucket probe of `server init` and `backup setup` refuses a prefix whose epoch names another server or another install of this one (`repo_in_use`): a fresh install would fence itself there; `server init --restore` takes it over instead. The check runs before every job that writes to or deletes from the bucket (backups, dumps, stanza expiry, forks, restores, recoveries, the master backup, the final dump of `db drop`), at boot (`eru-fence.service`, before both poolers, 30 s budget) and every five minutes (`eru-fence.timer`). Fencing writes `/etc/eru/fenced`, disables every `eru-*.timer` but the fence timer, `eru-master-backup.path` and both poolers; the dispatcher then refuses every method that locks exclusive or writes a run log (`node_fenced`), and every cluster's `archive_command = '/usr/local/bin/eru node archive-push --pgbackrest=/usr/bin/pgbackrest --stanza=<stanza> %p'` refuses (it stats the file, then `exec`s `pgbackrest archive-push`; a stat that fails for any reason but "no such file" refuses too, so PostgreSQL keeps the segment and retries; every refusal writes its reason to stderr, into the PostgreSQL log). `--pgbackrest` names the binary it `exec`s and is also what puts `pgbackrest` in the `archive_command`: pgBackRest's `check` and `backup` refuse one without it (error 068). Lifting a fence is a human decision, on the node over break-glass SSH: `sudo eru node unfence --confirm <server>/unfence` reads the bucket's epoch and refuses while it names another instance; `--reclaim` writes the next epoch naming this node (fencing the other, for an abandoned rebuild whose VPS is gone); then `eru apply` brings the timers and poolers back.
- **Commands:** `server backup now` (RPC `master-backup.now`, no node lock), `list` (the bucket through the kit, the node through `master-backup.list`), `verify` (download or `--bundle-file`, sidecar sha256, decrypt with the kit's identity, validate, compare the content hash with the node's), `export` (`master-backup.get`, written 0600, refusing an existing file; records `last_exported_at`).
- **Rebuild:** `eru server init --restore --host root@<new-ip> [--bundle <name|time> | --bundle-file <file|->] --confirm <server>/replace` runs from the context of the server (log in first with only the secrets document). Phases in `pending/<server>/progress.json` (`--resume`): fetch and open the bundle (newest complete one of the highest epoch; refused for another server, an operator the bundle lacks, or a newer eru); refuse while the old master answers over WireGuard (`old_master_alive`, `--old-master-unreachable`); write `epoch + 1` with a new instance id (read back: `epoch_conflict`); first contact and the binary; `cluster.bootstrap phase=restore` (token `<server>/replace`, the decrypted tar as the request secret `master.bundle`): the files with their owners and modes, `cluster.toml` with the new public address, `records.json` pruned of restore leftovers, open restores, drills and undos and every database marked `awaiting_recovery`, `protect.json`'s failures and lockout reset, the instance id and epoch, `systemctl try-reload-or-restart ssh`; then init's host phase, WireGuard with the context's keys and the restored host keys, the lockdown, the DNS step when the address changed, the database phase with the full top-up, a forced first bundle, the context's new `public_address`, a new secrets handoff when the address changed, and `cluster.confirm-secrets`. The result carries each phase's seconds (`timings_s`).
- **Awaiting recovery:** `records.json` `awaiting_recovery`. The step engine sees `cluster.toml` without those databases (`engine.ClusterView`), so nothing creates an empty cluster, a pooler entry or a timer for them; commands that need the cluster refuse (`precondition_failed`, reason `awaiting_recovery`); `db list` and `status` show the state; doctor skips their checks.
- **`db recover`** (RPC `db.recover`, job `recover`): refused unless the database awaits recovery, without a bucket (`no_offsite_backup`), when its stanza has no backup (`no_backup_in_stanza`), when its stanza got WAL after the epoch was written (`stanza_still_written`, checked again before promote), and `--time` on a protected database. The job waits up to 6 h for the heavy-I/O lock, builds the empty scaffold `18/<db>` with its own cluster and configuration steps and archiving off, materialises the stanza into `<db>__restoring` (the recorded timeline), clears `awaiting_recovery`, renders the timers, promotes on the Phase 3 path, drops the scaffold, runs the database's steps and `metrics-v1`. `--no-promote` stops at a ready copy for `restore promote`. `--all` (the CLI) runs one database after another, protected first, then smallest stanza first, then a full of each.
- **Context endpoint:** in `acme` mode a session whose tunnel fails asks the public resolvers for the master's hostname and, if every answer agrees on another address, stores it and retries once (`context_endpoint_updated`); `eru context set-address` sets it by hand.
- **Doctor and metrics:** `master.fenced`, `master.backup` (warn after 1 h of unbundled or unuploaded change, fail after 24 h), `master.verified` (warn when the bucket's newest bundle wasn't confirmed in 48 h), `master.offsite` (no bucket and the newest bundle never exported). `eru-master-backup.prom`: `eru_master_backup_last_success_timestamp_seconds`, `_last_failure_timestamp_seconds`, `_last_verified_timestamp_seconds`, `_size_bytes`, `_dirty`, `_local_bundles`, `eru_master_epoch`, `eru_master_fenced`, `eru_database_awaiting_recovery{db}`.

## 14. Roles, databases and PgBouncer

### 14.1 `20-eru-security.conf` and pg_hba

In every cluster's `conf.d`:

```
listen_addresses = ''           # socket only: the port names the socket
port = {{ .Port }}
ssl = on                        # kept for the replication listener of later phases
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

Network exposure is the poolers' (§14.3, §11 step 5); clusters have no TCP listener in Phase 3. Phases 6 and 10 add a `wg0` listener on the cluster's port for replication only.

`pg_hba.conf`, the same template in every cluster:

```
# managed by eru — do not edit; use cluster.toml
local   all             postgres                                peer
local   replication     postgres                                peer      # pg_basebackup into a temporary cluster
local   all             eru_monitor                             peer map=eru
local   all             eru_admin                               peer map=eru
local   postgres        pgbouncer_auth                          peer map=eru
local   all             all                                     scram-sha-256
```

`pg_ident.conf` (map `eru`): `<admin_user> → eru_admin`, `root → eru_admin`, `postgres_exporter → eru_monitor`, `postgres → eru_admin` (the session pooler logs in as `eru_admin` over the socket for `eru sql`, `psql` and `console`), `postgres → eru_monitor` (pgbackrest_exporter runs as `postgres`), `postgres → pgbouncer_auth`. PgBouncer runs as `postgres` and logs in as `pgbouncer_auth` over the socket to run `auth_query`. That login can't use SCRAM: PgBouncer only reuses SCRAM keys a client handed it, and the auth query runs before any client has, so a SCRAM verifier in `userlist.txt` fails with `wrong password type`. Peer auth needs no password on disk.

pg_hba stops at the first line whose type, database and user match. If the peer check then fails, the connection is rejected; it doesn't fall through to the next line. That's why peer lines name specific users and the final `local all all scram-sha-256` line serves every other role (both poolers connecting app roles and `eru_admin` over the socket).

### 14.2 Role and database SQL (idempotent)

In every cluster (`postgres-roles-v1@<db>`), in its database `postgres`. The verifiers come from the setup-wide service secrets, so every cluster gets the same `eru_admin`, `eru_monitor` and `pgbouncer_auth` passwords:

```sql
-- admin
CREATE ROLE eru_admin LOGIN CREATEDB CREATEROLE PASSWORD :'pw';            -- if not exists (DO block)
GRANT pg_monitor, pg_signal_backend, pg_maintain, pg_use_reserved_connections TO eru_admin;
-- monitor
CREATE ROLE eru_monitor LOGIN PASSWORD :'monitor_pw'; GRANT pg_monitor TO eru_monitor;  -- password used by PgBouncer stats and the exporters
-- pgbouncer auth
CREATE ROLE pgbouncer_auth LOGIN PASSWORD :'pw';
-- roles the transaction pooler refuses: members must use the session pooler (5432)
CREATE ROLE eru_direct_only NOLOGIN;
CREATE SCHEMA IF NOT EXISTS eru AUTHORIZATION postgres;
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
    AND NOT pg_has_role(usename, 'eru_direct_only', 'MEMBER')
$$;
-- the session pooler's: refuses only postgres and the pooler's own service roles; eru_admin may log in
CREATE OR REPLACE FUNCTION eru.pgbouncer_get_auth_session(p_usename text)
  RETURNS TABLE(usename name, passwd text) LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
  SELECT usename, passwd FROM pg_shadow
  WHERE usename = p_usename AND NOT usesuper
    AND usename NOT IN ('postgres','pgbouncer_auth','eru_monitor')
$$;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION eru.pgbouncer_get_auth_session(text) FROM PUBLIC;
GRANT USAGE ON SCHEMA eru TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth(text) TO pgbouncer_auth;
GRANT EXECUTE ON FUNCTION eru.pgbouncer_get_auth_session(text) TO pgbouncer_auth;
REVOKE CONNECT ON DATABASE postgres FROM PUBLIC;
GRANT CONNECT ON DATABASE postgres TO pgbouncer_auth, eru_admin, eru_monitor;
```

Then that cluster's one database `<d>`:

```sql
CREATE ROLE <d>_owner NOLOGIN;
CREATE ROLE <d>_migrate LOGIN PASSWORD :'pw' IN ROLE <d>_owner;
ALTER ROLE <d>_migrate SET role = '<d>_owner';              -- objects created by migrations are owned by <d>_owner
GRANT eru_direct_only TO <d>_migrate;                        -- the transaction pooler refuses it: advisory locks, session SET and temp tables break under transaction pooling
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
CREATE EXTENSION IF NOT EXISTS ... ;                         -- per [[clusters]].extensions, as postgres
```

- **`eru db drop <d>`** drops the whole cluster: the final dump, `pg_dropcluster 18 <d> --stop`, the drop-in, the leftovers, the `cluster.toml` entry, the pooler entries and the timers; the stanza is kept for its window (§13.8). The confirm token is `<d>`; `--yes` is never enough.
- **`eru db credentials <d> --rotate --role app`**: generate a password, `ALTER ROLE … PASSWORD`, and print it once with the URLs. Nothing is stored and the secrets document is unaffected. Without `--rotate` the command fails with `usage`: eru keeps no database passwords to print.

Role names are validated by regex and also quoted with `pgx.Identifier{}.Sanitize()` (`pg.Quote`). The `:'pw'` placeholders above are SCRAM verifiers computed in Go (§10.2), never plaintext. psql `-v` variables carry only validated identifiers and non-secret literals.

**Operator SQL** (`eru sql`, `eru psql`, `eru console`) connects to the session pooler on 5432 of the database's node over the tunnel (pgx with `transport.Tunnel.DialContext`, `ServerName` = the node's hostname, `verify-full` against system roots; `self-signed` mode uses `require` and warns `tls_unverified`), which routes by `--db` to that database's cluster. `--db` is required, and there is no default database; a missing `--db` is `input_required`. It logs in as `eru_admin`, read-only unless `--write`, which acts as `<db>_owner`. Each `eru sql` call runs in one transaction, `READ ONLY` unless `--write`. `--db <db>__restoring` reaches an open restore's paused copy.

### 14.3 The two poolers

Both are rendered from one template (`pgbouncer.ini.tmpl`) by `pgbouncer-v1`. Database names are unique in a setup, so both route by name to the right cluster:

| | Transaction pooler | Session pooler |
| --- | --- | --- |
| Unit | Debian's `pgbouncer.service` | `eru-pgbouncer-session.service` (`Type=notify`, `User=postgres`, `RuntimeDirectory=eru-pgbouncer-session`) |
| Config | `/etc/pgbouncer/pgbouncer.ini`, `pgbouncer_hba.conf` | `/etc/pgbouncer/session.ini`, `session_hba.conf` |
| Port, socket and admin console | 6432, `/var/run/postgresql` | 5432, `/run/eru-pgbouncer-session` (with its `pidfile`) |
| `pool_mode` | `transaction`, `max_prepared_statements = 200`, `reserve_pool_size = 5` | `session`, `application_name_add_host = 1` |
| Who | `<db>_app`, `<db>_ro` (and 443 through nginx) | Everyone else: `<db>_migrate`, `<db>_owner` via migrate, `eru_admin` for `sql`/`psql`/`console`/`tunnel` |
| `auth_query` | `eru.pgbouncer_get_auth` (refuses `eru_direct_only`) | `eru.pgbouncer_get_auth_session` |
| TLS | `client_tls_sslmode = prefer`; the hba allows non-TLS only from `127.0.0.1` (the nginx hop) | `client_tls_sslmode = require`; the hba wants `hostssl` on TCP, no loopback exception |
| `max_client_conn` | `[postgres] client_connections` | `[postgres] session_client_connections` |
| Per database | `max_db_connections = pool_size` | `max_db_connections = session_pool_size` |
| Startup parameters | ignores `extra_float_digits,search_path,options` | ignores `extra_float_digits`; tracks `IntervalStyle,search_path,default_transaction_read_only,role` |

```ini
[databases]
app       = host=/var/run/postgresql port=5433 auth_dbname=app__auth max_db_connections=27
app__auth = host=/var/run/postgresql port=5433 dbname=postgres pool_size=2
billing       = host=/var/run/postgresql port=5434 auth_dbname=billing__auth max_db_connections=37
billing__auth = host=/var/run/postgresql port=5434 dbname=postgres pool_size=2

[pgbouncer]
listen_addr = *
listen_port = 6432                    # 5432 for the session pooler
unix_socket_dir = /var/run/postgresql # /run/eru-pgbouncer-session
auth_type = hba
auth_hba_file = /etc/pgbouncer/pgbouncer_hba.conf
auth_file = /etc/pgbouncer/userlist.txt
auth_user = pgbouncer_auth
auth_dbname = app__auth               # the admin console's, for eru_admin
auth_query = SELECT usename, passwd FROM eru.pgbouncer_get_auth($1)
pool_mode = transaction
max_client_conn = {{ .ClientConnections }}
default_pool_size = {{ largest pool }}
client_tls_cert_file = /etc/eru/tls/server.crt
client_tls_key_file = /etc/eru/tls/server.key
client_tls_protocols = tlsv1.2,tlsv1.3
server_tls_sslmode = disable
admin_users = eru_admin
stats_users = eru_monitor
log_connections = 1
log_disconnections = 1
```

- The `*` wildcard is gone: a database that doesn't exist is refused by the pooler. Each `<db>__auth` alias points `auth_query` at that database's cluster (`auth_dbname`). The global `auth_dbname` names the first database's alias only for the admin console's `eru_admin`, which has no entry of its own; service passwords are the same in every cluster, so any alias works. **To verify on the VPS:** per-entry `auth_dbname` pointing at another alias; the fallback is the `eru` schema and functions in each application database.
- A ready restore adds `<db>__restoring` (with `dbname=<db>`) and its alias to the session pooler only (§13.5).
- Adding or removing a database reloads both (`pgbouncer.sh`), never restarts them. Dropping the last database disables both (§13.8).
- `pool_size + session_pool_size + 3 ≤ max_connections` per cluster (`pool_size = 0` means the rest; a `nano` transaction pool is 7). The 3 (`config.ReservedConnections`) are tuning's `reserved_connections = 3` (`superuser_reserved_connections = 0`), so both pools full leave exactly those, shared by `eru_admin`, `eru_monitor` (the exporter, `CONNECTION LIMIT 2`: postgres_exporter's `/probe` holds two connections at once) and superusers.
- `userlist.txt` holds only `pgbouncer_auth` and `eru_monitor`, each with its SCRAM verifier read back from `pg_authid`; both poolers share it.
- nginx on 443 is unchanged: ALPN `postgresql` → TLS terminated → transaction pooler → by database name.
- What the session pooler costs: one more process, one more hop for migrations, and `pg_stat_activity.client_addr` shows the pooler (`application_name_add_host` puts the client back in `application_name`). Session pooling is transparent for advisory locks, `SET`, temp tables, `LISTEN` and prepared statements.

## 15. TLS, metrics and nginx

### 15.1 Exporters

| Exporter | Listen | Runs as | DB access |
| --- | --- | --- | --- |
| node_exporter | 127.0.0.1:9100 | `node_exporter` | `--collector.textfile.directory=/var/lib/node_exporter/textfile`, `--collector.systemd` for the postgresql@, pgbouncer, eru-*, nginx and wg-quick units |
| postgres_exporter | 127.0.0.1:9187 | `postgres_exporter` (system user) | Multi-target, no `DATA_SOURCE_NAME`, no config file: nginx probes one target per cluster, `host=/var/run/postgresql port=<port> dbname=<db> user=eru_monitor sslmode=disable connect_timeout=5` (peer via the ident map, so it holds no password). Collectors on top of the defaults: `stat_statements` (limit 50, no query text), `stat_checkpointer`, `database_wraparound`, `long_running_transactions` |
| pgbouncer_exporter | 127.0.0.1:9127 | `pgbouncer_exporter` | `PGBOUNCER_EXPORTER_CONNECTION_STRING=postgres://eru_monitor:<pw>@/pgbouncer?host=/var/run/postgresql&port=6432` from `/etc/eru/runtime/pgbouncer_exporter.env` (root 0600; systemd reads it). `eru_monitor` is in PgBouncer `stats_users` and authenticates against `userlist.txt` |
| pgbouncer_exporter-session | 127.0.0.1:9128 | `pgbouncer_exporter` | The session pooler's console in `/run/eru-pgbouncer-session` on 5432, from `pgbouncer_exporter-session.env` |

No `pgbackrest_exporter`: it would hold the S3 keys and the cipher in a long-running process and poll the bucket; `eru.prom` carries the backup facts instead (§15.4). Each exporter is a pinned upstream release binary (`internal/versions`), downloaded by `metrics-v1` and checked against its sha256 before extraction (`release_verification_failed` on a mismatch), at `/usr/local/lib/eru/exporters/<name>-<version>/<name>`. The units run in `eru-exporters.slice` (`CPUWeight=20`, `MemoryMax=256M`; per unit 64M, 128M and 32M) with `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes` and more. `postgres-roles-v1` gives `eru_monitor` `pg_use_reserved_connections` (shared with `eru_admin`), `CONNECTION LIMIT 2` (a `/probe` holds two connections: the legacy exporter's and the probe collectors'), `statement_timeout = 10s` and `lock_timeout = 1s`, so a probe still connects with both pools full (see the pooler section for the arithmetic).

### 15.2 nginx

eru renders the whole `/etc/nginx/nginx.conf` (`internal/render/templates/nginx.conf.tmpl`). nginx comes from the distribution with `libnginx-mod-stream`.

- **Port 80** (`http`): serves `/.well-known/acme-challenge/` from `/var/lib/eru-acme` for HTTP-01 (outside `/var/lib/eru`, which is root 0700 and closed to nginx's workers), and 404 for everything else.
- **Port 443** (`stream`, `ssl_preread on`): routes by ALPN without terminating TLS on 443 itself.
    - ALPN `postgresql` (PostgreSQL 17+ clients with `sslnegotiation=direct`) from an allowed address goes to `127.0.0.1:6433`, a `stream` server that terminates TLS with `/etc/eru/tls/server.{crt,key}` (`ssl_alpn postgresql`) and forwards the plain protocol to the transaction pooler on `127.0.0.1:6432`, which routes by database name.
    - Allowed addresses come from a `geo` block: the WireGuard subnet always, plus `client_allowlist` when `expose_pgbouncer = true`, so 443 follows the same rule as 6432. Other PostgreSQL clients go to `127.0.0.1:6434`, a `stream` server with `ssl_reject_handshake on` (nginx 1.25.5+), which fails the handshake with a fatal `unrecognized_name` alert: libpq reports `SSL error: tlsv1 unrecognized name`, pgx `remote error: tls: unrecognized name`.
    - A connection whose first bytes aren't a TLS ClientHello (`$ssl_preread_protocol` empty), in practice a PostgreSQL SSLRequest from a client without `sslnegotiation=direct`, goes to `127.0.0.1:6435`, which answers `N` ("no TLS here") and closes. With `sslmode=require` or stricter libpq reports `server does not support SSL, but SSL was required`, pgx `server refused TLS connection`. nginx `stream` can't complete the SSLRequest exchange and then terminate TLS, so these clients need `sslnegotiation=direct` or port 6432.
    - Everything else (HTTP/1.1, h2, no ALPN) goes to the `http` server on `127.0.0.1:8443`, which terminates TLS, serves the authenticated `/metrics` endpoint and answers 404 otherwise.
    - The 443 server sends the PROXY protocol header to every internal upstream (8443, 6433, 6434, 6435 all `listen … proxy_protocol`), and 8443 takes the client's address from it (`set_real_ip_from 127.0.0.1; real_ip_header proxy_protocol`), so the metrics allowlist, `limit_req` and fail2ban see the real client. PgBouncer still sees 127.0.0.1 from 6433.
- **`/metrics`** (Phase 4, `metrics-v1` owns `/etc/nginx/eru-metrics.conf`, included in 8443 by the glob `eru-metrics*.conf`): an exact location per path under `[metrics] path_prefix`: `/node`, `/postgres/<db>` (proxied to postgres_exporter's `/probe` with the target set by `set $args`, so the client's query string can't choose a DSN), `/pgbouncer`, `/pgbouncer-session` and `/targets.json` (the `http_sd` list, `/etc/nginx/eru-metrics-targets.json`, with `__metrics_path__`, `__scheme__` and `eru_server`, `eru_node`, `eru_component`, `db`/`pooler` labels). Each has `auth_basic` with `/etc/nginx/eru-metrics.htpasswd` (`root:www-data 0640`; nginx's workers read it per request, so it can't live in `/etc/eru/runtime`), `limit_req` (10 r/s, burst 40) and the optional `[metrics] allowlist` (a set allowlist also admits 127.0.0.1 and ::1, so doctor's unauthenticated probe through 443 gets its 401; only the node itself can arrive as loopback, since the 443 hop writes the PROXY header from the real peer). fail2ban's `nginx-http-auth` jail (`/etc/fail2ban/jail.d/eru-metrics.conf`) bans repeated bad passwords. The operator generates the password and sends only its bcrypt hash (cost 8) as the request secret `metrics.htpasswd`; the node writes it when the file is missing, and `eru metrics rotate --confirm <server>/metrics` replaces it.
- The transaction pooler uses `auth_type = hba` with `/etc/pgbouncer/pgbouncer_hba.conf`: `local` and `hostssl` from anywhere use `scram-sha-256`; `hostnossl` is allowed only from `127.0.0.1/32` (the nginx hop); any other non-TLS connection is rejected. `client_tls_sslmode = prefer`, so the hba file, not the TLS mode, enforces TLS. SCRAM channel binding isn't available through 443, because PgBouncer sees a plain connection there; `channel_binding=require` clients must use 6432. `server_name` is the node's hostname.

### 15.3 Certificates (`eru node run certs-renew`)

eru issues and renews the certificate itself with `golang.org/x/crypto/acme`. Nothing else on the server speaks ACME.

- `eru-certs.timer` runs `eru node run certs-renew` hourly (randomized 10 min); `tls-v1` also starts it once right away. The job waits up to 20 minutes for the shared lock, so the run `tls-v1` starts during `cluster.bootstrap` goes ahead once the bootstrap releases the lock; if the lock is still busy it exits 0 with a skipped result. Any other lock failure is an error. `eru node run` refuses with `precondition_failed` (`reason: not_a_node`) when `/etc/eru/cluster.toml` is missing, which is the case on an operator machine (run it there as `eru ssh --command 'sudo eru node run certs-renew'`), and with `root_required` when not root.
- The certificate is for the node's one hostname, `<node>.<domain>` (`steps.NodeHostname(cfg)`: this node in `[[nodes]]`, falling back to the master), passed to scripts as `TLS_HOSTNAME`. It does nothing unless `/etc/eru/tls/server.crt` is missing, isn't for that hostname, is the self-signed placeholder in `acme` mode (or not self-signed in `self-signed` mode), or expires within 30 days. When it does nothing it logs that the certificate for the hostname is current until `not_after`; the result always carries it.
- **acme mode:** first a self-check: write a random token into the webroot and fetch `http://<hostname>/.well-known/acme-challenge/<token>`. If another host answers, the job stops with `changed: false, pending: …` (exit 0) so a wrong record doesn't use up Let's Encrypt's failed-validation limit. A connection error is inconclusive and the job carries on. Then: account key `/etc/eru/acme/account.key` (P-256, 0600, created once; `acme_email`, when set, is the account contact), order, HTTP-01 through the webroot, a fresh P-256 leaf key and CSR, and the full chain. `tls.acme_ca` overrides the directory URL (for example Let's Encrypt staging). ACME errors fail the unit with `external_command_failed`; a rate limit says when to retry.
- **self-signed mode:** a self-signed P-256 leaf for the hostname, valid 397 days. Clients use `sslmode=require` (the document's URLs switch accordingly, and doctor warns).
- A new certificate is written as `server.crt` (full chain, 0644) and `server.key` (`root:ssl-cert` 0640), then both poolers (`systemctl reload pgbouncer eru-pgbouncer-session`), nginx and then each cluster in turn (`pg_ctlcluster 18 <db> reload`) are reloaded. Creating, forking or dropping a database never touches the certificate or DNS.
- `tls.status` reports `pending` while the placeholder is served, `acme` with `not_after` once the issued certificate is in place, and `self-signed` in that mode.

### 15.4 eru textfile metrics (`eru.prom`)

```
eru_info{version,server,node} 1
eru_backup_last_success_timestamp_seconds{db,type="full|diff|incr"}
eru_backup_verify_last_success_timestamp_seconds{db}
eru_restore_drill_last_run_timestamp_seconds{db}
eru_restore_drill_last_result{db,result="pass|fail|skipped"} 1
eru_tls_cert_expiry_timestamp_seconds
eru_doctor_check{check,db,status} 1
eru_restore_in_progress 0|1
eru_job_last_skipped_timestamp_seconds{job,db}
```

From Phase 4 the textfiles are split by owner (`internal/promfile`, `/var/lib/node_exporter/textfile`), so no writer needs another's data: `eru.prom` (above; `eru_info` labels `server`, not `cluster`; plus `eru_backup_last_size_bytes`, `eru_backup_last_repo_size_bytes`, `eru_backup_last_duration_seconds`, `eru_backup_last_failure_timestamp_seconds{db,type}`, `eru_backup_window_start_timestamp_seconds{db}`, `eru_backup_sets{db}`, `eru_backup_repo_size_bytes{db}`, from one `pgbackrest info --stanza` at the end of each backup and verify, in place of `pgbackrest_exporter`), `eru-host.prom` (`metrics-refresh`, every minute: per database `eru_cluster_memory_current_bytes`, `eru_cluster_memory_high_bytes`, `eru_cluster_memory_max_bytes`, `eru_cluster_memory_events_total{event}`, `eru_cluster_pressure_seconds_total{resource,kind}`, `eru_cluster_cpu_seconds_total`, `eru_cluster_io_bytes_total{direction}`, `eru_cluster_running`, `eru_cluster_restart_pending`; `eru_memory_budget_bytes`, `eru_memory_allocated_bytes`, `eru_memory_reserve_bytes`, `eru_commit_limit_bytes`, `eru_committed_bytes`, `eru_tls_cert_expiry_timestamp_seconds`, `eru_deferred_restart_requested_timestamp_seconds{db}`), `eru-doctor.prom` (`eru_doctor_check{check,db,status}`, `eru_doctor_last_run_timestamp_seconds`, `eru_doctor_failing_checks`) and `eru-maint.prom` (`eru_wraparound_age{db}`, `eru_mxid_age{db}`, `eru_bloat_estimated_bytes{db,kind}`, `eru_maint_analyzed_tables{db}`, `eru_maint_last_run_timestamp_seconds{job}`). `records.json` gains `last_backup_failures` and `repo`; `db drop` removes the database's results, so its lines stop.

---

## 16. Maintenance (`internal/maint`)

Every maintenance session sets `SET lock_timeout = '5s'; SET statement_timeout = 0;` (vacuum and reindex can be long); reports run read-only with `statement_timeout = '60s'`. Everything runs on the node over the database's socket as `eru_admin` (psql as root, which `pg_ident` maps to `eru_admin` only): `pg_maintain` covers vacuum, analyze and reindex, `pg_monitor` the reports, `pg_signal_backend` kill (never a superuser's backend). RPCs: `maint.report`, `maint.run` (vacuum, analyze, reindex as the detachable job `maint`; a whole-database vacuum and every reindex take the heavy-I/O lock; repack refuses until Phase 7), `maint.kill` (token `<db>/kill`). Without `--db` the CLI runs one job per database, one after another. The bloat queries are vendored from ioguix/pgsql-bloat-estimation at 8fde3c9 (BSD-2-Clause, `internal/maint/sql`, notice in `eru version --licenses`).

| Command | Flags | Implementation |
| --- | --- | --- |
| `maint vacuum` | `--db` (default all), `--table`, `--analyze`, `--freeze`, `--parallel N` | `VACUUM (VERBOSE false, ANALYZE x, FREEZE y, PARALLEL n) <table>`, or `vacuumdb --all --jobs=min(4,C)` via node RPC when there's no table |
| `maint analyze` | `--db`, `--table` | `ANALYZE` / `vacuumdb --analyze-only` |
| `maint reindex` | `--db`, `--table`/`--index`, `--concurrently` (required while single-node) | `REINDEX (CONCURRENTLY) …`; on failure, reports leftover `_ccnew` invalid indexes and offers `--cleanup-invalid` |
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
| pg_cron | `postgresql-18-cron` | `pg_cron` | `cron.database_name` = the cluster's database; PerCluster |
| pgaudit | `postgresql-18-pgaudit` | `pgaudit` | `pgaudit.log='ddl,role'` default |
| pg_repack | `postgresql-18-repack` | | Required for `maint repack` |
| pg_partman | `postgresql-18-partman` | `pg_partman_bgw` (optional) | |
| hypopg | `postgresql-18-hypopg` | | |
| timescaledb | `timescaledb-2-postgresql-18` (Timescale apt repo) | `timescaledb` | Timescale licence; adds a repo; mark `Note` |

At implementation time, verify each package exists for PG18 on every supported OS/arch. Drop any that don't and remove them from this list.

**`ext add <name> [--db <d>]`:**

1. Install the package (adding the repo if needed).
2. If there's a preload, re-render that database's `30-eru-extensions.conf`. That restarts only its cluster, so `--confirm <db>/restart` is needed; without it, stop after the package install and report `restart_required`. (The command's metadata still says `{server}/restart` until Phase 7 builds it.)
3. Run `CREATE EXTENSION IF NOT EXISTS <ext>` in `--db`.
4. Persist to the database's `[[clusters]].extensions`.

**`ext remove`**: `DROP EXTENSION` (no CASCADE unless `--cascade`; confirm `<db>`), then remove it from preload if no database uses it any more.

**`ext list`**: catalog × installed (`pg_available_extensions` and per-db `pg_extension`) → `{name, available, installed_in[], version, default_version, update_available}`.

---

## 18. Timers

Server-wide jobs use a pair of units named `eru-<job>.service` and `eru-<job>.timer`. Database jobs use a **template service** per job and a timer per database: `eru-<job>@.service` runs `eru node run <job> --db %i`, and `eru-<job>@<db>.timer` starts its instance.

```
[Unit]
Description=eru {{ .Job }}
After=postgresql.service network-online.target
Wants=network-online.target
[Service]
Type=oneshot
ExecStart=/usr/local/bin/eru node run {{ .Job }}{{ if .Arg }} {{ .Arg }}{{ end }}     # Arg is --db %i for templates
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

| Job | Unit | Schedule source | Jitter |
| --- | --- | --- | --- |
| backup-full | `eru-backup-full@<db>` | `clusters.<db>.backup.full_schedule`, else `backup.database.full_schedule`, staggered | 5m |
| backup-diff | `eru-backup-diff@<db>` | `…diff_schedule`, staggered | 5m |
| backup-incr | `eru-backup-incr@<db>` | `…incr_schedule` (omitted if empty), staggered | 2m |
| backup-verify | `eru-backup-verify@<db>` | `…verify_schedule`, staggered | 10m |
| restore-drill | `eru-restore-drill@<db>` | `…drill_schedule`, staggered in the day and over days 1–28 | 30m |
| db-dump | `eru-db-dump@<db>` | `clusters.<db>.backup.dump_schedule` (omitted if empty), staggered | 10m |
| stanza-expiry | `eru-stanza-expiry` | daily 06:00 (with backups on) | 30m |
| maint-daily | `eru-maint-daily` | daily at window start | 10m |
| maint-weekly | `eru-maint-weekly` | Saturday at window start | 10m |
| certs-renew | `eru-certs` | hourly | 10m |
| metrics-refresh | `eru-metrics-refresh` | every minute (`minutely`); not recorded in `jobs`, skips at once on a busy lock | 0 |
| doctor | `eru-doctor` | hourly; not recorded in `jobs` | 10m |

`timers-v1` arrives in Phase 3 and renders `eru-certs.timer` (owned by `tls-v1` until then; `tls-v1` keeps only `eru-certs.service`), the database timers (§13.4 for staggering) and the stanza expiry. Phase 4 adds `maint-daily` and `maint-weekly` (at the start of `[maintenance] window`), `metrics-refresh` and `doctor`, server-wide and with or without backups. `maint-daily` reads each database's wraparound age, analyzes tables with stale statistics until the window ends, then performs the restarts `db resize --defer-restart` left in `state.json`'s `deferred_restarts` (each under its cluster's lock exclusive, through `apply --db <db>` with `<db>/restart`; a busy cluster waits for the next night). `maint-weekly` estimates bloat. Both write `/var/lib/eru/maint.json` and `eru-maint.prom`. Phase 5 adds `eru-master-backup.{service,path,timer}` and, with a bucket, `eru-fence.{service,timer}` (§13.11). A dropped database's timers are stale units and go with the next `timers-v1` run; `db create` runs it, so a new database gets its timers.

`node run <job>` takes the global lock shared with timeout 0 (`certs-renew` waits up to 20 minutes), then for database jobs the heavy-I/O lock (all but incrementals) and the database's cluster lock (§9.5). If a lock is busy it logs, exits 0, records the job as `skipped` in `state.json.jobs` and in `eru_job_last_skipped_timestamp_seconds{job,db}`. Busy isn't a failure, but repeated skips show up in doctor. Every run is recorded as a job (§13.4) with its `db`.

---

## 19. Upgrades

### 19.1 Releases and verification

- Copy the deploycrate release model: a manifest at `<ReleaseBaseURL>/manifest.json` with a sigstore bundle `manifest.json.sigstore.json`, signed keyless by GitHub Actions OIDC (`publish-stable.yml` on tags `vX.Y.Z`, `publish-edge.yml` on pushes to `master`). Stable channel files (`install.sh`, `manifest.json`, `manifest.json.sigstore.json`) live at the domain root and edge files under `/edge/`. Every version also has an immutable tree at `/releases/<version>/` with its own `manifest.json`; the channel manifest is a byte-for-byte copy of it.
- Manifest schema:

  ```json
  {"schemaVersion":1,"channel":"stable","version":"1.0.0","releaseBaseUrl":"…/releases/1.0.0",
   "artifacts":{"linux/amd64":{"url":"…","sha256":"…"},"linux/arm64":{},"darwin/amd64":{},"darwin/arm64":{}}}
  ```

- In-process verification uses `sigstore-go` against the Sigstore public-good trust root. The certificate identity regexp and OIDC issuer are exactly as in deploycrate `install.sh`, with the repository `mbvlabs/eru`.
- `ReleaseBaseURL` is a build-time constant. The production value is `https://eru.mbvlabs.com`.

### 19.2 Commands

`eru upgrade`:

1. Fetch and verify the manifest, then download, verify (sha256) and replace the local binary atomically, keeping the old one as `<binary>.prev` (hard link). If the binary lives in a root-owned dir, escalate via §20.3. Refuse binaries under `/opt/homebrew/Cellar`, `/usr/local/Cellar` or `/nix/store` (`precondition_failed`, `details.reason = "managed_by_package_manager"`).
2. Unless `--skip-nodes`, re-exec the new binary with `node upgrade`.

`eru node upgrade [--version v]`:

1. Make sure the latest master bundle is current (Phase 5).
2. Push and install via §7.5.
3. Run `eru node selfcheck`. If it fails, roll back automatically and return `step_failed`.
4. Run `apply`, which re-runs only steps whose hash or ID changed. If any step needs a PG restart, report `restart_pending` and don't restart without `--confirm`.

`--rollback` swaps `eru.prev` back (confirm `<server>/rollback`). Only one previous version is kept.

`eru pg update`:

1. An incremental backup of every database (`backup now --type incr`, one at a time).
2. `apt-mark unhold`, `apt-get install --only-upgrade postgresql-18 postgresql-client-18 postgresql-contrib pgbouncer pgbackrest`, then `apt-mark hold`.
3. Restart the clusters one at a time (each paused on the transaction pooler while it restarts) at the next window start (scheduled via `systemd-run --on-calendar`), or now with `--now --confirm <server>/restart`.
4. Report versions before and after.

---

## 20. Local preflight and privilege escalation (`internal/localenv`)

### 20.1 Checks per command class

| Command class | Requirements |
| --- | --- |
| Any remote command | Context exists, key files are 0600, UDP egress (no check, just a hint on `transport_failed`) |
| `server init` (remote) | Target reachable on 22/tcp. An SSH credential: `--identity`, agent keys, or `--password-stdin` |
| `psql` | `psql` binary ≥ 16 on PATH |
| `console` | `usql` on PATH, or eru's pinned download (checksum-verified) |
| `wg export --install` | `wireguard-tools` (`wg`, `wg-quick`) and root |
| `server init --local` | Root (or escalation), a supported OS, no existing `/etc/eru/node.toml` |

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

- `server`, `node`, `role`, `eru_version`, `pg_version`;
- the server sheet (§12.4) and its warnings, plus `no_offsite_backup` without a bucket;
- `pgbouncer {cl_active, cl_waiting, sv_active, sv_idle}` for both poolers;
- `disk {data_free_pct}`, `tls {not_after}`;
- per database (`--db` narrows to one): `state`, `uptime`, `size`, `memory.current` and limits, `connections {used, max, by_state}`, `cache_hit_ratio`, `tps` (delta over 1 s), `db_size`, `wal_bytes`, `backup {last_full, last_diff, last_incr, pitr_window, archive_failed_count, last_archived_age_s}`, `drill {last_result, at}`, `restart_pending`, `restore_in_progress`.

**Doctor checks** (Phase 4, `internal/doctor`: the node gathers facts, every check is a pure function of them; `pg.*` and `backup.*` run per database; each returns `{id, db, status: pass|warn|fail|skip, detail, fix, category, security}`; `fix` is an eru command when one exists; `skip` is a check that does not apply). `eru doctor [--only security|backups|host|postgres|tls|metrics] [--db]` exits 5 with `doctor_failed` on any `fail`. The node writes `/var/lib/eru/doctor.json` and `eru-doctor.prom` on every full run (hourly `eru-doctor.timer`). Step 18 is not a catalogue step: a full `apply` ends with the checks (each `fail` is the warning `doctor_check_failed`) and `server init`'s database phase fails with `doctor_failed` (checks that cannot pass yet, `secrets.confirmed`, `backup.*` and `jobs.*`, are left out). `--skip-doctor` skips either.

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
| `tls.dns` | warn: A/AAAA of the node's hostname ≠ node public address |
| `tls.mode` | warn: self-signed |
| `backup.archiving` | fail: `pg_stat_archiver.failed_count` increased in the last hour, or last archived > 10 min with WAL activity |
| `backup.full_age` | warn > schedule interval + 1 day, fail > 2× |
| `backup.verify` | warn: last verify failed or older than 14 days |
| `backup.drill` | warn: no pass in 40 days; fail: last result fail |
| `backup.offsite` | warn: no bucket (`no_offsite_backup`) |
| `pg.memory_budget` | warn: `memory_overcommitted`, `cpu_crowded`, `disk_crowded` |
| `pg.memory_pressure` | warn: `memory.pressure` some avg300 above 10%, or `memory.events high` rising since the last run |
| `pg.would_not_start` | fail: a stopped cluster that `would_not_start` refuses |
| `pg.listen_socket_only` | fail: `listen_addresses` not empty |
| `pooler.running`, `nginx.running` | fail: a pooler or nginx inactive |
| `jobs.skipped` / `jobs.failed` | warn: a timer job skipped three runs in a row / its last run failed |
| `net.allowlist_open` | warn: `0.0.0.0/0` or `::/0` in an allowlist |
| `ctx.file_modes`, `metrics.auth` (operator) | warn: a context file readable by others; fail: the node refuses the context's `/metrics` credential |
| `secrets.confirmed` | fail: `secrets_confirmed_at` empty |
| `eru.version_skew` | warn: operator ≠ node (operator side) |
| `metrics.endpoint` | warn: any exporter not responding |
| `metrics.pg_up` | warn: a running database's postgres_exporter probe gives `pg_up 0` (the detail carries the exporter's last journal errors) |

---

## 22. Testing

There's no automated test suite and no `_test.go` files. CI runs `just lint` and builds. Each phase ends with the manual test listed for it in [`MASTER.md`](./MASTER.md) §7, run on a real VPS. See MASTER §8.

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
- When root on a supported Linux server with a TTY, asks "Initialise this server now (eru server init --local)? [y/N]" on `/dev/tty` (stdin is the piped script) and on yes execs `eru server init --local` with the terminal as stdin.
- `ERU_INSTALL_ONLY=1` skips the next-step text and the init question.

### 23.3 Pinned versions (`internal/versions`)

```go
const (
    PostgresMajor = 18
    NginxMinVersion = "1.26"              // distribution nginx with stream ssl_preread + ssl_alpn
    PgBackRestMinVersion = "2.59.3"       // fixes an unchecked error in encrypted repos; PGDG, held
    NodeExporterVersion = "1.12.1"        // Phase 4: upstream release binaries, sha256 per arch in ExporterSHA256
    PostgresExporterVersion = "0.20.1"    // 0.20 moved stat_activity, stat_archiver, settings into /probe's collectors
    PgbouncerExporterVersion = "0.12.1"
    // no pgbackrest_exporter: eru.prom carries the backup facts (PHASE-4-WORK-UNITS D2)
    CosignVersion = "3.1.3"               // install.sh and mise.toml
)
var Checksums = map[string]map[string]string{ /* artifact -> arch -> sha256 */ }
```

PGDG packages (postgresql-18, pgbouncer, pgbackrest, extensions) aren't version-pinned (pgBackRest has the floor above). They're held (`apt-mark hold`) and updated only via `eru pg update`.

---

## 24. Build order

Build the phases in [`MASTER.md`](./MASTER.md) §7 in order. That section is also where status is tracked; this document carries none.

---

## 25. The agent guide (`eru guide agents` / `eru skill install`)

The embedded markdown MUST cover:

1. what eru is (one paragraph);
2. always using `--output agent` or `--json` and reading `ok`, `error.code` and `next`;
3. discovering commands with `eru commands --json` and reading `meta` (`mutating`, `destructive`, `confirm_token`, `needs_restart`);
4. never inventing confirm tokens: obtain them from `error.details.confirm_token` and ask the human before passing one for a destructive command;
5. the exit code table;
6. the model (each database is its own cluster with a size; one hostname per node, the database in the URL's path; restarts per database with `<db>/restart`) and common workflows: status check, backup now plus verify, PITR restore (paused → inspect → promote/abort/undo), a fork at a time when only some rows are needed, add or resize a database, add an extension, run a maintenance report, upgrade, and the warning codes (`memory_overcommitted`, `size_stepped_down`, `would_not_start`, `no_offsite_backup`, `first_backup_failed`, `pooler_reload_failed`, `after_cutover_failed`);
7. what an agent must never do:
   - print the secrets document or passphrase;
   - run `restore cleanup` or `db drop` without explicit human approval;
   - pass `--sudo` or `--install-missing` without approval.

`eru skill install` writes this as a skill into the requested harness directories, using andurel's `skill.go` harness list and layout.
