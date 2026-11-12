# External database copy review and verification

Status: integrated reviewer acceptance **not obtained**. The user explicitly waived additional pre-merge review and directed merge to master followed by testing. Final observer/prompt fixes are implemented and worker-verified; mandatory pre-commit checks are rerun before publication. No live qualification has run. Passing tests do not retrospectively change the recorded review verdict.

## Baseline

- Worktree: `/home/mbv/.local/share/opencode/worktree/0283f0/feat/external-database-copy`.
- Branch: `feat/external-database-copy`.
- Base: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552`.
- Initially no commits/publication were authorized. The user subsequently authorized commit/push/PR/merge to master/edge and explicitly waived another pre-merge review. No deployment or live source connection is included.

## Early C02 independent review

Read-only reviewer session `ses_e305ffe60ffe0svEmc1evEMYZf` inspected the copy-specific primitives and tests against the approved contract. This review excludes the evolving node lifecycle and does not constitute integrated feature acceptance.

| ID | Severity | Finding | Disposition |
| --- | --- | --- | --- |
| E01 | High | Source SQL/data can appear in arbitrary server error messages and continuation lines streamed or returned by preflight/export/restore. Prefix filtering is insufficient. | Resolved in original reviewer's first re-review: fixed bounded classifications, raw log suppression and leakage regressions. |
| E02 | High | Existing workspace root ownership/write permissions are not checked; pathname ownership/deletion operations can act on replaced directories. | Resolved by original reviewer after targeted second correction: trusted-owner checks apply to root/sticky ancestors and unsafe writes refuse before creation. |
| E03 | Medium | TOC-inspection removal errors and passfile removal errors after failed CA creation are ignored. | Resolved in original reviewer's first re-review: cleanup errors joined and regression-tested. |

Node lifecycle worker was notified immediately; corrections will be integrated with one source writer at a time and independently re-reviewed. Passing rootless tests is not proof of these properties.

E02 first re-review detail: production UID 0 bypasses ancestor ownership/group-write checks; sticky ancestors bypass owner trust even though their owner can replace entries. Enforce trusted ownership/write permissions for root too, permit only trusted sticky ancestors, and test simulated UID 0 without privileged execution. One incomplete correction cycle recorded; stop for user direction if a second correction cycle fails for this defect.

E02 second correction was accepted by the original reviewer: root and sticky ancestry is now checked, simulated-UID-0 regressions inspected, no remaining concrete E02 blocker. The second correction did not fail; the stop threshold was not reached.

## Early C03 independent review

Read-only reviewer session `ses_e3036b0eaffeBWDqL2PmWiF0gK` inspected node/steps integration, excluding the changing primitives and E01–E03. Verdict: BLOCK pending corrections and re-review.

| ID | Severity | Finding | Disposition |
| --- | --- | --- | --- |
| C03-01 | High | Verification SQL calls nonexistent `has_foreign_table_privilege`; parsing fails even without foreign tables. | Open: use supported table privilege functions; add generated-SQL regression and live-pending PG18 query qualification. |
| C03-02 | High | Extension availability demands a direct default-version installation script, rejecting valid installation/update chains used by supported contrib extensions. | Open: resolve valid install chains or authoritative available-version metadata; test reachable default and broken chain. |
| C03-03 | High | Ambiguous systemd startup is recorded as definite failure without job ID; terminal-record pruning can remove a still-active workspace. | Open: explicit unknown-start outcome and job ID, credential retention, verified liveness before pruning, accepted-unit/interrupted-client regression. |
| C03-04 | Medium | Required workspace cleanup failure after successful import is only a warning and still reports job success. | Open: structured failed/blocked outcome retaining imported target and truthful import status. |
| C03-05 | Medium | Preflight workspace age is treated as proof of inactivity; suspended active processes can lose their workspace. | Open: verifiable process/operation identity and inactivity, live-old-owner regression. |

Original reviewer re-review resolved C03-01 through C03-05 for their original defects. No live proof is claimed for mocked SQL or synthetic extension metadata. Two additional copy-private-spec hygiene defects remain:

| ID | Severity | Finding | Disposition |
| --- | --- | --- | --- |
| R1 | Medium | Shared spec consumption ignores unlink failure and copy can execute while its source credentials remain in the spec. | Open: copy consumption must fail closed with sanitized cleanup details; register decoded secrets before diagnostics; test no execution on failed unlink. |
| R2 | Medium | Inactive private-spec cleanup is skipped if the workspace root is absent/unreadable, retaining credentials from uncertain starts indefinitely. | Open: run verified-inactive spec cleanup independently of workspace listing, retaining active/unknown; test absent root and safe warnings. |

Evidence correction: MainPID/ControlPID branches, same-inode token mismatch and deliberately interleaved lease-acquisition-gap tests were not established by the inspected C03 correction tests. Closing a parent lease in a descendant test is not proof of parent-process death behavior. These are pending test evidence or live qualification, not claimed passes.

## Frozen integrated review checkpoint

Source frozen for C06/C07 at `2026-11-12T14:20:13Z`; integrated reviewer session `ses_e2fde5bd6ffezVzklmwKsh6rhv`, independent tester session `ses_e2fde2ad4ffeVNaLDYK11kqdAL`.

Feature diff SHA-256: `c5cf7e0b9eff817825e5b24853d77df83fed2ac449eb66b473706d9791f377a7`. This covers tracked changes plus every new untracked implementation/planning/operator document, excluding this evolving review, STATE and evidence output. No HEAD/base change. R1/R2 corrections are implemented and interim-tested, awaiting independent disposition. Whole-feature acceptance is still pending.

## Final verification

## Integrated C06 findings

Reviewer `ses_e2fde5bd6ffezVzklmwKsh6rhv` independently confirmed the first frozen diff identity and inspected tracked and new untracked feature files. Verdict: **BLOCK**. E01–E03, original C03-01–05 and R1/R2 are resolved for their reported defects; the following new findings remain:

| ID | Severity | Finding | Required correction |
| --- | --- | --- | --- |
| C06-01 | High | Installed-extension inventory always includes built-in plpgsql, but modern dumps normally omit its definition; selection requires it and rejects ordinary exports. | Distinguish compatibility inventory from actual dump definitions, narrowly permit the identified built-in omission, keep strict non-built-in checks, test a realistic TOC. |
| C06-02 | High | State-read/follow failure after successful launch is forwarded as an ordinary error; CLI can discard possibly installed credentials despite a known job ID. | Classify failed observation after launch as unknown, preserve job ID/credential handoff across initial/poll/reattachment errors in human and structured modes. |
| C06-03 | Medium | Individual source password prompt uses a trimming secret helper, changing valid surrounding/all-space passwords unlike URL input. | Copy-specific exact-byte masked password prompt in both renderers, preserving unrelated passphrase policy; test encoded-URL convergence. |

All are within the approved contract, no renewed product direction needed. One source writer is assigned a minimal correction batch; original integrated reviewer re-review and fresh C07 follow. Earlier test evidence below applies only to its recorded identity, not the forthcoming corrected tree.

### First integrated correction re-review

At final-checkpoint identity `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`, the original reviewer accepted C06-01 and C06-03 for their reported defects. C06-02 remains **High / BLOCK** for node-RPC observer cancellation: `followJob` returns raw context cancellation, while the independent copy unit can continue; direct-copy/reattachment must classify this as observation unknown and preserve credentials/ID, distinct from recorded terminal cancellation. This is one incomplete correction cycle for C06-02. A second failed correction requires user direction under the handoff.

New low finding C06-04: exact-byte plain password reader omits its prompt label, silently waiting on TTY when rich UI is unavailable. Print to diagnostic output before masked read and test order/no echo/exact bytes. Both findings are assigned a narrow correction batch. No publication has happened.

### Explicit user override of publication gate

The user subsequently instructed: “just merged it to master and we test”. This supersedes waiting for another independent review before merging. Finish the in-flight narrow corrections and repository-required pre-commit checks, then publish through the authorized feature PR to master/edge. Independent acceptance and live qualification remain unclaimed; final static/runtime risks are disclosed rather than silently marked passed. This does not authorize reading secrets or deleting/rebuilding live databases.

Final worker handoff: observer cancellation now rereads the job record so an already-recorded terminal outcome wins, otherwise carries `jobObservationError` through direct-copy and `job.follow` unknown-outcome handoff. Added running-observer-cancellation, definite recorded-terminal-cancellation and typed follow tests. Plain source-password prompt now prints its label before masked input; both renderer tests assert no echo and exact-byte preservation. Worker reported passing targeted tests, build/vet/uncached full tests/lint/Darwin build/just lint/whitespace. These final two fixes have **not** received independent re-review; that gate was explicitly waived, not silently satisfied.

## First frozen-tree verification

Independent tester completed C07 on the frozen feature identity above, unchanged before/after, during `2026-11-12T14:20:56Z`–`2026-11-12T14:23:13Z`. All required commands passed. Integrated review is still pending; these checks do not override a review finding.

Tool versions: Go `1.27.1 linux/amd64`, golangci-lint `2.14.0`, just `1.58.0`, git `2.53.0`; shellcheck/actionlint present through mise. Just differs from the repository's declared version; the installed version is recorded rather than misreported as pinned. Modules resolved locally with `GOPROXY=off`.

| Command | UTC start–end | Exit |
| --- | --- | --- |
| `go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint` | 14:21:26–14:21:39 | 0; all chained stages ran |
| `git diff --check` | 14:21:45 | 0 |
| `go test -count=1 ./...` | 14:21:49–14:22:14 | 0; uncached |
| `go test -count=1 ./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport` | 14:22:18–14:22:25 | 0; seven packages |

Full durable evidence, exact commands, tool facts and source-identity captures: [verification summary](evidence/external-copy/summary.md). Command logs are alongside it. The first chain's tests used Go's cache; the separate `-count=1` run supplies fresh execution. Lint reported zero issues, command metadata and 17 steps passed, shellcheck/actionlint/install-version checks passed. Darwin/arm64 is compilation only, not macOS runtime. No skips or blocked commands. Tests rebuilt only ignored `bin/vaire` besides explicitly retained evidence files; no source changes, installations or live actions.

## Corrected final-tree verification

C06 corrections were implemented, then source re-frozen at `2026-11-12T14:39:24Z`. Final feature diff SHA-256: `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67` (same exclusions). The independent tester confirmed that identity before, after and at final capture. Base/HEAD/branch unchanged. Original-reviewer re-review is pending; verification does not self-accept the fixes.

| Exact command | UTC on 2026-11-12 | Exit |
| --- | --- | --- |
| `go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint` | 14:40:05–14:40:24 | 0; all stages ran |
| `git diff --check` | 14:40:27 | 0 |
| `go test -count=1 ./...` | 14:40:31–14:40:55 | 0; uncached |
| `go test -count=1 ./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport` | 14:40:58–14:41:05 | 0; all seven packages |

[Final durable evidence](evidence/external-copy/final/summary.md) includes command logs and compact metadata/command/hash identity captures. First chain test cache was used; separate full/targeted runs were uncached. Go 1.27.1, golangci-lint 2.14.0, git 2.53.0 and installed just 1.58.0 were used; the declared just 1.43.0 deviation remains disclosed. No failures or skipped checks. No live/runtime proof or publication action is implied.

## Live limitations

Real PG18 archive/ownership/grants, TLS/provider behavior, systemd abrupt death, disk faults and credential/detach qualification are NOT RUN and require separate permission. See the runtime run sheet. No live fixtures were created.
