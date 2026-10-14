# Phase 3 plan: database backups & restore

> **Superseded** (2026-10-13). Phase 3 was re-scoped by [`RFC-cluster-per-database.md`](./RFC-cluster-per-database.md) to "Databases as clusters, backups and native restore" and built from [`PHASE-3-WORK-UNITS.md`](./PHASE-3-WORK-UNITS.md). This plan describes the earlier shared-cluster design (one `18/main`, scratch restores and a rename swap) and is kept only as history.

Goal (MASTER roadmap): back up to S3/R2, PITR restore, drills, per-database backup policies and `db fork`.

**Exit test (reworded for the per-database model):** take a backup, write rows to `app`, restore `app` to a time before them while a second database keeps serving, then promote. Run a drill. Fork a database at a point in time with `db fork`.

What exists today: `[backup]` and `[backup.database]` already decode and validate in `internal/config`. `secretsdoc.Backup` (the recovery kit) is already in the document schema. `cluster init` accepts the backup flags but only warns `flag_not_used_yet`. `eru node run` knows only `certs-renew`. Step 8 doesn't install `pgbackrest` yet, and there's no `40-eru-backup.conf`.

---

## 0. Decisions settled in review

- **Backups are optional in `cluster init`.** The interview asks; "Skip for now" is allowed and prints `eru backup setup`.
- **Long-running work runs as detachable jobs** (transient systemd units + `eru job follow`).
- **The user-facing model is per database.** pgBackRest and WAL archiving run cluster-wide underneath because PostgreSQL has one WAL stream for all databases, and PITR needs it. Nobody restores "the cluster": every restore, fork and listing names a database. A restore rewinds one database while the others keep serving.
- **`restore` and `db fork` stay two commands.** They share one node-side pipeline (§4.4).
- **Per-database dumps exist only for longer retention** than the PITR window (§5).

## 1. Gate: pgBackRest health (checked while planning)

- 2026-04-27: the maintainer archived the project after losing sponsorship; 2.58.0 was announced as the last release.
- 2026-05-18: a sponsor group (AWS, TigerData, Supabase, Percona, pgEdge) funded it again; 2.59.x has shipped since (2.59.0 adds PG 19).
- **2.59.3 fixes a bug in encrypted repos** (an unchecked error when generating random data for encryption). eru always encrypts, so **2.59.3 is the floor**.

Decision: stay on pgBackRest. Install it from PGDG (already configured by step 8), `apt-mark hold` it, record `PgBackRestMinVersion = "2.59.3"` in `internal/versions`, and fail step 8 with `precondition_failed` if PGDG offers something older. Keep `backup.Driver` as the seam for WAL-G; don't build WAL-G. Add the April/May history and the floor to MASTER's risks.

---

## 2. Turning backups on

Every Phase 2 cluster has `backup.provider = ""`, so backups must be addable to an existing cluster as well as at init.

### 2.1 `eru backup setup` (new operator command)

- Interview in a TTY: provider preset (§13.1 table), endpoint, region, bucket, prefix (default: cluster name), access key ID, secret key (hidden input). Flags for everything: `--provider --endpoint --region --bucket --prefix --uri-style --key-id --secret-stdin|--secret-file`, plus `--full-schedule --diff-schedule --incr-schedule --retention-full`.
- The interview phrases retention as a **restore window** ("you can rewind any database up to ~4 weeks"), not as "full backups kept".
- **Operator-side bucket probe** before anything reaches the node: HEAD the bucket, then put/get/delete `<prefix>/.eru-probe` with `minio-go`. Errors name the cause: `backup_auth_failed`, `bucket_not_found`, `endpoint_unreachable`. A prefix holding another cluster's stanza is refused (and `stanza-create` catches a system ID mismatch again).
- The operator generates `cipher_pass` (64 chars base62) and, for §5, an age X25519 identity. It saves them with the keys in `credentials.json` **before** sending, writes `[backup]` through `config.set` with `base_revision`, then runs `apply --step postgres-config-v1 --step pgbackrest-v1 --step timers-v1`.
- The first enable flips `archive_mode = on`, which needs a restart: `--confirm <cluster>/restart`, and the prompt says "Turning on WAL archiving restarts PostgreSQL once (about N s of downtime)".
- Ends with `secrets_document_stale` and the hint `eru secrets export`. In a TTY it offers the secrets handoff right away.
- Re-running rotates keys for the same bucket. Moving to a different bucket or prefix once a stanza exists is refused (`precondition_failed`, `reason: repo_in_use`), since the restore window would be lost.

### 2.2 `cluster init`

The backup flags start working. The interview gets a "Backups" page with "Skip for now". When configured, the setup runs between the database phase and the handoff, so the handoff already carries the recovery kit and the first full backup result. When skipped, the final summary prints `eru backup setup`.

### 2.3 Secrets on the node

- New RPC secret names in `internal/steps/secrets.go`: `backup.s3_key_secret`, `backup.cipher_pass`. Key ID and the age recipient travel alongside but aren't secret.
- `pgbackrest-v1` writes `/etc/pgbackrest/pgbackrest.conf` (postgres 0600) and `/etc/eru/runtime/backup.env` (0600) from request secrets when missing or rotated; otherwise it reuses what's on disk. Secrets never enter the input hash. A missing file with no secret in the request is `input_required`.
- Plan diffs of `pgbackrest.conf` redact `repo1-s3-key-secret` and `repo1-cipher-pass`.
- The operator top-up before `apply` covers backup secrets too. With `[backup]` configured and no cipher in `credentials.json`, it's `input_required` (never generate a new cipher for an existing repo), with the hint to `eru login` from a current bundle.

---

## 3. Steps, timers and the driver

| # | Step | Change |
| --- | --- | --- |
| 8 | `postgres-packages-v1` | Also installs and holds `pgbackrest` (floor check). No ID bump before the first release |
| 11 | `postgres-config-v1` | Renders `40-eru-backup.conf` when `[backup]` is configured, removes it otherwise. `archive_mode` goes through the existing restart classification |
| 14 | `pgbackrest-v1` (new) | §13.1 config; `install -d` for `/var/spool/pgbackrest` (and `/var/lib/pgbackrest` if `local_repo`); `stanza-create`, `check`; first full if the journal has no `first-full`. Skipped (not failed) without `[backup]` |
| 17 | `timers-v1` (pulled forward from Phase 5) | Renders the §18 unit pairs for every schedule in `cluster.toml`, takes ownership of `eru-certs.timer` from `tls-v1` now, disables `eru-*.timer` not in the render. Phase 5 only adds `maint-*` and `metrics-refresh` |

`DatabaseStepIDs()` gains `pgbackrest-v1` after `pgbouncer-v1`, then `timers-v1`. Schedules are validated with `systemd-analyze calendar` on the node during `config.set`.

`eru node run` gains `backup-full`, `backup-diff`, `backup-incr`, `backup-verify`, `restore-drill` and `db-dump`. Each takes its lock with timeout 0 and records `skipped` when busy.

```
internal/backup/
  driver.go        Driver: StanzaCreate, Check, Backup, Info, Verify, RestoreScratch, ArchiveStatus
  pgbackrest.go    the implementation; shells out through the engine shell (redaction applies)
  progress.go      pgbackrest detail logs -> progress events
  scratch.go       scratch instance lifecycle (restore, start on the drill socket, stop, remove)
  swap.go          the per-database swap (§4.3)
  dumps.go         age-encrypted streaming dumps to the bucket (§5)
  state.go         state.json: restores (per database), drills, last results
  metrics.go       eru.prom backup lines (scraped from Phase 5 on)
  space.go         free-space checks against `info` sizes
```

`internal/render` gets `pgbackrest.conf.tmpl`, `40-eru-backup.conf.tmpl` and the timer templates.

---

## 4. Commands

### 4.1 Backups

| Command | Notes |
| --- | --- |
| `eru backup setup` | §2.1 |
| `eru backup now [--type full\|diff\|incr]` | Runs as a job. Default `incr` if a full exists, else `full` |
| `eru backup list [--db <name>]` | The restore window (from, to, archive lag), sets with sizes, last verify, last drill, and per-database long-retention dumps. With `--db`, only what applies to that database: its window, and which sets contain it (from the backup manifest) |
| `eru backup verify` | `pgbackrest verify`; result to `state.json` and metrics |

`backup now` stays unscoped. Physically it always covers every database, and it says so in its help text.

### 4.2 Detachable jobs

`backup.now`, `restore`, `restore.drill` and `db.fork` start their work as `systemd-run --unit=eru-job-<request_id> eru node run <job> …`. The lock is held inside the unit and the RPC streams the unit's log. Ctrl-C or a dropped tunnel detaches without stopping the work. The command prints the job ID; `eru job follow <id>` re-attaches, and `eru job list` shows running and recent jobs from `state.json`. Timers use the same path.

### 4.3 `eru restore <db>`: rewind one database

```
eru restore <db> --time <ts> | --latest | --set <label> [--promote]
eru restore status [<db>]
eru restore promote <db>
eru restore abort <db>
eru restore cleanup [<db>]
```

Confirm token `<db>/restore` (it replaces a live database). Steps:

1. **Preflight:** target inside the restore window, `<db>` exists, no restore in progress for `<db>`, and enough free space for a scratch copy of that database plus WAL (`space.go`). `--time` takes RFC 3339 or relative (`-15m`), echoed in UTC and the cluster timezone before the confirm.
2. **Scratch restore:** `pgbackrest restore --db-include=<db> --pg1-path=/var/lib/eru/scratch/<id> --type=time --target=<ts> --target-action=promote --archive-mode=off`. Other databases come back as zeroed sparse files, so scratch costs about one database, not the cluster. Start it on the drill socket (port 5499, no TCP, `archive_mode=off`).
3. **Load beside the live database:** `pg_dump -Fc` from scratch into `pg_restore --no-owner --no-privileges --role=<db>_owner -d <db>__restoring` on the live cluster, then re-run the grants and default privileges from `postgres-roles-v1` for `<db>`. Tear down scratch. The live `<db>` is untouched and still serving.
4. **Paused:** `state.json.restores[<db>] = {id, target, phase: ready, …}`. `eru sql --db <db>__restoring` / `psql` / `console` let the operator inspect the restored copy. `restore status` shows the target, row-count comparison per table (cheap `pg_class.reltuples`), and what promote will do.
5. **Promote (the swap, a few seconds):** close `<db>` to new connections and terminate its backends (direct 5432 sessions and PgBouncer's server connections), `ALTER DATABASE <db> RENAME TO <db>__pre_restore_<ts>`, `ALTER DATABASE <db>__restoring RENAME TO <db>`, open `<db>` again. PgBouncer is not paused; only this database's clients see errors for those seconds. Role names and passwords are unchanged, so apps reconnect with the same credentials. The old copy is recorded in `restore_leftovers`.
6. `--promote` on the first command does steps 1–5 in one go.

- `abort <db>`: drop `<db>__restoring`; the live database was never touched.
- `cleanup [<db>]`: lists leftover `__pre_restore_*` databases with sizes, then drops them (`--confirm <db>/cleanup`). Until then the pre-restore copy is a one-command undo: `restore undo <db>` swaps it back the same way.
- While a restore of `<db>` is open, `db drop <db>`, `db fork <db>` and `db credentials <db> --rotate` refuse (`reason: restore_in_progress`). Other databases, `apply` and backups carry on.
- `__restoring` and `__pre_restore_*` names are reserved: `db create` refuses them, and `postgres-roles-v1` and `orphaned_database` ignore them.

No in-place whole-cluster restore in Phase 3. Rebuilding a whole cluster is Phase 7's `cluster init --restore`, and the drill exercises the full physical restore path every month.

### 4.4 `eru db fork <src> [--name <new>] [--at <time>]`

Shares steps 1–3 of the restore pipeline, with a new database instead of a swap:

- **Without `--at`:** no scratch at all, `pg_dump -Fc` from the live database straight into the new one.
- **With `--at`:** scratch restore of `<src>` at that time, as in `restore`.
- The new database goes through the `db create` path: a `[[databases]]` entry (copying `statement_timeout` and `extensions`), its own `<new>_owner/_migrate/_app/_ro` roles, operator-generated passwords sent as secrets and printed once with the URLs. `pg_restore --role=<new>_owner`.
- `--name` defaults to `<src>_fork_<yyyymmddhhmm>` and must fit the 63-char role rule.
- Not destructive, no confirm token. Runs as a job.

This replaces `restore --db` and the `createdb … -O <name>_owner` sketch in §13.5.

### 4.5 Drill

`eru restore drill [--time]` and the monthly timer, as in §13.4: a **full** physical restore to scratch (the path Phase 7 relies on), every database opened and `pg_amcheck`ed, the list compared with live. Result to `state.json.drills` (keep 12) and shown in `backup list`. Doctor integration waits for Phase 5.

---

## 5. Per-database policies: long-retention dumps

The restore window is the same for every database (`retention_full`). A database that has to be kept longer, for example 90 days for compliance, gets scheduled dumps on top:

```toml
[[databases]]
name = "app"
[databases.backup]
dump_schedule = "*-*-* 03:00:00"   # empty = no dumps (default)
dump_retention = 90                  # dumps kept
```

- Strict decode still holds (old files decode). `config set databases.app.backup.dump_schedule …` works through the dotted-key path.
- `timers-v1` renders one `eru-db-dump@<db>.timer` per database with a schedule.
- `pg_dump -Fc` streamed from the node to `<prefix>/dumps/<db>/<ts>.dump.age` with `minio-go`, age-encrypted to the X25519 recipient. The identity lives only in `credentials.json` and the recovery kit, so a compromised node can't read old dumps.
- `eru dump <db> [--to bucket|local] [--out]` (already in the command tree) takes one on demand; `--to local` streams it over the tunnel to the laptop.
- `eru dump list [<db>]`, and `eru dump get <db> <id> --out <file>` (download and decrypt on the laptop).
- `eru restore <db> --dump <id>` restores from a dump through the same `__restoring` + swap path, for points older than the restore window.
- `db drop` takes a final dump first when backups are configured (`--no-final-dump` skips it), replacing today's `no_backup_before_drop` warning.

---

## 6. Order of work (stacked branches)

1. **Packages + config + driver:** pgbackrest in step 8 with the floor, `internal/backup` skeleton, render templates, `40-eru-backup.conf` in step 11, backup secrets plumbing.
2. **`backup setup` + step 14 + init wiring:** bucket probe, cipher and age identity, recovery kit in the handoff, first full backup.
3. **`timers-v1`, `node run` backup jobs and the job model:** `backup now/list/verify`, `job follow/list`, `eru.prom`.
4. **Scratch restore + `db fork`** (the shared pipeline, without the swap).
5. **`restore <db>`:** swap, `status/promote/abort/undo/cleanup`, guards.
6. **Drill.**
7. **Long-retention dumps:** `dump`, `dump list/get`, `restore --dump`, timers, final dump on `db drop`.
8. **Docs:** IMPLEMENTATION (§4.2 command tree, §11 steps, §13 rewritten for the per-database model, §18, secrets table), MASTER (decisions table, §6.5, risks, roadmap status), MANUAL-TESTING.

Each branch ends with `just lint` and a build for all targets. No local test harnesses.

## 7. Manual test (real VPS)

1. On a Phase 2 cluster with databases `app` and `other`: `eru backup setup` against R2 gives one restart and a first full backup, and `backup list --db app` shows a restore window. `eru secrets export` carries the recovery kit, and `eru login` on a second machine accepts it.
2. Under a write loop on `other`, write rows to `app`, note the time, write more, then `eru restore app --time <t>`. It's ready; inspect `app__restoring` with `eru sql`; `restore promote app` leaves only the early rows in `app`, the app reconnects with the same credentials, and `other` never stopped.
3. `restore undo app` brings the newer rows back; `restore cleanup app` removes the leftover.
4. `restore app --time <t>` then `restore abort app` leaves `app` unchanged.
5. Close the laptop lid during `backup now --type full`; `eru job follow` shows it finished.
6. `eru restore drill` passes and shows in `backup list`.
7. `eru db fork app --at <t>` creates a new database with its own credentials and the rows as of `t`; `eru db fork app` without `--at` copies live.
8. A scheduled dump of `app` lands in the bucket; `eru dump get` decrypts it and `pg_restore --list` reads it. `db drop` leaves a final dump.
9. A second `eru apply` changes nothing.
