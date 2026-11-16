# V3 final-candidate Linux qualification (2026-11-16)

V3 source freeze: 2026-11-16T18:01:49Z.
Manifest digest:
`66ae70f96f88f0577dac3388b9da6f26add06a897ad047950e762ab8c27ff7ea`
(687 files). `sha256sum -c` passed before and after build and again after the
run. Source was not edited.

- Build: `bin/vaire-extension-final-v3`
- Identity: `vaire dev (84f71bbaa180-ext-v3-66ae70f96f88, dev)`
- SHA-256: `069516576918705d82b137994adf6dfe3588951a83a1ac75b8a2d3db99ca2c6f`
- Guest `/usr/local/bin/vaire` matched the same SHA-256.
- Guest: Debian 13.7, 4 vCPU, 4 GiB, 20 GiB, clean baseline. Normal
  `vaire server init` provisioned a fresh `v3-lab` workspace at 18:22Z;
  PostgreSQL 18 and the WireGuard operator path worked.
- CLI transport was exercised successfully over WireGuard; no CLI/WG
  limitation was needed for this run.

## Case results

| Case | Result | Sanitized observation |
| --- | --- | --- |
| EFIN16 | **PASS (fresh)** | Fresh custom-only `v3src` -> `v3fol`: source and follower both `SHOW shared_preload_libraries = pg_stat_statements,auto_explain,pg_stat_kcache`; follower owned guard line is the same correct order, not lexical; no `pg_stat_statements.max` placeholder; native `pg_stat_statements.max=5000`; follower started and streamed. |
| EFIN14 | **PASS (fresh SQL convergence)** | `v3fresh` with `extensions=[]`: an allowed manual fixture DROP removed the installed `pg_stat_statements` SQL, then normal `apply --db v3fresh` completed roles with no pending restart and recreated `pg_stat_statements` 1.12 from native defaults: `max=5000 source=default`, `track=top source=default`; no placeholder written. |
| EFIN15 | **PASS (unit guard equivalence)** | `v3unit` custom override of builtin `vector` with `copy_safety=disabled`, `copy_settings work_mem='1024kB'`, follower `v3unitfol`. Guard line: `work_mem = '1024kB'`; follower `SHOW work_mem = 1MB`; `pg_settings work_mem setting=1024 unit=kB source=configuration file`; follow succeeded. Fixture used normal `ext add`/follow, no generated-file edit. |
| X1 | **PASS** | `pgvector` alias added to `v3maint`; persisted as `vector`, version 0.8.7, package `postgresql-18-pgvector`; package remains installed after removal; no unrelated restart required. |
| X2 | **PASS** | Real `postgresql-18-pg-stat-kcache` 2.3.2-1.pgdg13+3: control and `.so` present, custom descriptor, `pg_stat_kcache` 2.3.2, effective preload; extnamespace `public`; source/follower both load stats before kcache. |
| X4 | **PASS** | `v3_vec_dep.v vector(3)` dependent column made `ext remove vector` fail with `extension_in_use` (1 dependent, 0 own tables); `--cascade` succeeded, dropped the column, kept the table and package. |
| X5 | **PASS** | `pg_cron` on `v3cron`: no-token request required `v3cron/restart`, left package absent and extension absent; exact token installed/restarted only `v3cron` (uptime 8 s) while v3maint/v3src/v3fol stayed at their prior uptimes; effective preload `pg_stat_statements,auto_explain,pg_cron`. |
| X6 | **PASS** | `pgaudit` on `v3src`/`v3fol`: source-only token refused (`v3fol/restart` required); both tokens succeeded, follower configured/restarted first, both showed `pg_stat_statements,auto_explain,pgaudit,pg_stat_kcache`; normal `apply --db v3src` rc=0 no pending; stopped `v3src`+`v3fol` normal apply brought both online. Pgaudit removal without restart tokens was refused before effects; a direct both-token removal then ran and kept the package. **No ordinary pending-removal retry was executed.** One earlier pgaudit attempt was blocked by a lab-only `kcache_unit` descriptor conflict created during EFIN15 setup; clearing that own descriptor's preload via normal `config set` allowed the successful retry. |
| EFIN12 | **PASS (main) / PARTIAL (extra)** | Fresh `v3hist`: preinstalled hypopg missing-control and bogus APT package both failed rc=5 `package_install_failed`, `intent_saved=false`, config unchanged; requirement history held only `auto_explain`/`pg_stat_statements`, no hypopg; `db follow v3hist -> v3histfol` succeeded. Extra SQL-absent removal of `pgstattuple` on v3maint returned `installed=false` with no false DROP claim; the result has no explicit `sql_dropped` field. |
| X7 | **PASS** | Real cron job on `v3cron`: follower `v3cronfol` had `cron.launch_active_jobs=off`, copied job inactive after unfollow (`active=f database=v3cronfol`), source `active=t database=v3cron` and source settings unchanged; source had an active `pg_sleep` job session while follower had no pg_sleep session. Fork `v3croncopy` had job `active=f database=v3croncopy`, launch off, copied source history only; source unchanged. |
| X10 | **PARTIAL (manual+auto PASS)** | `v3_repack` owner `v3maint_owner`, no explicit ACL; dry run selected `public.v3_repack`, need 9,846,784 bytes, versions 1.5.3; actual used `pg_repack ... --no-kill-backend --wait-timeout=60` and returned `completion_unknown/clean_exit` (selected=1, unknown=1). Auto: `v3_auto` owner `v3maint_owner`, no explicit ACL, bloat 77,987,840 bytes / 0.938; `maint-weekly` selected it and returned `completion_unknown/clean_exit`; table stayed 500 rows. Protected refusal not exercised. |
| X11 | **PASS (functional)** | New `v3_measure` owner `v3maint_owner`: exact report `method=pgstattuple`, table_bytes=1,523,712, tuple_count=5,000, tuple_bytes=1,460,000 (95.82%), dead=0, free_bytes=19,248 (1.26%). Full scan, not `_approx`; EFIN9 OID-race was not independently forced. |
| EFIN16 legacy drift | **BLOCKED / not exercised** | No old lexical guard fixture remains; creating one requires generated-file/record injection, which is forbidden. Fresh EFIN16 order passed. |
| Protected/TOTP, crash, drill, bucket/rebuild, platform | **BLOCKED** | Not exercised by design; no bucket, TOTP fixture, crash injection or destructive rebuild was authorized. |

## Commands and identities

- `vaire server init ... --name v3-lab --domain vps-lab.test --tls-mode self-signed --default-size nano` at 18:22Z.
- `db create` and all `ext`/`apply`/`db follow`/`db unfollow`/`maint` runs used the V3 CLI over the workspace tunnel; direct node RPC was also used for read-only facts.
- Preload/order facts came from `SHOW shared_preload_libraries`, `pg_settings` sourcefile/sourceline, exact managed `30-vaire-extensions.conf` lines and only the exact guard `shared_preload_libraries`/`work_mem` lines in auto.conf. No other auto.conf lines or credentials were printed.
- Verified package facts: pgvector 0.8.7, pg-stat-kcache 2.3.2, pgaudit 18.0, pg_repack 1.5.3, pg_cron 1.6.8; all `hold ok installed`.

### UTC milestones and job IDs

```text
18:22:32Z  server init v3-lab completed
18:28:49Z  v3maint created
18:30:11Z  v3src -> v3fol follow
18:32:58Z  v3fresh empty-selection apply
18:34:43Z  v3unit -> v3unitfol follow (EFIN15)
18:38:52Z  v3cron pg_cron add/restart
18:40:01Z  v3cron -> v3croncopy fork; job 01M7JTHE24STXDBC11TGCXX29T
18:41:00Z  v3cronfol unfollow; job 01M7JTK79MW98YVW4Y8498H1EC
18:42:07Z  EFIN12 failed preinstalled hypopg add; op 01M7JTN88VS2WS3K36DZ7WJ8H7
18:42:08Z  EFIN12 failed bogus-package hypopg add; op 01M7JTND5KXD5MKZB4ZRQMMVSB
18:42:28Z  v3hist -> v3histfol follow
18:44:20Z  v3maint exact pgstattuple report
18:44:33Z  v3maint manual repack; job 01M7JTSQNQB7835ZWQ2GMJ0DPY
18:45:37Z  v3maint auto-repack maint-weekly completion
```


## Cleanup and remaining state

- All clusters left online: `v3maint` 5433, `v3src` 5434, `v3fol` 5435, `v3cron` 5436, `v3cronfol` 5437, `v3hist` 5438, `v3histfol` 5439.
- Remaining fixtures: v3maint tables `v3_measure`, `v3_repack`, `v3_auto`, `v3_vec_dep`; v3src/v3fol with kcache (pgaudit removed, package retained); v3cron/v3cronfol with `v3sleeptest` **source job removed**, copied job left inactive/retargeted; v3hist/v3histfol default stats. No app data existed.
- Cleanup 2026-11-16T18:51:19Z: `SELECT cron.unschedule('v3sleeptest')` on v3cron (returned `t`); source `cron.job` count became 0. One already-running own `pg_sleep(60)` backend finished naturally; after a bounded 65-second wait no `pg_sleep` backend remained on v3cron and no active copied job existed on v3cronfol. No backend was killed. Extension/source configuration and all test data otherwise preserved. No temporary host transfer server remains running.
- Post-cleanup source check: manifest `sha256sum -c` PASS and digest
  `66ae70f96f88f0577dac3388b9da6f26add06a897ad047950e762ab8c27ff7ea`;
  V3 binary SHA-256 `069516576918705d82b137994adf6dfe3588951a83a1ac75b8a2d3db99ca2c6f` unchanged.
- Private init/handoff and database-create outputs were removed after use; the
  operator workspace credentials remain only in private lab state. No raw
  captures were added to the repository. No external bucket, production state,
  eregion repository/state, or source edit was used.
- This is integrated real PostgreSQL/systemd-via-node-run and CLI transport evidence; it is independent of source acceptance/release status and makes no publish/tag/production claim.
