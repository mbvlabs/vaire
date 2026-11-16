# Combined follower and extension qualification

**Preparation only — not execution-ready.** Current source acceptance and existing live evidence must be reconciled using the [finishing checklist](../specs/extensions-finish.md). This sheet records procedures, not proof that no later live testing occurred, and authorizes no live actions.

One verified signed edge candidate will qualify both features. Publication does not establish installation identity, deployment or a live pass. Activity AC-01 must pass, or the operator must explicitly accept its named gaps in MANUAL-TESTING before deployment. An activity-health read alone is insufficient.

**Authorized development lane (2026-11-16):** the user permits local candidate
builds and tests in the disposable Debian Firecracker VM at `~/work/vps-lab`.
Use its isolated operator state and lab-only fixtures, with recorded build
identity and bounded resources. These results can establish real package,
PostgreSQL and systemd behavior; they do not establish signed-release install/
upgrade or production deployment. The production admission requirements below
still apply to a later signed-candidate run. No production credentials, bucket
access or host resource destruction is authorized by the lab permission.

The command shapes below come from the implemented CLI metadata and source at the original documentation checkpoint. Revalidate them against the actual candidate, including the context-to-workspace CLI change. They are local preparation templates, not copy-pasteable live commands: every `<...>` value is unresolved until the admission table is filled from the actual published candidate, workspace, inventory, fixtures and approved budgets. Do not invent a workspace, host, database, table, package, bundle, threshold or budget to make a template look complete.

## Execution admission

Use one live owner and serial activity, no-bucket, then separately approved bucket/rebuild passes. This sheet retains both follower and extension coverage. The older separate follower run sheet was removed before this cleanup; its necessary observations and blocked boundaries are consolidated below. Dated results remain in [MANUAL-TESTING.md](MANUAL-TESTING.md), not in historical planning status.

Before any execution, record the candidate version/commit/channel, immutable artifact checksum/signature identity, operator/node installed identities, named context and target host, plan/doctor, database inventory/protection, recovery options, fixture absence and measured sizes, human permissions, evidence directory and cleanup scope. Context names and VPS recorded in the handoff are observations to revalidate, not permission to switch or repoint them.

Resolve approved write-row/batch/time/payload bounds, WAL-byte/time bounds, slot cap, disk reserve, stop duration and eligible resize sizes from actual inventory. No guessed defaults. Missing budgets block the affected case. Repack requires additional temporary disk headroom and the existing host reserve; exact measurement requires a full-scan time/I/O budget.

No source-built binary uploads, local harnesses, `ALTER SYSTEM`, generated-config/record hand edits, corruption, production fault hooks, repair commands or timing-only crash proof. Stop on unexpected data, WAL, slot, disk, restart, privilege or background-job effects, failed-job outcome uncertainty, breached budget or secret exposure. Cleanup, destructive fixtures, bucket changes and rebuilds need separately named permission. Record remaining resources if cleanup is not authorized.

### Admission values still unresolved

| Value | State |
| --- | --- |
| Candidate version, commit, channel, immutable artifact checksum/signature | **Unresolved.** Requires an authorized publication and signed-artifact verification |
| Operator and node installed identities | **Unresolved.** Revalidate after normal signed upgrade; no locally built binary |
| Named context and target host | **Unresolved.** Revalidate; do not reuse or repoint an old context |
| Fixture names, measured sizes, absence of unrelated data | **Unresolved.** Resolve from actual inventory |
| Write-row/batch/time/payload bounds, WAL byte/time bounds, slot cap, disk reserve, stop duration, eligible resize sizes | **Unresolved.** Require explicit approval; no guessed defaults |
| Repack temporary headroom and exact-scan time/I/O budget | **Unresolved.** Twice the measured table/index/TOAST footprint plus host reserve; one-table scan budget |
| Human permissions (sudo, TOTP, root key, provider console, rebuild, cleanup) | **Unresolved.** Name each separately |
| Activity gate (AC-01 pass or explicit named-gap acceptance) | **Unresolved** |
| Evidence directory and cleanup scope | **Unresolved.** One owner-only `/tmp/opencode/` run directory; durable narrative only in repository docs |
| X2 real non-built-in package or preinstalled fixture | Debian 13 lab verified `postgresql-18-pg-stat-kcache` 2.3.2-1.pgdg13+3; revalidate availability/runtime for the final candidate and target platform. Vector is built-in and is insufficient |

**This sheet is not execution-ready until every row above is resolved and the implementation, documentation and candidate pass the source-review and final-verification gates in the [finishing checklist](../specs/extensions-finish.md).**

## Preserved follower case map

The original case map below has no reconciled results here; blocked boundaries remain blocked unless actual supported evidence resolves them. Reconcile any later results before scheduling reruns. Requirements below were recovered from `8c11370:docs/RUN-phase-6-live.md`; old fixture names, host identities and command spelling are not execution values.

| ID | Case | Extension overlap / status constraint |
| --- | --- | --- |
| A0 | Upgrade and baseline | One combined candidate; existing restart condition is conditional |
| A1 | Source/auxiliary fixtures and bounded load | Keep fixtures isolated from unrelated extension effects |
| A2 | No-bucket basebackup follow | X2, X6, X7 copy/preload behavior |
| A3 | Read-only follower facts | X6 inherited intent and effective preload facts |
| A4 | Follower routing/read-only checks on three ports | X6; tunnel remains optional |
| A5 | Doctor/metrics and stopped/unknown diagnostics | X3/X6; unavailable is not absent/zero |
| A6 | Budget refusal without partial effects | X6 missing authorization also has zero effects |
| A7 | Follower/source guards and aggregate backup skip | X4/X6; protection independence remains A12 |
| A8 | Stopped follower and retained-slot pressure | X6 must not conceal stopped-target facts |
| A9 | Bounded paused replay and resume | Preserve original stop bounds |
| A10 | Resize/defer/follower-first restart and shrink refusal | X6 cannot alter floor formula or authorization |
| A10b | Privileged config-write failure retry | BLOCKED: no deterministic supported live mechanism |
| A11 | Fresh-floor refusal and supported retry | Follow/cleanup still separately approved |
| A12 | No-bucket unfollow/access retirement/protection | X7 launch suppression through independence |
| A13 | Pass A cleanup | Explicit permission; retain/report otherwise |
| B1 | Stanza-seeded follow under bounded load | X2/X6/X7; compare with A2 |
| B2 | Stanza-backed unfollow/first full/timers/access | X7 independent disabling settings |
| B3 | Source restore with follower/divergence/re-follow | X9 historical libraries and safety |
| B4 | Rebuild, follower-first refusal, source-first recover-all | X2/X3/X9 prerequisites and bundle history |
| B5 | Recovery copy interruption | Conditional: approved fresh rebuild and conclusive boundary evidence; otherwise blocked/inconclusive |
| B6 | Recovery adoption interruption | BLOCKED: no deterministic live boundary |
| B7 | Interruption before adopted marker | BLOCKED: no deterministic live boundary |
| B8 | Slot diagnostics/lost-unreserved distinctions | Retained-copy preview/recopy BLOCKED without proven owned awaiting fixture; missing-slot subcase needs destructive permission |
| B9 | Unreadable slot preview | BLOCKED: no supported isolated failure mechanism |
| B10 | Prepared follow generation/retry | BLOCKED: no deterministic pre-copy boundary |
| B11 | Unfollow target ownership after promotion | BLOCKED: no deterministic pre-rename boundary |
| B12 | Unreadable transition records fail closed | BLOCKED: corruption/unreadable-record technique not approved |
| B13 | Pass B cleanup | Explicit cleanup and rebuild-boundary permission |

### Follower observations and safety requirements

Use this table with the case map above, the admission table and candidate-specific
command discovery. It preserves required observations, not an executable script.
Source/follower/auxiliary/probe fixtures must be verified absent and sized from
the actual budget. Never reuse historical `app`/`other` names as permission.

| Cases | Required observations |
| --- | --- |
| A0 | Verify signed CLI/node identity, plan and pending restarts. Apply without tokens must not restart existing clusters. Pre-restart streaming needs an approved pre-existing source with a genuinely pending wal_level restart; a fresh fixture does not prove it |
| A1–A2 | Bound write rows, batches, payload and duration; record LSN/times and reserve. No-bucket follow must report `from=base_backup`, active `vaire_<follower>` slot, recovery state and measured catch-up lag. Confirm a pending source restart only with permission; verify effective wal_level/slot cap and continued streaming |
| A3 | Inspect actual follower config, floors, hot_standby, primary connection and slot facts without exposing credentials. No-bucket has no restore_command; a stanza-backed follower has the correct one in its managed config, never an inherited stale auto.conf value |
| A4 | Source read credentials can read the follower alias on transaction 6432, direct TLS 443 and session 5432. Direct writes fail read-only; operator --write refuses `is_follower`. Optional tunnel is additional coverage, not a substitute. Never put passwords in argv/evidence |
| A5 | Record doctor follower checks/backup skips, status/list and metrics. Exercise both running, source stopped, follower stopped and both stopped within approved bounds. Source-known and self-known are independent; unknown facts/lag are omitted, not zero. Stopped list rows retain follows identity. NULL slot allowance is not proof of uncapped; only proven uncapped reports -1/true, measured zero/negative remains factual |
| A6 | An actually over-budget request without overcommit refuses with `memory_overcommitted`, a fitting size and actionable hint, before entry/slot/copy effects. Another refusal does not prove this case; no safe over-budget request means BLOCKED |
| A7 | Source drop refuses `has_followers`; follower backup, restore, fork, credential rotation, write maintenance and operator --write refuse `is_follower`; follow-of-follower refuses `follows_follower`; aggregate backup skips followers. Supply applicable tokens so guards, not missing authorization, win. Source still serves after refusals |
| A8–A9 | Bound stopped-follower/replay-pause time, writes/WAL and disk. Observe source slot retention independently of stopped self facts; paused replay is visible and doctor streaming fails. Resume/start and verify catch-up. Stop early on unsafe allowance/disk; do not force a lost slot |
| A10 | Select budget-safe growth/shrink sizes. Deferred growth writes follower config first and restarts nothing; where needed complete follower restart before source. Separately approved growth with all tokens proves ordered execution. Shrink refuses `has_followers` before mutation; absent safe sizes mean BLOCKED subcases |
| A11 | Use a genuinely absent probe and source running floors above its proposed floors. Dry-run refusal must name executable config-set commands on the existing source. Record original values/absence, apply the hints without restart, retry admission, and restore exact original semantics. Dry-run admission alone does not prove real follow completion |
| A12 | Verify source protection versus independently unprotected follower, unfollow promotion, both-pooler aliases, slot removal, retired source credentials on the copy, new copy credentials and a migration. Source remains unchanged. Creation-after-protection needs its own follow fixture; protecting an existing follower's source does not prove that variant. Human TOTP/protection changes are separate |
| B1 | With a verified full backup and bounded writes, follow must report `from=stanza`; fallback is not a pass. Repeat streaming, aliases, configuration and independent-known/unknown diagnostics from A2–A5 |
| B2 | Unfollow gets its own stanza, successful first full and timers. Compare rows/sequences at promotion, verify new access and source credential refusal, source slot removal and migration. first_backup_failed or missing timers is a failure, not a partial pass |
| B3 | Use a verified restore window and approved target. Source restore/promotion warns `followers_diverged`; follower serves the old timeline read-only and doctor fails streaming. Approved drop/re-follow restores streaming. Protected variant needs human unprotect/reprotect permission |
| B4 | Each approved rebuild needs verified backup/root access and a complete resolved command. Both entries become awaiting; follower recovery first refuses `source_awaiting_recovery`; recover-all runs source first, then follower, and takes a full only of source |
| A13/B13 | Stop writers; restore exact temporary setting values/absence, protection and pending work through supported interfaces. Drop followers before sources only with permission; bucket objects/rebuild cleanup are separately scoped. Record remaining databases, slots, stanzas, timers, routes, host and evidence resources |

### Fault boundaries that remain conditional or blocked

- **A10b:** root performs configuration writes; making postgres's conf.d
  unwritable does not prove a writer failure. No supported deterministic live
  failure mechanism is established. Keep BLOCKED; use source regression evidence.
- **B5:** requires its own fresh approved awaiting-recovery fixture. A `copying`
  marker precedes slot preparation and can outlive copying. Only post-termination
  artifacts/progress that distinguish the actual stopped phase prove the copy
  boundary. Otherwise record INCONCLUSIVE even if retry works. Valid evidence
  must show awaiting persists, dry-run plans partial-copy removal rather than
  unsafe reuse, and supported retry restores config/slot/streaming.
- **B6/B7:** adoption and pre-adopted-marker boundaries lack deterministic
  supported live observation. Timing-only kills, hooks or record edits are not
  substitutes. Adopted-copy reuse versus recopy remains live-pending.
- **B8:** start with a proven materialised streaming disposable follower and
  existing slot, not merely a rebuilt awaiting entry. Capture exact desired cap
  presence/value, inherited value, effective value and slot facts. Use supported
  config/apply for an approved temporary cap, verifying no unapproved restart/
  floor effect; stop the follower and generate only approved WAL/time while
  monitoring reserve. At most one separately approved checkpoint may aid
  observation; do not loop. Record reserved/extended, unreserved, lost or missing
  as actually observed, never relabel. Restore original value or absence through
  supported config, then verify effective inheritance. Missing-slot diagnostics
  require separate destructive approval for the exact fixture slot/source
  cluster endpoint, followed only by approved public drop/re-follow cleanup.
  Do not repair slots manually. Retained-copy preview/recopy remains BLOCKED
  until completed owned-copy progress plus awaiting state is proven; stopped
  streaming followers or copying markers do not establish that setup.
- **B9:** no supported isolated slot-unreadable mechanism; stopping a source or
  revoking access does not prove the intended conditional-resume path.
- **B10:** prepared-entry-before-copy interruption has no supported deterministic
  live boundary. General apply must not initialize it; source regression is
  evidence, not a live pass.
- **B11:** post-promotion/pre-rename unfollow interruption likewise lacks a
  deterministic live boundary. Ownership refusal/retry remains source evidence.
- **B12:** making records unreadable/corrupt is not approved. A missing records
  file is normal initial state, not this fault. Fail-closed routing regressions
  do not certify a live filesystem failure.

Every case result must include permission/bounds, exact resolved commands and
UTC times, workspace/host and binary identity, job/unit IDs, exit/first failure,
actual JSON/SQL/runtime observations, cleanup or remaining resources, and
PASS/FAIL/INCONCLUSIVE/BLOCKED/NOT RUN. A healthy command or existing procedure
does not prove the intended boundary. Record results in MANUAL-TESTING.

## Added extension coverage

Final signed-candidate qualification remains pending. The [current-candidate checkpoint](MANUAL-TESTING.md#current-candidate-clean-slate-linux-lab--2026-11-16) records clean-slate Debian CLI/WireGuard and native PostgreSQL/systemd passes plus remaining limits; it does not blanket-pass this procedure. Command templates require candidate-specific revalidation; placeholders and admission values are not approved execution values. A shared lab must have an explicit ownership handoff before use; availability never implies permission to reset another agent's disk.

| ID | Prerequisites / permissions | Required observations | Boundaries and cleanup |
| --- | --- | --- | --- |
| X1 | Disposable independent primary; verified vector package/control facts; add permission | Alias persists as vector; installed/default versions and provider factual; no unrelated restart | Bounded fixture/data; removal needs database token; packages retained |
| X2 | Actual compatible non-built-in package in configured trusted repo, or operator-preinstalled extension; explicit complete descriptor; installation permission | Custom definition durable; install/control/library facts; A2 and B1 use same resolver; rebuild prerequisites honest | pg_stat_kcache verified in the Debian development lab; revalidate the final platform/candidate. Vector is insufficient proof |
| X3 | Approved absent-repository or absent-preinstalled fixture without changing trust config | Actionable prerequisite; unknown is not absent; awaiting recovery preserved; no automatic repo/key setup | No destructive repository edits; use safe verified absence; cleanup only if authorized |
| X4 | Disposable unprotected primary; owned-data/dependent-object fixture; remove/cascade permission | Bounded preview with count/truncation; no implicit cascade; protected remove refuses; SQL-absent reconciliation retries | Database token required; preserve source jobs/data; explicit destructive fixture cleanup |
| X5 | One preload target plus unrelated serving database; restart permission | Missing target token has zero effects; exact target restart only; effective preload before CREATE; pending roles retry | Enumerated cluster tokens, never broad server substitute; bounded outage; retain packages |
| X6 | Source and followers from A2/B1; current membership and restart inventory; all target permissions | Each missing follower token refuses with zero effects; direct follower intent refused; followers configured/restarted first; primary SQL only; plain config/apply cannot bypass | Current membership under lock; no tuning-floor changes; unrelated clusters untouched; partial outcomes exact |
| X7 | Safe cron job fixture with bounded externally observable effect; fork/drill/unfollow permissions; custom disabled-settings fixture if available | No copied launches from first start through promotion/rename/settlement; copied jobs inactive and correctly retargeted; source unchanged; independent disabling settings durable | Budget job effects and observation time; do not deliberately run unsafe jobs; no timing-only proof of crash safety |
| X8 | Supported deterministic failure boundary and approved bounded fixture | Exact completed/pending targets; re-add finishes intent-equal pending work; remove retries after DROP; no false rollback/nothing-changed claim | BLOCKED where no supported mechanism exists; no production hooks or corrupt files; preserve A10b/B6–B12 limits |
| X9 | Known backup coverage start, extension-bearing historical point then removal; separate bucket/rebuild permission; bundle and provider inventory | Removed requirement survives stanza-bound records and rebuild; absent recovery declaration refuses unknown point; explicit empty/populated remain distinct; incompatible requirements fail before start | Never attach history by reused database name; missing repos/files remain prerequisites; no fixture destruction without permission |
| X10 | Eligible permanent ordinary table/key; matching executable/library/SQL versions; manual token/window or now permission; opt-in automation; measured space | Protected repack refuses supported limitation; no backend killing; busy/window/failure outcomes accurate; one table at a time; per-run counts accurate | Twice table+indexes headroom plus host reserve; bounded writes/lock wait; window stops new work, not unreviewed cancellation |
| X11 | Installed pgstattuple; one supported ordinary table; monitor permissions; approved scan budget | Full `pgstattuple`, not approximate; measured live/dead/free bytes/percentages and method; timeout/permission failures honest | Full scan/read-lock and concurrent-update caveats; no database-wide scan, exact index or reclaimable-bloat claim |

### Prepared command templates

These are local preparation templates. Resolve every placeholder before execution; no command below is approved as written.

**X1 — built-in vector.**

```sh
vaire ext list --db <db> --json
vaire ext add pgvector --db <db> --json
vaire ext list --db <db> --json
vaire ext remove vector --db <db> --confirm <db> --json
```

Expected: `pgvector` is accepted as an alias and persisted as `vector`; `postgresql-18-pgvector` and its control facts are reported; installed/default versions are factual; no unrelated cluster restarts; removal keeps the package. Reference: `internal/cli/ext.go`, `internal/node/extensions.go`, `EXTENSION-DEFINITIONS.md`.

**X2 — real custom non-built-in extension.**

Candidate APT shape, only if package availability and runtime behavior are verified on the target:

```sh
vaire ext add pg_stat_kcache --db <db> \
  --package postgresql-18-pg-stat-kcache \
  --preload pg_stat_kcache \
  --setting pg_stat_kcache.track=all \
  --copy-safety safe --standby-safe \
  --confirm <db>/restart --json
vaire ext list --db <db> --json
```

Preinstalled alternative shape, only with files already installed by the operator:

```sh
vaire ext add <operator_ext> --db <db> --preinstalled \
  --preload <library> \
  --copy-safety safe --standby-safe \
  --confirm <db>/restart --json
```

Expected: the definition is durable in `[extensions.<name>]`; package/control/library facts are reported; A2/B1 use the same resolver; rebuild prerequisites are honest. The development lab verified pg_stat_kcache 2.3.2 from `postgresql-18-pg-stat-kcache` on Debian 13; final candidate, other-platform, stanza-backed follower and rebuild coverage remain separate. Pgvector is built-in and does not prove general custom support.

**X3 — missing repository or preinstalled files.**

```sh
vaire ext add <missing_ext> --db <db> --package <package-not-in-configured-repositories> --json
vaire ext add <missing_preinstalled> --db <db> --preinstalled --json
```

Expected: an actionable missing repository/package/file prerequisite before any unsafe copy or SQL; unknown facts are not reported as absent; awaiting-recovery state is preserved; vaire does not add a repository, key or source build. Use a safely verified absence, never a destructive repository edit.

**X4 — dependencies and explicit cascade.**

```sh
vaire ext add <extension_with_dependents> --db <db> --json
vaire ext remove <extension_with_dependents> --db <db> --confirm <db> --json
vaire ext remove <extension_with_dependents> --db <db> --cascade --confirm <db> --json
```

Expected: the preview shows at most the first 50 dependent objects and the extension's own tables with total counts/truncation; dependent objects block without `--cascade`; no implicit cascade; protected removal refuses; a later retry after SQL-absent reconciliation does not repeat an already completed DROP.

**X5 — isolated preload restart and effective preload.**

```sh
vaire ext add pg_cron --db <db> --json
vaire ext add pg_cron --db <db> --confirm <db>/restart --json
vaire ext remove pg_cron --db <db> --confirm <db> --confirm <db>/restart --json
```

Expected: the first add without the restart token has zero effects; only `<db>` restarts with its token; the effective preload is loaded before `CREATE EXTENSION`; a pending restart leaves roles work incomplete and a later confirmed retry finishes it. An unrelated serving database must not restart.

**X6 — source and follower propagation.**

```sh
vaire ext add <ext> --db <source> \
  --confirm <source>/restart --confirm <follower>/restart --json
vaire ext remove <ext> --db <source> \
  --confirm <source> --confirm <source>/restart --confirm <follower>/restart --json
vaire ext add <ext> --db <follower> --json
vaire apply --db <source> --confirm <source>/restart --confirm <follower>/restart --json
```

Expected: omitting either cluster's exact token refuses with zero effects; a source token never authorizes a follower restart; direct follower mutation refuses; followers are configured/restarted first; primary SQL is the only SQL; ordinary config/apply cannot bypass missing follower runtime readiness. The target set and current membership are reloaded under the existing locks.

**X7 — copied cron suppression through independence.**

```sh
vaire ext add pg_cron --db <db> --confirm <db>/restart --json
vaire db fork <db> --name <copy> --json
vaire restore drill --db <db> --json
vaire db unfollow <copy> --confirm <copy>/unfollow --json
```

Expected: no copied `pg_cron` launch occurs from first start through promotion, rename and privilege settlement; copied jobs are inactive and retargeted to the owned copy using actual metadata identity (including after rename/OID changes); the source's jobs are untouched; independent copies keep disabling settings until a deliberate operator change. Use only a safe fixture job with bounded, externally observable effects; no timing-only crash proof.

**X8 — partial failure and retry.**

```sh
vaire ext add <ext> --db <db> --confirm <db>/restart --json
vaire ext add <ext> --db <db> --confirm <db>/restart --json
vaire ext remove <ext> --db <db> --confirm <db> --json
vaire ext remove <ext> --db <db> --confirm <db> --json
```

Expected: exact completed/pending targets; re-add finishes intent-equal pending SQL or a supported update; remove retries after a successful DROP; no false rollback or “nothing changed” claim; packages remain installed. Where no supported deterministic failure boundary exists, the case stays **BLOCKED**; do not add production hooks, corrupt records or use timing alone.

**X9 — historical requirements, removal and rebuild.**

```sh
vaire ext add <ext> --db <db> --confirm <db>/restart --json
vaire backup now --db <db> --type full --json
vaire ext remove <ext> --db <db> --confirm <db> --json
vaire config get clusters.<db>.recovery_extensions --json
vaire config set clusters.<db>.recovery_extensions '[]' --json
vaire server init --restore --host root@<vps> --bundle <bundle> --confirm <server>/replace
vaire db recover <db> --json
```

Expected: the removed requirement survives in stanza-bound `extension_requirements`; coverage is checked against the selected base-backup chain; an absent declaration refuses the unknown older point, explicit `[]` asserts completeness, and a populated list resolves through the same definitions; incompatible provider/settings history fails before the copy starts; an unbound dump is never attached by a reused database name. Bucket/rebuild and fixture destruction need separate permission.

**X10 — manual and automatic repack.**

```sh
vaire ext add pg_repack --db <db> --json
vaire maint repack --db <db> --dry-run --json
vaire maint repack --db <db> --table <table> --now --confirm <db> --json
vaire job follow <job-id> --json
vaire maint repack --db <protected_db> --table <table> --confirm <protected_db> --json
vaire config set maintenance.auto_repack true --json
vaire config set maintenance.repack_bloat_threshold <approved-ratio> --json
vaire ssh --command 'sudo vaire node run maint-weekly'
```

Expected: protected repack refuses on every path with the supported limitation; a missing extension/client is reported before the destructive token; the dry run discloses the deferred library/SQL version check; only an ordinary permanent heap table with a primary key or suitable unique index is selected; table/TOAST/index and database/session defaults must be `pg_default`; free space is twice the table/index/TOAST footprint plus the host reserve; `--no-kill-backend` and the 60-second lock wait are used; busy/window/failure outcomes are accurate; auto-repack is off by default, acts at 64 MiB and the approved ratio, biggest first, one table at a time, and starts nothing after the window ends without cancelling a running rewrite. Any attempted table whose rewrite completion cannot be attributed is `completion_unknown`, never confirmed `repacked`; a skip requires a known diagnostic with identical valid storage facts.

**X11 — measured one-table exact report.**

```sh
vaire ext add pgstattuple --db <db> --json
vaire maint report bloat --db <db> --table <table> --exact --json
```

Expected: the full `pgstattuple` function (never `pgstattuple_approx`) measures one supported ordinary table as `vaire_admin` with `pg_monitor`; the result carries measured table/live/dead/free bytes and percentages plus `method`; the report uses the 5-second lock and 60-second statement timeouts; permission and timeout failures are honest; it does not claim instantaneous results, reclaimable bloat, index coverage or a database-wide scan.

### Targeted qualification notes

- **Literal versus raw settings.** Descriptor settings are configuration literals; `[postgres.settings]` and `[clusters.settings]` are raw values the renderer quotes. Prove a descriptor value `off` and a raw override whose literal content is `'off'` stay distinct through CREATE/UPDATE readiness and follower verification; no double decoding.
- **Settings-only readiness.** A reload-only required setting that is not yet effective defers dependent SQL without inventing a restart token; an unrelated pending PostgreSQL setting does not block unrelated role/grant work.
- **Update effectiveness.** A supported re-add/`ALTER EXTENSION UPDATE` must check the actual effective primary requirement before running and report the actual resulting version. No package upgrade or extension downgrade is performed; an unsupported or direction-unknown path refuses.
- **Strict restore fence.** Restore promote/undo and unfollow must withdraw both pooler aliases before exposing the target, keep SQL access fenced until writable availability, and resume an existing validated phase on retry rather than rebuilding expectations against its own swapped state.
- **Unknown repack outcome.** Clean exit, a changed OID/relfilenode or a storage change never proves a rewrite. Every attempt that cannot be attributed is reported `unknown`/`completion_unknown`; `repacked` is not claimed from external observables. A skip is only a known client diagnostic with valid, identical before/after storage facts.

## Evidence and finalization checklist

For each case record PASS, FAIL, INCONCLUSIVE, BLOCKED or NOT RUN with UTC times, installed identity/context, actual commands, sanitized observations/job IDs, budget use, cleanup and remaining fixtures in MANUAL-TESTING. Raw non-secret logs belong in one owner-only `/tmp/opencode/` run directory; durable narrative stays in repository docs.

Before execution readiness: freeze implementation and documentation after the two whole-change reviews and fresh final checks; fill every command placeholder from implemented metadata and actual inventory; identify and approve the X2 real custom fixture; resolve every admission value, budget and permission; map local regressions and reviewer findings; preserve historical statuses. Ubuntu amd64 evidence does not prove Debian/arm64 installs or macOS live operation. Beta/stable release remains separate and separately authorized.
