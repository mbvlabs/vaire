# Phase 7 paused: preserved work, excluded from v1

Status: unfinished checkpoint, 2026-11-11. The operator chose to preserve this branch remotely and focus on releasing v1 without Phase 7. This is not approval to resume Phase 7 implementation, merge it, publish a release, or deploy it.

## Location and scope

- Branch: `feat/phase-7-extensions`.
- Worktree: `/home/mbv/work/vaire-phase-7`.
- Original base: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552`.
- Preserve the whole branch, including untracked implementation/tests/notes now included in the checkpoint commit.
- Do not merge this branch into the v1 candidate. Its canonical documentation changes describe the postponed feature and are not the v1 release scope.
- Original checkout and historical preserved snapshot were not changed by this preservation.

## Actual state at pause

Stage reviews accepted implementation through E7-06, but both final whole-change reviews blocked. Candidate verification before the final correction batch passed; it does not certify the current partial edits.

Final findings F7-72–78 are detailed in `FINDINGS-phase-7-review.md`: fresh database bootstrap ordering, pre-swap restore abort fencing, repack target identity/search-path resolution, manual repack window boundaries, tune/deferred-maintenance extension-order bypass, direct follower restart-token discovery for unrendered drift, and boolean effective-setting equivalence.

The last worker was stopped immediately at the operator's request. It reported partial fixes for findings 1, 3, 4, 5, 6 and 7; restore-abort fence cleanup was not started. Those edits were not independently re-reviewed. New regressions were added for findings 1, 5, 6 and 7; findings 2, 3 and 4 lacked their new regression coverage at stop. Do not infer closure from partial source changes.

## Preservation verification

The complete required pre-commit checks were executed without changing source:

| Command | Result |
| --- | --- |
| `go build ./...` | PASS |
| `go vet ./...` | PASS |
| `go test ./...` | FAIL |
| `golangci-lint run ./...` | PASS, 0 issues |
| `GOOS=darwin GOARCH=arm64 go build ./...` | PASS |
| `just lint` | PASS |
| `git diff --check` | PASS before this checkpoint note |

Current failing tests:

- `internal/node`: `TestRepackPlanNoTableSkips`, `TestAutoRepackOrderAndCounts`, `TestAutoRepackWindowStopsNewTables`.
- `internal/steps`: `TestRolesCheckReportsPGQueryErrors`.

Full local output: `/tmp/opencode/phase7-preserve/results.txt`. These failures were deliberately not repaired during preservation. This commit is an unfinished backup checkpoint, not a verified release candidate.

## Release focus

Proceed from the clean v1/main release line, not this feature worktree. Reconcile v1 scope to exclude Phase 7. Existing activity/follower qualification and release authorization gates remain; preserving this branch does not authorize a tag, merge, deployment or live test. Nothing from Phase 7 was installed or changed live.

To resume Phase 7 later, first obtain authorization, read this note and the findings ledger, inspect the partial edits, and establish a new verified baseline. Do not restart the previous open-ended review cycle or add backward-compatibility migrations: the approved pre-v1 contract does not require them.
