# Phase 4 work units: observability and maintenance

Source: MASTER's Phase 4 row and its "Still to decide" list, and IMPLEMENTATION's exporters, nginx, `vaire.prom`, maintenance, timers, status and doctor sections. This splits Phase 4 into small units we take one at a time. Each unit is one stacked branch off the previous one.

**Rules for every unit**

- Ends with `just lint` and a build for all targets. No local test harnesses, no docker. Pure functions (parsers, renderers, thresholds) get Go unit tests in the existing `_test.go` style, run with `go test ./...`; everything that needs a server goes in the manual test.
- Compiles and keeps the CLI coherent, but **doesn't have to be deployable** between units. The manual test runs once at the end (unit 22), on the server the Phase 3 exit test leaves behind.
- Linux and macOS for the CLI; Debian 13 and Ubuntu 26.04 only on servers. No compatibility code for older releases.
- Each step stays `-v1` (no release yet); a changed script or SQL text goes into the step's input hash so the next `apply` re-runs it.
- "To verify on the VPS" items are listed on the unit they belong to and again in the checklist at the end.

---

## Implementation record (2026-10-14)

Current status is in [MASTER.md](MASTER.md): main alpha live qualification completed 2026-10-15–19, with coverage limits in [MANUAL-TESTING.md](MANUAL-TESTING.md). The dated build notes and units below are historical, not an active pending-test list.

Units 1-21 are built on `phase-4-observability` (not verified on a server; the manual test is unit 22). Every "Decided (recommended)" item was approved as written. Where the build differs from the units below:

- **Quiet timer jobs.** `metrics-refresh` (every minute) and `doctor` (hourly) are not recorded in `state.json`'s job list, which keeps the last 50 runs: a minute timer would push the backups out of it within the hour. They take the node lock shared and skip at once when busy.
- **Unit 2:** `vaire_info{…,server,…}` replaces `cluster`. `vaire-host.prom` also has `vaire_cluster_running`, `vaire_memory_reserve_bytes` and `vaire_committed_bytes`; the deferred restarts are `vaire_deferred_restart_requested_timestamp_seconds{db}`.
- **Unit 5 (reserved slots, changed after review):** `config.ReservedConnections` stays 3 and the pool sizes are unchanged; tuning sets `superuser_reserved_connections = 0` and `reserved_connections = 3` (they sum to the 3 the pools leave free; superusers may use `reserved_connections` too), and `vaire_monitor` shares the three slots with `vaire_admin`. `vaire_monitor` has `CONNECTION LIMIT 2`, not 1: postgres_exporter 0.20.1's `/probe` registers the legacy exporter (its own connection, for `pg_up` and the default metrics) beside the probe collectors (a second connection) and gathers both at once, so a limit of 1 would fail one of them on every scrape. That still leaves at least one reserved slot to `vaire_admin`. Existing clusters get a restart pending for the two settings.
- **Unit 7:** `vaire metrics rotate` writes the new hash on the node directly (nginx reads the file per request, no reload). The fail2ban jail bans after 10 bad passwords in 10 minutes, for an hour.
- **Unit 9:** the stanza facts are read after each backup and verify (and the first full in `pgbackrest-v1`); `stanza-expiry` only handles dropped stanzas, which have no metrics. `records.json` gains `last_backup_failures` and `repo`.
- **Unit 11:** `vaire.version_skew` always passes: the session already refuses a node on another build. `jobs.skipped` and `jobs.failed` are in the backups category.
- **Unit 13:** the init gate runs inside `cluster.bootstrap phase=database` (so `--resume` re-runs it), and `jobs.*` are exempt during init along with `secrets.confirmed` and `backup.*`.
- **Unit 14:** `queries --reset` runs `pg_stat_statements_reset()` as `postgres` (the function is superuser-only) instead of granting it to `vaire_admin`; the top statements are read as `vaire_admin`.
- **Unit 15:** `vaire top` reads the database list from the context's placement cache (refreshed from the master when empty) and the admin consoles of both poolers (6432 and 5432) as `vaire_admin` with the simple query protocol; `--once`, `--json` or no terminal print one snapshot two seconds apart (so rates exist).
- **Unit 17:** the release ships raw binaries, not tarballs, so there is no `THIRD_PARTY_NOTICES` file; the notice is embedded and printed by `vaire version --licenses` (and named in the README).
- **Unit 19:** `maint repack` refuses even when `pg_repack` is installed (`repack_unavailable`), since vaire doesn't install or run the `pg_repack` client before Phase 7.
- **Unit 21:** MASTER, IMPLEMENTATION, README, the agent guide and MANUAL-TESTING are updated; the phase stays 🟡 until the manual test passes.

---

## Scope

**In Phase 4**

- **Metrics:** `node_exporter`, `postgres_exporter` (one target per cluster) and two `pgbouncer_exporter`s, pinned upstream binaries bound to 127.0.0.1, behind an authenticated `/metrics` endpoint on nginx's HTTPS server, with a service-discovery file so new databases appear in the scraper by themselves. vaire's own textfile metrics grow per-cluster cgroup and PSI lines, TLS expiry, doctor results and the backup facts `pgbackrest_exporter` would have given.
- **Read views:** `vaire status`, `vaire doctor` (every check, including the security subset), `vaire queries`, `vaire top`.
- **Maintenance:** `internal/maint` with `maint vacuum|analyze|reindex|report|kill` per database, `maint repack` as a refusal until Phase 7, the `maint-daily` and `maint-weekly` timers, and the maintenance window that `db resize --defer-restart` has been waiting for.
- **Steps 16 and 18:** `metrics-v1`, and doctor as the final gate of `server init` and a check after every `apply`.
- **Settled here:** the reserved-connection arithmetic left open in Phase 3 unit 15 (it decides whether the exporter can still connect when both pools are full).

**Not in Phase 4**

- `pg_repack`, auto-repack and `pgstattuple` exact bloat: they need `vaire ext add` (Phase 7).
- Alerting (Phase 9). Phase 4 exposes everything an external Prometheus or Grafana Cloud needs to alert, including the doctor results as metrics, but vaire sends nothing itself.
- Metrics storage, dashboards or a web UI on the box.
- Follower lag in `status` and `doctor` (Phase 6).

---

## Decisions

Each is **Decided (recommended)**: approve it or change it before the unit that needs it starts.

### D1. Exporter versions: pinned upstream release binaries

| Exporter | Pin | Released | Debian 13 package | Ubuntu 26.04 package | PGDG |
| --- | --- | --- | --- | --- | --- |
| `node_exporter` | **1.12.1** | 2026-07-14 | `prometheus-node-exporter` 1.9.0 | 1.10.2 (universe) | none |
| `postgres_exporter` | **0.20.1** | 2026-07-08 | `prometheus-postgres-exporter` 0.17.1 | 0.19.0 (universe) | none |
| `pgbouncer_exporter` | **0.12.1** | 2026-06-26 | `prometheus-pgbouncer-exporter` 0.10.2 | 0.11.0 (universe) | none |
| `pgbackrest_exporter` | **not installed** (see D2); 0.24.0 if overruled | 2026-07-30 | none | none | none |

Checked 2026-10-14 against GitHub releases, `qa.debian.org/madison` (trixie, resolute) and PGDG's `trixie-pgdg` index (PGDG ships `pg-exporter` and `prometheus-sql-exporter`, not these).

sha256 of the release tarballs, from each release's `sha256sums.txt`:

```
node_exporter-1.12.1.linux-amd64.tar.gz        b51d8a76aa2a9156a55d501aca6276fae09e262259a5e4e831d2c2222f084e63
node_exporter-1.12.1.linux-arm64.tar.gz        ad35b605f9954b9f1ffddf5ba054bdc5a98d790b9eae5291e1eeb83f1ecbd0e7
postgres_exporter-0.20.1.linux-amd64.tar.gz    89d4f7e7920cad48fdc3133f789556ef5253c330a9f5fdace3bdb6344c0a8b5a
postgres_exporter-0.20.1.linux-arm64.tar.gz    d5d86fb98bb1f26b088d1a6fda07fd6b6f035cb5d40492f75ec3bfebb5ddfe9d
pgbouncer_exporter-0.12.1.linux-amd64.tar.gz   fa391b513d3a3f03843a6e0d64e711fda1f057dc43133db5c1b130ba38767857
pgbouncer_exporter-0.12.1.linux-arm64.tar.gz   0abb808ae8e251126c854ebdce382993876df629b34c0401816d3818db92c700
```

Why upstream binaries and not the distribution packages:

- **One version on both OSes.** Debian and Ubuntu ship different versions three minors apart, so metric names, collector flags and PG18 support would differ by OS. 0.20 is the release that moved `stat_activity`, `stat_archiver`, `settings`, `stat_replication` and `replication_slots` into standalone collectors, which is what makes them work under `/probe` (D4); 0.17 and 0.19 don't.
- **Ubuntu's are in universe**, outside Ubuntu's standard security support.
- **Distribution defaults fight us.** Debian's `prometheus-node-exporter` listens on every interface on install and pulls in `prometheus-node-exporter-collectors` with its own timers; we'd spend the step undoing them.
- **vaire controls upgrades.** The pins live in `internal/versions`, inside the cosign-signed vaire binary, so the sha256 is as trusted as vaire itself (the Prometheus projects don't sign releases). A version bump is an vaire release, and `unattended-upgrades` never touches them.

Cost: vaire tracks upstream security releases itself. The docs unit adds the exporter pins to the "re-check before each phase" note beside pgBackRest's.

### D2. vaire's own metrics replace `pgbackrest_exporter`

**Decided (recommended): don't install `pgbackrest_exporter`.** vaire's backup jobs already write `vaire.prom`; unit 9 adds what the exporter would have added that matters for alerting, and `postgres_exporter`'s `pg_stat_archiver` gives archive health per cluster.

- **Secrets.** The exporter runs `pgbackrest info` on a timer, so a long-running HTTP process would need the S3 keys and the repository cipher (`pgbackrest.conf`, postgres 0600). Today only short jobs touch them.
- **Bucket calls.** It lists every stanza's `backup.info` and archive every collection interval: N stanzas × 144 calls a day at the documented 600 s, for numbers that only change when a backup runs. vaire's jobs call `pgbackrest info --stanza` once at the end of each backup, verify and expiry, which they already know the result of.
- **One binary fewer** to pin, harden and keep current.
- What we give up: the exporter's per-set details (each backup's size, delta, repo key) and in-progress backup bytes. `vaire backup list` and `vaire job follow` cover those for a human; the metrics keep last success, size, duration and window per database and type.

### D3. Table-bloat query: the ioguix estimate, vendored (BSD-2-Clause)

**Decided (recommended):** `maint report bloat` and `maint-weekly` use the estimation queries from [ioguix/pgsql-bloat-estimation](https://github.com/ioguix/pgsql-bloat-estimation) (`table/table_bloat.sql` and `btree/btree_bloat.sql`, the non-superuser versions), vendored as `internal/maint/sql/` with the licence file and a header naming the upstream commit.

- **Licence:** BSD-2-Clause (copyright 2015-2019 Jehan-Guillaume de Rorthais). The binary embeds the SQL, so the notice ships with it: `internal/maint/sql/LICENSE.ioguix` is embedded and printed by `vaire version --licenses` (new hidden flag), and the release tarballs carry a `THIRD_PARTY_NOTICES` file.
- **Why not pgstattuple:** it reads the whole table (and `pgstattuple_approx` still reads every page the visibility map doesn't mark all-visible), which is heavy I/O on a shared disk, and it needs `CREATE EXTENSION pgstattuple` in the user's database, which is Phase 7's `ext add`. The estimate reads only `pg_stats` and `pg_class` and is what `check_pgactivity` and most monitoring tools use.
- **Why not pgexperts' `pgx_scripts`:** BSD-3-Clause and fine too, but its table query is a variant of the same estimate with no btree query, and it's no longer maintained (last change 2023).
- **The limit, said in the report:** an estimate from statistics; tables without fresh `ANALYZE` or with columns `vaire_admin` can't read stats for are listed as `unestimated`. Phase 7 adds `--exact` (pgstattuple_approx on one table) once `ext add pgstattuple` exists.

### D4. postgres_exporter: one multi-target instance, a probe per cluster

**Decided (recommended): one `postgres_exporter` process, scraped through `/probe`**, with the target fixed per cluster by nginx (unit 7), not one exporter process per cluster.

- **Memory.** The exporters live in the server reserve (`max(512 MiB, 25% of RAM)`), not in any cluster's budget. One process is about 25-40 MiB whatever the number of databases; one per cluster is that times N: 10 databases on a 2 GiB VPS would put ~300 MiB of exporters into a 512 MiB reserve that also holds the OS, two poolers, nginx and vaire. The backend a probe opens is forked by the cluster's postmaster, so it is charged to **that cluster's** cgroup, for the length of one scrape (a few MiB, inside `MemoryHigh`'s 25% headroom).
- **Units and ports.** N units, N ports and an N-way render on every `db create` versus one unit whose targets live in nginx's config.
- **What a probe is.** 0.20.1's `/probe` builds, per scrape, the legacy exporter (one connection, `pg_up` and the default metrics) and the probe collectors (a second connection, every enabled collector), gathers both concurrently and closes both, so a probe holds two connections. The target is a URL with the socket directory in the query: `postgresql://vaire_monitor@/<db>?connect_timeout=5&host=%2Fvar%2Frun%2Fpostgresql&port=<port>&sslmode=disable` (the role's `statement_timeout` bounds a probe). Not a key=value DSN: `ConfigureTarget` accepts one, but turns it into a URL with `host:port` as its authority, and a socket path there becomes `%2F...`, which lib/pq refuses (found on the Phase 4 live run). `dbname` is the application database, so `stat_user_tables` and `stat_statements` see the application's tables; the server-wide collectors are the same from any database. No `auth_module`: the exporter runs as the system user `postgres_exporter`, which `pg_ident` already maps to `vaire_monitor` with peer auth, so the exporter holds no password.
- **No raw `/probe` from outside.** nginx exposes `/metrics/postgres/<db>` and sets the query string itself (the client's is dropped), so a scraper can't make the exporter connect to an arbitrary DSN.
- **Connection slots.** A probe needs a slot exactly when the pools are full, which is when the metrics matter. `postgres-roles-v1` grants `pg_use_reserved_connections` to `vaire_monitor` (beside `vaire_admin`), so the two share the reserved slots; it caps `vaire_monitor` with `CONNECTION LIMIT 2` per cluster (one probe's two connections) and sets `statement_timeout = 10s` and `lock_timeout = 1s` on the role.
- **Settles Phase 3 unit 15's open question:** `config.ReservedConnections` stays **3** and the pool sizes stay as they were (a `nano` transaction pool is 7). Tuning now sets `superuser_reserved_connections = 0` and `reserved_connections = 3` (it was 3 + 3, so ordinary roles got `max_connections − 6` and both pools full hit "remaining connection slots are reserved" three connections early). Both pools full leave exactly the three slots, shared by `vaire_admin`, `vaire_monitor` (at most two, one probe) and superusers, who may use `reserved_connections` too. The change is a postmaster setting, so existing clusters report a restart pending after the upgrade.
- Collectors on top of the defaults: `stat_statements` (limit 50, no query text: `--no-collector.stat_statements.include_query`), `stat_checkpointer`, `database_wraparound`, `long_running_transactions`. `stat_user_tables` stays on (series per table; see the risks).

### D5. `/metrics` auth: bcrypt basic auth, hash made on the operator, credential in the secrets document

**Decided (recommended):**

- **Where:** nginx's TLS server on `127.0.0.1:8443`, which every non-PostgreSQL connection to 443 already reaches. Paths under `[metrics] path_prefix` (`/metrics`): `/metrics/node`, `/metrics/postgres/<db>`, `/metrics/pgbouncer`, `/metrics/pgbouncer-session` and `/metrics/targets.json` (D6). Nothing listens on 80 for it.
- **Credential:** user `metrics`, password 32 chars base62, generated **on the operator** like every other vaire secret (`crypto/rand`), at `server init`'s database phase and topped up by `apply` on servers that predate Phase 4. It's saved to `credentials.json` (`metrics {username, password}`) **before** it's sent, as the service passwords are.
- **The node never sees the plaintext.** The operator computes the bcrypt hash (`golang.org/x/crypto/bcrypt`, already a dependency through `x/crypto`) and sends only the hash as the request secret `metrics.htpasswd` (redacted like a SCRAM verifier). The node writes `metrics:<hash>` to `/etc/nginx/vaire-metrics.htpasswd`, `root:www-data 0640`. It isn't in `/etc/vaire/runtime/` as IMPLEMENTATION's files table says: nginx's workers read the file on each request as `www-data`, and `runtime/` is root 0700.
- **Set once.** Written only when missing, like the service passwords, so an operator with a stale `credentials.json` never rotates it by accident. `vaire metrics rotate --confirm <server>/metrics` makes a new one, saves it, sends the hash with a `rotate` flag, and warns `secrets_document_stale`.
- **bcrypt cost 8.** The password has 190 bits of entropy, so the cost protects nothing an attacker could brute-force anyway; at cost 10 a scraper hitting N+3 paths every 15 s would spend measurable CPU on hashing. Cost 8 is ~15 ms per request.
- **Handoff:** the secrets document's existing `metrics` section (`base_url`, `username`, `password`) is filled, so the plain-text record gets a *Metrics* group and the transfer bundle carries it; `vaire login` stores it in `credentials.json`. `base_url` is `https://<node hostname><path_prefix>`. `vaire metrics show` prints it again from the context (password only with `--reveal`).
- **Hardening on top of the password:** `limit_req` on the metrics locations (10 r/s per address, burst 40); fail2ban's stock `nginx-http-auth` jail on bad passwords; an optional `[metrics] allowlist` of CIDRs (empty = any address with the password). The last two need the client's real address at 8443, which today is always 127.0.0.1 behind the stream hop, so unit 7 turns on the PROXY protocol between nginx's own servers (D7).

### D6. Service discovery: `targets.json` for Prometheus `http_sd` and Alloy `discovery.http`

**Decided (recommended):** `metrics-v1` renders `/metrics/targets.json`, behind the same password, in the `http_sd` format: one entry per path with `__metrics_path__`, `__scheme__ = https`, and labels `vaire_server`, `vaire_node`, `vaire_component` (`node`, `postgres`, `pgbouncer`) and `db` / `pooler` where they apply. A scraper configured once picks up `db create`, `db drop` and `db fork` without edits. `vaire metrics show` prints a Prometheus `scrape_configs` block and a Grafana Alloy block that use it. Without it the operator would have to edit the scrape config for every database, since each database is its own path.

### D7. PROXY protocol on nginx's internal hops

**Decided (recommended):** the 443 `stream` server sends the PROXY protocol header (`proxy_protocol on`) to every internal upstream: 8443 (`listen … ssl proxy_protocol`, `set_real_ip_from 127.0.0.1; real_ip_header proxy_protocol;`) and the stream servers on 6433, 6434 and 6435 (`listen … proxy_protocol`). Only nginx's own servers sit behind the hop, so nothing outside vaire sees the header. This gives the metrics allowlist, `limit_req` and fail2ban the client's address; PgBouncer still sees 127.0.0.1 from 6433, as now. It's a `tls-v1` template change, so it lands with unit 7.

### D8. Maintenance runs on the node, as `vaire_admin`, over the cluster's socket

**Decided (recommended):** every `maint` command is an RPC that runs node-side on the database's socket, not over the tunnel through the session pooler as IMPLEMENTATION's maintenance section allows. Vacuum and reindex can outlive an SSH session, so they run as detachable jobs (as backups do); reports are short RPCs. They log in as `vaire_admin` (peer map `root → vaire_admin`), whose `pg_maintain` (PG17+) covers `VACUUM`, `ANALYZE` and `REINDEX` on every table, `pg_monitor` covers the reports, and `pg_signal_backend` covers `kill` without reaching superuser backends. No pooler slot is taken. Every session sets `lock_timeout = '5s'`; reports also run `READ ONLY` with `statement_timeout = '60s'`.

`vaire top` stays as MASTER decided: it reads PostgreSQL over the tunnel (D9).

### D9. `top` over the tunnel; `status`, `doctor` and `queries` node-side

**Decided (recommended):** `vaire status`, `vaire doctor` and `vaire queries` are one RPC each; the node reads every cluster's socket, cgroup files, `records.json` and both poolers' admin consoles in one round trip. `vaire top` refreshes every 2 s, so it keeps one `pgx` connection per database through the session pooler as `vaire_admin` (read-only) plus one to the session pooler's admin console, as MASTER decided; cgroup numbers stay in `status`.

### D10. Doctor gates `server init`, warns after `apply`; it is not a journaled step

**Decided (recommended):** IMPLEMENTATION's `finalize-v1` ("never complete, always runs") would make every `apply` report one applied step, which breaks "a second `apply` changes nothing". Instead, step 18 is a **post-apply gate** in the runner, not a catalogue step: after the steps, `apply` and `server init` run the node's doctor checks and return them in `data.doctor`.

- `server init` fails on any `fail` (`doctor_failed`, resumable with `--resume` after the fix), except checks that can't pass yet during init (`secrets.confirmed`; `backup.*` before the first full).
- `apply` turns each `fail` into a warning `doctor_check_failed` naming the check and its fix. Blocking `apply` would block the very fix of some checks (a `config set` that needs an `apply`).
- `--skip-doctor` on both, for an operator who knows why.

### D11. Deferred restarts run at the start of the maintenance window

**Decided (recommended):** `db resize --defer-restart` records the database in `state.json`'s `deferred_restarts` (database, requested_at). `maint-daily`, which starts at the window's start, restarts those clusters one at a time with the `Restarter` (PAUSE/RESUME), under each cluster's lock exclusive, and clears the entry. Only explicitly deferred restarts run; a restart that's merely pending (no `--confirm`, no `--defer-restart`) never happens by itself. A cluster whose lock is busy (a backup) is retried the next night and reported by doctor's `pg.restart_pending`.

### D12. Confirm tokens per database

**Decided (recommended):** the Phase 2 tokens predate a cluster per database. `maint kill` needs `--db` and `--confirm <db>/kill` (was `<server>/kill`). `queries --reset` takes `--confirm <db>/stats` with `--db` and `<server>/stats` without (resets every database). `maint repack` keeps `<db>`. New: `metrics rotate` takes `<server>/metrics`.

### D13. Timers

**Decided (recommended):** `timers-v1` adds four server-wide timers: `vaire-maint-daily` (every day at the window's start, 10 min jitter), `vaire-maint-weekly` (Saturday at the window's start), `vaire-metrics-refresh` (**every minute**, not every 5 minutes, so PSI and `memory.current` have a useful resolution; it's a short Go run that reads files) and `vaire-doctor` (hourly, new: doctor results reach the scraper as metrics, which is the only alerting path until Phase 9). `metrics-refresh` and `doctor` skip at once on a busy lock instead of the 15 minutes a timer run waits today. All maintenance timers are server-wide units that walk the clusters in name order, not templates per database: the jobs are light, and a template per database would multiply units for no gain.

---

## A. Foundations

### 1. Exporter pins and the node-side fetcher
- `internal/versions`: `NodeExporterVersion = "1.12.1"`, `PostgresExporterVersion = "0.20.1"`, `PgbouncerExporterVersion = "0.12.1"`, and `ExporterSHA256` keyed `<name>-<arch>` with the sums from D1. `cmd/print` keeps printing only the cosign values `just lint` greps for in `install.sh`.
- New `internal/exporters`: `Artifact(name, arch)` (URL, tarball name, binary path inside it), `Fetch(ctx, name, arch, dest)`: HTTPS download from the GitHub release into a temp file, sha256 **before** extraction, extract the one binary to `/usr/local/lib/vaire/exporters/<name>-<version>/<name>` (root 0755), fsync, rename. A present, matching binary is not downloaded again; older version directories are removed once the unit points at the new one.
- Files: `internal/versions/versions.go`, `internal/exporters/{exporters,fetch}.go`.
- Error codes: a checksum mismatch is `release_verification_failed` (exit 14, the code for any downloaded binary that fails verification); a download failure is `external_command_failed` with `details.url`.
- Tests: every exporter has a sum for `amd64` and `arm64` (also checked by `just lint` through a new `vaire commands --check-steps` assertion); `Artifact` names match the upstream tarball layout (`node_exporter-1.12.1.linux-amd64/node_exporter`); tar extraction refuses paths outside the target and extra files.
- Done when: the fetcher builds for linux and is linked only into node mode; lint fails if a pin lacks a sum.

### 2. The textfile layout and `metrics-refresh`
- Move the textfile writer out of `internal/backup/metrics.go` into `internal/promfile`: a small writer (`Family`, `Sample`, `WriteFile(name, families)`, atomic rename, `0644`) and the directory `/var/lib/node_exporter/textfile` (root 0755). One file per owner, so no writer needs another's data or a shared lock: `vaire.prom` (backup, unchanged lines, `internal/backup`), `vaire-host.prom` (unit 8), `vaire-doctor.prom` (unit 11), `vaire-maint.prom` (unit 20). Phase 5 adds `vaire-master-backup.prom`.
- `vaire_info{version,cluster,node}` becomes `vaire_info{version,server,node}` (the Phase 3 rename that missed it).
- `node run metrics-refresh` stops returning `not implemented yet`: it rewrites `vaire.prom` and `vaire-host.prom`. It takes the global lock shared with timeout 0 and skips at once when busy (D13).
- Files: `internal/promfile/promfile.go`, `internal/backup/metrics.go`, `internal/node/jobs.go`.
- Tests: label escaping (`"`, `\`, newline), `# HELP`/`# TYPE` once per family, deterministic order, an empty family left out.
- Done when: every current `WriteMetrics` caller (13 of them) goes through `promfile`; `vaire.prom`'s output is byte-identical apart from the `server` label.

### 3. The metrics credential and the handoff
- `contexts.Credentials.Metrics {username, password}`; `ensureMetricsSecret` beside `ensureClusterSecrets`: generate when missing, save, and return the bcrypt hash (cost 8) as the RPC secret `metrics.htpasswd` (`steps.SecretMetricsHash`, registered for redaction). `server init`'s database phase and every `apply` send it; a newly generated one warns `secrets_document_stale`.
- `secretsdoc`: the document's `metrics` section from the context (`base_url` from the node hostname and `path_prefix`); `vaire login` stores it; the handoff's *Metrics* group appears.
- Files: `internal/contexts/{contexts,credentials}.go`, `internal/cli/{dbcreds,serverinit_db,steps,handoff,login,secrets}.go`, `internal/secretsdoc/{document,context}.go`, `internal/steps/secrets.go`.
- Tests: the hash verifies the password; the document round-trips through `age` with the section; a context without `metrics` tops up exactly once.
- Done when: `vaire secrets export` on a Phase 3 context shows a Metrics group after one `apply`.

---

## B. Exporters and the endpoint

### 4. `metrics-v1`: node_exporter and the exporters' slice
- New step `metrics-v1` (step 16), after `pgbackrest-v1` and before `timers-v1` in `DatabaseStepIDs()` and the catalogue. It skips with `metrics_disabled` when `[metrics] enabled = false`, after stopping and disabling the exporters and removing nginx's metrics include (unit 7).
- This unit: system user `node_exporter` (no login, no home), `Fetch`, `vaire-exporters.slice` (`CPUWeight=20`, `MemoryMax=256M`), `node_exporter.service` in that slice: `--web.listen-address=127.0.0.1:9100`, `--collector.textfile.directory=/var/lib/node_exporter/textfile`, `--collector.systemd --collector.systemd.unit-include='(postgresql@18-.*|pgbouncer|vaire-.*|nginx|wg-quick@wg0)\.(service|timer)'`, `MemoryMax=64M`, and IMPLEMENTATION's hardening (`NoNewPrivileges`, `ProtectSystem=strict`, `ProtectHome`, `PrivateTmp`, `ReadOnlyPaths=/`).
- The units are named after the exporters (`node_exporter.service`, not `vaire-…`) so `timers-v1`'s stale-unit sweep never touches them.
- Check: binary at the pinned version, unit file equals the render, active, and `GET http://127.0.0.1:9100/metrics` returns 200 with an `vaire_info` line.
- Files: `internal/steps/metrics.go`, `internal/steps/scripts/metrics.sh`, `internal/render/templates/{node_exporter.service,vaire-exporters.slice}.tmpl`, `internal/steps/catalog.go`.
- CLI: `vaire logs node_exporter`.
- Tests: unit render (flags, slice, hardening); `--check-steps` sees the new ID.
- To verify on the VPS: the systemd collector reading unit states as a non-root user over D-Bus; the textfile collector reading `vaire*.prom`.
- Done when: `apply` on a Phase 3 server installs and starts it, and a second `apply` skips it.

### 5. postgres_exporter, one probe per cluster
- `metrics-v1` installs `postgres_exporter.service` (user `postgres_exporter`, already in `pg_ident`), `--web.listen-address=127.0.0.1:9187`, `--config.file=` (none), no `DATA_SOURCE_NAME`, the collectors from D4, `MemoryMax=128M`, in `vaire-exporters.slice`.
- `render.ProbeTarget(spec)`: the key=value DSN from D4, URL-encoded for nginx (unit 7).
- `postgres-roles-v1`: `GRANT pg_use_reserved_connections TO vaire_monitor`, `ALTER ROLE vaire_monitor CONNECTION LIMIT 2`, `SET statement_timeout = '10s'`, `SET lock_timeout = '1s'`; the SQL text joins the step's input hash so every cluster re-runs it once.
- `config.ReservedConnections` stays 3 (D4) and the pool sizes don't change; tuning's `superuser_reserved_connections` / `reserved_connections` become 0 / 3 so they sum to it (a restart pending on existing clusters).
- Check: active, and a probe of the first cluster returns `pg_up 1`.
- Files: `internal/steps/{metrics,postgres_roles}.go`, `internal/render/{render.go,templates/postgres_exporter.service.tmpl}`, `internal/config/{sizes,clusters}.go`.
- Tests: the DSN for a cluster (socket dir, port, dbname, options quoting) and its URL encoding; the pool arithmetic for every size; the two reserved settings summing to `config.ReservedConnections`.
- To verify on the VPS: the exporter starting with no `DATA_SOURCE_NAME` and serving `/probe` (fallback: point `DATA_SOURCE_NAME` at the first cluster and ignore `/metrics`); a key=value target with `host=/var/run/postgresql` reaching the socket; peer auth as `vaire_monitor`; `pg_stat_statements_*` and `pg_stat_user_tables_*` series for the application database; a probe still answering with both pools saturated (reserved slots).
- Done when: `curl '127.0.0.1:9187/probe?target=…'` returns metrics for each cluster.

### 6. Two pgbouncer_exporters
- `pgbouncer_exporter.service` (transaction pooler, `127.0.0.1:9127`, socket `/var/run/postgresql` port 6432) and `pgbouncer_exporter-session.service` (session pooler, `127.0.0.1:9128`, socket `/run/vaire-pgbouncer-session` port 5432), user `pgbouncer_exporter`, `MemoryMax=32M` each. The connection string with `vaire_monitor`'s password goes in `PGBOUNCER_EXPORTER_CONNECTION_STRING` from `/etc/vaire/runtime/pgbouncer_exporter{,-session}.env` (root 0600; systemd reads it as PID 1, so it stays in `runtime/`), written from `monitor.pass`.
- Skipped while there are no databases (both poolers are stopped then).
- Files: `internal/steps/metrics.go`, `internal/render/templates/pgbouncer_exporter.service.tmpl`.
- Tests: both renders; the env file never appears in a plan diff (secret).
- To verify on the VPS: `SHOW STATS` / `SHOW POOLS` as `vaire_monitor` on both admin consoles; `pgbouncer_up 1` from each.
- Done when: both answer on loopback with the pooler's pools.

### 7. The authenticated endpoint, service discovery and `vaire metrics`
- `tls-v1`'s `nginx.conf`: the PROXY protocol on the internal hops (D7); `include /etc/nginx/vaire-metrics*.conf;` inside the 8443 server (a glob, so nginx starts without the file); a `limit_req_zone` for metrics.
- `metrics-v1` renders `/etc/nginx/vaire-metrics.conf` (owned by this step): an exact `location` per path from D5, each with `auth_basic`, `auth_basic_user_file /etc/nginx/vaire-metrics.htpasswd`, `limit_req`, the optional `allow`/`deny all` from `[metrics] allowlist`, and `proxy_pass` to the exporter; `/metrics/postgres/<db>` sets `$args` to the rendered probe target so the client's query string is ignored. `/metrics/targets.json` (D6) is a static file next to it. `nginx -t`, then reload; a failed test restores the previous file and fails the step.
- `htpasswd`: written from `metrics.htpasswd` only when missing, or when the request carries `metrics_rotate` (D5); missing secret and missing file is `input_required`.
- fail2ban: `/etc/fail2ban/jail.d/vaire-metrics.conf` enables `nginx-http-auth` on 443 (`iptables` ban actions, as the sshd jail).
- `[metrics] allowlist` (CIDRs; validated like `client_allowlist`).
- `db create`, `db fork` and `db drop` run `metrics-v1` after the poolers (`steps.MetricsStepIDs()`), so the database's path and its `targets.json` entry follow. A failure is the warning `metrics_reload_failed`, not an error.
- CLI: `vaire metrics show [--reveal]` (base URL, user, the paths, the `targets.json` URL, Prometheus and Alloy snippets; password only with `--reveal`) and `vaire metrics rotate --confirm <server>/metrics [--dry-run]` (mutating, destructive: running scrapers fail until updated).
- RPC: `metrics.show` (paths and targets from the node's render), `metrics.rotate` (global lock exclusive).
- Files: `internal/render/templates/{nginx.conf,vaire-metrics.conf,targets.json}.tmpl`, `internal/steps/{tls,metrics,host_baseline}.go`, `internal/config/{cluster,clusters}.go`, `internal/node/{methods,metrics,db}.go`, `internal/cli/{metrics,tree}.go`.
- Error codes: `input_required` (no hash for a missing file); warnings `metrics_reload_failed`, `secrets_document_stale` (rotate).
- Tests: render with zero, one and three databases; allowlist rendering; `targets.json` is valid JSON with one entry per path; the probe target can't be overridden by a query string (render puts `set $args` before `proxy_pass`).
- To verify on the VPS: `curl https://db1.<domain>/metrics/node` → 401 without, 200 with the credential; `/metrics/postgres/app?target=…` still scrapes `app`; a database created afterwards appears in `targets.json`; Prometheus `http_sd` (or Alloy `discovery.http`) honouring `__metrics_path__`; PROXY protocol: nginx's error log shows the real client address, PgBouncer still sees 127.0.0.1 on 443, direct TLS on 443 and the `N`/reject paths still work; fail2ban banning after repeated bad passwords; bcrypt (`$2b$`, cost 8) accepted by nginx's `crypt()` on both OSes (libxcrypt).
- Done when: an external Prometheus scrapes every path through one `http_sd` config.

### 8. Per-cluster cgroup lines and TLS expiry
- `vaire-host.prom`, written by `metrics-refresh` (every minute, D13): per database `vaire_cluster_memory_current_bytes`, `vaire_cluster_memory_high_bytes`, `vaire_cluster_memory_max_bytes` (when set), `vaire_cluster_memory_events_total{event="high|max|oom|oom_kill"}`, `vaire_cluster_pressure_seconds_total{resource="cpu|memory|io",kind="some|full"}`, `vaire_cluster_cpu_seconds_total`, `vaire_cluster_io_bytes_total{direction}`, `vaire_cluster_restart_pending`; server-wide `vaire_memory_budget_bytes`, `vaire_memory_allocated_bytes`, `vaire_commit_limit_bytes`, `vaire_tls_cert_expiry_timestamp_seconds`, `vaire_deferred_restart{db}`.
- `internal/hostinfo/cgroup.go`: `ClusterCgroup(db)` (the path `budget.go` already reads), `ReadPressure`, `ReadMemoryEvents`, `ReadCPUStat`, `ReadIOStat`; `clusterMemoryCurrentMiB` moves there.
- Files: `internal/hostinfo/cgroup.go`, `internal/node/{budget,metrics}.go`.
- Tests: parsers on captured `memory.pressure`, `memory.events`, `cpu.stat`, `io.stat` text (a missing `full` line, an empty `io.stat`).
- To verify on the VPS: PSI files present under `system-postgresql.slice/postgresql@18-<db>.service` on both OSes (kernel `psi=1`); `memory_events_total{event="high"}` rising under the Phase 3 throttle test.
- Done when: the lines appear for every running cluster and stop for a dropped one.

### 9. Backup facts in place of pgbackrest_exporter
- At the end of `backup-*`, `backup-verify` and `stanza-expiry`, the job runs `pgbackrest info --stanza=<s> --output=json` once and records in `records.json`: per database and type the last set's `size`, `repo_size`, `delta` and duration; per database the window start (oldest restorable point) and the count of sets. A failed backup records `last_failure` per type.
- `vaire.prom` gains `vaire_backup_last_size_bytes{db,type}`, `vaire_backup_last_repo_size_bytes{db,type}`, `vaire_backup_last_duration_seconds{db,type}`, `vaire_backup_last_failure_timestamp_seconds{db,type}`, `vaire_backup_window_start_timestamp_seconds{db}`, `vaire_backup_sets{db}`.
- No new process and no timer touches the bucket.
- Files: `internal/backup/{pgbackrest,state,metrics}.go`, `internal/node/{jobs,backup}.go`.
- Tests: parsing a captured `info --output=json` (two fulls, diffs, a stanza with no backup); the window start after expiry.
- Done when: after a backup the six series exist for that database, and archive health is visible from the exporter's `pg_stat_archiver_*`.

---

## C. Read views

### 10. `vaire status`
- New `internal/health`: per-cluster snapshots read concurrently over each socket (`clusterPG`): state, uptime, `pg_database_size`, connections by state against `max_connections`, cache hit ratio, `tps` (two `pg_stat_database` reads 1 s apart), WAL bytes, `pg_stat_archiver`, `pending_restart`; both poolers' `SHOW POOLS`/`SHOW STATS` over their sockets; cgroup readings from unit 8; `records.json` (last backups, window, drill, open restore); the server sheet and its warnings; `no_offsite_backup`; TLS `not_after`; data-root free %.
- RPC `status {db}`: no lock (must answer while an `apply` holds the global lock); a cluster that can't be read is `state: unreachable` with the error, never a failed RPC.
- CLI: `vaire status [--db] [--watch]`: a server card (memory budget, warnings, poolers, disk, TLS), then a table per database (size, memory use/limit, connections, cache hit, TPS, last backup, window, drill); `--watch` re-runs the RPC every 5 s on the shared tunnel. JSON is IMPLEMENTATION's status data.
- Files: `internal/health/*.go`, `internal/node/status.go`, `internal/cli/{status,tree}.go`, `internal/cli/ui` (cards).
- Error codes: `database_not_found` for `--db`; warnings as `plan` returns them plus `restart_pending`, `restore_in_progress`, `exporter_down`.
- Tests: cache-hit and TPS arithmetic (zero denominators, counter reset), the PITR window from records, rendering a stopped and an unreachable cluster.
- Done when: `vaire status` on a two-database server answers in about 1-2 s and `--json` validates against the schema.

### 11. Doctor: framework, host, SSH, firewall, TLS, secrets and metrics checks
- New `internal/doctor`: `Check{ID, Category, Security bool, PerDatabase bool, Run}`, `Result{ID, DB, Status: pass|warn|fail|skip, Detail, Fix}`, a registry and `Run(ctx, env, filter)`. `skip` is new: a check that doesn't apply (no bucket for `backup.archiving`, metrics off).
- This unit's checks: `host.os_supported`, `host.reboot_required`, `host.disk_data` (warn under 20% free, **fail under 10%**, i.e. 90% used), `host.unattended_upgrades`, `host.time_sync`, `ssh.public_closed`, `ssh.config`, `fw.active`, `fw.rules`, `net.allowlist_open` (`0.0.0.0/0` or `::/0` in `client_allowlist` or the metrics allowlist), `tls.expiry`, `tls.dns`, `tls.mode`, `secrets.confirmed`, `metrics.endpoint` (each exporter on loopback; nginx answers 401 without the credential).
- Operator-side, added by the CLI: `vaire.version_skew`, `ctx.file_modes` (the context files 0600), `metrics.auth` (an authenticated GET of `/metrics/node` over the tunnel to the node's hostname, verify-full).
- RPC `doctor.run {only, db}`; results go to `/var/lib/vaire/doctor.json` (root 0600, regenerable) and `vaire-doctor.prom` (`vaire_doctor_check{check,db,status}`, `vaire_doctor_last_run_timestamp_seconds`).
- CLI: `vaire doctor [--only security|backups|host|postgres|tls|metrics] [--db]`. Any `fail` ends with the new code `doctor_failed` (exit 5, added after `partial_failure`) and every result in `details.checks`; warnings alone exit 0. `--only security` is the CIS-style subset (`Security: true`), which MASTER calls `doctor --security`.
- `node run doctor` (hourly `vaire-doctor.timer`, D13).
- Files: `internal/doctor/*.go`, `internal/node/doctor.go`, `internal/cli/{doctor,tree}.go`, `internal/cli/output` (the code), `internal/guide/agents.md`.
- Tests: thresholds (disk, TLS days), the `--only` filter, the exit code mapping, `0.0.0.0/0` detection.
- Done when: `vaire doctor --only host` lists every host check with a fix command where one exists.

### 12. Doctor: PostgreSQL, budget and backup checks
- Per database: `pg.running`, `pg.config_drift` (rendered vs disk, and the settings `90-overrides.conf` overrides, listed), `pg.restart_pending`, `pg.alter_system_off`, `pg.hba_no_trust`, `pg.ssl`, `pg.listen_socket_only` (new: `listen_addresses = ''`), `pg.checksums`, `pg.wraparound` (warn > 1.0e9, fail > 1.6e9), `pg.connections` (warn > 80% of `max_connections`), `pg.memory_pressure` (warn: `memory.pressure` `some avg300` above 10%, or `memory.events high` rising over the last hour from `doctor.json`'s previous reading), `pg.would_not_start` (a stopped cluster that `checkStarts` would refuse).
- Server: `pg.memory_budget` (`memory_overcommitted`, `cpu_crowded`, `disk_crowded` as warnings, with `db resize` as the fix), `pooler.running` (both units and admin consoles), `nginx.running`, `jobs.skipped` (warn: a database's timer job skipped three runs in a row), `jobs.failed` (warn: the last run of a timer job failed).
- Backups per database: `backup.offsite`, `backup.archiving`, `backup.full_age` (against the database's full schedule: interval from two `systemd-analyze calendar --iterations=2` timestamps; warn > interval + 1 day, fail > 2×), `backup.verify` (warn: last failed or older than 14 days), `backup.drill` (`skip` until the first drill is due; warn: no pass in 40 days; fail: last result failed). A database younger than its first full schedule gets `skip`, not `warn`.
- Files: `internal/doctor/{pg,backup,budget}.go`, `internal/health` (shared readings).
- Tests: the full-age arithmetic for weekly, daily and custom schedules; drill and verify status from records; the pressure threshold; the override listing.
- Done when: `vaire doctor` on a fresh server with two databases is all `pass` or `skip`, and each of the forced conditions in the checklist flips its check.

### 13. Doctor as the gate of `server init` and `apply` (step 18)
- D10: the runner calls `doctor.Run` (node checks) after the steps; `ApplyData.doctor` and `server init`'s result carry the results; init fails with `doctor_failed` (exempting `secrets.confirmed` and `backup.*` pending), `apply` warns `doctor_check_failed` per failing check. `--skip-doctor` on both.
- `server init`'s summary card gets a *Health* row (passes, warnings, failures, the `vaire doctor` hint). IMPLEMENTATION's "Doctor results join with Phase 4" is done.
- Not a catalogue step: no journal entry, so a second `apply` still skips everything.
- Files: `internal/node/{apply,bootstrap}.go`, `internal/cli/{steps,serverinit,serverinit_db}.go`, `internal/cli/ui` (summary).
- Tests: which checks init exempts; the warning list from a result set.
- Done when: a second `apply` prints "applied 0" and a health line; init with a forced failing check stops before the handoff and `--resume` finishes after the fix.

### 14. `vaire queries`
- RPC `queries.top {db, limit, order}` reads `pg_stat_statements` (joined with `pg_roles`, query text truncated to 500 chars) in each database's cluster and merges them with a `db` column; `queries.reset {db}` calls `pg_stat_statements_reset()` in one or every cluster (`vaire_admin` needs `EXECUTE` on it: `postgres-roles-v1` grants it).
- CLI: `vaire queries [--db] [--limit 20] [--order total|mean|calls]`, `--reset --confirm <db>/stats | <server>/stats` (D12). The command's metadata gets the new token template.
- Error codes: `precondition_failed` reason `extension_missing` when a database lacks `pg_stat_statements` (hint: Phase 7's `vaire ext add pg_stat_statements --db <db>`; listed per database, the others still answer).
- Files: `internal/node/queries.go`, `internal/cli/{queries,tree}.go`, `internal/steps/postgres_roles.go`.
- Tests: ordering and merging across databases; token choice with and without `--db`.
- Done when: `vaire queries --db app` shows the statements the Phase 3 test ran.

### 15. `vaire top`
- A `bubbletea` TUI over the tunnel (D9): one connection per database through the session pooler as `vaire_admin`, read-only, `application_name = vaire top`, plus the session pooler's admin console. Panes: per database (connections by state, TPS, cache hit, longest transaction), active sessions (pid, db, user, state, wait event, duration, query), blocking chains (`pg_blocking_pids`), top statements by recent delta (`pg_stat_statements` sampled each refresh), PgBouncer pools of both poolers. Keys: `d` cycle database filter, `s` sort, `q` quit. `--db` starts filtered. Refresh 2 s.
- Structured output: `vaire top --json` (or no TTY) prints one snapshot and exits; `--once` does the same on a TTY.
- Files: `internal/cli/top/*.go`, `internal/cli/tree.go`.
- Tests: the statement delta between two samples (reset in between), the blocking-tree builder.
- To verify on the VPS: `vaire_admin` reading the session pooler's admin console through 5432 (the global `auth_dbname`); the connection count staying at one per database.
- Done when: `vaire top` shows a `pg_sleep` session in `app` and a lock wait between two sessions.

---

## D. Maintenance

### 16. `internal/maint` and the reports
- `internal/maint`: a session over the database's socket as `vaire_admin` (D8) with `lock_timeout`, `statement_timeout` and `READ ONLY` for reports; the SQL embedded as files.
- Reports: `wraparound` (per database `age(datfrozenxid)`, `mxid_age(datminmxid)`, the 20 oldest tables; warn/fail at doctor's thresholds), `unused-indexes`, `missing-indexes`, `long-queries --older-than`, `locks` (blocking tree), each as IMPLEMENTATION's maintenance table defines them. `--db` narrows; without it every database, with a `db` field.
- RPC `maint.report {kind, db, older_than, min_bytes}`, no lock.
- CLI: `vaire maint report <kind> [--db] …` with tables and `--json`.
- Files: `internal/maint/{session,report}.go`, `internal/maint/sql/*.sql`, `internal/node/maint.go`, `internal/cli/{maint,tree}.go`.
- Error codes: `usage_error` for an unknown kind (listing the kinds), `database_not_found`.
- Tests: SQL files embed and parse (`pg_query` isn't a dependency; a smoke check that each file is non-empty and has no `;` before its end), threshold mapping, output shaping.
- Done when: every report kind runs against both databases.

### 17. The bloat report (vendored ioguix queries)
- D3: `internal/maint/sql/table_bloat.sql`, `btree_bloat.sql`, `LICENSE.ioguix` and a header naming the upstream commit; `vaire version --licenses` (hidden) prints the notice; `scripts/build-release.sh` writes `THIRD_PARTY_NOTICES` into the tarballs.
- `maint report bloat [--db] [--min-bytes]` → `{db, schema, table, kind: table|index, real_size, est_bloat_bytes, ratio, fillfactor}` sorted by bytes, plus `unestimated` (tables without stats vaire_admin can read, or never analysed).
- Files: `internal/maint/{bloat.go,sql/*}`, `internal/cli/version.go`, `scripts/build-release.sh`.
- Tests: result shaping and `--min-bytes`; the notice is embedded.
- To verify on the VPS: the non-superuser queries returning rows for tables owned by `<db>_owner` as `vaire_admin` (inherited membership); a table bloated on purpose (`UPDATE` every row twice, no vacuum) showing a ratio near its real bloat.
- Done when: the report lists the bloated table at the top.

### 18. `maint vacuum`, `analyze` and `reindex` as jobs
- RPC `maint.run {kind, db, table, index, analyze, freeze, parallel, cleanup_invalid}` validates, then starts a detachable job `maint` (rpc-only, `node run maint --job-id`): Ctrl-C detaches, `vaire job follow` re-attaches, results in `job list`.
- Locks: global shared, the database's cluster lock shared; `vacuum` without `--table` and `reindex` of a database or table also take the heavy-I/O lock (timeout 0 → `precondition_failed` reason `lock_held` with `details.lock = heavy_io`), so they never run beside a full backup or a drill, as MASTER's maintenance section requires.
- `vacuum`: `VACUUM (ANALYZE, FREEZE, PARALLEL n) <table>`, or `vacuumdb --jobs=min(4, CPU share)` for the database. `analyze`: `ANALYZE` or `vacuumdb --analyze-only`. `reindex`: `REINDEX (CONCURRENTLY) TABLE|INDEX`; `--concurrently=false` is refused (`usage_error`: it blocks writes). A failed concurrent rebuild reports leftover `_ccnew` invalid indexes with the warning `reindex_left_invalid` and the hint `--cleanup-invalid`, which drops them (`DROP INDEX CONCURRENTLY`).
- Without `--db`, the CLI runs one job per database, one after another (the `eachDatabase` pattern from `backup now`).
- Dry run: the statements that would run, per database.
- Files: `internal/maint/run.go`, `internal/node/{maint,jobs}.go`, `internal/cli/{maint,tree}.go`.
- Tests: statement building (quoting, option combinations), lock selection per kind.
- To verify on the VPS: `vaire_admin`'s `pg_maintain` vacuuming and reindexing a table owned by `<db>_owner`; a vacuum refused with `lock_held` while a full backup runs.
- Done when: `maint vacuum --db app --analyze` runs as a job, `pg_stat_user_tables.n_dead_tup` for the bloated table drops to about zero, and `last_vacuum` is set. (The bloat estimate stays high: a plain `VACUUM` makes the space reusable but doesn't shrink the table; that's repack's job in Phase 7.)

### 19. `maint kill` and `maint repack`
- `maint kill --db <db> (--pid N | --idle-in-tx) [--older-than] --confirm <db>/kill`: `pg_terminate_backend` as `vaire_admin`. Never targets `postgres`, `vaire_*`, `pgbouncer_auth`, autovacuum, replication or `vaire top` backends unless `--pid` names one; a `--pid` on a superuser backend fails (`precondition_failed` reason `protected_backend`). Dry run lists the targets. RPC `maint.kill`.
- `maint repack --db <db> --table <t> --confirm <db>`: checks `pg_extension` for `pg_repack` and fails with `precondition_failed`, reason `extension_missing`, hint "pg_repack arrives with extensions in Phase 7: vaire ext add pg_repack --db <db>". The window check (`--now`) is kept in the flags for Phase 7. No `pg_repack` binary is installed.
- Files: `internal/maint/{kill,repack}.go`, `internal/node/maint.go`, `internal/cli/{maint,tree}.go` (token templates per D12).
- Tests: the target filter (role, state, age), the protected list, token templates.
- Done when: an idle-in-transaction session in `app` is terminated and `maint repack` refuses with the Phase 7 hint.

### 20. Maintenance timers, the window and deferred restarts
- `timers-v1` renders `vaire-maint-daily`, `vaire-maint-weekly`, `vaire-metrics-refresh` and `vaire-doctor` (D13) with the existing job templates; `ownedService` and the stale sweep learn them. They exist with or without backups.
- `maint-daily`: for each database in name order, stopping at the window's end: the wraparound report (to `vaire-maint.prom`: `vaire_wraparound_age{db}`, `vaire_mxid_age{db}`), then `ANALYZE` of tables with `n_mod_since_analyze > 0.1 × n_live_tup` under the cluster lock shared; then the deferred restarts (D11) under each cluster's lock exclusive.
- `maint-weekly`: the bloat report per database to `/var/lib/vaire/maint.json` (regenerable, root 0600) and `vaire-maint.prom` (`vaire_bloat_estimated_bytes{db,kind}`, `vaire_maint_last_run_timestamp_seconds{job}`). `auto_repack` stays inert with a log line until Phase 7.
- `db resize --defer-restart` writes `deferred_restarts`; `db resize` and `apply` with the restart token clear the entry; `db drop` removes it. `db resize`'s `next` names the window ("restarts at 02:00 in the maintenance window").
- `laterJobs` keeps only `vps-backup` (dropped in Phase 5); `jobNames` gains `doctor` and `maint`.
- Files: `internal/steps/timers.go`, `internal/node/{jobs,maint,db}.go`, `internal/maint/daily.go`, `internal/engine/state.go`.
- Tests: timer render at the window's start (window across midnight, unparseable window falls back as `StaggerSpread` does); the stale-table selection; the window-end cut-off.
- To verify on the VPS: `systemctl list-timers 'vaire-*'` shows the four new timers; a deferred resize restarts at the window start (move the window a few minutes ahead with `config set maintenance.window`); `metrics-refresh` skipping at once during an exclusive `apply`.
- Done when: a night's run leaves fresh `vaire-maint.prom` lines and the deferred restart is gone from `state.json`.

---

## E. Docs and the exit test

### 21. Docs and the agent guide
- MASTER: "Still to decide" loses the exporter and bloat items (pointing here); the decisions table gets the metrics endpoint and the exporter pins; the technology table drops `pgbackrest_exporter`; the metrics, maintenance and day-to-day sections; the files on a node (exporters, `vaire-metrics.htpasswd`, `doctor.json`, `maint.json`, the textfiles); Phase 4's row and module statuses; risks.
- IMPLEMENTATION: the command table (`metrics show|rotate`, tokens per D12, `--skip-doctor`); error codes (`doctor_failed`); exporters, nginx (PROXY protocol, metrics locations), the textfile metrics (every file and line); maintenance (node-side, jobs, locks); timers (the four new ones, one-minute refresh); status and doctor (new checks, `skip`, the gate); step 16, and step 18 as the gate; pinned versions with the sums; the reserved-connection decision in the pooler section; the secrets table (the hash made on the operator, the file's place).
- README: metrics endpoint and scraping with `targets.json`; status, doctor, top, queries, maint.
- The guide: `status` and `doctor` in the workflows, `doctor_failed`, `doctor_check_failed`, `metrics_reload_failed`, `reindex_left_invalid`, `extension_missing`, `protected_backend`, the new tokens; never print `metrics show --reveal`.
- MANUAL-TESTING: the checklist below as the Phase 4 section.
- Done when: no doc says `pgbackrest_exporter`, `finalize-v1` or `<server>/kill`.

### 22. Manual test (the exit test)
- Run the checklist below on the server the Phase 3 exit test leaves, then on a fresh `server init` for the init gate. Record date, vaire version and VPS next to Phase 4 in MASTER.

---

## Manual test checklist (to add to MANUAL-TESTING.md)

## Phase 4: Observability and maintenance

Run on a real VPS with two databases (`app`, `other`) and backups on R2, in order. Useful on the node: `systemctl status node_exporter postgres_exporter pgbouncer_exporter pgbouncer_exporter-session`, `curl -s 127.0.0.1:9100/metrics | grep ^vaire_`, `cat /etc/nginx/vaire-metrics.conf`, `systemctl list-timers 'vaire-*'`, `cat /var/lib/vaire/doctor.json`.

- [ ] **Exporters install and stay local**
    - `vaire apply` on the Phase 3 server. Expected: `metrics-v1` applies once, a second `apply` skips it; `ss -ltnp` shows 9100, 9187, 9127 and 9128 on 127.0.0.1 only; each binary is the pinned version under `/usr/local/lib/vaire/exporters/`.
    - Also check: the units run in `vaire-exporters.slice` with their `MemoryMax`; node_exporter's systemd collector lists `postgresql@18-app.service`; the textfile collector serves the `vaire_*` lines.
- [ ] **`/metrics` needs the credential**
    - `curl -i https://db1.<domain>/metrics/node` → 401. With `-u metrics:<password>` from the secrets handoff → 200. The same for `/metrics/postgres/app`, `/metrics/postgres/other`, `/metrics/pgbouncer`, `/metrics/pgbouncer-session` and `/metrics/targets.json`.
    - `/metrics/postgres/app?target=host=/tmp` still returns `app`'s metrics. `/probe` is 404.
    - Also check: the error log shows the client's real address (PROXY protocol); after ten wrong passwords fail2ban bans the address (`fail2ban-client status nginx-http-auth`), then unban it; with `[metrics] allowlist` set to another CIDR the right password gets 403.
    - Also check that 443 still does its other jobs: direct TLS to `db1.<domain>:443/app` connects, a client without `sslnegotiation=direct` is refused, and PgBouncer's `SHOW CLIENTS` shows 127.0.0.1 for it.
- [ ] **A scraper picks up databases by itself**
    - Point a Prometheus (or Grafana Alloy) at `targets.json` with the snippet from `vaire metrics show`. Expected: node, both poolers and both databases are up.
    - `vaire db create third --size nano`. Expected: within one SD refresh `third` is scraped; `vaire db drop third --confirm third` removes it.
- [ ] **postgres_exporter per cluster**
    - `pg_up 1` for `app` and `other`, `pg_stat_statements_calls_total` and `pg_stat_user_tables_*` for the application's tables, `pg_stat_archiver_last_archive_age` per database, `pg_database_wraparound_age_datfrozenxid_seconds`.
    - Reserved slots: after the upgrade's restart of `app`, `SHOW superuser_reserved_connections` is 0 and `reserved_connections` 3, and the pool sizes are unchanged from Phase 3. During a probe of `app`, `SELECT count(*) FROM pg_stat_activity WHERE usename = 'vaire_monitor'` peaks at 2, and the exporter's journal has no "too many connections for role". Fill both of `app`'s pools (a pgbench through 6432 with more clients than `pool_size` and one through 5432 with more than `session_pool_size`). Expected: no client gets "remaining connection slots are reserved"; the probe of `app` still answers and `vaire doctor` (as `vaire_admin`) still connects and warns `pg.connections`.
- [ ] **vaire's own lines**
    - `vaire_cluster_memory_current_bytes{db="app"}` and the PSI counters move during the Phase 3 throttle query; `vaire_cluster_memory_events_total{db="app",event="high"}` rises.
    - After `vaire backup now --db app`: `vaire_backup_last_size_bytes`, `vaire_backup_last_duration_seconds` and `vaire_backup_window_start_timestamp_seconds` for `app`.
    - `vaire_tls_cert_expiry_timestamp_seconds` matches `openssl x509 -enddate`; `vaire_doctor_check` lines exist after the hourly run.
- [ ] **`vaire status`**
    - Shows the server card (budget, poolers, disk, TLS) and a row per database with memory use and limit, connections, cache hit, TPS, last backup and window. `--db app` narrows; `--json` has every field; `--watch` refreshes.
    - Stop `other` (`pg_ctlcluster 18 other stop`): `status` still answers, `other` is `stopped`. Start it again.
    - During a long `vaire apply` in another terminal, `status` still answers.
- [ ] **`vaire doctor` is clean on a fresh server with two databases**
    - On a fresh `server init` with backups and two databases: every check `pass` or `skip`, exit 0; init's summary has a Health row.
    - Force each and see its check flip, then undo: a `trust` line in `pg_hba.conf` (`pg.hba_no_trust` fail, and `pg.config_drift`); a setting in `90-overrides.conf` (`pg.config_drift` lists it); public 22 opened with `vaire node unlock-ssh` (`ssh.public_closed`); `fallocate` the data root past 90% (`host.disk_data` fail); `chmod 644` a context file (`ctx.file_modes`); stop `postgres_exporter` (`metrics.endpoint`).
    - `vaire doctor --only security` shows only the security checks; a failing doctor exits 5 with `doctor_failed`.
    - Init gate: on a fresh `server init`, make a check fail before the database phase finishes (init has no databases yet, so use a host check: fill the data root past 90% with `fallocate`, or stop nginx); init stops with `doctor_failed`, and `--resume` finishes after the fix.
    - `vaire apply` with a failing check succeeds with `doctor_check_failed`; a second `apply` still applies 0 steps.
- [ ] **`vaire queries` and `vaire top`**
    - `vaire queries --db app` lists the statements of a pgbench run; without `--db` both databases with a `db` column. `--reset` without `--confirm` is refused with the token `app/stats` (or `<server>/stats`).
    - `vaire top` shows a `pg_sleep(60)` in `app`, a lock wait between two sessions as a blocking chain, and both poolers' pools; `vaire top --json` prints one snapshot.
- [ ] **Run a bloat report**
    - In `app`, create a table, insert 1M rows, `UPDATE` every row twice with autovacuum off for that table. `vaire maint report bloat --db app` lists it first with a ratio near the real one; `vaire maint vacuum --db app --table <t>` runs as a job (`vaire job follow`) and `n_dead_tup` drops to about zero; the bloat estimate stays high, since a plain vacuum doesn't shrink the table.
    - The other reports: `wraparound`, `unused-indexes`, `missing-indexes`, `long-queries --older-than 10s` (with a `pg_sleep`), `locks`.
- [ ] **Maintenance commands and refusals**
    - `maint reindex --db app --table <t>` rebuilds concurrently; `--concurrently=false` is refused.
    - `maint vacuum --db app` during a full backup of `app` is refused with `lock_held` (`heavy_io`).
    - An `idle in transaction` session in `app`: `maint kill --db app --idle-in-tx --confirm app/kill` terminates it; `--pid` of a `postgres` backend is refused (`protected_backend`).
    - `maint repack --db app --table <t> --confirm app` fails with `precondition_failed` (`extension_missing`) and the Phase 7 hint.
- [ ] **Timers and the window**
    - `systemctl list-timers 'vaire-*'` shows `vaire-maint-daily`, `vaire-maint-weekly`, `vaire-metrics-refresh` (every minute) and `vaire-doctor` (hourly).
    - `vaire db resize other --size medium --defer-restart`, then `vaire config set maintenance.window <now+5m>-<now+65m>` and `apply`. Expected: at the window start `maint-daily` restarts only `other` (PAUSE/RESUME on the transaction pooler), `state.json` loses the entry, and `ANALYZE` ran on stale tables (`last_analyze`). Set the window back.
- [ ] **The metrics credential through the handoff**
    - `vaire secrets export` has a Metrics group with base URL, user and password; `vaire login` on a second machine and `vaire metrics show --reveal` there print the same password.
    - `vaire metrics rotate --confirm <server>/metrics`: the old password gets 401, the new one 200, and the command warns `secrets_document_stale`.

---

## What Phase 5 (master backup) needs from Phase 4

v1 is Phases 0-5, so these are designed in now and noted for Phase 5's plan:

- **Doctor must know a rebuilt master.** `server init --restore` ends with the doctor gate (D10), but every database comes back `awaiting_recovery` until `db recover`. `pg.*` and `backup.*` checks must return `skip` for those databases, or the rebuild fails its own gate. `internal/doctor` gets a per-database state input from the start so Phase 5 only sets it.
- **`secrets.confirmed` after a rebuild.** It reads `state.json`'s `secrets_confirmed_at`, which is ephemeral and not in the bundle. Phase 5 must either set it when `--restore` is confirmed or exempt the check on a restored master.
- **A textfile per owner** (unit 2) lets Phase 5 add `vaire-master-backup.prom` (`vaire_master_backup_last_success_timestamp_seconds`, size, the epoch) without touching the others, and a `backup.master_bundle` check plugs into the doctor registry.
- **Nothing Phase 4 adds needs to be in the bundle.** The exporters and their units are rendered by `metrics-v1`; `doctor.json`, `maint.json` and the textfiles are regenerable; deferred restarts are ephemeral in `state.json`. `records.json` (in the bundle) only gains backup facts that the next backup refreshes anyway.
- **The metrics credential comes from the operator.** The htpasswd is not in the bundle; `server init --restore` must send the hash from `credentials.json` like the service secrets, so existing scrapers keep working with the same password. The secrets document already carries it (unit 3).
- **`db recover` runs `metrics-v1`** after each database, as `db create` does (unit 7), so recovered databases come back into `targets.json`.
- **RTO depends on GitHub.** A rebuild downloads the three exporters from GitHub releases. If that's unacceptable for DR, Phase 5 can skip `metrics-v1` until the databases are recovered, or the operator can push the pinned tarballs; the doctor's `metrics.endpoint` then warns rather than blocking the rebuild.
- **`status` gets a master-backup row** (age of the last bundle) in Phase 5; the server card from unit 10 has room for it.
- **Changes that trigger a bundle:** `metrics rotate` changes nothing in the bundle and needn't trigger one; `config set metrics.*` already does through `config set`.

---

## Risks

- **Exporter supply chain and freshness.** vaire pins upstream binaries from GitHub; a compromised release would be caught by the pinned sha256, but a security fix upstream reaches servers only with an vaire release. Re-check the three pins before each phase, as for pgBackRest.
- **postgres_exporter `/probe` has no timeout** (a `TODO` in 0.20.1's handler). A hung cluster holds a probe until the scraper gives up. Mitigated by `connect_timeout=5` and the role's `statement_timeout = 10s`; still to watch on the VPS.
- **postgres_exporter without a `DATA_SOURCE_NAME`.** Multi-target-only mode is documented but untested here; the fallback (a default DSN pointing at the first cluster) is in unit 5.
- **Series cardinality.** `stat_user_tables` and `stat_statements` grow with tables and statements; a schema with thousands of tables makes each probe large. Grafana Cloud bills per series. If it bites, a `[metrics] table_metrics = false` switch is a small follow-up.
- **Three shared reserved slots.** With both pools full, `vaire_admin`, `vaire_monitor` and `postgres` share three slots and there is no superuser-only slot: a probe (two) plus an `vaire_admin` session fill them. `vaire_monitor` may hold one probe: two scrapers probing the same database at once, or a probe overlapping a slow one, get "too many connections for role".
- **PROXY protocol on 443's internal hops** touches the path every direct-TLS client takes. A mistake breaks PostgreSQL on 443 and the metrics together; the checklist re-runs the 443 checks from Phase 2.
- **bcrypt in nginx** depends on the system `crypt()`. Checked statically: Debian 13's libxcrypt 4.4.38 is built with the default `--enable-hashes=all`, and Ubuntu 26.04's libxcrypt 4.5.1 `crypt()` verifies Go's `$2a$` hashes. The VPS test still confirms nginx's 200 with the password.
- **Doctor as a gate** can block `server init` on a check that's flaky in the first minutes (DNS propagation for `tls.dns`, chrony not yet synced). `tls.dns` and `host.time_sync` must be `warn` during init, and `--skip-doctor` exists for anything else.
- **Maintenance window contention.** `maint-daily` (ANALYZE, deferred restarts) shares the window with the staggered backups. ANALYZE is light and doesn't take the heavy-I/O lock; a deferred restart waits for its cluster's lock, so a long full backup can push it to the next night (doctor reports it).
- **The bloat estimate is an estimate.** Off for tables with stale statistics, wide variable-length columns or TOAST; the report says so and lists what it couldn't estimate. Exact numbers wait for Phase 7.
- **`metrics-refresh` every minute** spawns `vaire node run` 1,440 times a day; each run is short, but it adds journal lines. Its unit logs only failures.
- **The metrics endpoint is public on 443.** Security rests on a 190-bit password, `limit_req`, fail2ban and the optional allowlist; the exporters themselves stay on loopback and `/probe` is never reachable from outside.
