# Finish extensions

## Completion checkpoint — 2026-11-16

The corrected Linux source passed two independent whole-change reviews and
fresh full local verification on the exact 687-file manifest below. Bounded
clean-slate Debian lab cases passed through real CLI/WireGuard, PostgreSQL and
systemd. Final reports, source verdicts and remaining limits are in
`specs/evidence/extensions/`; the evidence hygiene audit accepted all 86
inspected artifacts, including final and historical blocker reports.

The lab test cron job was unscheduled, its own sleep backend finished
naturally, and the temporary transfer server is stopped. Test databases remain
online for inspection; no further lab work is running.

Source integration proceeds through the feature PR. Signed publication and
production deployment are not authorized. Strict lint remains red at 142
findings versus baseline 131; macOS and the complete runtime qualification
matrix remain deferred/incomplete.

## Historical resume checkpoints

The notes below preserve the investigation and coordination history. Earlier
“pending”, “blocked”, “on hold” and agent ownership statements describe their
named candidate/stage; the completion checkpoint above and final reports
supersede them.

**Goal:** finish the authorized Linux extension feature, clean up obsolete docs,
and integrate corrections into `master` through a PR. **Not finished or merged.**

### Boundaries

- Work only in `/home/mbv/work/vaire-phase-7`, branch
  `feat/phase-7-extensions`. Main `/home/mbv/work/vaire` contains only the user's
  staged `justfile`; do not stage, modify or discard it.
- Main and feature HEAD were both `84f71bbaa1804fb9b7d581ca8a109192dc6375a9`;
  feature code was already in master. All current finishing changes are
  uncommitted; no push, PR or merge performed. Remote master was rechecked at
  that revision; revalidate before integration.
- **No access to `~/work/eregion`.** Parent's attempted read-only status was
  declined and was not retried; agents explicitly scoped away from it.
- User authorizes disposable Firecracker testing in `~/work/vps-lab`.
  No production VPS, production credentials or external bucket access.
- **New user requirement:** notify the user before starting any VPS-lab
  checks. Live worker is explicitly on hold; a new source freeze alone is not
  clearance. Current work is local source fixes and verification only.
- User explicitly defers macOS and broad lint refactoring. Do not weaken rules
  or claim green lint/publication. Existing edge workflow remains blocked by
  strict lint and multi-platform build failures even after source integration.

### Current candidate state

- **Current candidate accepted and bounded clean-slate Linux lab cases passed.**
  Both independent whole-change source reviews ACCEPT; fresh full uncached
  tests/build/vet/static checks passed with all 687 source hashes unchanged.
  Sanitized source verdicts: `evidence/extensions/review-final-candidate.md`.
- Clean Debian 13.7/PostgreSQL 18 provisioning succeeded through normal
  `server init`; **CLI over WireGuard worked** in this run. The earlier
  node-RPC-only transport limitation applies only to historical runs.
  Final case evidence: `evidence/extensions/results-final-candidate-v3.md`.
- Native-default bootstrap, stats-before-kcache fresh follower ordering,
  equivalent-unit copy guard, real custom package, alias/removal/cascade,
  exact-token isolation, follower-first propagation, stopped apply, failed-add
  history prevention, real cron fork/unfollow, manual/auto repack and exact
  scan passed within the documented scope. Repack truthfully reported unknown
  completion. No privileged statistics probe or extra grant was added.
- Full qualification remains incomplete: protected/TOTP, deterministic crash,
  drill, bucket/rebuild, other platforms, forced OID races and legacy
  lexical-guard-to-restart-token qualification are not claimed. Strict lint
  remains red at 142 versus baseline 131; macOS deferred; publication blocked.
- No source changes followed the accepted freeze. Final evidence/docs review
  and feature commit/PR integration are next. No release tag, signed
  publication or production deployment is authorized.
- **User approved clean-slate provisioning:** “ofc, we always test from a clean
  slate”, after parent asked permission to boot/provision the current clean
  disk. Parent gave advance notice and cleared live worker to boot, provision
  a fresh vaire/PostgreSQL fixture and run V3 qualification continuously.
  No disk reset/replacement is needed or permitted. Current KVM access checks
  pass; previous pre-boot/provisioning holds are historical. Routine documented
  provisioning/testing is authorized; unexpected other-tool state must stop.
- **Latest user handoff: “vps-lab is availble to you now”.** Parent gave advance
  notice and cleared the live worker for initial read-only state/prerequisite
  inspection, then bounded V3 tests only if the current fixture/ownership is
  appropriate. **No disk reset, replacement or reprovisioning.** Unexpected
  other-tool state or required provisioning changes must be escalated to the
  user before changes. This supersedes the blanket hold below, not its shared
  ownership safeguards. No further guest changes have yet been reported.
- **USER CORRECTION: VPS-LAB IS SHARED WITH ANOTHER AGENT.** Stop all lab
  access and changes; earlier authorization did not establish exclusive
  ownership. Do not resume on sudo confirmation or run the previous unblock
  command for this task. Explicit coordination with the user and the other
  lab owner is required before any future access. Parent acknowledged that
  resetting the disposable disk without checking ownership was a mistake and
  may have disrupted the other agent. Do not attempt recovery/reset/inspection
  without approval. V3 live qualification remains blocked.
- Evidence hygiene audit found no blocker in 82 inspected artifacts; summary:
  `evidence/extensions/evidence-hygiene-v3.md`. The new V3 lab-blocker report
  and its MANIFEST entry were excluded and still require review before commit.
- **V3 runtime qualification BLOCKED before guest boot:** host `/dev/kvm`
  access/network setup needs interactive sudo. User was asked to run
  `cd ~/work/vps-lab && sudo ./lab network-up`; await confirmation before
  clearing the live worker to resume. No V3 PostgreSQL/systemd tests ran.
  This is an environment block, not a functional failure.
- V3 lab binary built with all 687 source hashes checked before/after:
  `bin/vaire-extension-final-v3`,
  identity `84f71bbaa180-ext-v3-66ae70f96f88`, SHA-256
  `069516576918705d82b137994adf6dfe3588951a83a1ac75b8a2d3db99ca2c6f`.
  Temporary transfer HTTP server was stopped; no listener remains.
- The disposable guest had been repurposed for another tool and was reset to
  its base disk before the boot blocker. **Prior V1 fixtures are gone**; no
  existing legacy-guard retry or preserved guest data may be claimed. Repo
  evidence remains. No eregion repository/state was read. See
  `evidence/extensions/results-final-candidate-v3-blocked.md`.
- **V3 complete local gate PASS; lab clearance issued after user notice.**
  Tester verified all 687 hashes unchanged before/after, full uncached tests,
  build/vet, all 615 Go formatting checks, metadata/18 steps/shell/workflow/
  pin checks. Strict lint remains 142 (same normalized set as V2; baseline 131).
  Evidence `evidence/extensions/99-V3-FINAL-SUMMARY.log` and
  `V3-20261116T1802Z-*`. Both source reviews ACCEPT.
- Parent explicitly told the user **“I’m now starting the VPS-lab checks”**
  before clearing live worker `ses_e1b3fd5cbffeuN9Oi2UtMDNwMs` to build and
  qualify V3. Advance-notice requirement fulfilled for this run. The worker
  alone owns guest access; no concurrent lab drivers. Prior “on hold” notes
  below are historical and superseded by this clearance only.
- **V3 lane B: ACCEPT whole-change Linux source.** Both independent source
  reviews now accept V3. Lane B independently passed full uncached tests,
  removal regressions, build and vet, with 687/687 hashes unchanged.
  The dedicated full verification report remains pending. Native preload-order
  drift → pending restart → token behavior remains an explicit lab gate.
  User advance notice is still required before any lab activity.
- **V3 lane A: ACCEPT whole-change Linux source.** All 687 source hashes
  verified before/after; EFIN-12 actual-DROP correction closed, prior source
  acceptance retained. Independent removal regressions passed. Lane B and full
  V3 verification remain pending. No lab activity or runtime acceptance implied.
- **V3 frozen at 2026-11-16T18:01:49Z:** complete source manifest SHA-256
  `66ae70f96f88f0577dac3388b9da6f26add06a897ad047950e762ab8c27ff7ea`,
  687 entries. The only V2 delta is removal DROP-state propagation in
  `node/extensions.go`, `extensiontransition.go` and new
  `extensions_removedropclaim_test.go`. Actual `sqlDropped` now flows through
  config/restart/retry/follower/replay/verification failures; no-DROP messages
  no longer claim execution. Worker focused regressions (64), full node tests,
  build and vet passed. Both independent final reviews and fresh full Linux
  verification are running on V3. **VPS-lab remains on hold.**
- V2's historical source manifest is preserved as
  `evidence/extensions/source-manifest-v2-superseded.sha256`.
- **V2 review outcome: BLOCK, superseded for source acceptance.** Lane A
  accepted; lane B found later removal failure paths still hardcode
  `sql_dropped:true` even for supported already-absent SQL/config-only removal
  and retries. All other reviewed scopes accepted. V2 tests passed but do not
  override this defect. Source reopened only for the bounded correction below;
  VPS-lab remains on hold.
- Worker `ses_e1a820b9bffeLCd8iDfT2ljipm` owns removal-only paths in
  `node/extensions.go` and `extensiontransition.go` plus dedicated regressions.
  Propagate actual `sqlDropped` through config/pending/restart/follower/absence
  verification failures. Test actual DROP true and no-DROP false, including
  retries; preserve all accepted ordering, guards and partial-history behavior.
  Re-freeze a new candidate after this correction and review its exact source.
- **V2 frozen at 2026-11-16T17:45:20Z:** replacement manifest
  `evidence/extensions/final-source-manifest.sha256`, SHA-256
  `63d2208e405678e32781331c199134ab7ca33d9a0a269a3d7042014b7dbd58ef`,
  686 source/test/build files. All assigned fixes and parent order integration
  are implemented. Two independent whole-change reviews and fresh Linux
  verification are running. **No VPS-lab checks started; explicit hold remains.**
- V2 lane A whole-change verdict: **ACCEPT Linux source**, with all 686 hashes
  verified before/after. EFIN-14/15/16 closed and prior lifecycle closures
  retained. Selective uncached regressions passed. Lane B and the complete
  independent verification remain pending; runtime/release approval is separate.
- V2 complete Linux verification: **PASS**, 17:46:00–17:48:53Z, all 686
  hashes unchanged. Build/vet/full uncached unskipped suite passed (835 tests,
  zero skipped), all 614 Go files formatted, metadata/18 steps/shellcheck/
  actionlint/pins passed. Strict lint remains **142 findings** (V1 142,
  baseline 131); macOS deferred. Evidence:
  `evidence/extensions/99-V2-FINAL-SUMMARY.log` and `V2-20261116T1746Z-*`.
  Lane B whole-change review and native/runtime qualification remain pending.
- The V1 manifest is preserved as
  `evidence/extensions/source-manifest-v1-superseded.sha256`.
- V1 source freeze `2026-11-16T16:39:17Z`, manifest
  `evidence/extensions/final-source-manifest.sha256`, digest
  `0cf11932fee499fb15748e43c6af9d14a9a62d7e04dccb8c488adfd0069fa272`.
- V1 independently passed Linux build, vet, full uncached Go tests, all 608
  tracked/untracked Go formatting checks, metadata, 18 steps, shellcheck,
  actionlint and pin checks. All 680 manifest entries matched.
- Strict lint: 142 findings versus 131 baseline; added findings include benign
  dead stores and test/style warnings. This remains red, not “all pre-existing.”
- **Both final source lanes BLOCKED V1. Source has now reopened for the
  assignments below. V1 manifest is historical, not current freeze authority.**
- Historical F7-72–78 and EFIN-1/2/3/5/6/7/8/9/10/11 source-closed within their
  reviewed scopes. EFIN-13 withdrawn after correcting fixture ownership; normal
  application-owner auto-repack works without extra grants. An approved
  privileged statistics probe was therefore deliberately **not** implemented.

### Active source owners — resume these sessions, do not duplicate their work

| Owner / session | Scope and remaining work |
| --- | --- |
| worker `ses_e1a8f7117ffey4e7NNYwmcd0H2` | **EFIN-14 completed and frozen:** explicit canonical selection controls descriptor defaults; rendered extension-owned global/database overrides still expected for implicit modules. New renderer/native-5000 bootstrap, explicit mismatch and missing-setting regressions; full unskipped suite/build/focused vet passed per worker. Included in V2 freeze. |
| worker `ses_e1a8f50b4ffeCBpP109AhsuvJA` | **EFIN-15 completed, pending final review:** froze `node/extensionactivation.go` and `extensions_activation_test.go`. Strict `copyGuardSettingSQL` projects value/type/unit/pending; shared typed comparison accepts equivalent units and keeps missing required settings fail-closed. Boundary tests include mismatch, pending, exact custom placeholder, missing empty setting and SQL shape. Full node tests/build/node vet and focused steps comparisons passed per worker. |
| Parent | **Integration implemented:** stable retained-preload dedup, ordered `FollowerGuardDrifted`, resolver-ordered initial copy plans, legacy-order reconciliation and initial-plan regressions. Generic unload hints now limit claims to the withheld configuration and allow earlier-step effects. Focused order tests pass; whole suite and review pending. |
| worker `ses_e1b10d414ffevFsecICTDeIR5C` | **EFIN-4 / follower EFIN-12 completed, pending parent/final review**: froze `node/extensiontransition.go`, `methods.go`, `steps/postgres_config.go` and related tests. Typed start failures retain observed started/pending targets, unknown start state, package effects and old configuration; apply errors preserve engine partial data and warnings. Follower remove now reports partial history and actual DROP/intent state through shared direct-remove helpers; already-enabled add save failures retain correct existing-intent state. Build/vet/full node tests passed; full suite passed only with the known obsolete guard-order test skipped. Parent must update that assertion and run an unskipped suite before freezing. No other source files or VM touched. |
| worker `ses_e1aefed8fffemwlJ9pA0kJxQ1N` | **Direct remove EFIN-12 completed, pending parent/final review**: froze `node/extensions.go` and new `extensions_removehistory_test.go`. Capture failures disclose retained partial history without DROP/config/restart. Post-DROP Encode/Save share typed partial helper with actual `sql_dropped`, intent/retry/pending state; config-only failures correctly say no DROP. Existing capture semantics/add/schema guards preserved. Focused/full node tests, build, formatting and diff check passed per worker. Shared `extensionRemovePartialDetails` and `extensionRemoveIntentFailure` APIs sent to runtime worker for follower integration. No encode-fault seam added. |
| worker `ses_e1aa89626ffew0RHudtFsVgMTK` | **EFIN-16 backup scope completed, pending integration/review**: `backup/extensions.go` now preserves desired → retained → operator load order, stable dedup and retained > desired > operator provenance; parsing and descriptor-derived guards preserve order. New `extensions_preload_order_test.go`; backup tests/vet pass. Full-suite failure is the old steps guard-order expectation described below. No source edits outside backup. |

**EFIN-16 parent integration now implemented; independent review pending:**

1. `FollowerGuardDrifted` compares ordered slices, exposing order-only drift.
2. Initial copy plans derive desired order from `resolver.Preloads`.
3. Retained preload collection uses stable dedup, not lexical sort.
4. Updated complete-guard expectation and added legacy-order reconciliation
   plus initial-copy resolver-order regression. Both focused tests pass.
5. Lane B is checking order drift through restart/token discovery; lane A is
   reviewing completed partial-outcome/removal fixes. Neither is a replacement
   whole-candidate verdict yet.

Lane A subsequently **accepted the bounded EFIN-4/12 corrections**. Parent
fixed its nonblocking diagnostic issue: a follower post-start failure names the
actual started follower, while the removal retry still names the source.
The focused hint regression passed. Both lanes are now reviewing all of V2.

### Review/test sessions to resume after replacement freeze

- Oracle lane A: `ses_e1b44b705ffeRNzY0DWKaZ2uDD`.
  V1 BLOCK: EFIN-14 primary unrendered defaults and EFIN-15 missing copy-guard
  unit metadata. Other reviewed lifecycle/admission fixes closed.
- Worker lane B: `ses_e1b449ba8ffeA1p3ZaOfo00cCb`.
  V1 BLOCK: EFIN-4 started-state/error truthfulness; EFIN-12 remove capture and
  post-DROP errors; minor already-enabled follower-add save reporting.
  EFIN-8 accepted, including actual namespace before grants and no public-schema
  assumption. Both lanes must review replacement whole change independently.
- Tester: `ses_e1b4479f6ffeQeQYczHLgOBQRT`, V1 checks finished.
  Re-run exact replacement source after all writers freeze; preserve old logs.
- Live worker: `ses_e1b3fd5cbffeuN9Oi2UtMDNwMs`, owns guest serially.
  Awaiting V2 source manifest/build; do not drive the guest concurrently.
  Build unique dev identity and verify source hashes before/after build.

### Live evidence and important factual corrections

- Guest Debian 13.7, PostgreSQL 18, 4 CPU/4 GiB/20 GiB, IP 172.30.99.2.
  Bootstrap SSH closed; old isolated workspace keys do not match guest
  operators. Tests use **real production node RPC + systemd**, not qualified
  CLI/WireGuard or signed release transport.
- Baseline and EFIN-1/11 overlay passed real custom pg_stat_kcache provider,
  alias/install/removal, isolated tokens, source/follower ordering, real cron
  fork/unfollow with launcher draining and copied jobs inactive; manual/auto
  repack honestly reports completion_unknown; full pgstattuple report works.
- V1 VM: stopped apply, unrelated numeric settings, schema refusal, failed-add
  history prevention then follow, dependency cascade, token isolation and exact
  scan succeeded within their bounded cases. V1 is **superseded**, not final.
- **EFIN-16 is distinct from EFIN-14:** source managed and actual preload order
  is `pg_stat_statements,auto_explain,pg_stat_kcache`; follower managed file has
  same order, but its winning copy guard and actual SHOW are
  `auto_explain,pg_stat_kcache,pg_stat_statements`. Removing the explicit stats
  setting exposed native startup FATAL reading pg_stat_statements.max in
  kcache. Lab recovered follower with a temporary native start override and
  supported config apply; recorded as recovery, never a pass.
- Namespace facts: pg_cron extnamespace **pg_catalog**, owned objects/grants
  **cron**. pg_repack extnamespace **public**, owned objects **repack**.
  Do not conflate namespace fields or assume unspecified schema means public.
- EFIN-13 retest: fresh **app-owner-owned** table, no explicit admin grant,
  auto-repack selected=1 unknown=1. Initial postgres-owned fixture was wrong.
  Diagnostic grants revoked; no new privilege probe implemented.
- Source/history pollution in old `extlab` is retained as evidence; do not
  repair/delete valid records. Use fresh stanzas for EFIN-12 prevention reruns.
- Protected/TOTP, deterministic crash boundaries, drill, bucket/rebuild and
  full platform matrix remain honest partial/BLOCKED limits.

### Artifacts and next steps

- This file is the only finishing tracker. Ten obsolete Phase 7 plans,
  handoffs, ledgers and notes are deleted in the feature worktree; canonical
  feature reference/live procedure/testing log remain. Human docs are updated
  but still need final outcomes.
- Sanitized evidence lives in `specs/evidence/extensions/`; safe artifact index
  is `MANIFEST.md`. Raw potentially sensitive archives were removed from repo
  evidence to private `/tmp/opencode/vaire-extension-evidence-raw/` and guest
  state. Do not read secrets or commit raw captures.
- Parent hardened `node-rpc-case.sh` to derive binary commit and recursively
  redact credential/handoff/URL/DSN fields while retaining non-secret restart
  tokens. Bash syntax, shellcheck and synthetic redaction tests pass. It is
  evidence tooling, outside the production source manifest.
- Finish assigned fixes; update ledger; freeze V2 with a **new complete source
  manifest**, preserving V1 evidence. Run two independent final reviews, fresh
  Linux verification and integrated VM reruns. Reopen only for actual defects.
- Then update factual docs and safe evidence, commit only authorized feature
  files, push branch/create reviewed PR and merge to master. Recheck remote
  base and preserve the user's staged main-checkout justfile. No tag,
  production deployment or successful signed-release claim is authorized.

---

Current finishing checklist, reconciled 2026-11-16 against
`84f71bbaa1804fb9b7d581ca8a109192dc6375a9` on `master`.
This replaces the Phase 7 plans, handoffs, work-unit record, findings ledger
and implementation notes. Their original text remains in Git history.

## Current state

| Area | Observed evidence | Remaining gate |
| --- | --- | --- |
| Catalogue, custom definitions, config/render, packages, SQL, CLI and doctor | Production code and regression tests exist | Integrated source acceptance |
| Followers, historical requirements and copy activation | Production code and lifecycle/file/model tests exist | Integrated source acceptance and live qualification |
| Manual/automatic repack and exact table report | Production code and regression tests exist | Integrated source acceptance and live qualification |
| Git integration | Feature and subsequent fixes are in local `master`, matching local `origin/master` at this checkpoint | Verify actual workflow, signed artifact and installed identities; local refs do not prove publication |
| Final source reviews | Both historical whole-change reviews returned BLOCK on 2026-11-11; later fixes and regressions exist | Two fresh independent whole-change verdicts on the frozen current candidate |
| Local verification | Full uncached `go test -count=1 ./...`, build, vet, metadata/step/shell/workflow/pin checks passed at the initial candidate | Rerun after fixes; strict lint debt and macOS are explicitly deferred |
| Live evidence | `dd6106d` reports Debian VPS lifecycle qualification; the checked-in extension cases have no recorded results | Reconcile actual evidence per case; do not infer an extension pass or that no live testing occurred |

**Interpretation:** implementation is present and in final hardening. Neither
final acceptance nor extension live qualification is established by this audit.
The user subsequently authorized finishing the feature and integrating it into
master, and explicitly authorized the disposable Firecracker lab at
`~/work/vps-lab`. Production deployment, external bucket credentials and
destructive VPS operations are not implied.
The user explicitly deferred macOS work during this finishing pass; the Darwin
cross-build failure is recorded below, not a Linux completion gate.
The user also explicitly chose to defer the broad pre-existing strict-lint
refactor. Preserve the failed lint result as accepted debt; do not weaken rules
or claim a fully green verification gate.
The existing edge publication workflow runs strict lint and multi-platform
release builds. These deferrals therefore leave edge publication blocked even
if the Linux source is accepted and merged. Source integration is not a
successful signed release.

Current references:

- [Extension definitions](../docs/EXTENSION-DEFINITIONS.md): public descriptor,
  command, provider and recovery contract.
- [Combined run sheet](../docs/RUN-phase-6-7-live.md): qualification procedures,
  preserved follower cases, admission values and stop conditions.
- [Manual testing](../docs/MANUAL-TESTING.md): dated results and coverage limits.
- [Release procedure](../docs/PLAN-v1-release.md): release/branch policy.

## Final findings: evidence found, closure pending

All seven findings below were blockers in the historical final review.
The current command-safety lane independently source-closes them within their
original scopes; both lanes source-close F7-73. F7-77 closure excludes
hand-edited generated files and is not live journal evidence; F7-78 closes the
boolean scope, not the adjacent numeric/enum dependency defect EFIN-6.
Both independent whole-change verdicts **ACCEPT V3 Linux source**. Fresh V3
verification passed on the exact manifest recorded above. Earlier blocked
candidates remain historical evidence, not current acceptance. Native runtime
qualification is running separately and does not imply signed publication.

| ID | Requirement to verify | Current regression evidence |
| --- | --- | --- |
| F7-72, major | Fresh bootstrap creates the database before connecting to its catalogs; failed prerequisites have no effects | [postgres_roles_pipeline_test.go](../internal/steps/postgres_roles_pipeline_test.go): `TestPostgresRolesFreshDatabaseBootstrapOrder`, `TestPostgresRolesFreshDatabaseFailedPrerequisiteHasNoEffects` |
| F7-73, major | Pre-swap abort retires only exact owned authority; failed routing/resume/clear retains a supported retry | [restore_abort_fence_regression_test.go](../internal/node/restore_abort_fence_regression_test.go): `TestPreSwapRestoreAbortRetiresPreparedFence`, `TestRestoreAbortRetainsRoutingOnRenderFailure`, `TestRestoreAbortResumeVerificationFailureRetainsContinuation`, `TestRestoreAbortApplyRetryFinalizesContinuation` |
| F7-74, major | Repack probes and execution use the same resolved schema/name/OID despite role/search-path changes; replacement refuses | [repack_test.go](../internal/node/repack_test.go): `TestRepackResolvedIdentityCarriesThroughProbesAndExecution`, `TestJobRepackRefusesReplacedRelationBeforeExecution` |
| F7-75, moderate | Manual multi-table repack rechecks the window after all probes before each execution; an active rewrite is not cancelled at window end | [repack_test.go](../internal/node/repack_test.go): `TestJobRepackManualWindowRecheckedAfterAllProbes` |
| F7-76, major | Tune and deferred-maintenance callers cannot bypass pending source/follower extension dependencies | [extensions_transition_test.go](../internal/node/extensions_transition_test.go): `TestTuneApplyRefusesPendingExtensionTransitionBeforeSourceEffects`, `TestDeferredRestartRefusesPendingExtensionTransition`; [extensions_lifecycle_test.go](../internal/node/extensions_lifecycle_test.go): `TestTuneApplyPreflightFailureStopsBeforeConfigWrite` |
| F7-77, major | Direct add/remove discover unrendered follower config/journal drift and require every restart token before any effect | [extensions_transition_test.go](../internal/node/extensions_transition_test.go): `TestExtAddFollowerUnrenderedConfigDriftRequiresTokenBeforeEffects`, `TestExtRemoveFollowerUnrenderedConfigDriftRequiresTokenBeforeEffects`, `TestExtAddConfigDriftForwardsPlannedRestartConfirmation` |
| F7-78, moderate | Effective boolean settings use PostgreSQL type-aware equivalence, including valid prefixes, without weakening exact string/quote semantics | [extensions_test.go](../internal/steps/extensions_test.go): `TestExtensionSettingMatchesBooleanInputForms`; [extensions_transition_test.go](../internal/node/extensions_transition_test.go): `TestExtensionPrimaryReadinessBooleanFacts` |

For each finding, record the reviewed revision, source reasoning, focused test
result and reviewer disposition here. Passing a mock/model regression is not
live PostgreSQL, PgBouncer, APT or process-death evidence.

### Integrated findings and source closure

Initial reviews blocked the baseline and later V1/V2 candidates. Both final
whole-change reviews now accept the corrected V3 source; all EFIN findings
below are source-closed or withdrawn. Runtime qualification remains separate.
Strict lint/macOS remain deferred release blockers, not green checks.

| ID | Severity | Observed source failure | Correction / status |
| --- | --- | --- | --- |
| EFIN-1 | Major | Unfollow deactivates cron jobs but renames the metadata database without gating/draining sessions; the launcher connects even with launches suppressed | Targeted independent source ACCEPT. Isolated full Go tests/vet pass. Baseline-plus-fix overlay successfully drained a real pg_cron launcher and completed unfollow in the VM; final integrated candidate rerun pending |
| EFIN-2 | Major | Rebuild clears activation fences but retains interrupted `Unfollowing`, inheriting lost-host SQL/pooler fencing and operation/cron authority | Targeted independent source ACCEPT: `PruneForRestore` clears lost-host continuations and preserves history. Real bundle build/read/place/reload tests cover three states; backup/masterbackup and targeted node checks pass |
| EFIN-3 | Major | Adopted fork retains materialisation ownership after failure, but public apply's earlier materialisation guard blocks its advertised non-secret continuation | Targeted independent source ACCEPT after correcting actual-registration checks before adopted effects and durable marker absence before every full/scoped/step-selected publication path. Public refusal/retry regressions and full Go tests pass; integrated live/final review pending |
| EFIN-4 | Blocker | Ordinary apply queries stopped source/follower catalogs before its own cluster-start step, producing a circular “start with apply” refusal | Both V2 source lanes ACCEPT: preserve old startup runtime, prove unload after start, check affected-target fences, and report actual partial start/package outcomes. Managed-file/fence/error regressions pass; replacement runtime qualification pending |
| EFIN-5 | Blocker | `handleApply` returns non-pending transition errors before installing the returned lock-release defer | Targeted independent source ACCEPT: every non-nil release is deferred immediately; production-caller release-once/reacquisition regression passes |
| EFIN-6 | Major | Follower expected settings include every unrelated raw global/cluster setting and compare non-bool values textually | Targeted independent source ACCEPT: extension-owned obligations/overrides only; typed bool/enum/numeric/unit comparison and exact string/unknown fallback. Regression/full Go checks pass; integrated live rerun pending |
| EFIN-7 | Moderate | Follower verification requires implicit default-module settings that the renderer never writes for undeclared SQL extensions | Targeted independent source ACCEPT: explicit ordinary expectations match real rendering; implicit modules retain preloads/packages and retained obligations remain. Regression/full Go checks pass |
| EFIN-8 | Major | Installed extension schema/control-name edits can leave SQL in the old identity while saving new intent or granting the wrong schema | Both final source lanes ACCEPT guards across initial config selection, direct resolved intent, roles/grant boundary, explicit-schema unknown facts, selected identity edits and live followers. Real control/metadata namespace differences preserved |
| EFIN-9 | Minor | Exact report resolves caller table text again and can attribute a scan to stale relation facts | Targeted independent source ACCEPT: scans admitted OID and rechecks canonical identity. Full-scan SQL and changed/missing/unreadable identity regressions pass |
| EFIN-10 | Lifecycle risk | Direct add/remove do not explicitly refuse an active fork activation fence after materialisation authority is absent | Targeted independent source ACCEPT for direct add/remove: any-phase fence refusal for target and affected live followers, preserving precedence and unrelated safe targets. Ordinary apply paths remain part of integrated review |
| EFIN-11 | Live blocker | `psql --csv` emits native booleans as t/f, but copied cron's table-presence check expected textual true/false | Targeted independent source ACCEPT: explicit SQL text cast, exact-one-row check and real-wire-format fake/regressions. Corrected VM rerun pending |
| EFIN-12 | Major | Failed missing-file/package adds retain speculative conflicting incoming-provider snapshots even after intent rollback; a later current copy refuses | Both V3 source lanes ACCEPT capture ordering, partial-history/save truthfulness and actual DROP-state propagation through all failures/verification. Full V3 tests pass; no history deletion. Integrated runtime rerun pending |
| EFIN-13 | Withdrawn blocker | Initial postgres-owned fixture hid statistics from vaire_admin; this was incorrectly generalized to ordinary application tables | Corrected fresh app-owner-owned fixture needs no explicit SELECT grant: 77,987,840 estimated bytes / 0.938, selected=1 unknown=1. Independent source reassessment confirms owner membership supports this. No privileged probe or grant expansion added |
| EFIN-14 | Major | Primary readiness still requires unrendered implicit pg_stat_statements settings on empty/custom-only fresh selections, so native max=5000 never satisfies catalog max=10000 | Both V2 lanes source ACCEPT: expectations follow explicit canonical selection and rendered owned overrides; native-default bootstrap regressions pass. Integrated runtime rerun pending |
| EFIN-15 | Moderate | Copied-guard verification omits pg_settings.unit, falsely rejecting equivalent unit-bearing settings despite the typed comparator | Both V2 lanes source ACCEPT: strict setting query includes unit/type/pending metadata; equivalent units, genuine mismatch, placeholder and missing-empty regressions pass. Runtime qualification pending |
| EFIN-16 | Major | Winning copy guards alphabetically reordered preloads, making pg_stat_kcache load before pg_stat_statements and fail startup without an explicit stats placeholder | Both V2 lanes source ACCEPT: desired resolver order, stable retained/operator union, preserved provenance/readback and order-sensitive drift detection. Native order-only drift/restart-token qualification remains pending |

Regressions must exercise actual public/production boundaries: session
reconnect/failure compensation for unfollow, real bundle restore transformations
for pruning, and public apply with retained ownership plus an adopted checkpoint
for fork continuation. The initially suspected public SQL-only safety bypass
was withdrawn because the materialisation guard made it unreachable; enabling
the legitimate continuation must still validate its complete descriptor union.
CLI restart prompting remains predictive: it does not discover/prompt every
follower token interactively. The node still requires the complete exact set
before effects and returns retry tokens. Record this as a usability limitation,
not a token-authorization bypass or an automatic-prompting claim.
Unfollow/fork rename compensation is bounded but not crash-durable: SIGKILL
after a committed connection gate can leave `datallowconn=false`. No durable
original-flag journal or hard-crash recovery proof was added; do not present
ordinary cancellation/compensation tests as that guarantee.
The initially approved auto-only privileged statistics probe was not implemented:
corrected EFIN-13 evidence made it unnecessary. Unrelated-role/postgres-owned
tables without applicable statistics visibility can remain honestly
`unestimated`; that does not mean zero bloat or always imply a permission error.

## Contract that must survive closure

These are existing requirements, not new implementation scope.

### Providers, privileges and mutation

- One config-aware resolver serves all paths, including implicit preload-only
  defaults. Canonical keys and SQL/control identifiers are distinct. Validate
  shared provider/library/settings conflicts and reserved security settings
  before persistence; unused notes must not churn plans.
- The catalogue is not an allowlist. Support explicitly selected compatible APT
  providers in already trusted repositories, or operator-preinstalled files.
  Native code is trusted, not sandboxed. No source builds, install scripts,
  automatic repository/key setup, package upgrades or silent provider swaps.
  Changed providers require renewed safety declarations.
- Install/hold required packages and verify actual PostgreSQL 18 control/library
  facts before affected writes/starts. Scoped work prepares only its targets;
  full package convergence retains the host union. Never uninstall on removal
  or failure, and never claim rollback of package effects.
- Protected addition preserves access policy; protected removal and protected
  repack refuse. Custom definitions cannot grant arbitrary privileges.
  Dry runs have no mutation effects. Unknown/unreadable facts are not absence.
  Activity records safe method/target/outcome metadata, not settings, SQL,
  credentials or job payloads.
- Direct add/remove require the complete exact per-cluster token set before
  history, persistence, package, restart or SQL effects. Source tokens, `--yes`
  and broad server tokens do not substitute for follower authorization.
  Revalidate membership and immutable identities under global → heavy-I/O where
  needed → sorted cluster locks; never nest locks or upgrade a held envelope.

### Ordering and retries

```text
add:    preflight/tokens → outgoing history → package/file preparation
         → incoming history → intent save
        → followers config/restart/verify → primary config/restart → SQL/verify
remove: preview/tokens/history → primary DROP while loaded
        → reduced intent → primary config/restart → followers
```

- Never CREATE on standby. Direct follower intent changes refuse. Explicit
  database/step selection, tune and deferred jobs cannot bypass dependencies.
  Preserve ownership, protection independence and standby floors.
- Ordinary config/apply retains safe deferred staging without every restart
  token. Only authorized targets restart; unmet follower runtime requirements
  withhold dependent primary configuration/runtime/SQL. Missing tokens alone
  are not grounds for forbidding safe staging.
- Verify actual required libraries and settings, not only pending flags.
  Decode descriptor literals once; global/database overrides are raw values.
  Compare decoded actual values with correct types; literal quote content stays
  distinct. Unrelated roles/settings do not acquire extension dependencies.
- Re-add finishes pending work; remove retries after a completed DROP and
  follower-only unfinished work. Require an actual supported non-downgrade SQL
  update path and report the resulting version. No implicit CASCADE or silent
  deletion of extra restored extensions. Partial failures report completed and
  pending stages truthfully, without false rollback.

### History, copied jobs and activation

- Retain self-contained descriptor snapshots by immutable primary stanza before
  replacing intent, dropping identity or producing backups. Capture failure
  stops dependent work. Include history in durable hashes and bundle
  build/read/restore/pruning, including dropped/replaced stanzas.
- Observed UTC coverage is never backdated and must cover the selected base
  backup/replay chain, not merely target time. Absent, explicit empty and
  populated `recovery_extensions` remain distinct throughout editing/cloning/
  bundles. Declarations supplement unknown knowledge, never erase known
  requirements. An unbound dump is not attached by a reused database name.
- Before every copied first/retry start, validate the complete current/retained/
  operator preload and settings union, files and copy safety. Physical recovery
  requires standby safety even if promotion is immediate; a logical dump does
  not require it merely because it is a restore. Unknown/ambiguous safety fails
  closed, including SQL-only extensions.
- Mandatory own-descriptor copy settings overlay ordinary values; conflicting
  cross-descriptor obligations refuse. Preserve guard provenance and actual
  precedence through copy, adoption, promotion, swap, rename, settlement and
  later apply. Existing followers refresh their complete winning guard and
  verify all ordinary-plus-mandatory settings before dependent primary work.
- Keep copied cron launches suppressed continuously. Inspect actual metadata
  database/job identity, deactivate copied jobs, quiesce only owned-copy
  sessions and retarget only matching old database names. Do not modify source
  jobs or invent a cron database from retained-library metadata. Suppression
  does not itself prove an already running job stopped.
- Persist instance/stanza/operation-bound activation authority. Validate actual
  registration and ownership before continuation/publication/retirement.
  Fork continuation checkpoints distinguish prepared, adopting, adopted and
  routing; ordinary apply never replays credentials. Both poolers and public
  SQL must withhold unsettled data. Safety completion/authority transfer is
  atomic; verify routing and resume before exact fence retirement. Failures
  retain continuation. Name reuse or a new installation never inherits old
  authority; rebuild pruning preserves history, not lost-host file ownership.
- Hand off suppression only after proven equivalent permanent configuration
  and effective settings. Fresh operations re-establish their obligations;
  settled guards must not override deliberate later supported operator changes.

### Maintenance

- Repack is detachable and auto-repack is opt-in. Admit only ordinary permanent
  heap tables with a valid primary key or suitable non-null unique index and
  `pg_default` relation/TOAST/index/database/session defaults. Resolve the exact
  table identity and revalidate it, protection, versions, space and window
  immediately before each execution.
- Match executable, loaded library and SQL versions. Require twice the measured
  table/index/TOAST footprint plus host reserve; unknown space refuses. Use
  `--no-kill-backend` and the bounded 60-second lock wait. Automation selects
  biggest first with the 64 MiB minimum and configured ratio, one table at a
  time; busy/window outcomes are explicit. Window end starts nothing new.
- Zero exit or changed storage does not prove attributable rewrite completion.
  Unattributable attempts are `completion_unknown`, never confirmed repacked.
  A confirmed skip needs a known diagnostic and identical valid OID,
  relfilenode and tablespace facts.
- Exact reporting scans one supported table with `pgstattuple`, not
  `pgstattuple_approx`, using monitor permissions, a 5-second lock timeout and
  60-second statement timeout. Report measured live/dead/free bytes and
  percentages, not reclaimable, instantaneous, index or database-wide bloat.

## Remaining qualification

The [combined run sheet](../docs/RUN-phase-6-7-live.md) retains detailed
procedures and follower case IDs, including blocked fault boundaries.
These extension cases have **no reconciled live result** at this checkpoint:

| Case | Evidence needed |
| --- | --- |
| X1 | Built-in alias, package/control/version facts, add/remove and retained package |
| X2 | Debian lab verified real non-built-in pg_stat_kcache 2.3.2 from `postgresql-18-pg-stat-kcache`; revalidate final candidate/platform and stanza-backed/rebuild paths. Built-in vector is insufficient |
| X3 | Safe absent prerequisite, actionable refusal and preserved recovery state |
| X4 | Dependencies/owned data, explicit cascade, protection and SQL-absent retry |
| X5 | Isolated preload authorization, effective startup and pending SQL retry |
| X6 | Complete source/follower token discovery, ordering and deferred apply |
| X7 | Continuous copied cron suppression through fork/drill/unfollow independence |
| X8 | Partial outcomes and retries at supported deterministic boundaries; otherwise BLOCKED, not synthetic live proof |
| X9 | Historical requirements after removal, bundle/rebuild and all recovery declaration states |
| X10 | Manual/automatic repack identity, privileges, versions, space, windows and truthful outcomes |
| X11 | One-table full-scan report, privileges, timeouts and measurement semantics |

Before executing: reconcile any existing VPS evidence by version, command,
case and outcome; identify one verified signed candidate and operator/node
identities; resolve the named workspace/host, inventory/protection/recovery
options, fixtures, disk/WAL/write/time/scan budgets, permissions and cleanup.
The run sheet's older “context” terminology must be checked against the current
workspace CLI before execution. Activity AC-01 must pass or its named gaps be
explicitly accepted. One live owner runs activity, no-bucket, then separately
approved bucket/rebuild passes serially.

Production qualification still forbids ad hoc source-built uploads or harnesses.
The user explicitly authorizes local candidate builds and testing in the
isolated Firecracker lab; record its code/build identity and do not label it
signed-release qualification. No production fault hooks, corrupt records,
generated-config hand edits, `ALTER SYSTEM`, repair commands or timing-only
crash proof. Stop on unexpected effects or breached budgets.
Deployments, restarts, sudo/TOTP, rebuilds, bucket changes and destructive
cleanup need their own permissions. Record retained fixtures if cleanup is
not authorized. One VPS and cross-builds do not establish the complete
Debian/Ubuntu amd64/arm64 install/runtime matrix or macOS live behavior.

## Finish checklist

- [ ] Reconcile any later live evidence into X1–X11 and the preserved follower cases.
- [ ] Resolve all seven final findings with source reasoning and regression evidence.
- [ ] Freeze the candidate and obtain two independent whole-change accept/block
  verdicts, covering both provider/privilege/ordering and copy/history/routing/
  maintenance boundaries. No unresolved blocker/major finding; other risks
  require explicit disposition. Re-review fixes; tests do not waive findings.
- [ ] Run full fresh verification on the frozen reviewed tree and record revision,
  diff identity, UTC times, tool versions, commands, exit codes and output:

  ```sh
  go build ./...
  go vet ./...
  go test ./...
  golangci-lint run ./...
  just lint
  git diff --check
  go test -count=1 ./...
  ```

  Verify gofmt and Markdown links too. The feature worktree's `justfile` retains
  metadata, step, shellcheck, actionlint and version-pin checks. The user's
  separate staged change in the main checkout comments these out; preserve it
  untouched and report verification against the actual feature-worktree recipe.
  Darwin cross-build was attempted and fails on pre-existing Linux-only
  `Renameat2`/device-ID code in `internal/node`; macOS fixes and qualification
  are deferred explicitly by the user. No unsupported-platform fallback was
  introduced merely to make the check pass.
  Strict lint initially reported 131 findings, dominated by complexity/nesting
  and stale suppressions in existing extension/lifecycle code. The user
  explicitly deferred the broad refactor. Record final lint output and the
  separately executed metadata/step/shell/workflow/pin checks; an early lint
  failure means `just lint` did not execute its tail.
- [ ] Verify the actual matching edge workflow and immutable signed artifact;
  record version/commit/channel/checksum/platform. Code in master, workflow
  success and installed identity are different evidence. Any further Git or
  release action needs authorization; no stable-line merge/tag is implied.
- [ ] With separate live permission, complete qualification or record each
  BLOCKED/INCONCLUSIVE case and obtain explicit acceptance of remaining gaps.
  Correct source defects through review/publication/upgrade, not host repairs.
- [ ] Update roadmap and draft release notes from actual results; beta remains a
  separately authorized release activity.

## Verification and provenance

### Frozen integrated candidate

Production/test/build source frozen at **2026-11-16T16:39:17Z**, base `84f71bb`
plus the approved uncommitted corrections. The complete source-file manifest is
[final-source-manifest.sha256](evidence/extensions/final-source-manifest.sha256),
SHA-256 `0cf11932fee499fb15748e43c6af9d14a9a62d7e04dccb8c488adfd0069fa272`.
Final independent source reviews, Linux verification and integrated VM tests
must use that exact source (or explicitly supersede this checkpoint after fixes).
Evidence/documentation updates do not authorize source drift.
V1 Linux build/vet/full uncached tests, formatting of all 608 tracked/untracked
Go files, metadata/18-step/shell/workflow/pin checks passed with all 680 source
hashes unchanged. Strict lint failed with 142 findings versus 131 baseline,
including added dead-store/test-style findings; it is not a green lint result.
Final source lane A nevertheless **BLOCKS V1 on EFIN-14/15**. Passing automated
checks do not override that verdict; V1 must not be merged as accepted.

On 2026-11-16, at the revision above, this command exited 0 for all eight
packages (including the regressions indexed above):

```sh
go test -count=1 ./internal/catalog ./internal/config ./internal/render \
  ./internal/steps ./internal/node ./internal/backup \
  ./internal/doctor ./internal/cli
```

That initial eight-package audit was not the full gate. A subsequent full
`go test -count=1 ./...` passed, including activity, maintenance and
masterbackup. Independent verification also passed build, vet, formatting,
command metadata, all 18 step definitions, shellcheck, actionlint and installer
version pins. Initial evidence is in
[specs/evidence/extensions](evidence/extensions/00-SUMMARY.log). Strict lint
failed with 131 findings and Darwin cross-build failed; both dispositions are
explicit above. These results predate the EFIN corrections and must be rerun.

Historical activity store-lock timeouts passed retries but remain a
contention/flake risk; record first failures rather than rewriting them as
passes.

Historical stages E7-02–06 received source acceptance; F7-01–71 were closed in
their stage scope. Both whole-change reviews then blocked on F7-72–78 on
2026-11-11 despite passing candidate checks. Preservation commit `8c11370`
contains the original ledger, plans, evidence hashes and pause record.
Subsequent fixes do not themselves establish renewed acceptance.
The approved pre-v1 direction requires no legacy compatibility migrations;
scope changes still require a product decision.

Useful upstream references (version-specific behavior still needs runtime proof):
[pg_cron](https://github.com/citusdata/pg_cron#setting-up-pg_cron),
[pg_repack](https://reorg.github.io/pg_repack/),
[PostgreSQL 18 pgstattuple](https://www.postgresql.org/docs/18/pgstattuple.html),
[Timescale 2.31.0 worker suppression](https://github.com/timescale/timescaledb/blob/2.31.0/src/loader/bgw_counter.c).
Timescale's zero-worker setting must be effective at startup and does not
claim zero process/log effects; retained metadata and alternative providers
still need explicit safety verification. Not loading `pg_partman_bgw` does
not suppress external schedulers.
