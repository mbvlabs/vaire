# Phase 6 review findings

## Review record

U3 reviewed `master...243157c` with four read-only `reviewer` agents using `openai/gpt-6.1-sol`. All lanes completed. Earlier Claude attempts failed on quota before producing findings. No reviewer ran live tests or independently repeated local verification. The orchestrator ran the complete AGENTS verification chain at `243157c`; all passed. Passing local checks do not cover the lifecycle failure boundaries below.

Verdict: **blocked before PR**. Ten majors and eight minors, no duplicates merged. All findings are open; none has a fix commit or regression test yet. The orchestrator spot-checked the early awaiting-state removal, unfollow state transition/resumption, shared-state update, follower-only engine options, and numeric floor parser against current source. Remaining findings are source-review evidence pending confirmation during fixes and re-review.

## Findings register

All locations refer to `243157c`.

| ID | Severity | Lane | Finding / location | Required regression | Status |
| --- | --- | --- | --- | --- | --- |
| R1-1 | Major | R1 | Follower-only engine apply has `Only` but no `DB`, so pending source restarts can run prematurely and twice during resize. `node/follower.go:697` | Seed pending source/follower restarts; follower helper cannot consume source restart; source restarts once after config write | Open |
| R1-2 | Major | R1 | `strconv.Atoi` silently discards valid PG numeric floor settings such as hexadecimal; resulting standby floors can be too low. `steps/follower.go:40–73` | Source/follower hexadecimal, octal and fractional PG numeric inputs normalize or fail validation explicitly; never silently ignored | Open |
| R1-3 | Minor | R1 | Resize dry-run plans only source despite changing follower config/restarts. `node/db.go:827–833` | Preview includes affected follower config steps, excludes unrelated databases | Open |
| R2-1 | Major | R2 | Recovery clears durable awaiting state before materialisation; process death makes incomplete recovery look complete and blocks retry. `node/follower.go:351–365` | Fail before/during copy/adoption; reload records, awaiting persists and retry admitted | Open |
| R2-2 | Major | R2 | Clearing `follows` makes unfinished unfollow impossible to resume; stale config, slot, routing and backups may remain. `node/follower.go:550–581` | Fail after state save/before restart; public retry finishes with same stanza and correct config/slot/routing | Open |
| R2-3 | Major | R2 | Rename resumption treats missing source DB as proof all role renames finished; partial role rename cannot complete. `node/follower.go:623–633`, `node/restore.go:1440–1472` | Retry database renamed with only subset of roles renamed; all names/settings/passwords complete idempotently | Open |
| R2-4 | Major | R2 | Unfollow saves desired state under shared node/cluster locks; concurrent jobs can lose transitions or promote same target concurrently. `node/jobs.go:1217–1218`, `node/follower.go:656–685` | Controlled concurrent transitions preserve both changes; same target exclusive; reload under appropriate lock | Open |
| R2-5 | Major | R2 | Attached unfollow failure after password change returns error before credential handoff; passwords were only in CLI memory. `cli/follower.go:171–202` | Post-password restart/apply failure retains explicit secret-bearing operator handoff, never ordinary logs/history/activity | Open |
| R3-1 | Major | R3 | `maint kill` connects using follower alias rather than actual source database name. `node/maint.go:420,449` | Both selection/termination use DatabaseName(), external confirmation token still follower/kill | Open |
| R3-2 | Minor | R3 | Follower guard omits restore.promote/restore.undo, producing wrong refusal/protection contract. `node/follower.go:94–104` | Both methods return is_follower/source details including protected follower | Open |
| R3-3 | Minor | R3 | Combined restore --promote converts divergence warning to string, then labels it restore_dump_errors. `node/restore.go:357–358`, `cli/restore.go:152–153` | Structured followers_diverged preserved; genuine dump warnings unaffected | Open |
| R3-4 | Minor | R3 | No-bucket rebuild guidance drops sources before followers and recreates followers as independent databases. `cli/serverinit_restore.go:740–743` | Drop followers before sources, recreate sources before db follow; no db create for followers | Open |
| R4-1 | Major | R4 | One `Known` bit merges partial source/self evidence; query failures produce false zero lag, missing slots or out-of-recovery diagnoses. `health/follower.go:76–84`, `node/followerlag.go:38–48,120–124` | Source-only/self-only/neither/both readings through parser/doctor/JSON/human/metrics; unknown never measured zero | Open |
| R4-2 | Major | R4 | All follower checks skip stopped followers, hiding source slot WAL pressure/lost/missing slots precisely when needed. `doctor/follower.go:42–43` | Stopped follower with readable source: healthy/half-used/lost/missing → pass/warn/fail/fail; unreadable source unknown | Open |
| R4-3 | Minor | R4 | db list hides paused replay behind streaming/lag suffix. `cli/db.go:407–414` | Streaming but replay paused shown explicitly; out-of-recovery and unknown cases too | Open |
| R4-4 | Minor | R4 | NULL replay_lag becomes zero for streaming even with positive/unknown byte backlog. `health/follower.go:105–110` | Positive byte lag + NULL timing never known zero; caught-up idle case remains zero | Open |
| R4-5 | Minor | R4 | Slot enumeration error becomes no orphan slots. `node/followerlag.go:83–86`, `doctor/follower.go:112–114` | Query failure warns unknown rather than claiming absence | Open |
| R4-6 | Minor | R4 | Follower metric write errors discarded; refresh succeeds while stale healthy samples remain. `node/followerlag.go:131–132`, `node/metrics.go:168–172` | Follower-only write failure propagates from refresh; preserve atomic replacement | Open |

Paths in the table are relative to `internal/`.

## Design decisions approved by the operator

All three recommendations below are approved. Q1 concerns configuration/restart ordering only: **never wait for data catch-up before resizing**. Followers may catch up over hours or days. Q2 keeps fork-style noninheritance of protection. Q3 preserves unknown readings rather than fabricating zero lag. Include prolonged catch-up and archive/slot-cap limitations in manual qualification.

### Q1 — Standby floor ordering and failure handling (D6)

PostgreSQL requires standby-first increases but primary-first decreases. Current D6's unconditional follower-first rule is unsafe for decreases; mixed changes need staging. Also, an attempted follower update failure currently becomes a warning and source resize proceeds.

**Recommendation:** stage conservatively: first configure followers to at least the maximum of the old and target source floors; after required increases have actually taken effect on followers, apply the source; lower follower floors only after the source's lower settings are effective. Handle mixed changes using that same sequence. Stop on failed attempted follower updates. Preserve the existing explicit no-token deferral contract only as a visible pending-restart/possible replay-pause result, not as a successful safety check. Never force a restart or consume a source confirmation in the follower stage.

### Q2 — Protection of copies (D8/D10)

A follower made from a protected source is unprotected because forkSpec doesn't copy Protected. Unfollow leaves it unprotected and writable. This matches the current fork-style reuse, not an implementation defect.

**Recommendation:** keep current semantics: follow/unfollow creates a rehearsal copy, not a change to protected production. Explicitly document that protection is not inherited; operators can protect the copy independently. Inherited protection is an alternative if the operator wants it.

### Q3 — Meaning of lag and unavailable measurements (D13)

Slot-retained bytes are not replay backlog. The current disconnected case substitutes retention for byte lag. PostgreSQL replay_lag measures recent transaction visibility delay, not estimated catch-up duration; it can remain nonzero briefly after catching up.

**Recommendation:** report replay byte lag only when actually measurable, otherwise unknown; keep slot retention separate. Define seconds as PostgreSQL replay/visibility delay, not catch-up ETA or age of idle source. Caught-up idle NULL is zero only with known zero byte backlog; positive/unknown backlog plus no timing sample is unknown. Do not silently substitute replay-timestamp age. Use independent source/self availability, JSON null/omission for unknown values, and omitted unavailable metric samples. Document semantics and test them consistently.

## U4 lifecycle re-review record

The first lifecycle worker timed out after 30 minutes; one retry completed the batch. The orchestrator repeated the full verification successfully. No lifecycle changes are committed yet. Re-review by `openai/gpt-6.1-sol` (artifact `c95811d4-d016-4e72-85cb-01600af36446_reviewer_output.md`) confirms R2-2, R2-3 and R2-4 addressed in the working tree, pending final acceptance/commit.

Two majors remain:

- **R2-1 partial:** physical temporary registration/adoption precedes durable `Adopted=true`; process death during adoption or before marker save leaves a cluster that public recovery refuses. Persist operation ownership before materialisation; resume operation-owned partial registration/adoption safely. Simply setting Adopted early is not sufficient because it skips incomplete adoption. Tests must interrupt inside actual adoption file operations and before marker persistence, not replace all build work with one callback.
- **R2-5 partial:** attached inactive-unit synthesis builds a structured `job_outcome_unknown` error, then passes `errors.New(rec.Error.Message)` to `finishJob`, which overwrites the structured error and loses the marker. Preserve structured details and test synthesis → finishing → jobOutcome → RPC serialization → explicit CLI credential handoff. R2-5's direct CLI-marker test was insufficient.

### Second lifecycle re-review

Reviewer artifact `a76bf826-0706-45e2-8793-8bcc41e444a4_reviewer_output.md` confirms R2-2 through R2-5 fixed in the working tree, pending acceptance/commit. R2-1 is still partial. The orchestrator independently repeated the complete verification successfully before this review. Three majors need another correction:

- **R2-1 slot preservation:** copied-phase retry calls `createFollowerSlot`, which drops/recreates an inactive slot before reusing old data. This discards WAL retention the old copy needs, especially without archives. Reuse a valid existing slot without resetting it; missing/lost slot must choose an explicit owned recopy path. Test retained old slot position and absence of drop/reset on completed-copy resume.
- **R2-1 partial configuration directories:** artifact discovery checks `postgresql.conf`, but the constructor refuses any existing config directory. Interrupted registration/cleanup can leave an owned directory with no postgresql.conf or data; retry skips cleanup and can clear ownership on constructor failure, stranding it. Track directory presence separately from valid registration, clean only proven-owned directories, retain ownership while any owned artefacts remain. Test owned and foreign directory-only cases through public admission and build.
- **R2-6 new regression, Major, open:** fresh-copy constructor failure after slot creation returns without slot cleanup. Normal follow then removes its desired-state entry, orphaning a WAL-retaining slot. Restore bounded cleanup for normal follow and fresh recovery constructor failures; do not drop retained slot for completed-copy resume. Test exhausted-port/constructor failure and cleanup error propagation with no foreign local deletion.

The operator authorised one more narrow attempt.

### Third and fourth lifecycle re-reviews

Reviewer artifact `ab7f261c-fe0f-4a1e-90cc-5ae99b8c9843_reviewer_output.md` confirms the partial configuration directory fix, R2-6 and R2-2 through R2-5. It found two more issues, both since fixed:

- **P1:** `wal_status=unreserved` was treated as reusable. Only `reserved` or `extended` with a restart position is reusable now; every other status takes the owned recopy path.
- **P2:** the recover dry run promised `resume=true` without reading the slot. The preview now reads the slot without changing it, reports `recopy_reason`, and sets `resume_conditional` when the slot cannot be read.

The final focused review could not run: the `gpt-6.1-sol` quota was exhausted. The orchestrator reviewed this last delta, the full verification passed, and the operator approved committing on that basis. The U6 whole-branch review covers it again.

**Status:** R2-1 through R2-6 fixed in the lifecycle commit. No live evidence yet; the manual test must kill recovery inside the copy, inside adoption and before the adopted marker, and exercise an `unreserved` slot.

## U4 batching after decisions

One writer at a time in the phase worktree; no push/deploy. Each batch gets full verification, orchestrator check, commit and original-lane re-review with `openai/gpt-6.1-sol`.

1. **Lifecycle and credential delivery:** R2-1 through R2-5. Durable operation progress is part of correct execution/resumption, not a repair command or server healing mechanism. Keep secrets out of durable job history. Worker must propose the smallest coherent persistence/locking approach before expanding it.
2. **Configuration and resize:** R1-1 through R1-3 and approved Q1 semantics.
3. **Guards and integration:** R3-1 through R3-4 and approved Q2 documentation/tests.
4. **Visibility:** R4-1 through R4-6 and approved Q3 semantics.

Source reports remain available in session artifacts: R1 `38e2efb4-3abd-4649-8c0c-8123b20c4a93`, R2 `b576965c-3c40-4337-8db4-1a09c617d222`, R3 `acdec093-27c3-4869-b6c7-aa48b7eb14db`, R4 `817cb363-f379-4df6-8b75-f5ca833fb43c` (each `_reviewer_output.md`).

## U4 configuration re-review record

The lifecycle batch is committed as `0511ecf`.

Reviewer artifact `258a321c-12f0-4b12-86db-973985313370_reviewer_output.md` reviewed the first configuration fix. Verdict: BLOCK.

- **R1-1:** fixed. Each follower runs with `Only` and `DB`, so a follower update never restarts its source.
- **R1-2:** partial. The parser rejects valid PG18 fractions such as `0.6` and `010.5`. It must match `parse_int`: integer first, then the whole value as a float when parsing stops at `.`, `e` or `E`, or overflows.
- **R1-3:** partial. A follower restart that is already pending is missing from the human preview.
- **Q1, retry:** partial. A retried resize after a failed follower update sees no size change and goes source first.
- **Q1, decrease:** partial. Doing the source first does not make lowering a follower safe. Until the follower replays the source's parameter change, its control file still requires the old, higher values, and it fails recovery at startup with lower ones.
- `applyOtherConfigs` clears its confirmations, so it does not have the R1-1 defect.

### Decision Q1a (operator, supersedes the decrease half of Q1)

`db resize` refuses any change that would lower a follower's hot-standby floors, with reason `has_followers`. To shrink such a source, unfollow or drop the follower, shrink, then run `db follow` again.

Because a resize can no longer lower a follower's floors, it always applies the followers first, which also fixes the retry order. A deferred follower restart lets the source proceed with `restart_pending`, and that warning says replay may pause until the follower restarts.

Known limitation, documented in U5: a hand edit to the floor settings in `cluster.toml` followed by plain `vaire apply` is not ordered. Run `vaire apply --db <follower>` first.

### Second configuration re-review

The second worker's runner crashed after it finished, because the per-user `/tmp` quota was full. The orchestrator verified its edits. The first re-review run died on the same quota; stale `go-build*` directories were removed and the review was rerun.

Reviewer artifact `8238b7f1-b362-4e2a-a6ed-c971ae43b901_reviewer_output.md`: verdict PASS with notes. R1-1 and Q1/Q1a passed. R1-2 and R1-3 each had one minor gap, which the orchestrator fixed:

- **R1-2:** Go's `ParseFloat` returns zero without an error on underflow, where `strtod` reports `ERANGE` and PostgreSQL rejects the value. A nonzero literal that comes out zero or subnormal is now rejected; `0e-400` is still zero.
- **R1-3:** a dry run of a retried resize, after a failed follower write, has the same old and new entry and no pending restart, so it said no restart. The preview now also counts a restart when the planned `60-vaire-follower.conf` diff changes a floor's value; a changed reason alone does not count.

The reviewer noted, outside this batch: a follower's own resize and a new follow are not guarded by Q1a. Both are pre-existing; floors take the larger of the follower's and the source's values, so a follower's own resize cannot go below its source.

**Status:** R1-1, R1-2, R1-3, Q1 and Q1a fixed in the configuration commit. No live evidence yet.

## U4 guards re-review record

Reviewer artifact `dbc72399-c077-4f0d-85e3-d2597fa436a4_reviewer_output.md`: R3-1, R3-3 and R3-4 fixed. R3-2 was partial: the node refused `restore.promote` and `restore.undo` on a follower, but the CLI checked protection and asked for the token first, so a protected follower answered `database_protected`. The orchestrator fixed it: `restoreAction` refuses a follower first with the node's own `is_follower` error (`node.RefuseFollowerMethod`), tested for both methods on protected and unprotected followers.

- R3-1: other maintenance paths already use `DatabaseName()`, refuse followers or skip them.
- R3-3: mixed-version RPCs are rejected by the version and channel checks, so the new `follower_warnings` key needs no compatibility handling.
- Q2: behaviour is tested. Operator documentation is still open for U5: follow and unfollow help and the guide must say that a follower and an unfollowed copy do not inherit protection.

**Status:** R3-1 through R3-4 fixed in the guards commit. Q2 docs open (U5).
