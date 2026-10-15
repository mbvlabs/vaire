# eru

One CLI that turns a fresh VPS into a hardened PostgreSQL 18 server, and then stays the only way in.

From your laptop (or from a coding agent), `eru` takes a new Debian 13 or Ubuntu 26.04 server, locks it down, installs PostgreSQL 18 with PgBouncer and nginx, issues its TLS certificate, and connects you to it over WireGuard. Each database you create gets its own tuned PostgreSQL cluster with a size and resource limits, the way a managed provider gives each database its own instance. Every later operation goes through the same binary, over SSH, inside WireGuard. There is no daemon, no web UI and no open control port.

> **Status:** early and under active development. Bootstrap and the first server (Phases 1 and 2) work end to end on a real VPS. Phase 3 (databases as clusters, backups and native restore) is built and waiting for its manual test on a VPS. Upgrades, observability and multi-node are still to come. See [the roadmap](docs/MASTER.md#7-roadmap-and-status).

## What works today

| Area | Commands |
| --- | --- |
| Setup | `eru server init` (remote over SSH, or `--local` on the server), `--resume` |
| Access | `eru login`, `eru context list\|show\|use\|remove`, `eru ssh`, `eru tunnel` |
| Desired state | `eru plan`, `eru apply`, `eru config show\|get\|set\|edit\|validate` |
| Databases | `eru db list\|create\|resize\|drop\|credentials\|fork\|protect\|unprotect` |
| Protection | `eru protect init\|status` (two-factor code), `eru db protect\|unprotect` |
| SQL | `eru sql` (one statement, JSON-friendly), `eru psql`, `eru console` (usql) |
| Tuning | `eru tune show\|explain\|apply` |
| Backups | `eru backup setup\|now\|list\|verify`, `eru dump`, `eru dump list\|get` |
| Restores | `eru restore <db>` (`--time`, `--latest`, `--set`, `--dump`), `eru restore status\|promote\|undo\|abort\|cleanup\|drill` |
| Jobs | `eru job list\|follow` |
| Health | `eru status [--watch]`, `eru doctor [--only security]`, `eru top`, `eru queries` (Phase 4, manual test pending) |
| Metrics | `eru metrics show\|rotate`: an authenticated `https://db1.<domain>/metrics/...` for Prometheus or Grafana Alloy, with a `targets.json` for service discovery |
| Maintenance | `eru maint vacuum\|analyze\|reindex\|report\|kill` (`repack` waits for Phase 7), nightly and weekly maintenance timers |
| Operations | `eru logs`, `eru secrets export`, `eru upgrade` |
| Agents | `eru commands --json`, `eru guide agents`, `eru skill install` |

Commands from later phases are already in the tree and return `not implemented yet`.

## Quick start

```sh
# On your laptop: install the CLI (signed release, verified with cosign)
curl -fsSL https://eru.mbvlabs.com/install.sh | bash

# Bootstrap a fresh server (provider root key or password for first contact)
eru server init --host root@203.0.113.10

# eru asks a few questions, shows the DNS record to create (db1.<your domain>),
# hardens the box, closes public SSH, installs PostgreSQL, and ends with the
# secrets handoff.

eru db create app --size small   # a cluster of its own; prints the URLs and passwords once
eru sql --db app "select now()"
eru console app                # interactive usql session
eru console app --write        # run DDL as the database owner
```

On another machine, `eru login` and paste the transfer bundle from the handoff.

## How it fits together

```
 laptop / CI / agent                                     VPS db1.<domain> (the master node)
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
                                                       │ nginx · 2 PgBouncers · one PostgreSQL   │
                                                       │ 18 cluster per database                 │
                                                       └─────────────────────────────────────────┘
```

- **One binary, two roles.** On your machine `eru` is the operator CLI. On the server the same binary runs as `eru node …`, called over SSH for each command and by systemd timers. Operator and node versions are kept in lockstep: a mismatch is refused unless you pass `--auto-upgrade-nodes`, which pushes the matching signed build.
- **Desired state in, convergence out.** The server and its databases are described by one TOML file on the server. `eru plan` shows what would change and `eru apply` converges, and it's always safe to run again. Steps have versioned IDs (`tls-v1`, and `postgres-config-v1@app` for one database's cluster), and a journal records what ran, so an upgrade only re-runs the steps that changed.
- **eru owns the config it writes.** Managed files are rendered from templates and checked for drift. `ALTER SYSTEM` is disabled so nothing changes behind eru's back.

## Databases as clusters

Every database is its own PostgreSQL cluster: `eru db create app` makes the cluster `18/app` with its own data directory, socket, WAL stream, configuration, restarts, pgBackRest stanza and systemd resource limits. You only ever name the database; the cluster sits underneath. Things that belong together share one database with several schemas.

```
 db1.<domain>
┌──────────────────────────────────────────────────────────────────────┐
│ :443 nginx (ALPN postgresql) ─┐                                      │
│ :6432 transaction pooler ◀────┘   routes by database name            │
│ :5432 session pooler              routes by database name            │
│        │                                                             │
│        ├─ app      → postgresql@18-app      large   socket .s.PGSQL.5433
│        ├─ billing  → postgresql@18-billing  medium  socket .s.PGSQL.5434
│        └─ staging  → postgresql@18-staging  small   socket .s.PGSQL.5435
│                                                                      │
│ clusters listen on Unix sockets only; nothing but the poolers faces  │
│ the network                                                          │
└──────────────────────────────────────────────────────────────────────┘
```

- **Sizes.** A database has a size, like a provider's plan: `nano` (256 MiB, 20 connections), `small` (512 MiB, 30), `medium` (1 GiB, 50, the default), `large` (2 GiB, 100), `xlarge` (4 GiB, 200). The size drives its tuning, its CPU weight and its connection limit. `--memory` overrides the memory, and `eru db resize app --size large` changes it later: limits and reload-only settings change live, and a restart, when one is needed, restarts only that database.
- **A memory budget.** The server keeps a reserve (`max(512 MiB, 25% of RAM)`) for the OS, the poolers, nginx and eru; the databases share the rest. `eru tune show` prints the sheet: each database's size, limits and current use, and what's allocated or free.
- **No overcommit by accident.** `db create`, `db fork` and a growing `db resize` are refused when the new size would take the sizes past the budget (`memory_overcommitted`); the error gives the numbers and the largest size that still fits, as a command to run, for example `eru db create big2 --size small`. Nothing steps down by itself. `--allow-overcommit` goes ahead with a warning; under load the databases are then throttled rather than killed. A database PostgreSQL couldn't start at all is always refused (`would_not_start`).
- **Rough density.** An idle cluster's own processes cost about 19 MiB (measured on PostgreSQL 18 with a `small` database: the cgroup's `anon` plus `kernel` memory). `shared_buffers` is charged as it fills, not up front, and the page cache is reclaimable. A 2 GiB VPS takes 2 `small` and 1 `nano`; a 4 GiB VPS takes 1 `medium` and 3 `small`; an 8 GiB VPS takes 1 `large`, 2 `medium` and 2 `small`.

### URLs by node

Every server (node) has one hostname, `<node>.<domain>`; the first one is `db1.<domain>`. The database name in the URL's path picks the database, which is how PostgreSQL URLs already work, so DNS needs one record per server, not per database:

```
postgres://app_app:…@db1.example.com:6432/app?sslmode=verify-full&sslrootcert=system       app, transaction pooler
postgres://app_app:…@db1.example.com:443/app?…&sslnegotiation=direct                         app, direct TLS on 443 (PG17+ clients)
postgres://app_migrate:…@db1.example.com:5432/app?sslmode=verify-full&sslrootcert=system   migrations and tools, session pooler
```

Creating, forking, restoring, resizing or dropping a database never touches DNS or the certificate. Multi-node arrives in Phase 10, and moving a database to another server will change its URL's host.

### Limits, said plainly

- **Isolation is cgroups, not a VM.** Each cluster's `MemoryHigh` (its memory plus 25%), CPU weight and I/O weight are set by systemd; a hard `memory_max` or `cpu_quota` is opt-in. `MemoryHigh` throttles, it doesn't partition. A managed provider's VM is a harder boundary.
- **I/O weights depend on the disk scheduler.** They only work with BFQ or the kernel's `io.cost` model; on a typical virtio disk they do nothing, and `eru tune show` says "not enforced".
- **Disk space and the page cache are shared.** A database that fills the disk affects its neighbours.
- **Density costs.** Each cluster carries its own processes (about 19 MiB idle) and buffers; a 1 GiB VPS isn't a sensible home for several databases.
- **One server is a single point of failure** until HA (Phase 11).
- **Recovering a lost server.** Until Phase 5 brings the master backup and `server init --restore`, a lost server is recovered by creating a new one with `eru server init` and restoring each database's pgBackRest stanza by hand with the recovery kit from your secrets handoff. The stanzas are in the bucket under `<prefix>/pgbackrest/`.

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
 │ wg0 10.42.0.1 ── sshd :22, transaction pooler :6432,          │
 │                  session pooler :5432                         │
 │                  (WireGuard peers always reach these)         │
 └───────────────────────────────────────────────────────────────┘
```

- **SSH only over WireGuard.** Port 22 is allowed only `in on wg0`. Root login and password authentication are off, and only the admin user may log in, with the operator's key.
- **Commit-confirm lockdown.** Before eru closes public SSH, it snapshots sshd and firewall state and arms a server-side timer. If eru doesn't confirm a working SSH-over-WireGuard session within two minutes, the server reverts on its own. A broken tunnel can't lock you out.
- **Break-glass** is the provider console plus `eru node unlock-ssh`, which re-opens public SSH with a password for the admin user for a fixed window. The steps are in the secrets handoff.
- **Database ports are closed to the public by default.** WireGuard peers can always reach 6432 and 5432. `client_allowlist` with `expose_pgbouncer` / `expose_postgres` opens them to specific CIDRs. Both are PgBouncers; no PostgreSQL cluster has a TCP listener.
- **fail2ban** watches sshd. unattended-upgrades installs security updates only, with PostgreSQL, PgBouncer and nginx held for controlled updates.

### TLS and nginx

```
 client (sslmode=verify-full)                          VPS
                                ┌──────────────────────────────────────────────────┐
 ── :6432 ─────────────────────▶│ transaction pooler (TLS) ─┐                      │
 ── :5432 ─────────────────────▶│ session pooler (TLS)  ────┼─ unix socket ─▶ the  │
                                │                           │  database's cluster  │
 ── :443  ─────────────────────▶│ nginx stream, ssl_preread │  (by database name)  │
   (sslnegotiation=direct,      │   ALPN "postgresql" + allowed source             │
    PG17+ clients)              │      → terminate TLS → transaction pooler        │
                                │   same ALPN, other source → handshake rejected   │
                                │   SSLRequest (no direct) → "no TLS" (N)          │
                                │   anything else → HTTPS: /metrics, else 404      │
                                │                                                  │
 ── :80   ─────────────────────▶│ nginx: /.well-known/acme-challenge/ only         │
                                └──────────────────────────────────────────────────┘
```

- **443 needs `sslnegotiation=direct`.** nginx routes 443 by peeking at the TLS ClientHello, so it can't answer the PostgreSQL SSLRequest that clients send by default. Without `sslnegotiation=direct`, nginx answers "no TLS here" and closes: libpq (with `sslmode=require` or stricter) says `server does not support SSL, but SSL was required`, pgx says `server refused TLS connection`. Add `sslnegotiation=direct` (PostgreSQL 17+ libpq, pgx 5.7+), or use port 6432, which speaks the normal negotiation. A direct-TLS client from an address that isn't allowed gets its handshake rejected: libpq says `SSL error: tlsv1 unrecognized name`, pgx `remote error: tls: unrecognized name`.
- **eru is its own ACME client.** An hourly `eru-certs` timer issues and renews the certificate for the node's hostname (`db1.<domain>`) over HTTP-01, installs it for both PgBouncers, nginx and every cluster, and reloads them. Until the first certificate arrives, a placeholder certificate lets everything start.
- The DNS record must point straight at the server (DNS only, not proxied).
- A `self-signed` mode exists for servers without a domain. Connections are encrypted but not verified, and eru warns about that.

### PostgreSQL hardening

- Every cluster listens on its Unix socket only (`listen_addresses = ''`), and its `pg_hba.conf` allows nothing else. The poolers terminate TLS and require it on TCP, with `scram-sha-256`. The `postgres` superuser is local-socket only.
- `allow_alter_system = off`, `REVOKE` of `CONNECT` and of `public` schema rights from `PUBLIC`, `idle_in_transaction_session_timeout`, and connection and lock logging.
- Settings are tuned per database from its size and workload profile (`eru tune explain --db app` shows why each value was picked).

### Roles per database

```
 <db>_owner   NOLOGIN, owns the database and every object in it
    ▲
    │ member, SET role = <db>_owner on login
 <db>_migrate   DDL: migrations              → :5432 (session pooler; the transaction pooler refuses it)
 <db>_app       SELECT/INSERT/UPDATE/DELETE  → :6432 or :443 (transaction pooler), statement_timeout
 <db>_ro        SELECT, read-only by default → :6432 or :443 (transaction pooler), reporting and read-only clients

 in every cluster:
 eru_admin        CREATEDB/CREATEROLE, not superuser (operator)
 pgbouncer_auth   may only call the auth_query functions
 eru_monitor      pg_monitor
```

Run migrations as `migrate` and the application as `app`. The transaction pooler refuses the `migrate` role: in transaction mode, advisory locks, session `SET` and temporary tables can land on a different connection, so migrations go through the session pooler on 5432, where a client keeps its server connection for the whole session. From outside WireGuard, use `eru tunnel --db app --target postgres` or set `expose_postgres = true`. A compromised app can change data but can't drop or change the schema. Default privileges give `app` and `ro` access to every new table without extra grants. The service passwords (`eru_admin`, `eru_monitor`, `pgbouncer_auth`) are the same in every cluster, so adding a database never makes your secrets handoff stale.

### Secrets

- eru generates every password. You never type one in.
- The server keeps only SCRAM verifiers, plus the two service passwords PgBouncer and the exporters need at runtime.
- At the end of `server init`, eru prints the **secrets handoff** once:
  - every secret in plain text, grouped for a password manager;
  - an ASCII-armored `age` transfer bundle (scrypt), with a generated passphrase.
- You type `stored` to confirm you've saved it, and eru offers to clear the screen. Nothing is written to disk unless you ask with `--secrets-out`.
- `eru login` on any machine takes the bundle and passphrase and sets up a **context**: credentials under `~/.config/eru/contexts/<server>/`, files 0600.
- Releases are signed with cosign. `eru upgrade` and node upgrades verify the signature and the sha256 before swapping binaries.

### Protected databases

A protected database refuses `db drop`, a shrinking resize, credential rotation and restore promote/undo, even with `--confirm`. Lifting the protection asks for a current 6-digit code from an authenticator app, typed at a terminal and checked on the node, so an agent with a shell can't lift it by typing a word.

Set it up once with `eru protect init`: the node makes the secret and keeps it root-only, and eru shows it as a QR code. Once the code is confirmed, it asks which of your databases to protect (all of them picked to start with) and protects them in the same run; `--all` or `--db app` (repeatable) choose without asking. After that:

```sh
eru db protect app billing     # or --all; in effect at once, no apply
eru db create scratch --protected
eru db unprotect app           # asks for a code from the authenticator app
eru protect status             # every database, protected yes or no
```

Protecting a database is refused until `eru protect init` has run. A code works once, and 5 wrong codes lock codes out for 15 minutes. `eru config set clusters.app.protected true` still works too. Root on the server can still edit the files directly; this guards against agents and slips, not against someone who owns the box.

### Operator connections

```
 eru console app
   │
   ├─ usql ──plaintext──▶ 127.0.0.1:<port>   (loopback only)
   │                          │
   │              eru: SSLRequest + TLS handshake, verify-full
   │                  against the node's certificate (db1.<domain>)
   │                          │
   └──────────── userspace WireGuard ──▶ 10.42.0.1:5432  session pooler → app
```

`eru sql`, `eru psql` and `eru console` all connect over the tunnel to the session pooler on 5432 of the node that holds the database, which routes by `--db`, and verify the certificate (`verify-full`). They log in as `eru_admin`, never as an application role, and are read-only unless `--write` is passed; with `--write` they act as `<db>_owner`, so new objects get the same owner and default grants as a migration. `eru tunnel --db app --target postgres` forwards a local port to that pooler, which is how a migration tool on your machine reaches the database as `<db>_migrate` without opening 5432 to the internet. `eru console` uses usql from PATH, or downloads a pinned release and checks its SHA-256. Arguments after `--` go to `psql` as they are (`eru psql --db app -- -c "select 1"`); a `-c` before `--` is eru's `--context`.

## Backups and restores

```
 VPS                                                     S3-compatible bucket (R2, S3, B2, …)
┌──────────────────────────────────────────┐           ┌──────────────────────────────────────┐
│ app, billing, … (one cluster each)       │           │ <prefix>/pgbackrest/                 │
│   ──WAL, continuously (≤60 s)────────────┼──────────▶│   one stanza per database:           │
│   └─ eru-backup-*@<db> timers ───────────┼──────────▶│   full · diff · incr sets, WAL       │
│        Sun full, Mon–Sat diff, staggered │           │   (aes-256-cbc, zstd)                │
│        Wed verify, monthly drill         │           │                                      │
│   └─ scheduled per-database dumps ───────┼──────────▶│ <prefix>/dumps/<db>/  (age-encrypted)│
└──────────────────────────────────────────┘           └──────────────────────────────────────┘
```

- **A stanza per database.** Each database's cluster has its own WAL stream, so backups, point-in-time recovery, retention and verification are all per database, in one repository with one cipher. Everything names a database: `backup list --db`, `backup now --db`, `restore <db>`, `db fork <db>`.
- **Restore window.** A database can be rewound to any second between just after its oldest backup and its last archived WAL, which is at most about a minute old. The backup sets only make restores faster; the WAL fills the gaps between them. `retention_full` is the window in weeks (default 4, and a database can have its own with `clusters.<db>.backup.retention_full`). Retention is time-based, so extra full backups never shorten it.
- **`eru backup setup`** probes the bucket from your machine, generates the repository cipher (the age key for dumps already exists from `server init`), switches every database's WAL archiving on with a reload (no restart), and takes a first full backup of each. The bucket keys, cipher and age identity form the **recovery kit**, which goes into the secrets handoff: run `eru secrets export` afterwards. Backups are optional during `server init`; until a bucket exists, `server init` and every `apply` warn `no_offsite_backup`.
- **Staggered.** Each database's timers run at their own offset inside the maintenance window, and only one full, diff, verify, drill or dump runs at a time on a server.
- **Dropped databases stay restorable for their window.** `db drop` keeps the stanza; `eru backup list` shows it until a daily timer deletes it once the window has passed.
- **Restoring one database doesn't touch the others.** A restore builds a new cluster from the database's stanza beside the live one, you check it, and only `promote` swaps it in. It needs free space for one copy of the database.
- **`eru db fork <db> [--name <new>] [--at <time>]`** makes a separate database: a physical copy of the source (from its stanza at a point in time, or from the live cluster), renamed, with its own roles and new passwords, the source's size and settings (`90-overrides.conf` included), its own stanza and a first full backup. It works without backups when there's no `--at`.
- **Long-retention dumps.** A database that must be kept longer than the window gets scheduled `pg_dump` archives (`clusters.<db>.backup.dump_schedule` and `dump_retention`), encrypted with age and streamed to the bucket. `eru dump get` decrypts one, `eru restore <db> --dump <id>` loads it, and `eru db drop` takes a final dump first.
- **Long work runs as detachable jobs.** Backups, verify, restores, drills and forks run as systemd units on the node. Ctrl-C detaches, and `eru job list` / `eru job follow <id>` re-attach.
- **Checked, not assumed.** `eru backup verify` checks every file of a database's stanza against its checksum, weekly by default. The monthly `eru restore drill` restores each database into a throwaway cluster, runs `pg_amcheck` on it and compares it with the live one; each database's drill falls on its own day of the month. Both results show in `eru backup list`.

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

   pgBackRest restores `app`'s stanza into a new cluster, `app__restoring`, which replays WAL up to the target and pauses there, read-only. Only one restore per database can be open at a time; if a previous one failed, the new one clears it.

3. **Check the copy before you promote.** `restore status` compares row estimates with the live database. Query the copy directly through the session pooler to confirm it holds what you expect:

   ```sh
   eru restore status app
   eru sql --db app__restoring "select count(*) from users"
   ```

   If it's the wrong point in time, `eru restore abort app` drops `app__restoring` (the live database was never touched), and you can restore again.

4. **Promote.** The transaction pooler pauses `app`, the live cluster stops, the data directories swap places, and `app` starts on the restored data with its own configuration and limits, finishes replaying to the target and opens on a new timeline. Clients see a few seconds of downtime plus that replay; other databases aren't touched. The URL, roles and passwords don't change, so the application reconnects without any change. A full backup starts right after. The old data is kept as the stopped cluster `app__pre_restore_<timestamp>`.

   ```sh
   eru restore promote app --confirm app/restore
   eru sql --db app "select count(*) from users"
   ```

5. **Undo or clean up.** If the promoted data is wrong, `undo` swaps the old data back the same way, as long as the leftover still exists, and eru records which timeline to follow so later restores don't wander onto the abandoned one. Once you're happy, `cleanup` drops the leftover and frees its space. Run `eru restore cleanup` without a database to list leftovers. `eru db drop` drops a database's leftovers with it.

   ```sh
   eru restore undo app --confirm app/restore       # go back to what was live before the promote
   eru restore cleanup app --confirm app/cleanup    # or: keep the restore, drop the old copy
   ```

`eru restore app --time … --promote --confirm app/restore` does steps 2 and 4 in one go, skipping the check. Use it only when you're sure of the target. When only some rows are needed, `eru db fork app --name app_restored --at <time>` gives you a separate database at that point instead, without touching the live one.

A restore from a dump (`--dump <id>`) builds the copy from the dump instead, with the database's roles and passwords. On promote the database gets a new stanza, because its data has a new system identity; the old stanza stays in `backup list` as dropped for the rest of its window.

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
| `postgresql-common` | PostgreSQL packages | Adds the PGDG apt repository (`apt.postgresql.org`) and `pg_createcluster`, which makes each database's cluster |
| `postgresql-18`, `postgresql-client-18`, `postgresql-contrib` | PostgreSQL packages (PGDG) | PostgreSQL 18; the major version is held with `apt-mark hold` |
| `pgbouncer` | PostgreSQL packages (PGDG) | Two poolers: transaction mode on 6432 (Debian's `pgbouncer.service`) and session mode on 5432 (`eru-pgbouncer-session.service`) |
| `nginx`, `libnginx-mod-stream`, `ssl-cert` | TLS step | ACME HTTP-01 on 80 and ALPN routing on 443; held with `apt-mark hold` |
| `pgbackrest` (≥ 2.59.3) | PostgreSQL packages (PGDG) | Database backups, WAL archiving and restores; held with `apt-mark hold`. 2.59.3 is the floor because earlier releases have an encryption bug |
| `eru` | `server init` / `--auto-upgrade-nodes` | The node binary at `/usr/local/bin/eru`, pushed from the operator and signature-checked |

| `node_exporter` 1.12.1, `postgres_exporter` 0.20.1, `pgbouncer_exporter` 0.12.1 | metrics step (upstream release binaries, sha256 pinned in eru) | Metrics on loopback, served through nginx's authenticated `/metrics`; the same versions on Debian and Ubuntu |

No ACME client (certbot and the like) is installed: eru issues certificates itself. eru's bloat report embeds the estimate queries of [ioguix/pgsql-bloat-estimation](https://github.com/ioguix/pgsql-bloat-estimation) (BSD-2-Clause; `eru version --licenses` prints the notice).

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
| `rsc.io/qr` | The QR code `eru protect init` shows for the authenticator app |

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
- [docs/RFC-cluster-per-database.md](docs/RFC-cluster-per-database.md): why each database is its own cluster, and how that shapes every phase.
- [docs/MANUAL-TESTING.md](docs/MANUAL-TESTING.md): manual VPS checks still to run.
