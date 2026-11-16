# Lint-cleanup behavior review

Date: 2026-11-16. Base: `b09b4635a711d03ac5eb49fe50bb6e5a8d7a220e`.

## Exact corrected source

Freeze: `2026-11-16T20:43:48Z`. All 688 source/test/build files are listed in
`source-manifest.sha256`, SHA-256
`d7a481dd99da76acc834aa85e6868cc4f852f2ffbca7c8fea573fe4d8cc9d345`.
Both final reviewers verified every hash before and after their review/checks.

## Independent verdicts

- Architecture/lifecycle reviewer: **ACCEPT corrected whole lint-cleanup diff**.
  Incorporates detailed backup, steps, config/CLI/doctor and node lifecycle
  source comparison, followed by the integrated correction review.
- Extension/integration reviewer: **ACCEPT behavior preservation**.
  Independently compared extension mutation, transition, activation, config
  intent and copy-plan refactors, plus their related node/steps boundaries.
- Test-coverage reviewer: **ACCEPT the 19 refactored node test files** and the
  pure pending-error type wiring. Test scenarios, assertion semantics and
  refusal-before-effects boundaries remain covered.

## Review findings corrected before acceptance

1. A prepared empty copy-guard refresh accidentally continued after guard
   absence. The explicit presence result and early return restore the
   standalone-primary no-op. A production regression checks an unknown-safety
   custom preload, no guard creation and unchanged records.
2. A shared stopped-apply test recorder could miss forbidden configuration
   effects. Its failure-boundary mode now runs before config classification;
   the original boundary assertion and a new exact recorder test cover it.
3. Follower-add incomplete/pending roles briefly changed public error reasons.
   The exact baseline combined branch, message and `extension_sql_failed`
   identity are restored. Four public-path regressions pin error, incomplete,
   warning and pending-restart outcomes, partial fields and effect order.

## Verification and limits

Reviewers independently passed the targeted regressions and package tests,
vet and unrestricted scoped lint. Broader checks on the initial snapshot do
not replace the dedicated final verification of the corrected source.

No linter configuration, thresholds or dependencies changed; no new `nolint`
or test skips were introduced. No reviewer modified source or performed
lab/live VPS operations. macOS and later deployment/runtime qualification are
separate from this Linux lint-cleanup acceptance.
