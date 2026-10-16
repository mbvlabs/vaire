# eru: self-hosted, production-grade PostgreSQL

> Master document: goal, decisions, architecture, roadmap and **current status** (§7). This is the one place that tracks progress. Implementation detail lives in [`IMPLEMENTATION.md`](./IMPLEMENTATION.md), which carries no status.
> Approved 2026-10-09 · roadmap made linear and testing changed to manual 2026-10-10 · re-scoped to a PostgreSQL cluster per database on a master node 2026-10-13 ([`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md), approved)

## 1. Goal

**One Glorious CLI**: a single signed Go binary, `eru`, that sets up, secures, runs, backs up, restores and maintains PostgreSQL databases, and is the only interface to them.

From a laptop (or from an agent), `eru` takes a fresh Debian or Ubuntu VPS, hardens it, installs PostgreSQL 18, and connects the operator to it over WireGuard. Each database the operator creates gets its own tuned and secured PostgreSQL cluster, sized and limited like a managed provider's instance. Once setup is done, eru prints a **secrets handoff**: every secret in plain text for the operator's password manager, plus an encrypted transfer bundle that authenticates any `eru` installation to the server. Every later operation goes through the same CLI, over SSH, through WireGuard. That includes backups, restores, maintenance, extensions, adding nodes and running queries.

The CLI is **agent-first**. Every command is non-interactive when asked, emits structured JSON, has documented exit codes and dry-run support, and protects destructive operations. Pointing an agent at `eru` is a supported way to operate a server.

The first goal is the **master node**: one VPS that hosts every database and owns the desired state, plus the CLI that drives it. Worker nodes (including a node for a single database), followers and replicas come later and HA comes last, but early decisions must not block them.

### What has to be right first

1. **Backups and restores** to S3-compatible storage (Cloudflare R2, AWS S3, Backblaze B2, MinIO, Hetzner Object Storage), with point-in-time recovery, encryption, verification and a tested path to rebuild on a brand-new server. This covers each **database** (its own stanza, configurable schedule, continuous WAL) and the **master's own state** (a small encrypted bundle after every change, Phase 5).
2. **Settings tuning and security settings**, derived from the hardware and workload and kept under eru's control.

### Non-goals (for now)

- PostgreSQL versions other than 18. The design is version-aware so more can be added later.
- RHEL, Alpine, Windows servers, Kubernetes, containers. Postgres runs on the host from PGDG packages. The operator CLI does run on Linux and macOS.
- A web UI. eru is terminal-first; rich views are TUIs (`eru top`) and everything is scriptable.
- Multi-tenant DBaaS, sharding, and logical multi-master.

## 2. Design principles

- **One binary, two roles.** The same `eru` binary runs on the operator's machine (**operator mode**) and on each server (**node mode**). Operator mode drives node mode over SSH. Host mutations are small, idempotent bash scripts embedded with `go:embed`, the same pattern as `deploycrate-ce/internal/setup`.
- **SSH through WireGuard is the only control channel.** There is no daemon, no HTTP control API and no open control port. After bootstrap, sshd listens only on the WireGuard address.
- **Desired state in, convergence out.** The desired state of the whole setup (the server, its nodes and every database's cluster) is one TOML file on the master. `eru apply` converges every node to it, and running it again is always safe. `eru plan` shows what would change.
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
| Databases as clusters | **Each database is its own PostgreSQL cluster** (`pg_createcluster 18 <db>`): its own data directory, socket, WAL stream, pgBackRest stanza, configuration, restarts and systemd resource limits. The user only names databases; the cluster underneath shares the name. Several databases in one cluster aren't offered: things that belong together share one database with several schemas. Decided by [`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md) |
| Naming | **Server** (or node) is a VPS, **cluster** is the PostgreSQL cluster, **database** is the only noun the user manages. `cluster init` became `server init`, and contexts name servers |
| Master node | The first server `server init` sets up is the **master**: it holds the desired state for everything (`cluster.toml`), the registries and `records.json`, is the WireGuard hub, and runs clusters like any node. Only the master accepts changes; from Phase 10 it drives worker nodes. It's a single point of failure for *changes* until HA. One operator is assumed; teams come later |
| Sizes and limits | A database has a **size** (`nano`, `small`, `medium`, `large`, `xlarge` or `custom`) that sets its memory, CPU weight and `max_connections`; explicit values win. `[postgres] default_size` (default `medium`) steps down to what fits. Each cluster gets a cgroup drop-in: `MemoryHigh` (memory + 25%), `CPUWeight`, `IOWeight`, with `MemoryMax` and `CPUQuota` opt-in. The server keeps a reserve of `max(512 MiB, 25% of RAM)` and the clusters share the rest |
| Oversubscription | A new or larger size past the memory budget is refused (`memory_overcommitted`, with the largest size that fits as a command) unless `--allow-overcommit`, which goes ahead with the warning; nothing steps down by itself. `cpu_crowded` and `disk_crowded` are warnings. eru always refuses what PostgreSQL couldn't start (`would_not_start`, against `CommitLimit` and the huge-page pool). eru never rebalances existing databases on its own |
| Hostnames | **One hostname per node**, `<node>.<domain>` (`db1.<domain>` for the master), never one per database. The database name in the URL's path picks the database. Moving a database to another node (Phase 10) changes its URL's host |
| Client access | Two PgBouncers on each node route by database name: the **transaction pooler** on 6432 (and behind nginx's ALPN routing on 443, direct TLS with `sslnegotiation=direct`, PG17+ clients) for `<db>_app` and `<db>_ro`, and the **session pooler** on 5432 for migrations, `<db>_migrate` and eru's own tools. Clients use `sslmode=verify-full` against the node's ACME certificate. Clusters listen on Unix sockets only. The record must be DNS only, not proxied |
| Metrics | Exporters only, exposed via an authenticated nginx endpoint for external scraping (Grafana Cloud, own Prometheus). No metrics storage on the box |
| Config format | TOML |
| Node networking | WireGuard mesh managed by eru by default; Tailscale as an option. From Phase 10 the master is the hub: the operator has one peer, and packets for workers are forwarded `wg0 → wg0` |
| Operator WireGuard | Embedded userspace by default; `eru wg export` produces a system config when wanted |
| Public SSH | Closed after bootstrap: SSH only on WireGuard, commit-confirm switchover, break-glass via the provider console |
| Install paths | Both: the operator CLI bootstraps servers remotely (primary), or `eru server init --local` runs directly on a server installed with `curl \| sudo bash` and hands off with a secrets document |
| Secrets handoff | Printed once in the terminal: plain secrets for a password manager, plus an armored age transfer bundle with a passphrase eru generates. No file unless `--secrets-out` |
| Agent integration | First: CLI + `--json` + `eru guide agents` / `eru skill install`. `eru mcp` comes later |
| CLI conventions | Modelled on the andurel CLI's agent support (§5.4) |
| Testing | Manual testing on real VPSes from Phase 2 on; CI runs lint and builds only. No unit tests (§8) |
| Release site | `https://eru.mbvlabs.com`. The root manifest is the highest stable line, each major line under `/<major>.x/` (for example `/1.x/`), edge under `/edge/`, and every version is kept under `/releases/<version>/`. Edge publishes on every push to `master`, stable on `vX.Y.Z[-alpha.N\|-beta.N\|-rc.N]` tags whose commit is on `origin/<major>.x`. A binary from a line tag follows its line by default. Releases are signed with cosign (pinned 3.1.3) |
| Platforms | The CLI runs on Linux and macOS. Node-side commands are Linux-only and are marked that way in `eru commands --json` |
| Self-upgrade | `eru upgrade` keeps the previous binary next to it as `eru.prev`, and refuses to replace a binary installed by Homebrew or Nix |
| Risky flags | A command can mark a single flag as changing or destroying something (for example `queries --reset`), so agents don't treat the plain read as dangerous |
| `eru top` | Reads Postgres directly over the WireGuard tunnel rather than through the node |
| Database ports on WireGuard | WireGuard peers can always reach 6432 and 5432 (both poolers). `expose_pgbouncer` / `expose_postgres` only control the public allowlist rules |
| Generated secrets | The secrets document holds server access only: every server secret eru generates, including `eru_admin` and the `pgbouncer_auth` and `eru_monitor` service passwords, which are the same in every cluster so adding a database never makes the document stale. Application database passwords are shown once by `eru db create` (and `db fork`) and never stored. The operator generates all database passwords; the node keeps only SCRAM verifiers, plus the two service passwords that PgBouncer and the exporters need at runtime |
| Database backups model | **A stanza per database** (`<db>-<6 hex>`, immutable) in one repository with one cipher, so backups, PITR, retention, verify and drills are all per database. Restore is the provider's primitive: build a new cluster at a point in time (**materialise**), inspect it, then cut over in place (`restore promote`, same URL and credentials) or keep it as a new database (`db fork --at`). A dropped database's stanza is kept for its window. No in-place restore of a whole server; rebuilding the master is `server init --restore` (Phase 5) |
| Turning backups on | Optional at `server init` ("Skip for now", which ends with the `no_offsite_backup` warning), and `eru backup setup` adds them later. `server init` creates the recovery identity either way. Every cluster has `archive_mode = on` from creation, so turning backups on is a reload, never a restart. The operator probes the bucket, generates the pgBackRest cipher, and saves the kit before anything reaches the node |
| Backup schedules | Template timers per database (`eru-backup-full@<db>` …), defaulting to `[backup.database]`, each database offset inside the maintenance window by a hash of its stanza. A server-wide heavy-I/O lock lets only one full, diff, verify, drill or dump run at a time |
| Long-running work | Backups, restores, drills, forks and dumps run as detachable jobs (transient systemd units); Ctrl-C detaches and `eru job follow` re-attaches |
| Longer retention | Per-database scheduled `pg_dump` archives, age-encrypted to a recipient whose identity only the operator holds, for points older than the restore window |
| pgBackRest floor | 2.59.3 from PGDG, held; WAL-G stays a fallback behind `backup.Driver` (not built) |
| DNS and certificates | One certificate per node, for its one hostname. In `acme` mode, `server init` shows the DNS record to create, waits for the operator to confirm it, and checks it against public resolvers. TLS never blocks on ACME: a placeholder certificate lets the poolers and nginx start, and a certificate that's still pending after 120 s is a warning, not a failure |
| Master backup (Phase 5) | Replaces the restic VPS backup: a small age-encrypted bundle of what's born on the master (desired state, registries, `records.json`, the TOTP secret, WireGuard and host keys, the certificate and ACME account), built after every change (a dirty flag and a path unit) and uploaded to `<prefix>/master/` with a sidecar, kept locally too. An epoch in the bucket fences an old master: it stops its timers and poolers, and every cluster's `archive_command` (`eru node archive-push`) refuses. `server init --restore` rebuilds on a new VPS with the old identity; databases come back `awaiting_recovery` and `db recover` brings each back from its stanza. Decided in [`PHASE-5-WORK-UNITS.md`](./PHASE-5-WORK-UNITS.md) |
| Followers (Phase 6) | Physical standbys of one database's cluster, with `wal_level = replica`; `db unfollow` promotes one into an independent database |
| HA (Phase 11, last) | Patroni per cluster with one shared etcd. Until then eru runs plain `postgresql-common`, with a designed and tested adoption step into Patroni |

**Still to decide** (when the phase that needs it starts):

- ~~Which exporter versions to pin, and whether eru's own metrics replace `pgbackrest_exporter` (Phase 4).~~ Decided in [`PHASE-4-WORK-UNITS.md`](./PHASE-4-WORK-UNITS.md): pinned upstream binaries (node_exporter 1.12.1, postgres_exporter 0.20.1, pgbouncer_exporter 0.12.1) with sha256 in `internal/versions`; no `pgbackrest_exporter`, `eru.prom` carries the backup facts.
- ~~Where the table-bloat query comes from (Phase 4).~~ Decided: the ioguix estimate queries, vendored under BSD-2-Clause (`eru version --licenses`); exact numbers with pgstattuple wait for Phase 7.
- Which catalog extensions to drop because they have no PostgreSQL 18 package (Phase 7).

## 4. Technology choices

| Concern | Choice | Notes |
| --- | --- | --- |
| Binary / CLI | Go, `cobra` for commands, Charm `huh` for interactive prompts, `bubbletea` + `lipgloss` for TUIs (`eru top`, progress) | Same Charm stack as deploycrate |
| Transport | `golang.org/x/crypto/ssh` in-process; embedded userspace WireGuard (`wireguard-go` + netstack) in operator mode | Operator needs no root, no `wg` interface and no system SSH config |
| Distribution | `install.sh` + cosign-signed manifest + sha256 for linux/darwin × amd64/arm64 | Copy of `deploycrate-ce/scripts/install.sh` and `build-release.sh`. Operator mode pushes the matching verified linux binary to nodes |
| Transfer bundle | ASCII-armored `age` passphrase-encrypted bundle (`filippo.io/age`, scrypt) | §5.3 |
| Server OS | The latest Debian and Ubuntu LTS only: Debian 13, Ubuntu 26.04 | Preflight refuses anything else, including older releases. PGDG ships `postgresql-18` for both on amd64 and arm64 (checked 2026-10-11) |
| PostgreSQL | PGDG apt repo (`apt.postgresql.org`), `postgresql-18`, managed via `postgresql-common`: one `pg_createcluster 18 <db>` per database, run as `postgresql@18-<db>` | Data checksums are on by default in 18. Major version pinned with an apt hold |
| DB backups | **pgBackRest** (S3 repo, PITR, full/diff/incr + block incremental, AES-256 repo encryption, async WAL archive, `verify`), one stanza per database in one repository | WAL-G fallback behind the interface |
| Master backup (Phase 5) | An age-encrypted tar of the master's own state, uploaded in-process with `minio-go` | Replaces the restic VPS backup; restic is not a dependency |
| Logical exports | `pg_dump -Fc` to the bucket or locally, per database | Long retention and portability, not DR |
| Pooling | **PgBouncer**, two instances from one template: transaction mode on 6432 (`max_prepared_statements`) and session mode on 5432 (eru's own `eru-pgbouncer-session.service`). Both route by database name with a `[databases]` entry per database and `auth_query` against that database's cluster | Clients connect to a pooler, which reaches the database's cluster over its unix socket |
| TLS & metrics edge | **nginx** (distribution package): HTTP-01 challenges on 80; on 443 `stream` + `ssl_preread` routes ALPN `postgresql` to the transaction pooler and everything else to HTTPS (authenticated `/metrics` later). Certificates come from eru's own ACME client (`x/crypto/acme`) | Only direct-TLS clients on 443 go through nginx; 6432 and 5432 are the poolers' own TLS |
| Resource limits | systemd cgroup v2 drop-ins on each `postgresql@18-<db>.service` | `MemoryHigh`, `CPUWeight`, `IOWeight`; `MemoryMax`, `CPUQuota` opt-in |
| Metrics | `node_exporter`, `postgres_exporter` (one `/probe` target per cluster), two `pgbouncer_exporter`s, eru's own textfiles (`eru*.prom`); `pg_stat_statements` on by default | Pinned upstream binaries on loopback, behind nginx's authenticated `/metrics` with an `http_sd` target list (Phase 4) |
| DB access from CLI | `pgx` over the WireGuard tunnel | `eru sql`, `eru psql`, `eru console` (usql), `eru tunnel` |
| HA (Phase 11) | Patroni per cluster + one etcd (3 voters) | Routing decided when the phase starts (libpq multi-host URLs, or every node's poolers routing to the current primary) |
| Scheduling | systemd timers on each node calling `eru node …` subcommands | No long-running eru process anywhere |

## 5. Architecture

### 5.1 The control model

```
 operator laptop / CI / agent                         VPS: the master node, db1.<domain>
┌──────────────────────────────┐                     ┌──────────────────────────────────────┐
│ eru (operator mode)          │                     │ sshd  ── listens on wg0 only         │
│  ├ context: server "prod"    │   WireGuard (UDP)   │ wg0 10.42.0.1                        │
│  │  (from secrets document)  │════════════════════▶│                                      │
│  ├ userspace WireGuard       │   SSH inside WG     │ eru (node mode), invoked per command │
│  ├ in-process SSH client  ───┼────────────────────▶│  ├ steps + embedded scripts          │
│  └ pgx over the tunnel    ───┼────────────────────▶│  ├ journal, /etc/eru/cluster.toml    │
└──────────────────────────────┘   (5432 via wg0)    │  └ systemd timers → eru node …       │
                                                     │                                      │
                                                     │ nginx · transaction + session poolers│
                                                     │ a PostgreSQL 18 cluster per database │
                                                     │ pgBackRest · exporters               │
                                                     └──────────────────────────────────────┘
```

- **Operator mode** (`eru <command>` on a laptop) resolves the server's context, brings up an in-process WireGuard tunnel, opens SSH to the right node, and runs `eru node <subcommand> --json` there. It streams progress back and renders it, or passes it through as JSON.
- **Node mode** (`eru node …`, only on servers) does the actual work as root via sudo: steps, backups, restores, maintenance. It's also what the systemd timers call. A human can SSH in and run it by hand.
- **Version lockstep.** Before running anything, operator mode checks the node's eru version. If it differs, operator mode refuses to continue and tells the user to run `eru upgrade`, unless `--auto-upgrade-nodes` is set or configured on the context. One CLI version means one behaviour.
- **Upgrading nodes from the operator.** Node binaries are always upgraded from outside the VPS, through the operator CLI:
  - `eru upgrade` self-updates the local CLI (signed), then upgrades every node.
  - `eru node upgrade [--node <name>|--all] [--version <v>]` upgrades nodes only, for example to roll back or to pin a version.
  - Each node upgrade downloads the matching linux build, verifies its cosign signature and sha256 against the manifest, and swaps `/usr/local/bin/eru` atomically, keeping the previous binary. It runs `eru node selfcheck`, then `eru apply` re-runs only the steps whose versioned IDs changed.
  - Upgrades wait for running timers (backups, maintenance) to finish. In HA (Phase 11) they roll one node at a time, replicas first.
  - `eru node upgrade --rollback` restores the previous binary.
- **State.** The desired state (`/etc/eru/cluster.toml`), the durable records (`/var/lib/eru/records.json`: restores, leftovers, timeline switches, dropped stanzas, drills and last results per database) and the journal live on the master. From Phase 5 the master backs its own state up to the bucket after every change. The operator machine holds only credentials, in its contexts, plus a cache of the nodes and of which node holds each database. Any machine with the transfer bundle and its passphrase can take over operation.

### 5.2 Bootstrap: from a fresh VPS to WireGuard-only SSH

0. **Local preflight** on the operator machine (§5.2.1).
1. `eru server init --host root@203.0.113.10` (or interactively, `eru server init`). The first contact goes over public SSH with the provider-supplied key or password.
2. eru pushes the node binary, runs preflight, and applies the hardening and WireGuard steps. The server becomes `10.42.0.1` and the operator gets a peer identity.
3. **Commit-confirm switchover.** eru verifies that SSH works over WireGuard, then moves sshd to the wg0 address and closes public 22. A server-side timer reverts the firewall and sshd change unless eru confirms within 2 minutes, so a broken tunnel can't lock the operator out.
4. The rest of the setup (PostgreSQL packages, nginx and the certificate, backups) runs over WireGuard. The server starts with no databases; `eru db create` adds each one as its own cluster.
5. eru prints the **secrets handoff**, makes the operator confirm it's stored safely, and wipes the plaintext copies it held server-side, except for what services need at runtime.

Break-glass is the provider console plus `eru node unlock-ssh`, which re-opens public SSH for a fixed window. This is documented in the secrets document itself.

#### 5.2.1 Local preflight and privilege escalation

Before any command that needs them, eru checks the machine it runs on and reports what's missing as a structured `missing_tool` error, or as a prompt in a TTY:

- **Operator machine:** reachability of the target, an SSH key to use for first contact (or a password), and optional tools by feature: `wireguard-tools` when a system WireGuard interface is wanted (`eru wg export --install`), `psql` for `eru psql`, and `tailscale` when that transport is chosen. The embedded userspace WireGuard needs nothing installed.
- **Install offer:** eru shows the exact package command for the platform (`apt`, `dnf`, `brew`) and asks whether it may run it. Without a TTY it never installs; it fails with the command to run, or installs only when `--install-missing` is passed.
- **Sudo escalation:** when a step needs root locally (system WireGuard interface, `eru server init --local`), eru asks *"eru needs root to <reason>. Escalate with sudo? [y/N]"*, then re-executes the specific subcommand through `sudo`. It doesn't keep a root shell, and only that step runs privileged. `--sudo` pre-approves this for agents and scripts, and `sudo -n` is tried first so passwordless sudo works without a prompt.
- **`eru server init --local`** (on a server installed via `curl | sudo bash`): checks it's root or can escalate, checks the OS, runs the same steps locally, then switches SSH to WireGuard. The secrets handoff is printed on the server console (nothing is written to `/root`), and the operator pastes its transfer bundle into `eru login` on their machine. From that point on, operation is identical to a remote bootstrap.

### 5.3 Secrets handoff and contexts

At the end of setup eru prints the **secrets handoff** in the terminal and writes no file unless `--secrets-out` asks for one. It has two parts: every secret in plain text, grouped for a password manager, and a **transfer bundle**, an ASCII-armored `age` passphrase-encrypted block holding the same content. eru generates the passphrase and shows it once, and it's never stored by eru. The content is:

- Server identity: name, domain and TLS mode, node list with hostnames, WireGuard endpoints and public keys, server SSH host keys (pinned)
- Operator credentials: WireGuard private key and address, SSH private key, `eru_admin` DB role password, service role passwords (the same in every cluster)
- Backup **recovery kit**: the age recovery identity (created at `server init`, used for dumps and, from Phase 5, the master bundles), and once a bucket is set up its endpoint and keys and the repository cipher passphrase. The stanzas aren't listed: they follow from the desired state, or from listing `<prefix>/pgbackrest/backup/`
- Metrics endpoint credentials
- Break-glass instructions

Application database passwords are not server access and are not in it: `eru db create` prints them once and eru keeps no copy.

```
eru login                               # paste the transfer bundle → create local context "prod"
eru context list | use <name> | remove <name>
eru secrets export [--rotate-operator]  # show a new handoff, optionally with a new operator identity
eru operator add <name>                 # new WireGuard peer + SSH key → separate document for a teammate/agent
eru operator revoke <name>
```

Losing the document doesn't mean losing the server as long as console access exists, but losing the **recovery kit** means the backups can't be read. eru won't finish `init` until the user confirms the document is stored, and `eru doctor` keeps reminding them.

Per-operator identities let an agent get its own scoped document, for example a **read-only operator** whose SSH certificate and DB role only allow read commands (Phase 9).

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
- **Destructive operations need an explicit confirmation token** that names the target: `--confirm app/restore`, `--confirm app/restart` or `--confirm <db-name>`. Passing `--yes` alone isn't enough, which protects against agents too.
- **Self-description.** `eru commands --json` (above), and `eru guide agents` prints the usage guide that `eru skill install` installs.
- **Read helpers** for agents: `eru sql --json "select …"` (read-only role by default; `--write` needs a write-capable context), `eru status --json`, `eru maint report … --json`.
- An MCP server mode (`eru mcp`) is planned for a later phase, built on the same command tree.

### 5.5 Phases 10–11: workers, dedicated nodes and HA

```
                                 operator (one WireGuard peer: the master)
                                            │
                                            ▼
   ┌─────────────────────────────┐   wg0 → wg0   ┌─────────────────────────────┐
   │ db1.<domain>  master        │══════════════▶│ db2.<domain>  worker         │
   │ cluster.toml, records, hub  │  fleet SSH key │ node.toml, its own journal  │
   │ 2 poolers · nginx · cert    │               │ 2 poolers · nginx · cert     │
   │ app, billing, staging       │               │ app_big (dedicated)          │
   │ their timers                │               │ its own timers               │
   └─────────────────────────────┘               └─────────────────────────────┘
          clients: db1.<domain>/app                 clients: db2.<domain>/app_big
                       pgBackRest → one bucket, a stanza per database
```

- **Workers are driven from the master.** `eru node add db2 --host root@<ip> [--dedicated <db>]` bootstraps the node (hardening, WireGuard to the master, the fleet SSH key, its hostname `db2.<domain>` and certificate). Only the master accepts changes; `apply` applies the master's steps first, then each worker's. Workers run their own poolers, certificates and timers, so their databases keep serving and backing up while the master is down.
- **The master is the hub.** The operator keeps one WireGuard peer; the master forwards `wg0 → wg0` only. `eru sql`, `psql`, `console` and `tunnel` look up the database's node in the context's cache and connect to that node's session pooler through the hub, verifying its hostname.
- **Placement.** A database has a `node`. `eru db create <db> --node db2` puts one there; `eru db move <db> --node db2` materialises it on the new node from its stanza, catches up from the live cluster and cuts over in seconds. Its URL's host changes; its name, roles and passwords don't. What happens to the old URL during a move is decided in Phase 10.
- **Takeover.** `eru server takeover --node <worker>` turns a worker into the master from the latest bundle (Phase 10).
- **HA (Phase 11)** runs Patroni per cluster with one shared etcd (3 voters), so one database fails over without the others. The desired state and records move to a replicated store, or a standby master follows the bundles, with separate keys per master and operator-signed revisions. Because URLs name nodes, clients use libpq multi-host URLs or every node's poolers route a database to its current primary; that's decided when the phase starts. HA may be offered only for databases on dedicated nodes.
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
internal/contexts/           ~/.config/eru: contexts (with the node and database placement cache), current context, pending init state
internal/engine/             step engine: Check/Apply, plan, journal, versioned IDs, commit-confirm
internal/steps/              step definitions
internal/steps/scripts/*.sh  embedded host scripts (go:embed)
internal/config/             cluster.toml schema, validation, defaults, migrations
internal/hostinfo/           CPU/RAM/disk/kernel/OS detection
internal/tune/               pure tuning calculator, memory budget and oversubscription arithmetic
internal/render/             conf.d, pg_hba, pg_ident, both poolers' ini files, pgbackrest.conf, cgroup drop-ins, timers, nginx.conf
internal/backup/             Driver interface, pgbackrest impl, materialise (temporary clusters), dumps, records.json
internal/maint/              maintenance tasks + reports
internal/catalog/            extension catalog
internal/pg/                 pgx admin client
internal/state/              Store interface (the master's file now, replicated in Phase 11)
scripts/install.sh, build-release.sh, sign-release.sh, upload-release.sh
schema/eru-v1.json           agent JSON contract
```

### 5.7 Files on a node

| Path | Owner / mode | Purpose |
| --- | --- | --- |
| `/usr/local/bin/eru` (+ `eru.prev`) | root 0755 | Node binary, version-locked to the operator; previous version kept for rollback |
| `/etc/eru/node.toml` | root 0640 | Marks the host as a node: `server`, `node_name`, `hostname`, `role = "master"` |
| `/etc/eru/cluster.toml` | root 0640 | Desired state, no secrets: the server, its nodes and every database's `[[clusters]]` entry (master only) |
| `/etc/eru/protect.json` | root 0600 | The TOTP secret that lifts a database's protection (`eru protect init`), the last accepted time step, and the failure count and lockout. Never in `cluster.toml` |
| `/etc/eru/runtime/` | root 0700 | Only the secrets services need at runtime (bucket keys, the dump recipient, pgbouncer auth, monitor password); `recovery.recipient`, the age recipient the master bundles are encrypted to (Phase 5) |
| `/etc/eru/fenced` | root 0644 | Exists once a newer master took over the bucket (Phase 5); world-readable so the archive wrapper, run as postgres, sees it |
| `/var/lib/eru/master-backup/` | root 0700 | Phase 5: the local master bundles (`local_keep`) and their sidecars, the dirty flag, `last.json`, the node's copy of the bucket's `epoch.json` |
| `/var/lib/eru/instance` | root 0600 | This install's random id, which the bucket's epoch names (Phase 5); never in the bundle |
| `/var/lib/eru/journal.json` | root 0600 | Applied step IDs and instances (`postgres-config-v1@app`), input hashes, pending restarts |
| `/var/lib/eru/state.json` | root 0600 | Ephemeral: jobs and lockdown flags |
| `/var/lib/eru/records.json` | root 0600 | Durable records: open restores and leftovers, timeline switches after `restore undo`, dropped stanzas and their expiry, drills, last backup, verify and dump per database |
| `<data_root>/<db>` | postgres 0700 | Each database's data directory; `data_root` defaults to `/var/lib/postgresql/18` |
| `/etc/postgresql/18/<db>/conf.d/10-eru-tuning.conf` | managed | Tuning from the database's size |
| `/etc/postgresql/18/<db>/conf.d/20-eru-security.conf` | managed | Socket only, its port, auth, logging |
| `/etc/postgresql/18/<db>/conf.d/30-eru-extensions.conf` | managed | `shared_preload_libraries` and extension settings |
| `/etc/postgresql/18/<db>/conf.d/40-eru-backup.conf` | managed | `archive_mode = on`; `eru node archive-push` (which refuses on a fenced node, then runs pgBackRest's) to the database's stanza, or `/bin/true` without backups |
| `/etc/postgresql/18/<db>/conf.d/50-eru-settings.conf` | managed | `[postgres.settings]` and the database's own `[clusters.settings]` |
| `/etc/postgresql/18/<db>/conf.d/90-overrides.conf` | user | The only hand-edited file; `eru doctor` lists what it overrides |
| `/etc/systemd/system/postgresql@18-<db>.service.d/` | managed | The database's cgroup limits and `OOMScoreAdjust` |
| `/etc/pgbouncer/pgbouncer.ini`, `session.ini` | managed | The transaction and session poolers, a `[databases]` entry per database |
| `/etc/pgbackrest/pgbackrest.conf` | postgres 0600 | One repository, a section per stanza |
| `/run/eru-ports/` | | Port reservations of temporary clusters (5480–5499) |
| `/var/log/eru/` | | Per-run logs for apply, backup, maintenance and restore drills |

### 5.8 Databases as clusters

```
 db1.<domain>
┌──────────────────────────────────────────────────────────────────────────────┐
│ :443 nginx ── ALPN postgresql ──┐                                            │
│ :6432 transaction pooler ◀──────┘  <db>_app, <db>_ro         ─┐              │
│ :5432 session pooler               <db>_migrate, eru tools    ─┤ by database │
│                                                                │ name        │
│   18/app       postgresql@18-app       socket 5433  large   ◀──┤             │
│   18/billing   postgresql@18-billing   socket 5434  medium  ◀──┤             │
│   18/staging   postgresql@18-staging   socket 5435  small   ◀──┘             │
│   18/app__restoring (temporary, 5480–5499, session pooler only)              │
│                                                                              │
│ server reserve: max(512 MiB, 25% of RAM) for the OS, poolers, nginx and eru  │
└──────────────────────────────────────────────────────────────────────────────┘
```

- **One database, one cluster, one name.** `app` is the database, the cluster `18/app`, the unit `postgresql@18-app`, the stanza `app-<6 hex>`, the pooler entry `app` and the URL path `db1.<domain>/app`. Port (5433–5479), stanza and node are assigned at create and immutable.
- **Socket only.** Each cluster has `listen_addresses = ''`; its port only names its socket. The poolers are the only database listeners on the network, and eru's own work (steps, dumps, pgBackRest, restores) uses the sockets directly.
- **Per-cluster steps.** `postgres-cluster-v1`, `postgres-config-v1` and `postgres-roles-v1` run once per database, journaled as `<step>@<db>` with their own input hash, so changing `app` re-runs nothing for `billing`. Each database has its own lock (`/run/lock/eru-<db>.lock`) beside the global one.
- **Restarts are per database.** A restart-only change asks for `<db>/restart`; `apply` takes `--confirm` more than once, and `<server>/restart` restarts every pending database one at a time. The transaction pooler pauses the database around its restart.
- **Resources.** The size's memory is the cluster's `RAMMiB` for tuning and its `MemoryHigh` (+25%); `max_worker_processes` and `max_parallel_workers` plan for `vCPU / clusters` (minimum 2); `huge_pages = try` only from 2 GiB, and `vm.nr_hugepages` is the sum. `eru tune show` prints the server's budget sheet with each database's size, limits, current use, and what's allocated or free.
- **Roles.** Every cluster has `eru_admin`, `eru_monitor`, `pgbouncer_auth`, `eru_direct_only`, both pooler auth functions, and that one database's four roles. Service passwords are setup-wide.
- **What it costs:** about 50–100 MiB of fixed memory per idle cluster on top of its buffers (to be measured), a second pooler, N stanzas and N sets of timers, and server-wide changes (a certificate, a minor update) that touch each cluster in turn. What it buys: native PITR per database, its own configuration, preload, restarts, retention and failure boundary, and per-database upgrades later.

### 5.9 How this compares to managed providers

Managed providers (DigitalOcean, RDS, Cloud SQL, Azure, Crunchy Bridge, Aiven) make the **instance** the unit of isolation, backup, PITR, fork, resize, failover and billing, and put each instance on its own VM. Several databases inside one instance share all of that, and none of them restores one database out of a shared instance: a restore or a fork always builds a **new instance** at a point in time, which you inspect and cut over to. Pooling is per database, with a mode and a size, next to a direct endpoint for sessions that need one. CloudNativePG, which packs many clusters onto shared Kubernetes nodes, goes further and allows only one database per cluster, with cgroup limits for each.

eru takes the same unit to the user's own VPS. The VPS plays the provider's host and eru plays the provider: a cluster per database is the instance, cgroups are the boundary (the strongest a single VPS offers), sizes are the plans, `materialise` is the "new instance at a point in time", and the transaction and session poolers mirror DigitalOcean's pooled and direct endpoints. Where eru goes further: `restore promote` cuts over **in place**, keeping the URL and the credentials, because a VPS user can't hand out a new connection string as easily as a cloud console can; `db fork --at` is the provider's way when only some rows are needed. Where eru stays below a provider, and the README says so: no VM boundary between databases on one server, I/O fairness that depends on the disk scheduler, shared disk space and page cache, one hostname per node (a URL that changes when a database moves, where a provider gives each instance its own), and HA only from Phase 11.

## 6. One server in detail (Phases 1–8)

### 6.1 `eru server init`

1. **Preflight** (remote): OS/version, arch, RAM ≥ 1 GB, disk, no existing Postgres, cloud-init finished, clock in sync.
2. **Mode**: *New server*, or *Rebuild the master* from its latest bundle with `server init --restore` (Phase 5).
3. **Questions**, with every answer defaulted and each also available as a flag or `--answers answers.toml`:
   - Server: server name, node name (default `db1`), admin user, timezone (default UTC), data root (root disk or attached volume; each database is a directory under it), transport (WireGuard / Tailscale)
   - Endpoint: base domain (the master becomes `db1.<domain>`), TLS mode, client CIDR allowlist for 6432/5432 (default: deny all public; WireGuard peers allowed), ACME email
   - Workload: profile (web/OLTP, mixed, analytics), default database size, expected client connections. These are setup-wide defaults; each database can override them
   - Backups: provider preset (R2/S3/B2/Hetzner/MinIO/custom), endpoint, region, bucket, prefix, keys, or "Skip for now"
   - DB backup schedule and retention (presets plus a custom cron-style schedule)
   - Extensions from the catalog (Phase 7)
4. **Plan**, then confirm with `--confirm <server>` when non-interactive.
5. **Apply** the steps (§6.2) with live progress. Each step is logged and resumable. The server starts with **no databases**; `eru db create` adds them afterwards, each as its own cluster. `server init` creates the age recovery identity either way, and without a bucket it ends with the `no_offsite_backup` warning and the fix, `eru backup setup`.
6. **Handoff**: show the secrets handoff, gate on "stored safely", then print connection info and the next steps (`eru db create`).

### 6.2 Step catalogue

| # | Step | Adapted from deploycrate |
| --- | --- | --- |
| 1 | Base packages, cloud-init wait, apt hygiene | `packages.sh` |
| 2 | Admin user, sudo, operator SSH key | `user.sh` |
| 3 | WireGuard interface + operator peer (or Tailscale join) | `wireguard.sh` |
| 4 | sshd hardening (no root, no passwords, `AllowUsers`, `MaxAuthTries`) + **commit-confirm move to wg0** | `ssh.sh` |
| 5 | ufw (WG UDP, 80, 443, the two poolers' 6432/5432 to the allowlist and WireGuard; nothing per database), fail2ban (sshd plus a postgres auth-failure jail) | `ssh.sh`, `host.sh` |
| 6 | journald limits, swap (small), unattended-upgrades for security updates only, with Postgres packages excluded | `host.sh` |
| 7 | Kernel/sysctl for databases: `vm.swappiness=1`, `vm.overcommit_memory=2` + ratio, dirty bytes, THP off, `vm.nr_hugepages` as the sum over the clusters that use huge pages, limits | new |
| 8 | PGDG repo + signing key, `postgresql-18`, `postgresql-contrib`, `pgbouncer`, `pgbackrest` (floor 2.59.3), apt hold | new |
| 9 | Per database (`postgres-cluster-v1@<db>`): `pg_createcluster 18 <db>` on its port, data dir `<data_root>/<db>`, locale `C.UTF-8`, checksums; the cgroup drop-in | new |
| 10 | nginx install, placeholder certificate for the node's hostname, and the hourly `eru-certs` timer that issues and renews the ACME certificate (eru's own client) and reloads both poolers, nginx and every cluster | new |
| 11 | Per database: render managed conf.d (socket only, tuned from its size, archiving on from creation), pg_hba and pg_ident; restart or reload depending on the diff | new |
| 12 | Per database: the service roles, both pooler auth functions, and that database with its four roles | new |
| 13 | Both PgBouncers: a `[databases]` entry per database, `auth_query` against that database's cluster, TLS; reload | new |
| 14 | pgBackRest: one config with a section per stanza; stanza create, check, first full backup per database | `backup.sh` (pattern) |
| 15 | `master-backup-v1`: the master backup's directory, recovery recipient and instance id; `timers-v1` renders its units (Phase 5) | new |
| 16 | Exporters and the authenticated `/metrics` behind nginx | `node-exporter.sh` |
| 17 | systemd timers: template units per database for backups, verify, drills and dumps, stanza expiry, maintenance, reports | `host.sh` (timer pattern) |
| 18 | `eru doctor` as the final gate, then generate the secrets document | new |

### 6.3 Tuning and security settings

**Tuning** comes from `internal/tune`, a pure function of (hardware, the database's size and workload profile, PG version) that returns settings plus a reason for each value. Each database's cluster is tuned on its own, from its share of the server. It is shown by `eru tune explain --db <db>`.

- Memory budget: the server keeps a reserve of `max(512 MiB, 25% of RAM)` (or `[server] memory_reserve`), and the clusters share the rest. Each cluster's `memory` (from its size) is the RAM it tunes for.
- Memory: `shared_buffers` ≈ 25% of the cluster's memory, `effective_cache_size` ≈ 70–75%, `work_mem` from (memory − shared_buffers) / (connections × profile factor), `maintenance_work_mem` / `autovacuum_work_mem` capped, `huge_pages = try` from 2 GiB with the OS pre-sized to the sum.
- Connections: `max_connections` comes from the size (20 for `nano` up to 200 for `xlarge`), and both pools plus 3 reserved slots must fit in it.
- WAL / checkpoints: `wal_compression = zstd`, `min_wal_size` / `max_wal_size` from the cluster's share of the disk (in proportion to its memory), `checkpoint_timeout = 15min`, `checkpoint_completion_target = 0.9`.
- I/O (PG18): `io_method` (`worker` by default, `io_uring` when the kernel and build support it), `effective_io_concurrency` / `maintenance_io_concurrency` for SSD/NVMe, `random_page_cost = 1.1`.
- Parallelism: `max_worker_processes`, `max_parallel_workers(_per_gather)` from the cluster's CPU share (`vCPU / clusters`, at least 2) and profile. JIT is off for OLTP.
- Autovacuum: more aggressive scale factors, `autovacuum_vacuum_max_threshold` (new in 18), `autovacuum_worker_slots`, cost limits sized to the disk.
- Re-running `eru tune` after a VPS resize recalculates and applies, flagging settings that need a restart. `eru db resize` changes one database's size: its limits and reload-only settings change live, the rest waits for `<db>/restart`.

**Security** settings:

- Clusters listen on Unix sockets only (`listen_addresses = ''`), and their `pg_hba.conf` allows nothing else. TLS ends at the poolers (and nginx on 443): `ssl_min_protocol_version = TLSv1.2`, clients told to use `sslmode=verify-full`, PG18 `sslnegotiation=direct` supported. Clusters keep `ssl = on` for the replication listener of later phases.
- Auth: `password_encryption = scram-sha-256` everywhere, `peer` for local admin via pg_ident, TLS required by the poolers' hba on TCP and a terminal `reject` rule.
- Network: public 5432/6432 is closed unless the allowlist opens it. Operators and agents reach the poolers over WireGuard.
- Roles, in every cluster: `postgres` superuser is local-socket only. A separate admin role (CREATEDB/CREATEROLE, not superuser) for operators. Per-database owner, migrate and app roles (migrate goes through the session pooler on 5432 only; the transaction pooler refuses it), a read-only role, `eru_monitor` (`pg_monitor`), `replicator` (Phase 6). `pgbouncer_auth` gets only the two `auth_query` SECURITY DEFINER functions.
- Guardrails: `idle_in_transaction_session_timeout`, per-role `statement_timeout` defaults, `allow_alter_system = off`, `REVOKE` on `public` schema / database `CONNECT` from `PUBLIC`.
- Logging: PG18 `log_connections` aspects, `log_disconnections`, `log_lock_waits`, `log_temp_files`, `log_checkpoints`, `log_autovacuum_min_duration`, `log_min_duration_statement` per profile. Optional `pgaudit`.
- Host: SSH only on wg0, fail2ban jail on Postgres auth failures, systemd sandboxing overrides where safe.
- `eru doctor --security` runs a CIS-style audit with pass/fail and fixes.

### 6.4 nginx, certificates and `db1.example.com`

- **One hostname per node:** `domain` in `cluster.toml` is the setup's base, and each node is `<node>.<domain>`; the master is `db1.<domain>` by default. One DNS record and one certificate per node, made once; creating, forking or dropping a database never touches DNS or the certificate. The database name in the URL's path routes: `postgres://app_app:…@db1.example.com:6432/app`.
- **Certificates:** eru gets and renews the certificate for the node's hostname itself (`eru node run certs-renew`, hourly), over HTTP-01 on port 80 served by nginx. It installs the certificate for both PgBouncers, nginx and every cluster and reloads them, so clients connect to `db1.example.com:6432/<db>` with `sslmode=verify-full`.
- **Port 443:** nginx's `stream` module reads the TLS ClientHello (`ssl_preread`). PostgreSQL 17+ clients with `sslnegotiation=direct` send ALPN `postgresql`; nginx terminates their TLS and passes them to the transaction pooler over loopback, under the same allowlist as 6432. This helps on networks that only allow 443. Everything else on 443 is HTTPS, where Phase 4 serves the authenticated `/metrics` endpoint.
- **DNS:** the record must point straight at the server ("DNS only" in Cloudflare). A proxied record ends the connection at the proxy, which only passes HTTP.

### 6.5 Database backups and restores

**Model:** pgBackRest with continuous WAL archiving (`archive-async`) to `repo1` = S3/R2, `repo1-cipher-type=aes-256-cbc`, block incremental, optional `repo2` on local disk. **One repository and one cipher, a stanza per database** (`<db>-<6 hex>`, generated at create and immutable), so backups, PITR, retention, verify and drills are all per database. Default schedule: weekly full, daily diff, optional incr; `retention_full = 4` is a **restore window** of four weeks, kept with time-based retention (`repo1-retention-full-type=time`), so extra full backups never shorten it. A database can have its own window and schedules in `[clusters.backup]`. Provider presets set the right options (path-style URIs and region `auto` for R2).

- **Archiving from creation.** Every cluster starts with `archive_mode = on` and `archive_command = '/bin/true'`, and switches to `pgbackrest archive-push` with a **reload** when backups are on. `backup setup` never restarts anything.
- **Staggered.** Each database's timers (`eru-backup-full@<db>`, …) get a fixed offset inside the maintenance window from a hash of its stanza, and the drill a day of the month of its own. A server-wide heavy-I/O lock lets only one full, diff, verify, drill or dump run at a time; a job that meets it is recorded as skipped.
- **Dropped databases stay restorable for their window.** `db drop` keeps the stanza and records when it expires; `backup list` shows it, and a daily timer runs `stanza-delete --force` once the window has passed. A database created again under the same name gets a new stanza.

**Restore is the provider's primitive.** Every restore, fork and drill builds a **new cluster at a point in time** from the database's stanza (or from the live cluster with `pg_basebackup`), registered as `18/<name>` on a temporary port (5480–5499) with archiving off. eru calls this **materialise**. What happens next depends on the command:

| Command | Materialise into | Then |
| --- | --- | --- |
| `eru restore <db>` | `<db>__restoring`, paused at the target, read-only | Inspect (`eru sql --db <db>__restoring` through the session pooler, `restore status`), then cut over in place or abort |
| `eru db fork <src> [--name <new>] [--at <time>]` | `<new>` | Promote at once, rename the database and roles, new passwords, the source's size and overrides, its own stanza and first full: a new database |
| `eru restore drill [--db]` | `<db>__drill` | `pg_amcheck` the database, compare with live, drop |
| `eru db follow` (Phase 6) | `<new>` | Keep streaming from the source |

**The cut-over** (`restore promote`) takes a few seconds plus the replay to the target: the transaction pooler pauses the database, the live cluster stops, the data directories swap (live → `<db>__pre_restore_<ts>`, copy → live path, same filesystem), and `18/<db>` starts with its own configuration and limits, reaches the target and opens on a new timeline, archiving to the same stanza. A full backup starts right after. The URL, roles and passwords don't change. `restore undo` swaps back with the leftover and records the timeline switch in `records.json`, so later restores pass `--target-timeline` instead of following the abandoned branch. A restore needs free space for **one** copy of the database. `restore --dump <id>`, for points older than the window, builds the copy from a dump with the live cluster's roles, and the database gets a new stanza on promote (the old one is kept as dropped for its window).

**Commands** (operator mode; each runs remotely; long ones run as detachable jobs):

```
eru backup setup                          # bucket, cipher, archiving on with a reload, a stanza and first full per database
eru backup now [--db <name>] [--type full|diff|incr]   # without --db: every database, one at a time
eru backup list [--db <name>]             # per database: window, sets, verify, drill, dumps; dropped stanzas
eru backup verify [--db <name>]
eru restore <db> --time <ts>|--latest|--set <label>|--dump <id> [--promote]
eru restore status|promote|undo|abort|cleanup [<db>]
eru restore drill [--db <name>]           # materialise, amcheck, compare, drop
eru db fork <db> [--name <new>] [--at <time>]
eru dump <db> [--to bucket|local] · eru dump list|get
eru job list · eru job follow <id>
eru server init --restore --host root@<new-vps>   # rebuild the master from its bundle (Phase 5)
```

**Safety rules:**

- A restore never touches the live database until promote, and promote keeps the replaced data as a stopped cluster until `restore cleanup` (or `db drop`).
- Every restore reports its target and stops at "ready" so the user can inspect before promoting; `--promote` skips the stop. When only some rows are needed, `db fork --at` is the safer tool.
- While a restore of a database is open, `db drop`, `db fork` and password rotation of that database refuse.
- `db drop` takes a final dump to the bucket first when backups are configured.
- **Protected databases.** `clusters.<db>.protected = true` (set with `eru protect init`, which protects the databases picked or named with `--db`/`--all` once its code is confirmed, `eru db protect <db>…|--all`, `eru db create <db> --protected` or `eru config set clusters.<db>.protected true`) refuses `db drop`, a `db resize` that shrinks, `db credentials --rotate` and `restore promote`/`undo` (and `restore --promote`) with `database_protected`, even with `--confirm`. The node enforces it too. Lifting it (`eru db unprotect <db>…`, one code for every database named, `eru config set clusters.<db>.protected false`, or removing the entry in `config edit`) needs a current 6-digit TOTP code from the authenticator app set up with `eru protect init`: the CLI reads it without echo at an interactive terminal and sends it as a request secret (redacted, never logged), and the node checks it (RFC 6238: HMAC-SHA1, 6 digits, 30-second step, one step of skew either side). A code works once (the node remembers the last accepted step), and 5 wrong codes in a row lock codes out for 15 minutes (`protect_code_rejected`; the lockout is on disk, so it survives a restart). A typed word would prove nothing, since an agent with a terminal can type it too; a TOTP code proves a human with the authenticator approved it. With `--yes`, without a terminal or with structured output lifting is refused outright. Protecting a database is refused until `eru protect init` has run (`precondition_failed`, reason `protect_not_initialized`), so a database is never protected without a way to lift it; a database protected before TOTP existed can only be lifted after `eru protect init`. The secret lives on the node in `/etc/eru/protect.json` (root 0600), never in `cluster.toml`. The honest limit: root on the node can edit `cluster.toml` or that file directly, so this stops agents and slips that go through eru, not someone who owns the server. `db list` has a PROTECTED column and `eru protect status` shows the setup, any lockout and every database with protected yes or no.

**Verification:** weekly `verify` per stanza, a monthly automated **restore drill** per database whose result goes into `records.json`, `eru.prom` and `backup list` (doctor from Phase 4), and metrics for last-backup age per database.

### 6.6 The master backup (Phase 5)

The databases are covered by pgBackRest. What a rebuild also needs is the master's own state that can't be regenerated, and only that: `cluster.toml`, the registries, `node.toml`, `records.json` (timeline switches and dropped stanzas exist nowhere else), the WireGuard private key, the SSH host keys, the fleet key, the certificate and the ACME account. Everything else is rendered by steps, re-sent from the operator's secrets, or is data.

- **Bundle:** a tar of those files with a manifest, age-encrypted to the recovery kit's recipient, uploaded in-process to `<prefix>/master/` after every change (`apply`, `config set`, `db create/drop/fork/resize`, `restore promote/undo`, certificate renewal), with a daily safety-net timer. Retention: every bundle for 30 days, then one a week for a year (`[master_backup]`). Without a bucket the last ten stay on the master, and `server backup export` copies one to the operator's machine.
- **Rebuild:** `eru server init --restore --host root@<new-ip>` fetches and decrypts the bundle on the operator's machine, fences the old master with an epoch in the bucket, places the files before any step runs, and runs every server-level step. The keys match, so the operator's context keeps working. Every database comes back as `awaiting_recovery`, and `eru db recover <db>|--all` materialises each from its stanza.
- **The fence:** `<prefix>/master/epoch.json` names the one instance allowed to write to the bucket. Every job that writes to it checks first, `eru-fence` checks at boot and every five minutes, and a fenced node stops its timers and poolers and refuses `archive-push` and every mutating RPC (`node_fenced`).
- restic and `vps backup|restore` are gone.

### 6.7 Metrics

- Exporters bind to localhost only. nginx exposes one authenticated `/metrics` endpoint (federated) for external scrapers. The credentials are in the secrets document.
- `pg_stat_statements` and `auto_explain` (sampled, threshold-based) are on by default.
- Per database: `postgres_exporter` with one `/probe` target per cluster (fixed by nginx, `/metrics/postgres/<db>`), and eru's textfiles: cgroup memory and pressure (PSI) per cluster, backup facts (last success, size, duration, window) in place of `pgbackrest_exporter`, doctor results and maintenance numbers, each with a `db` label (Phase 4).
- CLI views: `eru status` (a server summary with the memory budget and warnings, then per database: uptime, connections, cache hit, last backup, PITR window, size and limits), `eru top` (live TUI of activity, locks and slow queries), `eru queries` (top statements). `--db` narrows each to one database. All support `--json`.

### 6.8 Extensions

`internal/catalog` holds a curated list. Each entry has the PGDG package name, `shared_preload_libraries` needs, required settings, whether a restart is needed, and a trust note.

Initial catalog: `pg_stat_statements`, `pgvector`, `postgis`, `pg_cron`, `pgaudit`, `pg_repack`, `pg_partman`, `hypopg`, `pg_trgm`, `citext`, `btree_gin`/`btree_gist`, `timescaledb` (licence note).

```
eru ext list | add <name> [--db <db>] | remove <name> [--db <db>]
```

Install is: apt package (server-wide) → render preload/settings into that database's cluster → restart that cluster only if required (needs `--confirm <db>/restart`) → `CREATE EXTENSION` in the database. An extension that needs preloading restarts one database, not the server.

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

Maintenance runs per database's cluster. It sets `lock_timeout` and refuses to run during a backup or a restore drill of that database.

### 6.10 Day-to-day commands

- `eru apply` / `eru plan`: converge nodes to `cluster.toml`.
- `eru config edit`: pull `cluster.toml`, open `$EDITOR`, validate, plan, push.
- `eru doctor`: drift, security, backups, disk, certificates, versions.
- `eru sql`, `eru psql` (system psql through a local forward), `eru console` (usql; eru downloads a pinned, checksum-verified copy if needed and does the verify-full TLS itself), `eru tunnel --db <db>` (forward a local port to the session pooler of the database's node for GUI and migration tools). All reach the session pooler on 5432, which routes by `--db`.
- `eru ssh [node]`: interactive shell over WireGuard.
- `eru upgrade` / `eru node upgrade [--rollback]`: upgrade the CLI and nodes from the operator machine (§5.1).
- `eru db resize <db> --size …`: change a database's size or limits, live where possible.
- `eru protect init [--rotate]`: interactive only. The node makes a 160-bit TOTP secret and keeps it root-only; the CLI shows it once as a terminal QR code (and in base32) and asks for a code before the node uses it. With a secret already set up it is refused unless `--rotate`, which asks for a current code from the old secret first (the old one keeps working until the new one is confirmed). `eru protect status` shows whether it is set up, any lockout, and the protected databases.
- `eru db create <db> [--protected]`: `--protected` needs `eru protect init` first. Prints the credentials once, in a framed block on a terminal (plain lines otherwise). If the connection is lost after the request was sent, it still prints them: the node keeps creating the database and `eru apply` finishes it.
- `eru pg update`: controlled minor upgrade (apt unhold → upgrade → a rolling restart of the clusters, one at a time, in the window → re-hold), with a pre-upgrade backup.

## 7. Roadmap and status

This section is the only place that tracks progress. Update it whenever a module or phase changes state.

**Rules**

1. **One phase at a time.** A phase starts only when the previous one is done, and there's no overlap. "Done" means its exit check passes.
2. **Each phase is a set of modules.** A module is a Go package, or a group of steps, with one clear job.
3. **Each phase leaves something that works.** From Phase 2 on, every phase ends with a manual test on a real VPS (§8).
4. **HA is last.** Extensions come late. Anything unscheduled goes in the backlog.

Legend: ✅ done · 🟡 in progress · ⬜ not started

### 7.1 Current state (2026-10-15)

- **Phase 0 is done.** The CLI, releases and the node engine are on `master`, the first edge release has been published, and the incus test suite has been removed.
- **Phases 1 and 2 are done (2026-10-12, Hetzner, Ubuntu 26.04 and Debian 13).** `server init` (then `cluster init`) goes from a fresh server to public 22 closed, SSH over WireGuard and a working PostgreSQL 18 cluster. The ACME certificate is issued, the secrets handoff is accepted by `eru login`, `eru sql` connects with verify-full, `eru apply --auto-upgrade-nodes` upgrades the node, and an app connects to 6432 through PgBouncer with `sslmode=verify-full`. The checks not yet run on a VPS (lockdown self-revert, `--local`, a no-op `apply`, `db drop` refusal, `eru console`, direct TLS on 443) are in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md) and don't block Phase 3.
- **Phase 3 was re-scoped on 2026-10-13** by [`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md) to **Databases as clusters, backups and native restore**, before its first manual test had passed. It was rebuilt in the units of [`PHASE-3-WORK-UNITS.md`](./PHASE-3-WORK-UNITS.md) (which replaces `PHASE-3-PLAN.md`) and **passed the RFC's exit test on 2026-10-14** on a rebuilt Hetzner VPS (Ubuntu 26.04, R2), see [`MANUAL-TESTING.md`](./MANUAL-TESTING.md). Deferred to a later rebuild: the Phase 1 lockdown revert and `server init --local`, and the macOS edge install.
- **The order after Phase 3 was changed on 2026-10-14** to observability, master backup and disaster recovery, followers, extensions (Phases 4–7), so that eru can run in production sooner.
- **Phase 4 is built (2026-10-14)** in the units of [`PHASE-4-WORK-UNITS.md`](./PHASE-4-WORK-UNITS.md): the exporters and the authenticated `/metrics` endpoint, `status`, `doctor` (also the gate after `apply` and in `server init`), `queries`, `top`, `maint` and the maintenance timers. Its manual test in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md) has not run yet.
- **Phase 5 is built (2026-10-15)** in the units of [`PHASE-5-WORK-UNITS.md`](./PHASE-5-WORK-UNITS.md): the master bundle after every change with its retention and local copies, the epoch fence (with the `archive-push` wrapper), `server backup now|list|verify|export`, `server init --restore`, `db recover`, `context set-address`, and the doctor, metrics and status hooks. restic and `vps backup|restore` are gone. Its manual test in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md) has not run yet; v1 (alpha) needs it and Phase 4's.
- One open item from Phase 0, also in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md): install from the edge channel once on Linux and once on macOS, then run `eru upgrade` against a second edge release.
- About 30 of the ~65 commands (before Phase 3) do real work today: `version`, `commands`, `guide agents`, `skill install`, `upgrade`, `login`, `context list|show|use|remove`, `secrets export`, `server init` (remote and `--local`, `--resume`), `plan`, `apply`, `config show|get|set|edit|validate`, `db list|create|drop|credentials`, `sql`, `psql`, `console`, `tunnel`, `ssh`, `logs`, `tune show|explain|apply`, `node rpc`, `node version`, `node selfcheck`, `node run certs-renew`, `node ssh-revert` and `node unlock-ssh`. Phase 3 adds `db resize`, `db fork`, `backup setup|now|list|verify`, `restore <db>|status|promote|undo|abort|cleanup|drill`, `dump`, `dump list|get`, `job list|follow`, the backup `node run` jobs and `node dump`, and turns the existing commands per database (built, manual test pending). The rest return `not implemented yet` until their phase lands.

### 7.2 Overview

| # | Phase | What you can do at the end | Status |
| --- | --- | --- | --- |
| 0 | Foundations | Install and self-upgrade the `eru` CLI; a node can run steps | ✅ |
| 1 | Bootstrap | Turn a fresh VPS into a hardened, WireGuard-only host and get a secrets document | ✅ |
| 2 | First server | Set up a new server with a working PostgreSQL 18 database. **Manual testing starts here** | ✅ |
| 3 | Databases as clusters, backups & native restore | Each database its own cluster with a size and limits, two poolers, one hostname per node; a stanza per database, native PITR restore with an in-place cut-over, native `db fork`, drills per database | ✅ |
| 4 | Observability & maintenance | `status`, `top`, `doctor`, metrics, maintenance commands and timers | 🟡 |
| 5 | Master backup & disaster recovery | The master's state backed up on every change; rebuild a dead master on a fresh VPS, then recover each database from its stanza | 🟡 |
| 6 | Followers | Per-database physical standby followers; unfollow one to rehearse a risky migration on current data | ⬜ |
| 7 | Extensions | Extension catalog and `eru ext` | ⬜ |
| 8 | Upgrades | Upgrade and roll back node binaries and Postgres minors from the laptop | ⬜ |
| 9 | Operators & agents | Teammate and agent identities, read-only agents, full agent guide, alerting | ⬜ |
| 10 | Multi-node & replicas | Worker nodes driven from the master, a node for one database, `db move`, followers and replicas on other nodes | ⬜ |
| 11 | High availability | Patroni per cluster + etcd, automatic failover; the master stops being a single point | ⬜ last |

**v1 is Phases 0–5**, with observability and the master backup (disaster recovery) as release requirements. Followers (Phase 6), extensions (Phase 7), upgrades, operators and agents, multi-node and HA come after the first release.

**v1 is an alpha: nothing is frozen.** `cluster.toml`, the secrets document (`eru-secrets/v1`), the bundle format (`eru-master-bundle/v1`), the `eru/v1` JSON and the step IDs may change between alpha releases, and a later alpha release may require rebuilding a server with a fresh `server init` and restoring its databases. Compatibility promises start after the alpha.

[`IMPLEMENTATION.md`](./IMPLEMENTATION.md) specifies Phases 0–5, 7 and 8 in detail, plus the agent guide (Phase 9). Phases 6 and 9–11 get their own design before they start.

### 7.3 Phase 0 — Foundations ✅

The CLI contract, releases and the node-side engine. No server work.

| Module | What it does | Status |
| --- | --- | --- |
| `internal/cli`, `cli/output` | Cobra tree, `CommandMeta` + `--check-meta`, output modes, error/exit codes, `commands --json`, `skill install`, `guide agents` (stub text) | ✅ |
| `internal/release`, `scripts/`, publish workflows | Build, sign, upload, `install.sh`, sigstore verification, edge + per-major stable channels (`stable-1.x`), pre-release versions | ✅ edge published; stable tooling ready, not yet published |
| `internal/upgrade` (local) | `eru upgrade` for the local CLI, keeping `.prev` | ✅ |
| `internal/localenv` (escalation) | Sudo re-exec | ✅ |
| `internal/engine` | Steps, plan/apply runner, journal, locks, run logs, shell, redaction | ✅ |
| `internal/node`, `internal/rpc` | `eru node rpc` dispatcher with version check, `selfcheck`, `upgrade.install` | ✅ |

### 7.4 Phase 1 — Bootstrap ✅

From a laptop, take a fresh Debian/Ubuntu VPS to "hardened, SSH only over WireGuard, secrets document in hand".

| Module | What it does | Status |
| --- | --- | --- |
| `internal/transport` | SSH client with TOFU host-key pinning; embedded userspace WireGuard | ✅ |
| `internal/remote` | Binary push, RPC client, version handshake, `--auto-upgrade-nodes` | ✅ |
| `internal/secretsdoc`, `internal/contexts` | age secrets document, contexts, `eru login`, `eru context *` | ✅ |
| `internal/localenv` (preflight) | `missing_tool` checks and install offers; the pinned, checksum-verified usql download for `eru console` | ✅ |
| Steps 1–6 | Base packages, admin user, WireGuard, sshd hardening + **commit-confirm** move to wg0, ufw/fail2ban, journald/swap/unattended-upgrades | ✅ |
| `server init` (host part) | Remote and `--local`, `--resume`, `node unlock-ssh`, the `install.sh` init prompt | ✅ |

Built alongside, ahead of their phase because Phase 1 needs them: the Phase 1 subset of `cluster.toml` with validation (`internal/config`), OS and RAM detection (`internal/hostinfo`), and `eru plan` / `eru apply` over the tunnel (they run steps 1–6 and are how an operator first reaches a `--local` node).

**Exit:** `eru server init --host root@<vps>` ends with public 22 closed, SSH working over WireGuard, and a secrets document that `eru login` accepts on a second machine. A broken tunnel during lockdown reverts by itself (not yet forced on a VPS; see [`MANUAL-TESTING.md`](./MANUAL-TESTING.md)).

### 7.5 Phase 2 — First server ✅

`server init` now finishes with a usable PostgreSQL. (Phase 2 ran one shared cluster, `18/main`; Phase 3 replaced it with a cluster per database.)

| Module | What it does | Status |
| --- | --- | --- |
| `internal/config` | Strict `cluster.toml` schema, defaults and validation; dotted-key `get`/`set` | ✅ |
| `internal/hostinfo` | CPU, RAM, disk, kernel and OS detection, public IPv4/IPv6 | ✅ |
| `internal/tune` | Tuning calculator; `eru tune explain/apply` | ✅ |
| `internal/render` | conf.d, pg_hba, pg_ident, pgbouncer.ini, pgbouncer_hba.conf, nginx.conf, eru-certs units, unified `Diff` | ✅ |
| `internal/pg`, `internal/state` | pgx admin client (local psql + remote pgx), desired-state store | ✅ |
| Steps 7–13 | Kernel/sysctl, PGDG + PG18, cluster create, managed config, roles and databases, nginx + ACME certificates, PgBouncer | ✅ |
| Commands | `plan`, `apply`, `config *`, `db *`, `sql`, `psql`, `console`, `tunnel`, `ssh`, `logs`, `tune *` | ✅ |

**Manual test:** on a fresh VPS, run `server init`, then connect an app to `data.example.com:6432` with `sslmode=verify-full`. `eru sql --db app "select 1"` works. Re-running `eru apply` changes nothing, and `db drop` with only `--yes` is refused. Passed 2026-10-12 (Hetzner, Ubuntu 26.04 and Debian 13) apart from the checks listed in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md).

### 7.6 Phase 3 — Databases as clusters, backups & native restore ✅

Re-scoped by [`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md) and built in the units of [`PHASE-3-WORK-UNITS.md`](./PHASE-3-WORK-UNITS.md).

| Module | What it does | Status |
| --- | --- | --- |
| Config and naming | `server init` (was `cluster init`, `--data-root`, `--node-name`, `--default-size`); contexts, `node.toml` and the secrets document name servers; the master at `db1.<domain>`; `[[clusters]]` with sizes, limits, pools and `[clusters.backup]`; `config set clusters.<db>.*`; `records.json` split from `state.json`; the recovery identity at `server init` and the `no_offsite_backup` warning | ✅ |
| Per-cluster steps | Step instances (`<step>@<db>`) in the engine and journal, `apply --db`, repeatable `--confirm <db>/restart`; per-cluster locks; socket-only `postgres-cluster-v1@<db>` with archiving on from creation; the memory budget in `internal/tune`; `vm.nr_hugepages` as a sum; cgroup drop-ins | ✅ |
| Routing | Both PgBouncers with a `[databases]` entry and `auth_dbname` alias per database; the session pooler's unit and auth function; one hostname per node; operator connections through the session pooler with the placement cache | ✅ |
| Database lifecycle | `db create` (size, `memory_overcommitted` refused unless `--allow-overcommit`, `would_not_start`), `db drop` (stanza kept), `db resize`, `db list`; oversubscription warnings and the `tune show` budget sheet | ✅ |
| Backups per stanza | Multi-stanza `pgbackrest.conf`, reload-only `backup setup`, template timers with staggering and the heavy-I/O lock, `backup now|verify|list` per database, `db` labels in `eru.prom`, dropped-stanza expiry | ✅ |
| Native restore | Materialise; `restore <db>` to a paused `<db>__restoring` in the session pooler; promote with the data-directory swap; undo with timeline recording; abort and cleanup; `--dump` with a new stanza | ✅ |
| `db fork` | Materialise from the stanza (`--at`) or `pg_basebackup`, promote, rename, new passwords, the source's size and overrides, its own stanza and first full | ✅ |
| Restore drill | Per database: materialise `<db>__drill`, `pg_amcheck`, compare with live, drop; `restore drill --db`; the drill timer staggered over the month | ✅ |
| Per-database policies | `[clusters.backup]` window, schedules and long-retention dumps: age-encrypted `pg_dump` to the bucket on a schedule, `dump list/get`, `restore --dump`, final dump on `db drop` | ✅ |

**Manual test (the RFC's exit test, on a real VPS with R2):** rebuild `mbvlabs`. Create `app` and `other` (two clusters), check the memory split and the cgroup limits, and that `other` keeps serving while `app` restarts. Run a memory-heavy query in `app` and see it throttled while `other` stays responsive. Connect a migration tool through `db1.<domain>:5432/app` and an application through `db1.<domain>:443/app`. Back up, write rows to `app`, `restore app --time` and inspect the paused copy, then promote while `other` keeps writing. Undo and cleanup. Fork `app` at a time and without one. A drill per cluster. Drop a database and see its stanza stay listed. Change a restart-only setting of one cluster and check that only it restarts (a preload extension waits for Phase 7). Create a third database that would overcommit the server and see it refused with the size that fits, then resize one down. A second `apply` changes nothing. The checklist, with what to verify at each step, is in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md). Passed on 2026-10-14.

### 7.7 Phase 4 — Observability & maintenance 🟡

| Module | What it does | Status |
| --- | --- | --- |
| Step 16 | `metrics-v1`: pinned node_exporter, postgres_exporter (a `/probe` per cluster) and two pgbouncer_exporters on loopback in `eru-exporters.slice`; the authenticated `/metrics` endpoint on nginx (bcrypt basic auth, PROXY protocol on the internal hops, optional allowlist, fail2ban) with `targets.json` for `http_sd`; `eru metrics show|rotate`; eru's textfiles in place of `pgbackrest_exporter` | ✅ built, manual test pending |
| Status views | `status`, `top`, `queries`, `doctor` (every check, `--only security`, operator-side checks), per database with a server summary; doctor reads cgroup pressure, the memory budget, `CommitLimit`, disk use (fails at 90%) and the offsite-backup state | ✅ built, manual test pending |
| `internal/maint` | `maint vacuum/analyze/reindex/report/kill` per database (jobs as `eru_admin`, heavy-I/O lock), the ioguix bloat estimate, `maint repack` refusing until Phase 7; `maint-daily` and `maint-weekly`; `--defer-restart` restarts at the window's start | ✅ built, manual test pending |
| Step 18 and timers | `doctor` as the gate after `apply` (warnings) and `server init`'s database phase (fails); `eru-maint-daily`, `eru-maint-weekly`, `eru-metrics-refresh` (every minute), `eru-doctor` (hourly) | ✅ built, manual test pending |

`maint repack` and auto-repack need `pg_repack`, which `eru ext add` brings in Phase 7. Until then `maint repack` fails with `precondition_failed`, and the repack is tested in Phase 7.

Built in the units of [`PHASE-4-WORK-UNITS.md`](./PHASE-4-WORK-UNITS.md) (2026-10-14); it stays 🟡 until the manual test passes.

**Manual test:** scrape `/metrics` with and without credentials. `doctor` is clean on a fresh server with two databases. Run a bloat report. The checklist is in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md). Not run yet.

### 7.8 Phase 5 — Master backup & disaster recovery 🟡

This phase is what eru needs to run in production. It comes before followers and extensions, so the bundle and `db recover` cover databases as Phase 3 left them; Phases 6 and 7 extend them to followers and extensions.

Built in the units of [`PHASE-5-WORK-UNITS.md`](./PHASE-5-WORK-UNITS.md) (2026-10-15); it stays 🟡 until the manual test passes.

| Module | What it does | Status |
| --- | --- | --- |
| Master bundle | `internal/masterbackup` and step 15 `master-backup-v1`: the age-encrypted bundle of the master's own state, the dirty flag and `eru-master-backup.path` (every change, debounced), the daily safety net, retention in the bucket and local copies | ✅ built, manual test pending |
| The fence | The epoch in the bucket, `eru-fence` at boot and every five minutes, the `archive-push` wrapper, `node_fenced` | ✅ built, manual test pending |
| Commands | `server backup now|list|verify|export`, `server init --restore [--bundle <name\|time> \| --bundle-file <file>]`, `db recover <db>…|--all`, `context set-address` | ✅ built, manual test pending |
| Phase 4 hooks | doctor `master.*` checks, `eru-master-backup.prom`, the status row, databases awaiting recovery skipped | ✅ built, manual test pending |
| Dropped | restic, `vps backup|restore`, `--vps-backup-schedule`, `--restore-time`, `[backup.vps]` | ✅ |

**Manual test:** rebuild `mbvlabs` from scratch, make changes, destroy the VPS, run `server init --restore` on a new one, then `db recover --all`. The operator's context works without edits, records (timeline switches, dropped stanzas) are intact, the old master's jobs stop on the epoch, and the measured RTO goes into the risks below. Then the same without a bucket: `server backup export`, destroy, `server init --restore --bundle-file`.

### 7.9 Phase 6 — Followers ⬜

Heroku-style followers for rehearsing risky migrations on production data. **Decided: physical standbys.** A follower is a cluster of its own that streams from one source database's cluster; `wal_level` drops from `logical` to `replica`. Logical replication stays possible later as an opt-in.

| Module | What it does | Status |
| --- | --- | --- |
| Followers | `db follow <db> --name <new> [--size]` materialises from the source's stanza (or `pg_basebackup` without backups), sets `primary_conninfo` and keeps streaming with `hot_standby = on`, through a physical slot capped by `max_slot_wal_keep_size`. A pooler alias (`<new> = … dbname=<db>`) makes it `db1.<domain>/<new>`, read-only with the source's credentials. `db unfollow <follower>` promotes it into an independent database: the fork's renames, a new stanza and a first full | ⬜ |
| Checks | `db list`, `status` and `doctor` show the source, the lag in bytes and seconds, and a slot holding WAL; dropping a source with followers is refused | ⬜ |
| Commands | `db follow`, `db unfollow` | ⬜ |
| Recovery | `server init --restore` and `db recover` bring a follower back as a follower of its source (the master backup came first, in Phase 5) | ⬜ |

**Manual test:** follow a database under write load and watch the lag reach zero. Unfollow it, check row counts and sequence values against the source, run a migration on the copy, then drop it.

### 7.10 Phase 7 — Extensions ⬜

`internal/catalog` and `ext list/add/remove` per database, including extensions that need a restart: the package install is server-wide, the preload and the restart affect only that database's cluster. `pg_stat_statements` and `auto_explain` are already on by default from Phase 2.

It also brings what Phase 4 left waiting for `pg_repack`: `maint repack` and auto-repack. A master rebuild (Phase 5) reinstalls each database's extension packages from the desired state.

**Manual test:** add a catalog extension that needs a preload to one database, apply, and check it in `pg_extension`; only that database restarts. Remove it again. Add `pg_repack` to a database and run `maint repack`.

### 7.11 Phase 8 — Upgrades ⬜

| Module | What it does | Status |
| --- | --- | --- |
| `internal/upgrade` (nodes) | `eru upgrade` also upgrades nodes; `node upgrade`, `--rollback` | ⬜ |
| `pg update` | Postgres minor upgrade with a pre-upgrade backup, then a rolling restart of the clusters, one at a time. Major upgrades (backlog) become feasible per database | ⬜ |

**Manual test:** publish version N+1 and run `eru upgrade`. Only the changed steps re-run. `node upgrade --rollback` brings back version N.

### 7.12 Phase 9 — Operators & agents ⬜

`operator add/revoke`, scoped read-only operators (scopes can be per database: an agent that can read `app` only), the full `eru guide agents` text, a generated `docs/CLI.md`, alerting (webhook, ntfy, email) on backup age, archive lag, disk, wraparound, certificate expiry and failed drills, config drift detection, provider snapshots, and Ubuntu support checks.

**Manual test:** a read-only agent operator can't change anything; each alert fires once when its condition is forced.

### 7.13 Phase 10 — Multi-node & replicas ⬜

Workers driven from the master (`node add|list|remove`, `--dedicated <db>`), each with one hostname; hub routing through the master and the fleet SSH key; forwarding for `sql`, `psql`, `console` and `tunnel`; `node` placement, `db create --node`, `db move` and what happens to the old URL during a move; `server takeover --node <worker>`; followers and replicas on other nodes. Tailscale becomes available as a transport option here.

### 7.14 Phase 11 — High availability ⬜ (last)

The master stops being a single point: desired state and records in a replicated store, or a standby master following the bundles; separate keys per master behind a load balancer or a shared name, and operator-signed revisions. Patroni per cluster with a shared etcd (3 voters), adopting the existing clusters into Patroni, sync standby, switchover/failover commands, and the routing strategy (libpq multi-host URLs, or every node's poolers routing a database to its current primary). HA might be offered only for databases on dedicated nodes.

**Exit:** kill a database's primary under load. Writes resume in under 30 s with no lost committed transactions in sync mode, the other databases aren't affected, and the CLI keeps working against the new primary.

### 7.15 Backlog (unscheduled)

More PostgreSQL versions, major upgrades (`pg_upgrade --link` or logical blue/green), logical replication tooling, PgDog / read-write split, `eru mcp`.

## 8. Testing strategy

There's no automated test suite. The incus VM suite was dropped on 2026-10-10 because it was too flaky to trust.

- **CI** runs `just lint` (golangci-lint, shellcheck, actionlint, `--check-meta`, `--check-steps` and the cosign pin check) and builds for all four OS/arch targets. Both are fast and deterministic.
- **Manual testing** starts at the end of Phase 2, once eru can set up a new server with a database. Each phase from then on ends with the manual test listed for it in §7. Run it on a real VPS, and record the date, eru version and VPS (provider, OS, size) next to the phase when it passes. Checks that couldn't be run before a phase was closed go in [`MANUAL-TESTING.md`](./MANUAL-TESTING.md).
- **No unit tests.** The behaviour that matters is what happens on a real host.
- **The agent JSON contract** is described in `schema/eru-v1.json`. Bump `"schema"` when envelopes change incompatibly.

## 9. Risks

- **Lock-out during bootstrap.** Moving SSH onto WireGuard can lock the operator out. Mitigated by commit-confirm with automatic revert, `eru node unlock-ssh` via the provider console, and tests that force the failure.
- **The secrets document is a single high-value file.** It's age-encrypted, operators are per-person and revocable, and the recovery kit can be exported separately for cold storage.
- **pgBackRest project health.** Crunchy Data, its main sponsor, was acquired by Snowflake in 2025. On 2026-04-27 the maintainer archived the project after losing sponsorship and announced 2.58.0 as the last release; on 2026-05-18 a sponsor group (AWS, TigerData, Supabase, Percona, pgEdge) funded it again, and 2.59.x has shipped since (2.59.0 adds PostgreSQL 19). 2.59.3 fixes an unchecked error when generating random data for repository encryption; eru always encrypts, so **2.59.3 is the floor** (`versions.PgBackRestMinVersion`, checked by step 8). Re-check before each phase that depends on it. The `backup.Driver` interface keeps WAL-G as a fallback (not built).
- **Agents with write access.** Destructive operations need named confirmation tokens, and read-only operator scopes arrive in Phase 9. The agent guide explains both.
- **The master is a single point of failure.** In Phase 3 it's the only server, so losing it loses everything until the rebuild; from Phase 10, losing it freezes every change and the operator's normal access to workers, while workers keep serving. Database RPO is `archive_timeout` (60 s). Until Phase 5, recovery is a new server and a hand restore of each stanza; Phase 5's bundle brings state RPO to about zero, and its manual test measures the RTO, which goes here.
- **Split brain on a rebuild** (Phase 5): the epoch fences an old master that comes back, `archive-push` included (the wrapper refuses once the node has seen the newer epoch), but it can push WAL for up to five minutes before its first check, or longer while it can't read the bucket. `server init --restore` refuses while the old master answers over WireGuard, says "power it off first", and `db recover` refuses a stanza that received WAL after the fence (`stanza_still_written`).
- **Compromising the master compromises the fleet** (Phase 10): it holds the fleet key and every node's desired state. HA brings separate keys per master and operator-signed revisions.
- **Isolation is soft.** `MemoryHigh` throttles a cluster, it doesn't partition the server; disk space and the page cache are shared, and `IOWeight` only works with BFQ or `io.cost` (to verify on Debian 13 and Ubuntu 26.04; `tune show` reports "not enforced"). A database that fills the disk affects its neighbours. The README says so.
- **Density and oversubscription.** About 50–100 MiB fixed per idle cluster plus its buffers, to be measured in the Phase 3 manual test; the size table and the README's density guidance are guesses until then. Warnings never block, so a user who overcommits a small VPS gets a slow server.
- **Things the Phase 3 build couldn't verify without a server**, each in the manual test: per-entry `auth_dbname` in PgBouncer (fallback: the auth functions in each application database); two PgBouncers side by side; the timeline after `restore undo` (the subtlest part); the hot-standby floors of a paused copy; a fork's shared system identifier in its own stanza; cgroup accounting of shared memory.
- **More moving parts per server:** N clusters, N stanzas, two poolers, per-cluster timers and cgroups. Server-wide changes (a certificate, a minor update) touch each cluster in turn, and HA (Phase 11) gets heavier with a Patroni scope per cluster.
- **A database that moves changes its URL's host** (Phase 10), and failover to another node would too unless the HA design routes around it.
- **OOM on small VPSes.** Sensible overcommit settings, the server reserve, per-cluster `MemoryHigh`, and `would_not_start` against `CommitLimit` before a cluster starts.
