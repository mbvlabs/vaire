# Phase 6 work units: followers

## Current execution status — 2026-11-10

**Final local gate:** both independent cumulative source reviews accept with
no remaining actionable findings. Independent required chain, eight uncached
targeted packages and full uncached suite pass. U6's local review/verification
portion is complete, fixes uncommitted. Commit/publication portion remains
separately gated; U7/live qualification has not started. Historical status
updates below remain chronology, superseded by this disposition.

Diagnostic/routing completion update: both B3-01 and AF-A1 were explicitly
authorized and implemented. Worker full/uncached checks pass; both independent
verdicts and final-tree verification pending. Diagnostic acquisition excludes
only recovery-awaiting entries, while engine/direct-fence routing eligibility
also excludes unfinished generations except explicit owned execution keep.

Latest U6 result: independent local checks pass; targeted four corrections and
original nine findings source-accepted. Lane A has one routing minor; lane B
blocks on unfinished-follower diagnostic acquisition regression. Both are open
and no further source writer is active pending user disposition. U7/live and
publication remain unstarted/unauthorized.

Latest: after the required stop the user authorized one additional targeted
pass on the four remaining findings. It is implemented and worker checks pass;
independent final-tree verification and both review verdicts are pending.

Latest U6 disposition: **STOPPED after second corrective attempt**. Independent
local full/uncached checks pass; both re-review lanes BLOCK with three majors
and one minor after deduplication. Remaining areas: unread-state pooler security,
follow/unfollow operation exclusion, general apply of unfinished followers and
fresh-follow remediation. No third source correction without user disposition.
No U7/live or publication authority has been granted.

The port and fix batches are committed through `2155a07` on
`phase-6-followers`, based on master `8d68056`. The current execution authority
is main checkout's `docs/HANDOFF-followers-final-gate.md`, following the approved
state-of-the-union direction. See [the final review](REVIEW-phase-6-final.md)
for the frozen checkpoint and fresh evidence, and
[the current findings ledger](FINDINGS-phase-6-review.md) for closure status.

- U0–U5: historically implemented/committed; final independent acceptance pending.
- U6: FG-01/02 complete; both independent whole-diff reviews returned BLOCK.
  Four majors and five minors were found after deduplication. Authorized fix
  batches and two explicitly approved lifecycle corrections are implemented
  but await independent re-review. The final worker's full local chain and
  uncached targeted tests passed; independent final-tree verification is pending.
  First fixed-tree independent checks passed but re-review found six major
  boundary gaps and two minor follow-ups. Second lifecycle/security corrections
  are implemented, pending independent acceptance; no third attempt without
  stopping and asking if the same unit remains incomplete.
- U7: not started; activity qualification must precede follower deployment.
  Commits, publication and all live operations remain separately gated.

The initial-build introduction, setup instructions, old role/model names and
unit-level commit permissions below are historical, not instructions to repeat
setup or authority to mutate Git today. Preserve both legacy directories.
Q1a/Q2/Q3 in the implementation record supersede conflicting original design
prose. Manual checklist fault mechanisms still require a bounded run sheet;
timing-only adoption kills, postgres chmod and disabled `ALTER SYSTEM` are not
valid proof of the intended faults.

## Historical initial-build introduction and execution plan

Phase 6 is designed, and a first build exists. That build was made on 2026-10-16 in the worktree `.claude/worktrees/phase-6-followers`, on top of `fb13ec3`. It was never committed, reviewed or run on a server. Since then master has gained the rename to vaire, the v1 fixes (the TOTP write guard, hiding admin sessions from `queries` and `top`, `sql` with several statements, the secrets gate), pre-release versions, and local activity history. The worktree's git link still points at `/home/mbv/work/eru`, so git can't see the work there.

This document turns what's left into work units that an orchestrator hands to worker and reviewer sub-agents. The design (scope, decisions D1–D15, the manual test checklist and the risks) is carried over from the first build's work-units file, renamed to vaire. It's in the second half of this document, and it's what reviewers check the code against.

**What exists (checked 2026-10-28):** at `fb13ec3`, the uncommitted build is about 2,800 lines in 17 new files (`internal/{config,render,steps,backup,node,health,doctor,cli}/follower*.go`, `internal/node/followerlag.go`, `60-eru-follower.conf.tmpl`) and about 1,200 diff lines in 39 existing files. On a copy of it, `go build`, `go vet` and `go test ./...` pass. Since `fb13ec3`, master has changed 37 of those 39 files, with heavy churn in `node/jobs.go`, `steps/postgres_roles.go`, `cli/tree.go`, `node/doctor.go`, `node/maint.go` and `node/restore.go`.

**Approach (recommended):** port the first build onto master. Don't rebuild it from the design. Integrate it with what master gained, review it in parallel lanes against the design, fix what the reviews find, update the docs, and merge it by pull request. Then live-test it on edge.

---

## Orchestration

### Roles

- **Orchestrator** (this session). It owns the branch, the worktree, git, commits, the findings file and the decisions to bring to the user. It does the git-only units itself (U0, opening the pull request). It never edits code while a worker is running in the same worktree. It checks each worker's result before starting the next unit, by reading the diff and running the verification itself.
- **Workers** (`worker`). Each writes code and tests for one unit, in the phase worktree, and doesn't commit. Only one worker writes in the worktree at a time.
- **Reviewers** (`reviewer`, read-only). Each reviews one lane of the diff against the design and returns findings. Reviewers don't edit files. Lanes run in parallel. The final whole-diff review adds a second model (`codex-exec`) for an independent read.

### Worktree and branches

- `phase-6-wip`: the first build, frozen as one commit on top of `fb13ec3`. It's the port's source and a record of what was built, and nothing else is committed to it.
- `phase-6-followers`: the port, cut from `master` in the worktree `.claude/worktrees/phase-6`. Every unit commits here. The branch that exists today under this name has no commits of its own (it points at `fb13ec3`), so recreating it from master loses nothing.
- The old directory `.claude/worktrees/phase-6-followers` stays until the snapshot is verified. After that, it's removed only with the user's go-ahead.
- Phase 7's worktree (`.claude/worktrees/agent-phase7-extensions`) has the same broken link. It's out of scope here: don't touch it.

### The brief every worker gets

- The unit's text from this document, the design sections it names, and the worktree path.
- The rules in `AGENTS.md`: no local test harnesses (no docker, incus or scratch VMs), fix root causes, Debian 13 and Ubuntu 26.04 servers only, Linux and macOS for the CLI. Steps stay `-v1`.
- Pure logic gets Go unit tests in the existing `_test.go` style. Anything that needs a server goes in a "To verify on the VPS" list in the worker's report.
- Before it reports, the worker runs the full verification: `go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint`.
- The worker doesn't commit, push, create branches or touch other worktrees.
- Its report lists the files it changed, the decisions it made that the design doesn't cover, the tests it added, the verification output, and the VPS items.

### The brief every reviewer gets

- The lane's scope, the design sections it checks against, and the diff range (`master...phase-6-followers`, or a fix commit).
- Each finding has a severity (**blocker**, **major**, **minor** or **nit**), its `file:line`, what's wrong, why (with the design or PostgreSQL behaviour it breaks), a proposed fix, and the unit test that would catch it.
- The reviewer also reports, separately, any decision in the design that it thinks is wrong, as a question for the user and not as a finding.

### Gates

- A unit is done when its verification passes in the orchestrator's own run, its review findings are fixed or explicitly deferred, and it's committed on `phase-6-followers`.
- Findings go in `docs/FINDINGS-phase-6-review.md`, which only the orchestrator writes. Each finding carries an id, a severity, a lane, a status (open, fixed in `<sha>`, deferred with a reason, or rejected with a reason) and its regression test.
- Blockers and majors are fixed before the pull request. Minors are fixed or deferred with a note. Nits are at the orchestrator's discretion.
- The orchestrator stops and asks the user when:
  - a design decision needs to change;
  - a fix would change behaviour from Phases 3–5 beyond the design's list of changes to earlier phases;
  - a worker fails twice on the same unit;
  - the next step is a push, a pull request, a merge, a release or anything on thelab or the VPS.

---

## Units

```
U0 snapshot ─▶ U1 port ─▶ U2 integrate ─▶ U3 review (4 lanes in parallel) ─▶ U4 fixes ─▶ U5 docs ─▶ U6 final gate ─▶ U7 live test
```

### U0. Snapshot and worktree (orchestrator, no sub-agent)

- Commit the old worktree's files as they are, onto `fb13ec3`, as branch `phase-6-wip`. Use a temporary index and `--work-tree` pointed at the old directory, so the files on disk and the main index stay untouched.
- Check that the snapshot is complete: a clean checkout of `phase-6-wip` differs from the old directory by nothing. It builds and its tests pass, as they did on the copy.
- Run `git worktree prune` for the two `eru` paths. Recreate `phase-6-followers` from `master` in `.claude/worktrees/phase-6`.
- **Done when:** `phase-6-wip` holds the first build, `phase-6-followers` equals `master`, and the new worktree builds.
- **Done 2026-10-28:** `phase-6-wip` is `028ad26` (39 modified and 18 new files over `fb13ec3`; it matches the old directory, builds and passes its tests). `phase-6-followers` was cut from master `8d68056` in `.claude/worktrees/phase-6`, and it's the base branch for all Phase 6 work until the merge into master. The prune also dropped Phase 7's dead worktree link; its files are untouched.

### U1. Port the first build onto master (one worker)

- Bring `phase-6-wip`'s diff over `fb13ec3` onto `phase-6-followers`. Use a cherry-pick so git follows the template renames, and resolve each conflict by hand.
- Rename to vaire: `60-vaire-follower.conf(.tmpl)`, the slot prefix `vaire_<follower>`, `vaire-followers.prom`, the `vaire_follower_*` metric families, and the `vaire` command names in messages, hints and tests. Nothing named `eru` is left (`grep -rn eru` over the diff is empty).
- Port only. Conflicts are resolved in favour of master's behaviour plus the follower's addition, and the worker lists every conflict it resolved and how. Master's v1 fixes come through unchanged.
- Main conflicts: `node/jobs.go`, `node/db.go`, `node/restore.go`, `node/recover.go`, `node/methods.go`, `node/dispatch.go`, `node/doctor.go`, `node/maint.go`, `steps/postgres_roles.go`, `steps/timers.go`, `cli/tree.go`, `doctor/checks.go`, and `schema/vaire-v1.json` (renamed).
- **Done when:** the full verification passes, the follower unit tests from `phase-6-wip` all exist and pass, and the conflict list is in the report. The orchestrator commits: `feat: add followers (db follow, db unfollow)`.

### U2. Integrate with what master gained since `fb13ec3` (one worker)

Each item gets a unit test where the logic is pure:

- **Activity history.** `db.follow`, `db.unfollow` and the `db-follow` and `db-unfollow` jobs get entries in `node/activity.go`'s method classification and in `activityjob.go`, so they're recorded and `activity` lookup and incident collection show them. Their refusals are recorded like other refusals.
- **Protection and the TOTP write guard.**
  - `db follow` of a protected source: a follower adds no write path, so it isn't gated.
  - `db unfollow` of a protected follower: allowed (design D10).
  - `db.write-access` on a follower: refused with `is_follower`, because nothing can be written.
  - The write guard in `sql` and `maint` must not mistake a follower for a writable protected database.
- **Awaiting recovery.** `db.follow` and `db.unfollow` are in `awaiting.go`'s list of refused methods (D15).
- **`queries` and `top`.** Decide how the source's `walsender` backend for a follower appears. Recommended: hide it with the other administrative sessions, and show replication in `status` and `db list` instead. Recheck the follower side's startup process the same way.
- **Step secrets and `sql`.** The follow and unfollow jobs take the secrets the way master's `db fork` and `db recover` now do. `vaire sql --db <follower>` with several statements runs read-only and fails cleanly on a write.
- **CLI metadata.** `db follow` and `db unfollow` in `cli/tree.go` match `--check-meta`. The guide text (`internal/guide/agents.md`) and the skill text cover followers.
- **Schema.** `schema/vaire-v1.json` gets the new reasons and warnings if master's schema now enumerates them. It didn't at `fb13ec3`.
- **Done when:** the full verification passes. The orchestrator commits: `feat: integrate followers with activity history and protection`.

### U3. Review (four reviewers in parallel, read-only)

Diff range: `master...phase-6-followers`. The lanes:

| Lane | Scope | Checked against |
| --- | --- | --- |
| R1 Model and configuration | `config/follower.go`, keys and immutability, `tune` (`wal_level`, `max_slot_wal_keep_size`), `render/follower.go` and the template, `steps/follower.go`, `postgres_config`, `postgres_roles`, `pgbouncer`, `pgbackrest`, `timers` | D1, D4 (the cap), D5, D6, D7. Hot-standby floor semantics in PostgreSQL 18. Step input hashes, so a change re-runs the step. The `wal_level` restart path through `--confirm <db>/restart` |
| R2 Lifecycle jobs | `node/follower.go`, `backup/follower.go`, the follow, unfollow, drop and recover paths, locks, fence checks, slot creation and dropping | D2, D3, D4, D8, D10, D11, D14. Cleanup after a failure at every step, resuming an interrupted job, idempotency, SQL and identifier quoting, `pg_basebackup -S` with a reserved slot, removing `standby.signal` and the auto.conf recovery settings on promote |
| R3 Guards and integration | Every RPC method in `node/methods.go` and every CLI loop over databases. Protection, activity, awaiting recovery, `restore promote` and `restore undo` of a source | D9, D12, D15 and U2. Enumerate every method and say whether a follower is refused, skipped or allowed, and whether that matches the design |
| R4 Visibility | `health/follower.go`, `node/followerlag.go`, `doctor/follower.go`, metrics, `db list` and `status` (human output and JSON) | D13. The lag SQL on PostgreSQL 18 (`replay_lag` is NULL when idle, `wal_status`, `safe_wal_size`), thresholds, skip rows, the JSON contract and schema |

- The orchestrator merges the four reports into `docs/FINDINGS-phase-6-review.md`, removes duplicates and assigns ids. It brings the design questions to the user in one batch.
- **Done when:** the findings file is written and the user has answered the design questions.

### U4. Fixes (workers, one batch at a time)

- Group the open blockers, majors and agreed minors into batches by area: one batch per review lane that has findings. Run the batches one after another in the worktree, one worker each.
- Each fix comes with its regression test. After a batch, the reviewer of that lane re-reviews just the batch's diff and checks that each finding is closed.
- The orchestrator commits each batch (`fix(followers): …`) and updates the findings file with the commit.
- **Done when:** no blocker or major is open, and the full verification passes.

### U5. Docs (one worker)

- **MASTER:** Phase 6 🟡 (built, live test pending), the follower files on a node, and the `wal_level` line.
- **IMPLEMENTATION:** the commands, `60-vaire-follower.conf`, the tuning table (`wal_level`, `max_slot_wal_keep_size`), routing, and the new codes.
- **README:** the `db follow` and `db unfollow` usage.
- **MANUAL-TESTING:** the checklist from this document, updated for what U2–U4 changed.
- **PLAN-v1-release and the draft beta notes:** the restart that `wal_level` makes pending on every database after the upgrade from an alpha.
- **This document:** a short "Implementation record" listing where the build differs from the design.
- **Done when:** the docs agree with the code (the reviewer checks this in U6). The orchestrator commits: `docs: phase 6 followers`.

### U6. Final gate (orchestrator and two reviewers)

- A whole-diff review of `master...phase-6-followers`: one `reviewer` and one `codex-exec`, in parallel, both read-only, with the design and the findings file. New blockers and majors go back through U4.
- The orchestrator runs the full verification.
- With the user's go-ahead: push `phase-6-followers` and open a pull request into `master`. The user merges it, which publishes an edge release.

### U7. Live test on edge (orchestrator with the user)

- This comes after activity history's own live test on edge, which the release plan puts first.
- On thelab: `vaire upgrade --yes`, `vaire -c <context> apply --auto-upgrade-nodes`, the `wal_level` restarts with `--confirm <db>/restart`, then the checklist below.
- The orchestrator runs every `vaire`, `curl` and `vaire ssh --command` step itself. The user gets only what needs sudo, a TOTP code, an interactive prompt or the desktop (the rebuild with `server init --restore`).
- Which context to use: the follow-from-stanza, restore and rebuild items need the bucket (`mbvlabs`, which has no live server now). `nobucket` covers the `pg_basebackup` path. The orchestrator proposes the order, and the user decides.
- A bug found live is fixed through a pull request into `master`, released and upgraded. Never patched on the server.
- Results and the date, vaire version and VPS go in MANUAL-TESTING and next to Phase 6 in MASTER.
- **Done when:** the checklist passes or each gap is logged. Then Phase 7 starts its own merge.

---

## Implementation record

Where the build differs from the design below; the findings file and the code win.

- **Resize (Q1a, which supersedes D6's unconditional follower-first rule).** Any resize that would lower a follower's hot-standby floors is refused with `has_followers`; to shrink such a source, unfollow or drop the follower, resize, then follow again. A resize that grows or keeps the floors applies the followers' configuration first, one at a time, before the source, so a retry after a failed follower update runs in the same order. A deferred follower restart leaves `restart_pending` and warns that replay may pause. A hand edit of floor settings in `cluster.toml` followed by plain `vaire apply` is not ordered: run `vaire apply --db <follower>` first.
- **Protection (Q2).** A follower and an unfollowed copy do not inherit the source's protection; an unfollowed copy keeps only the protection the operator gave it. The `db follow` and `db unfollow` help and the guide say so.
- **Visibility (Q3).** The replay-timestamp fallback is gone. Lag in seconds is PostgreSQL's `replay_lag`, with the idle NULL reported as zero only for a streaming follower whose byte backlog was measured as zero; slot retention is measured against `restart_lsn` separately from the replay backlog against `replay_lsn`; unknown values are omitted from JSON and metrics; `source_known`/`self_known`/`source_error`/`self_error` replace one `known`/`error` bit. Doctor still checks a stopped follower's slot from the source, and `db list` names an unread follower side.
- **Resumable follow/recovery.** Both ordinary `db follow` and follower `db recover` write node-local ownership before local mutation. Public retry admits only matching unfinished owned progress. Copied and adopted unfinished copies require matching registration and a `reserved` or `extended` slot with a restart position for reuse; unsafe slots take proven-owned recopy, unreadable inspection fails closed, and foreign paths are refused. Ownership survives failed starts; destructive recopy first invalidates the adopted marker. Preview reports `resume`, `recopy_reason` and `resume_conditional`. These final-gate corrections await independent re-review and live qualification.
- **Lifecycle serialization.** All follow jobs take exclusive locks, reload and revalidate desired state and operation identity before effects. Prepared-entry rollback, including skipped/start-failure cleanup, is serialized and checks entry identity. No repair or refresh command is added.
- **Ordinary operation generation.** A managed immutable in-flight `follow_generation` distinguishes retries from same-shape replacements. Fresh-only untouched preparation cleanup checks current generation and artifacts under lock; retry cleanup never removes existing work. Successful completion clears the generation durably before ownership cleanup; drop retires matching ownership after physical cleanup. Empty awaiting stanza remains valid for follower recovery.
- **Interrupted unfollow protection.** Durable unsettled transition records fence protected arbitrary SQL even for read-intent requests and withhold aliases from both rendered poolers. Strict fencing precedes promotion; admin policy is settled against actual names before exposure, including partial renames. Resume is no longer an unconditional failure cleanup. These second-attempt corrections remain subject to independent review and real-host qualification.
- **New-follow floor admission.** New follow compares proposed standby floors against effective source settings, refusing an unsafe mismatch with an explicit source-restart hint. Unknown necessary facts fail closed. Unrelated pending restarts do not by themselves block admission; no automatic restart or runtime-derived floor configuration is added.
- **Resumable unfollow.** `db unfollow` records its source and stanza in `records.json` before the first irreversible step, holds the node and cluster locks, reloads `cluster.toml`, and resumes at the step an earlier attempt stopped at. A job that ends without a result is `job_outcome_unknown`; the CLI then hands the operator the new passwords, which may have been installed.
- **New warnings.** `slot_not_dropped` (the source was down or refused the drop) and `follower_metrics_failed` (the follower file could not be written) join `followers_diverged` and `follower_not_streaming`.
- **Hot-standby floors.** PostgreSQL numeric floor values are parsed as PostgreSQL reads them; an invalid or unrepresentable value fails the resize instead of silently using a lower floor.
- **Integration.** `db.follow` and `db.unfollow` are in activity history, the protection and TOTP write guards, the awaiting-recovery refusals and the command metadata; the guide covers followers.


# Design (carried over from the first build, 2026-10-16)

Historical introduction: the first build implemented these original proposals.
Later operator decisions and the implementation record above govern current
review; the original approval-request wording is not a new approval gate.

## Scope

**In Phase 6**

- **`vaire db follow <db> [--name <new>] [--size] [--allow-overcommit]`:** a read-only physical standby of one database's cluster, on the same node, as a cluster of its own (`18/<new>`, its own port, size, cgroup and tuning). Materialised from the source's stanza when it has a backup, otherwise with `pg_basebackup` of the live source; then streaming over the source's socket through a physical slot, with `hot_standby = on`.
- **Routing:** a pooler alias in both poolers, `<new> = host=/var/run/postgresql port=<follower's> dbname=<db>`, so `db1.<domain>/<new>` reaches it with the source's credentials, read-only.
- **`vaire db unfollow <new> --confirm <new>/unfollow`:** promote the follower into an independent database: the fork's renames with new passwords (printed once), its own stanza and first full, its timers. The source keeps running.
- **Visibility:** `db list`, `status` and `doctor` show the source, the lag in bytes and seconds, whether it streams, and the slot the source keeps for it; `vaire-followers.prom` for scrapers.
- **Guards:** dropping a source with followers is refused; a follower refuses what needs a writable database or a stanza; `wal_level` drops from `logical` to `replica`; `max_slot_wal_keep_size` caps every slot.
- **Recovery:** after `server init --restore`, a follower comes back `awaiting_recovery` like any database (the bundle already carries `cluster.toml`); `db recover <follower>` follows its recovered source again, and `db recover --all` recovers sources first.

**Not in Phase 6**

- Followers on another node, the `wg0` replication listener and a `replicator` role (Phase 10).
- Logical followers (an opt-in later), cascading followers (a follower of a follower), delayed followers (`recovery_min_apply_delay`), `hot_standby_feedback` as an option (it stays a `[clusters.settings]` key the operator may set).
- `db unfollow --name <other>`: unfollowing keeps the follower's name (D10).
- Failover. Unfollow is not a switchover: the source is never touched.
- Re-following a diverged follower in place (`db follow --refresh`): drop and follow again (D12).

---

## Decisions

Each is **Decided (recommended)**: approve it or change it in review.

### D1. A follower in `cluster.toml`

**Decided (recommended):** a `[[clusters]]` entry with `follows = "<source>"` and **no stanza** (the RFC's example). `follows` is immutable: `config set` refuses it, and `config edit` refuses changing it on an existing entry and refuses a new entry that carries it (a follower is made by `db follow` only, never by `apply`, which would `initdb` an empty cluster). Only `db unfollow` clears it, giving the entry a stanza at the same moment. Validation:

- `follows` names another entry, which isn't itself a follower (no cascading) and isn't the entry itself.
- A follower has an empty `stanza`; every other entry still needs `<name>-<6 hex>`.
- A follower has no `[clusters.backup]`: it has nothing to back up, its source's backups cover the data.
- A follower lives on its source's node (there is only the master until Phase 10).
- `primary_conninfo`, `primary_slot_name`, `hot_standby`, `restore_command` and `recovery_target*` become reserved setting keys: vaire writes them for followers.

Its size, limits, pools, profile, statement timeout, extensions and settings are its own, so the budget, `tune show`, cgroups and the `postgres_exporter` probe treat it as any cluster. Helpers: `ClusterSpec.IsFollower()`, `ClusterSpec.DatabaseName()` (the database inside the cluster: the source's name for a follower), `Cluster.FollowersOf(name)`, `Cluster.StanzaClusters()` (every entry with a stanza), `config.FollowerSlot(name)`.

### D2. Where the copy comes from

**Decided (recommended), as MASTER says:** from the source's stanza when backups are on and the stanza has a backup set (`pgbackrest restore --type=standby` on the timeline `records.TargetTimeline` gives, the path `restore --latest` already takes); otherwise `pg_basebackup -X stream -c fast -S vaire_<new>` of the live source over its socket, plus `standby.signal`. The stanza path reads the bucket instead of the production cluster's disk; the `pg_basebackup` path needs no bucket and is what a server without backups gets.

Both start the copy once as a temporary cluster (`UntilOpen`: it reached a consistent state), then `Adopt` makes it `18/<new>` on its own port. pgBackRest's recovery settings are removed from `postgresql.auto.conf`, and the follower's whole managed configuration, `60-vaire-follower.conf` included (D6), is written **before** its first start under systemd (`steps.WriteFollowerConf`): a standby that started with defaults below the source's floors would pause replay at once, and one without `primary_conninfo` would never stream.

### D3. The replication connection

**Decided (recommended):** the follower runs on the source's node, so it connects over the source's Unix socket as `postgres` with peer authentication: `primary_conninfo = 'host=/var/run/postgresql port=<source> user=postgres application_name=<follower>'`. `pg_hba.conf` already has `local replication postgres peer` (Phase 3 added it for `pg_basebackup`). No password, no new role, no TCP listener. Phase 10's followers on another node need the `wg0` listener, TLS and a `replicator` role; that's designed there.

### D4. The slot and how much WAL it may hold

**Decided (recommended):**

- One physical slot per follower on its source, `vaire_<follower>` (at most 59 characters: a database name is at most 55). Created with `immediately_reserve` before the copy starts (an existing inactive `vaire_<follower>` from a failed attempt is dropped first), so WAL from then on stays on the source until the follower has it. The `pg_basebackup` path streams its WAL through the same slot (`-S`). `primary_slot_name` names it.
- **`max_slot_wal_keep_size`** caps every slot, on every cluster: 10% of the cluster's share of the data disk, 2-64 GiB (the same share `max_wal_size` uses), in `internal/tune`, a reload. A follower that's down longer loses its slot (`wal_status = lost`): it catches up from the archive when the source has a stanza (D6's `restore_command`), otherwise it has to be dropped and followed again. Doctor fails either way.
- `db unfollow` and `db drop` of the follower drop the slot. A slot named `vaire_*` that no follower owns is reported by doctor (`follower.orphan_slots`, warn) with the SQL to drop it; vaire never drops it by itself.

### D5. `wal_level`

**Decided (recommended), as MASTER says:** `replica` instead of `logical` on every cluster. It's a postmaster setting, so after an upgrade `apply` leaves every cluster's restart pending (`restart_pending` with `<db>/restart`, doctor's `pg.restart_pending`), as any restart-only tuning change does; nothing restarts by itself. Nothing in vaire uses logical decoding; physical streaming works with either level, so followers work before the restart too. Logical replication as an opt-in later sets it back per database.

### D6. The follower's configuration

**Decided (recommended):** `postgres-config-v1@<follower>` renders the follower's own tuning (from its size), then one more file, **`60-vaire-follower.conf`**, only for a follower:

- The **hot-standby floors**: `max_connections`, `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions` and `max_locks_per_transaction` are the larger of the follower's own and the source's, each computed from `cluster.toml` (the tuning, then `[postgres.settings]`, then `[clusters.settings]`, then PostgreSQL's default). A small follower of a large source pays with a larger `max_connections` than its size gives; its memory is still its size's.
- `hot_standby = on`, `primary_conninfo` (D3), `primary_slot_name` (D4).
- With backups on, `restore_command = '/usr/bin/pgbackrest --pg1-path=<follower's data> --stanza=<source's stanza> archive-get %f "%p"'`, so a follower behind its slot (or that lost it) catches up from the source's archive, and `recovery_target_timeline` when `restore undo` recorded one for the source's stanza (otherwise PostgreSQL's `latest`).
- `40-vaire-backup.conf` renders `archive_command = '/bin/true'` (no stanza); on a standby `archive_mode = on` archives nothing anyway.
- `hot_standby_feedback` stays off (PostgreSQL's default): a long query on the follower must not hold back vacuum on production; it's cancelled after `max_standby_streaming_delay` (30 s). An operator who wants otherwise sets it in `[clusters.settings]`.

`postgres-roles-v1@<follower>` skips itself (reason `follower`): the roles, the database, the auth functions and the service roles' verifiers are the source's, replicated. `pgbackrest-v1` and `timers-v1` leave followers out (`StanzaClusters`).

Because the floors come from the source's entry, **`db resize` of a source** applies its followers' configuration first, in the same run: a reload, or a restart when the request carries the follower's own token (`--confirm <follower>/restart`, which `db resize` already accepts); without it the warning `restart_pending` names the token. A follower restarted after its source with a higher floor pauses replay until it restarts (doctor's `follower.streaming` fails on the paused replay).

### D7. Routing and credentials

**Decided (recommended):** both poolers get `<follower> = host=/var/run/postgresql port=<follower's> dbname=<source> auth_dbname=<follower>__auth`, through `PoolerDatabase.DBName`, which `<db>__restoring` already uses. `auth_query` runs on the follower's own `postgres` database, where the replicated auth functions answer. Clients connect to `db1.<domain>:6432/<follower>` (or 5432, or 443) with the source's `<db>_app` or `<db>_ro` credentials; writes fail as on any standby. `vaire sql --db <follower>`, `psql`, `console` and `tunnel` route the same way through the session pooler as `vaire_admin`. `db credentials <follower> --rotate` refuses (`is_follower`): the source's credentials are the follower's.

### D8. Size and the budget

**Decided (recommended):** a follower takes the source's size, limits, pools, profile, statement timeout, extensions and settings (`forkSpec`), or `--size`. It goes through `addDatabase` like `db create` and `db fork`: `would_not_start` and `memory_overcommitted` refuse it, the latter with the largest size that fits as a complete command (`vaire db follow <db> --name <new> --size <size>`), and `--allow-overcommit` follows anyway with the warning. It needs free space for one copy of the source.

### D9. What a follower refuses, and what skips it

**Decided (recommended):** `precondition_failed` with reason `is_follower`, the follower and its source in details, and a hint naming the source or `db unfollow`:

| Refused for a follower | Why |
| --- | --- |
| `backup now`, `backup verify`, `restore` (start, promote, undo), `restore drill`, `dump`, `db fork <follower>` | No stanza: back up, restore or fork the source |
| `db credentials --rotate` | Its roles are the source's, read-only |
| `maint vacuum`, `analyze`, `reindex`, `repack` | Read-only: maintenance runs on the source and replicates |
| `db follow <follower>` | No cascading (`follows_follower`) |
| `db unfollow <db>` of a database that follows nothing | `not_a_follower` |

Skipped quietly: the backup timers, the backup rows of `backup list` (one row per stanza), the CLI's loops over every database (`backup now`, `backup verify`, `restore drill`, `maint` without `--db`), the daily and weekly maintenance jobs, doctor's `backup.*` checks (`skip`, "a follower has no stanza; <source>'s backups cover it"). Allowed: `db resize`, `db protect`, `apply --db`, `queries`, `maint report`, `maint kill`, `top`, `status`, `logs`, `sql`.

### D10. `db unfollow`

**Decided (recommended):** `vaire db unfollow <follower> --confirm <follower>/unfollow` (destructive: the follower stops following for good, and the credentials that work on it change). A detachable job, `db-unfollow`:

1. Promote it (`backup.Promote`): it opens on a new timeline. Skipped when it's already out of recovery (a resumed unfollow).
2. The fork's renames (`renameFork`): `<source>` → `<follower>`, `<source>_owner|migrate|app|ro` → `<follower>_…`, with the passwords the operator generated (printed once). A resumed unfollow whose renames are done sets the passwords only.
3. `KILL <follower>` on both poolers: its clients logged in as the source's roles.
4. In `cluster.toml`: clear `follows`, give it a new stanza (`<follower>-<6 hex>`). Remove `60-vaire-follower.conf`.
5. Drop the slot on the source.
6. Its own steps with a restart allowed (the floors go, `archive_command` points at its stanza), the poolers (the alias becomes a plain entry), `RESUME <follower>` on both, metrics, then with backups `pgbackrest-v1` (its stanza and first full) and `timers-v1`. A failed first backup is the warning `first_backup_failed`, as a fork's.

The name stays the follower's: `--name` is deferred (fork the unfollowed database to get another name). Unfollow is refused while the follower's cluster is down (`follower_not_running`). A protected follower may be unfollowed (nothing is lost).

### D11. Dropping

**Decided (recommended):**

- `db drop <source>` while followers exist: `precondition_failed`, reason `has_followers`, the followers in details, hint `vaire db drop <f> --confirm <f>` or `vaire db unfollow <f> --confirm <f>/unfollow` first. Protection doesn't change this.
- `db drop <follower>`: no final dump and no stanza to keep (nothing of its own); stop and drop the cluster, drop the slot on the source, then the poolers and metrics as any drop. A follower awaiting recovery has no cluster; its entry goes, and its source's slot too if one is left.

### D12. A source that's restored

**Decided (recommended):** `restore promote` and `restore undo` of a source stay allowed (a restore is an emergency; a rehearsal follower must not block it), but the result warns `followers_diverged`: the source opens on a new timeline from an earlier point and its swapped-in data directory has no slot (slots aren't in backups). Each follower stops replaying. The hint is to drop and follow again. Doctor fails `follower.streaming` until then. Nothing else in Phase 3's promote changes.

### D13. Lag and visibility

**Decided (recommended), as built:** read on the node from both sides, independently:

- On the source: the newest `pg_stat_replication` row for `application_name = <follower>` (state, `replay_lag`, `pg_current_wal_lsn() - replay_lsn` as the byte backlog), and the `pg_replication_slots` row for `vaire_<follower>` (active, `wal_status`, `pg_current_wal_lsn() - restart_lsn` as the WAL it retains, `safe_wal_size`).
- On the follower: `pg_is_in_recovery()` and `pg_get_wal_replay_pause_state()`.
- **Lag in seconds is PostgreSQL's `replay_lag`**, the replay/visibility delay; it is not a catch-up estimate and not the age of an idle source. PostgreSQL reports NULL once a standby has caught up and the source is idle: vaire reports zero only for a streaming follower whose byte backlog was measured as zero. A positive or unknown backlog with no timing sample leaves the seconds unknown. There is no replay-timestamp fallback.
- **Slot retention is measured separately**, against `restart_lsn`; it is never substituted for the replay byte lag, which is measured against `replay_lsn` and needs a walsender.
- **Unknown is not zero.** `lag_bytes`, `lag_seconds` and `slot_retained_bytes` are omitted when unmeasured. `source_known` and `self_known` say independently whether each side answered, and `source_error`/`self_error` say why not (they replace one `known`/`error` bit). A fact read from an unread side is omitted too: `streaming`, `slot_exists`, `slot_active` and `slot_safe_bytes` without the source, `in_recovery` and `replay_paused` without the follower. Slot allowance is nullable independently: unknown/malformed/NULL without positive unlimited-cap evidence is omitted. Measured zero or negative allowance remains measured; proven unlimited capacity uses `slot_safe_bytes = -1` with `slot_uncapped = true`, distinct from a measured negative allowance. Missing/lost slots never establish unlimited capacity.
- `db list`: state `following` for a running follower, `follows <source>` in the STANZA column, and the lag after the state, naming an unknown side or a paused replay instead of printing a healthy value. JSON carries `follower` (source, `source_known`/`self_known` and their errors, streaming, lag bytes and seconds, slot). `status` is the same per database, with the source's slot.
- Doctor (category `postgres`, `internal/doctor/follower.go`): `follower.streaming` (fail when not streaming, out of recovery, or replay paused), `follower.lag` (warn at 5 min or 1 GiB, fail at 1 h or 16 GiB), `follower.slot` (warn when the slot holds more than half of what it may, fail when lost or missing), `follower.orphan_slots` (warn on a source). A stopped follower skips the streaming and lag checks but its slot is still checked from the source; a failure one side proves stands even when the other is unread, and an unread side warns. A server without followers gets one `skip` row per check.
- Metrics: `vaire-followers.prom`, written by `metrics-refresh`: `vaire_follower_streaming{db,source}`, `vaire_follower_lag_bytes`, `vaire_follower_lag_seconds`, `vaire_follower_slot_retained_bytes`, `vaire_follower_slot_lost`. A sample is written only for a measured value; a failed write fails refresh and the job warns `follower_metrics_failed`.

### D14. Recovery after a rebuild

**Decided (recommended):** nothing new in the bundle (`follows` is in `cluster.toml`; slots are made again). Placement marks a follower `awaiting_recovery` like everything else. `db recover <follower>`:

- Refused while its source still awaits recovery (`source_awaiting_recovery`, hint `vaire db recover <source>`).
- Runs the follow job again for the existing entry, from the recovered source (D2): from its stanza when it has a backup, else `pg_basebackup`. It needs no bucket of its own, so it is allowed without one. `--time` and `--no-promote` are refused for a follower (`usage_error`): it follows the source's present.
- `db recover --all` puts every follower after every source (then protected first, smallest first, as today), and the fulls at the end skip followers.

### D15. Commands, tokens and codes

**Decided (recommended):**

- `vaire db follow <db> [--name <new>] [--size <size>] [--allow-overcommit] [--dry-run]`: mutating, a detachable job (`db-follow`); `--name` defaults to `<db>_follower`.
- `vaire db unfollow <follower> --confirm <follower>/unfollow [--dry-run]`: destructive, a detachable job (`db-unfollow`); prints the new credentials once.
- New `precondition_failed` reasons: `has_followers`, `is_follower`, `follows_follower`, `not_a_follower`, `source_awaiting_recovery`, `source_not_running`, `follower_not_running`. New warnings: `followers_diverged`, `follower_not_streaming`. `awaiting_recovery` refusals gain `db.follow` and `db.unfollow`.

### Changes to earlier phases

- `wal_level` `logical` → `replica`, and `max_slot_wal_keep_size` on every cluster (D4, D5): the next `apply` leaves every cluster's restart pending.
- `db drop` refuses a source with followers (D11); `restore promote` and `restore undo` warn `followers_diverged` (D12).
- `db resize` of a source also applies its followers' configuration (D6).
- `db recover --all` orders followers last and skips their fulls (D14); `db recover` of a follower needs no bucket.
- A `stanza` may be empty, for followers only; `config edit` refuses a new entry with `follows` (D1). Five more reserved setting keys (D1).
- `archive-push-queue-max` divides the spool among the clusters with a stanza, not all clusters (unchanged without followers).
- `backup list` without `--db` lists the databases with a stanza; the CLI's loops over every database (`backup now`, `backup verify`, `restore drill`, `maint`) skip followers.

---

## Map of the first build (units 1–11 of 2026-10-16)

Where each part lives, for the port and the reviewers. Units 12 and 13 of the first build (docs, live test) are now U5 and U7.

### A. Model and configuration

#### 1. `follows` in `cluster.toml`
- `ClusterSpec.Follows`, the helpers of D1, validation, `follows` in `ImmutableChanges` (changed, or set on a new entry), the reserved keys, `Stanzas()` skipping empty stanzas.
- Files: `internal/config/{clusters,follower,keys,cluster}.go`.
- Tests: a valid follower; cascading, self, unknown source, another node, a stanza on a follower, a missing stanza on a primary, `[clusters.backup]` on a follower; the helpers; `config set` and `config edit` of `follows` refused, a new entry with `follows` refused.
- Done when: a hand-written follower entry validates and `config set clusters.f.follows x` is refused.

#### 2. Tuning: `wal_level` and `max_slot_wal_keep_size`
- `internal/tune`: `wal_level = replica`, `max_slot_wal_keep_size` from the disk share (D4, D5). Two lines beside `max_wal_size`.
- Tests: the values and their clamps.
- To verify on the VPS: `apply` after the upgrade leaves `restart_pending` for each database, and `--confirm <db>/restart` clears it.

#### 3. The follower's configuration
- `render.FollowerConf` and `60-vaire-follower.conf.tmpl`; `steps.followerFiles` adds the file to a follower's `postgres-config-v1` (one line in `confFiles`); the floors from both entries; `postgres-roles-v1` skips followers; `steps.WriteFollowerConf` for the first start.
- Files: `internal/render/follower.go`, `internal/render/templates/60-vaire-follower.conf.tmpl`, `internal/steps/{follower,postgres_config,postgres_roles}.go`.
- Tests: the floors take the larger value, settings win over tuning; conninfo, slot, restore_command and timeline; the file absent for a primary; the roles step's skip.
- To verify on the VPS: the follower starts under systemd with the floors and streams.

#### 4. Routing, pgBackRest, timers and metrics
- `poolerDatabases` gives a follower `DBName = <source>`; `PgBackRestConf`, `pgbackrest-v1` and `timers-v1` use `StanzaClusters`; the `postgres_exporter` probe opens the database inside the cluster.
- Files: `internal/steps/{pgbouncer,pgbackrest,timers}.go`, `internal/render/{render,metrics}.go`.
- Tests: the pooler line; no stanza section and no timer for a follower; the probe's database.

---

### B. Commands

#### 5. `db follow`
- RPC `db.follow {source, name, size, allow_overcommit}`: the checks of D8 and D9, `addDatabase` (no role passwords and no stanza for a follower), the job `db-follow`.
- The job: the slot, materialise (D2, `backup.Materialised.StandbyBaseBackup` beside `Restore`), `Adopt`, `WriteFollowerConf`, the kernel, the follower's own steps, the poolers, metrics, the other databases' changed configuration, then a short wait for streaming (`follower_not_streaming` after a minute). A failure before the follower is `18/<new>` drops the copy, the slot and the entry.
- CLI `vaire db follow`, metadata, progress, `job list` naming.
- Files: `internal/node/{follower,methods,jobs,db}.go`, `internal/backup/follower.go`, `internal/cli/{follower,tree,jobs,progress}.go`.
- Tests: the refusals; the default name; the size command.
- To verify on the VPS: both paths (with and without a bucket); the follower reaches `streaming` in `pg_stat_replication`; `app_ro` reads through `db1.<domain>:6432/app_follower`; a write fails with "read-only transaction".

#### 6. `db unfollow`
- RPC `db.unfollow {name}` (token `<f>/unfollow`), job `db-unfollow` as D10; the CLI prints the credentials once.
- Files: `internal/node/follower.go`, `internal/cli/follower.go`.
- Tests: the token; the refusals (`not_a_follower`).
- To verify on the VPS: row counts and sequence values equal the source's at the promote; the new credentials connect, the old ones don't; a stanza and a first full exist; the source's slot is gone.

#### 7. Drop, resize, restore and the other guards
- `db drop` (D11), `db resize` of a source (D6), `followers_diverged` (D12), the refusals of D9 through one helper (`refuseFollower`), the CLI loops skipping followers.
- Files: `internal/node/{db,restore,backup,maint,follower,awaiting}.go`, `internal/cli/backup.go`.
- Tests: the refusal table; `has_followers`.

---

### C. Visibility

#### 8. Lag
- `followerState(ctx, cfg, spec)`: D13's reading from both clusters, parsed by pure functions.
- Files: `internal/node/followerlag.go`.
- Tests: parsing the rows (streaming, caught up and idle, not streaming, slot lost, no slot).

#### 9. `db list` and `status`
- The `following` state, the source and lag; `status`'s follower block; the CLI renderings.
- Files: `internal/node/{db,status}.go`, `internal/cli/{db,status}.go`.

#### 10. Doctor and metrics
- `internal/doctor/follower.go` with D13's checks; `ClusterFacts.Follows` and `.Follower`; backup checks skip followers; `vaire-followers.prom`.
- Files: `internal/doctor/{follower,checks,doctor}.go`, `internal/node/{doctor,metrics}.go`.
- Tests: thresholds; skip rows without followers; backup checks skip a follower; the prom families.

---

### D. Recovery and docs

#### 11. `db recover` of a follower
- `handleDBRecover` hands a follower to the follow job (D14); `recoverOrder` puts followers last; the fulls skip followers.
- Files: `internal/node/{recover,follower}.go`, `internal/cli/recover.go`.
- Tests: the order; the refusals.
- To verify on the VPS: after `server init --restore`, `db recover --all` brings back the source, then the follower streaming from it.


## Manual test checklist (added to MANUAL-TESTING.md)

Run on the server the the live test context chosen in U7, after an `vaire apply` with the new build.

- [ ] **The upgrade**
    - `vaire apply`: every database's `postgres-config-v1` re-runs and warns `restart_pending` (`wal_level`); `vaire apply --confirm app/restart --confirm other/restart` clears it. `SHOW wal_level` is `replica`, `SHOW max_slot_wal_keep_size` matches `vaire tune explain --db app`.
- [ ] **Follow under write load (from the stanza)**
    - Start a write loop on `app` (an `insert … select generate_series` every second). `vaire db follow app --name app_follower --size small`. Expected: a `db-follow` job that restores from `app`'s stanza, then `app_follower` in `db list` as `following`, `follows app`; the lag reaches 0 within a minute (`vaire db list`, `vaire status --db app_follower`).
    - On the node: `select * from pg_replication_slots` in `app` has `vaire_app_follower`, active; `pg_stat_replication` shows it `streaming`; `/etc/postgresql/18/app_follower/conf.d/60-vaire-follower.conf` has the floors, `primary_conninfo` and `restore_command`; `postgresql.auto.conf` has no `restore_command`.
    - `psql "postgres://app_ro:…@db1.<domain>:6432/app_follower?sslmode=verify-full"` reads the newest rows; an `insert` fails with "cannot execute INSERT in a read-only transaction". The same through 443 with `sslnegotiation=direct`, and `vaire sql --db app_follower "select count(*) from …"`.
    - `vaire doctor`: `follower.streaming`, `follower.lag` and `follower.slot` pass for `app_follower`; its `backup.*` checks are `skip`.
    - `vaire metrics show` / scrape `/metrics/node`: `vaire_follower_lag_bytes{db="app_follower",source="app"}`.
- [ ] **Follow without backups**
    - On a database whose stanza has no backup yet (or a server without a bucket), `vaire db follow other`. Expected: the `pg_basebackup` path, then streaming as above.
- [ ] **The budget**
    - `vaire db follow app --name too_big --size xlarge` on a small server: refused with `memory_overcommitted` and the size that fits as a command.
- [ ] **Guards**
    - `vaire db drop app --confirm app`: refused with `has_followers`. `vaire backup now --db app_follower`, `vaire restore app_follower --latest --confirm app_follower/restore`, `vaire db fork app_follower`, `vaire db credentials app_follower --rotate --confirm app_follower`, `vaire maint vacuum --db app_follower`: each refused with `is_follower`. `vaire db follow app_follower`: `follows_follower`. `vaire backup now` without `--db` skips the follower.
- [ ] **A follower that falls behind**
    - `systemctl stop postgresql@18-app_follower` for a few minutes under the write loop: doctor fails `follower.streaming`, the slot's retained bytes grow (`vaire status --db app_follower`); start it again and the lag returns to 0.
- [ ] **Unfollow and rehearse a migration**
    - Stop the write loop. `vaire db unfollow app_follower --confirm app_follower/unfollow`. Expected: new credentials printed once, a stanza and a first full (`vaire backup list --db app_follower`), its timers.
    - Row counts and `select last_value from <seq>` for each sequence equal `app`'s at the moment of the promote; `app_follower_app` connects with the new password; `app_app` doesn't connect to `app_follower`; the slot `vaire_app_follower` is gone from `app`.
    - Run a migration on `app_follower` (an `alter table … add column … default …` through `vaire tunnel --db app_follower --target postgres` as `app_follower_migrate`). Then `vaire db drop app_follower --confirm app_follower`.
- [ ] **Resize a source**
    - Follow `app` again, then `vaire db resize app --size large --confirm app_follower/restart --confirm app/restart`: the follower restarts first with the higher floors, then `app`, and it keeps streaming. Without the follower's token the result warns `restart_pending` for it.
- [ ] **Restore a source with a follower**
    - `app` is protected since the Phase 5 test: lift it first with `vaire db unprotect app` (a human, with the authenticator), and `vaire db protect app` again afterwards.
    - `vaire restore app --time <t> --confirm app/restore`, then `vaire restore promote app --confirm app/restore`: warns `followers_diverged`; doctor fails `follower.streaming`; `vaire db drop app_follower --confirm app_follower` and `vaire db follow app` again bring it back.
- [ ] **Recovery after a rebuild** (with the Phase 5 rebuild, or the next one)
    - With `app_follower` following `app`, rebuild with `server init --restore`. `db list` shows both `awaiting_recovery`; `vaire db recover app_follower` is refused with `source_awaiting_recovery`; `vaire db recover --all` recovers `app`, then `app_follower` (following again), and takes a full of `app` only.

---

## Risks

- **A small follower of a large source isn't small.** The hot-standby floors give it the source's `max_connections` and worker counts whatever its size; its memory is still its size's (`shared_buffers`, `MemoryHigh`), and `work_mem` comes from its own tuning. Measured in the manual test.
- **Slots on the production cluster.** A follower that stops keeps WAL on the source, up to `max_slot_wal_keep_size` (10% of its disk share, at most 64 GiB). That's disk the source's neighbours share. Doctor warns at half and fails when the slot is lost; dropping the follower drops the slot. An orphaned `vaire_*` slot is reported, not dropped by itself.
- **A source restored under its followers** leaves them diverged (D12): they keep serving old data read-only until they're dropped and followed again. Doctor fails, the promote warns.
- **`wal_level` needs a restart of every database** on the first `apply` after the upgrade. Nothing restarts by itself; until restarted the clusters keep `logical`, which still serves followers.
- **Read queries on a follower get cancelled** by replay conflicts after 30 s (`hot_standby_feedback = off`, D6). Rehearsing a migration means unfollowing first; reading on a follower is best effort, as on Heroku.
- **Floors from `cluster.toml` only.** A `90-overrides.conf` raising `max_connections` on the source isn't seen; the follower's replay pauses on the parameter change, which doctor's `follower.streaming` shows. The fix is the same setting in the follower's `90-overrides.conf`, or `[clusters.settings]` on both.
- **Not verified without a server:** a physical standby registered by vaire's own `Adopt` starting under `postgresql@18-<f>` with `standby.signal`; peer replication over the socket from inside a cgroup'd unit; `pg_basebackup -S` with an `immediately_reserve` slot; pgBackRest's `archive-get` with `--pg1-path` of the follower; the pooler's `auth_query` on a standby; `KILL`/`RESUME` around the unfollow's renames.
