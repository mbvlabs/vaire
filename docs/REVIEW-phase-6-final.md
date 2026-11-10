# Phase 6 final review

## Publication preparation authorization

Focused publication documentation re-review: **ACCEPT**, all three procedure
blockers resolved, no source/test finding reopened. Runtime approval/inventory/
budgets/live qualification remain pending. LV-01 publication documentation is
ready as a procedure-and-explicit-gap map, not executable live permission.

Publication-doc review found three procedure-only gaps: rebuilt awaiting state
did not establish a completed owned recovery copy, pre-stop copying marker was
not boundary proof, and destructive slot SQL omitted its cluster socket/port.
These are corrected without source changes: B8 qualifies only running-follower
slot diagnostics while retained-copy preview/recopy is explicitly blocked;
B5 requires post-termination evidence or inconclusive/blocked classification;
the fixture-only destructive template selects verified source socket/port.
The checklist and coverage map carry the same limitations. Focused re-review
and final checks on these last doc edits precede the commit.

Independent precommit check of the preceding doc tree passed at
16:22:21–16:23:38Z, required chain plus full uncached suite; internal source/test
patch remained exactly `9c175d5a…3ef35ea`, matching the accepted code checkpoint.
Raw evidence `pub-precommit/`, manifest `d545c9b0…794d04`. Final doc corrections
still require another full pre-commit chain; old pass is not reused silently.

The user's request to get the feature merged into master for a new edge release
authorizes scoped commit/push/PR publication preparation for the reviewed
follower changes. No deployment, live activity/follower testing, branch/tag
creation, worktree cleanup or unrelated planning-document staging is implied.
Use the existing feature branch and PR path only. Planned correction commit:
`fix(followers): close final lifecycle and protection gaps`.

Fresh read-only GitHub observations: master remains `8d680566…feda4e`, release
line remains `ac742025…b29b19`, no feature PR exists and no remote feature branch
was listed. No integration/base change is required. Repository supports squash
feature merge; release-line merge-up rules are not involved.

Before publication readiness, the orchestrator compared the complete main versus
feature `MANUAL-TESTING.md` delta. Only the follower checklist/corrective cases
were added at the earlier point; existing dated results and the entire activity
section are preserved. A direct suffix comparison from the activity heading
through EOF returned equal. Main has no uncommitted activity evidence—only its
four separate untracked planning documents, which remain unstaged and untouched.
No main activity evidence was overwritten or silently imported.

LV-01 source-based run sheet `RUN-phase-6-live.md` maps every checklist case to
a procedure or explicit blocked gap, serial no-bucket then bucket, with no
invented live identities/headroom/budgets or approvals. Ineffective historical
fault instructions were removed/marked blocked in the checklist. Runtime values,
approved target/fixture scope and deterministic fault proof remain prerequisites
for execution, not publication evidence. Focused docs-to-code review and a fresh
full pre-commit chain are pending for this documentation delta. Source/tests
remain at the independently accepted checkpoint; no new source delta intended.

## Final local gate — PASS, uncommitted

FG-01 through FG-05 are complete to the **local source-review and verification
boundary**. Both independent reviewers accept the cumulative implementation
with no remaining actionable findings. Independent full-chain, targeted and
full uncached checks pass on the same frozen source/test tree. Historical
BLOCK/STOP/pending headings below are retained chronology, not current status.

Branch `phase-6-followers`, HEAD `2155a07300095d4845caedc37cd8c3d1c9928d73`,
base `8d68056621b19da318ce972552d45973b7feda4e`. Reviewed final tracked delta
SHA-256 `c9af17984e0bd4426425c75e5070f9fd107ecd84cc9bdff87fd644def92afb8f`.
Untracked SQL regression SHA-256
`209ea32299e34e565d539f6215b604c7497f201e38f2d1a3e94f2a30002d3912`.
These hashes identify the reviewed implementation plus then-current docs;
subsequent evidence-only documentation updates are not part of that hash.

All fixes remain **uncommitted**. No branch/tag creation, push, PR, merge,
publication, deployment, context/credential access, server operations or live
fixtures/resource cleanup occurred. Both legacy directories and main's four
untracked planning documents are preserved. No live qualification is implied.

Remaining gates: separately authorized commits/publication; AC-01 live activity
qualification; completed bounded LV-01 run sheet with actual approved resource/
target inputs; then separately authorized follower deployment and LV-02/03.
Extensions/beta work is not reached. Full pre-commit chain must be rerun before
any separately authorized commit, including these final evidence-doc changes.

## Current scope authorization — diagnostic/routing completion

Implementation update: both corrections are implemented, worker local full/
uncached checks pass, and the tree is freezing for independent verification
and both review lanes. Neither finding is independently closed yet.

The user authorized completing both B3-01 and AF-A1 and their check/re-review
cycle, explicitly asking not to repeat the same permission request while that
work remains incomplete. Implementation may proceed within this exact scope.
No acceptance or commit/publication/live authorization follows from this.
Historical stop statuses below record the prior checkpoints.

## Latest verdict — additional pass complete, two integration findings open

Lane A: **PASS WITH ONE MINOR**. Lane B: **BLOCK** on introduced diagnostic
acquisition behavior. The four targeted fixes and original nine findings are
accepted on source review; **B3-01 (medium, blocking review) and AF-A1 (minor)**
remain open. Independent local full-chain/uncached verification passes. No
further source writer is active; user disposition is needed before another
correction. Historical status headers below reflect previous checkpoints.

## Current execution authorization

Latest: the explicitly authorized additional corrective pass is implemented;
all four remaining findings await independent acceptance on a new frozen tree.
No further source writer is active. Historical STOP/BLOCK entries below remain
evidence, not a claim that the latest fixes are independently accepted.

The user responded affirmatively to the explicit request for one additional
targeted correction pass on the four remaining findings. Implementation may
resume for that scope only, followed by independent verification and both
review lanes. The stop below was observed; it is not being bypassed. No finding
is closed by authorization. No commit, publication or live permission is given.

## Current verdict — STOPPED after second corrective attempt

Both independent lanes still return **BLOCK**. Consolidated remaining findings:
**three majors, one minor**. Independent local verification passes; this does
not refute uncovered source-level failures. No third corrective pass has been
started. Explicit user disposition is required before further source writes.
All earlier checkpoint/status prose below is historical evidence where it
conflicts with this latest disposition.

| Current IDs | Severity | Remaining outcome gap |
| --- | --- | --- |
| C2-A1 / B2-01 | Major | Unreadable/corrupt transition records become an empty pooler exclusion set, reopening unsafe protected access |
| C2-A2 | Major | Ordinary-follow progress can resume after an irreversible unfollow has taken ownership of the same target |
| C2-A3 | Major | General apply can initialize a prepared unfinished follower as an empty primary outside its owned copy lifecycle |
| C2-A4 / B2-02 | Minor | Fresh-follow floor refusal suggests configuring the not-yet-existing follower |

Remaining concrete correction directions and reviewer evidence are recorded
at the end. No live activity/follower testing, publication, commits, branches,
tags, remote mutation or resource cleanup occurred. Main's four untracked
planning documents and both legacy directories remain preserved.

## FG-01 checkpoint — 2026-11-10

Observed at 10:41:33 UTC. Execution follows main checkout's
`docs/HANDOFF-followers-final-gate.md` and approved
`docs/STATE-OF-THE-UNION-2026-11-10.md`.

| Item | Observed value |
| --- | --- |
| Follower directory | `/home/mbv/work/vaire/.claude/worktrees/phase-6` |
| Branch / HEAD | `phase-6-followers`, `2155a07300095d4845caedc37cd8c3d1c9928d73` |
| Base / master / merge base | `8d68056621b19da318ce972552d45973b7feda4e` |
| Release line `1.x` | `ac742025e5e9c5a015e6217657bcb2c0d2b29b19` |
| Code review range | `8d68056621b19da318ce972552d45973b7feda4e...2155a07300095d4845caedc37cd8c3d1c9928d73` |
| Feature status before reconciliation | Clean; eight commits, 86 files, 9,628 insertions / 215 deletions |
| Main status | Four untracked planning documents; no tracked changes |
| GitHub branches (read-only API) | `master` and `1.x` match the SHAs above |
| GitHub PRs for feature, all states | None (`[]`) |

The four main planning documents are `HANDOFF-followers-final-gate.md`,
`HANDOFF-planner-handoff-workflow.md`, `PLAN-planner-handoff-workflow.md` and
`STATE-OF-THE-UNION-2026-11-10.md`, all under `docs/`. Preserve them separately.
Legacy `.git` pointers still name `/home/mbv/work/eru/.git/worktrees/phase-6-followers`
and `/home/mbv/work/eru/.git/worktrees/agent-phase7-extensions`; read only, not
repaired or removed. No base divergence or branch operation is needed.

Owner-only runtime evidence directory:
`/tmp/opencode/followers-final-gate-20261110`; initial output: `checkpoint.txt`.
No server, installed binary, context or signed release identity was inspected.

Permissions: local investigation, documentation reconciliation, independent
reviews and build/unit/lint checks only. Source/test fixes require concrete
FG-05 authorization. No commit, branch/tag, push, PR, merge, publication,
deployment, upgrade, live fault injection or resource deletion is authorized.

## Gate status

Latest checkpoint: first FG-05 re-review remained BLOCK despite independent
full/uncached local PASS. A second corrective lifecycle/security attempt is now
implemented and awaiting independent re-review and final-tree verification.
If the same unit remains incomplete, stop rather than attempt a third pass.

Current update: authorized FG-05 changes are implemented but uncommitted;
independent final-tree re-review/verification is pending. Original review/check
outcomes below remain evidence of the pre-fix tree, not current completion.

- FG-01 complete; immutable code checkpoint named above.
- FG-02 complete: historical ledger reconciled against commits/test symbols.
- FG-03A / FG-03B complete: independent whole-diff verdicts **BLOCK**.
- FG-04 incomplete: build/vet passed, tests failed; subsequent chain not run.
- FG-05 awaiting concrete fix authorization and design-question disposition.
- AC-01 and all publication/live operations remain separately gated.

Historical successful builds and focused reviews are not fresh final-gate or
live evidence. Phase 6 remains built, live qualification pending.

## Frozen review input identity

Both independent reviewer sessions inspected the entire code range above plus
the FG-02 findings/work-units documentation delta and this report's initial
checkpoint. No writer was active during reviews or verification.

- Tracked reconciliation patch SHA-256:
  `32f5e5d08c0c7efe3afd262b945e8ad8f1b53b875dbf23d88eee8012d5cf28ac`.
- Initial untracked report SHA-256:
  `e8acac42f4d26413394b89bee1897932a2e30510a4093ca02ced2c3fb535b525`.
- These hashes identify the reviewed input, not subsequent report updates.
- Reviewer A session: `ses_e3af0a85bffes6Jz4QIrfnHORx`.
- Reviewer B session: `ses_e3af07a5dffehTgy6IXIMpUZWk`.
- Tester session: `ses_e3af04bb9ffecqFS4S077S2dFJ`.

The reviewers worked independently without reading each other's conclusions.
Both available `reviewer` sessions completed whole-diff reports; there is no
claim of distinct-model diversity. Neither reviewer ran tests or live commands.

## Independent reviewer A report — BLOCK

Reviewed all historical closures and final small corrections. Findings:

| ID | Severity / current location | Failure, requirement and minimal correction / regression |
| --- | --- | --- |
| A-1 | Major; `internal/node/protectwrite.go:186–196`, `follower.go:1049–1057`, `sql.go:38–68` | Protected follower is promoted during unfollow, then the job fails before clearing `Follows`. Subsequent write SQL skips TOTP on configured followers despite writable PostgreSQL. Refuse follower write SQL at the node boundary. Add `TestSQLWriteRefusesFollowerAfterInterruptedUnfollow` through the actual wrapper; assert no SQL execution without authorization and retain protected-primary tests. |
| A-2 | Major; `internal/node/follower.go:599–603,640–643,561–571` | Adopted-but-incomplete recovery bypasses slot inspection; missing/lost/unreserved slot can reuse unsafe data and clear awaiting after streaming warning. Preserve owned progress until completion and revalidate retained slots after adoption. Add `TestRecoverAdoptedCopyRequiresRetainedSlot`, including preview and unread-slot fail-closed cases. |
| A-3 | Major; `internal/node/jobs.go:258–263,312–335`, `follower.go:518–525` | Recovery loads configuration before exclusive lock and never refreshes it; another writer can change source floors/resources while it waits. Reload/revalidate config, target and awaiting/source state under locks before effects. Add `TestRecoverFollowReloadsDesiredStateAfterLock`, including removed/replaced/no-longer-awaiting targets. |
| A-4 | Minor; `internal/node/follower_test.go:565–593`, `budget.go:30–33,131–133` | Retry regression reads real host memory and fails before its intended injected boundary. Introduce deterministic test host facts while keeping real admission checks and insufficient-headroom coverage. Actual FG-04 failure confirms this gap. |
| A-5 | Minor; `internal/health/follower.go:120`, `docs/IMPLEMENTATION.md:1263` | NULL/malformed `safe_wal_size` becomes `-1` meaning no cap, including lost/missing slots. Omit unknown allowance; establish unlimited only from positive evidence. Add `TestParseFollowerSlotSafetyUnknown` covering lost, missing, malformed and genuinely unlimited cases. |

Historical dispositions: R1-1/2/3 accepted on source/assertion review, including
underflow and retry-preview corrections. R2-1 reopened by A-2; R2-2/3/5/6
accepted for their original scope; R2-4 unfollow fix accepted but recovery
freshness remains A-3. R3-1/2/3/4 accepted for original scope. R4-1/2/3/4/5/6
accepted for principal semantics, with additional slot-safety unknown defect A-5.
Final lifecycle P1/P2 correct before the adopted marker, incomplete after it.
Q1a implemented but retry execution blocked; Q2 partial-transition protection
unsafe under A-1; Q3 has A-5. Acceptance is not test execution or live closure.

Separate question: ordinary follow has ownership/resumable admission only for
`Recover=true`. Process death during ordinary copy can leave desired state,
partial paths and retaining slot without admitted follow/recover retry. Clarify
the progress contract rather than inventing a manual repair procedure.

Coverage gaps: real process death inside adoption/marker operations, real
privileged config-write failure, pooler authentication and both seeding paths.
Lower-level adoption file tests are stronger than stubs but not live crash proof.

## Independent reviewer B report — BLOCK

Reviewed the same entire diff independently. Findings:

| ID | Severity / current location | Failure, requirement and minimal correction / regression |
| --- | --- | --- |
| B-01 | Major; `internal/node/follower.go:599–603,640–643,566–578`; `follower_lifecycle_test.go:841–855` | Same adopted-slot gap as A-2; existing adopted preview test codifies unsafe exemption. Add `TestRecoverRevalidatesAnAdoptedUnfinishedCopysSlot` with missing/lost/unreserved/restartless/unreadable slots, foreign-path preservation, awaiting persistence and retained reuse without reset. |
| B-02 | Major; `internal/node/jobs.go:413–417,547–552`, `follower.go:548–555`, `db.go:356–376` | Ordinary follow rollback calls `forgetDatabase` under shared locks, or no acquired lock on skipped-job cleanup. Concurrent load/remove/save resurrects removed entries or overwrites another writer. Serialize the real rollback transaction, including `notRun`. Add `TestFailedFollowsSerializeDesiredStateRollback` and `TestSkippedFollowRollbackWaitsForDesiredStateWriter`. |
| B-03 | Minor; `internal/cli/recover.go:168–172,265–267,291–310`; `internal/node/follower.go:582–583,870–889` | Follower recovery structured warnings remain nested and human summary says following again despite streaming/pooler/metrics warnings. Forward original warning codes and qualify human completion. Add `TestRecoverPreservesFollowerJobWarnings` with JSON-decoded results. |
| B-04 | Minor; `internal/cli/restore.go:129–133`, `db.go:406–415` | Combined restore-promote and credential rotation check protection first; unprotected credentials dry-run locally succeeds for categorically refused follower. Refuse follower before protection/confirmation/secret generation/dry-run success. Add `TestCombinedRestorePreflightRefusesAFollowerFirst`, `TestCredentialsPreflightRefusesAFollower` across protected/unprotected and dry-run/actual paths. |
| B-05 | Minor; `docs/MANUAL-TESTING.md:472`; `internal/node/db.go:142–147` | Checklist expects replication object on stopped list rows, but list omits it. Separate status source-slot expectations from list's stopped/follows-only output. Add node acquisition-level stopped-list regression, not only manually built serialization rows. |
| B-06 | Minor; `internal/node/follower_test.go:565–593`, `methods.go:384–386`, `budget.go:131–144` | Same nonhermetic retry regression as A-4. Fixed sufficient host facts must reach both attempts; keep deterministic insufficient-headroom refusal before apply. Not evidence of defective production admission. |

Historical dispositions: R1-1/2/3 accepted; R2-1 reopened by B-01;
R2-2/3/5/6 accepted; R2-4 unfollow/recovery lock-mode fix accepted but whole-feature
writer safety open under B-02. R3-1/3/4 accepted; R3-2 standalone promote/undo
accepted but broader matrix incomplete under B-04. R4-1/2/3/4/5/6 accepted for
their original scope. Final copied-phase slot P1/P2, numeric underflow,
retry-preview, protected standalone restore ordering, known-failure precedence
and four-way JSON omissions were explicitly inspected. No fresh execution claimed.

Separate question: new follow after deferred source shrink may compute floors
below still-running/control-file source floors. Decide whether to refuse until
source restart or seed effective safe floors; exact live failure not established.
Other gaps include unhelpful stanza-based recovery preview text and node
acquisition coverage beyond constructed JSON rows. Live behavior remains pending.

### B method inventory

Every registration in `internal/node/methods.go` was inventoried. Allowed below
means follower-specific policy only; normal confirmation, protection, awaiting,
fencing and existence guards still apply.

| RPC(s) | Follower policy |
| --- | --- |
| `plan`, `apply` | Allowed; awaiting excluded from engine; explicit awaiting apply refused; plain-apply ordering limitation retained |
| `selfcheck`, `cluster.bootstrap`, `cluster.confirm-secrets` | Node/global allowed; configuration cannot manufacture physical follower |
| `ssh.lockdown`, `ssh.confirm`, `ssh.status`, `upgrade.install` | Node/global allowed |
| `config.get`, `config.set`, `config.validate` | Allowed; follows immutable/new follower entry refused |
| `protect.init`, `protect.confirm`, `protect.status`, `protect.verify` | Allowed; independent copy protection |
| `db.list`, `status` | Allowed; list only reads running followers, status reads stopped source slots |
| `db.create` | Ordinary creation only |
| `db.drop` | Follower allowed subject to protection; source with followers refused |
| `db.resize`, `tune.show`, `tune.apply` | Allowed; Q1a floor refusal/order applies to source resize |
| `db.credentials` | Rotation refused at node; B-04 CLI mismatch |
| `logs`, `tls.status`, `metrics.show`, `metrics.rotate` | Allowed |
| `backup.now`, `backup.verify`, `backup.list` | Explicit follower refused; aggregate paths skip/list stanza entries only |
| `job.list`, `job.follow` | Allowed |
| `restore.start`, `restore.promote`, `restore.undo` | Follower refused; source allowed with divergence warnings on switch |
| `restore.status`, `restore.abort`, `restore.cleanup` | Existing inspection/record cleanup allowed subject to preconditions |
| `restore.drill`, `db.fork`, `dump.create` | Explicit follower refused; aggregate drill skips |
| `db.follow` | Ordinary/protected source allowed; follower source refused; rollback B-02 |
| `db.unfollow` | Follower/protected follower allowed; resumable transition admitted; awaiting refused |
| `sql` | Alias routes to source-named DB on follower port; see A-1 cross-cutting protection finding |
| `db.write-access` | Follower refused before grants/code check |
| `activity.list`, `activity.show`, `activity.health` | Metadata-only reads allowed/unrecorded |
| `doctor.run` | Allowed; follower checks and backup skips |
| `queries.top`, `queries.reset`, `maint.report` | Allowed; underlying database name |
| `maint.run` | Explicit follower refused; aggregate write-maintenance skips |
| `maint.kill` | Allowed; underlying DB for SQL, alias for token/output |
| `master-backup.now`, `master-backup.list`, `master-backup.get` | Global allowed; desired state included, no independent follower backup |
| `db.recover` | Allowed after source recovery; time/no-promote refused; B-01/B-03 |

B independently treated SQL as standby-refused; the A-1 annotation above is
orchestrator cross-reference after both reviews, not B's original conclusion.

CLI/scheduled iteration audit: aggregate backup now/verify/list, restore drill
and write maintenance skip followers. Reports/query inspection/reset include
them using underlying database names; explicit write maintenance/repack refuses.
Recover-all orders sources first and final full loop excludes follows results.
No-bucket guidance drops followers first, recreates sources then re-follows.
Daily/weekly maintenance, pgBackRest and backup timers exclude followers;
spool allocation counts stanza-bearing entries. Exporter probes use underlying
DB names. Both poolers retain alias with source dbname and follower-local auth
alias. SQL/psql/console/tunnel use alias routing. Follow/unfollow methods/jobs,
dry-run and refusal reasons participate in metadata-only activity. Credential
uncertainty persists flags/reasons, never credential values.

## FG-04 independent verification

UTC 2026-11-10 10:46:17–10:49:21; chain started 10:46:21.
HEAD/tree stable: HEAD above; Git tree `e3adcbc74d9506f7248e2f8a639d7f1a668840ca`.
Tracked/untracked input hashes matched the frozen review identities. No
unexpected working-tree changes. Owner-only evidence: `fg04-*` files in the run
directory, indexed by `fg04-evidence-sha256.txt`; summary
`fg04-verification-summary.txt` SHA-256
`453122d5…15a1696` (abbreviated; consult index for full hash).

Mise-pinned tools: Go 1.27.1, golangci-lint 2.14.0, just 1.43.0;
shellcheck 0.11.0 and actionlint 1.7.12 installed.

| Command | Exit / UTC result |
| --- | --- |
| `go build ./...` | 0; 10:46:21–10:46:31 |
| `go vet ./...` | 0; 10:46:31–10:46:33 |
| `go test ./...` | 1; 10:46:33–10:47:03 |
| `golangci-lint run ./...` | Not run; chain stopped |
| `GOOS=darwin GOARCH=arm64 go build ./...` | Not run; chain stopped |
| `just lint` | Not run; chain stopped |
| `git diff --check` | 0 |

First actual failure: `TestResizeAppliesFollowersFirstOnRetry`,
`internal/node/follower_test.go:593`: app needs 344 MiB startup shared memory,
but CommitLimit headroom is zero (23.1 GiB limit, 36.3 GiB committed).
Uncached node/focused reproduction has the same failure. Both reviewers classify
the new test's real-host dependency as an actionable test defect, triggered by
environment pressure, not a production admission bug. No resource manipulation
or safety bypass was attempted.

Uncached targeted `internal/backup`, `internal/steps`, `internal/health`,
`internal/doctor`, `internal/cli`, `internal/config`, `internal/tune` passed;
`internal/node` failed at the same boundary. Historical activity race gap remains.

## FG-05 disposition and stop gate

Consolidated **four majors and five minors**: A-2/B-01 and A-4/B-06 are
duplicates, not separate defects. All nine remain open pending authorization.
R2-1 is reopened; R2-4's original fixes do not establish whole-feature locking
and freshness. Source/assertion acceptance of other historical items is not
an executed or live pass.

Proposed root-cause batches (not authorized):

1. Node write authorization: A-1 and real interrupted-unfollow SQL regression.
2. Lifecycle/desired-state consistency: A-2/B-01, A-3 and B-02; marker ownership,
   adopted-slot recopy, post-lock reload and locked rollback, including no-lock
   skipped-job cleanup. Cross-cutting: both reviewers must reassess whole-diff
   guarantees after fixes.
3. Unknown slot allowance: A-5, parser/JSON/docs and tests.
4. CLI warning/refusal integration: B-03/B-04, actual/dry-run and human/JSON tests.
5. Deterministic local regression and stopped-list checklist: A-4/B-06, B-05.

Resolve ordinary-follow interruption and deferred-shrink/new-follow questions
before authorizing changes that choose new behavior. A behavioral decision goes
through Plannotator; no unapproved implementation or user gap acceptance is
inferred. Full verification must be rerun after fixes. No commits, source edits,
publication, live operations or fixtures were created by this final-gate pass.

## Authorized FG-05 implementation record

The user authorized the nine finding corrections, then explicitly approved both
additional lifecycle outcomes: safe public ordinary-follow retry and targeted
refusal when effective source floors make new-follow admission unsafe. A
redundant Plannotator gate was stopped at the user's request (exit 143); approval
comes from chat, not dismissal. No further approval gate is requested for that
same scope. Commit/publication/live permissions remain absent.

Three serial implementation workers, one writer at a time:

| Batch / evidence subdirectory | Implemented scope | Worker verification |
| --- | --- | --- |
| `fg05-batch1` | FG-M1 node write refusal after partial unfollow; FG-m3 structured recovery warnings; FG-m4 follower-first combined restore/credential preflights | Build/vet and relevant uncached regressions passed; full tests still blocked by known host-memory test. Lint, Darwin build and just lint passed separately, not as completed chain. |
| `fg05-lifecycle` | FG-M2 adopted-slot revalidation and owned recopy; FG-M3 locked reload/identity checks; FG-M4 serialized rollback; approved public retry/effective-floor admission additions | Relevant uncached regressions and separate lint/build checks passed; full tests still hit known host-memory test. Worker reports targeted rollback race test passed; this does not qualify historical activity race coverage. |
| `fg05-minors` | FG-m1 deterministic host facts with real budget checks; FG-m2 independently unknown/measured/unlimited slot allowance; FG-m5 stopped-list acquisition regression and checklist correction | Full required chain passed at 13:17:41–13:18:05 UTC, uncached eight-package targeted run and focused regressions passed; diff check passed. Independent final verification still required. |

Evidence directories are owner-only children of the initial run directory.
Worker sessions: `ses_e3a9abc09ffewIbQ8JDnlqkca1`,
`ses_e3a8e4d63ffeffBgDyoUYAtAIl`, `ses_e3a7201c9ffeOuiWixVKiSoLhC`.

Principal new regression symbols (source inspected, execution as reported above):

- Security/CLI: `TestSQLWriteRefusesFollowerAfterInterruptedUnfollow`,
  `TestRecoverPreservesFollowerJobWarnings`,
  `TestCombinedRestorePreflightRefusesAFollowerFirst`,
  `TestCredentialsPreflightRefusesAFollower`.
- Lifecycle: `TestRecoverAdoptedCopyRequiresRetainedSlot`,
  `TestOrdinaryFollowRetryResumesOwnedProgress`,
  `TestOrdinaryFollowRetryAfterStartFailure`,
  `TestOrdinaryFollowRetryRefusesForeignOrReplacedProgress`,
  `TestRecoverFollowReloadsDesiredStateAfterLock`,
  `TestFailedFollowsSerializeDesiredStateRollback`,
  `TestSkippedFollowRollbackWaitsForDesiredStateWriter`,
  `TestFollowFloorMismatches`, `TestFollowRefusesUnsafeSourceFloors`,
  `TestRunningFloorsOf`.
- Minor corrections: fixed `TestResizeAppliesFollowersFirstOnRetry`, new
  `TestResizeRefusesInsufficientCommitHeadroom`,
  `TestDBListStoppedFollowerDoesNotReadReplication`,
  `TestParseFollowerSlotSafetyUnknown`, updated doctor/CLI JSON cases.

The slot representation now omits unknown allowances independently of side
availability. Positive effective-cap evidence distinguishes genuinely unlimited
capacity (`slot_uncapped: true`, legacy `-1` sentinel) from measured negative
allowance. Both reviewers must assess this correction and its docs/contracts.

The lifecycle delta is cross-cutting: both lanes must reassess the whole-diff
guarantees, not only check test names. Ordinary follow now serializes other node
mutations during copying, a conservative concurrency cost of protecting its
desired-state transitions. Test seams retain real production admission logic.

No live coverage was added. Real process death, root-level filesystem failures,
pooler/authentication, slot invalidation and effective PostgreSQL behavior remain
live-pending. Systemd start failure uses a shared rollback helper in tests, not
a real systemd-run failure. Helper-level CLI preflight tests do not drive an
end-to-end fake-node session. All current findings remain pending independent
re-review; no final closure is inferred from worker checks.

## First FG-05 re-review and second corrective attempt

Both lanes independently blocked the first fixed tree (`eae3bf1d…3086f9`).
They accepted all five original minors and the copied/adopted slot decision,
locked reload and serialization structure, but found six major remaining gaps:

| A / B IDs | Major gap | Second-attempt correction / regression entry points |
| --- | --- | --- |
| A-R1 / RB-01 | Ownership rejected legitimate empty follower awaiting stanza; fixture hid the real rebuild shape | Accept exact empty stanza with real awaiting identity; `TestRecoveryAcceptsRealEmptyFollowerAwaitingStanza` |
| A-R2 / RB-02 | Skipped same-operation retry could delete owned or completed work after waiting for lock | Explicit fresh versus retry; retry never rolls entry back; fresh cleanup rechecks progress after lock; `TestFreshFollowCleanupChecksProgressAfterLock`, `TestFreshFollowCleanupOvertakenByCompletion` |
| A-R3 / RB-02 | Structural name/node/port/source hash allowed drop/recreate ABA | Managed opaque in-flight generation, immutable through operator config paths, bound to jobs/ownership, completion clears atomically; `TestStaleGenerationCannotClaimReplacement`, `TestOrdinaryFollowCompletionClearsGeneration`, `TestRetireFollowOwnershipAfterDrop`, `TestFollowGenerationIsManagedAndImmutable` |
| A-R4 / RB-03 | Protected read-intent SQL could escape read-only framing after promotion using inherited writable admin authority | Durable unsettled transition SQL refusal; pooler exclusion and strict pre-promotion fence; real-name admin policy settlement before exposure; `TestSQLReadEscapeRefusedForProtectedUnsettledUnfollow`, `TestUnfollowFenceFailureRefusesPromotion`, `TestUnfollowSettlesAccessBeforeExposingTheAlias` |
| A-R5 / RB-05 | Running but unfinished owned ordinary copies were rejected before progress inspection | Completion derives from durable generation/progress, not socket existence; `TestRunningUnfinishedFollowIsRetried` |
| A-R6 / RB-04 | Floor preflight raced locked configuration reload | Exact proposal checked under preparation and execution locks without duplicate target; `TestFollowFloorsCheckedUnderPreparationLock`, `TestFollowRetryFloorCheckUsesCurrentFacts` |

Two additional B minors: RB-06 restart hint was ineffective for membership-driven
CPU tuning mismatch; RB-07 IMPLEMENTATION described old adopted exemption.
Second lifecycle worker changed diagnostics to explicit settings prerequisite,
added `TestMembershipFloorMismatchNeedsExplicitSettings`, and reconciled the
implementation reference. No new behavioral approval was needed for these
already authorized outcomes. Detailed agent routing is retained in
`HANDOFF-followers-fg05-correction.md`, not a new user review gate.

Independent verification of the first fixed tree: 2026-11-10 13:21:26–13:24:49Z,
required chain all exit 0, eight uncached targeted packages and supplemental
uncached full suite all pass. The first supplemental attempt under verifier
umask 077 failed three activity-mode expectations; normal umask 0022 rerun and
focused mode cases passed. This verifier setup artifact is preserved, not erased.
Evidence: `fg04-final/fg04f-evidence-sha256.txt`, summary `bf07a7ca…0390aaea`.
Historical activity race gap remains. Supplemental linter cache remains at
`/tmp/opencode/fg04-final-golangci-cache`; no cleanup performed.

Second corrective implementation workers completed serially:

- Lifecycle session `ses_e3a8e4d63ffeffBgDyoUYAtAIl`; evidence
  `fg05-lifecycle-correction2/`. Full uncached suite, build/vet/lint/cross-build/
  just lint and focused/race regressions reported passed. Security was excluded.
- Security session `ses_e3a36e952ffe1iNr48FC8ZQc5v`; evidence
  `fg05-security-correction2/`. Full uncached suite and required checks plus
  eight targeted packages and focused regressions reported passed. Previous
  worker-session continuation failed to launch; replacement writer started only
  after that failure, not concurrently.

Security transition sequence now records before mutation, durably renders both
aliases out, strictly fences both poolers, promotes, settles admin policy using
actual names, renames/reapplies policy, completes configuration/restart and only
then marks settled/renders/resumes. Unconditional deferred resume is removed.
Protected arbitrary SQL is refused while unsettled, including read intent and
unknown transition state. Additional regressions cover unread records, ordinary
protected reads, actual-name ambiguity, settled retries and both-pooler fencing.

Worker-flagged boundary requiring independent assessment: SQL fails closed on
unreadable records, but renderer cannot enumerate unknown unsettled aliases if
the records read itself fails. Root-owned `ApplyPoolerFence` file writes have no
real host-level test; rendered alias exclusion is unit-covered. Real PgBouncer
pause/kill/reload/restart, PostgreSQL privilege settlement, process death and
destructive drop are still live-pending. Drop retirement has helper-level tests,
not a live destructive-drop run. No source checks are live qualifications.

No commits, branches/tags, publication, live operations or fixture changes.
No third corrective attempt is authorized automatically if independent review
still finds the same unit incomplete.

## Final independent second-attempt evidence and stop disposition

Reviewed code HEAD/base remain unchanged. Tracked second-attempt delta:
`1ede4176becec4b9f83cb30cc10782eb764476dd1b27cfc4bc3ecb12aefde429`.
Untracked source `internal/node/sql_unsettled_test.go`:
`209ea32299e34e565d539f6215b604c7497f201e38f2d1a3e94f2a30002d3912`.
Frozen input docs: REVIEW `c7dc162b…be0ac`, PLAN `838ca257…0f82d84`,
correction HANDOFF `9b47a854…0bcd5`. These identify reviewed input, not this
post-review evidence update. Both reviewers independently verified hashes and
reviewed cumulative base-to-working-tree guarantees, without new tests or edits.

### Lane B second-attempt report — BLOCK

- **B2-01 Major**, `internal/steps/pgbouncer.go:75–95`, especially 81–83:
  `fencedPoolerAliases` translates failed record reads to no exclusions. After
  promotion but before policy settlement, corrupt/unreadable records plus normal
  pooler apply rewrites safe alias-free files to expose inherited writable admin
  access. Node SQL fail-closed behavior does not protect direct session-pooler
  SQL/psql/console/tunnel access. Propagate errors consistently through input,
  check, render, apply and direct fence application; preserve safe bytes/no
  exposing reload. Regression: `TestPoolerRenderingFailsClosedOnUnreadUnfollowState`
  with existing safe files and malformed/unread records, production step paths,
  both aliases and no reload/start; readable no-transition behavior retained.
- **B2-02 Minor**, `internal/node/follower.go:758–765`: fresh floor admission
  fails before desired entry save, but hints `config set clusters.<follower>...`,
  which rejects the absent array member. Distinguish existing retry from fresh
  proposal. Exercise actual setter against persisted pre-admission configuration
  and prove supported prerequisite permits subsequent admission.

B accepts previous empty-stanza recovery, fresh/retry-aware cleanup, random
generation/ABA distinction, locked floor checks, running unfinished retry and
implementation-reference corrections. Original eight of nine findings are
accepted on source/assertion review; FG-M1 remains partial through B2-01.
Both lifecycle additions accepted for safety mechanics, with B2-02 diagnostic
gap. RPC registration set unchanged; pooler apply/direct access remains the
critical security surface. Full method/iteration inventory from prior reports
still applies, with this remaining failure explicitly overriding acceptance.

### Lane A second-attempt report — BLOCK

A independently confirms **C2-A1 Major** (same as B2-01) and **C2-A4 Minor**
(same as B2-02), and additionally identifies:

- **C2-A2 Major**, `internal/node/follower.go:658–680,782–856,1496–1507`:
  running unfinished ordinary follow retains copied owner/generation; unfollow
  promotes then fails before independence clears generation. Public follow and
  locked execution do not check `Unfollowing[name]`, so old progress can adopt/
  start a primary as a purported follower or recopy across the newer transition.
  Make operation kinds mutually exclusive before effects, without refresh/rejoin
  behavior. Regression `TestOrdinaryFollowCannotResumeAfterUnfollowBegins`:
  actual unfollow promotion interrupted before rename/independence; public
  preview/execution refuses retained and unsafe slot continuations before any
  slot/adopt/recopy/start effects; public unfollow retry remains admitted.
- **C2-A3 Major**, `internal/node/awaiting.go:29–34`,
  `internal/steps/postgres_cluster.go:142–182`,
  `internal/steps/scripts/postgres-cluster.sh:25–42`: legitimate prepared
  generation is saved before materialisation. General apply excludes awaiting
  recovery but not unfinished ordinary follow; initdb can create an empty primary,
  exposing the alias and making follow retry refuse unowned paths. Exclude
  unfinished followers from general creation/start/exposure while allowing the
  explicit owned lifecycle view after materialisation. Regression
  `TestApplyDoesNotInitialisePreparedFollower` uses actual plan/apply with
  command-recording effects, proves no initdb/start/pooler exposure, unrelated
  DBs remain applicable and public retry remains admitted.

A accepts the six prior majors except security's remaining unread-state path;
original five minors and other three majors accepted for their original scope.
New cross-operation/general-apply gaps prevent certifying the complete ordinary
retry integration. Historical fixes and Q1a/Q3 remain accepted; Q2 enforcement
is still incomplete. No new behavioral question is required to identify these
as failures to fulfill already approved outcomes, but the explicit second-attempt
stop rule requires user disposition before another implementation attempt.

### Independent FG-04 second-attempt local PASS

UTC 2026-11-10 14:37:38–14:39:42; test-process umask 0022. Stable tracked and
all untracked hashes, 35 modified plus four untracked entries, no writer activity.
Go 1.27.1, golangci-lint 2.14.0, just 1.43.0, shellcheck 0.11.0,
actionlint 1.7.12; GCC 15.2.0/CGO on.

| Required command | Exit / UTC |
| --- | --- |
| `go build ./...` | 0; 14:37:47–49 |
| `go vet ./...` | 0; 14:37:49 |
| `go test ./...` | 0; 14:37:49–58 |
| `golangci-lint run ./...` | 0; 14:37:58–59 |
| Darwin/arm64 build | 0; 14:37:59–14:38:01 |
| `just lint` | 0; 14:38:01–02 |
| `git diff --check` | 0 |

Eight targeted packages uncached all pass, 14:38:15–28. Supplemental full
`go test -count=1 ./...` passes 14:38:32–57 with zero skips/cache hits.
Untracked SQL tests explicitly listed and run focused, all pass. No unexpected
tree changes; only ignored binary rebuilt by just lint. Evidence
`fg04-correction2/fg04c2-evidence-sha256.txt`, summary `373af79f…44c2b`.
Historical optional activity race remains untested. This is local verification,
not live behavior, reviewer acceptance or authorization to publish.

### Work item boundary

FG-01/02 complete. FG-03A/B complete with BLOCK dispositions. FG-04 passes on
the named second-attempt code/test tree. FG-05 stopped incomplete after two
corrective attempts; no third worker started. AC-01, PUB-01, LV-02/03 and all
live targets remain unauthorized/unexecuted. LV-01 has read-only preparation
material but no completed executable run sheet: target/resource measurements
and bounded fault proof remain pending. NX-01/02 not reached. No fixtures or
live resources were created or removed. Next decision: authorize a bounded
additional correction pass or choose another disposition; security/lifecycle
majors cannot be silently accepted as publication-ready.

## Explicitly authorized additional pass — awaiting independent acceptance

The stop was reported and the user explicitly authorized one further pass on
the four remaining findings. Worker `ses_e3a0f4bc9fferJ7Uc8bxUVhWSZ` implemented:

- **C2-A1/B2-01:** one readable records snapshot feeds both poolers and restore
  aliases; input/check/apply/direct fence refuse read/decode errors before
  overwriting safe files or service effects. Missing initial records remain
  legitimate bootstrap. `TestPgbouncerRenderingFailsClosedOnUnreadUnfollowState`
  covers existing safe files and no exposing writes/reload paths.
- **C2-A2:** durable unfollow progress wins over ordinary follow public preview,
  admission, locked execution and stale rollback.
  `TestOrdinaryFollowCannotResumeAfterUnfollowBegins` covers retained and unsafe
  slots while preserving unfollow continuation.
- **C2-A3:** engine view excludes unfinished generated followers from general
  creation/start/timers/routing and explicit plan/apply refuses them. Explicit
  owned lifecycle execution keeps its materialised target. Regression
  `TestApplyDoesNotInitialisePreparedFollower` also checks unrelated convergence
  and retry admission. Awaiting helper naming/comment adaptations included.
- **C2-A4/B2-02:** fresh follow suggests settings on the existing source;
  existing retry can target the follower. Regression
  `TestFreshMembershipFloorMismatchSuggestsSourceSetting` executes the real setter
  on actual pre-admission desired state and proves subsequent admission.

Worker reports full chain passed 15:12:53–15:13:16Z; targeted eight packages and
full uncached suite passed 15:13:21–15:13:46Z; focused regressions passed 15:13:50Z.
An initial test used inconsistent host facts and was corrected through the
existing test seam, not a production safety bypass. Independent verification
and both review lanes must reproduce/assess the final tree; no closure inferred.

**Runtime evidence availability limitation:** the environment/session restart
removed the original shared `/tmp/opencode/followers-final-gate-20261110` contents.
The worker recreated it at 15:07:43Z. Only `fg05-additional-pass/` was present
when the orchestrator checked after handoff. Earlier results remain reported
in durable documentation and agent messages, but their runtime output files
cannot now be inspected. Do not claim those old paths still contain evidence.
The authoritative surviving worker logs are `fg05a-final2-*`, summary
`fg05a-verification-summary.txt`, index `fg05a-evidence-sha256.txt`. Final fresh
independent verification will regenerate inspectable outputs for this checkpoint.
No intentional cache/resource cleanup was performed by the orchestrator.

Untracked SQL regression remains preserved with its original content hash.
No commits, Git branch changes, publication, live operations, fixtures or server
resource changes. Real PostgreSQL/PgBouncer/systemd/root-file/crash/slot behavior
remains live-pending regardless of local pass/review result.

## Additional-pass independent disposition

Frozen tracked delta `cf9b5a10eb52cb8c316d225fcf9a8530e35ddf6663736c5a14486a13ad8c381b`;
HEAD/base unchanged. Supplied untracked source and docs hashes verified by both
reviewers and tester. Report updates after review are not part of that identity.

### Lane B — BLOCK, B3-01 medium

`internal/node/awaiting.go:28–37,45–52` filters all generated unfinished
followers from `stepsView`, but diagnostics also use that helper:
`internal/node/status.go:259`, `internal/node/doctor.go:132–143`,
`internal/node/followerlag.go:170`. After permanent start but failed completion
save, a stopped/paused/lost-slot follower disappears from follower gathering.
Status loses replication facts, metrics omit measurements, and doctor warns
unread instead of reporting measured failure/source slot pressure. This violates
Q3 acquisition, not parser semantics or an optional diagnostic improvement.

Correction direction: separate convergence and read-only diagnostic views.
General apply still excludes unfinished creation/start/routing/timers; configured
non-recovery-awaiting followers remain independently observable. Exact regression
proposal `TestUnfinishedMaterialisedFollowerRetainsDiagnostics` should exercise
production status/doctor/metrics with deterministic query seams for stopped
readable-source pressure, running paused replay and lost slot, preserving measured
failure severity and unknown-self omissions while asserting apply exclusion.

B accepts pooler unread-state failure propagation, cross-operation refusal and
fresh-source setter remediation. General engine exclusion is correct for its
purpose but partial overall through B3-01. Original nine and historical R1–R3
accepted on source review; R4 acquisition acceptance qualified by B3-01.
No RPC registration changes; relevant unintended change is status/doctor/metrics
filtering. Resource accounting still includes excluded entries; no false-orphan
slot defect established. Q1a/Q2 remain accepted; Q3 acquisition needs correction.

### Lane A — PASS WITH ONE MINOR, AF-A1

`internal/node/follower.go:1747–1752`, `internal/steps/pgbouncer.go:52–70,152–162`:
direct unfollow fence passes full saved config to `ApplyPoolerFence`. Renderer
excludes unsettled unfollow but can reintroduce another generated unfinished
follower or recovery-awaiting database that general apply deliberately withholds,
including auth aliases on both poolers. No target protection bypass/data loss
established; this is unrelated routing eligibility integration.

Correction direction: direct fence applies appropriate lifecycle eligibility
against its same records snapshot, preserving explicit owned-target keep for
real follow/recovery convergence. Exact regression
`TestUnfollowFenceKeepsOtherUnfinishedAliasesExcluded`: actual direct render from
full config containing unsettled unfollow target, different generated follower,
awaiting database and ordinary source; both main/auth aliases withheld for the
first three while source stays routable. Retain owned execution keep tests.

A accepts all four prior corrections, all original nine, historical closures and
Q1a/Q2/Q3 for inspected scope. B3-01 was independently found only by B; A's broad
acceptance does not override that actionable source path. No deduplication of
these two different view-consumer defects.

### Fresh independent local evidence — PASS

UTC 2026-11-10 15:17:01–15:18:59Z; normal test umask 0022. HEAD/diff and all
untracked identities stable, 39 modified plus four untracked entries. Go 1.27.1,
golangci-lint 2.14.0, just 1.43.0, shellcheck 0.11.0, actionlint 1.7.12,
GCC 15.2.0/CGO on.

Required sequential commands all exit 0: build 15:17:23–25; vet 15:17:25–26;
tests 15:17:26–35; golangci-lint 15:17:35–36; Darwin/arm64 build 15:17:36–38;
just lint 15:17:38–39. Diff check 0. Eight uncached targeted packages all pass.
Full `go test -count=1 ./...` passes 15:18:08–33 with zero cache hits/skips.
All four focused regressions pass. Evidence directory
`fg04-additional-final/`, manifest `fg04af-evidence-sha256.txt`, summary
`44d2dbd8…8f9ab7`. Only ignored binary rebuilt; no unexpected tree change.
Earlier runtime evidence loss remains disclosed; this is a new self-contained
inspectable evidence set. Historical activity race not run. No live qualification.

### Stop boundary

The explicitly authorized additional implementation pass is complete. Both
reports are integrated; no further worker started. Next decision is whether
to authorize targeted diagnostic/routing view corrections for B3-01 and AF-A1.
No user-facing review gate or broader policy change is requested. Publication,
commits and all live actions remain separately gated.

## Diagnostic/routing integration pass — pending independent acceptance

User authorized completing both remaining findings and their verification/
re-review cycle without repeating the same scoped permission request. Worker
`ses_e39ede628ffeSw3Huu9F27b4N5` implemented:

- B3-01: separate diagnostic view excludes only recorded recovery-awaiting
  entries; status and doctor reuse their loaded record snapshot, follower metrics
  use the diagnostic view. Generated unfinished ordinary followers retain
  independent source/self facts. Engine convergence stays excluded.
- AF-A1: shared pure `config.Cluster.RoutingView` applies lifecycle eligibility
  to direct fence against the same records snapshot used for pooler exclusion.
  The normal owned engine keep target is preserved; no new lock/state write.

Regressions `TestUnfinishedMaterialisedFollowerRetainsDiagnostics` and
`TestUnfollowFenceKeepsOtherUnfinishedAliasesExcluded` exercise production
status/doctor/metrics gathering and actual direct fence rendering. Stopped
source-slot pressure, paused/lost measured failures, unknown-self omissions,
general apply exclusion, unrelated ordinary routes and owned keep behavior
are covered. Prior fail-closed read-state tests remain.

Worker full chain passed 15:53:21–15:53:39Z; full uncached suite passed
15:53:43–15:54:14Z; eight targeted packages and diff check passed. Early test
fixture duration/dry-run expectations were corrected in tests, not production
safety logic. Owner-only raw logs `fg05-views-final/full-chain-final3.log`
(`69c75845…a99e287`) and `uncached-final3.log` (`55072c01…629418`). No narrative
plans/reports saved outside the repository. Independent checks/re-review follow.

No live PostgreSQL/PgBouncer/systemd/root-file/RPC/crash qualification. No
commits, publication, server/context/secret access or resource cleanup. Existing
untracked SQL tests preserved. All prior evidence-loss disclosures still apply.

## Final independent acceptance — diagnostic/routing checkpoint

### Lane A — PASS, no actionable findings

Session `ses_e3af0a85bffes6Jz4QIrfnHORx` verified the final frozen tracked and
untracked hashes and reassessed cumulative base-to-working-tree guarantees.
B3-01 and AF-A1 accepted; no prior accepted finding reopened. Diagnostic view
retains generated unfinished source/self observations but never enters engine
execution. Direct fence uses shared routing eligibility and its same records
snapshot; explicit owned keep remains in normal rendering. Resource accounting
counts excluded entries once. Q1a/Q2/Q3, operation generations, locks, ownership,
retained-slot rules, source floors and privacy remain accepted. Source/assertion
review only; no tests or edits performed and B's new conclusions not consulted.
A inspected surviving worker logs, not a yet-available independent tester result.

### Lane B — ACCEPT, no actionable findings

Session `ses_e3af07a5dffehTgy6IXIMpUZWk` independently verified the same hashes
and production/regression paths, without reading A's new conclusions. B3-01
accepted: status/doctor existing snapshot reuse and metrics diagnostic view
restore measurable stopped/paused/lost states without allowing convergence.
AF-A1 accepted: direct fence excludes unrelated generated/awaiting main/auth
aliases from both poolers using one snapshot, preserves ordinary routes and
owned engine keep. Original nine and historical R1–R4 closures accepted;
earlier R4 acquisition qualification removed. Prior complete RPC/CLI inventory
stands, no registrations changed. No tests, source writes or live operations.

Both reviewers distinguish source acceptance from test execution and live pass.
Constructed lifecycle/query seams and captured renders do not qualify actual
PostgreSQL privileges, live PgBouncer/session behavior, root writes or crashes.

### Independent FG-04 final PASS

Tester session `ses_e3af04bb9ffecqFS4S077S2dFJ` executed fresh checks with no
writer active. UTC 2026-11-10 15:57:07–15:58:47Z, normal umask 0022. Frozen
diff `c9af1798…2afb8f` and all untracked hashes identical pre/post, 43 modified
and four untracked entries. Go 1.27.1, golangci-lint 2.14.0, just 1.43.0,
shellcheck 0.11.0, actionlint 1.7.12; GCC 15.2.0, CGO on.

| Required sequential command | Exit / UTC |
| --- | --- |
| `go build ./...` | 0; 15:57:15–17 |
| `go vet ./...` | 0; 15:57:17 |
| `go test ./...` | 0; 15:57:17–28 |
| `golangci-lint run ./...` | 0; 15:57:28 |
| `GOOS=darwin GOARCH=arm64 go build ./...` | 0; 15:57:28–30 |
| `just lint` | 0; 15:57:30–32 |
| Separate `git diff --check` | 0 |

Eight uncached targeted packages all pass. Full `go test -count=1 ./...` passes
15:58:04–28 with zero cache hits/skips, removing ordinary-chain cache reliance.
Focused diagnostic/direct-fence, owned keep, general apply and unread-state
fence regressions all pass. No first failure or downstream not-run. Only ignored
binary rebuilt by just lint; no unexpected source/docs changes. Evidence raw
logs/status/identities and manifest under `fg04-views-final/`; manifest SHA-256
`79406fbc…f574528`. No narrative report saved in temporary storage.

### Final boundary

No actionable local review finding remains. FG-01/02/03A/03B/04/05 local gate
complete, changes uncommitted. Final evidence-only docs updates occurred after
the frozen source/test checks; rerun full pre-commit checks if a commit is later
authorized. AC-01 and follower live qualification, completed runtime-resolved
LV-01, publication, extensions and beta remain pending their respective gates.
No installed/live binary identity was inspected and no fixture exists from this
work. Historical activity race gap and lost earlier runtime logs remain disclosed.
