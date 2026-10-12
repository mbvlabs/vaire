# eru: self-hosted, production-grade PostgreSQL

> Master document: goal, decisions, architecture, roadmap and **current status** (§7). This is the one place that tracks progress. Implementation detail lives in [`IMPLEMENTATION.md`](./IMPLEMENTATION.md), which carries no status.
> Approved 2026-10-09 · roadmap made linear and testing changed to manual 2026-10-10

## 1. Goal

**One Glorious CLI**: a single signed Go binary, `eru`, that sets up, secures, runs, backs up, restores and maintains a PostgreSQL cluster, and is the only interface to it.

From a laptop (or from an agent), `eru` takes a fresh Debian or Ubuntu VPS, hardens it, installs a tuned and secured PostgreSQL 18 cluster, and connects the operator to it over WireGuard. Once setup is done, eru prints a **secrets handoff**: every secret in plain text for the operator's password manager, plus an encrypted transfer bundle that authenticates any `eru` installation to the cluster. Every later operation goes through the same CLI, over SSH, through WireGuard. That includes backups, restores, maintenance, extensions, adding nodes and running queries.

The CLI is **agent-first**. Every command is non-interactive when asked, emits structured JSON, has documented exit codes and dry-run support, and protects destructive operations. Pointing an agent at `eru` is a supported way to operate a cluster.

The first goal is the **central command** node: one VPS that hosts the primary and owns the cluster's desired state, plus the CLI that drives it. Multiple nodes and read replicas come later and HA comes last, but early decisions must not block them.

### What has to be right first

1. **Backups and restores** to S3-compatible storage (Cloudflare R2, AWS S3, Backblaze B2, MinIO, Hetzner Object Storage), with point-in-time recovery, encryption, verification and a tested path to rebuild on a brand-new server. This covers the **database** (configurable schedule, continuous WAL) and the **VPS** itself (weekly by default).
2. **Settings tuning and security settings**, derived from the hardware and workload and kept under eru's control.

### Non-goals (for now)

- PostgreSQL versions other than 18. The design is version-aware so more can be added later.
- RHEL, Alpine, Windows servers, Kubernetes, containers. Postgres runs on the host from PGDG packages. The operator CLI does run on Linux and macOS.
- A web UI. eru is terminal-first; rich views are TUIs (`eru top`) and everything is scriptable.
- Multi-tenant DBaaS, sharding, and logical multi-master.

## 2. Design principles

- **One binary, two roles.** The same `eru` binary runs on the operator's machine (**operator mode**) and on each server (**node mode**). Operator mode drives node mode over SSH. Host mutations are small, idempotent bash scripts embedded with `go:embed`, the same pattern as `deploycrate-ce/internal/setup`.
- **SSH through WireGuard is the only control channel.** There is no daemon, no HTTP control API and no open control port. After bootstrap, sshd listens only on the WireGuard address.
- **Desired state in, convergence out.** The cluster's desired state is one TOML file on the central command. `eru apply` converges every node to it, and running it again is always safe. `eru plan` shows what would change.
- **Versioned, journaled steps.** Each step has a `Check`/`Apply` pair and a versioned ID (for example `tls-v1`). A journal on each node records what ran, so upgrading eru re-runs only the steps that changed.
- **eru owns the config it writes.** Managed config goes into dedicated files, user overrides go into one documented override file, and `ALTER SYSTEM` is disabled (`allow_alter_system = off`), so nothing drifts silently.
- **Secrets are generated, never asked for, and shown exactly once.** They're printed in the secrets handoff at the end of setup and never written to disk by eru unless asked.
- **Restores are the product.** A backup that has never been restored doesn't count. Restore drills run on a schedule and are reported.
- **Agent-first, human-friendly.** The output is readable in a terminal and exact as JSON, and the interactive prompts are a thin layer over flags.

## 3. Decisions so far

| Topic | Decision |
| --- | --- |
| Name | `eru` |
| Interface | One Glorious CLI, terminal and agent first. No web UI, so the andurel web framework is out of scope (its CLI conventions are reused) |
| Control channel | SSH (in-process, `x/crypto/ssh`) over WireGuard. Tailscale is supported as an alternative transport |
| Backup engine | pgBackRest, with WAL-G as fallback behind a `backup.Driver` interface |
| VPS backup | restic file-level backups, weekly by default and scheduled separately from the database. Provider snapshots later (Phase 7, §6.6) |
| HA (Phase 10, last) | Patroni + etcd. Until then eru runs plain `postgresql-common`, with a designed and tested adoption step into Patroni |
| Client access | eru issues and renews the ACME certificate for `data.example.com` itself; clients connect to `:6432` (PgBouncer) or `:5432` with `sslmode=verify-full`, or to PgBouncer through `:443` with direct TLS (`sslnegotiation=direct`, PG17+ clients). The record must be DNS only, not proxied |
| Metrics | Exporters only, exposed via an authenticated nginx endpoint for external scraping (Grafana Cloud, own Prometheus). No metrics storage on the box |
| Config format | TOML |
| Node networking | WireGuard mesh managed by eru by default; Tailscale as an option |
| Operator WireGuard | Embedded userspace by default; `eru wg export` produces a system config when wanted |
| Public SSH | Closed after bootstrap: SSH only on WireGuard, commit-confirm switchover, break-glass via the provider console |
| Install paths | Both: the operator CLI bootstraps servers remotely (primary), or `eru cluster init --local` runs directly on a server installed with `curl \| sudo bash` and hands off with a secrets document |
| Secrets handoff | Printed once in the terminal: plain secrets for a password manager, plus an armored age transfer bundle with a passphrase eru generates. No file unless `--secrets-out` |
| Agent integration | First: CLI + `--json` + `eru guide agents` / `eru skill install`. `eru mcp` comes later |
| CLI conventions | Modelled on the andurel CLI's agent support (§5.4) |
| Testing | Manual testing on real VPSes from Phase 2 on; CI runs lint and builds only. No unit tests (§8) |
| Release site | `https://eru.mbvlabs.com`. Stable lives at the root, edge under `/edge/`, and every version is kept under `/releases/<version>/`. Edge publishes on every push to `master`, stable on `vX.Y.Z` tags. Releases are signed with cosign (pinned 3.1.3) |
| Platforms | The CLI runs on Linux and macOS. Node-side commands are Linux-only and are marked that way in `eru commands --json` |
| Self-upgrade | `eru upgrade` keeps the previous binary next to it as `eru.prev`, and refuses to replace a binary installed by Homebrew or Nix |
| Risky flags | A command can mark a single flag as changing or destroying something (for example `queries --reset`), so agents don't treat the plain read as dangerous |
| `eru top` | Reads Postgres directly over the WireGuard tunnel rather than through the node |
| Database ports on WireGuard | WireGuard peers can always reach 6432 and 5432. `expose_pgbouncer` / `expose_postgres` only control the public allowlist rules |
| Generated secrets | Every secret eru generates goes into the secrets document, including the `pgbouncer_auth` and `eru_monitor` service passwords. The operator generates all database passwords; the node keeps only SCRAM verifiers, plus the two service passwords that PgBouncer and the exporters need at runtime |
| DNS and certificates | In `acme` mode, `cluster init` shows the DNS records to create, waits for the operator to confirm them, and checks them against public resolvers. TLS never blocks on ACME: a placeholder certificate lets PostgreSQL and PgBouncer start, and a certificate that's still pending after 120 s is a warning, not a failure |

**Still to decide** (when the phase that needs it starts):

- Which exporter versions to pin, and whether eru's own metrics replace `pgbackrest_exporter` (Phase 6).
- Where the table-bloat query comes from (Phase 6).
- Which catalog extensions to drop because they have no PostgreSQL 18 package (Phase 8).

## 4. Technology choices

| Concern | Choice | Notes |
| --- | --- | --- |
| Binary / CLI | Go, `cobra` for commands, Charm `huh` for interactive prompts, `bubbletea` + `lipgloss` for TUIs (`eru top`, progress) | Same Charm stack as deploycrate |
| Transport | `golang.org/x/crypto/ssh` in-process; embedded userspace WireGuard (`wireguard-go` + netstack) in operator mode | Operator needs no root, no `wg` interface and no system SSH config |
| Distribution | `install.sh` + cosign-signed manifest + sha256 for linux/darwin × amd64/arm64 | Copy of `deploycrate-ce/scripts/install.sh` and `build-release.sh`. Operator mode pushes the matching verified linux binary to nodes |
| Transfer bundle | ASCII-armored `age` passphrase-encrypted bundle (`filippo.io/age`, scrypt) | §5.3 |
| Server OS | The latest Debian and Ubuntu LTS only: Debian 13, Ubuntu 26.04 | Preflight refuses anything else, including older releases. PGDG ships `postgresql-18` for both on amd64 and arm64 (checked 2026-10-11) |
| PostgreSQL | PGDG apt repo (`apt.postgresql.org`), `postgresql-18`, managed via `postgresql-common` (`pg_createcluster`) | Data checksums are on by default in 18. Major version pinned with an apt hold |
| DB backups | **pgBackRest** (S3 repo, PITR, full/diff/incr + block incremental, AES-256 repo encryption, async WAL archive, `verify`) | WAL-G fallback behind the interface |
| VPS backups | **restic** to the same bucket (separate prefix, own encryption key) | Same tool deploycrate pins |
| Logical exports | `pg_dump -Fc` to the bucket or locally, per database | For portability and single-database restores, not DR |
| Pooling | **PgBouncer** (transaction mode, `max_prepared_statements`, `auth_query`, TLS on both sides) | Clients connect to PgBouncer, which reaches Postgres over the unix socket |
| TLS & metrics edge | **nginx** (distribution package): HTTP-01 challenges on 80; on 443 `stream` + `ssl_preread` routes ALPN `postgresql` to PgBouncer and everything else to HTTPS (authenticated `/metrics` later). Certificates come from eru's own ACME client (`x/crypto/acme`) | Only direct-TLS clients on 443 go through nginx; 6432 and 5432 are direct |
| Metrics | `node_exporter`, `postgres_exporter`, `pgbouncer_exporter`, `pgbackrest_exporter`; `pg_stat_statements` on by default | Localhost-bound, federated through nginx |
| DB access from CLI | `pgx` over the WireGuard tunnel | `eru sql`, `eru psql`, `eru console` (usql), `eru tunnel` |
| HA (Phase 10) | Patroni + etcd (3 voters), HAProxy health-routing on the Patroni REST API | |
| Scheduling | systemd timers on each node calling `eru node …` subcommands | No long-running eru process anywhere |

## 5. Architecture

### 5.1 The control model

```
 operator laptop / CI / agent                         VPS: central command
┌──────────────────────────────┐                     ┌──────────────────────────────────────┐
│ eru (operator mode)          │                     │ sshd  ── listens on wg0 only         │
│  ├ context: cluster "prod"   │   WireGuard (UDP)   │ wg0 10.42.0.1                        │
│  │  (from secrets document)  │════════════════════▶│                                      │
│  ├ userspace WireGuard       │   SSH inside WG     │ eru (node mode), invoked per command │
│  ├ in-process SSH client  ───┼────────────────────▶│  ├ steps + embedded scripts          │
│  └ pgx over the tunnel    ───┼────────────────────▶│  ├ journal, /etc/eru/cluster.toml    │
└──────────────────────────────┘   (5432 via wg0)    │  └ systemd timers → eru node …       │
                                                     │                                      │
                                                     │ PostgreSQL 18 · PgBouncer · nginx    │
                                                     │ pgBackRest · restic · exporters      │
                                                     └──────────────────────────────────────┘
```

- **Operator mode** (`eru <command>` on a laptop) resolves the cluster context, brings up an in-process WireGuard tunnel, opens SSH to the right node, and runs `eru node <subcommand> --json` there. It streams progress back and renders it, or passes it through as JSON.
- **Node mode** (`eru node …`, only on servers) does the actual work as root via sudo: steps, backups, restores, maintenance. It's also what the systemd timers call. A human can SSH in and run it by hand.
- **Version lockstep.** Before running anything, operator mode checks the node's eru version. If it differs, operator mode refuses to continue and tells the user to run `eru upgrade`, unless `--auto-upgrade-nodes` is set or configured on the context. One CLI version means one behaviour.
- **Upgrading nodes from the operator.** Node binaries are always upgraded from outside the VPS, through the operator CLI:
  - `eru upgrade` self-updates the local CLI (signed), then upgrades every node.
  - `eru node upgrade [--node <name>|--all] [--version <v>]` upgrades nodes only, for example to roll back or to pin a version.
  - Each node upgrade downloads the matching linux build, verifies its cosign signature and sha256 against the manifest, and swaps `/usr/local/bin/eru` atomically, keeping the previous binary. It runs `eru node selfcheck`, then `eru apply` re-runs only the steps whose versioned IDs changed.
  - Upgrades wait for running timers (backups, maintenance) to finish. In HA (Phase 10) they roll one node at a time, replicas first.
  - `eru node upgrade --rollback` restores the previous binary.
- **State.** The desired state (`/etc/eru/cluster.toml`) and journal live on the central command, and a copy is pushed to the bucket after every `apply`. The operator machine holds only credentials, in its contexts. Any machine with the transfer bundle and its passphrase can take over operation.

### 5.2 Bootstrap: from a fresh VPS to WireGuard-only SSH

0. **Local preflight** on the operator machine (§5.2.1).
1. `eru cluster init --host root@203.0.113.10` (or interactively, `eru cluster init`). The first contact goes over public SSH with the provider-supplied key or password.
2. eru pushes the node binary, runs preflight, and applies the hardening and WireGuard steps. The server becomes `10.42.0.1` and the operator gets a peer identity.
3. **Commit-confirm switchover.** eru verifies that SSH works over WireGuard, then moves sshd to the wg0 address and closes public 22. A server-side timer reverts the firewall and sshd change unless eru confirms within 2 minutes, so a broken tunnel can't lock the operator out.
4. The rest of the setup (Postgres, backups and so on) runs over WireGuard.
5. eru prints the **secrets handoff**, makes the operator confirm it's stored safely, and wipes the plaintext copies it held server-side, except for what services need at runtime.

Break-glass is the provider console plus `eru node unlock-ssh`, which re-opens public SSH for a fixed window. This is documented in the secrets document itself.

#### 5.2.1 Local preflight and privilege escalation

Before any command that needs them, eru checks the machine it runs on and reports what's missing as a structured `missing_tool` error, or as a prompt in a TTY:

- **Operator machine:** reachability of the target, an SSH key to use for first contact (or a password), and optional tools by feature: `wireguard-tools` when a system WireGuard interface is wanted (`eru wg export --install`), `psql` for `eru psql`, and `tailscale` when that transport is chosen. The embedded userspace WireGuard needs nothing installed.
- **Install offer:** eru shows the exact package command for the platform (`apt`, `dnf`, `brew`) and asks whether it may run it. Without a TTY it never installs; it fails with the command to run, or installs only when `--install-missing` is passed.
- **Sudo escalation:** when a step needs root locally (system WireGuard interface, `eru cluster init --local`), eru asks *"eru needs root to <reason>. Escalate with sudo? [y/N]"*, then re-executes the specific subcommand through `sudo`. It doesn't keep a root shell, and only that step runs privileged. `--sudo` pre-approves this for agents and scripts, and `sudo -n` is tried first so passwordless sudo works without a prompt.
- **`eru cluster init --local`** (on a server installed via `curl | sudo bash`): checks it's root or can escalate, checks the OS, runs the same steps locally, then switches SSH to WireGuard. The secrets handoff is printed on the server console (nothing is written to `/root`), and the operator pastes its transfer bundle into `eru login` on their machine. From that point on, operation is identical to a remote bootstrap.

### 5.3 Secrets handoff and contexts

At the end of setup eru prints the **secrets handoff** in the terminal and writes no file unless `--secrets-out` asks for one. It has two parts: every secret in plain text, grouped for a password manager, and a **transfer bundle**, an ASCII-armored `age` passphrase-encrypted block holding the same content. eru generates the passphrase and shows it once, and it's never stored by eru. The content is:

- Cluster identity: name, node list, WireGuard endpoints and public keys, server SSH host keys (pinned)
- Operator credentials: WireGuard private key and address, SSH private key, admin DB role password
- Database credentials: owner and app roles, connection strings (`data.example.com:6432`, `verify-full`)
- Backup **recovery kit**: bucket endpoint and keys, pgBackRest stanza and repo cipher passphrase, restic repository password
- Metrics endpoint credentials
- Break-glass instructions

```
eru login                               # paste the transfer bundle → create local context "prod"
eru context list | use <name> | remove <name>
eru secrets export [--rotate-operator]  # show a new handoff, optionally with a new operator identity
eru operator add <name>                 # new WireGuard peer + SSH key → separate document for a teammate/agent
eru operator revoke <name>
```

Losing the document doesn't mean losing the cluster as long as console access exists, but losing the **recovery kit** means the backups can't be read. eru won't finish `init` until the user confirms the document is stored, and `eru doctor` keeps reminding them.

Per-operator identities let an agent get its own scoped document, for example a **read-only operator** whose SSH certificate and DB role only allow read commands (Phase 7).

### 5.4 Agent-first CLI contract

The contract is modelled on the **andurel CLI** (`andurel/cli`, `cli/output`), which already has strong agent support. The patterns to reuse:

- **Output modes** via a persistent `--output human|json|agent|markdown` flag, plus `--json` as shorthand, `--jq` filtering, `--ids-only`, `--count` and `--quiet`.
- **Structured errors** with stable string codes (`usage_error`, `missing_tool`, `unsafe_action_requires_confirmation`, `config_error`, `ambiguous_input`, `update_required`, …) mapped to stable exit codes. eru adds codes such as `remote_unreachable`, `node_version_mismatch`, `precondition_failed` and `partial_failure`.
- **Per-command `CommandMeta`**: summary, `when_to_use`, `when_not_to_use`, prerequisites, `next`, examples, `mutating`, `destructive`, `supports_dry_run`, `prompts` and `interactive_flags`. A check fails the build when a command lacks metadata.
- **Discovery**: `eru commands --json` emits the full catalog.
- **Skill install**: `eru skill install` writes the eru agent skill into supported agent harnesses.
- **Prompts**: `requireFlagInStructuredMode` and `confirmDestructiveAction` helpers so that structured mode never prompts.

On top of that:

- **Every command supports structured output** and emits one JSON object, or NDJSON events with `--stream` for long operations. The schema is versioned (`"schema": "eru/v1"`). Human output is a rendering of the same data.
- **Non-interactive by default when there's no TTY.** Every prompt has a flag. When a required value is missing, eru exits with a machine-readable error saying which flag to pass.
- **`--dry-run` / `eru plan`** on every mutating command.
- **Destructive operations need an explicit confirmation token** that names the target: `--confirm prod/restore` or `--confirm <db-name>`. Passing `--yes` alone isn't enough, which protects against agents too.
- **Self-description.** `eru commands --json` (above), and `eru guide agents` prints the usage guide that `eru skill install` installs.
- **Read helpers** for agents: `eru sql --json "select …"` (read-only role by default; `--write` needs a write-capable context), `eru status --json`, `eru maint report … --json`.
- An MCP server mode (`eru mcp`) is planned for a later phase, built on the same command tree.

### 5.5 Phases 9–10: read replicas and HA

```
                         clients
                            │ data.example.com (rw)  ·  read.data.example.com (ro)
            ┌───────────────┼──────────────────┐
            ▼               ▼                  ▼
   ┌──────────────┐ ┌──────────────┐  ┌──────────────┐      ┌───────────────┐
   │ node-1       │ │ node-2       │  │ node-3       │      │ replica-ams   │
   │ PgBouncer    │ │ PgBouncer    │  │ etcd witness │      │ async, region │
   │ HAProxy      │ │ HAProxy      │  │ (or standby) │      │ bootstraps    │
   │ Patroni+etcd │ │ Patroni+etcd │  │              │      │ from bucket   │
   │ PG primary   │ │ PG sync stby │  │              │      │               │
   └──────┬───────┘ └──────┬───────┘  └──────┬───────┘      └──────┬────────┘
          └────── WireGuard mesh 10.42.0.0/16 (operators are peers) ──┘
                              │
                     pgBackRest → R2/S3 (from primary, or from standby)
```

- **Adding a node** is `eru node add --host root@… --role standby|replica|witness [--region ams]`. It uses the same bootstrap as §5.2, joins the mesh, and gets the cluster config pushed to it. No join tokens or agents are needed, because the operator CLI orchestrates every node over SSH.
- **Central command is a role, not the primary.** Once failover exists, the primary moves. The desired state and journal are replicated to every node and the bucket, and any node can serve as the state holder. The `state.Store` interface exists from Phase 2.
- HA needs **3 voters**. Two nodes plus a tiny etcd witness is the minimum safe topology, and eru refuses a 2-voter HA setup.
- **rw/ro endpoint routing** has options: DNS A records over all nodes plus local HAProxy routing to `/primary` or `/replica`; libpq multi-host strings with `target_session_attrs`; or a Patroni callback that updates DNS. This is decided in the Phase 10 design.
- With **Tailscale** as the transport, nodes and operators use the tailnet instead of eru's mesh (embedded via `tsnet` in operator mode). Everything else is the same.

### 5.6 Code layout

```
cmd/eru/                     entrypoint (operator + node mode)
internal/cli/                cobra tree, CommandMeta, flag↔prompt layer, output modes, error codes (after andurel/cli)
internal/transport/          userspace WireGuard, Tailscale (tsnet), SSH client, host-key pinning
internal/localenv/           local preflight, package-install offers, sudo escalation
internal/upgrade/            self-update, node upgrade/rollback
internal/release/            release manifest fetch, sigstore verification, download cache
internal/rpc/                eru-rpc/v1 request types
internal/node/               node-side RPC dispatcher, selfcheck, upgrade.install
internal/secrets/            secret registry and redaction
internal/versions/           pinned external versions and checksums
internal/buildinfo/          version, commit, channel, release base URL
internal/guide/              embedded agent guide
internal/remote/             push binary, run `eru node …`, stream NDJSON events
internal/secretsdoc/         age bundle, passphrase, break-glass text, document → context
internal/contexts/           ~/.config/eru: contexts, current context, pending init state
internal/engine/             step engine: Check/Apply, plan, journal, versioned IDs, commit-confirm
internal/steps/              step definitions
internal/steps/scripts/*.sh  embedded host scripts (go:embed)
internal/config/             cluster.toml schema, validation, defaults, migrations
internal/hostinfo/           CPU/RAM/disk/kernel/OS detection
internal/tune/               pure tuning calculator
internal/render/             conf.d, pg_hba, pg_ident, pgbouncer.ini, pgbackrest.conf, restic, nginx.conf
internal/backup/             Driver interface, pgbackrest impl, restic host backups
internal/maint/              maintenance tasks + reports
internal/catalog/            extension catalog
internal/pg/                 pgx admin client
internal/state/              Store interface (central-command file + bucket copy now, replicated later)
scripts/install.sh, build-release.sh, sign-release.sh, upload-release.sh
schema/eru-v1.json           agent JSON contract
```

### 5.7 Files on a node

| Path | Owner / mode | Purpose |
| --- | --- | --- |
| `/usr/local/bin/eru` (+ `eru.prev`) | root 0755 | Node binary, version-locked to the operator; previous version kept for rollback |
| `/etc/eru/cluster.toml` | root 0640 | Desired state, no secrets (central command; replicated in Phase 10) |
| `/etc/eru/runtime/` | root 0700 | Only the secrets services need at runtime (bucket keys, cipher pass, pgbouncer auth) |
| `/var/lib/eru/journal.json` | root 0600 | Applied step IDs, versions, timestamps |
| `/etc/postgresql/18/main/conf.d/10-eru-tuning.conf` | managed | Tuning |
| `/etc/postgresql/18/main/conf.d/20-eru-security.conf` | managed | TLS, auth, logging |
| `/etc/postgresql/18/main/conf.d/30-eru-extensions.conf` | managed | `shared_preload_libraries` and extension settings |
| `/etc/postgresql/18/main/conf.d/90-overrides.conf` | user | The only hand-edited file; `eru doctor` lists what it overrides |
| `/var/log/eru/` | | Per-run logs for apply, backup, maintenance and restore drills |

## 6. The single-node central command in detail (Phases 1–6)

### 6.1 `eru cluster init`

1. **Preflight** (remote): OS/version, arch, RAM ≥ 1 GB, disk, no existing Postgres, cloud-init finished, clock in sync.
2. **Mode**: *New cluster* or *Restore from backup*. The second uses a secrets document or recovery kit to rebuild a dead server on a fresh VPS (§6.5).
3. **Questions**, with every answer defaulted and each also available as a flag or `--answers answers.toml`:
   - Server: cluster name, hostname, admin user, timezone (default UTC), data directory (root disk or attached volume), transport (WireGuard / Tailscale)
   - Endpoint: domain (`data.example.com`), ACME email, client CIDR allowlist for 6432/5432 (default: deny all public; WireGuard peers allowed)
   - Workload profile (web/OLTP, mixed, analytics), expected client connections
   - Initial database, owner role and app role
   - Backups: provider preset (R2/S3/B2/Hetzner/MinIO/custom), endpoint, region, bucket, prefix, keys
   - DB backup schedule and retention (presets plus a custom cron-style schedule)
   - VPS backup: on/off, schedule (default weekly) and retention, asked separately
   - Extensions from the catalog (§6.7)
4. **Plan**, then confirm with `--confirm <cluster>` when non-interactive.
5. **Apply** the steps (§6.2) with live progress. Each step is logged and resumable.
6. **Handoff**: write the secrets document, gate on "stored safely", then print connection info, the first DB and VPS backup results, and an `eru doctor` summary.

### 6.2 Step catalogue

| # | Step | Adapted from deploycrate |
| --- | --- | --- |
| 1 | Base packages, cloud-init wait, apt hygiene | `packages.sh` |
| 2 | Admin user, sudo, operator SSH key | `user.sh` |
| 3 | WireGuard interface + operator peer (or Tailscale join) | `wireguard.sh` |
| 4 | sshd hardening (no root, no passwords, `AllowUsers`, `MaxAuthTries`) + **commit-confirm move to wg0** | `ssh.sh` |
| 5 | ufw (WG UDP, 443, 6432/5432 to the allowlist and WireGuard), fail2ban (sshd plus a postgres auth-failure jail) | `ssh.sh`, `host.sh` |
| 6 | journald limits, swap (small), unattended-upgrades for security updates only, with Postgres packages excluded | `host.sh` |
| 7 | Kernel/sysctl for databases: `vm.swappiness=1`, `vm.overcommit_memory=2` + ratio, dirty bytes, THP off, huge pages sized to `shared_buffers`, limits | new |
| 8 | PGDG repo + signing key, `postgresql-18`, `postgresql-contrib`, `pgbouncer`, apt hold on major (`pgbackrest` arrives with step 14) | new |
| 9 | Cluster create (`pg_createcluster 18 main`, data dir, locale `C.UTF-8`, ICU, checksums) | new |
| 10 | nginx install, placeholder certificate, and the hourly `eru-certs` timer that issues and renews the ACME certificate (eru's own client) and reloads Postgres, PgBouncer and nginx | new |
| 11 | Render managed conf.d, pg_hba and pg_ident; restart or reload depending on the diff | new |
| 12 | Roles and databases (§6.3) | new |
| 13 | PgBouncer config, `auth_query` function, TLS | new |
| 14 | pgBackRest: stanza create, `archive_mode`, check, first full backup | `backup.sh` (pattern) |
| 15 | restic: VPS backup repo, include/exclude sets, first snapshot | `backup.sh` |
| 16 | Exporters and the authenticated `/metrics` behind nginx | `node-exporter.sh` |
| 17 | systemd timers: DB backups, VPS backups, verify, restore drill, maintenance, reports | `host.sh` (timer pattern) |
| 18 | `eru doctor` as the final gate, then generate the secrets document | new |

### 6.3 Tuning and security settings

**Tuning** comes from `internal/tune`, a pure function of (hardware, workload profile, connection target, PG version) that returns settings plus a reason for each value. It is shown by `eru tune explain`.

- Memory: `shared_buffers` ≈ 25% RAM, `effective_cache_size` ≈ 70–75%, `work_mem` from (RAM − shared_buffers) / (connections × profile factor), `maintenance_work_mem` / `autovacuum_work_mem` capped, `huge_pages = try` with the OS pre-sized.
- Connections: Postgres `max_connections` stays modest (≈ 4× vCPU, floor 50) because PgBouncer fans in. Pool sizes are derived from it.
- WAL / checkpoints: `wal_compression = zstd`, `min_wal_size` / `max_wal_size` from disk size, `checkpoint_timeout = 15min`, `checkpoint_completion_target = 0.9`.
- I/O (PG18): `io_method` (`worker` by default, `io_uring` when the kernel and build support it), `effective_io_concurrency` / `maintenance_io_concurrency` for SSD/NVMe, `random_page_cost = 1.1`.
- Parallelism: `max_worker_processes`, `max_parallel_workers(_per_gather)` from vCPU and profile. JIT is off for OLTP.
- Autovacuum: more aggressive scale factors, `autovacuum_vacuum_max_threshold` (new in 18), `autovacuum_worker_slots`, cost limits sized to the disk.
- Re-running `eru tune` after a VPS resize recalculates and applies, flagging settings that need a restart.

**Security** settings:

- TLS everywhere: `ssl = on`, `ssl_min_protocol_version = TLSv1.2` (1.3 optional), `hostssl` only in pg_hba for non-local, clients told to use `sslmode=verify-full`. PG18 `sslnegotiation=direct` is supported.
- Auth: `password_encryption = scram-sha-256`, no md5 or trust over TCP, `peer` for local admin via pg_ident, and a terminal `reject` rule.
- Network: public 5432/6432 is closed unless the allowlist opens it. Operators and agents reach the DB over WireGuard.
- Roles: `postgres` superuser is local-socket only. A separate admin role (CREATEDB/CREATEROLE, not superuser) for operators. Per-database owner and app roles, a read-only role (the default for `eru sql`), `eru_monitor` (`pg_monitor`), `eru_backup`, `replicator` (Phase 9). `pgbouncer_auth` gets only the `auth_query` SECURITY DEFINER function.
- Guardrails: `idle_in_transaction_session_timeout`, per-role `statement_timeout` defaults, `allow_alter_system = off`, `REVOKE` on `public` schema / database `CONNECT` from `PUBLIC`.
- Logging: PG18 `log_connections` aspects, `log_disconnections`, `log_lock_waits`, `log_temp_files`, `log_checkpoints`, `log_autovacuum_min_duration`, `log_min_duration_statement` per profile. Optional `pgaudit`.
- Host: SSH only on wg0, fail2ban jail on Postgres auth failures, systemd sandboxing overrides where safe.
- `eru doctor --security` runs a CIS-style audit with pass/fail and fixes.

### 6.4 nginx, certificates and `data.example.com`

- **Certificates:** eru gets and renews the certificate for `data.example.com` itself (`eru node run certs-renew`, hourly), over HTTP-01 on port 80 served by nginx. It installs the certificate for Postgres, PgBouncer and nginx and reloads them, so clients connect to `data.example.com:6432` with `sslmode=verify-full`.
- **Port 443:** nginx's `stream` module reads the TLS ClientHello (`ssl_preread`). PostgreSQL 17+ clients with `sslnegotiation=direct` send ALPN `postgresql`; nginx terminates their TLS and passes them to PgBouncer over loopback, under the same allowlist as 6432. This helps on networks that only allow 443. Everything else on 443 is HTTPS, where Phase 6 serves the authenticated `/metrics` endpoint.
- **DNS:** the record must point straight at the server ("DNS only" in Cloudflare). A proxied record ends the connection at the proxy, which only passes HTTP.

### 6.5 Database backups and restores

**Model (pgBackRest):**

- Continuous WAL archiving (`archive-async`) to `repo1` = S3/R2, with `repo1-cipher-type=aes-256-cbc`. Optional `repo2` = local disk for fast restores.
- Default schedule: weekly full, daily diff, optional hourly incr. Block incremental is enabled. Retention defaults to 4 full backups, giving about 4 weeks of PITR.
- Provider presets set the right options, for example path-style URIs and region `auto` for R2.
- **Per-database backup policies:** a database can have its own schedule and retention in `cluster.toml`, overriding the cluster default.

**Commands** (operator mode; each runs remotely):

```
eru backup now [--type full|diff|incr]
eru backup list                           # sets, sizes, WAL range, PITR window
eru backup verify                         # pgbackrest verify + checksum report
eru restore --latest | --time "2026-10-09 12:00:00+00" | --set <label> [--db <name>] --confirm <cluster>/restore
eru restore drill                         # restore to scratch dir on alt port, sanity-check, tear down
eru cluster init --restore bundle.txt --host root@<new-vps>   # full DR on a fresh VPS
eru dump <db> [--to bucket|local]         # logical export, streamed to the laptop if local
```

**Safety rules:**

- An in-place restore never deletes the current data directory. It's moved aside with a timestamp and kept until the user runs `eru restore cleanup`.
- Every restore reports the exact recovery target and ends paused at it (`recovery_target_action = pause`) so the user can inspect before promoting. `--promote` skips the pause.
- Single-database restore: restore the cluster to a scratch instance on an alternate port, `pg_dump` the database, and load it into the live cluster under a new name. No overwrite without a flag.

**Verification:** weekly `verify`, a monthly automated **restore drill** whose result goes into metrics and `doctor`, and metrics for last-backup age and WAL archive lag.

### 6.6 VPS backups

The database is covered by pgBackRest. The VPS backup covers everything else on the host that matters for a rebuild or that eru doesn't generate from `cluster.toml`:

- **What:** `/etc` (including eru with its certificates and ACME account, WireGuard, sshd, nginx and PgBouncer config), `/var/lib/eru`, `/home` and `/root`, and user-declared extra paths in `cluster.toml`.
- **Excluded:** the PGDATA directory, WAL, pgBackRest local repo, caches, and `/var/log`. Logs are optional.
- **How:** restic to the same bucket under a separate prefix with its own repository password, which is stored in the recovery kit.
- **Schedule:** independent of the database schedule. The default is **weekly** (Sunday, staggered away from the full DB backup), with `forget --prune` retention of 8 weekly and 6 monthly, and a monthly `restic check`. The schedule is configurable in `cluster.toml` (`[vps_backup] schedule = "weekly"`), and `eru vps backup now` runs one on demand, for example before risky changes. `eru upgrade` and `eru pg update` take one automatically.
- **Restore:** `eru vps backup list`, `eru vps restore --snapshot <id> [--path …] [--to <dir>]`. During DR, `cluster init --restore` restores the VPS snapshot first (identity, certificates, extra files), then re-applies the steps from `cluster.toml`, then restores the database with pgBackRest.
- **Provider snapshots** (Hetzner, DigitalOcean, Vultr APIs) as an extra layer are planned for Phase 7.

### 6.7 Metrics

- Exporters bind to localhost only. nginx exposes one authenticated `/metrics` endpoint (federated) for external scrapers. The credentials are in the secrets document.
- `pg_stat_statements` and `auto_explain` (sampled, threshold-based) are on by default.
- CLI views: `eru status` (role, uptime, connections, cache hit, replication, last DB and VPS backup, PITR window, disk, certificate expiry), `eru top` (live TUI of activity, locks and slow queries), `eru queries` (top statements). All support `--json`.

### 6.8 Extensions

`internal/catalog` holds a curated list. Each entry has the PGDG package name, `shared_preload_libraries` needs, required settings, whether a restart is needed, and a trust note.

Initial catalog: `pg_stat_statements`, `pgvector`, `postgis`, `pg_cron`, `pgaudit`, `pg_repack`, `pg_partman`, `hypopg`, `pg_trgm`, `citext`, `btree_gin`/`btree_gist`, `timescaledb` (licence note).

```
eru ext list | add <name> [--db <db>] | remove <name> [--db <db>]
```

Install is: apt package → render preload/settings → restart if required (needs `--confirm`) → `CREATE EXTENSION` in the chosen databases. The init questions use the same path.

### 6.9 Maintenance

**On demand:**

```
eru maint vacuum   [--db] [--table] [--analyze] [--freeze]
eru maint analyze  [--db]
eru maint reindex  [--db] [--table|--index] --concurrently
eru maint repack   [--db] [--table]          # pg_repack, online "VACUUM FULL"
eru maint report   bloat | wraparound | unused-indexes | missing-indexes | long-queries | locks
eru maint kill     --idle-in-tx | --pid <pid>
```

**Scheduled with systemd timers on the node,** within a configurable maintenance window:

- Daily: wraparound/age check and an `ANALYZE` of tables with stale stats.
- Weekly: bloat report, plus opt-in auto-repack above a threshold.

All maintenance sets `lock_timeout` and refuses to run during a backup or a restore drill.

### 6.10 Day-to-day commands

- `eru apply` / `eru plan`: converge nodes to `cluster.toml`.
- `eru config edit`: pull `cluster.toml`, open `$EDITOR`, validate, plan, push.
- `eru doctor`: drift, security, backups, disk, certificates, versions.
- `eru sql`, `eru psql` (system psql through a local forward), `eru console` (usql; eru downloads a pinned, checksum-verified copy if needed and does the verify-full TLS itself), `eru tunnel` (forward `localhost:5432` to the cluster for GUI tools).
- `eru ssh [node]`: interactive shell over WireGuard.
- `eru upgrade` / `eru node upgrade [--rollback]`: upgrade the CLI and nodes from the operator machine (§5.1).
- `eru pg update`: controlled minor upgrade (apt unhold → upgrade → restart in window → re-hold), with a pre-upgrade backup.

## 7. Roadmap and status

This section is the only place that tracks progress. Update it whenever a module or phase changes state.

**Rules**

1. **One phase at a time.** A phase starts only when the previous one is done, and there's no overlap. "Done" means its exit check passes.
2. **Each phase is a set of modules.** A module is a Go package, or a group of steps, with one clear job.
3. **Each phase leaves something that works.** From Phase 2 on, every phase ends with a manual test on a real VPS (§8).
4. **HA is last.** Extensions come late. Anything unscheduled goes in the backlog.

Legend: ✅ done · 🟡 in progress · ⬜ not started

### 7.1 Current state (2026-10-12)

- **Phase 0 is done.** The CLI, releases and the node engine are on `master`, the first edge release has been published, and the incus test suite has been removed.
- **Phase 1 (Bootstrap) works on a real VPS.** Several manual runs (2026-10-12, Hetzner, Ubuntu 26.04) went from a fresh server to public 22 closed and SSH over WireGuard, and the secrets handoff was accepted by `eru login`. It turns ✅ together with Phase 2 once the exit checks are recorded.
- **Phase 2 (First cluster) works end to end on a real VPS; the exit check is being finished.** `cluster init` completes, the ACME certificate is issued, `eru sql` connects with verify-full, and `eru apply --auto-upgrade-nodes` upgrades the node. Fixes from each manual run are in [`PHASE2-FINDINGS.md`](./PHASE2-FINDINGS.md). Still to record: a no-op `eru apply`, an app connection to 6432 with `sslmode=verify-full`, `db drop` refused with only `--yes`, and the date, version and VPS of the passing run. `eru console` (usql through an eru-side TLS proxy) was added after the runs and hasn't been tried on the VPS yet.
- One open item from Phase 0: install from the edge channel once on Linux and once on macOS, then run `eru upgrade` against a second edge release.
- About 30 of the ~65 commands do real work today: `version`, `commands`, `guide agents`, `skill install`, `upgrade`, `login`, `context list|show|use|remove`, `secrets export`, `cluster init` (remote and `--local`, `--resume`), `plan`, `apply` (steps 1–13), `config show|get|set|edit|validate`, `db list|create|drop|credentials`, `sql`, `psql`, `console`, `tunnel`, `ssh`, `logs`, `tune show|explain|apply`, `node rpc`, `node version`, `node selfcheck`, `node run certs-renew`, `node ssh-revert` and `node unlock-ssh`. The rest return `not implemented yet` until their phase lands.

### 7.2 Overview

| # | Phase | What you can do at the end | Status |
| --- | --- | --- | --- |
| 0 | Foundations | Install and self-upgrade the `eru` CLI; a node can run steps | ✅ |
| 1 | Bootstrap | Turn a fresh VPS into a hardened, WireGuard-only host and get a secrets document | 🟡 works on a VPS; exit check being recorded |
| 2 | First cluster | Set up a new server with a working PostgreSQL 18 cluster. **Manual testing starts here** | 🟡 works on a VPS; exit check being recorded |
| 3 | Database backups & restore | Back up to S3/R2, PITR restore, drills, per-database backup policies | ⬜ |
| 4 | VPS backups & disaster recovery | Rebuild a dead server on a fresh VPS from the secrets document | ⬜ |
| 5 | Upgrades | Upgrade and roll back node binaries and Postgres minors from the laptop | ⬜ |
| 6 | Observability & maintenance | `status`, `top`, `doctor`, metrics, maintenance commands and timers | ⬜ |
| 7 | Operators & agents | Teammate and agent identities, read-only agents, full agent guide, alerting | ⬜ |
| 8 | Extensions | Extension catalog and `eru ext` | ⬜ |
| 9 | Multi-node & read replicas | Add nodes over a WireGuard mesh; regional read replicas | ⬜ |
| 10 | High availability | Patroni + etcd, automatic failover | ⬜ last |

[`IMPLEMENTATION.md`](./IMPLEMENTATION.md) specifies Phases 0–6 in detail, plus the extension catalog (Phase 8) and the agent guide (Phase 7). Phases 7–10 get their own design before they start.

### 7.3 Phase 0 — Foundations ✅

The CLI contract, releases and the node-side engine. No server work.

| Module | What it does | Status |
| --- | --- | --- |
| `internal/cli`, `cli/output` | Cobra tree, `CommandMeta` + `--check-meta`, output modes, error/exit codes, `commands --json`, `skill install`, `guide agents` (stub text) | ✅ |
| `internal/release`, `scripts/`, publish workflows | Build, sign, upload, `install.sh`, sigstore verification, edge + stable channels | ✅ edge published |
| `internal/upgrade` (local) | `eru upgrade` for the local CLI, keeping `.prev` | ✅ |
| `internal/localenv` (escalation) | Sudo re-exec | ✅ |
| `internal/engine` | Steps, plan/apply runner, journal, locks, run logs, shell, redaction | ✅ |
| `internal/node`, `internal/rpc` | `eru node rpc` dispatcher with version check, `selfcheck`, `upgrade.install` | ✅ |

### 7.4 Phase 1 — Bootstrap 🟡

From a laptop, take a fresh Debian/Ubuntu VPS to "hardened, SSH only over WireGuard, secrets document in hand".

🟡 below means code complete and linted, but not yet run on a VPS. Each module turns ✅ when the exit check passes.

| Module | What it does | Status |
| --- | --- | --- |
| `internal/transport` | SSH client with TOFU host-key pinning; embedded userspace WireGuard | 🟡 SSH and pinning tried against a local sshd; WireGuard untested |
| `internal/remote` | Binary push, RPC client, version handshake, `--auto-upgrade-nodes` | 🟡 in-process RPC path tried; SSH path untested |
| `internal/secretsdoc`, `internal/contexts` | age secrets document, contexts, `eru login`, `eru context *` | 🟡 encrypt, decrypt, login and contexts tried locally |
| `internal/localenv` (preflight) | `missing_tool` checks and install offers; the pinned, checksum-verified usql download for `eru console` | 🟡 built; first used by `psql` and `console` (Phase 2) |
| Steps 1–6 | Base packages, admin user, WireGuard, sshd hardening + **commit-confirm** move to wg0, ufw/fail2ban, journald/swap/unattended-upgrades | 🟡 scripts pass shellcheck; firewall logic tried against a stub `ufw` only |
| `cluster init` (host part) | Remote and `--local`, `--resume`, `node unlock-ssh`, the `install.sh` init prompt | 🟡 input checks and first contact up to auth tried locally |

Built alongside, ahead of their phase because Phase 1 needs them: the Phase 1 subset of `cluster.toml` with validation (`internal/config`), OS and RAM detection (`internal/hostinfo`), and `eru plan` / `eru apply` over the tunnel (they run steps 1–6 and are how an operator first reaches a `--local` node).

**Exit:** `eru cluster init --host root@<vps>` ends with public 22 closed, SSH working over WireGuard, and a secrets document that `eru login` accepts on a second machine. A broken tunnel during lockdown reverts by itself.

### 7.5 Phase 2 — First cluster 🟡

`cluster init` now finishes with a usable database. It runs end to end on a VPS; the exit check below is being recorded.

| Module | What it does | Status |
| --- | --- | --- |
| `internal/config` | Strict `cluster.toml` schema, defaults and validation; dotted-key `get`/`set` | 🟡 |
| `internal/hostinfo` | CPU, RAM, disk, kernel and OS detection, public IPv4/IPv6 | 🟡 |
| `internal/tune` | Tuning calculator; `eru tune explain/apply` | 🟡 |
| `internal/render` | conf.d, pg_hba, pg_ident, pgbouncer.ini, pgbouncer_hba.conf, nginx.conf, eru-certs units, unified `Diff` | 🟡 |
| `internal/pg`, `internal/state` | pgx admin client (local psql + remote pgx), desired-state store | 🟡 |
| Steps 7–13 | Kernel/sysctl, PGDG + PG18, cluster create, managed config, roles and databases, nginx + ACME certificates, PgBouncer | 🟡 |
| Commands | `plan`, `apply`, `config *`, `db *`, `sql`, `psql`, `console`, `tunnel`, `ssh`, `logs`, `tune *` | 🟡 |

**Manual test:** on a fresh VPS, run `cluster init`, then connect an app to `data.example.com:6432` with `sslmode=verify-full`. `eru sql --db app "select 1"` works. Re-running `eru apply` changes nothing, and `db drop` with only `--yes` is refused.

### 7.6 Phase 3 — Database backups & restore ⬜

| Module | What it does | Status |
| --- | --- | --- |
| `internal/backup` (pgBackRest driver) | Stanza, WAL archiving, full/diff/incr, verify. Check pgBackRest's maintenance status before starting (§9) | ⬜ |
| Per-database backup policies | Each database can have its own schedule and retention in `cluster.toml` | ⬜ |
| Step 14 + backup timers | Schedules for backups, verify and restore drills | ⬜ |
| Commands | `backup now/list/verify`, `restore` (time/latest/set; paused, then promote/abort/cleanup), `restore drill`, `restore --db`, `dump` | ⬜ |

**Manual test:** take a backup, write rows, restore to a time before them, then promote. Run a drill. Restore a single database under a new name.

### 7.7 Phase 4 — VPS backups & disaster recovery ⬜

| Module | What it does | Status |
| --- | --- | --- |
| `internal/backup` (restic) | VPS snapshots to the same bucket, under a separate prefix | ⬜ |
| Step 15 + VPS timer | Weekly by default | ⬜ |
| Commands | `vps backup now/list`, `vps restore`, `cluster init --restore` | ⬜ |

**Manual test:** load data, destroy the VPS, create a new one and run `cluster init --restore`. The data, WireGuard identity and host keys match, and the old context keeps working.

### 7.8 Phase 5 — Upgrades ⬜

| Module | What it does | Status |
| --- | --- | --- |
| `internal/upgrade` (nodes) | `eru upgrade` also upgrades nodes; `node upgrade`, `--rollback` | ⬜ |
| `pg update` | Postgres minor upgrade with a pre-upgrade backup | ⬜ |

**Manual test:** publish version N+1 and run `eru upgrade`. Only the changed steps re-run. `node upgrade --rollback` brings back version N.

### 7.9 Phase 6 — Observability & maintenance ⬜

| Module | What it does | Status |
| --- | --- | --- |
| Step 16 | Exporters and the authenticated `/metrics` endpoint behind nginx | ⬜ |
| Status views | `status`, `top`, `queries`, `doctor` (all checks, including `--security`) | ⬜ |
| `internal/maint` | `maint vacuum/analyze/reindex/repack/report/kill` and the maintenance timers | ⬜ |
| Steps 17–18 | Remaining timers; `doctor` as the final gate of `init` | ⬜ |

**Manual test:** scrape `/metrics` with and without credentials. `doctor` is clean on a fresh cluster. Run a bloat report, then a repack.

### 7.10 Phase 7 — Operators & agents ⬜

`operator add/revoke`, scoped read-only operators, the full `eru guide agents` text, a generated `docs/CLI.md`, alerting (webhook, ntfy, email) on backup age, archive lag, disk, wraparound, certificate expiry and failed drills, config drift detection, provider snapshots, and Ubuntu support checks.

**Manual test:** a read-only agent operator can't change anything; each alert fires once when its condition is forced.

### 7.11 Phase 8 — Extensions ⬜

`internal/catalog` and `ext list/add/remove`, including extensions that need a restart. `pg_stat_statements` and `auto_explain` are already on by default from Phase 2.

### 7.12 Phase 9 — Multi-node & read replicas ⬜

`node add --role replica`, the WireGuard mesh across nodes, bootstrapping from the bucket, cascading replication, the ro endpoint and lag in `status`. Tailscale becomes available as a transport option here.

### 7.13 Phase 10 — High availability ⬜ (last)

Patroni + etcd (3 voters), adopting the existing node into Patroni, sync standby, switchover/failover commands, HAProxy + PgBouncer on each node, the rw endpoint strategy (§5.5) and replicated desired state.

**Exit:** kill the primary under load. Writes resume in under 30 s with no lost committed transactions in sync mode, and the CLI keeps working against the new primary.

### 7.14 Backlog (unscheduled)

More PostgreSQL versions, major upgrades (`pg_upgrade --link` or logical blue/green), logical replication tooling, PgDog / read-write split, `eru mcp`.

## 8. Testing strategy

There's no automated test suite. The incus VM suite was dropped on 2026-10-10 because it was too flaky to trust.

- **CI** runs `just lint` (golangci-lint, shellcheck, actionlint, `--check-meta`, `--check-steps` and the cosign pin check) and builds for all four OS/arch targets. Both are fast and deterministic.
- **Manual testing** starts at the end of Phase 2, once eru can set up a new server with a cluster. Each phase from then on ends with the manual test listed for it in §7. Run it on a real VPS, and record the date, eru version and VPS (provider, OS, size) next to the phase when it passes.
- **No unit tests.** The behaviour that matters is what happens on a real host.
- **The agent JSON contract** is described in `schema/eru-v1.json`. Bump `"schema"` when envelopes change incompatibly.

## 9. Risks

- **Lock-out during bootstrap.** Moving SSH onto WireGuard can lock the operator out. Mitigated by commit-confirm with automatic revert, `eru node unlock-ssh` via the provider console, and tests that force the failure.
- **The secrets document is a single high-value file.** It's age-encrypted, operators are per-person and revocable, and the recovery kit can be exported separately for cold storage.
- **pgBackRest project health.** Crunchy Data, its main sponsor, was acquired by Snowflake in 2025. Check maintenance status before committing. The `backup.Driver` interface keeps WAL-G as a drop-in fallback.
- **Agents with write access.** Destructive operations need named confirmation tokens, and read-only operator scopes arrive in Phase 7. The agent guide explains both.
- **Single-node blast radius.** One VPS is a single point of failure until HA (Phase 10). PITR and VPS backups bound data loss to seconds, but downtime is "time to rebuild". Document an RPO/RTO for that.
- **OOM on small VPSes.** Sensible overcommit settings plus conservative tuning for < 2 GB RAM.
