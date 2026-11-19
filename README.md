![Vairë wordmark](assets/vaire-wordmark-dark.svg)

# Vairë (`vaire`)

Run PostgreSQL 18 on your own VPS with one CLI for setup, operations, and recovery.

`vaire` provisions a fresh Debian 13 or Ubuntu 26.04 server. Each database gets its own PostgreSQL cluster, resource limits, and, when backups are configured, a separate backup and restore history. Operators manage the server over SSH inside WireGuard; applications connect with standard PostgreSQL clients through PgBouncer. There is no server control daemon or public management API.

> **Beta:** Use a fresh server dedicated to Vaire. The current stack uses PgBouncer and Vaire-managed nginx; it does not migrate existing Caddy installs. Multi-node management and in-place extension removal are not supported.

**Upgrade policy:** Alpha and beta builds are not a compatibility baseline.
`1.0.0-beta.2` is planned as a fresh install, not an in-place upgrade from
`1.0.0-beta.1`. Supported in-place upgrade paths start with the release
candidates and will be listed in the release notes.

## What works today

| Area | Commands |
| --- | --- |
| Setup | `vaire server init` (remote over SSH, or `--local` on the server), `--resume` |
| Access | `vaire login`, `vaire workspace list\|show\|use\|remove\|set-address`, `vaire ssh`, `vaire tunnel` |
| Desired state | `vaire plan`, `vaire apply`, `vaire config show\|get\|set\|edit\|validate` |
| Databases | `vaire db list\|create\|resize\|drop\|credentials\|copy\|fork\|follow\|unfollow\|protect\|unprotect` |
| Protection | `vaire protect init\|status` (two-factor code), `vaire db protect\|unprotect` |
| SQL | `vaire sql` (multiple statements and ordered results, JSON-friendly), `vaire psql`, `vaire console` (usql) |
| Tuning | `vaire tune show\|explain\|apply` |
| Backups | `vaire backup setup\|now\|list\|verify` (physical backups and WAL) |
| Restores | `vaire restore <db>` (`--time`, `--latest`, `--set`), `vaire restore status\|promote\|undo\|abort\|cleanup\|drill` |
| Disaster recovery | `vaire server backup now\|list\|verify\|export` (the master bundle), `vaire server init --restore`, `vaire db recover <db>\|--all` |
| Jobs | `vaire job list\|follow` |
| Health | `vaire status [--watch]`, `vaire doctor [--only security]`, `vaire top`, `vaire queries` |
| Evidence | `vaire activity list\|show\|health`, `vaire diagnostics collect\|inspect` (local metadata-only history and portable incident files) |
| Metrics | `vaire metrics show\|rotate`: authenticated HTTPS endpoints for node, PostgreSQL and PgBouncer metrics, with Prometheus service discovery via `targets.json` |
| Extensions | `vaire ext list\|add` (built-in and explicit custom definitions; APT or preinstalled) |
| Maintenance | `vaire maint vacuum\|analyze\|reindex\|repack\|report\|kill` (including one-table `--exact`), nightly and weekly maintenance timers |
| Operations | `vaire logs`, `vaire secrets export`, `vaire upgrade` |
| Agents | `vaire commands --json`, `vaire guide agents`, `vaire skill install` |

## Install

Official installers are available for Linux and macOS on AMD64 and ARM64.
They verify signed releases with cosign. Use a **versioned URL**
to install one exact release, or a **channel URL** to install whatever that
channel points to today.

### Install an exact version

Use `https://vaire.mbvlabs.com/releases/<version>/install.sh`, replacing
`<version>` with a published version **without** the leading `v`. For example,
this always installs `1.0.0-beta.1`, even after `edge` moves on:

```sh
curl -fsSL https://vaire.mbvlabs.com/releases/1.0.0-beta.1/install.sh | bash
```

Already have vaire? To install that exact CLI version without upgrading any nodes:

```sh
vaire upgrade --channel edge --version 1.0.0-beta.1 --skip-nodes
```

**This pins one install, not future upgrades.** To keep choosing an exact
version, use the versioned URL or pass `--version` each time. Without
`--version`, `vaire upgrade` follows the selected channel.

### Install from a channel

```sh
# Edge: rolling master builds and tagged alpha/beta releases
curl -fsSL https://vaire.mbvlabs.com/edge/install.sh | bash

# Release candidates only — available once the first RC is published
curl -fsSL https://vaire.mbvlabs.com/rc/install.sh | bash

# Final releases only — available once the first final release is published
curl -fsSL https://vaire.mbvlabs.com/install.sh | bash

# Final releases on the 1.x line only
curl -fsSL https://vaire.mbvlabs.com/1.x/install.sh | bash
```

The corresponding `vaire upgrade --channel` values are `edge`, `rc`, `stable`,
and `stable-1.x`.

## Quick start

Start with a fresh VPS, initial root SSH access, and a DNS name you control.
If you use Cloudflare, see [Cloudflare setup](#cloudflare-setup) for DNS and
optional R2 backups.

```sh
# Bootstrap a fresh server (provider root key or password for first contact)
vaire server init --host root@203.0.113.10
```

During setup, Vaire shows the DNS records to create, locks down SSH, and issues
the node certificate in the background. Save the secrets handoff before closing
the terminal. Configure offsite backups during setup or later with
`vaire backup setup`.

```sh
vaire db create app --size small   # a cluster of its own; prints the URLs and passwords once
vaire sql --db app "select now()"
vaire console app                # interactive usql session
vaire console app --write        # run DDL as the database owner
```

On another machine, `vaire login` and paste the transfer bundle from the handoff.
Fresh setups generate a `machine-<id>` registration. Importing the same bundle
on another computer shares that registration and its keys; it does not enroll a
new machine.

## Cloudflare setup

Cloudflare is optional. **DNS** points clients to the VPS; **R2** stores offsite
backups. You can use either without the other.

### DNS for the VPS

In **Cloudflare → your domain → DNS → Records**, add the records shown by
`vaire server init`. For `db1.example.com`, create an **A** record for `db1`
pointing to the VPS's public IPv4 address. Add an **AAAA** record only if
the VPS has working public IPv6. Set both to **DNS only** (gray cloud), not
**Proxied** (orange cloud).

Use one hostname per VPS, not one per database. PostgreSQL clients connect
directly to the VPS, including when they use PostgreSQL over port 443.
Vaire obtains and renews the certificate itself using ACME HTTP-01 on port 80;
you don't need a Cloudflare DNS API token or a Cloudflare Origin Certificate.

### Private R2 bucket for backups

1. In **Cloudflare → R2 Object Storage**, create a private bucket, such as
   `vaire-backups`. Leave public access off. If you publish your own Vaire
   builds, keep their downloads in a separate bucket.
2. Create R2 S3 credentials with **Object Read & Write** access scoped to that
   bucket. Save the access key ID and secret access key in your password manager.
3. Choose **Cloudflare R2** during `vaire server init` or run `vaire backup setup`.
   Enter the bucket's S3 API endpoint (`https://<account_id>.r2.cloudflarestorage.com`
   for a standard bucket), region `auto`, bucket name, and credentials.
   The R2 preset uses path-style access. Give each independent installation its
   own prefix if they share a bucket.
4. Save the recovery handoff securely and check `vaire backup list --db app`
   and `vaire server backup list`. The former shows physical backups and the
   archived WAL restore window; the latter shows the server-state bundles.

Use the S3 API endpoint from the R2 dashboard, not an `r2.dev` URL or a public
custom domain.

## Workspaces and nodes

A **workspace** groups an installation's nodes, database placement and operator
access. Its name is independent of any node name: `prod` is a workspace, while
`db1` is its first node. This establishes the grouping for future multi-node
support; adding and managing multiple nodes is not implemented yet.

```sh
vaire server init --name prod --node-name db1 --host root@203.0.113.10
vaire workspace list
vaire workspace show prod
vaire workspace use prod
vaire --workspace prod status       # -w prod also works
```

Selection is `--workspace` / `-w`, then `VAIRE_WORKSPACE`, then the saved current
workspace. Local access lives under `~/.config/vaire/workspaces/<name>/`, with
`workspace.toml` and owner-only credential files; `config.toml` saves
`current_workspace`.

## How it fits together

```
 laptop / CI / agent                                     VPS db1.<domain> (the master node)
┌───────────────────────────────┐                      ┌─────────────────────────────────────────┐
│ vaire (operator mode)           │                      │ wg0 10.42.0.1                           │
│  ├ workspace + credentials      │  WireGuard  UDP 51820│  └ sshd (port 22 reachable on wg0 only) │
│  ├ userspace WireGuard  ══════╪═════════════════════▶│                                         │
│  │  (no root, no wg0 needed)  │                      │ vaire (node mode), run per command:       │
│  ├ in-process SSH  ───────────┼──── inside WG ──────▶│  ├ versioned steps + embedded scripts   │
│  └ pgx / usql / psql ─────────┼──── inside WG ──────▶│  ├ /etc/vaire/cluster.toml (desired state)│
└───────────────────────────────┘     (5432 / 6432)    │  ├ journal of applied steps             │
                                                       │  └ systemd timers → vaire node run …      │
                                                       │                                         │
                                                       │ nginx · 2 PgBouncers · one PostgreSQL   │
                                                       │ 18 cluster per database                 │
                                                       └─────────────────────────────────────────┘
```

- **One binary, two roles.** On your machine `vaire` is the operator CLI. On the server the same binary runs as `vaire node …`, called over SSH for each command and by systemd timers. An existing node must run the matching build; only a trusted host administrator may replace its binary.
- **Desired state in, convergence out.** The server and its databases are described by one TOML file on the server. `vaire plan` shows what would change and `vaire apply` converges, and it's always safe to run again. Steps have versioned IDs (`tls-v1`, and `postgres-config-v1@app` for one database's cluster), and a journal records what ran, so an upgrade only re-runs the steps that changed.
- **vaire owns the config it writes.** Managed files are rendered from templates and checked for drift. `ALTER SYSTEM` is disabled so nothing changes behind vaire's back.

## Databases as clusters

Every database is its own PostgreSQL cluster: `vaire db create app` makes the cluster `18/app` with its own data directory, socket, WAL stream, configuration, restarts, pgBackRest stanza and systemd resource limits. You only ever name the database; the cluster sits underneath. Things that belong together share one database with several schemas.

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

- **Sizes.** A database has a size, like a provider's plan: `nano` (256 MiB, 20 connections), `small` (512 MiB, 30), `medium` (1 GiB, 50, the default), `large` (2 GiB, 100), `xlarge` (4 GiB, 200). The size drives its tuning, its CPU weight and its connection limit. `--memory` overrides the memory, and `vaire db resize app --size large` changes it later: limits and reload-only settings change live, and a restart, when one is needed, restarts only that database.
- **A memory budget.** The server keeps a reserve (`max(512 MiB, 25% of RAM)`) for the OS, the poolers, nginx and vaire; the databases share the rest. `vaire tune show` prints the sheet: each database's size, limits and current use, and what's allocated or free.
- **No overcommit by accident.** `db create`, `db fork` and a growing `db resize` are refused when the new size would take the sizes past the budget (`memory_overcommitted`); the error gives the numbers and the largest size that still fits, as a command to run, for example `vaire db create big2 --size small`. Nothing steps down by itself. `--allow-overcommit` goes ahead with a warning; under load the databases are then throttled rather than killed. A database PostgreSQL couldn't start at all is always refused (`would_not_start`).
- **Rough density.** An idle cluster's own processes cost about 19 MiB (measured on PostgreSQL 18 with a `small` database: the cgroup's `anon` plus `kernel` memory). `shared_buffers` is charged as it fills, not up front, and the page cache is reclaimable. A 2 GiB VPS takes 2 `small` and 1 `nano`; a 4 GiB VPS takes 1 `medium` and 3 `small`; an 8 GiB VPS takes 1 `large`, 2 `medium` and 2 `small`.

### URLs by node

Every server (node) has one hostname, `<node>.<domain>`; the first one is `db1.<domain>`. The database name in the URL's path picks the database, which is how PostgreSQL URLs already work, so DNS needs one record per server, not per database:

```
postgres://app_app:…@db1.example.com:6432/app?sslmode=verify-full&sslrootcert=system       app, transaction pooler
postgres://app_app:…@db1.example.com:443/app?…&sslnegotiation=direct                         app, direct TLS on 443 (PG17+ clients)
postgres://app_migrate:…@db1.example.com:5432/app?sslmode=verify-full&sslrootcert=system   migrations and tools, session pooler
```

Creating, forking, restoring, resizing or dropping a database does not require any DNS or certificate changes.

For an external PostgreSQL source, `vaire db copy` imports a snapshot into a new database without overwriting an existing one or importing source roles and settings. The VPS must be able to reach the source. For unattended use, pass the source URL via `--source-url-stdin` and confirm the destination name with `--confirm`.

### Limits, said plainly

- **Isolation is cgroups, not a VM.** Each cluster's `MemoryHigh` (its memory plus 25%), CPU weight and I/O weight are set by systemd; a hard `memory_max` or `cpu_quota` is opt-in. `MemoryHigh` throttles, it doesn't partition. A managed provider's VM is a harder boundary.
- **I/O weights depend on the disk scheduler.** They only work with BFQ or the kernel's `io.cost` model; on a typical virtio disk they do nothing, and `vaire tune show` says "not enforced".
- **Disk space and the page cache are shared.** A database that fills the disk affects its neighbours.
- **Density costs.** Each cluster carries its own processes (about 19 MiB idle) and buffers; a 1 GiB VPS isn't a sensible home for several databases.
- **One server is a single point of failure.** Followers run on the same VPS for read-only workloads and migration rehearsals. They do not provide failover.
- **Recovering a lost server.** The server backs its own state up after every change: an age-encrypted master bundle in the bucket under `<prefix>/master/` (`vaire server backup list`). When the server is lost, power the old VPS off or destroy it, create a new one, and run `vaire server init --restore --host root@<new-ip>` from your machine: the new server takes over the old one's WireGuard key, SSH host keys and certificate, so your workspace keeps working; then `vaire db recover --all` brings each database back from its stanza. Move the `db1.<domain>` record to the new IP, or use a provider IP that moves between servers (a Hetzner Primary IP, a floating IP) so nothing changes in DNS; a short TTL on that record shortens the wait for clients. Without a bucket, keep copies with `vaire server backup export`; the server comes back from one with `--bundle-file`, but the databases' data doesn't: configure a bucket for production.

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
      │         │         │       expose_session_pooler)
      ▼         ▼         ▼          ▼
 ┌───────────────────────────────────────────────────────────────┐
 │ wg0 10.42.0.1 ── sshd :22, transaction pooler :6432,          │
 │                  session pooler :5432                         │
 │                  (WireGuard peers always reach these)         │
 └───────────────────────────────────────────────────────────────┘
```

- **SSH only over WireGuard.** Port 22 is allowed only `in on wg0`. Root login and password authentication are off, and only the admin user may log in, with the operator's key.
- **Commit-confirm lockdown.** Before vaire closes public SSH, it snapshots sshd and firewall state and arms a server-side timer. If vaire doesn't confirm a working SSH-over-WireGuard session within two minutes, the server reverts on its own. A broken tunnel can't lock you out.
- **Break-glass** is the provider console plus `vaire node unlock-ssh`, which re-opens public SSH with a password for the admin user for a fixed window. The steps are in the secrets handoff.
- **Public database access is off by default.** Add client egress CIDRs to `network.client_allowlist` for direct application connections. `network.expose_pgbouncer` controls transaction-pooler access on 6432 and PostgreSQL over 443; `network.expose_session_pooler` separately controls session-pooler access on 5432. An empty allowlist admits no public database clients. WireGuard peers can reach both poolers regardless. No PostgreSQL cluster has a TCP listener.
- **fail2ban** watches sshd. unattended-upgrades installs security updates only, with PostgreSQL, PgBouncer and nginx held for controlled updates.

### TLS and nginx

Vaire uses nginx's stream module to share port 443 between HTTPS metrics and
direct-TLS PostgreSQL. nginx reads the client's ALPN before terminating TLS:
PostgreSQL goes to the transaction pooler, and HTTPS goes to the metrics server.
nginx also serves ACME HTTP-01 challenges on port 80; Vaire itself manages
certificate issuance and renewal.

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
- **vaire is its own ACME client.** An hourly `vaire-certs` timer issues and renews the certificate for the node's hostname (`db1.<domain>`) over HTTP-01, installs it for both PgBouncers, nginx and every cluster, and reloads them. Until the first certificate arrives, a placeholder certificate lets everything start.
- The DNS record must point straight at the server (DNS only, not proxied).
- A `self-signed` mode exists for servers without a domain. Connections are encrypted but not verified, and vaire warns about that.

### Monitoring

Metrics are off on fresh nodes until approved. Set `metrics.allowlist` first
if you want source restrictions, then run `vaire config set metrics.enabled true --apply`.
An empty metrics allowlist accepts any source with valid Basic Auth credentials.

Run `vaire metrics show` for the authenticated HTTPS endpoints, including
`/metrics/node`, `/metrics/postgres/<db>`, `/metrics/pgbouncer`, and
`/metrics/pgbouncer-session`. Prometheus can discover them through
`/metrics/targets.json`. For Grafana Cloud's **Metrics Endpoint** integration,
create a scrape job for each metrics URL you want to monitor, not for
`targets.json`. Use Basic Auth with the metrics credentials from the secrets
handoff. No local collector is needed for direct scraping.

### PostgreSQL hardening

- Every cluster listens on its Unix socket only (`listen_addresses = ''`), and its `pg_hba.conf` allows nothing else. The poolers terminate TLS and require it on TCP, with `scram-sha-256`. The `postgres` superuser is local-socket only.
- `allow_alter_system = off`, `REVOKE` of `CONNECT` and of `public` schema rights from `PUBLIC`, `idle_in_transaction_session_timeout`, and connection and lock logging.
- Settings are tuned per database from its size and workload profile (`vaire tune explain --db app` shows why each value was picked).

### Roles per database

```
 <db>_owner   NOLOGIN, owns the database and every object in it
    ▲
    │ member, SET role = <db>_owner on login
 <db>_migrate   DDL: migrations              → :5432 (session pooler; the transaction pooler refuses it)
 <db>_app       SELECT/INSERT/UPDATE/DELETE  → :6432 or :443 (transaction pooler), statement_timeout
 <db>_ro        SELECT, read-only by default → :6432 or :443 (transaction pooler), reporting and read-only clients

 in every cluster:
 vaire_admin        CREATEDB/CREATEROLE, not superuser (operator)
 pgbouncer_auth   may only call the auth_query functions
 vaire_monitor      pg_monitor
```

**Use `app` for runtime and keep `migrate` out of the running application's configuration.** The `app` role can read and write rows in granted tables but cannot alter or drop those tables. The `migrate` role acts as the database owner: it can change the schema and write data for backfills. It is not a DDL-only login, and there is no migration window to open.

The transaction pooler rejects `migrate` on both 6432 and 443. Run migrations through the session pooler on 5432 so session state, such as advisory locks and temporary tables, stays on one server connection. Use WireGuard or `vaire tunnel --db app --target postgres` to reach it without exposing the port publicly.

Default privileges grant access to new tables created by the owner in `public`: `app` can read and write, and `ro` can read. For other schemas, configure grants and default privileges explicitly.

### Secrets

- vaire generates server and database passwords. The optional master password for the secrets-export gate is chosen by the operator.
- The server keeps only SCRAM verifiers, plus the two service passwords PgBouncer and the exporters need at runtime.
- At the end of `server init`, vaire prints the **secrets handoff** once:
  - every secret in plain text, grouped for a password manager;
  - an ASCII-armored `age` transfer bundle (scrypt), with a generated passphrase.
- You type `stored` to confirm you've saved it, and vaire offers to clear the screen. Nothing is written to disk unless you ask with `--secrets-out`.
- `vaire login` on any machine takes the bundle and passphrase and sets up a **workspace**: credentials under `~/.config/vaire/workspaces/<name>/`, files 0600.
- Later full-document reveals (`vaire secrets export` and replacement handoffs) require a master password or a node-verified authenticator code at a real terminal. With neither configured, set one using `vaire secrets password set` or `vaire protect init`. This is a command gate, not encryption of workspace files at rest.
- Releases are signed with cosign. Use `vaire upgrade --skip-nodes` for the CLI. Node binary replacement is a separate, trusted host-administrator operation; operator RPC cannot install or downgrade it.

### Approving public access

Use a fresh host with a dedicated Vaire operator account. Do not give that
account general sudo, membership in privileged groups, or access to provider
credentials. The approval guard does not constrain a host administrator.
Do not use `beta.2` to retrofit this boundary onto a `beta.1` node.

From the provider console as root, run `vaire node exposure-enroll password`
or `vaire node exposure-enroll totp`. The password is verified on the node
and is separate from the local secrets-export master password and the SSH
recovery password. Exposure TOTP is separate from database protection.
After first enrollment, adding or replacing a factor requires an existing one.

Use interactive `vaire config set` or `vaire config edit` to review and approve
a change that widens public database access or HTTPS metrics. Enabling the
session pooler switch requires approval even with an empty database allowlist.
Dry runs and narrowing do not need a factor. An approved edit can be applied
later. A fresh restore does not inherit approvals: database access starts
private and metrics are off. Re-enroll and approve public access on the restored node.

### Protected databases

A protected database refuses `db drop`, a shrinking resize, credential rotation and restore promote/undo, even with `--confirm`. Lifting the protection asks for a current 6-digit code from an authenticator app, typed at a terminal and checked on the node, so an agent with a shell can't lift it by typing a word.

Writing to it takes a code too. `vaire sql --write`, `vaire psql --write`, `vaire console --write` and `vaire maint kill` ask for one when they start, before anything connects; a `psql` or `console` session then stays writable until you close it, and `vaire sql --write` asks on every run. Reads never ask. Without a terminal, with `--yes` or with `--output json`/`agent` the write is refused, so an agent gets read-only access. The node enforces it: on a protected database the `vaire_admin` login in every workspace can only read, and a write runs as a two-minute login role the node creates after it checks the code. Vacuum, analyze and reindex, restores into a copy and forks work as before.

Set it up once with `vaire protect init`: the node makes the secret and keeps it root-only, and vaire shows it as a QR code. Once the code is confirmed, it asks which of your databases to protect (all of them picked to start with) and protects them in the same run; `--all` or `--db app` (repeatable) choose without asking. After that:

```sh
vaire db protect app billing     # or --all; in effect at once, no apply
vaire db create scratch --protected
vaire db unprotect app           # asks for a code from the authenticator app
vaire sql --db app --write "…"   # on a protected database, asks for a code too
vaire protect status             # every database, protected yes or no
```

Protection is opt-in and requires `vaire protect init`. The master password does not substitute for an authenticator code. Protection does not change the write permissions of the application's `<db>_app` role. A code works once, and 5 wrong codes lock codes out for 15 minutes. This guards against mistakes and unintended operator writes, not a compromised server.

### Operator connections

```
 vaire console app
   │
   ├─ usql ──plaintext──▶ 127.0.0.1:<port>   (loopback only)
   │                          │
   │              vaire: SSLRequest + TLS handshake, verify-full
   │                  against the node's certificate (db1.<domain>)
   │                          │
   └──────────── userspace WireGuard ──▶ 10.42.0.1:5432  session pooler → app
```

`vaire sql`, `vaire psql` and `vaire console` all connect over the tunnel to the session pooler on 5432 of the node that holds the database, which routes by `--db`, and verify the certificate (`verify-full`). They log in as `vaire_admin`, never as an application role, and are read-only unless `--write` is passed; with `--write` they act as `<db>_owner`, so new objects get the same owner and default grants as a migration. `vaire tunnel --db app --target postgres` forwards a local port to that pooler, which is how a migration tool on your machine reaches the database as `<db>_migrate` without opening 5432 to the internet. `vaire console` uses usql from PATH, or downloads a pinned release and checks its SHA-256. Arguments after `--` go to `psql` as they are (`vaire psql --db app -- -c "select 1"`); put psql's flags after `--`.

## Backups and restores

```
 VPS                                                     S3-compatible bucket (R2, S3, B2, …)
┌──────────────────────────────────────────┐           ┌──────────────────────────────────────┐
│ app, billing, … (one cluster each)       │           │ <prefix>/pgbackrest/                 │
│   ──WAL, continuously (≤60 s)────────────┼──────────▶│   one stanza per database:           │
│   └─ vaire-backup-*@<db> timers ───────────┼──────────▶│   full · diff · incr sets, WAL       │
│        Sun full, Mon–Sat diff, staggered │           │   (aes-256-cbc, zstd)                │
│        Wed verify, monthly drill         │           │                                      │
└──────────────────────────────────────────┘           └──────────────────────────────────────┘
```

- **A stanza per database.** Each database's cluster has its own WAL stream, so backups, point-in-time recovery, retention and verification are all per database, in one repository with one cipher. Everything names a database: `backup list --db`, `backup now --db`, `restore <db>`, `db fork <db>`.
- **Restore window.** `vaire backup list --db app` shows the actual window, from the oldest usable backup through the latest archived WAL. The WAL fills the gaps between backup sets. `retention_full` is the window in weeks (default 4, overridable with `clusters.<db>.backup.retention_full`). Retention is time-based, so extra full backups do not shorten it.
- **`vaire backup setup`** probes the bucket from your machine, generates the repository cipher, switches every database's WAL archiving on with a reload (no restart), and takes a first full backup of each. Refresh your secrets handoff afterwards with `vaire secrets export`. Backups are optional during `server init`; until a bucket exists, `server init` and every `apply` warn `no_offsite_backup`.
- **Staggered.** Backup timers use per-database offsets so they don't all start together. Check the actual timer schedule rather than assuming that every backup runs within the maintenance window.
- **Dropped databases stay restorable for their window.** `db drop` keeps the stanza; `vaire backup list` shows it until a daily timer deletes it once the window has passed.
- **Restoring one database doesn't touch the others.** A restore builds a new cluster from the database's stanza beside the live one, you check it, and only `promote` swaps it in. It needs free space for one copy of the database.
- **`vaire db follow <db> [--name <new>] [--size <size>]`** makes a read-only physical standby of one database on the same server, copied from the source's stanza (or with `pg_basebackup` when it has none) and streaming through a replication slot capped by `max_slot_wal_keep_size`. Clients read it at the node's host with the follower's name in the path (`db1.<domain>:6432/<new>`) as the source's `_ro` or `_app` role; writes fail. `vaire db unfollow <new> --confirm <new>/unfollow` promotes it into an independent database with its own roles (new passwords, printed once), its own stanza and a first full backup, for a migration rehearsal; the source is not touched. A follower and an unfollowed copy do not inherit the source's protection: protect the copy with `vaire db protect` if it needs it.
- **`vaire db fork <db> [--name <new>] [--at <time>]`** makes a separate database: a physical copy of the source (from its stanza at a point in time, or from the live cluster), renamed, with its own roles and new passwords, the source's size and settings (`90-overrides.conf` included), its own stanza and a first full backup. It works without backups when there's no `--at`.
- **Long work runs as detachable jobs.** Backups, verify, restores, drills and forks run as systemd units on the node. Ctrl-C detaches, and `vaire job list` / `vaire job follow <id>` re-attach.
- **Checked, not assumed.** `vaire backup verify` checks every file of a database's stanza against its checksum, weekly by default. The monthly `vaire restore drill` restores each database into a throwaway cluster, runs `pg_amcheck` on it and compares it with the live one; each database's drill falls on its own day of the month. Both results show in `vaire backup list`.

### Restoring a database, step by step

The example rewinds `app` to just before a bad `DELETE` at 14:03 UTC. Every confirm token is `<db>/restore`, except cleanup's, which is `<db>/cleanup`.

1. **Pick the target.** All times are UTC. `vaire backup list --db app` shows the restore window. Use `--time` for a point in time (RFC 3339, or relative like `-15m`), `--latest` for the newest WAL, or `--set <label>` for one backup set.

   ```sh
   vaire backup list --db app
   ```

2. **Restore into a copy.** This runs as a job on the node. Ctrl-C only detaches, and `vaire job follow <id>` re-attaches. Add `--dry-run` first to see the plan. The live `app` keeps serving throughout.

   ```sh
   vaire restore app --time 2026-10-12T14:02:00Z --confirm app/restore
   ```

   pgBackRest restores `app`'s stanza into a new cluster, `app__restoring`, which replays WAL up to the target and pauses there, read-only. Only one restore per database can be open at a time; if a previous one failed, the new one clears it.

3. **Check the copy before you promote.** `restore status` compares row estimates with the live database. Query the copy directly through the session pooler to confirm it holds what you expect:

   ```sh
   vaire restore status app
   vaire sql --db app__restoring "select count(*) from users"
   ```

   If it's the wrong point in time, `vaire restore abort app` drops `app__restoring` (the live database was never touched), and you can restore again.

4. **Promote.** The transaction pooler pauses `app`, the live cluster stops, the data directories swap places, and `app` starts on the restored data with its own configuration and limits, finishes replaying to the target and opens on a new timeline. Clients see a few seconds of downtime plus that replay; other databases aren't touched. The URL, roles and passwords don't change, so the application reconnects without any change. A full backup starts right after. The old data is kept as the stopped cluster `app__pre_restore_<timestamp>`.

   ```sh
   vaire restore promote app --confirm app/restore
   vaire sql --db app "select count(*) from users"
   ```

5. **Undo or clean up.** If the promoted data is wrong, `undo` swaps the old data back the same way, as long as the leftover still exists, and vaire records which timeline to follow so later restores don't wander onto the abandoned one. Once you're happy, `cleanup` drops the leftover and frees its space. Run `vaire restore cleanup` without a database to list leftovers. `vaire db drop` drops a database's leftovers with it.

   ```sh
   vaire restore undo app --confirm app/restore       # go back to what was live before the promote
   vaire restore cleanup app --confirm app/cleanup    # or: keep the restore, drop the old copy
   ```

`vaire restore app --time … --promote --confirm app/restore` does steps 2 and 4 in one go, skipping the check. Use it only when you're sure of the target. When only some rows are needed, `vaire db fork app --name app_restored --at <time>` gives you a separate database at that point instead, without touching the live one.

## Extensions and maintenance

`vaire ext` lists and adds PostgreSQL extensions per database. A custom `[extensions.<name>]` definition can name a compatible PostgreSQL 18 extension from a trusted APT repository or from preinstalled files. Native extensions are trusted server code, not sandboxed. Removing a selected extension or deleting a database declaration through configuration editing is refused. To delete the database itself, use `vaire db drop`.

```sh
vaire ext list [--db app]
vaire ext add vector --db app
vaire ext add pg_cron --db app --confirm app/restart
vaire ext add pg_stat_kcache --db app \
  --package postgresql-18-pg-stat-kcache --preload pg_stat_kcache \
  --setting pg_stat_kcache.track=all --copy-safety safe --standby-safe \
  --confirm app/restart
```

The package install is server-wide and held; a preload change requires the exact `--confirm <db>/restart` token. A source with followers requires every affected cluster's token, configures followers first and runs SQL only on the primary; a direct change on a follower is refused. A protected database may take an addition without weakening its access policy. A custom extension whose copy/standby safety is unknown still works on a standalone primary; copies, followers and historical restores refuse it until the operator declares `safe` or `disabled` with suppressing settings.

Backups retain the resolved extension requirements they observed, bound to the immutable stanza. Restore points older than that coverage need an explicit `recovery_extensions` declaration: absent means unknown, `[]` asserts none, and a populated list names the complete requirement.

Daily maintenance is scheduled near the start of the maintenance window
(02:00–05:00 in the VPS timezone by default), with a short randomized delay.
It checks transaction ID age, analyzes stale tables, and handles restarts
already deferred to that window. PostgreSQL autovacuum runs independently.

### VPS updates

Vaire configures unattended APT updates for OS security fixes. Automatic VPS
reboots are disabled, but package installation can still restart services.
`vaire doctor` warns when the OS reports a pending reboot. The Vaire maintenance
window does not schedule VPS reboots or control APT's own timers.

Vaire does not yet offer a command that plans OS package updates by restart
impact. Use the distro's package tools to review pending updates and schedule
service restarts or a VPS reboot as needed. PostgreSQL-related packages and
nginx are excluded from unattended updates; `vaire pg update` is not yet
implemented. `vaire upgrade` updates Vaire itself, not the VPS packages.

For bloat, `vaire maint repack` rewrites one eligible table online with `pg_repack`, and opt-in `auto_repack` does so for estimated candidates at or above 64 MiB. Repack refuses protected databases and unsupported storage, never kills another backend, and reports a table whose rewrite completion cannot be attributed as `unknown`, not as repacked. `vaire maint report bloat --db app --table events --exact` measures one table with the full `pgstattuple` scan under the report's read lock and timeouts; it is not an instantaneous or reclaimable-space guarantee, and default reports remain estimates.

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
| `logrotate` | base packages | Rotates `/var/log/vaire` |
| `unattended-upgrades` | base packages | Security updates only; PostgreSQL, PgBouncer and nginx are excluded |
| `postgresql-common` | PostgreSQL packages | Adds the PGDG apt repository (`apt.postgresql.org`) and `pg_createcluster`, which makes each database's cluster |
| `postgresql-18`, `postgresql-client-18`, `postgresql-contrib` | PostgreSQL packages (PGDG) | PostgreSQL 18; the major version is held with `apt-mark hold` |
| `pgbouncer` | PostgreSQL packages (PGDG) | Two poolers: transaction mode on 6432 (Debian's `pgbouncer.service`) and session mode on 5432 (`vaire-pgbouncer-session.service`) |
| `nginx`, `libnginx-mod-stream`, `ssl-cert` | TLS step | ACME HTTP-01 on 80 and ALPN routing on 443; held with `apt-mark hold` |
| `pgbackrest` (≥ 2.59.3) | PostgreSQL packages (PGDG) | Database backups, WAL archiving and restores; held with `apt-mark hold`. 2.59.3 is the floor because earlier releases have an encryption bug |
| `vaire` | `server init` | The matching node binary at `/usr/local/bin/vaire`, installed during trusted bootstrap |
| `node_exporter` 1.12.1, `postgres_exporter` 0.20.1, `pgbouncer_exporter` 0.12.1 | metrics step (upstream release binaries, sha256 pinned in vaire) | Metrics on loopback, served through nginx's authenticated `/metrics`; the same versions on Debian and Ubuntu |

No ACME client (certbot and the like) is installed: vaire issues certificates itself. vaire's bloat report embeds the estimate queries of [ioguix/pgsql-bloat-estimation](https://github.com/ioguix/pgsql-bloat-estimation) (BSD-2-Clause; `vaire version --licenses` prints the notice).

### On your machine

| Tool | Needed for | How you get it |
| --- | --- | --- |
| `vaire` | Everything | `install.sh`, cosign-verified; `vaire upgrade` afterwards |
| WireGuard, SSH | Every remote command | Built into vaire (userspace WireGuard, in-process SSH). Nothing to install and no root needed |
| `psql` ≥ 16 | `vaire psql` only | Your package manager; vaire offers the install command |
| `usql` | `vaire console` only | On PATH, or vaire downloads a pinned release (v0.21.4) into `~/.cache/vaire/tools` and checks its SHA-256 |

### Built into the binary

| Library | Used for |
| --- | --- |
| `spf13/cobra` | Command tree and flags |
| Charm `huh`, `bubbles`, `lipgloss` | Interactive prompts and progress output |
| `golang.org/x/crypto/ssh` | The SSH client |
| `golang.zx2c4.com/wireguard` (wireguard-go + netstack) | The userspace WireGuard tunnel |
| `golang.org/x/crypto/acme` | Issuing and renewing certificates on the node |
| `jackc/pgx` | SQL over the tunnel (`vaire sql`, `db`, `tune`) |
| `filippo.io/age` | Encrypted transfer and master bundles |
| `minio/minio-go` | S3-compatible bucket access |
| `sigstore/sigstore-go` | Verifying signed releases on upgrade |
| `pelletier/go-toml` | `cluster.toml` |
| `itchyny/gojq` | `--jq` output filtering |
| `rsc.io/qr` | The QR code `vaire protect init` shows for the authenticator app |

## Agent-first

Operational commands support non-interactive structured JSON (`--output json`), with documented exit codes and `--dry-run` where it makes sense. Human-only secrets reveals and protected writes deliberately refuse unattended use. Destructive operations need a `--confirm <token>` that `--yes` never satisfies. `vaire commands --json` describes the whole surface, and `vaire guide agents` / `vaire skill install` load the operating guide into a coding agent.

## Development

```sh
just build   # bin/vaire
go test ./... # unit and regression tests; no local server harness
just lint    # go fix checks and golangci-lint
```

## License

vaire is released under the [MIT License](LICENSE). Embedded third-party code keeps its own licence (see above).
