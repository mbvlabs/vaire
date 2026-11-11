# Phase 7 upstream implementation evidence

Read-only research on 2026-11-10; no package install or live verification. Upstream main may differ from installed packages. Check actual versions/catalogs/effective settings at runtime.

## Copy scheduling

[pg_cron documentation](https://github.com/citusdata/pg_cron#setting-up-pg_cron) and [launcher source](https://github.com/citusdata/pg_cron/blob/main/src/pg_cron.c): jobs do not launch on hot standby but launch automatically after promotion. `cron.launch_active_jobs=off` is a superuser SIGHUP setting and must remain effective through promotion, rename, job deactivation and privilege settlement. It does not prove already executing jobs stopped.

Cron metadata lives in `cron.database_name`, not necessarily the application database. Inspect the actual setting. Deactivate all copied jobs; retarget only jobs naming the old database, never rewrite SQL commands or touch the source. `cron.alter_job` appeared in SQL version 1.4; direct privileged catalog updates are documented but require version/column checks and verified postconditions. Unknown/failed modification leaves launch suppression in place and reports failure. [Job management](https://github.com/citusdata/pg_cron#managing-and-creating-jobs), [metadata implementation](https://github.com/citusdata/pg_cron/blob/main/src/job_metadata.c).

## Repack

[pg_repack manual](https://reorg.github.io/pg_repack/) and [SQL definitions](https://github.com/reorg/pg_repack/blob/master/lib/pg_repack.sql.in): match executable, loaded library (`repack.version()`) and SQL extension (`repack.version_sql()`) versions on the actual target. A supported key is valid, unique, non-partial, expression-free, with every key attribute non-null (included attributes may differ). Conservatively accept only permanent ordinary tables and exclude special extension storage.

Use both `--no-kill-backend` and bounded `--wait-timeout`. The timeout bounds lock acquisition, not total work. Most work uses SHARE UPDATE EXCLUSIVE with short ACCESS EXCLUSIVE phases. The owner's `--no-superuser-check` skips a client check only; it does not establish vaire's protected privilege path. Bounded approved fallback: refuse protected repack at admission, execution and automation.

The manual recommends additional free space about twice table plus indexes. The handoff's table-plus-index threshold is a minimum, not a sufficiency guarantee. Implementation should conservatively require twice that footprint plus existing host reserve, with the stronger prerequisite disclosed; no new resource-management subsystem is needed.

## Measured table report

[PostgreSQL 18 pgstattuple](https://www.postgresql.org/docs/18/pgstattuple.html): `pgstattuple(regclass)` performs a full scan, unlike `pgstattuple_approx`. Return table length, live/dead tuple counts/bytes/percentages and free bytes/percentage with method explicitly named. These are not guaranteed reclaimable bytes or exact index bloat. Page overhead means components need not sum to table length.

The function takes a read lock; concurrent updates make the page-by-page observation non-instantaneous. Keep one-table scope and report timeout/permission failures without superuser escalation. Default execution is granted to `pg_stat_scan_tables`; verify effective monitor privileges.

## Timescale copy suppression

Tagged upstream 2.31.0 source supports PostgreSQL 18 and accepts `timescaledb.max_background_workers=0` (range 0–1000, PGC_POSTMASTER). It must win at postmaster startup, not merely reload. [Counter](https://github.com/timescale/timescaledb/blob/2.31.0/src/loader/bgw_counter.c), [launcher](https://github.com/timescale/timescaledb/blob/2.31.0/src/loader/bgw_launcher.c), [configuration](https://www.tigerdata.com/docs/self-hosted/latest/configuration/timescaledb-config/).

The recovery-finished launcher can briefly run and exit at zero; source supports suppression of database schedulers and their scheduled telemetry/jobs, not the claim that no process or log effect occurs. Copied job metadata remains present, so independent copies must retain the disabling setting until deliberate operator change. Apache-only builds still have scheduler/telemetry paths; licensing is not a safety mechanism.

This source evidence supports the approved catalogue's expected disabling mechanism, not unconditional compatibility certification for every provider/version. Actual package/control/runtime/effective-setting facts and live promotion qualification remain required; alternate providers require renewed operator safety declarations. Refuse unknown or incompatible mechanisms rather than assuming edition implies safety. No exhaustive audit, packaged binary verification or live test was performed.

pg_partman's no-background-worker default means only that `pg_partman_bgw` is not preloaded. External schedulers can still call maintenance and must be handled through their own copy-safety requirements.
