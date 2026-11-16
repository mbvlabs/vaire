# Extension live results, 84f71bb, Debian 13.7 lab

Candidate under test: worktree `/home/mbv/work/vaire-phase-7` at
`84f71bbaa180`, binary `bin/vaire-extension-lab` (dev/dev/dev), SHA-256
`7f5fab783bd314ce09cec55a7d27fd65e7226634c1a4ed3de6f91eca5ce40ddc`.
Requests were direct `vaire node rpc` as root on the disposable Firecracker
guest (4 vCPU, 4 GiB, 20 GiB, Debian 13.7, PostgreSQL 18).
The CLI transport itself was unqualified: public SSH is closed and the old lab
workspace keys do not match the guest's current operators. Direct node RPC is
the production dispatcher; jobs run through systemd and `/usr/local/bin/vaire`.

Raw guest captures were withheld from the repository and kept only in private
host/guest lab state: they include complete run/log streams that were not
provably sanitized at capture. The observations in this file are the sanitized
record; no request secrets or credential files are included here.
All test rows are bounded: 5,000-10,000 rows per table, largest fixture 85 MB;
no production state or credentials were used.

## Case results

| Case | Result | What was actually observed |
| --- | --- | --- |
| X1 | PASS | `pgvector` alias resolved to `vector`; package `postgresql-18-pgvector` 0.8.7-1.pgdg13+2 installed, control/library present, installed/default 0.8.7; add created SQL and remove dropped it; package retained; `app`/`other` uptimes unchanged. |
| X2 | PASS | Real non-built-in APT package `postgresql-18-pg-stat-kcache` 2.3.2-1.pgdg13+3 installed; custom `[extensions.pg_stat_kcache]` descriptor persisted with preload/settings/safety; control and `pg_stat_kcache.so` present; effective `shared_preload_libraries=pg_stat_statements,auto_explain,pg_stat_kcache`; `pg_stat_kcache.track=all`, no pending restart; version 2.3.2. Missing restart token had zero effects. |
| X3 | PASS | `hypopg --preinstalled` without files refused with the exact missing `hypopg.control`; a bogus package name refused after apt reported `E: Unable to locate package ...`; both left `cluster.toml` hash unchanged and no SQL/extension/package effect. Awaiting-recovery preservation was not exercised (no awaiting database). |
| X4 | PARTIAL | `vector` add, dependent `ext_vec_dep.v vector(3)`, remove without cascade refused with `extension_in_use`, one dependent and zero own tables; explicit `cascade:true` dropped it and kept the table; repeat remove returned `extension_not_installed` with no effect. Protected removal not exercised (no supported no-TOTP protected fixture). |
| X5 | PASS | pg_stat_kcache add without `extlab/restart` refused before package/config/SQL; with the token only `extlab` restarted (56 s uptime) while `app`/`other` kept their 17,597/20,352 s uptimes; CREATE ran only after effective preload; setting/pending facts observed. |
| X6 | PASS (bounded) | source `extsrc`, follower `extfol`; dry run required `extsrc/restart` and `extfol/restart`; source-only token refused before any config change (hash unchanged); both tokens configured/restarted the follower first, source second, and both reported pg_stat_kcache 2.3.2 with effective preload; direct follower `ext.add` refused `is_follower` with unchanged config. A separate `apply --db source` bypass test was not run. |
| X7 | PARTIAL (fork + unfollow PASS on fixed overlay) | pre-fix real cron follow/unfollow blocked by EFIN11; fixed EFIN1+EFIN11 overlay completed a real cron-bearing unfollow: live `pg_cron launcher` backend PID 51622 was terminated, rename succeeded, copied job became `active=f database=extcronfol`, `cron.database_name=extcronfol`, `cron.launch_active_jobs=off`, source job stayed `active=t database=extcron`. Fork `extcron -> extcroncopy` on the same overlay succeeded; copy job `active=f database=extcroncopy`, `cron.database_name=extcroncopy`, `cron.launch_active_jobs=off`; source unchanged; copied `cron.job_run_details` count stayed 17 for 65 s. Drill is BLOCKED: no backup bucket/stanza is configured. |
| X8 | BLOCKED | No supported deterministic fault boundary was available without corrupting records or adding fault hooks. A real interrupted unfollow retry did resume and settle (see EFIN1 result); that is not a general extension partial-retry proof. |
| X9 | BLOCKED | No backup bucket/rebuild credentials are authorized; no bucket or destructive rebuild was attempted. EFIN12 is related history-store contamination, not an X9 pass. |
| X10 | PARTIAL | Manual `maint repack --table public.ext_repack --now`: dry run selected the table, `TBL/TOAST/index` need 9,846,784 bytes, version check deferred; actual command was `pg_repack ... --no-kill-backend --wait-timeout=60`, client/library/SQL all 1.5.3, table stayed 4,000 rows and storage changed; result was truthfully `completion_unknown/clean_exit` (repacked=0, unknown=1). Auto-repack: default off, then opt-in. The first auto candidates were postgres-owned lab tables, where `pg_stats` is hidden from `vaire_admin`; that was a fixture artifact. After revoking the diagnostic grant, a fresh table owned by the app owner (`extlab_owner`) and carrying no explicit grant was estimated at 77,987,840 bytes / 0.938 and auto-repacked (`selected=1 unknown=1`) without a grant. EFIN13 is therefore not reproduced for ordinary app-owned tables. Protected refusal not exercised; final-candidate auto rerun still waits for freeze. |
| X11 | PASS | `maint report bloat --exact --table ext_measure` returned `method=pgstattuple`, `table_bytes=1523712`, `tuple_count=5000`, `tuple_bytes=1460000`, `tuple_percent=95.82`, `dead=0`, `free_bytes=19248`, `free_percent=1.26`. Source uses the full `pgstattuple` function, not `_approx`; `vaire_admin` has `pg_monitor`/`pg_stat_scan_tables`; `ReportSetup` sets `lock_timeout=5s` and `statement_timeout=60s`. EFIN9's final OID-pin rerun is pending candidate freeze. |

## Commands and identities

Representative request shape (all requests used `schema=vaire-rpc/v1`,
`operator=op-1`, `client_version=dev`, `client_channel=dev`, and the matching
`client_commit`):

```sh
printf '%s\n' '{"schema":"vaire-rpc/v1","method":"ext.add",
 "params":{"name":"pg_stat_kcache","db":"extlab",
 "package":"postgresql-18-pg-stat-kcache","preload":"pg_stat_kcache",
 "settings":{"pg_stat_kcache.track":"all"},
 "copy_safety":"safe","standby_safe":true},
 "confirm":"extlab/restart","operator":"op-1","client_version":"dev",
 "client_commit":"dev","client_channel":"dev"}' \
 | /tmp/vaire-extension-lab node rpc
```

Package/control facts were read with `dpkg-query`, `ls` on
`/usr/share/postgresql/18/extension/*.control` and
`/usr/lib/postgresql/18/lib/*.so`, and `pg_extension`/`pg_available_extensions`/
`pg_settings` SQL. `pg_stat_kcache` facts:

```
postgresql-18-pg-stat-kcache|2.3.2-1.pgdg13+3|hold ok installed
/usr/share/postgresql/18/extension/pg_stat_kcache.control
/usr/lib/postgresql/18/lib/pg_stat_kcache.so
pg_stat_kcache|2.3.2
pg_stat_statements,auto_explain,pg_stat_kcache
pg_stat_kcache.track|all|f  (pending_restart=false)
```

Repack dry-run/actual versions and command:

```
client=pg_repack 1.5.3  extension=1.5.3  library=pg_repack 1.5.3  sql=pg_repack 1.5.3
pg_repack -u postgres -- /usr/lib/postgresql/18/bin/pg_repack \
  -h /var/run/postgresql -p 5435 -U postgres -d extlab \
  --table "public"."ext_repack" --no-kill-backend --wait-timeout=60
```

## Guest modifications and fixtures left behind

- Candidate binaries: `/tmp/vaire-extension-lab` (pre-fix), `/tmp/vaire-extension-lab-efin`
  (fixed overlay); `/usr/local/bin/vaire` is the fixed overlay for systemd jobs,
  with the pre-fix binary kept at `/usr/local/bin/vaire.pre-efin`.
- Databases: `extlab` (nano, port 5435; pg_stat_kcache, pgstattuple, pg_repack;
  test tables `ext_measure`, `ext_repack`, `ext_vec_dep`, `ext_autorepack`,
  `ext_autorepack2`), `extcron` (nano, 5436, pg_cron), `extcronfol`
  (independent after fixed unfollow, 5437), `extcroncopy` (fork of extcron,
  5438, pg_cron suppressed).
- Removed during testing: `extfollower` (recreated as a follower and dropped),
  `extsrc` and `extfol` (dropped after X6 to make room for the fork).
- Configuration: `maintenance.auto_repack=true`,
  `maintenance.window='15:00-16:00'`,
  `maintenance.repack_bloat_threshold=0.4`. Diagnostic grants on
  `ext_autorepack`/`ext_autorepack2` were revoked through normal SQL. New
  fixture `ext_final_auto` is owned by `extlab_owner`, has no explicit
  `vaire_admin` grant, and was auto-repacked on the pre-fix overlay. The
  polluted `extlab` requirement history is retained as EFIN12 evidence and was
  not edited.
- `app` (5433) and `other` (5434) data were not intentionally changed; their
  postmaster uptimes stayed unchanged across extension restarts.

## Limits

- Direct node RPC, not the signed CLI transport. SSH/WireGuard access was not
  repaired; the guest's current operators come from a different lab install.
- X2 used pg_stat_kcache (the run sheet's template) after verifying the actual
  Debian/ PGDG package; it was not a source build and no repository/key was
  added.
- X4 protected removal, X6 apply-bypass, X7 drill, X8 all boundaries, X9
  bucket/rebuild and X10 protected refusal remain unexecuted or BLOCKED.
- X10 successful rewrites are reported `completion_unknown`, as the contract
  requires; no claim of confirmed repack is made.
- The fixed-overlay result above is not a final candidate; final EFIN4/6/7/8/9/10
  and exact-scan reruns wait for the frozen candidate.
