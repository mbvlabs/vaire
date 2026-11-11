# Phase 7 implementation record

**Local source implementation through E7-06 has stage acceptance. No publication or live qualification is claimed.** Approved scope is the extension edge plan and execution handoff. This record describes current implementation evidence; final cumulative acceptance, publication and live qualification remain pending.

## Outcomes

- A helpful built-in catalogue plus explicit compatible APT or operator-preinstalled custom extensions. No source builds or automatic alternate repository/key configuration.
- One config-aware resolver for runtime/provider/safety metadata, deterministic rendering and durable recovery declarations.
- Targeted package prerequisites, privileged SQL creation with protected access preserved, explicit restart authorization and truthful retryable partial outcomes.
- Source/follower propagation, historical backup requirements and suppression of copied background jobs across every activation boundary.
- Detachable manual repack, opt-in scheduled repack and one-table full-scan `pgstattuple` measurements.

## Work and current evidence

| Work | State | Evidence / remaining gate |
| --- | --- | --- |
| E7-01 checkpoint and selective inventory | Complete | Plan digest, local/remote base and selected snapshot fingerprints recorded; snapshot unchanged |
| E7-02 catalogue/config/render foundation | Accepted locally | Independent targeted source acceptance and fresh required checks; custom safety/provider validation, recovery presence and bundle roundtrips tested |
| E7-03 package/roles/prestart foundation | Accepted locally | Independent targeted source acceptance; runtime versus SQL requirements separated, implicit preload-only defaults guarded, incomplete SQL/restart retry tested |
| E7-04 commands/list/doctor/activity | Accepted locally (standalone) | Independent source acceptance and fresh required checks; global scope, current-hash retry, provider safety, version direction and removal authorization regressions passed |
| E7-05 followers/history/copy activation | Accepted locally by both cumulative source lanes | Both independent stage lanes accept the corrected source; fresh required checks, uncached tests and formatting pass on the stable complete manifest; runtime/crash qualification remains separate |
| E7-06 maintenance | Accepted locally | Targeted source review accepts maintenance and the final skip-identity correction; full final verification remains required |
| E7-07 canonical docs/combined run sheet | Candidate complete; pending final reviews | Existing follower case IDs and blocked cases preserved; run sheet carries implemented command shapes but remains preparation-only, not execution-ready |
| E7-08A/B cumulative reviews | Not run | Two independent whole-change verdicts required after integrated freeze |
| E7-09 final verification | Not run | Stage tests are not a substitute for fresh final-tree checks |
| E7-10 publication | Not authorized / not run | No commits, push, PR, merge or signed Phase 7 candidate |
| E7-11 combined qualification | Not authorized / not run | Activity gate, named contexts, deployment and bounded fixture permissions remain separate |

## Accepted foundations and remaining obligations

Custom definitions are privileged trusted-native-code intent, not a sandbox or automatic compatibility proof. Canonical names are distinct from SQL/control identifiers. Provider, preload and setting conflicts are validated including implicit defaults. Absent recovery requirements differ from an explicitly empty completeness declaration through editing, cloning and real synthetic master-bundle restore transformations. Retained requirements are bound to the immutable stanza and cover only the UTC range vaire observed; an unbound dump is never associated by a reused database name.

Runtime prerequisites include preload-only defaults; SQL creation does not. Selected configuration, cluster, roles and authorized restart boundaries prepare only the target's requirements before effects. Full package convergence retains its intentional host-wide union. Package failures can leave installed packages and never trigger uninstall. Actual effective preload and setting facts control deferred CREATE/UPDATE and incomplete journal state; later confirmed restart/retry finishes work without false completion. Descriptor settings are configuration literals; `[postgres.settings]` and `[clusters.settings]` are raw values decoded once through the same expected-value path, so literal quote content stays distinct.

Historical requirements and ordered source/follower execution have local source acceptance. Known older backups need explicit completeness declarations where observed coverage cannot prove their base-backup chain. Winning guards, durable instance/stanza/operation-bound activation checkpoints and effective settings gate dependent SQL and both poolers; direct commands require exact tokens before mutation while ordinary apply preserves safe deferred staging. Copied cron jobs are suppressed continuously through promotion/rename/settlement and deactivated/retargeted by actual metadata identity. Unknown provenance, unreadable facts or an unproven handoff refuses rather than being certified. These guarantees still require the designated live qualification and final whole-change review after maintenance integration.

Repack and auto-repack are implemented locally. Protected databases are refused on every path; ordinary permanent heap tables need a primary key or suitable unique index and `pg_default` storage, with twice the table/index/TOAST footprint plus the host reserve. No backend is killed. Every attempted table whose rewrite completion cannot be attributed is reported `unknown`/`completion_unknown`, never confirmed `repacked`; a skip requires a known diagnostic with identical valid storage facts. The exact report measures one table with the full `pgstattuple` scan under the report's 5-second lock and 60-second statement timeouts, not approximate or reclaimable-bloat figures.

## Verification and coverage limits

Detailed UTC command results, exact tree identities and all earlier failures/retries are retained in [STATE-phase-7-preserved-work.md](STATE-phase-7-preserved-work.md). Findings and dispositions are in [FINDINGS-phase-7-review.md](FINDINGS-phase-7-review.md). Stage build, vet, unit tests, Go/repository lint and Darwin arm64 builds have passed their accepted checkpoints.

An intermittent lock-timeout failure in unmodified activity concurrency tests occurred on some uncached runs and passed isolated/full retries. Preserve it as a contention/flake risk; final verification remains fresh and mandatory. No APT/PostgreSQL/systemd/live lifecycle qualification has occurred. Simulated callbacks and synthetic bundles are not process-death or installed-package evidence.

The [combined live run sheet](RUN-phase-6-7-live.md) is preparation only. It preserves all follower cases and blocked fault-boundary cases, plus extension/maintenance coverage. A real non-built-in compatible package/preinstalled fixture still needs verification. Linux/macOS cross-builds do not establish macOS live behavior or the complete supported server OS/architecture package matrix.
