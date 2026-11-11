# Agent-facing FG-05 second correction brief

This is implementation routing, not a new human approval request. The user
authorized the nine fixes and both lifecycle additions and explicitly requested
execution without redundant review gates. No commits, publication or live work.

## Input checkpoint and acceptance state

HEAD `2155a07300095d4845caedc37cd8c3d1c9928d73`; base
`8d68056621b19da318ce972552d45973b7feda4e`; current uncommitted tracked delta
SHA-256 `eae3bf1df93bf330de9499a20393735b65acfeb022e1578f844f5623473086f9`.
Existing tracked and untracked changes must be preserved.

Both re-review lanes independently BLOCK. Independent final-tree local checks
PASS, including full uncached suite; passing tests miss these boundaries.
All five previous consolidated minors are accepted on source review.

This is the second corrective attempt for the affected lifecycle/security units.
If still incomplete after implementation and re-review, stop; no third attempt
without asking. No writer/reviewer overlap. Canonical findings/report/work-units
remain orchestrator-owned. Agent may update IMPLEMENTATION for this exact delta.

## Required lifecycle corrections

1. **A-R1 / RB-01, Major:** `followerownership.go:176–180` rejects empty
   `Awaiting.Stanza`, but real follower rebuild records from `awaitingOf` and
   `backup.PruneForRestore` legitimately have empty stanza. Require actual
   awaiting presence/nonempty generation (`Since`), exact stanza equality
   including empty, explicit operation kind, instance and source. Fix the fake
   nonempty follower stanza fixture. Regress records produced by real placement/
   pruning through initial copy, retained-slot continuation and adopted recopy.
2. **A-R2 / RB-02, Major:** `notRun` routes public retries into preparation
   rollback; after acquiring the lock it deletes matching config even if another
   attempt owns or completed the copy. Distinguish fresh preparation from retry.
   A retry invocation never removes an existing entry. Fresh cleanup must reload
   and inspect current generation, ownership, registration/data/running state
   after locking; remove only untouched preparation, never completed/progressed/
   foreign/uncertain work. Test actual public same-target skipped retry, and
   fresh cleanup overtaken by another successful attempt before lock release.
3. **A-R3 / RB-02 ABA, Major:** name/node/port/source hash is not operation
   identity. Drop/recreate can reuse every value; stale jobs/rollback/ownership
   can claim replacement. Use a genuine opaque generation bound atomically to
   prepared desired state, job and ownership. Recommended minimal managed optional
   `follow_generation` on the cluster entry: random per fresh follow under
   exclusive lock, operator-immutable including config edit/new-entry validation,
   preserved on retries and settings changes. Clear atomically on successful
   completion before best-effort ownership deletion; failure to save completion
   is not success. Missing generation is never evidence for ordinary continuation.
   Retire matching old generation/ownership through normal successful drop,
   preserving evidence if physical cleanup fails. Test identical name/source/node/
   reused-port replacement, stale job, stale rollback and old ownership refusal.
   No broad state transaction framework or legacy ownership-hash promotion.
4. **A-R5 / RB-05, Major:** `followRetryable` rejects running target before
   checking unfinished owned operation. Check durable completion/generation and
   matching ownership first; admit unfinished running or stopped copies, refuse
   completed/unowned running AND stopped targets. Retain slot inspection and
   proven-owned recopy rules. Test actual public admission after successful
   permanent start followed by failure, not a stub that always says stopped.
5. **A-R6 / RB-04, Major:** effective-floor check is pre-lock only. Source
   can shrink desired floors with deferred restart between check and preparation;
   locked job reload then renders unsafe lower floors. Compare exact complete
   proposed config with target present exactly once, under preparation lock and
   again after locked execution reload/generation validation before ownership/
   slot/copy/start effects. Retry safety also needs current facts. Tests must
   change desired floors between preflight and lock while retaining higher live
   values; safe/equal/increase and unread facts must retain intended behavior.
6. **RB-06, Minor:** proposed membership itself changes CPU-derived tuning
   (four CPUs, one source worker floor 12 versus two clusters floor 10), so a
   restart of the unchanged one-cluster source cannot resolve the mismatch.
   Do not assert deferred restart without evidence. Report actual/proposed floors
   and accurate supported explicit-setting prerequisite, or only offer restart
   when proven sufficient. No auto-derived persistent floor settings, no auto
   restart and no unsafe bypass. Test real tuning-derived membership mismatch
   and unchanged-source restart not resolving it.
7. **RB-07, Minor:** IMPLEMENTATION still describes adopted-slot exemption and
   recovery-only ownership. Reconcile after final lifecycle implementation with
   both operation kinds, generation, slot validation through adoption, ownership
   through failed starts, marker invalidation before destructive recopy and
   completion handling. No live evidence claims.

Lock order remains global → heavy I/O → sorted clusters. Distinguish helpers
called with lock held from helpers acquiring it; flock is not reentrant. Reload
and identity checks must agree with acquired source/target lock set. Keep Q1a,
Q2 and Q3, ownership/foreign refusal, bounded cleanup and no catch-up wait.

## Required security correction (separate serial writer)

**A-R4 / RB-03, Major:** follower copied from unprotected source retains
`vaire_admin` owner membership. Explicitly protect the follower, then interrupt
unfollow after promotion before rename/policy settlement. `write:false` arbitrary
SQL runs as writable inherited admin and can COMMIT/SET ROLE/write, bypassing
TOTP. Current `p.Write` refusal and test do not prove read safety.

- Fence arbitrary node SQL for protected unfinished unfollow, including read
  intent, using durable transition state; fail closed on unread transition state.
- Ensure actual protected admin policy is settled before exposing promoted
  access. Require effective pooler fencing before promotion; best-effort KILL
  plus unconditional deferred RESUME is not sufficient.
- Use actual current database/role names during partial renames and existing
  protection-policy SQL; resume only after safe privilege settlement. Persisted
  transition must keep unsettled aliases fenced across pooler render/restart,
  not only in transient PgBouncer memory.
- No SQL text filtering, superuser execution with resettable SET ROLE, weakening
  primary TOTP, or change to deliberate maintenance-kill policy.
- Regress real wrapper with `write:false` transaction escape, unprotected source
  and explicitly protected follower; failure after promotion/before rename and
  before policy settlement; pooler failure/resume and durable re-render behavior.
  Keep ordinary physical follower reads and protected-primary regression checks.

## Worker/test/review handoff

Explain minimal changes before editing. Add actual-boundary regressions; do not
paper over these scenarios with successful callback assertions. No commits,
branches/tags, fetch/push, other worktree mutations, servers/contexts/secrets,
installs/sudo/cache deletion, Docker/Incus/VM harness or resource cleanup.

Run full AGENTS chain and uncached targeted packages. Record all failures and
later-not-run status honestly. Save owner-only logs below the existing
`/tmp/opencode/followers-final-gate-20261110/` run directory. Report source/tests,
exact regressions, results and limits. Then freeze for independent tester and
both whole-diff lanes; source review, local execution and live evidence differ.
