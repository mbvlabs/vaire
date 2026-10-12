# eru: implementation spec

> Audience: the agent (or engineer) implementing eru's single-node central command.
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

**In scope** (MASTER roadmap Phases 0–6, plus the extension catalog from Phase 8 and the agent guide from Phase 7): single-node central command, operator CLI, WireGuard transport, secrets document, hardening, PostgreSQL 18, tuning, security, nginx TLS (eru-issued ACME certificates), PgBouncer, pgBackRest DB backups and restores, restic VPS backups, DR rebuild, metrics exporters, maintenance, extensions v1, status/doctor/top, upgrade/rollback, `--local` install, agent guide and skill.

**Out of scope:** multiple nodes, replicas and the Tailscale transport (Phase 9), HA (Phase 10), operator add/revoke and scoped operators, provider snapshots and alerting (Phase 7), `eru mcp` and major PG upgrades (backlog).

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
| Secrets document | `filippo.io/age` (scrypt recipient) |
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

An operator command against a remote cluster always runs the same thing:

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
| `eru secrets export` | `--out`, `--reveal-passphrase` | ✓ | | | ✓ | |
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
| `eru node selfcheck` | Binary can read config, the journal is readable, required tools are present. Creates missing `/var/lib/eru` (0700), `/var/log/eru` (0750) and `/run/lock` (1777), and fails if the owner lacks `rwx` on them, since root can write to a mode-000 directory |
| `eru node run <job>` | Timer entry points: `backup-full`, `backup-diff`, `backup-incr`, `backup-verify`, `restore-drill`, `vps-backup`, `maint-daily`, `maint-weekly`, `certs-renew`, `metrics-refresh` |
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
    ConfirmToken string   // template, e.g. "{cluster}/restore", "{db}"
    NeedsRestart string   // "never" | "maybe" | "always" (Postgres restart)
    ErrorCodes   []string // codes this command can return beyond the generic ones
    MutatingFlags    []string // flags that make an otherwise read-only command mutate
    DestructiveFlags []string // flags that make it destructive, e.g. queries --reset, db credentials --rotate, node upgrade --rollback
    LinuxOnly      bool     // emitted as "platforms": ["linux"]; every node-audience command sets it
    LinuxOnlyFlags []string // flags that are Linux-only, e.g. cluster init --local, wg export --install
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

### 6.1 `cluster.toml` (desired state; lives on the central command at `/etc/eru/cluster.toml`)

```toml
schema = 1
name = "prod"                       # [a-z][a-z0-9-]{1,30}; also pgBackRest stanza
domain = "data.example.com"         # FQDN; clients connect here
acme_email = "ops@example.com"
timezone = "UTC"

[tls]
mode = "acme"                       # acme | self-signed
acme_ca = ""                        # override directory URL (e.g. Let's Encrypt staging)

[transport]
kind = "wireguard"                  # wireguard; tailscale (Phase 9)
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
- `domain` is an FQDN. `acme_email` is optional; when set it must be an email address. It is only the ACME account contact, used in `acme` mode.
- Schedules parse with `systemd-analyze calendar`. Run it on the node. On the operator, use a Go parser for the subset and let the node have the final say.
- CIDRs parse. `0.0.0.0/0` in `client_allowlist` is allowed but produces a doctor `warn`.
- `retention_full ≥ 1`, `keep_weekly ≥ 1`.
- A database named `postgres`, `template0` or `template1` is rejected.
- Extensions exist in the catalog (§17).
- Until Phase 9, exactly one node with `role = "primary"` is allowed. More are rejected with `precondition_failed` and the hint "multi-node arrives in Phase 9".

- **Strict decode.** `ParseCluster` refuses unknown keys with `config_error` naming them. Every table above exists from Phase 2 on with the defaults shown, so a file from an earlier phase still decodes and later phases don't change the decoder. `schema` stays `1`.
- A table's fields are validated only when its feature is in use: `[backup]` only when `provider != ""` (empty means not configured).
- `tls.mode ∈ {acme, self-signed}`; `acme_ca` empty or an `https://` URL; `postgres.version == 18`; `profile ∈ {oltp, mixed, analytics}`; `client_connections` 10–100000; `locale_provider ∈ {builtin, icu, libc}`; `locale` matches `^[A-Za-z0-9_.@-]+$`.
- `[postgres.settings]` keys match `^[a-z_][a-z0-9_.]*$` and may not name a setting eru owns: `allow_alter_system`, `ssl*`, `listen_addresses`, `port`, `password_encryption`, `shared_preload_libraries`, `archive_*`, `data_directory`, `hba_file`, `ident_file`, `include*`.
- Database names are unique, and `<name>_migrate` etc. fit in 63 characters. `statement_timeout` is `0` or matches `^\d+(ms|s|min|h)$`. Extensions must be catalog entries (§17); until Phase 8 only the contrib entries exist, and any other name gets `validation_failed` with the hint "arrives with eru ext in Phase 8".
- `ValidateFor(scope)` has scopes `host` and `full`. `cluster.bootstrap phase=host` uses `host`, which allows a missing `domain`. Everything else uses `full`, where a missing domain is `validation_failed` with hint `eru config set domain <fqdn>`. This is how a Phase 1 cluster gets its database: `eru config set domain …`, `eru apply` (optionally `eru config set acme_email …` first).
- `DefaultCluster()` has no databases. Init adds `app` (`statement_timeout = "60s"`, `extensions = ["pg_stat_statements"]`) when the interview and flags name none.

`config set` uses dotted keys (`backup.vps.schedule`, `postgres.settings.work_mem`, `databases.app.statement_timeout`). Array-of-table entries are addressed by `name`. Values parse by the target field's type (bool, int, float, string, `[]string` as a comma list or TOML array literal). `schema`, `name`, `nodes.*.wg_address` and `transport.*` are immutable after init: `config set` refuses them, and so does a whole-file `config.set` (`config edit`), with `precondition_failed`, reason `immutable_key`.

**Desired-state store** (`internal/state`): a `Store` interface (`Load`, `Save(raw)` that validates with `full` and writes atomically, `Revision()` = sha256 of the file). Phase 2 has `FileStore` at `/etc/eru/cluster.toml`; the bucket copy arrives with Phases 3 and 4. `config.set`, `db.create`, `db.drop` and `cluster.bootstrap` write through it. `config.set` takes `base_revision` and refuses a stale one (`precondition_failed`, reason `config_changed`); with `dry_run` it returns the plan for the new file without saving.

**Answers file** (`--answers answers.toml`): the same schema as `cluster.toml`, plus a `[bootstrap]` table (`host`, `user`, `identity`, `host_key_fingerprint`) and a `[backup.credentials]` table (`access_key_id`, `secret_access_key`). The CLI strips credentials before persisting `cluster.toml` (they go to runtime secrets, §6.3).

### 6.2 Node files

| Path | Owner:mode | Content |
| --- | --- | --- |
| `/usr/local/bin/eru`, `/usr/local/bin/eru.prev` | root:root 0755 | Binary and rollback copy |
| `/etc/eru/node.toml` | root 0640 | `cluster`, `node_name`, `role`, `installed_at`. Marks a host as an eru node |
| `/etc/eru/cluster.toml` | root 0640 | Desired state |
| `/etc/eru/operators.toml` | root 0640 | Operator registry: `name`, `wg_address`, `wg_public_key`, `ssh_public_key` per operator. Steps 2 and 3 render `authorized_keys` and the wg0 peers from it |
| `/etc/eru/runtime/` | root 0700 | `pgbouncer_auth.pass` and `monitor.pass` (0600, written only from operator-supplied secrets), `backup.env` (S3 keys), `restic.env`, `metrics.htpasswd` (bcrypt) |
| `/etc/eru/tls/` | root 0755 | `server.crt` (0644), `server.key` (root:ssl-cert 0640; `postgres` and `pgbouncer` are in `ssl-cert`) |
| `/var/lib/eru/journal.json` | root 0600 | §9.3 |
| `/var/lib/eru/state.json` | root 0600 | `secrets_confirmed_at`, `lockdown_pending`, `locked_down_at`, `last_apply`, `restore` (in-progress restore record), `drills[]` (last 12) |
| `/var/lib/eru/ssh-snapshot/` | root 0700 | Commit-confirm snapshot (`snapshot.json`, `sshd-dropin.conf`, `ufw-rules.txt`) and `reverted.json` (§8.3) |
| `/var/lib/eru/drill/` | postgres 0700 | Scratch restores |
| `/var/lib/node_exporter/textfile/eru.prom` | root 0644 | §15.4 |
| `/var/log/eru/` | root 0750 | One log per RPC/job: `<UTC ts>-<method>-<request_id>.log`, rotated by logrotate (30 days) |
| `/run/lock/eru.lock` | | Global lock (§9.5) |

### 6.3 Operator files (`$XDG_CONFIG_HOME/eru`, default `~/.config/eru`, dir 0700)

```
config.toml                         current_context = "prod"
contexts/<name>/context.toml        cluster name, domain, nodes [{name, role, public_address, wg_address, wg_public_key, listen_port}],
                                    operator {name, wg_address, ssh_user}, auto_upgrade_nodes, lockdown_confirmed_at,
                                    created_at, eru_version
contexts/<name>/wg.key              operator WireGuard private key (0600)
contexts/<name>/id_ed25519          operator SSH private key (0600)
contexts/<name>/known_hosts         pinned server host keys (wg address entries)
contexts/<name>/credentials.json    sudo password, database credentials (admin, per-db roles + URLs, services
                                    pgbouncer_auth / eru_monitor) and later metrics credentials (0600)
pending/<name>/                     in-progress init: answers.toml, secrets.json (0600), progress.json, known_hosts
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

- The request also carries `client_commit`, `client_channel` and `lock_timeout_s` (default 30; timers pass 0).
- **Version check on the node:** `client_version` and `client_channel` must equal the node's. For dev builds (`channel = "dev"`) `client_commit` must match too, because every dev build reports `version = "dev"`. The operator-side handshake uses the same rule.
- **Decoding:** the request must be exactly one JSON object within 1 MiB, then EOF, with `schema = "eru-rpc/v1"`, a well-formed `method` and a ULID `request_id` (it names the run log). Unknown top-level fields are allowed so a newer operator can add optional ones. Method params decode strictly, except `upgrade.install`, which crosses versions and decodes leniently.
- **Frozen:** a newer operator calls `upgrade.install` on an older node, so the request fields and `upgrade.install`'s params (`path`, `sha256`, `version`, `commit`, `channel`) and result (`previous`, `installed`, `rolled_back`) only ever gain optional fields.
- **Confirm token for `apply`:** a non-empty `confirm` that isn't `{cluster}/restart` is refused (exit 4) before any step runs, because whether a restart is needed is only known afterwards. An empty `confirm` never fails; it leaves the restart pending.
- **stdout**: NDJSON events (§5.2). The final line is `result`. Exit status equals the envelope's exit code.
- **stderr**: free-form diagnostics. The operator surfaces it only with `--verbose`.
- **Methods** mirror operator commands, named `<group>.<verb>` (each word may contain inner hyphens, as in `cluster.confirm-secrets`): `cluster.bootstrap`, `cluster.confirm-secrets`, `apply`, `plan`, `config.get|set|validate`, `status`, `doctor`, `db.*`, `ext.*`, `tune.*`, `backup.*`, `restore.*`, `vps.*`, `maint.*`, `ssh.lockdown`, `ssh.confirm`, `ssh.status`, `tls.status`, `logs`, `upgrade.install`, `upgrade.rollback`, `pg.update`, `sql`, `queries`, `top.sample`.
- **Confirmation is re-checked node-side.** The node recomputes the token and rejects mismatches, so a buggy operator can't skip it. Destructive methods (`db.drop`, `db.credentials` with `rotate`) also refuse an *empty* token with `unsafe_action_requires_confirmation`; for `apply` and `tune.apply` an empty token only leaves the restart pending.
- **Phase 2 methods:** `cluster.bootstrap phase=database`; `config.get` (raw TOML + revision), `config.set`, `config.validate`; `db.list`, `db.create` (needs `db.<name>.*` secrets, adds `[[databases]]`, runs `postgres-roles-v1` only, puts `cluster.toml` back if the database was never created), `db.drop`, `db.credentials` (rotate only; reading credentials is operator-side); `tune.show`, `tune.apply`; `tls.status` (served certificate state, polled by init); `logs` (unit allowlist `postgres`, `pgbouncer`, `nginx`, `eru-certs`, `eru`; `follow` streams until the client disconnects); `sql` (for `--local` only: one transaction as `postgres` with `SET LOCAL ROLE`, `READ ONLY` unless `write`).
- **Read-only SQL** (`eru sql`, `top.sample`, `queries`, reports) runs over pgx through the tunnel, not via RPC, except where node-local access is needed (`postgres` peer for superuser-only views). RPC methods for these exist for `--local` mode.
- **Sudoers**: `/etc/sudoers.d/eru` contains `eru ALL=(root) NOPASSWD: /usr/local/bin/eru node *`. Validate it with `visudo -cf`.

### 7.5 Binary push

1. Operator obtains the node artifact for the node arch: from `cache/releases/<ver>/linux-<arch>/eru`, downloading and verifying it per §19.1 if missing, or from `ERU_NODE_BINARY` for dev.
2. Stream it via `ssh exec: cat > /tmp/eru-<ver>-<rand>` as the SSH user.
3. Run `sudo -n /usr/local/bin/eru node rpc` with `upgrade.install {path, sha256, version, commit, channel}`. The `/tmp` file is owned by the SSH user, who could change it after a check, so the node first copies it into a root-only `/usr/local/bin/eru.new` (0700) while hashing it, verifies and preflights that copy, then makes it 0755, keeps the old binary as `eru.prev` and renames the new one into place. It runs `selfcheck` and rolls back if that fails (or removes the new binary if there was no old one). The `/tmp` file is deleted on every outcome.
4. **Bootstrap special case**: no node binary exists yet. As root (or `sudo -n`) the operator runs `install` directly after a `sha256sum` check executed remotely.

---

## 8. Cluster init and bootstrap

### 8.1 `eru cluster init` flags

`--host <user@ip[:port]>`, `--identity <path>`, `--password-stdin`, `--host-key-fingerprint`, `--accept-new-host-key`, `--answers <file>`, `--name`, `--domain`, `--admin-user`, `--timezone`, `--data-directory`, `--acme-email`, `--profile`, `--client-connections`, `--database <name>` (repeatable), `--allow <cidr>` (repeatable), `--backup-provider`, `--backup-endpoint`, `--backup-region`, `--backup-bucket`, `--backup-prefix`, `--backup-key-id`, `--backup-secret-stdin` / `--backup-secret-file`, `--vps-backup-schedule`, `--db-full-schedule`, `--db-diff-schedule`, `--extensions`, `--tls-mode`, `--local`, `--restore <bundle-file|->` (§13), `--restore-time <ts>`, `--secrets-out <path>`, `--reveal-passphrase`, `--confirm-secrets-stored`, `--dns-ready`, `--skip-dns-check` (deprecated, no effect), `--resume`, `--discard`. Until their phase lands, the backup and VPS flags are accepted with a `flag_not_used_yet` warning.

- `--host` also takes a `Host` alias from `~/.ssh/config`. First contact reads the matching block for `HostName`, `User`, `Port` and `IdentityFile`; explicit `user@`/`:port` and `--identity` win. `--identity` (or the resolved `IdentityFile`) is checked to be a readable private key while the answers are collected, before anything is prompted or generated.
- Without `--identity`, first contact offers ssh-agent keys and `~/.ssh/id_{ed25519,ecdsa,rsa}` as one `publickey` method. An encrypted default key that the agent doesn't hold gets a passphrase prompt in a TTY; otherwise it is skipped and named in `details.skipped_keys`.
- `--discard --name <n>` deletes `pending/<n>` after a typed `discard` (or `--confirm discard`). When the host phase already ran it warns `host_already_bootstrapped`: the discarded pending state holds the only credentials for that server. The `init_pending` error names both `--resume` and `--discard` and carries `details.pending_path`.
- During init, a `step_failed` hint says `eru cluster init --resume --name <n>` instead of `eru apply`, adding `--local` only when the init is local, and every `step_failed` carries `details.output_tail`: the last 20 redacted output lines of the failed script.

### 8.2 Remote init sequence

Each numbered phase emits `step` events and records progress in `pending/<name>/progress.json`. `--resume` continues from the last completed phase.

1. **Local preflight** (§20).
2. **Collect answers** from flags, the answers file, and prompts. Prompt only in a TTY. Validate (§6.1). The `huh` interview has groups *Server* (name, admin user, timezone, data directory), *Endpoint* (domain, TLS mode, client allowlist, then ACME email in `acme` mode), *Workload* (profile, client connections) and *Databases* (names, default `app`). Each question has a flag; a question already answered by a flag or the answers file is skipped. Backup, VPS and extension questions arrive with their phases. Non-interactive runs check `--confirm <cluster>`, `--confirm-secrets-stored`, `--secrets-out` and `--reveal-passphrase` before anything changes. In `acme` mode, init then shows the `A <domain> → <public IPv4>` record so the operator can create it while the host phase runs.
3. **Generate secrets** on the operator (§10.2), stored in `pending/<name>/secrets.json` (0600): the operator WireGuard keypair, SSH ed25519 key, sudo password, and every database and service password, so `--resume` reuses them.
4. **Backup credentials check**: from the operator, do a signed `PUT`/`GET`/`DELETE` of `<prefix>/.eru-probe` using the S3 client in `internal/backup/s3probe.go` (minimal SigV4 over `net/http`; no SDK). This fails early on bad keys or a bad bucket. For a non-empty `<prefix>/pgbackrest` without `--restore`, return `precondition_failed` ("prefix already contains a backup repository").
5. **First contact** over public SSH. Detect arch and OS (`/etc/os-release`). Push the binary (§7.5, bootstrap case).
6. **RPC `cluster.bootstrap` phase=host** over public SSH. The node writes `cluster.toml` and `node.toml`, then runs hardening steps 1–6 (§11), which:
   - create the admin user with the operator SSH key and sudo password hash;
   - install WireGuard with the operator peer;
   - configure ufw with **public 22 still open**.
7. **Switch transport**: start userspace WireGuard to `public_address:51820` and SSH to `10.42.0.1:22` as `admin_user`. Pin the host key (same host keys, new address entry). Run the handshake.
8. **Commit-confirm lockdown** (§8.3).
9. **DNS** (`dns_confirmed`, `acme` mode only). Show the records (`A`, plus `AAAA → <public IPv6>` when host bootstrap reported one; it is kept in `progress.json`; when none was reported, the panel says there is no AAAA record), with the note that they must be DNS only (not proxied). The A record is also shown early, while the host phase runs, with a note that the AAAA record appears once the host is up and that eru continues and then pauses for the operator to confirm the records before the database phase. In a TTY the operator types `confirmed` once the records exist; `--dns-ready` skips that pause, and non-interactive runs don't pause. eru then queries 1.1.1.1 and 8.8.8.8 once, directly (a Go `net.Resolver` dialing them, so local caches can't lie), and continues whatever they answer. A mismatch is a `dns_mismatch` warning listing each resolver's answers against the expected addresses. Answers inside Cloudflare's published ranges are a `dns_proxied` warning ("proxied by Cloudflare: database clients can't connect through it; set the record to DNS only"). DNS never blocks init: the certificate wait (step 11) shows whether the certificate was issued. `--skip-dns-check` is accepted with a `flag_deprecated` warning. `self-signed` mode skips this phase.
10. **RPC `cluster.bootstrap` phase=database** (`database_bootstrapped`) over WireGuard: it requires `node.toml` of the same cluster and a completed host phase, writes the new `cluster_toml` through `state.Store`, and runs the database steps (7–13 in Phase 2; later phases append theirs). All database and service passwords travel in `secrets`. Result: `{phase, steps, tls: {state, not_after}, restart_performed}`.
11. **Certificate wait** (`tls_checked`): poll `tls.status` for up to 120 s until the placeholder is replaced. `tls.state` is `acme`, `pending` or `self-signed`; `pending` is a `tls_pending` warning, never a failure. The warning carries the resolver answers (or the Cloudflare hint) from a fresh DNS check, and says that `eru-certs.timer` retries every hour.
12. **Secrets handoff** (`secrets_written`, §10): print the handoff (§10.1) in the terminal and write nothing, unless `--secrets-out <path>` asks for the armored transfer bundle to be written there (0600). In a TTY, require typing `stored` (the prompt reads `Type 'stored' once you've saved both parts in your password manager`), then offer to clear the screen (default yes). Without a TTY, require `--secrets-out`, `--reveal-passphrase` and `--confirm-secrets-stored`; the passphrase is in `data.passphrase` and the plain secrets are never in structured output. A resumed init that stopped before the confirmation shows a new handoff with a new passphrase. Then RPC `cluster.confirm-secrets` sets `secrets_confirmed_at`.
13. **Create the local context** from the pending data (with `lockdown_confirmed_at` set), make it current, and delete `pending/<name>`.
14. Print the summary: connection URLs with the passwords left out (`postgres://app_app:«in secrets document»@…`), TLS state, `restart_performed`, and `next: ["eru sql --db <first db> \"select 1\"", "eru psql --db <first db>"]`. Doctor and backup results join with their phases.

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

`curl -fsSL <base>/install.sh | sudo bash` on the server installs `/usr/local/bin/eru`. It prompts "Initialise a cluster on this server now? [y/N]" when a TTY is present, which runs `eru cluster init --local`.

`eru cluster init --local`:

- requires root, or escalates via §20.3, and must run as `/usr/local/bin/eru` (the revert timer calls that path);
- takes the node's public address from the answers file, then `--host <public-ip>`, then the default-route source address (with a warning when it is private);
- skips first contact and binary push and uses the in-process dispatcher;
- generates secrets on the server and runs the same phases as a remote init: host steps, lockdown arming, the DNS step (`acme` mode), the database phase and the certificate wait;
- prints the secrets handoff (§10.1) on the server console and writes nothing to `/root`, plus a one-line instruction: "On your machine run `eru login` and paste the transfer bundle".

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

Pending restarts are saved in the journal, so a later confirmed `apply` still performs the restart even though the steps themselves now skip. That apply restarts once and clears the list; `plan` reports it too. If a step needs a restart and the build has no restarter, `apply` fails with `error` rather than promising a restart that can't happen.

`plan` runs steps 1–3 and lists those that would apply, plus file diffs from `Inputs()` against the on-disk files (unified diff, secrets redacted).

### 9.3 Journal (`/var/lib/eru/journal.json`)

```json
{"schema":1,"eru_version":"1.0.0","updated_at":"…","restart_pending":["postgres-config-v1"],
 "steps":{"postgres-config-v1":{"status":"completed","inputs_hash":"…","started_at":"…","completed_at":"…","restart":"reload","error":""}}}
```

Write it atomically (temp file plus rename). Steps removed from the catalog are left as-is and ignored.

### 9.4 Embedded scripts

Use `//go:embed scripts/*.sh`. Run each script via `/bin/bash -s` with the env map, as in deploycrate `Shell.Run`. Scripts get a fixed `PATH`, `LANG`, `HOME` and `DEBIAN_FRONTEND` plus the step's validated keys, and don't inherit the caller's environment. Each runs in its own process group, which gets SIGTERM on cancel and SIGKILL 10 s later. The combined output is streamed line by line as `log` events, redacted, and appended to the run log.

Run logs live in `/var/log/eru/`. `plan` and `apply` write one; `selfcheck` and dry-run `apply` don't. Lock and version refusals happen before the log opens, so they leave none.

Rendered files are written by Go (`render` package) with an atomic write and owner/mode set, **not** by heredocs, for anything with a template. Scripts handle packages, services, users and ufw.

### 9.5 Locks

`/run/lock/eru.lock` uses `flock`:

- **exclusive**: `apply`, `restore.*` (except status), `upgrade.*`, `pg.update`, `ext.add/remove`, `tune.apply`, `vps.restore`, `restore-drill`;
- **shared**: `backup.*`, `vps.backup`, `maint.*`, `db.*`.

If a lock can't be acquired within `--timeout` (default 30 s; timers use 0), return `precondition_failed` with the holder (PID, method, start time, written to `/run/lock/eru.lock.info`). Exclusive holders remove the info file on release; shared holders leave it, and it names only the most recent shared holder. A holder whose PID is gone is reported as `null`.

Maintenance additionally refuses while a pgBackRest backup is running. Check `pgbackrest --stanza=<s> info --output=json`'s `status.lock.backup.held`.

### 9.6 Redaction

`secrets.Registry` collects every secret value loaded or generated in the process. All output paths (events, logs, errors, envelopes) pass through `Redact`, which replaces values of 6 or more characters with `«redacted»`.

---

## 10. Secrets

### 10.1 Secrets handoff and transfer bundle

eru never writes the secrets document to disk by default. At the end of init (and on `eru secrets export`) it prints the **secrets handoff**, the cluster's master record, in the terminal:

1. **Explicit secrets**: every value in plain text, grouped like password-manager entries (cluster, each node, the operator with its SSH and WireGuard private keys, the database admin, each database's roles and URLs, the service roles, then backups and metrics from their phases, then the connection notes and break-glass text). Each label is on its own line and each value on the next, flush left, so a triple-click copies one value. Groups with nothing yet are left out.
2. **Transfer bundle**: the document below as an ASCII-armored `age` block (`-----BEGIN AGE ENCRYPTED FILE-----`), plus its passphrase. `eru login` and `cluster init --restore` take it from a paste, stdin or a file.

The bundle is `age` scrypt-encrypted (`age.NewScryptRecipient(passphrase)`, work factor 18). The plaintext is JSON:

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

- **Passphrase**: 8 words from the EFF large wordlist (embedded, about 103 bits), joined with `-`. It's displayed once in the handoff and never stored by eru.
- **`eru login [bundle-file|-]`**: with no argument in a TTY, prompt to paste the bundle and read up to its END line; `-` reads it from stdin (not together with `--passphrase-stdin`); a path reads a file. Armored and binary `age` both work, so old `.secrets.age` files still open. Decrypt (passphrase from TTY, `--passphrase-stdin` or `--passphrase-file`) and create `contexts/<name>/…`. Bad passphrase returns `secrets_error`.
- **`eru secrets export`**: rebuilds the document from the context and shows a new handoff with a new passphrase, with the same `stored` confirmation. `--out <path>` also writes the armored bundle. Without a TTY it needs `--out` and `--reveal-passphrase`, like init. Credentials rotated since login require a re-login or the `credentials` command. The previous bundle still opens with its own passphrase.

### 10.2 Generated secrets

| Secret | Generator | Where it lives after init |
| --- | --- | --- |
| Admin user sudo password | 24 chars base62 | Doc; node has `/etc/shadow` hash only |
| `eru_admin`, per-db `migrate`/`app`/`ro` passwords | 32 chars base62 | Doc; node has PG SCRAM verifiers only |
| `pgbouncer_auth` password | 32 chars base62 | Doc (`database.services`) + node `/etc/eru/runtime/pgbouncer_auth.pass` (0600) and its SCRAM verifier in `userlist.txt` |
| pgBackRest `cipher_pass` | 64 chars base62 | Doc + node `pgbackrest.conf` |
| restic password | 64 chars base62 | Doc + node `restic.env` |
| Metrics password | 32 chars base62 | Doc + node bcrypt hash in an nginx `auth_basic_user_file` |
| `eru_monitor` password | 32 chars base62 | Doc (`database.services`) + node `/etc/eru/runtime/monitor.pass` (0600) and its SCRAM verifier in `userlist.txt` |
| Bucket keys | user-provided | Doc + node `backup.env`, `pgbackrest.conf`, `restic.env` |

Use `crypto/rand` only.

**Every secret eru generates goes into the secrets document**, which works as the cluster's vault. The **operator** generates all database passwords: `eru_admin`, each database's `migrate`/`app`/`ro`, and the service roles `pgbouncer_auth` and `eru_monitor`. The node never generates one. It keeps SCRAM verifiers only, except the two service passwords that PgBouncer and the exporters need at runtime.

- **RPC secret names:** `db.admin`, `db.<db>.migrate`, `db.<db>.app`, `db.<db>.ro`, `svc.pgbouncer_auth`, `svc.eru_monitor` (constants/helpers in `internal/steps/secrets.go`). The node registers every received secret for redaction before anything logs.
- **Verifiers, not plaintext:** `pg.SCRAMVerifier` (SCRAM-SHA-256, 4096 iterations, random 16-byte salt) is computed in Go and sent as `ALTER ROLE … PASSWORD '<verifier>'` on psql's stdin, so the plaintext never reaches psql, server logs or `pg_stat_statements`. Verifiers are redacted too.
- **Set once:** a password is set only when its role is created, or by `db credentials --rotate`. `postgres-roles-v1` never re-sets an existing role's password, and passwords are not part of its inputs. A login role that must be created without a supplied secret fails with `input_required` naming the role. The same goes for a missing `/etc/eru/runtime/*.pass`.
- **Operator top-up:** before `apply` and init's database phase, the operator compares `[[databases]]` and the service roles with `credentials.json`, generates anything missing, saves it to `credentials.json`, sends it, and warns `secrets_document_stale` ("run `eru secrets export`"). It saves before sending: a role created with a password the operator then lost would be unrecoverable.
- The document's `database` section holds `admin`, `databases[]` (roles + URLs), `services` (`pgbouncer_auth`, `eru_monitor`), and `connection_notes`, which says that 5432 is reachable only over WireGuard or with `expose_postgres = true`. URLs use `sslmode=verify-full&sslrootcert=system` in `acme` mode and `sslmode=require` in `self-signed` mode. The `app` URL is on 6432 (PgBouncer, transaction mode); the `migrate` URL is on 5432 (session features for migrations).

---

## 11. Step catalogue

Order is fixed. IDs end in `-v1`; bump the suffix whenever behaviour changes in a way the input hash can't capture (for example, script logic changes). Columns: what it does, and how Check decides completeness.

| # | ID | Does | Check |
| --- | --- | --- | --- |
| 1 | `base-packages-v1` | Wait for `cloud-init status --wait`. apt update and install: `curl ca-certificates gnupg sudo ufw fail2ban python3-systemd wireguard-tools chrony logrotate unattended-upgrades jq lsb-release`, plus `apt-transport-https` when the release still has it (it is transitional). `python3-systemd` lets the fail2ban sshd jail read the journal | All installed (dpkg-query) |
| 2 | `admin-user-v1` | Create `admin_user` (shell bash, groups `sudo`), set the password hash, install the operator SSH pubkey to `authorized_keys`, write `/etc/sudoers.d/eru` (§7.4) and validate it | User exists, key present, sudoers matches |
| 3 | `wireguard-v1` | Adapt deploycrate `wireguard.sh` (no ip_forward: not a gateway). Address `10.42.0.1/16`; operator peers rendered from `cluster.toml` + operator registry (`/etc/eru/operators.toml`: name, wg pubkey, address). `ufw allow 51820/udp`. The node key lives in `/etc/wireguard/eru.key` (`eru.pub`); a running wg0 is updated with `wg syncconf` | `wg show wg0` lists all peers |
| 4 | `sshd-hardening-v1` | Write `/etc/ssh/sshd_config.d/00-eru.conf` (below). It sorts first because sshd keeps the first value it reads, so provider drop-ins such as `50-cloud-init.conf` cannot re-enable passwords. Handle `ssh.socket` (§8.3). `sshd -t` and check that `sshd -T` shows `permitrootlogin no` and `passwordauthentication no` (roll back otherwise), reload. Public 22 stays open until lockdown | Drop-in matches render |
| 5 | `firewall-v1` | ufw: `default deny incoming`, `default allow outgoing`, allow 51820/udp, 80/tcp, 443/tcp, `in on wg0` 22/tcp, 6432/tcp and 5432/tcp; the allowlist rules for 6432 (if `expose_pgbouncer`) and 5432 (if `expose_postgres`). WireGuard peers always reach both ports; the expose flags gate only the allowlist. Keep public 22 only if `lockdown_pending` or not yet locked. Rules not in the set are deleted (expected rules are added before and after, so SSH always has an allow rule). `ufw --force enable` | `ufw status` active and `ufw show added` equals the expected rule set |
| 6 | `host-baseline-v1` | journald drop-in (copy deploycrate `host.sh`), swapfile (`swap_mb`), timezone, chrony enabled, unattended-upgrades security-only with `Package-Blacklist` `postgresql-`, `libpq`, `pgbouncer`, `pgbackrest`, `nginx`, `libnginx-mod-` and `Automatic-Reboot "false"` (via `#clear` of the distribution's origins), fail2ban sshd jail (`backend = systemd`, iptables ban actions so bans stay out of the ufw rule set), logrotate for `/var/log/eru` | Files match render, services active |
| 7 | `kernel-tuning-v1` | `/etc/sysctl.d/90-eru.conf` and the `eru-thp.service` oneshot (below), `sysctl --system`, `daemon-reload`, `enable --now eru-thp.service`. `vm.nr_hugepages` from §12.3 (0 when `huge_pages = off`) | `sysctl -n` values equal render, THP `enabled` = `[never]` |
| 8 | `postgres-packages-v1` | Write `/etc/postgresql-common/createcluster.d/00-eru.conf` with `create_main_cluster = false` **before** installing. Install `postgresql-common`, run `/usr/share/postgresql-common/pgdg/apt.postgresql.org.sh -y`, install `postgresql-18 postgresql-client-18 postgresql-contrib pgbouncer`, then `apt-mark hold postgresql-18 postgresql-client-18`. Debian's pgbouncer auto-starts with its own config, so `systemctl stop pgbouncer` right after install until step 13 renders it. `pgbackrest` arrives with Phase 3 as `postgres-packages-v2` | Packages installed and held |
| 9 | `postgres-cluster-v1` | If no cluster exists: `pg_createcluster 18 main --datadir=<data_directory> --locale-provider=<provider> --locale=<locale> --start-conf=auto -- --auth-local=peer --auth-host=scram-sha-256` (checksums are default-on in 18; assert `data_checksums = on` after start and fail the step otherwise). A non-default `data_directory` gets its parent created `postgres:postgres 0700`. An existing 18/main with a **different** data directory is `precondition_failed`; eru never re-creates a cluster. systemd drop-in for `postgresql@18-main.service`: `LimitNOFILE=65536`, `OOMScoreAdjust=-900`, `Environment=PG_OOM_ADJUST_FILE=/proc/self/oom_score_adj PG_OOM_ADJUST_VALUE=0` | `pg_lsclusters` shows 18/main online |
| 10 | `tls-v2` | Refuse a server that has `caddy` installed (an older eru used Caddy; there is no migration, rebuild the server). Install `nginx` and `libnginx-mod-stream` from the distribution (at least `NginxMinVersion`), `apt-mark hold` them, remove the default site. Add `postgres` and `pgbouncer` to `ssl-cert`. If no certificate exists, write a self-signed P-256 placeholder (30 days, `CN`/SAN = domain) into `/etc/eru/tls` so PG, PgBouncer and nginx can start; TLS never blocks on ACME. This step runs before `postgres-config-v1`, because that step turns `ssl` on and PostgreSQL refuses to start without a readable certificate. Render `/etc/nginx/nginx.conf` (§15.2), `nginx -t`, `systemctl reload-or-restart nginx`. Install `eru-certs.service` and `eru-certs.timer` (hourly), enable the timer and `systemctl start --no-block eru-certs.service` so issuance starts at once (§15.3) | nginx active, `/etc/eru/tls/server.crt` exists |
| 11 | `postgres-config-v1` | Render `conf.d/10-eru-tuning.conf` (§12), `20-eru-security.conf`, `30-eru-extensions.conf` (§17), `50-eru-settings.conf` (`[postgres.settings]`, values single-quoted with `''` escaping), and create `90-overrides.conf` (empty, comment header) if missing (`40-eru-backup.conf` arrives in Phase 3; `wal_level = replica` and `max_wal_senders = 10` are already in the tuning file, so only `archive_mode` will need a restart then). Render `pg_hba.conf` and `pg_ident.conf`. Every file is written owned by `postgres:postgres` with `0640` for `pg_hba.conf` and `pg_ident.conf` and `0644` for `conf.d`, so the server can read them before the reload. Ensure `include_dir = 'conf.d'` is in `postgresql.conf`. Before reload, `pg_file_settings` and `pg_hba_file_rules` must have no errors; otherwise the previous files are restored and the step fails with the errors. Classify changes via `pg_settings.context` (`postmaster` means restart, listed in the notes). The **first run** (no completed journal entry for this step) restarts unconditionally and without confirmation, because there is no traffic yet. When huge pages change, `shared_memory_size_in_huge_pages` is computed while PG is stopped during that restart, and `vm.nr_hugepages` is re-applied through step 7's helper | Rendered = on disk, with owner and mode |
| 12 | `postgres-roles-v1` | §14 SQL through `pg.Local` (psql as the `postgres` OS user, SQL on stdin); per-db roles, databases, default privileges, extensions, `eru.pgbouncer_get_auth`. Passwords follow §10.2 (set once, verifiers only). Writes `/etc/eru/runtime/{pgbouncer_auth,monitor}.pass` from the request secrets when missing. Inputs are names, timeouts and extensions, never passwords. Databases removed from `cluster.toml` are not dropped (only `db drop` drops); they are reported as `orphaned_database` warnings | All roles, dbs, grants, extensions and the auth function exist |
| 13 | `pgbouncer-v1` | Render `/etc/pgbouncer/pgbouncer.ini` (§14.3) and `userlist.txt` (verifiers read back from `pg_authid` for `pgbouncer_auth` and `eru_monitor`), `postgres:postgres 0640`, overwriting Debian's files. Enable and restart | Rendered = on disk (owner and mode included), and `SHOW VERSION` as `eru_monitor` on 6432 works (via a 0600 `PGPASSFILE`) |
| 14 | `pgbackrest-v1` | Render `/etc/pgbackrest/pgbackrest.conf` (§13.1). `pgbackrest --stanza=<s> stanza-create`, then `check`. If the journal has no `first-full`, run `backup --type=full` (streams progress) | `check` passes, first full exists |
| 15 | `restic-v1` | Install the pinned restic (copy deploycrate `backup.sh` checksum pattern). Render `restic.env`. `restic init` if needed (`restic cat config` decides). First snapshot (§13.6) | Repo reachable, ≥ 1 snapshot |
| 16 | `metrics-v1` | Install pinned `node_exporter` (copy deploycrate script; textfile collector dir `/var/lib/node_exporter/textfile`), `postgres_exporter`, `pgbouncer_exporter`, `pgbackrest_exporter` as systemd services bound to 127.0.0.1 (§15.1). It adds the `/metrics` locations to the 127.0.0.1:8443 server in `nginx.conf` | All exporters return 200 locally |
| 17 | `timers-v1` | Render systemd `.service`/`.timer` pairs for every schedule in `cluster.toml` (§18). `systemctl daemon-reload`, enable timers, disable removed ones (`eru-*.timer` not in the render) | Timer set matches |
| 18 | `finalize-v1` | Run doctor (§21). Fail the step if any check is `fail`. Write metrics textfile. Upload `cluster.toml` as a restic snapshot tagged `eru-config` | Never complete (always runs; cheap) |

`cluster.bootstrap phase=host` runs steps 1–6 (`steps.HostStepIDs()`). `phase=database` runs the database steps (`steps.DatabaseStepIDs()`), and `apply` runs everything. Each phase appends its own steps: Phase 2 has 1–13, and steps 14–18 arrive with Phases 3, 4 and 6.

**Restarter** (`steps.Restarter()`): `systemctl stop pgbouncer` → `pg_ctlcluster 18 main restart -m fast` → wait for `pg_isready` (60 s) → `systemctl start pgbouncer`, reporting `restart_downtime_ms`. A restart needs `--confirm {cluster}/restart` except on step 11's first run.

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
- Include list: `/etc` (with the ACME account in `/etc/eru/acme`), `/var/lib/eru`, `/root`, `/home`, `/usr/local/bin/eru`, plus `extra_paths`.
- Exclude list: `<data_directory>`, `/var/lib/postgresql`, `/var/lib/pgbackrest`, `/var/spool/pgbackrest`, `/var/lib/eru/drill`, `/var/cache`, `/var/log`, `/swapfile`, `/proc`, `/sys`, `/dev`, `/run`, `/tmp`.
- Job `vps-backup`: `restic backup --tag eru-vps --host <node> --files-from … --exclude-file …`, then `restic forget --tag eru-vps --keep-weekly N --keep-monthly M --prune`.
- Job `vps-check` (monthly): `restic check --read-data-subset=5%`.
- `eru vps backup list` → `restic snapshots --json --tag eru-vps`.
- `eru vps restore --snapshot <id> --path <p>… --to <dir>` restores into `--to` (default `/var/lib/eru/vps-restore/<id>`). Without `--to` and with `--in-place`, it's destructive and restores over the live paths given (requires `--confirm <cluster>/vps-restore`).
- Config snapshots: `restic backup --tag eru-config --stdin --stdin-filename cluster.toml` after each successful `apply`.
- `eru upgrade` and `eru pg update` take a `vps-backup` first.

### 13.7 DR: `eru cluster init --restore <bundle-file|-> --host root@<new-ip>`

1. Decrypt the transfer bundle (same input rules as `eru login`). Probe the bucket. Find the latest `eru-config` snapshot and load `cluster.toml` from it. Override `nodes[0].public_address` with the new host IP.
2. First contact and binary push (§8.2 steps 5–6), with these differences:
   - The operator keys come from the document, not newly generated.
   - Before step 3 (`wireguard-v1`), restore the allowlist paths from the latest `eru-vps` snapshot to their live locations: `/etc/wireguard`, `/etc/ssh/ssh_host_*`, `/etc/eru`, `/root`, `/home`, `extra_paths`. Use `restic restore --include` into a temp dir and copy with ownership. The server keeps its WireGuard identity and host keys, so existing contexts keep working once the public address is updated.
3. Lockdown (§8.3), then steps 7–13 with **`postgres-cluster-v1` in restore mode**. Create the cluster, stop it, empty the datadir, run `pgbackrest restore` (`--type=default`, or `--type=time --target=<--restore-time>` with `--target-action=promote`), and start it.
4. Steps 14–18 (stanza exists; `check`, then an immediate full backup on the new timeline).
5. Update the local context's `public_address`. Show a new secrets handoff with the new address and a new passphrase. Print a DNS reminder if the A record of `domain` doesn't resolve to the new IP.

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
CREATE ROLE eru_monitor LOGIN PASSWORD :'monitor_pw'; GRANT pg_monitor TO eru_monitor;  -- password used by PgBouncer stats and the exporters
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

- **`eru db drop <d>`**: `DROP DATABASE <d> WITH (FORCE)`, then `REASSIGN OWNED BY … TO postgres; DROP OWNED BY …` in `postgres` and drop the four roles, then remove the entry from `cluster.toml`. The confirm token is `<d>`; `--yes` is never enough, and the system databases are refused. Take a `backup --type=incr` first. Phase 2 has no pgBackRest yet, so it drops without one and warns `no_backup_before_drop`.
- **`eru db credentials <d> --rotate --role app`**: generate a password, `ALTER ROLE … PASSWORD`, print once, and update `contexts/<name>/credentials.json`. Warn that the secrets document is stale and suggest `eru secrets export`.

Role names are validated by regex and also quoted with `pgx.Identifier{}.Sanitize()` (`pg.Quote`). The `:'pw'` placeholders above are SCRAM verifiers computed in Go (§10.2), never plaintext. psql `-v` variables carry only validated identifiers and non-secret literals.

**Operator SQL** (`eru sql`, `eru psql`) connects to Postgres on 5432 over the tunnel (pgx with `transport.Tunnel.DialContext`, `ServerName = domain`, `verify-full` against system roots; `self-signed` mode uses `require` and warns `tls_unverified`). `--db` is required, and there is no default database; a missing `--db` is `input_required`. The default role is `ro`. `--write` is needed for `app`, `migrate` or `admin` and is a usage error with `ro`. Each call runs in one transaction, `READ ONLY` unless `--write`. `migrate` acts as `<db>_owner`.

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

## 15. TLS, metrics and nginx

### 15.1 Exporters

| Exporter | Listen | Runs as | DB access |
| --- | --- | --- | --- |
| node_exporter | 127.0.0.1:9100 | `node_exporter` | `--collector.textfile.directory=/var/lib/node_exporter/textfile` |
| postgres_exporter | 127.0.0.1:9187 | `postgres_exporter` (system user) | `DATA_SOURCE_NAME=postgresql:///postgres?host=/var/run/postgresql&user=eru_monitor` (peer via ident map) |
| pgbouncer_exporter | 127.0.0.1:9127 | `pgbouncer_exporter` | `postgres://eru_monitor:<pw>@/pgbouncer?host=/var/run/postgresql&port=6432`. `eru_monitor` is in PgBouncer `stats_users` and authenticates against `userlist.txt` (it's excluded from `auth_query`). Its password lives in `/etc/eru/runtime/monitor.pass` and is passed via an `EnvironmentFile` |
| pgbackrest_exporter | 127.0.0.1:9854 | `postgres` (needs repo config) | `--backrest.config=/etc/pgbackrest/pgbackrest.conf`, collection interval 600 s |

Each exporter is a downloaded pinned binary with a sha256 per arch, following the `node-exporter.sh` pattern. Unit hardening: `NoNewPrivileges=yes`, `ProtectSystem=strict`, `ProtectHome=yes`, `PrivateTmp=yes`, `ReadOnlyPaths=/`.

### 15.2 nginx

eru renders the whole `/etc/nginx/nginx.conf` (`internal/render/templates/nginx.conf.tmpl`). nginx comes from the distribution with `libnginx-mod-stream`.

- **Port 80** (`http`): serves `/.well-known/acme-challenge/` from `/var/lib/eru/acme` for HTTP-01, and 404 for everything else.
- **Port 443** (`stream`, `ssl_preread on`): routes by ALPN without terminating TLS on 443 itself.
    - ALPN `postgresql` (PostgreSQL 17+ clients with `sslnegotiation=direct`) from an allowed address goes to `127.0.0.1:6433`, a `stream` server that terminates TLS with `/etc/eru/tls/server.{crt,key}` (`ssl_alpn postgresql`) and forwards the plain protocol to PgBouncer on `127.0.0.1:6432`.
    - Allowed addresses come from a `geo` block: the WireGuard subnet always, plus `client_allowlist` when `expose_pgbouncer = true`, so 443 follows the same rule as 6432. Other PostgreSQL clients go to `127.0.0.1:6434`, which closes the connection.
    - Everything else (HTTP/1.1, h2, no ALPN) goes to the `http` server on `127.0.0.1:8443`, which terminates TLS and answers 404. Phase 6 adds the authenticated `/metrics` locations there (`auth_basic` with a bcrypt `auth_basic_user_file`; each location proxies to the exporter's `/metrics`).
- PgBouncer uses `auth_type = hba` with `/etc/pgbouncer/pgbouncer_hba.conf`: `local` and `hostssl` from anywhere use `scram-sha-256`; `hostnossl` is allowed only from `127.0.0.1/32` (the nginx hop); any other non-TLS connection is rejected. `client_tls_sslmode = prefer`, so the hba file, not the TLS mode, enforces TLS. SCRAM channel binding isn't available through 443, because PgBouncer sees a plain connection there; `channel_binding=require` clients must use 6432.

### 15.3 Certificates (`eru node run certs-renew`)

eru issues and renews the certificate itself with `golang.org/x/crypto/acme`. Nothing else on the server speaks ACME.

- `eru-certs.timer` runs `eru node run certs-renew` hourly (randomized 10 min); `tls-v2` also starts it once right away. The job takes the shared lock with timeout 0 and exits 0 when the lock is busy.
- It does nothing unless `/etc/eru/tls/server.crt` is missing, isn't for `domain`, is the self-signed placeholder in `acme` mode (or not self-signed in `self-signed` mode), or expires within 30 days.
- **acme mode:** first a self-check: write a random token into the webroot and fetch `http://<domain>/.well-known/acme-challenge/<token>`. If another host answers, the job stops with `changed: false, pending: …` (exit 0) so a wrong record doesn't use up Let's Encrypt's failed-validation limit. A connection error is inconclusive and the job carries on. Then: account key `/etc/eru/acme/account.key` (P-256, 0600, created once; `acme_email`, when set, is the account contact), order, HTTP-01 through the webroot, a fresh P-256 leaf key and CSR, and the full chain. `tls.acme_ca` overrides the directory URL (for example Let's Encrypt staging). ACME errors fail the unit with `external_command_failed`; a rate limit says when to retry.
- **self-signed mode:** a self-signed P-256 leaf for `domain`, valid 397 days. Clients use `sslmode=require` (the document's URLs switch accordingly, and doctor warns).
- A new certificate is written as `server.crt` (full chain, 0644) and `server.key` (`root:ssl-cert` 0640), then PostgreSQL (`pg_ctlcluster 18 main reload`), PgBouncer (`systemctl reload pgbouncer`) and nginx (`systemctl reload nginx`) are reloaded.
- `tls.status` reports `pending` while the placeholder is served, `acme` with `not_after` once the issued certificate is in place, and `self-signed` in that mode.

### 15.4 eru textfile metrics (`eru.prom`)

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
| pg_cron | `postgresql-18-cron` | `pg_cron` | `cron.database_name` = first database; PerCluster |
| pgaudit | `postgresql-18-pgaudit` | `pgaudit` | `pgaudit.log='ddl,role'` default |
| pg_repack | `postgresql-18-repack` | | Required for `maint repack` |
| pg_partman | `postgresql-18-partman` | `pg_partman_bgw` (optional) | |
| hypopg | `postgresql-18-hypopg` | | |
| timescaledb | `timescaledb-2-postgresql-18` (Timescale apt repo) | `timescaledb` | Timescale licence; adds a repo; mark `Note` |

At implementation time, verify each package exists for PG18 on every supported OS/arch. Drop any that don't and remove them from this list.

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
| certs-renew | hourly | 10m |
| metrics-refresh | every 5 min | 0 |

`certs-renew` is the only timer in Phase 2. `tls-v2` installs `eru-certs.{service,timer}` itself; `timers-v1` (Phase 6) takes ownership of the `.timer`. Until then every other `node run` job returns `not implemented yet`.

`node run <job>` takes the right lock with `--timeout 0`. If the lock is busy it logs, exits 0 and writes a `skipped` metric. Busy isn't a failure, but repeated skips show up in doctor.

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
- When root on a supported Linux server with a TTY, asks "Initialise a cluster on this server now (`eru cluster init --local`)? [y/N]" on `/dev/tty` (stdin is the piped script) and on yes execs `eru cluster init --local` with the terminal as stdin.
- `ERU_INSTALL_ONLY=1` skips the next-step text and the init question.

### 23.3 Pinned versions (`internal/versions`)

```go
const (
    PostgresMajor = 18
    NginxMinVersion = "1.26"              // distribution nginx with stream ssl_preread + ssl_alpn
    ResticVersion = "0.18.1"              // floor; update
    NodeExporterVersion = "1.11.1"        // floor; update
    PostgresExporterVersion = "…"         // latest stable at implementation
    PgbouncerExporterVersion = "…"
    PgbackrestExporterVersion = "…"
    CosignVersion = "3.1.3"               // install.sh and mise.toml
)
var Checksums = map[string]map[string]string{ /* artifact -> arch -> sha256 */ }
```

PGDG packages (postgresql-18, pgbouncer, pgbackrest, extensions) aren't version-pinned. They're held (`apt-mark hold`) and updated only via `eru pg update`.

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
6. common workflows: status check, backup now plus verify, PITR restore (paused → inspect → promote/abort), add a database, add an extension, run a maintenance report, upgrade;
7. what an agent must never do:
   - print the secrets document or passphrase;
   - run `restore cleanup` or `db drop` without explicit human approval;
   - pass `--sudo` or `--install-missing` without approval.

`eru skill install` writes this as a skill into the requested harness directories, using andurel's `skill.go` harness list and layout.
