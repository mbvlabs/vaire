# Phase 3 work units: databases as clusters

Source: `docs/RFC-cluster-per-database.md` (approved, revision 4). This splits its order of work (the roadmap's eight branches) into small units we take one at a time. Each unit is one stacked branch off the previous one.

**Rules for every unit**

- Ends with `just lint` and a build for all targets. No local test harnesses.
- Compiles and keeps the CLI coherent, but **doesn't have to be deployable** between units. The `mbvlabs` server is torn down and rebuilt (no conversion code), and the exit test runs once at the end (unit 33).
- No compatibility shims for the old `[[databases]]` / `18/main` layout.
- "To verify" items from the RFC's risks are noted on the unit they belong to, so the manual test knows where to look.

---

## A. Config and naming

### 1. `cluster init` → `server init`
- Rename the command, its interview and help text, `--data-directory` → `--data-root`. `commands`, `guide agents` and skill text follow.
- Files: `internal/cli/clusterinit*.go`, `tree.go`, `internal/guide`.
- Done when: `vaire server init --help` is the only init; no `cluster init` left in the tree or guide.
- **Status: finished.** Notes:
  - `[server] data_root` (default `/var/lib/postgresql/18`) came forward from unit 3, because `--data-root` needs a field to set. `data_directory` is gone; `Server.DataDirectory()` returns `<data_root>/main` for the existing callers until unit 9 makes it per cluster. Unit 3 only adds `memory_reserve` to `[server]`.
  - The files are now `internal/cli/serverinit*.go`; the interview asks "Server name" and "Data root".
  - Left for unit 2 (contexts name servers): the `{cluster}` confirm token, `ClusterName`, the `cluster` JSON fields in init's output and errors, and `cluster.toml`'s top-level `name`.
  - Not renamed: the internal RPC methods `cluster.bootstrap` and `cluster.confirm-secrets`. Nobody sees them; rename them with unit 2 if we want the names to line up.
  - `scripts/install.sh`, the `tls-v1` migration message and README's `cluster init` lines now say `server init`. MASTER, IMPLEMENTATION and MANUAL-TESTING still say `cluster init` and wait for unit 33.

### 2. Contexts, `node.toml` and the secrets document name servers
- `context.toml`: `cluster` → `server`; `nodes` becomes a cache (name, hostname, WireGuard address) plus an empty database placement map.
- `/etc/vaire/node.toml`: `server`, `hostname`, `role = "master"`.
- `domain` is the setup's base; the master's hostname is `db1.<domain>` by default (`server init` asks for the node name, default `db1`).
- Secrets document: `cluster` → `server`, drop `stanza`.
- Files: `internal/contexts`, `internal/node/config.go`, `internal/secretsdoc`, `internal/cli/login.go`, `dns.go`.
- **Status: finished.** Notes:
  - `context.toml` has `server`, a `[databases]` placement map (database → node), and per node `hostname` beside the WireGuard fields. The CLI fills the map from `cluster.toml` after every `config set`, `config edit` and `apply` (`syncContext`, formerly `syncContextTLS`); `context_tls_updated` fires only when a hostname or the TLS mode changed.
  - **Deviation:** the node cache keeps `public_address`, `wg_public_key` and `listen_port`, because the tunnel still dials the public IP. Dialing the hostname waits for Phase 5.
  - The hostname isn't stored in `cluster.toml`; `Cluster.Hostname(node)` derives `<node>.<domain>` (empty without a domain). `node.toml` stores it and is rewritten whenever `cluster.toml` is saved (`syncNodeHostname`). The error reason is now `node_belongs_to_other_server`.
  - `server init --node-name` (default `db1`) and the interview's "Node name"; the domain question is now "Base domain". `nodes[].role` is `master`; validation wants exactly one master until workers arrive in Phase 10.
  - Init's JSON (`plan`, result, discard, `init_pending` details) says `server`; the `{cluster}` confirm token is `{server}`.
  - Secrets document: `server` (name, domain, TLS mode), nodes carry `hostname`, and `stanza` is gone, from the recovery kit's `pgbackrest` too.
  - **Pulled forward from unit 17:** the certificate, nginx `server_name`, ACME, the DNS check, the CLI's TLS `ServerName`, `PGHOST`, the tunnel hint and the database URLs all use `MasterHostname()` (`db1.<domain>`). `tls.sh` reads `TLS_HOSTNAME` (bash owns `HOSTNAME`). Unit 17 is left with per-node routing once there is more than one node.


### 3. `[[clusters]]` schema, sizes and validation
- `[[databases]]` → `[[clusters]]` with `name, node, port, stanza, size, memory, memory_max, cpu_weight, cpu_quota, io_weight, profile, statement_timeout, extensions, pool_size, session_pool_size`, `[clusters.settings]`, `[clusters.backup]`.
- `[server] data_root, memory_reserve`; `[postgres] default_size, session_client_connections`; `[master_backup]` (decoded and validated only, used in Phase 5).
- The size table (nano … xlarge, custom) and "explicit values win" resolution as a pure function in `internal/config`.
- Validation: `node` must be the master; `pool_size + session_pool_size + 3 ≤ max_connections`; unique ports in 5433–5479.
- Callers of `cfg.Databases` move to `cfg.Clusters` mechanically (render, cli/db, steps/timers, roles, node/db, node/backup, backup/pgbackrest).
- **Status: finished.** Notes:
  - Schema in `internal/config/clusters.go`, the size table and `ResolveResources` in `sizes.go`. `data_root` already came with unit 1.
  - A custom size needs `memory`; its `max_connections` comes from the largest named size whose memory fits (nano below 256 MiB), and the CPU weight defaults to 100. `settings.max_connections` overrides either. `pool_size = 0` means `max_connections − session_pool_size − 3`.
  - Memory values accept KiB/MiB/GiB/TiB and PostgreSQL's kB/MB/GB/TB (binary).
  - **Pulled forward from unit 9/20:** `db create` places the new cluster itself (`placeCluster`: the master, the lowest free port from `FreeClusterPort`, `NewStanza` = `<name>-<6 hex>`, the default size). A fork copies the source's size. Everything else about running it as its own cluster is still unit 20.
  - `server init --default-size` and the interview's "Default database size" write `[postgres] default_size`.
  - `[master_backup]` (retention days 30, weeks 52, local keep 10) is only decoded and validated.


### 4. `config set|get` for clusters
- Dotted keys `clusters.<name>.*` including `settings.*` and `backup.*`.
- Immutable: `clusters.*.port`, `.stanza`, `.name`, `.follows`.
- Files: `internal/config/keys.go`, `internal/cli/config.go`.
- **Status: finished.** Notes:
  - The generic key walk already reaches `clusters.<name>.*`, `settings.*` and `backup.*`; only the immutable list and its hint were new (`ImmutableHint()` is shared with the node's `config.set`).
  - `follows` is reserved as immutable, but the field doesn't exist until Phase 6.


### 5. `records.json` split from `state.json`
- Durable records (restores per database, drills, last results, later timeline switches and dropped stanzas) move to `/var/lib/vaire/records.json`; `state.json` keeps jobs and lockdown flags only.
- Files: `internal/state`, `internal/backup/state.go`, `internal/node/jobs.go`.
- **Status: finished.** Notes:
  - `internal/backup` owns `/var/lib/vaire/records.json` (`Records`, `LoadRecords`, `UpdateRecords`), mode 0600 with its own `records.json.lock`. A corrupt file is the precondition `records_corrupt`.
  - `engine.WithFileLock` is the shared flock helper; `UpdateState` uses it too. `state.json` lost `backup` and keeps jobs and lockdown flags.
  - `internal/state` is the desired-state store and needed no change; `internal/node/jobs.go` already wrote only job records.


### 6. Recovery identity at `server init`, bucket optional
- `server init` creates the age recovery identity (moved out of `backup setup`, which reuses it).
- `no_offsite_backup` warning with the fix hint at the end of `server init`, on every `apply`, and in `status`.
- Files: `internal/cli/clusterinit_backup.go`, `backup.go`, `handoff.go`.
- **Status: finished.** Notes:
  - `ensureRecoveryIdentity` creates the age identity in the recovery kit; `server init` calls it before the bucket question is acted on, so every server's handoff has it. `backup setup` (`prepareKit`) keeps the identity it finds, including across a new bucket, which only gets a new cipher.
  - A kit holding only the identity is not a bucket: `BackupCreds.HasBucket()` decides whether `apply` sends the bucket secrets and whether "backups on, no kit" is refused.
  - `no_offsite_backup` (`node.NoOffsiteWarning`) ends `server init` and comes back from the node on every `apply` and dry run, so every client sees it. The message carries the fix, `vaire backup setup`; warnings have no hint field.
  - `vaire status` is still `notImplemented`; it should add the same warning when it lands.
  - The init summary's Backups row reads "none off the server: run vaire backup setup". `guide agents` lists the warning.


---

## B. Per-cluster steps

### 7. Step instances in the engine
- A step can be instantiated per cluster: journaled as `<id>@<cluster>` with its own input hash; the plan lists instances.
- `plan|apply --db <name>` limits to one cluster's instances (plus nothing server-level).
- Repeatable `--confirm <cluster>/restart`; restart classification is per instance; `<server>/restart` restarts clusters one at a time.
- Files: `internal/engine/runner.go`, journal, `internal/steps/catalog.go`, `restarter.go`, `internal/cli/steps.go`.
- **Status: finished.** Notes:
  - `engine.Template` (`Instance(cluster)`) and `Expand` turn a template into one step per `[[clusters]]` entry; `InstanceID`/`SplitID` handle `<id>@<cluster>`. The plan and the journal list instances; `steps.clusterStep` is the shared base.
  - `Options.DB` keeps only that cluster's instances. The journal (`update`, `setEntry`, `clearPending`) now merges under a file lock, since a `--db` apply holds only the global lock shared and two of them may write it at once.
  - `Restarter.Restart(ctx, cluster)` runs `pg_ctlcluster 18 <c> restart`; `<server>/restart` restarts the pending clusters one by one. `ApplyData.Restarts` reports each cluster and its downtime.
  - RPC requests carry `confirms []string` (`ConfirmTokens()` merges it with the old single `confirm`); `PlanData`/`ApplyData` return `confirm_tokens`, and the CLI's next hint lists one `--confirm` per token. `apply` and `tune apply` take a repeatable `--confirm`.
  - Ambiguity: a database named like the server makes `<name>/restart` the server token, which restarts every pending cluster. Validation could forbid that name.
  - The restarter does not pause PgBouncer; consider `PAUSE`/`RESUME <db>` around a restart in unit 15.


### 8. Per-cluster locks
- `/run/lock/vaire-<cluster>.lock` for cluster instances and per-database jobs; the global lock stays for server-level steps.
- Files: `internal/node/dispatch.go`, `jobs.go`.
- **Status: finished.** Notes:
  - `engine.ClusterLockPath(c)` is `/run/lock/vaire-<c>.lock` with its own `.info` holder file; `lock_held` names the cluster. Order is always the global lock first, then clusters by name.
  - Methods declare `clusterLock(req)`. `apply --db` (not dry run), `db credentials --rotate` and restore promote/undo/abort/cleanup take the global lock shared and the cluster exclusive; an unknown cluster is refused.
  - Jobs: `db-dump`, `restore` and `db-fork` (source and new name) take their clusters shared with no wait; a busy lock records `skipped lock_busy`. `restore start` takes the cluster shared too.
  - Backup jobs take no cluster lock yet; they become per stanza in unit 24.


### 9. Layout constants become functions of the cluster
- `PGCluster, PGConfDir, PGConfD, PGServiceName, PGServiceDropInDir`, the log path in `internal/node/logs.go`, `DefaultDataDirectory` → `<data_root>/<cluster>`.
- Port allocator: 5433–5479 for clusters (assigned at create, recorded in config), 5480–5499 for temporary clusters (replaces `ScratchPort`).
- `logs --db` reads `postgresql@18-<cluster>`.
- **Status: finished.** Notes:
  - `steps/pgcommon.go`: `PGConfDir(c)`, `PGConfD(c)`, `PGServiceName(c)`, `PGServiceDropInDir(c)`, `PGServiceDropIn(c)`, `PGLogPath(c)`, `PGSocketDir`, `ClusterPG(spec)`; `Server.DataDirectory(c)` is `<data_root>/<c>`. `hostinfo.Detect` measures `data_root`.
  - Cluster ports were already assigned at create and recorded in config (unit 3). Temporary clusters reserve a port in 5480–5499 under `<root>/ports.lock` (`backup.reserveTemporaryPort`), skipping ports in `pg_lsclusters`, live socket lock files and other scratch dirs' `port` files; the reservation is the scratch dir's `port` file, gone with the dir. `no_temporary_port` when all are taken. `ScratchPort` is gone.
  - `logs postgres --db <name>` reads `PGLogPath(<name>)`; without `--db` it works only with one database, else `input_required` listing them.


### 10. `postgres-cluster-v1@<cluster>`, socket only
- `pg_createcluster 18 <c> -p <port> --datadir <data_root>/<c>`; `listen_addresses = ''`.
- `archive_mode = on` with `archive_command = '/bin/true'` from creation (so turning backups on later is a reload; see unit 22).
- `node dump` and every vaire-internal connection use the cluster's socket.
- **Status: finished.** Notes:
  - `postgres-cluster-v1@<c>` refuses an existing cluster with another data directory or port (`cluster_mismatch`) and never re-creates. `ClusterExists` is exported for `db create`.
  - `20-vaire-security.conf` sets `listen_addresses = ''`; `40-vaire-backup.conf` always has `archive_mode = on`, pushing to `spec.Stanza` when backups are on and `/bin/true` otherwise.
  - `engine.Runtime.PG` is removed so nothing can fall back to the default socket. `node/clusters.go` (`clusterOf`, `clusterPG`, `onlyCluster`) resolves a database to its socket; db list/create/drop/credentials, `sql`, `tune`, certificate reloads (every cluster), dumps, copy, load, restore and the drill all use it. `db list` reads every configured cluster and shows a stopped one as not existing.
  - `db list` no longer lists live databases missing from `cluster.toml`; orphan detection belongs to unit 21.
  - Interim until unit 23: pgBackRest covers only the first cluster (`Cluster.BackupCluster()`): `pgbackrest.conf` (with `pg1-port`), the step's driver, `node/backup.go`'s driver, the drill, restore and fork use its stanza and socket. The `pgbackrest-v1` barrier stays as is until then.
  - Interim until units 15/16: `pgbouncer-v1` skips with no databases and sizes from the smallest pool; its wildcard entry still points at 5432, where nothing listens now.


### 11. Memory budget in `internal/tune`
- Server reserve `max(512MiB, 25% RAM)` or `memory_reserve`; cluster budget is the rest.
- Each cluster tunes from its `memory` as `RAMMiB`; `max_worker_processes`/`max_parallel_workers` capped at `vCPU / clusters` (min 2); `max_connections` from size; `huge_pages = try` only from 2 GiB.
- `postgres-config-v1@<cluster>` renders into the cluster's `conf.d` from that result; `[clusters.settings]` overrides.
- **Status: finished.** Notes:
  - `tune`: `ServerReserve`, `ClusterBudget`, `CPUShare`, `MaxConnections`, `HugePagesMinMiB`; `Input.Clusters`. `steps.tuningFor` feeds each cluster its `memory`, its CPU share, and a disk share proportional to memory.
  - `postgres-config-v1@<c>` renders `ExtensionsConf`/`SettingsConf` into `PGConfD(c)`; spec settings override the tune result.
  - `tune show|explain --db` show that cluster plus the server sheet (RAM, reserve, cluster budget, assigned memory, CPU share, huge pages); that sheet is the start of unit 19. `tune apply` sends `db` and takes a repeatable `--confirm`.


### 12. `kernel-tuning-v1`: huge pages as a sum
- `vm.nr_hugepages` = Σ per-cluster estimate for clusters with `huge_pages = try`.
- **Status: finished.** Notes:
  - `steps.hugePagesFor` sums each cluster's `tune.Result.HugePages` (zero below 2 GiB); `tune show` reports the same sum.


### 13. cgroup limits per cluster
- Drop-in per `postgresql@18-<c>.service`: `MemoryHigh` (memory + 25%), `CPUWeight`, `IOWeight`, opt-in `MemoryMax`/`CPUQuota`, `OOMScoreAdjust=-900`.
- Changing a cgroup key is `daemon-reload` + `set-property`, no restart.
- To verify on the VPS: `MemoryHigh` throttles before global OOM; shared memory charged to the cluster; whether `IOWeight` is enforced (report "not enforced" if not, in unit 19's `tune show`).
- **Status: finished** (not verified on a server). Notes:
  - `postgres-cluster-v1@<c>` owns `PGServiceDropIn(c)` from the `PGServiceDropin` template; the drop-in is part of its inputs. The script applies limits with `daemon-reload` and `systemctl set-property --runtime`, no restart.
  - `db drop` removes the drop-in directory and reloads systemd.
  - Still to check on the VPS: `MemoryHigh` throttles before global OOM, shared memory is charged to the cluster, and whether `IOWeight` is enforced.


### 14. `postgres-roles-v1@<cluster>`
- In every cluster: `vaire_admin`, `vaire_monitor`, `pgbouncer_auth`, `vaire_direct_only`, the `vaire` schema with **both** auth functions (`pgbouncer_get_auth`, `pgbouncer_get_auth_session`), and that one database's four roles.
- Service passwords setup-wide (same verifier in every cluster). `pg_hba`/`pg_ident` from one template, socket only.
- **Status: finished.** Notes:
  - `postgres-roles-v1@<c>` creates the service roles, the `vaire` schema with both auth functions, and that database's roles in each cluster; `pg_hba` is socket only.
  - Verifiers come from the service secrets (`serviceVerifier`), so every cluster gets the same one. The CLI's `db create` and `db fork` therefore send the service secrets too.
  - Pulled forward from unit 20: `db create` runs `apply` with `db` set, so the new cluster's instances run (not kernel huge pages or poolers yet). If it fails after the cluster exists, `cluster.toml` is kept and the hint is `vaire apply --db <name>`.
  - Pulled forward from unit 21: `db drop` runs `pg_dropcluster 18 <name> --stop` and removes the drop-in. Pooler entries, timers and stanza records stay with unit 21.


---

## C. Routing

### 15. Transaction pooler routes by database name
- `[databases]` entries per database plus the `<db>__auth` alias with `auth_dbname`; the `*` wildcard goes; `max_db_connections` from `pool_size`.
- Adding or removing a database is a `RELOAD`.
- To verify on the VPS: per-entry `auth_dbname` pointing at another `[databases]` alias. Fallback (auth functions in each application database) only if it fails.
- **Status: finished** (not verified on a server). Notes:
  - `render.PgbouncerIni` takes `[]render.PoolerDatabase` (name, port, `max_db_connections`); `steps.poolerDatabases(cfg, session)` builds it from `[[clusters]]` with `pool_size` (or `session_pool_size` for unit 16). Each database gets `<db> = … auth_dbname=<db>__auth max_db_connections=<pool>` and `<db>__auth = … dbname=postgres pool_size=2`. The wildcard is gone.
  - The global `auth_dbname` stays, pointing at the first database's alias (by name): only the admin console uses it, for `vaire_admin`, which has no entry of its own. Service passwords are the same in every cluster, so any alias works.
  - `pgbouncer-v1` reloads PgBouncer when it runs and starts it otherwise (`pgbouncer.sh`).
  - **Pulled forward from units 20/21:** `db create` (and so `db fork`) and `db drop` run `pgbouncer-v1` (`steps.PoolerStepIDs`, `node.reloadPoolers`) after the cluster changes, so the entries follow with a reload.
  - Unit 7's suggestion done: the restarter sends `PAUSE <db>` to the transaction pooler before `pg_ctlcluster restart` and `RESUME <db>` after (15 s timeout; a failed PAUSE doesn't stop the restart). It logs in as `pgbouncer` over the socket as the postgres OS user (`steps.poolerAdmin`). The session pooler isn't paused: in session mode PAUSE waits for every client to leave.
  - Left for unit 21: dropping the **last** database leaves its entries, because `pgbouncer-v1` skips with no databases (the userlist's verifiers come from a cluster). Clients get a connection error rather than "no such database".
  - A client can name `<db>__auth`; only userlist users (`pgbouncer_auth`, `vaire_monitor`) and the admin console's `vaire_admin` can log in to it, and it is that cluster's `postgres` database, which they reach anyway.
  - Open question: `ReservedConnections = 3`, but tune sets `superuser_reserved_connections = 3` **and** `reserved_connections = 3`, so ordinary roles get `max_connections − 6`. With the default `pool_size = max − session − 3`, both pools full at once would hit "remaining connection slots are reserved" three connections early. Either raise `ReservedConnections` to 6 or drop `reserved_connections`.
  - To verify on the VPS: per-entry `auth_dbname` pointing at another alias; admin console login as `pgbouncer` from the postgres OS user; `PAUSE`/`RESUME` around a restart; the global `auth_dbname` letting `vaire_admin` into the admin console.

### 16. Session pooler
- `vaire-pgbouncer-session.service`, `/etc/pgbouncer/session.ini`, own pidfile, socket dir and admin console; `pool_mode = session` on 5432 with `client_tls_sslmode = require` and the node certificate; `session_pool_size` per database.
- `firewall-v1`: 5432 on `wg0` and the allowlist as today (now the pooler).
- To verify: two PgBouncers side by side on Debian 13 and Ubuntu 26.04.
- **Status: finished** (not verified on a server). Notes:
  - One template, `pgbouncer.ini.tmpl`, renders both: `render.PgbouncerIni` (transaction, 6432, `/var/run/postgresql`, `pgbouncer_get_auth`, `client_tls_sslmode = prefer`) and `render.SessionIni` (session, 5432, `vaire.pgbouncer_get_auth_session`, `client_tls_sslmode = require`, the node certificate, `max_client_conn` from `session_client_connections`, `application_name_add_host = 1`, no `max_prepared_statements`/reserve pool/empty reset query). Each session entry's `max_db_connections` is its `session_pool_size`; `default_pool_size` is the largest of them.
  - `vaire-pgbouncer-session.service` (vaire's own unit, `Type=notify`, `User=postgres`, `RuntimeDirectory=vaire-pgbouncer-session`) runs `/usr/sbin/pgbouncer /etc/pgbouncer/session.ini`. Its socket (and so its admin console) and `pidfile` live in `/run/vaire-pgbouncer-session`; its hba is `/etc/pgbouncer/session_hba.conf` (socket, or TLS over TCP; no loopback exception). The userlist is shared.
  - `pgbouncer-v1` owns both poolers' files and the unit, checks both services and both admin consoles, and `pgbouncer.sh` reloads or starts each.
  - `firewall-v1` already opened 5432 on `wg0` and to the allowlist; only its comment and constants (`PgbouncerPort`, `SessionPoolerPort`) changed.
  - `vaire logs pgbouncer-session` reads the new unit. `db create` help, `tunnel` help, `guide agents` and the connection notes say the migrate URL goes through the session pooler.
  - `vaire.pgbouncer_get_auth_session` refused `vaire_admin`; unit 18 lets it in.
  - Left for unit 17: `certs-renew` reloads only the transaction pooler.
  - To verify on the VPS: both PgBouncers side by side (PGDG build with systemd notify, the second unit's runtime dir, pidfile and admin console); `client_tls_sslmode = require` not applying to the unix socket (the step's check of the session admin console relies on it).

### 17. One hostname per node
- `tls-v1`: one SAN, `<node>.<domain>`. `certs-renew` reloads both poolers, nginx, then each cluster.
- URLs printed as `<node>.<domain>:443/<db>` (app) and `:5432/<db>` (migrate) in `db create` and `db credentials`.
- **Status: finished.** Notes:
  - Unit 2 had already made the certificate a single SAN. What's new is that the node side names **this** node rather than the master: `steps.NodeHostname(cfg)` (node.toml's node in `[[nodes]]`, falling back to the master) feeds `tls-v1`'s `TLS_HOSTNAME`, nginx's `server_name` (`render.NginxConf` takes the hostname) and `certs-renew`, including the ACME order and the DNS probe.
  - `certs-renew` reloads `pgbouncer`, `vaire-pgbouncer-session` and nginx, then each cluster.
  - `config.DatabaseHostname(db)` is the hostname of the node the database's cluster runs on, or the master's for a database not created yet (where `db create` places it). `db create`, `db fork` and `db credentials` build their URLs from it.
  - **Deviation, needs a decision:** the URL set stays `app` (`:6432/<db>`), `app_direct_tls` (`:443/<db>?sslnegotiation=direct`) and `migrate` (`:5432/<db>`). The unit says the app URL is `:443/<db>`, but 443 only works with direct TLS negotiation (PostgreSQL 17+ libpq, pgx 5.7+), so making it the only app URL would break older clients. Swap them if 443 should be the primary.
  - The CLI's own connection (the context, `PGHOST`, `MasterHostname()` in the CLI, `dns.go`) still targets the master; that's correct until workers exist.

### 18. Operator connections through the session pooler
- `sql`, `psql`, `console`, `tunnel --target postgres` go to 5432 on the node and `--db` routes; `--target pgbouncer` stays 6432.
- Placement lookup goes through the context's cache (only the master exists now, but the lookup is in place for Phase 10).
- **Status: finished** (not verified on a server). Notes:
  - `contexts.DatabaseNode(db)` reads the placement cache; the CLI's `databaseEndpoint` uses it for the WireGuard address and the TLS `ServerName`/`PGHOST`. A cache miss refreshes the cache from the master once (`syncContextFromNode`); a database still unknown (a restore copy, a typo) goes to the master, whose pooler refuses it. `nodeWGAddr` is gone.
  - `sql`, `psql` and `console` keep `pgPort = 5432`, now the session pooler. `tunnel` gained `--db` (picks the node and fills the DSN's path); `--target postgres` is the session pooler, `--target pgbouncer` the transaction pooler on 6432.
  - `vaire.pgbouncer_get_auth_session` now lets `vaire_admin` in (the RFC's pooler table said it refuses every `vaire_*` role, but operator logins need it); it still refuses `postgres`, `pgbouncer_auth` and `vaire_monitor`. `postgres-roles-v1` notices the old body and replaces it.
  - The session pooler no longer ignores `options`: `psql` and `console` send `--default_transaction_read_only=on` or `--role=<db>_owner` there, and ignoring it would have silently dropped read-only. It tracks `IntervalStyle, search_path, default_transaction_read_only, role` instead, so anything else in `options` is refused. `vaire sql` sets both with `SET` after connecting and isn't affected.
  - To verify on the VPS: PgBouncer parsing the `--name=value` form inside `options` and applying `default_transaction_read_only` and `role` from it (if not, `psql`/`console` fail to connect, which is safe; switch them to `-c name=value` or a `PSQLRC` with the `SET`s); `vaire_admin` logging in through 5432; `tunnel --db` reaching the master.

---

## D. Database lifecycle as clusters

### 19. Oversubscription arithmetic and `tune show`
- Pure functions: budget sheet, `memory_overcommitted`, `cpu_crowded`, `disk_crowded`, `would_not_start` (against `CommitLimit` and the huge-page pool), smallest size that starts.
- `tune show|explain` per cluster (`--db`) plus the server sheet; IOWeight "not enforced" detection.
- Warnings surface in `plan` and `status`.
- **Status: finished** (not verified on a server). Notes:
  - Pure arithmetic in `internal/tune/budget.go`: `Demand` (one cluster's claim: size, memory, CPU weight and quota, start-up shared memory, huge pages, `max_wal_size`), `Host`, `NewSheet`, `Sheet.Warnings` (`memory_overcommitted`, `cpu_crowded`, `disk_crowded`), `WouldNotStart`, `SmallestThatStarts`, `StepDown` (used by unit 20) and `FormatMiB`. Shared memory at start-up is the huge-page estimate in MiB (`shared_buffers` × 1.05 + 128 MiB).
  - `would_not_start`: shared memory larger than `CommitLimit − Committed_AS` (plus what a restart frees), unless the cluster's huge pages are already free in the pool; with `settings.huge_pages = on`, the pool's free pages plus what `MemAvailable` could add must cover it. Unreadable `/proc/meminfo` refuses nothing.
  - `disk_crowded` uses the data root's filesystem usage (statfs) plus every `max_wal_size` plus the 4 GiB archive spool, not per-cluster data sizes: cheaper, and already counts what's on the disk. The doctor failure at 90% is Phase 4.
  - `internal/hostinfo/memory.go`: `ReadMemory`/`ParseMeminfo` (CommitLimit, Committed_AS, MemAvailable, huge pages), `IOWeight(device)` (enforced with the `bfq` scheduler or `io.cost.qos` `enable=1` for the disk, else "not enforced: …"), `UsedGiB`.
  - `internal/node/budget.go` builds demands from `[[clusters]]` (honouring `settings.shared_buffers`, `huge_pages` and `max_wal_size`), `oversubscriptionWarnings`, `checkStarts`, and reads `memory.current` from `system-postgresql.slice/postgresql@18-<c>.service`.
  - `tune show` prints the RFC's sheet (each cluster's size, memory, CPU/IO weight, connections, opt-in caps, current use, running or stopped; allocated, free or overcommitted; commit limit and huge pages; IOWeight enforcement) and its JSON `server` gained `clusters`, `free_mib`, `commit_limit_mib`, `committed_mib`, `huge_pages_free`, `io_weight_enforced`, `io_weight`, `disk_gib`, `disk_used_gib`. `tune explain` starts with the same sheet.
  - The warnings come back from `plan`, `apply` (and its dry run), `config set`, `tune show|explain` and `tune apply`. `status` is still `notImplemented`; it should add them when it lands. `guide agents` lists the three codes and `would_not_start`.
  - To verify on the VPS: the `memory.current` cgroup path; `IOWeight` detection on the virtio disk (expected "not enforced"); `CommitLimit` arithmetic against a real refusal.

### 20. `db create` creates a cluster
- Allocates port and stanza id, picks the size (`--size`, `--memory`, `--profile`; default size steps down to what fits), runs the cluster's instances, reloads both poolers, prints the URLs and warnings. Refuses `would_not_start`.
- `db list`: node, URL host, size, limits, stanza, state.
- **Status: finished** (not verified on a server). Notes:
  - `db create --size`, `--memory`, `--profile` (checked on the CLI before passwords are generated, and again by the node's validation). Port, stanza and node were already placed by unit 3.
  - Without `--size` and `--memory`, `default_size` steps down to the largest size up to it that fits the free budget, with the warning `size_stepped_down` ("created as `small`; `medium` would overcommit this server by 300 MiB"). When even `nano` doesn't fit, the default stays and `memory_overcommitted` says so.
  - `would_not_start` is checked (`checkStarts`) before `cluster.toml` is saved; the hint and `details.smallest_size` name the smallest size that would start. `db fork` shares it, and now copies the source's `memory` and `profile` as well as its size.
  - `createDatabase` runs `kernel-tuning-v1` (`steps.KernelStepIDs`) before the cluster's instances, so the huge-page pool grows before a 2 GiB+ cluster starts; then the instances, then `pgbouncer-v1` (both poolers). The result carries the step-down and the oversubscription warnings; the message adds the size ("created database app (small, 512 MiB)") and `data.cluster` the placed spec. The URLs are printed as before (unit 17).
  - `db list` now shows node, URL host, size, memory, limits (CPU/IO weight, connections, opt-in quota and `memory_max`), stanza and state: `running`, `stopped`, `pending` (configured, no cluster) or `orphaned` (a `pg_lsclusters` 18/<name> not in `cluster.toml`, with the `orphaned_database` warning). `steps.ClusterNames` lists the clusters.
  - Left as is: `db create --dry-run` is still answered on the CLI and doesn't preview the step-down or `would_not_start`.

### 21. `db drop` drops a cluster
- Final dump (as today), stop, `pg_dropcluster`, remove pooler entries and timers; **keep the stanza** and record it as dropped in `records.json` (expiry in unit 25).
- From unit 15: `db drop` already runs `pgbouncer-v1` afterwards, but dropping the **last** database leaves its pooler entries, because the step skips with no databases.
- **Status: finished** (not verified on a server). Notes:
  - The final dump, `pg_dropcluster 18 <name> --stop` and the drop-in removal were already in place (units 13/14). After `cluster.toml` loses the entry, `dropRouting` reloads the poolers and runs `timers-v1`, which removes the database's `vaire-db-dump@<db>` timer as a stale unit.
  - The stanza is kept: `backup.Records.DroppedStanzas` (database, stanza, `dropped_at`, final dump id) in `records.json`; `data.stanza_kept` and the CLI message say so. A failed record write is the warning `records_not_updated`, not a failure, because the cluster is already gone. Expiry is unit 25.
  - The last database: `pgbouncer-v1` still skips with no databases, so `dropRouting` runs `systemctl disable --now pgbouncer vaire-pgbouncer-session` instead (skipped when the session unit was never installed). Clients get "connection refused" rather than a stale route; `pgbouncer.sh` enables and starts both on the next `db create`.
  - An orphaned cluster (unit 20's `db list`) can be dropped with `db drop`; it has no stanza to keep.
  - To verify on the VPS: the dump timer disappearing after a drop; both poolers coming back on the next `db create` after the last one was dropped.

### 22. `db resize`
- `vaire db resize <db> --size … | --memory …` and `config set clusters.<db>.size`. cgroup and reload-only settings live; `shared_buffers`, `max_connections`, `huge_pages` need `<db>/restart` or `--defer-restart`. Shrinking below current use is a warning.
- **Status: finished** (not verified on a server). Notes:
  - `vaire db resize <db> --size … | --memory … [--defer-restart] [--confirm <db>/restart] [--dry-run]` calls the node's `db.resize` (`handleDBResize`): the global lock exclusive and the cluster's lock exclusive (shared and none for a dry run), with `restartTokens` as the allowed confirm tokens.
  - `--size` replaces the size and drops an old `memory` override unless `--memory` is given too; `--memory` alone keeps the size. The CLI checks both with `checkSizeFlags` before the call; the node validates the whole file again.
  - Refused before anything is saved: `would_not_start`, counting the running cluster's current shared memory as freed by the restart. Warned: `shrink_below_use` when the cgroup's `memory.current` is above the new memory, plus the oversubscription warnings.
  - It saves `cluster.toml`, runs `kernel-tuning-v1` (the huge-page sum), then the cluster's instances with `Restarter` and the request's restart confirmation. The drop-in changes with `set-property` and reload-only settings with a reload; `shared_buffers`, `max_connections` and `huge_pages` restart that cluster only with `--confirm <db>/restart`. Without the token, or with `--defer-restart` (which ignores one), the restart stays pending, and `next` is `vaire apply --db <db> --confirm <db>/restart`. A dry run returns the plan for the resized file without saving it.
  - **Deviation, needs a decision:** the RFC says a deferred restart "waits for the next maintenance window". vaire has no maintenance-window runner yet (Phase 4's `maint`), so `--defer-restart` only leaves the restart pending for a later `apply`. Wire it to the window when that exists.
  - `config set clusters.<db>.size` already worked (unit 4) and now carries the oversubscription warnings, but it doesn't check `would_not_start` and doesn't apply anything: the next `apply` does. `db resize` is the checked path.
  - `db resize` is back in the `memory_overcommitted` message and in the guide's `memory_overcommitted`, `size_stepped_down` and `would_not_start` entries. The guide also has `shrink_below_use` and a resize line in the "Add a database" workflow, and `db` help lists resize.
  - To verify on the VPS: a live resize (MemoryHigh and CPU weight changing with no restart); a `large` → `small` restart touching only that cluster; the huge-page pool growing before a resize to 2 GiB+.

---

## E. Backups per stanza

### 23. Multi-stanza `pgbackrest.conf` and reload-only setup
- One `[global]` repo and cipher, one section per stanza (`pg1-path`, `pg1-port`, socket, per-stanza `repo1-retention-full`).
- Enabling backups switches `archive_command` to `archive-push` with a **reload**; `backup setup` no longer restarts. Then `stanza-create`, `check` and a first full per cluster; `db create` does the same when backups are on.
- `driver.go`/`pgbackrest.go` gain `--stanza` and `--pg1-port`.
- **Status: finished** (not verified on a server). Notes:
  - `render.PgBackRestConf` renders one `[global]` (repository, cipher, `repo1-retention-full-type=time`, the local repo, process-max, spool) and a section per `[[clusters]]` entry: `pg1-path`, `pg1-port`, `pg1-socket-path`, `repo1-retention-full` from `config.Cluster.RetentionFull(spec)` (the cluster's `backup.retention_full`, else `[backup.database]`'s, in weeks × 7) and `archive-push-queue-max`.
  - **Pulled forward from the RFC's disk rules:** `archive-push-queue-max` is per stanza, `tune.ArchiveSpoolGiB` (4 GiB) split over the clusters with a 512 MiB floor (`render.ArchiveQueueMaxMiB`). Past eight clusters the floors add up to more than the 4 GiB `disk_crowded` counts.
  - `config.Cluster.BackupCluster()` is gone. The `pgbackrest-v1` barrier is gone too: `engine.RestartBarrier`, its runner block and `ReasonRestartPending` / `step_waits_for_restart` are removed, since nothing else implemented it. `archive_mode = on` is there from creation (unit 10), so `postgres-config-v1@<c>` switches `archive_command` with a reload.
  - `pgbackrest-v1` skips with `no_databases` when there are no clusters. Check reads every stanza with one `pgbackrest info` (`PgBackRest.InfoAll`) and wants a full in each. Apply writes the file, then per cluster without a full: `stanza-create`, `check`, a first full (`setupStanza`). Clusters that already have a full are skipped unless the request rotates the keys, so `db create` only touches the new stanza.
  - `backup.PgBackRest` gains `Port` and passes `--pg1-port` to `stanza-create`, `check` and `backup` (the commands that connect to the cluster; not to `info`, `verify` or `restore`). An empty `Stanza` leaves out `--stanza`, which `InfoAll` uses. `steps.BackupDriver(spec, …)` builds the driver for a cluster; the node's `driver` takes a spec.
  - `backup setup` no longer asks for `<server>/restart`: no confirm, no restart warning, no "restarted for WAL archiving" line, `ConfirmToken`/`NeedsRestart` dropped from its metadata, and `server init`'s backups phase passes no token. Help text and the guide's `no_offsite_backup` entry say setup restarts nothing.
  - `db create` (and so `db fork`) runs `pgbackrest-v1` after the poolers when backups are on (`firstBackup`, `steps.PgBackRestStepIDs`). A failure is the warning `first_backup_failed` (in the guide and `db create`'s help), not an error, because the database already works and the passwords are only printed on success.
  - Minimal changes outside the unit: `restore` and `db fork` now use the restored or forked database's stanza and port (`clusterOf` in `checkRestoreTarget` and `loadFromScratch`); `backup list --db` reads that database's stanza; `checkRepoMove` refuses a move when any stanza has backups. `backup now`, `verify`, `backup list` without `--db` and the drill still cover the first cluster through `node.firstCluster` (units 24, 25, 32). `RecordBackup` still records one `LastBackup` per type, not per database (unit 25).
  - `db drop` doesn't re-render `pgbackrest.conf`, so a dropped stanza's section stays until the next `apply` re-renders the file without it (see unit 25).
  - To verify on the VPS: `backup setup` on a server with two databases reloads both (no restart), creates two stanzas and takes two fulls; `db create` with backups on adds a section and a first full for the new database only; `pgbackrest check` with `--pg1-port` on a non-5432 cluster; `pgbackrest info` without `--stanza` lists every stanza.

### 24. Template timers, staggering and the heavy-I/O lock
- `timers-v1` renders `vaire-backup-{full,diff,incr,verify}@<cluster>`, `vaire-restore-drill@<cluster>`, `vaire-db-dump@<db>`; schedules from the cluster, defaulting to `[backup.database]`; fixed offset from the stanza id.
- `node run` backup jobs take `--db` and a server-wide heavy-I/O lock (timeout 0, `skipped` when busy). Jobs carry `db`.
- From unit 23: the backup jobs (`jobBackup`, `jobVerify`, the drill) still use `node.firstCluster`; give them the job's `--db` and build the driver with `driver(spec, …)`.
- **Status: finished** (not verified on a server). Notes:
  - `timers-v1` (`steps/timers.go`) renders, with backups on, a template service per job (`vaire-<job>@.service` running `vaire node run <job> --db %i`) and a timer per cluster and job (`vaire-backup-{full,diff,incr,verify}@<c>.timer`, `vaire-restore-drill@<c>.timer`, `vaire-db-dump@<c>.timer`). `scheduledJobs(cfg, spec)` takes the cluster's `[clusters.backup]` schedule, else `[backup.database]`'s; dumps have no default. Stale timers and the owned template services (`ownedService`) are removed as before.
  - Staggering: `StaggerOffset(stanza, spread)` is an FNV hash of the stanza id modulo the spread in whole minutes; `StaggerSpread` is the length of `[maintenance] window` (3 hours by default, also when the window doesn't parse). `Stagger` rewrites the OnCalendar time (`HH:MM[:SS]` or `*:MM[:SS]`) later by the offset, wrapping within what is left of the day or hour so the date and weekday fields never move. A schedule without a fixed minute (`daily`, `*:0/15`, lists) keeps its time. A cluster's full, diff, verify, drill and dump share its offset. `RandomizedDelaySec` jitter stays on top.
  - **Deviation, needs a decision:** systemd has no offset for `OnCalendar`, so the time is rewritten instead of adding a delay; `FixedRandomDelay=` would be simpler but hashes the machine id and unit name, not the stanza id. Offsets are hashed, not slotted, so two clusters can land a few minutes apart: with the timeout-0 heavy-I/O lock the second job is **skipped**, not queued, and `Persistent=` doesn't retry it. Consider evenly spaced slots by stanza order, or a wait on the heavy-I/O lock for timer runs.
  - `node run`: `databaseJobs` (backup-full|diff|incr, backup-verify, restore-drill, db-dump) need a database, from `--db` or the RPC's `params.database` (`jobDatabase`); missing is a usage error, unknown is `database_not_found`. They take that cluster's lock shared (timeout 0) after the global lock. `heavyIOJobs` (all of them except incr) first take the server's heavy-I/O lock, `engine.AcquireHeavyIO` on `/run/lock/vaire-heavy-io.lock`, exclusive, timeout 0: busy records `skipped` with `lock_busy` and `lock: heavy_io` (`lock_held` with `details.lock` from the RPC). Order: global, heavy-I/O, clusters by name.
  - Jobs carry `db`: `engine.JobRecord.DB` (`job list` has a DB column), and backup results include it. `jobBackup`, `jobVerify` and `jobDrill` build the driver from the job's cluster (`jobEnv.spec()`); `verify` takes a spec.
  - Minimal changes outside the unit: `backup now`, `backup verify` and `restore drill` RPCs still cover the first cluster, now by passing `database: firstCluster(...).Name` in the job params (units 25 and 32). `db create` runs `timers-v1` after `pgbackrest-v1` (`firstBackup`), so a new database gets its timers; `first_backup_failed` mentions the timers. Help (`node run`, `db create`) and the guide's `lock_held` and `first_backup_failed` entries are updated.
  - Incrementals don't take the heavy-I/O lock (small, frequent, and a skipped one is caught up by the next); pgBackRest's own lock still keeps two backups of one stanza apart.
  - To verify on the VPS: `systemctl list-timers 'vaire-*'` shows one set per database with different times; an instance timer file (`vaire-backup-full@app.timer`) starting the template service with `--db app`; a second full skipped with `lock: heavy_io` while one runs; `db drop` removing all of the database's timers; `db create` adding them.

### 25. Backup commands per database and dropped-stanza expiry
- `backup now|verify --db` (default every cluster, one at a time), per-stanza `verify`, `backup list` per database including dropped stanzas still in their window. `metrics.go` gets a `db` label.
- Expiry timer runs `stanza-delete --force` once a dropped stanza's window has passed.
- From unit 24: the backup jobs already run per database (`--db` or `params.database`); `handleBackupNow`, `handleBackupVerify` and `handleRestoreDrill` pass the first cluster's name. `backup now`'s help still says it "covers every database: PostgreSQL has one WAL stream". A `--db`-less `backup now` that loops over clusters should start one job per cluster (each takes the heavy-I/O lock, so they must run one after another, not side by side). Metrics and `RecordBackup` have no db yet.
- From unit 23: `pgbackrest.conf` only has sections for `[[clusters]]`, so a dropped stanza has none once `pgbackrest-v1` re-renders. Decide whether dropped stanzas keep a section (with their `repo1-retention-full`) for `expire` and `stanza-delete`, or whether those run with the options on the command line. `PgBackRest.InfoAll` lists every stanza in one call for `backup list`. `handleBackupNow`, `verify` and `backup list` without `--db` still use `node.firstCluster`; `RecordBackup` keeps one `LastBackup` per type.
- **Status: finished** (not verified on a server). Notes:
  - `backup now [--db] [--type]` and `backup verify [--db]`: without `--db` the CLI lists the databases (`db.list`, the ones in `cluster.toml`) and calls the RPC once per database, one after another (`eachDatabase` in `cli/backup.go`), so each job takes the heavy-I/O lock in turn. A failed or skipped database doesn't stop the others; the command then fails with the first failure's code and `details.failed` plus every outcome (`details.backups` / `details.verifies`). Ctrl-C detaches from the running job and starts no more (the message names the ones not started). Results are `data.backups` / `data.verifies`, one entry per database.
  - The node's `backup.now` and `backup.verify` need `params.database` (usage error otherwise); `backup.now`'s dry run without one plans every cluster (`data.backups`: database and type). Without `--type`, one `pgbackrest info` over every stanza (`InfoAll`) picks incr, or full for a stanza without one.
  - Verify is per stanza: the driver always passed `--stanza`, so `Verify` only needed its comment; the result is recorded per database.
  - `records.json`: `last_backups` (database, then type) and `last_verifies` (by database) replace `last_backup` and `last_verify` (old keys are dropped on the next write, no migration). `Drill` gains `database` and `LastDrill(db)` (pulled forward from unit 32). `DroppedStanza` gains `expires_at`: `db drop` sets it to the drop time plus the cluster's `retention_full` weeks, and removes the database's last backup and verify results so its metrics stop.
  - `backup list` without `--db` is one row per database (stanza, status, window, complete sets, latest set, last verify, drill and dump), then a row per dropped stanza still recorded (dropped and expiry times, window). One `InfoAll` plus each live cluster's `pg_stat_archiver`; an unreadable one is the warning `archive_status_unavailable`. `--db` names a live database or a dropped one still in its window (`data.dropped`); the per-set `info --set` calls that filtered sets by database are gone, since a stanza holds one database's cluster. A dropped stanza's window ends at its newest set (no live archiver to read). `backup setup`'s summary prints each database's latest backup and window.
  - `metrics.go`: `vaire_backup_last_success_timestamp_seconds{db,type}`, `vaire_backup_verify_last_success_timestamp_seconds{db}`, `vaire_restore_drill_last_run_timestamp_seconds{db}`, `vaire_restore_drill_last_result{db,result}`, `vaire_job_last_skipped_timestamp_seconds{job,db}`. `RecordBackup(cfg, db, set)`.
  - Expiry: `timers-v1` renders, with backups on, `vaire-stanza-expiry.timer` (daily 06:00, after the default window, 30 min jitter) and `vaire-stanza-expiry.service` (`vaire node run stanza-expiry`). `jobStanzaExpiry` takes the expired records (`Records.Expired`), skips one whose database has an open restore, and for a stanza the repository holds runs `pgbackrest stop`, `stanza-delete --force --pg1-path=<old data dir>`, `start` (`PgBackRest.StanzaDelete`, also on the `Driver` interface), then drops the record. A stanza not in the repository only loses its record; a failed delete keeps it for the next run and fails the job. It takes the global lock only, not the heavy-I/O lock.
  - Decision on the question from unit 23: dropped stanzas get **no** `pgbackrest.conf` section. `stanza-delete` takes `--pg1-path` on the command line, and `expire` isn't needed: a dropped stanza gets no new backups, so nothing expires in it before the whole stanza goes. `db drop` still doesn't re-render `pgbackrest.conf`; the leftover section is harmless (nothing uses it) and goes with the next `apply`.
  - The drill RPC takes an optional `params.database` (default still the first cluster through `firstCluster`, now its only user); `restore drill` has no `--db` yet (unit 32).
  - Help (`backup now|verify|list`, `node run`) and the guide (backup workflow, `no_backups`, `archive_status_unavailable`) are updated.
  - To verify on the VPS: `vaire backup now` with two databases runs two jobs in turn; `backup verify --db app` runs `pgbackrest --stanza=… verify`; `backup list` rows and a dropped database's row; `pgbackrest stop` / `stanza-delete --force` accepting a stanza with no conf section and `--pg1-path` on the command line (pgBackRest requires `pg1-path` for stanza-delete), e.g. by setting a record's `expires_at` in the past and running `sudo vaire node run stanza-expiry`; the vaire.prom lines with `db`.

---

## F. Materialise and native restore

### 26. Materialise primitive
- `scratch.go` becomes "materialise a temporary cluster": `pgbackrest restore --stanza … --type=time --target-action=pause` or `pg_basebackup` from live; registered as `18/<name>` on a temporary port, archiving off, `hot_standby = on`, small buffers, hot-standby floors copied from the source.
- Teardown with `pg_dropcluster`.
- **Status: finished** (not verified on a server). Notes:
  - `backup.Materialised` in `internal/backup/materialise.go`: `NewMaterialised(dataRoot, name, source, sourcePort, …)` refuses an existing `/etc/postgresql/18/<name>` or `<data_root>/<name>` (`cluster_exists`) and reserves a temporary port. `Restore(ctx, driver, opts)` restores the source's stanza into `<data_root>/<name>`, registers the cluster and starts it; `BaseBackup(ctx)` copies the live source with `pg_basebackup -X stream -c fast` over its socket instead (a writable copy that starts as its own primary). `Drop()` / `DropTemporaryCluster(sh, step, report, name, data)` run `pg_dropcluster 18 <name> --stop`, or remove the directories when the cluster was never registered, and free the port reservation, on a context of their own.
  - Registration writes `/etc/postgresql/18/<name>/` itself instead of `pg_createcluster` on an existing data directory (a pgBackRest restore of a Debian cluster has no `postgresql.conf` in the data directory): a fresh `postgresql.conf` (data directory, hba/ident files, pid file, temporary port, socket directory, `cluster_name`, `include_dir`), `start.conf = manual` (never started at boot), the source's `pg_hba.conf`, `pg_ident.conf`, `pg_ctl.conf`, `environment` and every `conf.d/*.conf`, plus `conf.d/99-materialise.conf` (`MaterialiseOverrides`): port, `listen_addresses = ''`, `archive_mode = off`, `hot_standby = on`, `shared_buffers = 128MB`, small `effective_cache_size` / `maintenance_work_mem`, `huge_pages = off`. Copying the source's conf.d is how the hot-standby floors are copied: `max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions` and `max_locks_per_transaction` are the source's own values. Started and stopped with `pg_ctlcluster 18 <name> start|stop` (no cgroup drop-in, so no limits); the log is postgresql-common's `/var/log/postgresql/postgresql-18-<name>.log` (`LogTail`).
  - The driver gains `Materialise(ctx, RestoreOptions)` (`PgBackRest.Materialise`); `RestoreOptions` gains `Pause` and `Timeline` (`--target-timeline`, for unit 29). With `Pause`: time → `--type=time --target-action=pause`, set → `--type=immediate --target-action=pause`, latest → `--type=standby` (keeps following the archive, promoted by `pg_promote()` at cut-over); without it the same targets with `--target-action=promote`, latest `--type=default` (drills, forks). **It never passes `--archive-mode=off`:** pgBackRest writes that into `postgresql.auto.conf`, which travels with the data directory when unit 28 swaps it in, and would leave the live cluster not archiving. `archive_mode = off` lives in the temporary cluster's conf.d instead, which stays behind.
  - Readiness: `pg_isready`, then for a paused target `pg_get_wal_replay_pause_state() = 'paused'` (or already out of recovery), for promote `NOT pg_is_in_recovery()`; a postmaster that dies (a target outside the window ends recovery with a FATAL) returns the log tail. `RecoveredTo` reads `pg_last_xact_replay_timestamp()`.
  - Port reservations moved to `/run/vaire-ports/<owner>` (`backup.TemporaryPortDir`, one file per temporary cluster or scratch instance, under `ports.lock`), shared by `Materialised` and the old `Scratch`, which still exists for its current users (restore, fork, drill) until units 27, 31 and 32 move them.
  - Pulled forward: `pg_hba.conf` gains `local replication postgres peer`, which `pg_basebackup` from a live cluster needs (`wal_level` is already logical and `max_wal_senders` 10).
  - To verify on the VPS: a `pgbackrest restore --type=time --target-action=pause` into `<data_root>/app__restoring` registered this way shows in `pg_lsclusters`, starts with `pg_ctlcluster`, accepts `SELECT` while paused, and `pg_dropcluster --stop` removes it; `--type=standby` for latest; `pg_basebackup` over the socket with peer replication; that `pg_lsclusters` reads the port from the generated `postgresql.conf`.

### 27. `restore <db>` to ready, `status`, `abort`
- Preflight (window, space for **one** copy, no open restore), materialise into `<db>__restoring`, temporary pooler entry so `vaire sql --db <db>__restoring` works, `restore status` with `compareRows`. `abort` drops the copy.
- From unit 18: `vaire sql --db <db>__restoring` goes to the master (not in the placement cache) and needs both a `[databases]` entry and its `__auth` alias in the session pooler; `poolerDatabases` only lists `[[clusters]]`.
- Delete `swap.go`, `CopyDatabase`, `createLoadTarget`, `finishLoad`, `loadFromScratch`, `--db-include`, `restore_load_errors` and the reserved-database-name rules.
- From unit 25: restoring a dropped database must remove (or re-key) its `DroppedStanzas` record once the cluster exists again, or `vaire-stanza-expiry` deletes the stanza when the window passes. The expiry job already skips a stanza whose database has an open restore. `backup list --db <dropped>` shows the kept stanza's sets and window (window end = newest set, conservative).
- From unit 26: `scratch.go`'s `Scratch` is still used by `loadFromScratch` (restore, fork) and `jobDrill`. Once restore moves to `Materialised`, only fork and the drill use it.
- **Status: finished** (not verified on a server). Notes:
  - Units 27-30 landed together in one working tree, so each unit's notes say which parts are its own; the code is mostly in `internal/node/restore.go` and `internal/backup/materialise.go`.
  - `jobRestore` → `materialiseRestore` builds `backup.NewMaterialised(data_root, <db>__restoring, <db>, port, …)` and calls `Restore` with `Pause: true`, the target from `restoreParams` and the restore record's `Timeline` (unit 29). A failed materialise drops the copy at once (`m.Drop()`), so a failed restore holds no disk; `clearFailedRestore` and `abort` still drop it (`dropRestoring` → `backup.DropTemporaryCluster`), for a job that was killed.
  - `backup.Restore` gains `Port` (set when ready) and `Timeline`. Readiness: `RecoveredTo` is logged; the record goes `ready` with the port.
  - Preflight: backups on, `<db>` in `cluster.toml`, the target in the window, no open restore, `restoringInTheWay` (refuses with `restoring_exists` and `details.cluster` when `18/<db>__restoring` or its data directory exists, and says when it is a database of that name), and `restoreSpace(…, false)`: one copy of the database's size on the data directory's filesystem. The scratch line stays only for `db fork --at` until unit 31 (`scratch` flag).
  - Pooler entry: `steps.poolerDatabases(cfg, true)` adds, for every ready restore in `records.json` with a port, `<db>__restoring = host=/var/run/postgresql port=<copy> dbname=<db> auth_dbname=<db>__restoring__auth` and its `__auth` alias (`restoreEntries`, max 5 connections). `render.PoolerDatabase` gains `DBName`; the template writes `dbname=` when it is set; `adminAuthDB` skips temporary entries. Only the session pooler gets them. `jobRestore` runs `pgbouncer-v1` when the copy is ready (`applyPoolers`; a failure is the result's `pooler_warning`, the CLI's warning `pooler_reload_failed`), `abort` and `promote` run it again without the entry.
  - `restore status`: `compareRows(ctx, live, restoring pg.Local, db)` reads the same database name from the live cluster and from the copy's port. The `on_promote` text describes the cut-over.
  - Deleted: `swap.go` (its `RestoringName` / `PreRestoreName` are now cluster names in `materialise.go`; the timestamp has seconds, `20060102T150405`), `MaxRestorableName` and `name_too_long`, `createLoadTarget`, `RestoreOptions.Database` and `--db-include`, `restore_load_errors` (pg_restore errors of a dump restore are now `restore_dump_errors`), `config.IsRestoreName`, `RestoringSuffix`, `PreRestoreMarker`, the reserved-name rule in `CheckDatabaseName`, `restoreNameRefusal`, the exclusions in `diffList` and `orphanedWarnings`, and `databaseExists`.
  - Name collisions are existence checks: `db create` and `db fork` already refuse a name whose cluster exists (`createDatabase`'s `steps.ClusterExists`). `db list` skips clusters a restore owns (`Records.IsTemporary`: an open restore's `<db>__restoring` or a recorded leftover), so a real database called `app__restoring` lists normally.
  - From unit 25, decided here: `restore <db>` needs the database in `cluster.toml`, so a dropped database is never restored in place and its `DroppedStanzas` record is never re-keyed by a restore. Its stanza is reached with `backup list --db <dropped>` or, later, a fork.
  - Help (`restore`, `--promote`, `promote`, `undo`, `abort`, `cleanup`) and the guide (the restore paragraph, `after_cutover_failed`, `restore_dump_errors`, `pooler_reload_failed`) are updated.
  - To verify on the VPS: `vaire sql --db app__restoring` reaches the paused copy through the session pooler as `vaire_admin` (auth query on the copy's `postgres`, read-only); `restore status` row estimates from both clusters; `abort` removes the cluster, its data directory and the pooler entry; a `restore <db> --latest` copy keeps following the archive as a standby while it waits.
  - Design notes kept for later units:
    - **Fork until unit 31:** `jobFork` still uses `CopyDatabase`, `loadFromScratch`, `finishLoad` and the old `Scratch`. Moving fork onto `Materialised` means `Restore` with `Pause: false` for `--at`, `BaseBackup` without.
  - **Decided (may be revisited):**
    - **Name collisions are existence checks, not name rules.** `db create` and `db fork` refuse a name whose `18/<name>` cluster exists (`steps.ClusterExists`). `restore <db>`'s preflight refuses when `18/<db>__restoring` exists, with a message that says which cluster is in the way. `db list` tells temporary clusters from orphans by the open restore records (`<db>__restoring` and the leftover named in `Leftover`), not by a name pattern, so a real database called `app__restoring` lists normally. The database-name rules (`IsRestoreName`, `RestoringSuffix`, `PreRestoreMarker`, `restoreNameRefusal`, the `diffList` exclusions) all go.
    - **Fork stays on the old path until unit 31.** Unit 27 deletes only what restore alone uses: `swap.go`, `createLoadTarget`, `--db-include` and `restore_load_errors`. `CopyDatabase`, `loadFromScratch`, `finishLoad` and the old `Scratch` stay for `jobFork` (and `Scratch` for the drill) and move to unit 31's deletion list.

### 28. Promote and cleanup
- Stop live, stop copy, rename data directories (live → `<db>__pre_restore_<ts>`), start `18/<db>`, `pg_promote()`, archiving on to the same stanza, start a full backup job. `cleanup` drops the leftover cluster.
- **Status: finished** (not verified on a server). Notes:
  - `promote` (`restore promote`, and `restore <db> --promote` at the end of the job): `steps.PauseDatabase` (PAUSE on the transaction pooler, RESUME after), `backup.Swap` (`systemctl stop postgresql@18-<db>`, stop the copy, `rename(<data_root>/<db>, <data_root>/<db>__pre_restore_<ts>)`, `rename(<data_root>/<db>__restoring, <data_root>/<db>)`, then re-register the old data as the stopped cluster `<db>__pre_restore_<ts>` on the copy's temporary port, with the overrides file and `start.conf = manual`, and remove `/etc/postgresql/18/<db>__restoring`), `backup.StartCluster` (`systemctl start postgresql@18-<db>`: its own configuration, cgroup and `archive_command`; then wait until paused, or open for latest and dumps), `backup.Promote` (`SELECT pg_promote() WHERE pg_is_in_recovery()`, then `ALTER SYSTEM RESET` of `restore_command` and `recovery_target*`, and a reload).
  - `repointAutoConf` rewrites the copy's data path to the live one in the moved `postgresql.auto.conf`: pgBackRest's `restore_command` carries `--pg1-path` when it was passed on the command line, and archive-get reads `pg_control` there.
  - Records: the restore closes, the database's `Timelines` entry is cleared (a promote makes the newest timeline the right one again), and a `Leftover` with `Port` and `Stanza` (the stanza the old data belongs to) is added. Then `pgbouncer-v1` drops the `__restoring` entry, and `startFullBackup` runs `systemctl start --no-block vaire-backup-full@<db>.service`. A failure after the cut-over is the warning `after_cutover_failed`, not an error.
  - Locks: `promote` and `undo` take the node lock shared and the database's cluster lock **exclusive** (`lockForCutover`). `restore --promote` promotes inside the job, which holds the cluster lock shared, as before.
  - `cleanup` lists leftovers with `exists` (`steps.ClusterExists`) and the data directory's apparent size (`dirSize`), and drops them with `backup.DropTemporaryCluster` (`pg_dropcluster 18 <name> --stop`, then the directories).
  - Pulled forward: `db drop` drops the database's leftover clusters and their records (`dropLeftovers`), since they are no longer inside the dropped cluster.
  - To verify on the VPS: the whole cut-over on a `--time` restore: the copy pauses at the target, `restore promote` brings `18/<db>` up paused at the same target, `pg_promote()` opens it on a new timeline, the history file and new WAL reach the same stanza, and the full backup starts; `systemctl start postgresql@18-<db>` returning on a cluster that has to replay to its target (Debian's `postgresql@.service` start timeout); whether pgBackRest's `restore_command` includes `--pg1-path` (the rewrite) and that archive-get works after the rename; `pg_lsclusters` showing the leftover down on its temporary port; `cleanup` removing it.

### 29. Undo with timeline recording
- Swap back with the leftover; record the timeline switch in `records.json`; later restores pass `--target-timeline` explicitly. Full backup after undo.
- To verify on the VPS: PITR after an undo lands on the right branch.
- From unit 30 (decided): undo after a `restore --dump` brings back a cluster whose system identifier matches the **old** stanza. Undo must put that stanza back in `cluster.toml` and remove its `DroppedStanzas` record; the dump-restored cluster's new stanza then becomes the dropped one (kept for its window, like any other).
- **Status: finished** (not verified on a server). Notes:
  - `handleRestoreUndo`: under `lockForCutover`, it reads the restored cluster's timeline (`backup.Timeline`: the first 8 hex digits of `pg_walfile_name(pg_current_wal_lsn())`, in decimal), then the same `backup.Swap` with the newest leftover as `from` (the restored data becomes `<db>__pre_restore_<ts>` on the leftover's port) and `StartCluster` until open. It reads the timeline the old cluster resumed on and records `records.json`'s `timelines[<db>] = {stanza, timeline, switched_at, abandoned}` (`backup.TimelineSwitch`), then starts a full backup.
  - Later restores: `restore.start` puts `Records.TargetTimeline(db, stanza)` (only when the record's stanza is the database's current one) into the restore record and passes it as `RestoreOptions.Timeline`, so `pgbackrest restore` gets `--target-timeline=<n>` instead of `latest`. A promote clears the record; `db drop` too. The dry run shows the timeline.
  - From unit 30 (decided), done: the leftover carries the stanza its data belongs to. When it differs from the database's current stanza (undo of a restore from a dump), undo puts it back in `cluster.toml` (`switchStanza`), removes its `DroppedStanzas` record and records the dump's stanza as dropped (`droppedStanza`, its normal expiry), then `applyStanza`: the database's own steps (its `archive_command` names the stanza, a reload) and `pgbackrest-v1` and `timers-v1`. The timeline is recorded against the old stanza.
  - Undo of an undo works the same way: the newest leftover is always the one swapped back.
  - To verify on the VPS: PITR after an undo lands on the right branch (the RFC's subtlest part): restore to T1, promote (timeline 2), write, undo (back on timeline 1, recorded), write, then `restore --time` to a point after the undo follows timeline 1 with `--target-timeline=1`; the timeline read from the WAL file name right after start; the full backup after undo; undo after a dump restore putting the old stanza back and archiving to it.

### 30. `restore --dump`
- initdb a temporary cluster, create roles, `pg_restore` the dump, then the promote swap from unit 28.
- **Decided (may be revisited): the dump-restored cluster gets a new stanza.** Its system identifier is new, so it can't keep pushing to the old stanza, and `stanza-upgrade` is meant for major-version upgrades, so we don't rely on it. On promote: `NewStanza` gives `<db>-<6 hex>`, `cluster.toml` records it (an exception to "stanza immutable", made by vaire, never by `config set`), the old stanza becomes a `DroppedStanzas` record with its normal expiry (it still holds the PITR history from before the dump), then `pgbackrest-v1` for the database (stanza-create, check, first full) and `timers-v1`. Undo is the reverse (see unit 29).
- Known cost: a later `restore <db> --time` for a point before the dump would need the old stanza. For now, refuse it with a clear error naming the dropped stanza and its window rather than searching both stanzas; `backup list` already shows the dropped stanza's row.
- **Status: finished** (not verified on a server). Notes:
  - `loadFromDump`: `Materialised.Initdb` (initdb into `<data_root>/<db>__restoring` with data checksums, `--auth-local=peer --auth-host=scram-sha-256` and `[postgres] locale` / `locale_provider`, as `postgres-cluster.sh` does; initdb's own `postgresql.conf`, `pg_hba.conf` and `pg_ident.conf` are removed from the data directory, then the same registration as a materialised cluster, started open), `Materialised.CopyRoles`, the pooler auth functions (`steps.AuthFunctionsSQL`), `CREATE DATABASE <db> OWNER <db>_owner` with `DatabaseConnectSQL` and `DatabaseGrantsSQL`, `LoadDump` as `<db>_owner`, then `finishLoad` (grants again, `ANALYZE`). pg_restore errors are `restore_dump_errors`.
  - **Deviation, needs a decision:** the roles are not created from `steps.DatabaseRoles` with new passwords (the node doesn't have them, and the RFC keeps credentials unchanged): `CopyRoles` copies every role of the live cluster with `pg_dumpall --roles-only` (verifiers, memberships, per-role settings; `CREATE ROLE postgres;` is filtered out) and loads it with psql. The output goes to a buffer, never the log.
  - Promote (unit 28's cut-over, waiting until open; `pg_promote()` is skipped, it is a primary): the decided new stanza. `switchStanza` writes `NewStanza(<db>)` into `cluster.toml`, the old stanza becomes a `DroppedStanzas` record with its normal expiry, and the leftover records the old stanza. Then `applyStanza`: the database's steps first, which point `archive_command` at the new stanza with a reload (until then archive-push to the old stanza fails on the system identifier and WAL waits in `pg_wal`), then `pgbackrest-v1` (section, `stanza-create`, `check`, first full) and `timers-v1`. A failure is `after_cutover_failed`.
  - The known cost, done as decided: a `restore --time` outside the current stanza's window gets the hint naming the older stanza, its drop time and expiry (`olderStanzaHint`). It doesn't search both stanzas. `restore status` says a dump restore gets a new stanza.
  - To verify on the VPS: `pg_dumpall --roles-only` loading into a fresh cluster (the `\restrict` lines, `ALTER ROLE postgres`, the `GRANT … GRANTED BY` lines), app and migrate logins working after promote with their old passwords, through both poolers; `stanza-create` and `check` for the new stanza while the old one stays in `backup list` as dropped; the archive-push failures before the reload being harmless.

---

## G. Fork and drill

### 31. Native `db fork`
- Materialise from the stanza (`--at`) or `pg_basebackup` (no `--at`, works without backups), promote, `ALTER DATABASE/ROLE … RENAME`, rewrite `ALTER ROLE <new>_migrate SET role`, new passwords printed once, size and port from the budget, own stanza and first full.
- To verify: the shared system identifier is accepted by `stanza-create` and `check` in a separate stanza.
- From unit 27 (decided): fork stayed on the old path. Delete `CopyDatabase`, `loadFromScratch`, `finishLoad` and the old `Scratch` here (the drill moves off `Scratch` in unit 32; if it still uses it, delete it there).
- From units 27-30: what fork can reuse is in `materialise.go`: `NewMaterialised`, `Restore` with `Pause: false` (promotes at the target), `BaseBackup`, `Promote` (also resets pgBackRest's recovery settings), `CopyRoles`, and `switchStanza` / `applyStanza` / `droppedStanza` in `node/restore.go` for giving a cluster its own stanza and first full. `restoreSpace` keeps a `scratch` flag only for `db fork --at`'s scratch instance; drop it with `loadFromScratch`. `RestoreScratch` still passes `--archive-mode=off` (fine for the scratch instance, never for a materialised cluster that becomes live). The temporary cluster of a fork becomes `18/<new>`, so it must not keep `99-materialise.conf`'s port and memory settings: register the new cluster with `postgres-cluster-v1@<new>`'s own configuration instead.
- **Status: finished** (not verified on a server). Notes:
  - `handleDBFork`: preflight as before (source in `cluster.toml`, name, no open restore of the source, with `--at` backups on and the time in the window), `restoreSpace` for one copy, then under the exclusive lock `addDatabase`, the first half of `createDatabase` split out (role passwords present, `placeCluster`'s node, port and stanza, step-down, validation, `would_not_start`, save) without creating the cluster. The fork's entry is `forkSpec(src, <new>)` and `addDatabase` runs without step-down, as `db create --size` does. The role passwords go to the job's secrets. If the job can't be started, the entry is removed again (`forgetDatabase`).
  - `jobFork` → `materialiseFork`: `NewMaterialised(data_root, <new>, <src>, …)`; with `--at`, `Restore` with `Pause: false` (promotes at the target) and the `Timeline` restore undo recorded for the source's stanza; without it, `BaseBackup` (no backups needed). Then `backup.Promote` (a no-op `pg_promote()` for the base backup, and the `ALTER SYSTEM RESET` of pgBackRest's recovery settings), `renameFork` (`ALTER DATABASE <src> RENAME TO <new>`, `ALTER ROLE <src>_owner|_migrate|_app|_ro RENAME TO <new>_…`, `ALTER ROLE <new>_migrate SET role = '<new>_owner'`, and `ALTER ROLE … PASSWORD '<verifier>'` for the three login roles from the job's secrets), then `Materialised.Adopt(port)`. Any failure until then drops the copy (`m.Drop()`) and the `cluster.toml` entry. A job skipped on a busy lock removes the entry too (`notRun` in `jobs.go`, which also does restore's `failRestore`).
  - `Adopt` (new, in `materialise.go`): stops the temporary cluster, writes `/etc/postgresql/18/<new>/postgresql.conf` as the **source's own** `postgresql.conf` (Debian's file from `pg_createcluster`) with `data_directory`, `hba_file`, `ident_file`, `external_pid_file`, `port` (the budget's port from `placeCluster`) and `cluster_name` set for `<new>` (`setConf`, line-based), `start.conf = auto`, empties `conf.d` except the source's `90-overrides.conf` (`backup.UserOverrides`, copied there by `register`; set to `0644`), so the source's vaire files and `99-materialise.conf` go, and frees the temporary port. The job then applies `kernel-tuning-v1` and the new database's step instances, as `db create` does: `postgres-cluster-v1@<new>` finds the cluster with the right data directory and port, writes its cgroup drop-in and starts it through systemd; `postgres-config-v1@<new>` sees no managed files, so it renders them as a first configuration (archiving on to the new stanza, its tuning, the copied `[clusters.settings]`), keeps the existing `90-overrides.conf` (it only writes one when missing) and restarts; `postgres-roles-v1@<new>` finds every role and only re-runs grants and settings. A failure there keeps the cluster and hints `vaire apply --db <new>`.
  - Then `pgbouncer-v1` (failure: warning `pooler_reload_failed`) and `firstBackup` (`pgbackrest-v1` and `timers-v1`: stanza-create of `<new>-<id>`, check, first full; failure: `first_backup_failed`). `firstBackup` now takes a runtime and a reporter, so the job can call it. The job's result carries `warnings` as `output.Warning`s; the CLI appends them (`fork_load_errors` is gone).
  - Deleted: `CopyDatabase` (scratch.go), `loadFromScratch`, `finishLoad` (its grants and `ANALYZE` are inlined at the end of `loadFromDump`), and `restoreSpace`'s `scratch` flag. `Scratch`, `ScratchRoot`, `RestoreScratch` and `--archive-mode=off` stay: `jobDrill` still uses them.
  - **Decided:** the fork takes the source's size, as `db create --size` would: `forkSpec` copies `size`, `memory`, `memory_max`, `cpu_weight`, `cpu_quota`, `io_weight`, both pool sizes, the profile, `statement_timeout` and extensions (the data has them), and `addDatabase` runs without step-down. A size that overcommits the server gives the same `memory_overcommitted` (and `cpu_crowded`, `disk_crowded`) warnings as `db create` and goes ahead; it is refused only where `db create` refuses (validation, `would_not_start`, no free port). The backup schedules are the defaults, as a new database's.
  - **Decided:** the source's overrides carry over. `[clusters.settings]` is copied in `cluster.toml` (the source of truth; `postgres-config-v1` renders it into `50-vaire-settings.conf`, and the budget and `would_not_start` read its `shared_buffers`, `huge_pages` and `max_connections`). `90-overrides.conf` has no source of truth but the file itself (the step writes it only when missing), so `Adopt` keeps the copy `register` made of the source's. Preloads come from the copied extensions (`shared_preload_libraries` is reserved in `[clusters.settings]`), plus anything the operator put in `90-overrides.conf`.
  - Help: `db fork`'s `WhenToUse` describes the copy, the renames, the size and the stanza; the detached message says the printed credentials work once the job is done.
  - To verify on the VPS: the shared system identifier accepted by `stanza-create` and `check` in the fork's own stanza; `pg_lsclusters` reading the adopted cluster's port from the rewritten `postgresql.conf`; `postgres-cluster-v1@<new>` starting it with an empty `conf.d` (Debian defaults, briefly `listen_addresses = localhost` on the new port) and `postgres-config-v1@<new>` then restarting it with its own files; `pg_ctlcluster stop` of the temporary cluster with the systemctl redirect; the renamed `<new>_app` and `<new>_migrate` logging in through both poolers with the new passwords, and the source's passwords refused; `--at` promoting at the target (`recovery_target_action = promote`) and the recovered-to time; a fork without backups configured; the fork's size, limits and `[clusters.settings]` matching the source's in `cluster.toml`, its `90-overrides.conf` identical to the source's (a preload set there active in the fork), and the first start with only that file in `conf.d`.

### 32. Per-cluster drill
- Materialise into `<db>__drill`, `pg_amcheck` the one database, compare with live, drop. `restore drill --db`; timer staggered over the month. Result in `records.json` and `backup list`.
- From unit 25: `backup.Drill` has `database` and `Records.LastDrill(db)`; metrics and `backup list` already read drills per database. The `restore.drill` RPC takes an optional `params.database` and still defaults to `node.firstCluster`; add `restore drill --db` (default every cluster, one at a time, like `backup now`) and remove `firstCluster`.
- From unit 24: `vaire-restore-drill@<db>.timer` exists and runs the drill on that database's stanza, with the same in-day offset as its backups (default `*-*-01 05:00:00`, so every drill is on the 1st). Staggering over the month means rewriting the day field too, which `steps.Stagger` doesn't do. `backup.Drill` has no database field yet.
- From units 27-30: `waitCluster`, `reached` and `clusterLogTail` in `materialise.go` serve any temporary cluster; `Materialise` with `Pause: false` and a `Timeline` from `Records.TargetTimeline(db, stanza)` gives the drill the branch restore undo recorded.
- From unit 31: `jobDrill` is the last user of `backup.Scratch`, `NewScratch`, `ScratchRoot`, `Driver.RestoreScratch` (with `--archive-mode=off`) and `ScratchNeed` (also used by `restoreSpace` as the size of one copy; keep or rename it). Delete the rest with the move to `Materialised`.
- **Status: finished** (not verified on a server). Notes:
  - `jobDrill`: `NewMaterialised(data_root, <db>__drill, <db>, …)` (`backup.DrillName`) and `Restore` with `Pause: false` (promotes at the target; latest is `--type=default`) and `Timeline` from `Records.TargetTimeline(db, stanza)`, so a drill after `restore undo` follows the recorded branch. Checks: `restore` (with the recovered-to time; a failure copies the copy's log tail into the job log, since `pg_dropcluster` removes the log), `open <db>`, `amcheck <db>` (`pg_amcheck --install-missing --database=<db>` on the copy's port, 30-minute bound) and `tables` (`compareRows` against live: the table count and the tables only one side has, at most ten names each). The copy is always dropped (`m.Drop()`). Skipped as before with `no_backups` (now "the stanza has no backup") and `insufficient_space` (one copy of the database on the data directory's filesystem).
  - **Decided (may be revisited):** the `tables` check is informational, always `ok`: a table created or dropped since the target is expected, and a database that is missing from the copy already fails `open <db>`. The old "database list" check is gone with the whole-cluster restore.
  - Killed drills: `records.json` gains `drilling` (`<db>` → `<db>__drill`), set once the copy's port is reserved and cleared after the drop. The next drill of the database drops a recorded leftover first (`dropKilledDrill`; the heavy-I/O lock means no other drill runs); a `<db>__drill` the records don't name is refused by `NewMaterialised` (`cluster_exists`). `Records.IsTemporary` includes it, so `db list` doesn't show a running drill's copy as orphaned, and `db drop` drops a recorded drill leftover with the restore leftovers (`dropLeftovers`). A failed drop keeps the entry and logs that the next drill drops it.
  - `records.json` keeps the last 12 drills **per database** (`trimDrills`) instead of 12 in all, so `LastDrill(db)` doesn't lose a database's result once there are many databases.
  - `restore.drill` RPC: needs `params.database` like `backup.now` (`oneDatabase`); a dry run without one plans every cluster (`drills: [{database, target, timeline}]`), checking `--time` against each database's window. `firstCluster` is gone. CLI: `restore drill [--db] [--time]` runs one job per database through `eachDatabase` (one at a time, a failed or skipped database doesn't stop the others, Ctrl-C detaches and starts no more), result `drills: [{database, job_id, result, error}]`; help rewritten.
  - Timers: `steps.StaggerDay(schedule, stanza)` moves a fixed day of the month (`[YYYY|*]-[MM|*]-DD`, DD up to 28) later by an fnv hash of the stanza, wrapping within days 1–28; `timerFiles` applies it to `restore-drill` only, after the in-day `Stagger`. The default `*-*-01 05:00:00` becomes a day of its own per database. A schedule without a fixed day (or a day after the 28th) is left as it is. Like `Stagger`, it also applies to a database's own `drill_schedule`.
  - Deleted: `backup.Scratch` and its methods, `NewScratch`, `ScratchRoot` (the `vaire-scratch` directory), `Driver.RestoreScratch` / `PgBackRest.RestoreScratch` (the last `--archive-mode=off`), `liveDatabases` and `diffList`. Renamed: `ScratchNeed` → `CopyNeed` (`restoreSpace` and the drill), `scratchReadyWait` → `recoveryWait`. `scratch.go` keeps its name; it now holds only the temporary-port reservation, `LoadDump` and helpers.
  - To verify on the VPS: `restore drill --db app` materialising `app__drill`, promoting at latest with `archive_mode = off` (no history file or WAL pushed to the stanza after the promote), `RecoveredTo` after promote, `pg_amcheck --install-missing` on the copy, the `tables` detail, and the copy gone from `pg_lsclusters` afterwards; `restore drill` without `--db` for two databases one after another; a drill killed mid-restore (`systemctl stop` of its unit) leaving `drilling` set, and the next drill dropping the leftover; the rendered `vaire-restore-drill@<db>.timer` having different days for `app` and `other` (`systemctl list-timers`).

---

## H. Docs and the exit test

### 33. Docs
- MASTER (decisions table, architecture, files on a node, backups, roadmap, risks, the comparison with managed providers), IMPLEMENTATION (config, steps, tuning, backups and restores, roles and PgBouncer, timers), README (density and isolation limits, URLs by node, recovery until Phase 5), MANUAL-TESTING with the RFC's new exit test. Mark `PHASE-3-PLAN.md` superseded.
- From unit 25: document the `db` label on the vaire.prom backup lines, `vaire-stanza-expiry.timer`, and `records.json`'s `last_backups` / `last_verifies` / `dropped_stanzas[].expires_at`.
- From unit 23: IMPLEMENTATION still describes `engine.RestartBarrier` and the `restart_pending` skip reason for `pgbackrest-v1`; both are gone. `backup setup` no longer restarts.
- From units 27-30: IMPLEMENTATION's restore section still describes the scratch restore, `--db-include`, the dump-and-reload pipe, the rename swap and `name_too_long`; rewrite it for materialise, the session pooler's `<db>__restoring` entry, the data-directory cut-over, leftovers as stopped clusters, `records.json`'s `timelines` and `restores[].port` / `restore_leftovers[].port|stanza`, the new stanza of a dump restore, and the warnings `after_cutover_failed`, `restore_dump_errors` and `pooler_reload_failed`. README and MASTER: a restore needs space for one copy; `db drop` also drops the database's restore leftovers.
- From unit 31: IMPLEMENTATION's `db fork` section (the `pg_dump` pipe, the scratch restore, "copying `statement_timeout` and `extensions`", "a copy that fails leaves the new, empty database") and its files table's `vaire-scratch` line for forks; describe the native fork: materialise or `pg_basebackup`, promote, renames, new passwords, `Adopt` as `18/<new>` with its own configuration, the source's size and overrides, its own stanza and first full, and the rollback of the `cluster.toml` entry. README's `db fork` line and the guide (`first_backup_failed`, `pooler_reload_failed` now also from `db fork`).
- From unit 32: IMPLEMENTATION's drill section (the full physical restore into scratch, every database opened, the database list compared) and its files table's `vaire-scratch` line (gone entirely now); the `Driver` list's `RestoreScratch`. Describe the per-database drill: `<db>__drill` materialised and promoted at the target (on the recorded timeline), `pg_amcheck` of the one database, the `tables` comparison with live, the drop; `restore drill [--db]` one database at a time; `records.json`'s `drilling` and the 12 drills kept per database; the drill timer's day of the month staggered by stanza (`StaggerDay`, days 1–28). README's "restores into a scratch instance" drill line and MASTER's `restore drill` lines ("full restore to scratch").
- **Status: finished.** Notes:
  - MASTER: header, goal, decisions table (cluster per database, naming, master node, sizes and limits, oversubscription, hostnames, poolers, stanza per database, backup schedules, master backup, followers, HA), technology table, architecture, the files on a node, new sections on databases as clusters and the comparison with managed providers, `server init`, step catalogue, tuning and security, certificates by node, backups and restores rewritten around materialise and the cut-over, the master backup (Phase 5) instead of restic, roadmap (Phase 3 re-scoped, 4–11 per the RFC) and risks.
  - IMPLEMENTATION: command table, `cluster.toml` with `[[clusters]]` and sizes, node and operator files, operator connections by node, RPC confirms, `server init`, step instances (no `RestartBarrier`, no `Runtime.PG`), locks (global, per cluster, heavy I/O), secrets document, step catalogue and restarter, tuning per cluster with a new memory budget section, the backups and restores chapter (multi-stanza conf, reload-only setup, timers and staggering, materialise, restore, promote, undo and timelines, `--dump`, native fork, per-database drill, dropped stanzas and expiry, `records.json`), roles in every cluster with both auth functions, the two poolers, certificates, `vaire.prom` with `db`, timers, status/doctor, and Phase 5's bundle in place of restic.
  - README: databases as clusters (sizes, budget, density guesses), URLs by node, limits said plainly, recovery until Phase 5, two poolers, socket-only clusters, per-stanza backups, the native restore walk-through, fork and drill.
  - MANUAL-TESTING: `server init`; Phase 2 checks adjusted to the two poolers; the RFC's exit test as the Phase 3 checklist, with every "to verify on the VPS" item from units 13–32 under its step.
  - `PHASE-3-PLAN.md` marked superseded. The guide's `pooler_reload_failed` entry now covers `db fork`, and its intro says each database is its own cluster.
  - Left for decisions already noted on earlier units: `ext add`'s metadata still says `{server}/restart` (Phase 7; the docs describe `<db>/restart`); the `ReservedConnections` question from unit 15 is written up as open in IMPLEMENTATION's pooler section; `--defer-restart` waiting for Phase 4's window; the `app` vs `:443` URL question from unit 17 is documented as built (6432 primary, 443 with direct TLS).

---

## Decisions

- **One branch and one PR per unit**, stacked. After a parent is squash-merged, the next branch is rebased `--onto master`.
- **No VPS checkpoint before the end.** The only manual test is the exit test, after unit 33.
