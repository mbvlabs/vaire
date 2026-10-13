# eru

One CLI that turns a fresh VPS into a hardened, tuned PostgreSQL 18 server, and then stays the only way in.

From your laptop (or from a coding agent), `eru` takes a new Debian 13 or Ubuntu 26.04 server, locks it down, installs and tunes PostgreSQL 18 with PgBouncer and nginx, issues its TLS certificate, and connects you to it over WireGuard. Every later operation goes through the same binary, over SSH, inside WireGuard. There is no daemon, no web UI and no open control port.

> **Status:** early and under active development. Bootstrap and the first cluster (Phases 1 and 2) work end to end on a real VPS. Database backups and per-database restores (Phase 3) are implemented and being tested on a VPS. Upgrades, observability and multi-node are still to come. See [the roadmap](docs/MASTER.md#7-roadmap-and-status).

## What works today

| Area | Commands |
| --- | --- |
| Setup | `eru server init` (remote over SSH, or `--local` on the server), `--resume` |
| Access | `eru login`, `eru context list\|show\|use\|remove`, `eru ssh`, `eru tunnel` |
| Desired state | `eru plan`, `eru apply`, `eru config show\|get\|set\|edit\|validate` |
| Databases | `eru db list\|create\|drop\|credentials` |
| SQL | `eru sql` (one statement, JSON-friendly), `eru psql`, `eru console` (usql) |
| Tuning | `eru tune show\|explain\|apply` |
| Backups | `eru backup setup\|now\|list\|verify`, `eru dump`, `eru dump list\|get` |
| Restores | `eru restore <db>` (`--time`, `--latest`, `--set`, `--dump`), `eru restore status\|promote\|undo\|abort\|cleanup\|drill`, `eru db fork` |
| Jobs | `eru job list\|follow` |
| Operations | `eru logs`, `eru secrets export`, `eru upgrade` |
| Agents | `eru commands --json`, `eru guide agents`, `eru skill install` |

Commands from later phases are already in the tree and return `not implemented yet`.

## Quick start

```sh
# On your laptop: install the CLI (signed release, verified with cosign)
curl -fsSL https://eru.mbvlabs.com/install.sh | bash

# Bootstrap a fresh server (provider root key or password for first contact)
eru server init --host root@203.0.113.10

# eru asks a few questions, shows the DNS records to create, hardens the box,
# closes public SSH, installs PostgreSQL, and ends with the secrets handoff.

eru sql --db app "select now()"
eru console app                # interactive usql session
eru console app --write        # run DDL as the database owner
```

On another machine, `eru login` and paste the transfer bundle from the handoff.

## How it fits together

```
 laptop / CI / agent                                     VPS (central command)
┌───────────────────────────────┐                      ┌─────────────────────────────────────────┐
│ eru (operator mode)           │                      │ wg0 10.42.0.1                           │
│  ├ context + credentials      │  WireGuard  UDP 51820│  └ sshd (port 22 reachable on wg0 only) │
│  ├ userspace WireGuard  ══════╪═════════════════════▶│                                         │
│  │  (no root, no wg0 needed)  │                      │ eru (node mode), run per command:       │
│  ├ in-process SSH  ───────────┼──── inside WG ──────▶│  ├ versioned steps + embedded scripts   │
│  └ pgx / usql / psql ─────────┼──── inside WG ──────▶│  ├ /etc/eru/cluster.toml (desired state)│
└───────────────────────────────┘     (5432 / 6432)    │  ├ journal of applied steps             │
                                                       │  └ systemd timers → eru node run …      │
                                                       │                                         │
                                                       │ PostgreSQL 18 · PgBouncer · nginx       │
                                                       └─────────────────────────────────────────┘
```

- **One binary, two roles.** On your machine `eru` is the operator CLI. On the server the same binary runs as `eru node …`, called over SSH for each command and by systemd timers. Operator and node versions are kept in lockstep: a mismatch is refused unless you pass `--auto-upgrade-nodes`, which pushes the matching signed build.
- **Desired state in, convergence out.** The cluster is described by one TOML file on the server. `eru plan` shows what would change and `eru apply` converges, and it's always safe to run again. Steps have versioned IDs (`tls-v1`), and a journal records what ran, so an upgrade only re-runs the steps that changed.
- **eru owns the config it writes.** Managed files are rendered from templates and checked for drift. `ALTER SYSTEM` is disabled so nothing changes behind eru's back.

## Security design

### Network exposure

After bootstrap the server has exactly these public ports open, enforced by ufw (`default deny incoming`):

```
                       Internet
                          │
      ┌─────────┬─────────┼──────────┬─────────────────────────┐
      │         │         │          │                         │
  51820/udp   80/tcp   443/tcp   6432/tcp, 5432/tcp       22/tcp
  WireGuard   ACME     nginx     only from client_allowlist CLOSED
      │       HTTP-01  stream    (expose_pgbouncer /       (wg0 only)
      │         │         │       expose_postgres)
      ▼         ▼         ▼          ▼
 ┌───────────────────────────────────────────────────────────────┐
 │ wg0 10.42.0.1 ── sshd :22, PgBouncer :6432, PostgreSQL :5432  │
 │                  (WireGuard peers always reach these)         │
 └───────────────────────────────────────────────────────────────┘
```

- **SSH only over WireGuard.** Port 22 is allowed only `in on wg0`. Root login and password authentication are off, and only the admin user may log in, with the operator's key.
- **Commit-confirm lockdown.** Before eru closes public SSH, it snapshots sshd and firewall state and arms a server-side timer. If eru doesn't confirm a working SSH-over-WireGuard session within two minutes, the server reverts on its own. A broken tunnel can't lock you out.
- **Break-glass** is the provider console plus `eru node unlock-ssh`, which re-opens public SSH with a password for the admin user for a fixed window. The steps are in the secrets handoff.
- **Database ports are closed to the public by default.** WireGuard peers can always reach 6432 and 5432. `client_allowlist` with `expose_pgbouncer` / `expose_postgres` opens them to specific CIDRs.
- **fail2ban** watches sshd. unattended-upgrades installs security updates only, with PostgreSQL, PgBouncer and nginx held for controlled updates.

### TLS and nginx

```
 client (sslmode=verify-full)                          VPS
                                ┌──────────────────────────────────────────────┐
 ── :6432 ─────────────────────▶│ PgBouncer (TLS) ──unix socket──▶ PostgreSQL  │
 ── :5432 ─────────────────────▶│ PostgreSQL (TLS, hostssl only)               │
                                │                                              │
 ── :443  ─────────────────────▶│ nginx stream, ssl_preread (reads ALPN):      │
   (sslnegotiation=direct,      │   ALPN "postgresql" + allowed source         │
    PG17+ clients)              │      → terminate TLS → PgBouncer :6432       │
                                │   other PostgreSQL clients → closed          │
                                │   anything else → HTTPS 404 (later /metrics) │
                                │                                              │
 ── :80   ─────────────────────▶│ nginx: /.well-known/acme-challenge/ only     │
                                └──────────────────────────────────────────────┘
```

- **eru is its own ACME client.** An hourly `eru-certs` timer issues and renews the certificate for your domain over HTTP-01, installs it for PostgreSQL, PgBouncer and nginx, and reloads them. Until the first certificate arrives, a placeholder certificate lets everything start.
- The DNS record must point straight at the server (DNS only, not proxied).
- A `self-signed` mode exists for servers without a domain. Connections are encrypted but not verified, and eru warns about that.

### PostgreSQL hardening

- `hostssl` only for TCP, `scram-sha-256` everywhere, and a terminal `reject`. The `postgres` superuser is local-socket only and rejected over TCP.
- `allow_alter_system = off`, `REVOKE` of `CONNECT` and of `public` schema rights from `PUBLIC`, `idle_in_transaction_session_timeout`, and connection and lock logging.
- Settings are tuned from the hardware and workload profile (`eru tune explain` shows why each value was picked).

### Roles per database

```
 <db>_owner   NOLOGIN, owns the database and every object in it
    ▲
    │ member, SET role = <db>_owner on login
 <db>_migrate   DDL: migrations              → :5432 (direct only; PgBouncer refuses it)
 <db>_app       SELECT/INSERT/UPDATE/DELETE  → :6432 (PgBouncer), statement_timeout
 <db>_ro        SELECT, read-only by default → :6432 (PgBouncer), reporting and read-only clients

 eru_admin        CREATEDB/CREATEROLE, not superuser (operator)
 pgbouncer_auth   may only call the auth_query function
 eru_monitor      pg_monitor
```

Run migrations as `migrate` and the application as `app`. PgBouncer refuses the `migrate` role: in transaction mode, advisory locks, session `SET` and temporary tables can land on a different connection, so migrations connect to PostgreSQL on 5432. From outside WireGuard, use `eru tunnel --target postgres` or set `expose_postgres = true`. A compromised app can change data but can't drop or change the schema. Default privileges give `app` and `ro` access to every new table without extra grants.

### Secrets

- eru generates every password. You never type one in.
- The server keeps only SCRAM verifiers, plus the two service passwords PgBouncer and the exporters need at runtime.
- At the end of `server init`, eru prints the **secrets handoff** once:
  - every secret in plain text, grouped for a password manager;
  - an ASCII-armored `age` transfer bundle (scrypt), with a generated passphrase.
- You type `stored` to confirm you've saved it, and eru offers to clear the screen. Nothing is written to disk unless you ask with `--secrets-out`.
- `eru login` on any machine takes the bundle and passphrase and sets up a **context**: credentials under `~/.config/eru/contexts/<cluster>/`, files 0600.
- Releases are signed with cosign. `eru upgrade` and node upgrades verify the signature and the sha256 before swapping binaries.

### Operator connections

```
 eru console app
   │
   ├─ usql ──plaintext──▶ 127.0.0.1:<port>   (loopback only)
   │                          │
   │              eru: SSLRequest + TLS handshake, verify-full
   │                  against the cluster's domain certificate
   │                          │
   └──────────── userspace WireGuard ──▶ 10.42.0.1:5432
```

`eru sql`, `eru psql` and `eru console` all connect over the tunnel to 5432 and verify the certificate (`verify-full`). They log in as `eru_admin`, never as an application role, and are read-only unless `--write` is passed; with `--write` they act as `<db>_owner`, so new objects get the same owner and default grants as a migration. `eru tunnel --target postgres` forwards a local port to 5432, which is how a migration tool on your machine reaches the database as `<db>_migrate` without opening 5432 to the internet. `eru console` uses usql from PATH, or downloads a pinned release and checks its SHA-256.

## Backups and restores

```
 VPS                                                   S3-compatible bucket (R2, S3, B2, …)
┌────────────────────────────────────────┐           ┌──────────────────────────────────────┐
│ PostgreSQL ──WAL, continuously (≤60 s)─┼──────────▶│ <prefix>/pgbackrest/                 │
│      │                                 │           │   full · diff · incr backup sets     │
│      └─ eru-job timers ─ pgBackRest ───┼──────────▶│   archived WAL                       │
│           Sun full, Mon–Sat diff       │           │   (aes-256-cbc, zstd)                │
│           Wed verify, monthly drill    │           │                                      │
│      └─ scheduled per-database dumps ──┼──────────▶│ <prefix>/dumps/<db>/  (age-encrypted)│
└────────────────────────────────────────┘           └──────────────────────────────────────┘
```

- **pgBackRest underneath, databases on top.** PostgreSQL has one WAL stream for the whole cluster, so backups and point-in-time recovery are cluster-wide. Everything you do with them names one database: `backup list --db`, `restore <db>`, `db fork <db>`.
- **Restore window.** Any database can be rewound to any second between just after the oldest backup and the last archived WAL, which is at most about a minute old. The nightly backup sets only make restores faster; the WAL fills the gaps between them. `retention_full` is the window in weeks (default 4), and retention is time-based, so extra full backups never shorten it.
- **`eru backup setup`** probes the bucket from your machine, generates the repository cipher and an age key for dumps, turns on WAL archiving (one confirmed PostgreSQL restart), and takes the first full backup. The bucket keys, cipher and age identity form the **recovery kit**, which goes into the secrets handoff: run `eru secrets export` afterwards. Backups are optional during `server init`.
- **Restoring one database doesn't touch the others.** A restore never overwrites the live database: it builds a copy beside it, you check the copy, and only `promote` swaps it in.

```
 before           after eru restore app …          after eru restore promote app
 app (live)       app (live, still serving)        app                      the restored data
                  app__restoring (the copy)        app__pre_restore_<ts>    the old live data, kept for undo
```

- **`eru db fork <db> [--at <time>]`** makes a copy under a new name, with its own roles, from now or from a point in time.
- **Long-retention dumps.** A database that must be kept longer than the window gets scheduled `pg_dump` archives (`databases.<db>.backup.dump_schedule` and `dump_retention`), encrypted with age and streamed to the bucket. `eru dump get` decrypts one, `eru restore <db> --dump <id>` loads it, and `eru db drop` takes a final dump first.
- **Long work runs as detachable jobs.** Backups, verify, restores, drills and forks run as systemd units on the node. Ctrl-C detaches, and `eru job list` / `eru job follow <id>` re-attach.
- **Checked, not assumed.** `eru backup verify` checks every file in the repository against its checksum, weekly by default. The monthly `eru restore drill` restores into a scratch instance and runs `pg_amcheck`. Both results show in `eru backup list`.

### Restoring a database, step by step

The example rewinds `app` to just before a bad `DELETE` at 14:03 UTC. Every confirm token is `<db>/restore`, except cleanup's, which is `<db>/cleanup`.

1. **Pick the target.** All times are UTC. `eru backup list --db app` shows the restore window: any second between its start and the last archived WAL can be restored. Use `--time` for a point in time (RFC 3339, or relative like `-15m`), `--latest` for the newest WAL, `--set <label>` for one backup set, or `--dump <id>` (from `eru dump list`) for a point older than the window.

   ```sh
   eru backup list --db app
   ```

2. **Restore into a copy.** This runs as a job on the node. Ctrl-C only detaches, and `eru job follow <id>` re-attaches. Add `--dry-run` first to see the plan. The live `app` keeps serving throughout.

   ```sh
   eru restore app --time 2026-10-12T14:02:00Z --confirm app/restore
   ```

   pgBackRest restores only `app` into a scratch instance, replays WAL up to the target, and loads the result into `app__restoring`. Only one restore per database can be open at a time; if a previous one failed, the new one clears it. A `must be owner of extension pg_stat_statements` warning from `pg_restore` is expected and harmless.

3. **Check the copy before you promote.** `restore status` compares row estimates with the live database. Query the copy directly to confirm it holds what you expect:

   ```sh
   eru restore status app
   eru sql --db app__restoring "select count(*) from users"
   ```

   If it's the wrong point in time, `eru restore abort app` drops `app__restoring` (the live database was never touched), and you can restore again.

4. **Promote.** PgBouncer pauses, the two databases swap names, and PgBouncer resumes, so clients see a pause of a few seconds. The roles and passwords don't change, so the application reconnects without any change. The old live database is kept as `app__pre_restore_<timestamp>`.

   ```sh
   eru restore promote app --confirm app/restore
   eru sql --db app "select count(*) from users"
   ```

5. **Undo or clean up.** If the promoted data is wrong, `undo` swaps the previous database back in, as long as the leftover still exists. Once you're happy, `cleanup` drops the leftover and frees its space. Run `eru restore cleanup` without a database to list leftovers.

   ```sh
   eru restore undo app --confirm app/restore       # go back to what was live before the promote
   eru restore cleanup app --confirm app/cleanup    # or: keep the restore, drop the old copy
   ```

`eru restore app --time … --promote --confirm app/restore` does steps 2 and 4 in one go, skipping the check. Use it only when you're sure of the target.

## Tools used and installed

### On the server

Installed from the distribution's apt repositories unless noted. Steps install only what is missing, and re-runs change nothing.

| Package | Installed by | Why |
| --- | --- | --- |
| `curl`, `ca-certificates`, `gnupg`, `lsb-release`, `jq`, `sudo` | base packages | Basics for the steps and the PGDG repository |
| `apt-transport-https` | base packages | Only on releases that still ship it |
| `ufw` | base packages | Firewall: deny by default, the allow rules above |
| `fail2ban`, `python3-systemd` | base packages | Bans repeated SSH auth failures (reads the journal) |
| `wireguard-tools` | base packages | The `wg0` interface the operator connects through |
| `chrony` | base packages | Time sync (TLS and logs depend on it) |
| `logrotate` | base packages | Rotates `/var/log/eru` |
| `unattended-upgrades` | base packages | Security updates only; PostgreSQL, PgBouncer and nginx are excluded |
| `postgresql-common` | PostgreSQL packages | Adds the PGDG apt repository (`apt.postgresql.org`) and `pg_createcluster` |
| `postgresql-18`, `postgresql-client-18`, `postgresql-contrib` | PostgreSQL packages (PGDG) | PostgreSQL 18; the major version is held with `apt-mark hold` |
| `pgbouncer` | PostgreSQL packages (PGDG) | Connection pooling on 6432 |
| `nginx`, `libnginx-mod-stream`, `ssl-cert` | TLS step | ACME HTTP-01 on 80 and ALPN routing on 443; held with `apt-mark hold` |
| `pgbackrest` (≥ 2.59.3) | PostgreSQL packages (PGDG) | Database backups, WAL archiving and restores; held with `apt-mark hold`. 2.59.3 is the floor because earlier releases have an encryption bug |
| `eru` | `server init` / `--auto-upgrade-nodes` | The node binary at `/usr/local/bin/eru`, pushed from the operator and signature-checked |

No ACME client (certbot and the like) is installed: eru issues certificates itself. Later phases add restic (VPS backups) and the Prometheus exporters, each pinned.

### On your machine

| Tool | Needed for | How you get it |
| --- | --- | --- |
| `eru` | Everything | `install.sh`, cosign-verified; `eru upgrade` afterwards |
| WireGuard, SSH | Every remote command | Built into eru (userspace WireGuard, in-process SSH). Nothing to install and no root needed |
| `psql` ≥ 16 | `eru psql` only | Your package manager; eru offers the install command |
| `usql` | `eru console` only | On PATH, or eru downloads a pinned release (v0.21.4) into `~/.cache/eru/tools` and checks its SHA-256 |
| `wireguard-tools` | `eru wg export --install` only | Your package manager; for a kernel WireGuard interface instead of the built-in one |

### Built into the binary

| Library | Used for |
| --- | --- |
| `spf13/cobra` | Command tree and flags |
| Charm `huh`, `bubbles`, `lipgloss` | Interactive prompts and progress output |
| `golang.org/x/crypto/ssh` | The SSH client |
| `golang.zx2c4.com/wireguard` (wireguard-go + netstack) | The userspace WireGuard tunnel |
| `golang.org/x/crypto/acme` | Issuing and renewing certificates on the node |
| `jackc/pgx` | SQL over the tunnel (`eru sql`, `db`, `tune`) |
| `filippo.io/age` | The encrypted transfer bundle and long-retention dumps |
| `minio/minio-go` | Bucket probe on `backup setup`, and streaming dumps to and from the bucket |
| `sigstore/sigstore-go` | Verifying signed releases on upgrade |
| `pelletier/go-toml` | `cluster.toml` |
| `itchyny/gojq` | `--jq` output filtering |

## Agent-first

Every command can run non-interactively and emit structured JSON (`--output json`), with documented exit codes. Commands support `--dry-run` where it makes sense. Destructive operations need a `--confirm <token>` that `--yes` never satisfies. `eru commands --json` describes the whole surface, and `eru guide agents` / `eru skill install` load the operating guide into a coding agent.

## Development

```sh
just build   # bin/eru
just lint    # golangci-lint, metadata and step checks, shellcheck, actionlint
```

## Docs

- [docs/MASTER.md](docs/MASTER.md): goal, decisions, architecture, roadmap and current status.
- [docs/IMPLEMENTATION.md](docs/IMPLEMENTATION.md): the detailed spec for each phase.
- [docs/MANUAL-TESTING.md](docs/MANUAL-TESTING.md): manual VPS checks still to run.
