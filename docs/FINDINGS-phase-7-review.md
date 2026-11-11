# Phase 7 findings ledger

## Foundation checkpoint review

Targeted E7-02 source review against base `0d8e1fcc493cd30ca4afb89b54d6bd050302f552` and its uncommitted foundation diff. This is not either required final cumulative review. Verdict: **BLOCK** pending corrections and focused re-review.

| ID | Severity | Scenario / correction | Status |
| --- | --- | --- | --- |
| F7-01 | Major | Extension settings/copy settings can override managed security logging after the security file. Refuse all managed security-policy keys before persistence/rendering. Add template-complete regressions. | Closed by targeted re-review after first correction; template-driven tests executed independently |
| F7-02 | Major | Distinct descriptors with different SQL/library names can conflict on the same setting, silently selecting by render order (for example overriding cron.database_name). Validate effective per-database normal/copy setting unions; equal values allowed, intentional same-descriptor copy suppression retained. | Closed by second targeted re-review; enabled and recovery normal/copy union tests executed independently |
| F7-03 | Moderate | Package resolution ignores implicitly preloaded defaults; an APT auto_explain override can load without its package requirement. Use one effective selection for preloads/packages while preserving baseline empty union. | Closed by targeted re-review after first correction; package/preload tests executed independently |
| F7-04 | Moderate | Semantic descriptor helper lacks production integration and roles inputs contain names only. Enabled provider/schema/safety changes must be detected by planning/step inputs; note-only/unused metadata must not cause churn. Full follower transition ordering remains E7-05. | Closed for E7-02 by second targeted re-review; effective default-inclusive identities tested independently. Final transition enforcement remains E7-05 |
| F7-05 | Moderate evidence gap | Absent/empty/populated recovery declarations lack actual master-bundle roundtrip tests, including custom global definitions and restore-file transformation. No serialization loss demonstrated; required evidence missing. | Closed: reviewer inspected added coverage; independent tester executed all three Build/ReadTar/restoreConfig states and explicit-empty restoreFiles path |
| F7-06 | Moderate | SQL extension/control names use canonical-key grammar, rejecting legitimate uuid-ossp. Separate safe SQL/control names from canonical keys and test quoted SQL plus roundtrip. | Closed by second targeted re-review; name/SQL/TOML tests executed independently |
| F7-07 | Moderate | New raw-value grammar accepts invalid unquoted comma lists/malformed numeric tokens; invalid intent can persist before PostgreSQL rejects it. Match legal single-token grammar or encode string values safely. | Closed by second targeted re-review; literal grammar and real FileStore rejection/no-change tests executed independently |
| F7-08 | Moderate | Host provider validation omits implicit defaults. Override default pg_stat_statements to package A, select custom_stats targeting the same SQL extension with package B while omitting explicit pg_stat_statements: validation accepts both but effective package union includes conflicting A/B. Validate effective default-inclusive host requirements without changing explicit SQL creation selection. | Closed by targeted source acceptance and fresh independent tests after F7-09 correction |
| F7-09 | Moderate | F7-08 narrowed same-SQL checks to source/package only. Same SQL/package descriptors with different nonempty preload libraries and contradictory settings now pass across databases or current/recovery selections. Preserve compatible sparse metadata inheritance while rejecting contradictory nonempty library and overlapping setting requirements; retain provenance so sparse entries cannot mask later conflicts. | Closed by targeted source acceptance and fresh independent aggregate/provenance regression tests |

Reviewer found no demonstrated cross-line quote/backslash injection under PostgreSQL 18 configuration grammar: malformed fragments fail parsing rather than inserting assignments. Nevertheless descriptor values must have explicit safe rendering semantics and invalid values must fail before writes. CLI alias canonicalization may remain E7-04; package/roles runtime behavior E7-03 and retained history/activation enforcement E7-05 are not independent foundation defects.

First correction re-review remained BLOCK. New focused tests demonstrate unknown/false custom safety defaults and view/copy isolation. No-output assertions on invalid rendered config/SQL are useful but do not substitute for later zero-mutation authorization integration tests. Minor definition-doc wording must name the three managed auto_explain keys rather than claiming the whole family is reserved. Second correction is the last attempt before the handoff's stop/ask condition applies if this unit fails again.

Second correction re-review closed its four assigned issues but found F7-08; targeted verdict remains **BLOCK**. The handoff's two-failed-correction stop condition is triggered. Orchestrator asked the operator for permission for one additional targeted correction/re-review; no third batch is authorized or running. Independent full checks and uncached tests passed the second-correction tree, which does not waive the source defect.

After receiving the concrete provider-conflict explanation, the operator replied “alright go ahead”. This explicitly authorizes one additional targeted F7-08 correction and re-review, not a waiver of remaining gates or Git/live permissions. Sole source worker resumed; E7-03 is still gated on foundation acceptance.

That additional correction was tested and independently re-reviewed. The exact implicit-default A/B conflict is fixed, but same-provider runtime metadata checks were weakened (F7-09), so E7-02 verdict remains BLOCK. Orchestrator stopped and asked permission to correct that compatibility regression. No subsequent worker is active and no further correction is implicitly authorized. All required independent local checks passed the reviewed tree; this does not override the source verdict.

After clarification that the requested permission covers only the remaining validation correction and repeat local review/testing, the operator replied “you have it”. A fresh sole source worker owns the focused F7-09 correction. No Git/publication/live authorization was granted; later stages remain dependent on foundation acceptance.

The fresh worker implemented accumulated host requirements with provider, nonempty preload and per-setting provenance. Sparse metadata cannot erase later requirements. Targeted reviewer **ACCEPTED E7-02** with no remaining actionable foundation defect. Independent full checks and uncached suite passed at the exact reviewed source hashes; aggregate tests exercised 12 same-SQL, 7 current/recovery and 2 placeholder cases plus F7-08 regressions. E7-02 is accepted as a foundation only; E7-03–07 and final cumulative reviews remain.

## Package/roles checkpoint review

Initial E7-03 targeted review verdict: **BLOCK**. Independent full local checks passed the frozen implementation, but do not close these defects. First correction batch assigned to the sole source worker.

| ID | Severity | Scenario / correction | Status |
| --- | --- | --- | --- |
| F7-10 | Major | Scoped apply excludes the server package step, so config-enabled APT/preload requirements may be absent before roles/start/restart. Enforce relevant prerequisites without broadening scoped apply to unrelated steps or restarts. | Closed by runtime/SQL split source re-review; implicit preload-only defaults guarded before config/restart and tested independently |
| F7-11 | Moderate | pg_partman minimum grants hard-code partman rather than the resolved schema. A valid schema override fails or grants an unrelated schema. Derive and quote resolved schema. | Closed by first correction source re-review; resolved-schema test executed independently |
| F7-12 | Moderate | Preload names with .so append another suffix; bare/absolute control module_pathname facts are silently ignored. Resolve PostgreSQL module forms consistently and refuse unknown/missing facts explicitly. | Closed by first correction source re-review; module-form tests executed independently |
| F7-13 | Moderate | Scoped prerequisite preflight expands raw config instead of engine convergence view. Template apply can act on excluded awaiting databases; excluded-instance apply can install/hold before engine rejects selection. Use identical filtered-view/selection validation before effects. | Closed structurally by second correction source re-review; duplicate selector removed and filtered engine execution authoritative |

Independent tester identified required evidence gaps: the incomplete/restart/retry test uses a synthetic engine step rather than real postgresRoles; direct roles Check incompleteness is not tested; node attachment of the prestart hook is inspected but not exercised; package probe argv is not asserted; successful installation followed by verification failure lacks a regression. These were assigned with the source correction batch. Host-only APT/package runtime remains intentionally not run and must not be inferred from stubs.

First correction independent execution closed those principal evidence gaps: real roles pipeline/check/defaults, node hook helper attachment/refusal, probe argv and post-install verification failure all ran and passed. Full source re-review nevertheless remained BLOCK on tune/configuration callers and raw-view preflight selection (F7-10/13). Read-only oracle is advising the shared enforcement boundary before a second correction is delegated; no source writer currently active. Lesser remaining coverage gaps include PG query failures, preflight-error preventing apply in one trace and full node job/start orchestration rather than helper attachment; these are recorded for correction/integration evidence rather than mislabelled live proof.

Oracle advice resolved the correction boundary: target-specific preparation in config/cluster/roles Apply and authorized Restarter before any mutation, plus owning-operation guards for direct lifecycle starts; remove RPC-only duplicate selection. Engine filtered view/selection validation controls which steps can have effects. Sole worker assigned this second E7-03 correction with tune/restart-only/excluded-selection/failure-ordering regressions. No source review acceptance yet.

Second correction source review found a major remaining runtime/SQL selection conflation: `PrepareExtensionStart` reuses a helper that omits entries with empty ExtName, dropping mandatory preload-only auto_explain. An implicit APT provider override is not installed/held, and a missing preinstalled auto_explain library passes target verification before restart. Shared guard placement otherwise addresses prior bypasses. Correct by separating runtime prerequisite resolution from SQL-creation selection; test implicit default APT and missing preinstalled preload through config and guarded restart before effects, preserving pending state.

This second E7-03 correction verdict is **BLOCK**, triggering stop/ask. Orchestrator asked permission for the remaining runtime-versus-SQL correction/re-review; no new source worker is active. Independent full checks/uncached tests passed but missed this semantic case. Tester also records absent direct tune-command and promote/undo/unfollow failing-preflight traces; existing lifecycle tests globally no-op that seam and cannot certify enforcement. These remain explicit evidence gaps, not live passes.

Operator subsequently explicitly directed completion without repeated routine permission requests. Local correction/review/testing resumed, superseding the routine two-attempt permission pause. F7-10 remains open until independently closed; no safety finding is waived. Material design decisions and Git/live permissions remain separately bounded.

Runtime/SQL split correction independently **ACCEPTED E7-03 foundation** with no remaining actionable stage finding. Runtime targets include preload-only defaults; SQL creation does not. Fresh tests exercised exact implicit auto_explain package/file guard cases, tune failure boundaries and promote/undo/unfollow no-effect preflights. Independent full uncached suite first encountered activity store-lock timeouts in unmodified code; isolated activity and full uncached retry passed. Retain this as a residual contention/flake risk for final verification, not a claim that no failure occurred. E7-04 source worker advanced; no E7-05 or final cumulative acceptance implied.

## Extension command checkpoint review

Initial E7-04 targeted verdict: **BLOCK**. First correction assigned automatically under the operator's instruction to finish approved local work without repeated routine permission prompts.

| ID | Severity | Scenario / correction | Status |
| --- | --- | --- | --- |
| F7-14 | Major | Direct flags can change a global definition affecting other databases/default modules while authorizing/applying only the named target. Detect all affected effective intent and, for standalone E7-04, refuse changes beyond target; preserve existing provider fields on partial flag edits. | Closed by source acceptance and independent tests; later source/follower integration must preserve scope |
| F7-15 | Major | Re-add declares no-op from SQL version/library loaded while configuration work or settings restart remains pending, including retry after intent save/apply failure. Prove target convergence before no-op and require exact authorization before effects. | Closed by second source re-review; current readonly engine-plan/hash and retry regressions executed independently |
| F7-16 | Major | Unknown/nonnumeric version ordering plus a supported PostgreSQL path can execute a downgrade (v2 to v1). A path is not direction proof; refuse automatic ordering that cannot be established. | Closed by first source re-review; nonnumeric/numeric refusal tests executed independently |
| F7-17 | Major | Remove checks only preload differences for restart tokens; removing postmaster settings can DROP/save/configure before discovering missing authorization. Preflight settings/restart work before any mutation. | Closed by first source re-review; settings-only token/no-effects tests executed independently |
| F7-18 | Major | Partial direct flags carry old safety declarations into a new source/package/preload provider. Require explicit fresh copy/standby flags against previous/proposed resolved identity; preserve declarations for settings-only edits. | Closed by second source re-review and independent renewal/explicit-false/no-effects tests |

Independent full command-stage checks passed with no activity flake on that run. Focused tests covered the main expected cases but missed these source scenarios. Assigned evidence additions include remove dry-run, unauthorized custom-definition no-save, handler-level lifecycle refusals and explicit no-restart assertions, real provider/source and package-kept results, and a two-invocation retry after DROP/partial failure. The intentional follower-source gate and no-op history hook remain E7-05 dependencies, not certified historical support.

First correction's independent execution passed the full chain and uncached suite, plus new direct handler/global-scope/pending/retry/removal/provider evidence. Source re-review still BLOCKED on current input-hash convergence and explicit safety renewal (F7-15/18). Sole worker assigned second correction under continuing local authorization; no routine permission pause and no finding waiver.

Second correction independently **ACCEPTED standalone E7-04** with no remaining actionable stage finding. Fresh independent full checks, uncached suite and targeted hash/safety/handler tests passed the same frozen tree. Source/follower and retained-history work remains E7-05, now assigned to the sole integration worker; no final cumulative acceptance implied.

## Follower/history integration pre-review

Initial E7-05 implementation returned with own local passes, not independent acceptance. Orchestrator requested targeted missing evidence and closure of a worker-acknowledged safety gap before freezing for review.

| ID | Severity | Scenario / correction | Status |
| --- | --- | --- | --- |
| F7-19 | Major (pre-review known gap) | RefuseExtensionUnload returns success on stopped/unreadable cluster facts. Already-saved unsafe intent could therefore unload a needed library/config requirement on later apply. Refuse unknown safety before effects while preserving safe first/unchanged starts. | Original gap corrected before review and tested independently; integrated stage remains blocked on findings below |

Additional required evidence: multi-target supported UPDATE sequencing/results, multiple independent sources and missing-one-token zero effects across the whole apply, interrupted dump-promote fixed old/new stanza identity, and complete known-target retry tokens. No independent E7-05 source verdict or fresh tester evidence yet; no historical/live pass inferred.

### Independent E7-05 two-lane review

Both independent initial lanes returned **BLOCK**, without consulting each other. Deduplicated findings retain every scenario:

| ID | Severity | Actual scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-20 | Major (A/B) | A partial retained-only preload list is written as a winning replacement; adoption/promotion/settlement can drop current/default libraries. Every guard assignment must contain the complete required union across all rewrites. | Open |
| F7-21 | Major (A/B) | Failed fork settlement has no durable identity-bound activation fence; later ordinary apply/pooler render can expose unsafe data. Restore errors can unconditionally RESUME after promotion before settlement. Persist and enforce activation eligibility across routing/convergence/retry/resume. | Open |
| F7-22 | Major (A) | Apply plans with env.Cluster loaded before waiting for global lock, so changed membership/descriptors escape current target locks/authorization. Reload/revalidate under the envelope; refuse changed lock requirements before effects rather than upgrading. | Open |
| F7-23 | Major (A) | Removal retry checks only the converged primary, refusing not-installed when followers alone remain unfinished after DROP/save/source success. Determine pending work across the complete current graph before no-work refusal. | Open |
| F7-24 | Major (A/B) | Retained ordinary settings are not conflict-checked or carried into runtime guard, permitting incompatible history or missing historical-only startup settings. Build/validate/preserve ordinary union before compatible copy suppression. | Open |
| F7-25 | Major (B) | Pre-swap restore guard verification queries old live endpoint rather than owned temporary copy, incorrectly refusing legitimate historical libraries. Use actual owned-copy endpoint before swap and live endpoint afterward. | Open |
| F7-26 | Major (B) | Pending mandatory postmaster suppression is returned but ignored before promotion; unfollow can activate enabled workers. Make suppression effective via safe owned-copy restart and verification before promotion. | Open |
| F7-27 | Major (B) | Unfollow retry after metadata-database rename reconnects using stale effective old name before examining progress. Persist authoritative cron rename/settlement progress and resume safely without guessing/source changes. | Open |
| F7-28 | Moderate (B) | Logical dump promotion rebuilds plan as physical standby, wrongly requiring standby_safe for an initdb copy. Preserve actual logical/physical mode across initial and retried promotion. | Open |

Oracle is resolving the minimal durable activation/guard/endpoint/cron correction architecture before one coherent source batch. No new high-level policy is proposed and no source worker active during this decision. Independent tester passed required full/uncached checks at the reviewed checkpoint; passing tests do not close these source failures.

Oracle resolved the in-contract architecture and the sole worker resumed the coherent F7-20–28 correction batch. Shared complete guard composition, explicit activation barriers, durable identity-bound eligibility/continuation and authoritative cron rename progress replace wrapper-only patches. No new user decision, command or generic transaction framework is needed; findings remain open until independent review/evidence closure.

Tester also records remaining explicit evidence gaps: same-name/new-stanza history isolation, real extension-requirement bundle build/prune, truly unbound dump, guard-through-swap/promotion/rename/later-apply, source cron no-effects, standby zero Exec, and all-effect (not only apply-call) atomicity for multiple sources. These are targeted additions for the correction batch; live qualification remains separate.

### First integrated correction review

Both lanes independently remain **BLOCK**. Original locked-reload, complete-graph removal retry, endpoint, pending barrier, logical-dump and cron-OID scenarios are addressed in principle. Guard and historical ordinary-setting composition improved; the new activation fence is not yet safely complete/retryable. New/regressed findings:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-29 | Major (A/B) | A descriptor's own mandatory copy override conflicts with its ordinary value, rejecting built-in Timescale 2→0 and valid custom disabling pairs. Apply own copy overlay before cross-descriptor effective conflict checks. | Open |
| F7-30 | Major (A/B) | Normal/resumed fork renders poolers while settled fence still excludes aliases, then clears without rerender. Enter durable routing eligibility before render; clear only after verified routing finalization. | Open |
| F7-31 | Major (A/B) | Restore record is deleted separately from fence advance to routing, permitting an orphan settled fence with no public continuation. Atomically transfer continuation ownership in one records update. | Open |
| F7-32 | Major (A/B) | Adopted fork remains prepared until fallible settings/start/config convergence succeeds; failure leaves apply unable to continue non-secret work. Persist identity-bound adopted checkpoint before those effects. | Open |
| F7-33 | Major (A/B) | Host is configuration text, not installation instance; recorded stanza/registration/ownership ignored and drop leaves fences. Reused name/new install can receive old settlement. Validate real installation and immutable target/operation ownership; safely refuse or retire matching drop fences. | Open |
| F7-34 | Major (B) | Restore fence does not withdraw existing session-pooler route before swap/start; SQL fence checked only for protected databases. Establish verified both-pooler runtime fence and enforce all SQL access before exposure. | Open |
| F7-35 | Major (B) | Later config apply drops retained historical mandatory suppression while keeping the library. Never remove suppression without equivalent verified permanent config/proven settlement. | Open |
| F7-36 | Major (B) | Rebuild pruning preserves old local-operation fences after discarding their restore/artifact state, blocking new recovery. Retire lost-installation operation fences while preserving historical requirements/awaiting protection. | Open |
| F7-37 | Moderate (B) | Prior operator preloads become sticky hidden guard requirements, overriding deliberate later supported override-file removal. Preserve required-retained provenance separately; refresh inherited operator values from current config. | Open |
| F7-38 | Major (B supplemental) | Final composition discovers/preserves operator/plain-auto/prior-guard libraries after safety validation. Unknown preload-only background libraries can start without resolved copy/standby metadata. Validate the final complete union before every copied first/retry start. | Open |

Oracle is specifying exact durable phases, atomic completion, real instance/target identity, runtime fencing and guard provenance before a fresh coherent correction. No renewed user decision is needed; no source writer active during this refinement. Required new evidence must exercise real continuation/engine/pooler rendering boundaries rather than stubbing the exact paths under review.

Precise oracle contract recorded in NOTES-phase-7-activation-corrections.md; fresh sole worker assigned the coherent second integrated correction. All F7-29–38 stay open pending source re-review and fresh evidence. Routine correction authorization continues automatically.

### Second integrated correction review

Both lanes remain **BLOCK** after fresh passing tests. Own-descriptor overlay, true instance/target checks, adopting/adopted checkpoints, atomic restore retirement, fork eligibility-before-render, and rebuild pruning materially address the original scenarios. Remaining/new concrete paths:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-39 | Major (A/B) | Public restore-promote retry rebuilds prepared temporary registration expectations against its own swapped/promoted live checkpoint. Resume the existing validated phase-specific operation instead. | Open; correction assigned |
| F7-40 | Major (A/B) | Final preload validation unconditionally demands suppression already legitimately handed off, blocking deliberate settled changes while a retained preload guard remains. Validate pending versus settled per-key obligations consistently. | Open; correction assigned |
| F7-41 | Major (A/B; remaining F7-38) | Unknown valid prior-guard libraries are discarded before final validation; tests assert that unsafe discard. Reconcile explicit origin evidence, refuse unexplained requirements before overwrite/start, preserve proven operator removal separately. | Open; correction assigned |
| F7-42 | Major (A; remaining F7-34) | Restore still calls standby-only FencePoolerDatabase, with preliminary/error RESUME and no explicit inspection-client cutoff. Strict restore fence must cut both aliases on both poolers without premature resume. | Open; correction assigned |
| F7-43 | Major (B) | Handoff uses desired managed values rather than actual higher-precedence operator/plain-auto configuration; removing a winning guard can enable copied background work. Prove permanent precedence and effective released keys before handoff. | Open; correction assigned |
| F7-44 | Moderate (B) | Old db/key/value handoff is honored before a fresh Pending obligation, without operation/data identity. New copied activation must re-establish suppression regardless of old handoff. | Open; correction assigned |
| F7-45 | Moderate (B) | Logical dump stanza switch updates cfg but sends stale old-stanza spec to routing verification/clear. Refresh current target spec, retain old identity separately. | Open; correction assigned |
| F7-46 | Moderate (B) | Routing-phase identity is validated after exposing render/reload; stale instance/stanza/registration can publish first. Validate every routing eligibility before publication across all callers. | Open; correction assigned |

The same sole fresh worker is correcting these precise paths automatically. Targeted evidence now explicitly requires actual hermetic Swap/rename with guards/endpoints, production restore-promote phase retries, strict fencing command failure traces, and a real engine-backed fork continuation (only host/PG effects injected), not whole continuation stubs. Two untracked test files were gofmt-unclean and one had an extra EOF blank line despite tracked diff-check passing; formatting correction included. No source findings waived, no new permission requested.

### Third integrated correction review

Both lanes remain **BLOCK**. Their prior concrete restore-retry, settled-validation, unknown-prior-guard, strict-fence, key-handoff and routing-prevalidation scenarios are addressed in original scope. Five introduced/cumulative gaps remain:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-47 | Major (A) | Independent pending fork scoped applies use shared global locks but persist whole configuration; concurrent continuations can lose each other's mandatory settings. Select exclusive continuation envelope before cluster locks; no lock upgrade. | Open |
| F7-48 | Major (A) | Permanent-proof quoted-value parsing truncates at a quote, falsely equating rendered off'suffix with off. Decode supported PostgreSQL escaping exactly or fail unproven syntax; never release guard on prefix equality. | Open |
| F7-49 | Major (B) | ReconcileCopyGuard skips existing followers, so their winning old preload list masks inherited new libraries; transitions also skip common copy/standby safety for no-preload custom SQL extensions. Preflight every effective requirement and refresh complete follower guard before configuration/restart/primary SQL. | Open |
| F7-50 | Major (B) | Permanent-proof keys are case-sensitive, unlike GUC names; uppercase higher-precedence activation can be ignored and suppression falsely released. Normalize setting-name semantics across source layers. | Open |
| F7-51 | Moderate (B) | Valid include_dir = 'conf.d' followed by a comment is misparsed as a different path, blocking settlement. Parse supported quoted include/comment grammar while retaining unknown-include refusal. | Open |

Oracle is resolving the narrow existing-follower refresh/preflight boundary, fork envelope and one permanent-proof lexer before the next source batch. Actual halfway Swap/guard preservation and public physical/dump promote retry evidence now execute; fork engine convergence remains stubbed and is still required to be exercised through the real engine, with only host/PG effects injected.

Oracle contract resolved and recorded in NOTES-phase-7-activation-corrections.md. Sole source worker assigned F7-47–51: shared follower-runtime safety/guard/effective planning (not all historical variants), record-aware exclusive continuation envelope with acquisition-decision retention, and a single complete-value/name-aware configuration lexer. Real-engine fork evidence explicitly required before return. No routine permission pause or finding waiver.

### Follower-runtime integration review

Both lanes remain **BLOCK**. The previous envelope and lexer scenarios are addressed; the new follower-runtime planner has six concrete construction/transition/policy defects:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-52 | Major (A) | Installed follower SQL query errors are ignored, silently omitting unsafe SQL-only extras. Fail closed unless complete operation-bound evidence truly establishes the set; preload metadata alone does not. | Open; fresh focused worker assigned |
| F7-53 | Major (A) | Runtime union is derived before guard-discovered retained descriptors are collected, so their metadata/settings/prerequisites are absent. Collect all required provenance before deriving the plan. | Open; assigned |
| F7-54 | Major (A/B) | Planned removal filters outgoing metadata before validating its known suppression, blocking cron/custom removal; execution drops transition context. Preserve outgoing evidence and retire obligations only after actual follower catalog absence/replay proof. | Open; assigned |
| F7-55 | Major (B) | Current libraries resolve through conflicting historical snapshots before current intent, making ordinary updates permanently ambiguous. Current inherited descriptors win their runtime variants; genuine retained extras retain strict historical checks. | Open; assigned |
| F7-56 | Major (B) | Follower verification checks guard-only settings, omitting required current ordinary values. Verify the complete ordinary-plus-mandatory runtime before primary configuration/SQL. | Open; assigned |
| F7-57 | Moderate (B) | Independent-primary changes now invoke copy merge validation and reject legitimate unknown copy safety. Keep ordinary validation separate from actual copy/follower safety gates. | Open; assigned |

A fresh sole worker owns this bounded planner correction; prior activation/lexer/envelope roots must remain intact. Read-only contract check also requested to ensure plain apply still preserves the approved deferred-restart behavior rather than inheriting direct-command token refusal indiscriminately. No new user policy/permission request.

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-58 | Major (approved-contract correction) | Shared executor/test advice applied direct-command all-token refusal to ordinary apply, conflicting with the approved deferred-restart model. Separate safe configuration staging/pending reporting from authorized runtime continuation; direct commands remain all-effects-zero without exact tokens. Required library/settings readiness and follower-first ordering still gate primary effects. | Open; authoritative doc-only oracle determination forwarded to active worker |

The oracle confirmed missing tokens alone cannot justify blanket ordinary-apply refusal. Safe follower staging may proceed while source runtime effects/SQL wait; specific unsafe/unprovable transitions still refuse. This supersedes overbroad earlier agent tests, not the approved plan. Worker is incorporating the distinction with dependent-settings tests; no new user decision.

### SQL-continuation readiness review

Latest independent full checks passed, including uncached tests and formatting, but both source lanes remain **BLOCK** on three concrete readiness gaps:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-59 | Major (A) | Post-effect catalog fallback reconstructs an incomplete plan, losing a preflight-discovered SQL-only disabling descriptor before guard write/reload/restart. Carry the complete identity-bound preflight obligations or stop before unsafe refresh. | Open; sole worker correcting |
| F7-60 | Major (A) | Primary extension SQL checks required-setting pending flags but not actual effective values. A reloadable mismatch with no pending flag can CREATE prematurely. Query resolved effective requirements before dependent SQL; preserve unrelated role work. | Open; sole worker correcting |
| F7-61 | Major (B) | Roles-only reconciliation sees converged configuration and exits transition gating despite missing desired source SQL, bypassing follower runtime verification. Gate actual dependent extension SQL continuation, not unrelated role changes. | Open; sole worker correcting |

Required regressions preserve a preflight SQL-only suppression through a later catalog failure; defer CREATE on ineffective reloadable settings and succeed after effective correction without restart tokens; and exercise handleApply with converged config, missing source extension SQL and a bad follower setting, proving zero primary extension SQL until corrected. No test pass waives source blockers. Maintenance and final whole-change reviews remain pending.

### Readiness correction rereview

Original F7-59–61 scenarios are addressed on source inspection; both lanes still BLOCK on four adjacent readiness boundaries. Fresh independent full checks/uncached tests passed without drift at tracked diff `614abb1dd1f5e31ce035b942d7545003bba61339e49b314c1f4cff808d3396fd`, full manifest `cc3991ff2f07008bcf4281aa30a9957290379937d3012439815ae2ebcde9a3e7` (08:30:45Z–08:34:09Z). Logs `/tmp/opencode/e7-05c6-*`; no activity flake, formatting clean.

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-62 | Moderate (A/B) | Unrelated source-role convergence still constructs follower targets/plans, so stopped or unreadable followers block work with no extension dependency. Decide selected source dependency work before follower availability/planning; test the production check path. | Open; sole worker correcting |
| F7-63 | Moderate (B) | Installed standalone extension UPDATE bypasses effective-setting readiness because roles correctly exempts installed extensions. Check actual resolved prerequisites immediately before dependent UPDATE without gating unrelated grants. | Open; assigned |
| F7-64 | Moderate (B) | Follower-enabled direct add verifies raw catalogue primary settings instead of supported global/database overrides. Share resolved primary expectation precedence at this boundary. | Open; assigned |
| F7-65 | Moderate (B) | Readiness quote-normalizes already-decoded current_setting output, falsely equating literal quote content with unquoted values. Normalize expected literals once, compare decoded actual values directly. | Open; assigned |

The sole worker is correcting these shared boundaries automatically. Verification gaps include an explicitly mismatched snapshot operation identity and production planning rather than an empty mocked follower plan. Live qualification remains separate; maintenance is still prepared and held.

### Expected-value representation correction

Lane B accepts the integrated E7-05 source boundary; lane A identifies one remaining major issue. Prior F7-62–65 scenarios are addressed in original scope. Independent checks passed with no drift at tracked diff `c539306f22065f398654784dcf6faea599150e961d150fdc3705a8506f2bdbb4`, full manifest `ccb21fd2bb780c3ae12f8dccd71717af4e56debdc940649163943dcb05285ff1` (08:57:35Z–09:00:52Z). Logs `/tmp/opencode/e7-05c7-*`; uncached tests/formatting clean, no activity flake.

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-66 | Major (A) | Descriptor settings are quoted configuration literals, but setup/database overrides are raw values quoted by rendering. Mixing then normalizing both loses literal quote content and can falsely permit CREATE/UPDATE or follower readiness. Decode descriptor literals before merging raw overrides; keep complete runtime expectations decoded and compare actual values directly. | Open; sole worker correcting |

Required tests cover global/database raw quote-content overrides in roles CREATE, follower verification and both direct UPDATE paths, plus per-cluster pgaudit precedence. One accepting lane does not waive the other lane's finding. Maintenance remains held until independent closure.

### E7-05 stage acceptance

F7-66 closed by both independent source lanes. Both now ACCEPT the cumulative implemented E7-05 boundary with no remaining actionable findings. Earlier F7-19–65 scenarios are accepted in their corrected scope; historical BLOCK entries above remain the audit trail, not current open findings. Fresh full required checks/uncached tests/formatting passed on complete manifest `36aaace7dcd715fd07a20be8efc6da2c5870383e0d27854c94043d0b9f94c8c6` (09:11:21Z–09:13:10Z), logs `/tmp/opencode/e7-05c8-*`. No activity flake.

E7-06 maintenance implementation is now active with one source writer. Final two whole-change E7-08 reviews, fresh E7-09 verification and separately authorized publication/live qualification remain required. Accepted unit/model evidence is not live PostgreSQL/PgBouncer/crash evidence.

## Maintenance stage review

Initial E7-06 implementation passed independent full checks and uncached tests on a stable 111-file tree (09:56:03Z–09:58:44Z), tracked diff `82d9bb7c22622690bd1f142509506e7a631be81a2694896c56da7aee7df67582`, full manifest `148ea2ae5648fdf9ed2f3b6382eb46e802e88c19a828f22f8e3b3b433a8baf4d`. Logs `/tmp/opencode/e7-06c1-*`; formatting clean, no activity flake. Targeted source verdict is **BLOCK**:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-67 | Major | Repack job rechecks captured protection from configuration loaded before locks, permitting unrestricted postgres execution after protection changed. Reload and validate target under acquired lock before selecting execution spec. | Open; sole worker correcting |
| F7-68 | Major | Standard postgres-owned pg_repack schema lacks public usage, so vaire_admin cannot call version functions. Use a narrow authorized probe after protected refusal, without permanent grants or exact-report escalation. | Open; assigned |
| F7-69 | Moderate | Space admission checks only configured PGDATA and accepts Unknown=true; external relation/index/database tablespaces can exhaust another filesystem. Establish actual destinations or refuse unsupported external tablespaces, and reject unknown repack space facts. | Open; assigned |
| F7-70 | Moderate | Auto window is checked before slow validation, not immediately before execution; a new rewrite can start after expiry. Recheck at final execution boundary, release locks and record window_ended. | Open; assigned |

Correction includes missing not-running/client-missing, cluster-lock-busy, exact timeout/identity and incomplete activity mapping assertions. No local harness or live operations are authorized; packaged/runtime behavior remains a qualification limit.

### Maintenance admission rereview

F7-67/68/70 corrected on source inspection; F7-69 remains partial. Independent full checks/uncached tests passed on stable tracked diff `444b66d87af4e0cec5985e4363035f444dc588e3c7f80d13eae7d6f3f03ef7da`, full manifest `7fc4f48eeb24e82d48112de5c442a6504d0bbfed86a32df7e254e43d2cc1aad8` (10:21:55Z–10:24:50Z), logs `/tmp/opencode/e7-06c2-*`; formatting clean, no activity flake. Source remains **BLOCK**:

- F7-69: explicit pg_default relations do not prove auxiliary log-table destination when database default or effective default_tablespace is external. Independently admit all auxiliary defaults or refuse; unknown measurements already corrected.
- **F7-71 (Moderate):** zero pg_repack exit status can mean timeout/invalid-index skip, not completed rewrite. Require reliable completion evidence and report manual/automatic skip/failure accurately without weakening no-kill-backend.

Sole worker correcting both, plus CLI deferred-version disclosure, final-window lock-release and identity/table-space test gaps. No source findings waived by passing mocked tests.

### Repack observability correction

F7-69 auxiliary destination proof is corrected. F7-71 remains open because a concurrent TRUNCATE can change the same relation's storage while this pg_repack invocation skips; process error plus storage change likewise does not prove partial rewrite. F7-70 regressed when a storage probe was added after the final deadline check. Independent full checks passed at tracked diff `caf174af6aa7e41ffd423af29889619dfdd88b88b1a29cf46e23ed2bc640f0e7`, full manifest `41920060944b10e82776552d795d721b3e40992a79b156c9f042a223881f3ad7` (10:48:35Z–10:51:10Z), logs `/tmp/opencode/e7-06c3-*`; passing models did not establish attribution.

Primary-source research and oracle review reject zero exit, storage change, or an unframed debug-marker regex as positive invocation proof. A bounded wrapper must keep actual execution but report an explicit unknown outcome whenever completion cannot be attributed, rather than inflate successful/partial rewrite counts. Known skip diagnostics remain diagnostics; forged marker text never certifies success. Sole worker implementing unknown counts/state/metrics/manual disclosure and moving the deadline check after all pre-execution probes. No new command/framework or user decision is required for truthful uncertainty.

### Final targeted maintenance closure

Unknown-outcome correction passed fresh independent full checks at tracked diff `c0e5b1d1d1a1580f1c68d6ef0c0795aeabe8c9a3fff7c69d863ec8b0ee92b56c`, complete manifest `49b2eedd8ab2159809151f133e6535aa777a1c3489984b94d6112af928faa5ec` (11:34:38Z–11:36:51Z), logs `/tmp/opencode/e7-06c4-*`. Source review accepted unknown policy and final deadline ordering but found skip consistency still used RepackRewritten, which returns false for changed OID/tablespace.

Orchestrator made the bounded correction: valid positive OID/relfilenode and nonempty tablespace are required, and all before/after storage fields must match for a confirmed skip. Changed OID/tablespace/node, invalid identity or unreadable facts become unknown. Added `TestRepackSkipRequiresIdenticalValidStorage`; focused node classifier/manual/auto tests passed with formatting/diff check clean. Source-only targeted rereview active; canonical documentation-only worker active independently. No overlapping source writers; final complete-tree verification awaits documentation freeze.

Targeted reviewer subsequently **ACCEPTED E7-06**, closing F7-67–71 in their corrected scope. Canonical documentation is finalized with publication/live evidence and unresolved qualification admission clearly separated. Enter final two independent whole-change E7-08 reviews and fresh E7-09 verification; earlier stage acceptance is not their substitute. All source/docs writers stopped for this freeze.

## Final whole-change review findings

Both independent E7-08 lanes returned **BLOCK**, despite passing E7-09 candidate verification. Reviewed tracked diff `58839d6a66730c946ae26d8fb0ef0d8a6a1238f1bab83ad574e05452d83197b3`, complete 116-path manifest `c86b6b52b6ced7c6ac4bd8327ef9f0d7b35fa6a0fc8d66a289879c5d2a6b4b8b`, stable 12:12:40Z–12:15:09Z; full chain/uncached tests/formatting passed, logs `/tmp/opencode/e7-09c5-*`. No activity retry. Deduplicated actionable roots:

| ID | Severity | Scenario / root correction | Status |
| --- | --- | --- | --- |
| F7-72 | Major (A/B) | Fresh bootstrap queries application database catalogs before CREATE DATABASE. Establish existence through postgres and correct creation/readiness ordering; fake must reject nonexistent targets. | Open; fresh sole worker assigned |
| F7-73 | Major (B) | Allowed pre-swap restore abort deletes restore but leaves its prepared activation fence orphaned. Atomically retire only matching verified pre-swap authority and reconcile safe routing/pause state. | Open; assigned |
| F7-74 | Major (A) | Repack admission and execution resolve unqualified names under different roles/search paths, permitting a different table rewrite. Carry resolved quoted schema/name/OID through every probe and execution. | Open; assigned |
| F7-75 | Moderate (B) | Manual multi-table repack checks window only during planning. Recheck after all probes before each execution; do not cancel a running table. | Open; assigned |
| F7-76 | Major (A) | Tune/deferred-maintenance direct engine callers bypass source/follower extension ordering. Reuse proper shared admission or refuse pending dependency transition before narrower-envelope effects. | Open; assigned |
| F7-77 | Major (A) | Direct follower-backed token planning omits unrendered configuration/journal drift, allowing mutation before discovering missing follower authorization. Read-only preflight every proposed live target before all effects. | Open; assigned |
| F7-78 | Moderate (A) | Textual setting equality permanently defers valid boolean spellings (true/on, false/off). Use actual PostgreSQL type-aware canonical semantics without changing exact string/quote-content behavior. | Open; assigned |

Fresh sole implementation worker owns this fixed final-review batch. No compatibility work, optional refactoring or new features authorized. Final reviews and fresh verification must rerun after correction; prior stage acceptance does not waive whole-change regressions.

## Final cumulative reviews

E7-08A and E7-08B not run. No final accept verdict, publication readiness or live qualification is claimed.
