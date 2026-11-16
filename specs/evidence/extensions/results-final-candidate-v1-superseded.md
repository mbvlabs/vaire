# Final candidate V1 Linux VM results (superseded, not final acceptance)

V1 source freeze: 2026-11-16T16:39:17Z. This V1 manifest digest is historical;
the source has since reopened for fixes and this digest is not the current tree.
Source manifest digest:
`0cf11932fee499fb15748e43c6af9d14a9a62d7e04dccb8c488adfd0069fa272`.
`sha256sum -c specs/evidence/extensions/final-source-manifest.sha256` passed
before and after the build. Source was not edited.

- Build: `bin/vaire-extension-final`
- Identity: `vaire dev (84f71bbaa180-ext-final-0cf11932fee4, dev)`
- SHA-256: `fc3ee76d6f9ac8797e5536f88d0802b2971461a3ff7b875a549ddc300f6dc012`
- Guest: final binary installed as `/usr/local/bin/vaire` for systemd jobs and
  copied to `/tmp/vaire-extension-final` for direct node RPC. Previous fixed
  overlay saved as `/usr/local/bin/vaire.efin-overlay`; pre-EFIN binary as
  `/usr/local/bin/vaire.pre-efin`.
- Parent's V1 lane A later blocked this candidate for EFIN14/EFIN15; the live
  startup-order failure is now attributed to EFIN16 (owned guard order), which
  is distinct. These results are diagnostics for V1 only; they are **not**
  final acceptance.

## V1 case observations

| Case | V1 result | Sanitized observation |
| --- | --- | --- |
| EFIN4 | **PASS** | Stopped `finalsrc` and `finalfollower` with `pg_ctlcluster`, then normal `apply --db finalsrc` with no restart tokens returned rc=0 at 16:48:46Z; both clusters were `online` afterwards (`finalsrc` online, `finalfollower` online,recovery). No injected records or hooks. |
| EFIN6 | **PASS** | Set global `postgres.settings.random_page_cost='1.10'`, `hash_mem_multiplier='2.0'`, `client_min_messages='WARNING'`; `apply --db finalsrc` and then `ext.add pgvector --db finalsrc` succeeded (vector 0.8.7 on source and follower). Actual settings were canonical `1.1`, `2`, `warning`; unrelated numeric/enum values did not block extension readiness. |
| EFIN7 | **FAIL / EFIN16 order observed** | Set `clusters.finalsrc.extensions=['pg_stat_kcache','vector']` (custom-only, no pg_stat_statements), then `ext.add pgstattuple` with source/follower restart tokens. Intent saved; follower restart failed: `FATAL: unrecognized configuration parameter "pg_stat_statements.max"`, hint to load pg_stat_statements before pg_stat_kcache. The follower was left down (`down,recovery`), command rc=5 `extension_apply_failed`, pending `finalfollower`. Read-only follow-up attributes the observed startup order to the owned guard in `postgresql.auto.conf` (EFIN16), not to EFIN14 primary readiness. Not a pass. |
| EFIN8 | **PASS** | `ext.add pg_cron --db extcron --schema public --copy-safety disabled --standby-safe` was refused with `extension_schema_mismatch`: actual_schema=`pg_catalog`, requested_schema=`public`, rc=11. `/etc/vaire/cluster.toml` hash was unchanged, no SQL/grant ran, and `pg_cron` still reported `pg_catalog`. |
| EFIN12 | **PASS** | On fresh `finalsrc` stanza (port 5437), two failed hypopg adds were run: preinstalled missing control and apt `postgresql-18-hypopg-nonexistent`. Both returned rc=5 `package_install_failed`, `intent_saved=false`, with config hashes unchanged. The fresh requirement history contained only `auto_explain` and `pg_stat_statements` (no hypopg providers). `db.follow finalsrc -> finalfollower` then succeeded: base_backup, streaming, slot `vaire_finalfollower`. Old polluted `extlab` was left untouched. |
| X1 | **PASS** | `ext.add pgvector --db extlab` required `extlab/restart` because the lab cluster had pending extension configuration; with that token it returned vector 0.8.7, package `postgresql-18-pgvector`, no restart. Removal occurred through the X4 cascade below; package retained. |
| X2 | NOT RERUN on V1 | Baseline/overlay evidence already has real `postgresql-18-pg-stat-kcache` 2.3.2 install, descriptor, control/library and effective preload. V1 was not used for another provider install. |
| X4 | **PASS** | Created `final_vec_dep.v vector(3)` owned by `extlab_owner`; `ext.remove vector` without cascade refused `extension_in_use` with one dependent and zero own tables; with `cascade:true` it succeeded and kept `postgresql-18-pgvector`. |
| X5 | NOT RERUN on V1 | Baseline/overlay evidence covers missing-token refusal, isolated restart and effective preload. No V1 isolate-token run was made. |
| X6 | **PASS** | `finalsrc`/`finalfollower`: dry run listed `finalfollower/restart` and `finalsrc/restart`; source-only token refused before any config change (hash unchanged); both tokens configured/restarted follower first (`postgres-config-v1@finalfollower` at 16:44:53Z) then source (`postgres-config-v1@finalsrc` at 16:44:57Z); both reported pg_stat_kcache 2.3.2 and effective preload; direct follower `ext.add` refused `is_follower`; normal `apply --db finalsrc` with no tokens returned rc=0. |
| X7 | NOT RERUN on V1 | Baseline+EFIN1/EFIN11 overlay proved real cron fork/unfollow with live `pg_cron launcher` drain and source unchanged. V1 did not repeat the cron fork/unfollow on the frozen binary. |
| X10 | NOT RERUN on V1 | Baseline/overlay manual and auto repack evidence exists; V1 did not repeat owner-created manual/auto repack. |
| X11 | **PASS (functional)** | New owner-created `final_measure` (5,000 rows, extlab_owner) exact report returned `method=pgstattuple`, `table_bytes=1523712`, `tuple_count=5000`, `tuple_bytes=1460000` (95.82%), dead 0, `free_bytes=19248` (1.26%). EFIN9's final OID-pin claim remains subject to V2. |

## EFIN14 / EFIN15 / EFIN16

- **EFIN16 is the observed V1 startup-order failure.** The managed
  `30-vaire-extensions.conf` correctly writes
  `pg_stat_statements,auto_explain,pg_stat_kcache` on both source and follower.
  The follower's owned guard in `postgresql.auto.conf` overrides it with the
  lexically sorted list `auto_explain,pg_stat_kcache,pg_stat_statements`; the
  guard wins because auto.conf is read later. `pg_stat_kcache` therefore loads
  before `pg_stat_statements`, producing the native FATAL unless an explicit
  `pg_stat_statements.max` placeholder exists. Exact read-only facts:
  `extension-preload-order-readonly.md`.
- **EFIN14 is distinct (source pending SQL/readiness).** It remains the V1 lane
  A source issue and is not established by the follower startup FATAL above.
  The V1 report previously used one attribution for both; this correction keeps
  the original recovery record but names EFIN16 as the live startup-order root.
- **EFIN15 not exercised:** no safe copy activation with a `copy_settings`
  unit-value fixture (`work_mem='1024kB'` vs actual `1MB`) was created. Marked
  BLOCKED/not run rather than synthetic.

## Recovery and bound

- `finalfollower` was recovered with a bounded native start override
  (`pg_ctlcluster 18 finalfollower start -- -o '-c
  pg_stat_statements.max=10000 -c pg_stat_statements.track=top'`), then
  `apply --db finalfollower --steps postgres-config-v1@finalfollower`
  completed and wrote `pg_stat_statements.max=10000`. The manual postmaster was
  then stopped with `pg_ctl -m fast` and restarted normally through
  `pg_ctlcluster 18 finalfollower start`; it is now `online,recovery` under
  systemd with the written configuration. This is a lab recovery action, not a
  candidate PASS.
- Global settings remain on the lab node for the V1 diagnostic:
  `random_page_cost='1.10'`, `hash_mem_multiplier='2.0'`,
  `client_min_messages='WARNING'`.
- `finalsrc`/`finalfollower` selection includes
  `pg_stat_statements, pg_stat_kcache, vector, pgstattuple` after recovery.
- Fresh fixtures left: `finalsrc`, `finalfollower`, `final_measure`,
  `final_vec_dep`; `extlab`, `extcron`, `app`, `other` retained. `app`/`other`
  data was not intentionally changed.
- All X/RPC runs used the direct production dispatcher; the signed CLI
  transport remains unqualified. Raw streams were not added to the repository;
  only explicit-field filtered results are recorded here.

## Verdict

V1 is a **superseded diagnostic candidate**, blocked by the live EFIN16 guard
order failure plus the separate EFIN14 source issue and pending EFIN15. It is
not a final acceptance run. Final claims wait for the parent's V2 manifest/build
and a fresh ungranted app-owned rerun.
