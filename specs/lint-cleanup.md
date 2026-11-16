# Lint cleanup

## Completion

The corrected frozen source passes both independent whole-change behavior
reviews and the independent final Linux gate:

- Unrestricted lint: **0 findings** after cleaning its cache.
- `just lint`: **PASS**, including metadata/18 steps, shellcheck, actionlint and
  installer pins.
- Fresh-cache build/vet and full uncached tests: **PASS**, zero test skips.
- Full repository race suite: **PASS**, zero reported races.
- Formatting/diff checks: **PASS**; all 688 source hashes unchanged.
- Lint configuration, thresholds, workflows, dependencies and `justfile` are
  unchanged. No new suppressions; 44 obsolete suppressions removed.

Durable evidence: `evidence/lint/final-verification.log`, `review.md` and the
corrected `source-manifest.sha256`. The default baseline report hid findings;
the unrestricted baseline was 259, now zero.

No lab/live VPS testing or macOS work occurred. Source integration/publication
and later real VPS qualification remain separate; do not assume the previously
deferred Darwin build blocker is resolved by Linux lint passing.

## Goal and boundaries

Make both `golangci-lint run ./...` and `just lint` pass without weakening
rules, increasing thresholds or introducing blanket suppressions. Preserve
the reviewed extension behavior and existing tests. No live VPS or lab work.
Do not touch the user's staged main-checkout `justfile`.

Branch: `fix/lint-cleanup`, based on merged master
`b09b4635a711d03ac5eb49fe50bb6e5a8d7a220e`.

## Baseline

The normal output reported 142 findings because per-linter and repeated-issue
reporting is capped. Unrestricted reporting found **259**:

| Linter | Count |
| --- | ---: |
| errcheck | 41 |
| errname | 1 |
| exhaustive | 4 |
| gocognit | 102 |
| gosec | 1 |
| ineffassign | 4 |
| nestif | 22 |
| nolintlint | 44 |
| prealloc | 22 |
| staticcheck | 1 |
| unparam | 14 |
| wastedassign | 3 |

Exact findings: `evidence/lint/initial.json` and `initial.log`.

## Implementation

Package/file ownership is assigned to workers. Prefer local guard clauses and
cohesive helpers, checked test errors, explicit exhaustive cases, removal of
dead assignments and obsolete suppressions. Preserve error identity, ordering,
locks, retries, tokens, effects and test assertions. No product redesign.

| Owner | Exclusive scope |
| --- | --- |
| `ses_e1a038077ffeCzH3Ni4NDI5o8w` | `internal/backup/**`, `catalog/catalog_test.go` |
| `ses_e1a035551ffe6zAylpKlvKGRg9` | `internal/config/**`, `internal/cli/**`, `internal/doctor/**` |
| `ses_e1a033516ffeXVgKIwFs8UYGUP` | `internal/steps/**` |
| `ses_e1a03051dffeBcawfCQBLbJ759` | node production `extensions.go`, `extensiontransition.go`, `extensionactivation.go`, `extensionintent.go`, `extensionplan.go` |
| `ses_e1a02e046ffekTa6zKsIbL231u` | all other node production files |
| `ses_e1a02ba2affexcQRDkWdLNcmtv` | node `extensions*_test.go` |
| `ses_e1a029a35ffeaSSXG9F9iUcJSx` | other node test files |

Parent coordinates cross-file signature updates and final integration. Workers
must not edit another owner's files. Their focused checks are intermediate,
not final frozen-tree evidence.

### Completed scopes

- Backup/catalog worker froze nine files after fixing all 42 owned findings:
  11 gocognit, 1 nestif, 28 obsolete nolint directives, 1 prealloc and 1 unparam.
  Scoped unrestricted lint reports zero; uncached tests and vet pass.
  Independent behavior-preservation review **ACCEPT** in
  `ses_e19fdbbddffexMWTQMpWqsRmPW`, including independent scoped unrestricted
  lint, uncached tests and vet. Lexer, guard origins/order, settings/history,
  selection/start semantics and retained test assertions were checked.
  Other package writers remain active.
- Steps worker froze 11 files after fixing all 30 owned findings. Scoped
  unrestricted lint is clean; uncached, race, repeated tests and vet passed.
  Independent review **BLOCKED** a missing-guard early-return regression:
  `ReconcileCopyGuard` continued after preparation returned an empty result for
  an absent guard, subjecting standalone unknown-copy-safety extensions to copy
  validation/writes. Worker owns the bounded restoration and a production
  no-guard/no-handoff regression; earlier passing tests do not close this defect.
  The worker has now restored an explicit presence result and orchestration
  early return. The new boundary regression failed on the broken version and
  passes after the fix; steps tests/race/vet/unrestricted lint pass. Independent
  re-review **ACCEPT** replaces the prior BLOCK. The reviewer independently
  passed the new regression, full uncached/race steps tests, vet and zero-issue
  unrestricted lint, and checked analogous helper fallthroughs.
- Remaining-node-tests worker froze seven test files, preserving refusal and
  public continuation coverage. An isolated baseline-plus-owned-files worktree
  passed full node tests/vet and had zero lint findings in those files.
  Shared-tree compile errors were observed during other active edits; these
  are not final integrated verification and must be rechecked after freeze.
- Config/CLI/doctor worker froze nine files. Scoped unrestricted lint, tests
  and vet passed against the isolated exact patch. Four reflect switches now
  explicitly enumerate unsupported kinds with prior error semantics; cohesive
  helpers reduce complexity. A named confirmation phrase avoids a gosec false
  positive without adding a waiver. Independent bounded review **ACCEPT**;
  config/doctor tests/vet/lint independently pass. Independent CLI checks were
  blocked by other active node compilation errors, so integrated checks remain
  necessary despite source acceptance.
- Extension-tests worker froze 12 files. Baseline-plus-owned-files isolated
  full node tests passed and owned-file lint is clean. Checked assertions,
  named fixture/assertion helpers and constant-parameter removal replace
  reported issues without claimed assertion loss. The independent test
  reviewer is checking all 19 frozen node test files.
- Test reviewer accepted the original seven files but found one coverage
  regression in the extension set: the shared stopped-apply recorder labeled
  config effects before its failure-boundary mode, hiding a forbidden config
  application. Parent moved the mode ahead of config handling and added a
  recorder regression for both config and roles steps. Focused production
  boundary/recorder tests pass. Independent re-review **ACCEPT** now covers all
  19 node test files and the pure type wiring. The reviewer independently ran
  stopped-start boundary regressions and confirmed no remaining coverage loss.
- Node lifecycle worker froze ten production files after fixing all 29 owned
  baseline findings. Real-tree build passes; isolated full node tests and
  unrestricted lint passed after the pending-error test wiring. Independent
  behavior review is running. Parent updated five test callsites to the renamed
  `extensionApplyPendingError`, including the pure type-only change in
  `apply_release_regression_test.go`.
- Extension node production worker remains active. No final shared-tree
  verification or zero-findings claim is made yet.

### Frozen integrated lint candidate

All source writers froze at **2026-11-16T20:33:19Z**. Source manifest:
`evidence/lint/source-manifest.sha256`, SHA-256
`4d34a1831f267801153d0a2290ad9021c62ed4b370a3a9cf8e7a096e847b55d9`,
687 files. The extension production worker completed all five files with
whole-node unrestricted lint at zero, node tests/build/vet passing. The
“active” notes above are historical.

Fresh independent whole-repository unrestricted lint, `just lint`, full
uncached tests/static checks and two production/integration reviews are
running. No result from those final gates is assumed yet. No lab or live VPS
operation is authorized by this lint task.

**Initial frozen candidate superseded:** final review found a follower-add
error-contract change in extracted `extensionAddRunRoles`. Baseline incomplete
or pending roles returned `extension_sql_failed`; the refactor introduced
different reasons/messages. The extension worker is restoring the exact
baseline contract and adding public follower-path regressions. Only that
helper and the dedicated new regression file are reopened. Preserve the old
manifest as `source-manifest-initial-superseded.sha256`; re-freeze and reverify
after correction. Passing lint is not behavior acceptance.
Both production reviewers identified the same follower error-contract defect
and found no additional concrete regressions in the remaining integration.
One independently passed full uncached tests/build/vet, node race tests and
zero-issue unrestricted node lint on the original snapshot. Replacement
acceptance still requires reviewing the exact fix and its new manifest.

### Corrected freeze

At **2026-11-16T20:43:48Z**, all writers froze the corrected source.
`source-manifest.sha256` now contains 688 files, digest
`d7a481dd99da76acc834aa85e6868cc4f852f2ffbca7c8fea573fe4d8cc9d345`.
The exact delta from the initial snapshot is the restored follower roles
branch plus `extensions_roles_partial_test.go`. Four public `handleExtAdd`
cases cover error, incomplete, incomplete-with-warning and pending-restart
outcomes, preserving baseline reason/message, intent, completed/pending,
restart flags and apply order. Worker node tests/vet/unrestricted lint pass.
Both independent final reviews and the fresh full verification gate are
running on this corrected freeze; results are not yet assumed.
The extension/integration reviewer now **ACCEPTS** the corrected whole
lint-cleanup behavior preservation. Exact two-file delta and all 688 hashes
were independently verified before/after; four public regressions, full
uncached node/steps tests, vet and unrestricted scoped lint passed.
The other independent reviewer also **ACCEPTS** the corrected whole diff,
including both fixes and all bounded reviews. Fresh public regressions, full
uncached node tests, node vet/lint, formatting and diff checks passed with all
688 hashes unchanged. The complete repository verification remains pending.

## Final gate

- Unrestricted lint reports zero findings, then ordinary lint and `just lint`.
- Fresh full uncached Go tests, build, vet, formatting and diff checks.
- Independent review of behavior-sensitive refactors, especially extension
  lifecycle and security boundaries.
- Source integration/publication and later live VPS testing remain separate.
  macOS work was previously deferred; report any remaining publication blocker
  rather than silently expanding scope or claiming a release.
