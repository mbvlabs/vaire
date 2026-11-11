# Agent contract: activation correction

Oracle refinement after the first integrated E7-05 correction. These implement approved safety and retry obligations; no new command/framework/user decision is required. Findings F7-29–38 remain open until independent closure.

## Durable phases

| Phase | Meaning | Allowed continuation |
| --- | --- | --- |
| prepared | Identity/requirements fixed; credential-bearing work may remain; all user access withheld | Owning operation only; no ordinary scaffold or guessed credential replay |
| adopting (fork) | Credentials/rename/copied-job retarget completed; checkpoint written before Adopt | Original operation or validated apply may idempotently finish adoption without passwords |
| adopted (fork) | Actual registration verified; configuration/start/settings/SQL may remain | Scoped apply executes complete non-secret continuation |
| routing | All safety settlement complete; only route publication/resume remains | Shared verified routing finalizer |
| absent | Both poolers verified routed, pause released, exact fence retired | Ordinary operation |

Unknown/malformed phases fail closed. Restore uses existing swap/promotion facts; promoted alone never proves fork adoption. Remove the loose settled phase or keep it owner-only without bypassing routing eligibility.

Fork allocates destination stanza/port before prepared fence and entry publication. After promotion/deactivation/retarget/credential rename, persist adopting before Adopt, then adopted immediately after verified adoption, before settings save or engine work. Apply continuation persists disabling settings, performs owned guarded configuration/start, desired SQL and cron/effective verification, then safe guard handoff and routing finalization. It must not merely run SQL on a stopped/incompletely configured copy. No passwords or credential replay in records/apply.

Restore retirement and fence routing eligibility are one UpdateRecords compare-and-set: verify full restore/fence identity, retire restore, update timeline/leftover/replaced-stanza facts, and set routing together. Unfollow safety completion and routing phase likewise atomic. Failed update leaves a coherent old continuation.

## Operational identity

Use actual nonempty installation identity from existing `masterbackup.ReadInstance("")` / `followHostInstance`, not server/hostname config text. Missing/unreadable identity fails closed; no generated fallback or healing.

Validate installation, operation ID/kind, destination stanza, phase-specific old/new stanza transition, actual registration/name/port/deterministic data directory, and relevant ownership/generation/awaiting/restore/aside facts before continuation, advance, routing or retirement. Record fields alone are expectations, not proof. Fence insertion cannot overwrite another operation; updates/clear compare expected complete identity inside records locking.

Pending targets cannot be removed/reidentified through ordinary config, dropped/recreated into old authority, or newly created over an outstanding operation. Owner cleanup retires only matching verified artifacts/config after successful cleanup. Preserve evidence on failure. Rebuild pruning retires lost-host local activation authority while retaining extension history and fresh awaiting state; metadata neither authorizes lost-file adoption nor blocks legitimate new recovery.

## Runtime access fence and finalizer

Before swap or retry start of swapped data, after durable prepared: withdraw target and invalid restore-inspection routes from both running poolers, cut existing clients/backend routes, reload and verify exclusion. Transaction PAUSE alone is insufficient. Do not reuse standby-only fencing that preliminarily RESUMEs or rolls back to exposed routing on error. All public node SQL checks fences regardless of protection, before opening a connection; internal validated settlement remains separate.

After safety completes durably as routing: verify identity, render/reload both poolers with correct aliases/backend mapping, verify actual running state, release/verify transition pause, then clear exact fence. Never clear because arbitrary scoped engine apply succeeded. Routing failure retains routing and ordinary apply explicitly retries finalization. After restore record retirement, retry hint must name apply, not only promote.

## Guard provenance and settings

Compose every winning preload assignment from current desired, actual retained requirements, and current underlying operator config excluding the generated guard. Preserve underlying plain-auto provenance/assignment beneath the winning block. Prior guard entries are not independent retained evidence; deliberate operator removal must take effect unless a library remains required. Malformed guards fail.

Before every copied first/retry start: compose final union, bind each library to explicit compatible resolved copy/standby metadata, reject unknown/ambiguous safety, verify complete package/files, install guard, start and verify effective owned endpoint. Unknown override/plain-auto/prior-guard libraries cannot load first and be inspected later; SQL catalog cannot detect preload-only workers.

Settings track descriptor provenance: own mandatory copy settings overlay own ordinary settings (including Timescale 2→0); conflicting effective cross-descriptor requirements refuse. Inherited ordinary destination settings never beat mandatory suppression. Replace blanket includeCopy=false with per-key handoff: pending and historical-only obligations remain until verified equivalent permanent configuration; independent persisted settings can release guard after equivalence. Do not reconstruct hidden suppression after successful handoff against a deliberate later supported change. Keep narrow handoff/provenance with the guard, not a new transaction subsystem.

## Required evidence

- Fail fork at credentials-complete/adopting/adopted/settings/config/start/SQL boundaries; supported non-secret retry completes through real engine seams without credential replay.
- Normal/resumed fork actual render and runtime routing verification publish both aliases before exact fence retirement; routing failure retry remains possible.
- Restore combined retirement/routing update failure cannot orphan continuation.
- Existing session clients and unprotected SQL cannot reach swapped/unsettled data.
- New stanza/registration/op/instance/name reuse never acquires old authority; real bundle pruning permits new-host recovery without lost-artifact ownership.
- Own suppression overlay succeeds, cross-descriptor conflicts refuse, historical-only suppression survives later apply, deliberate operator preload removal works, unknown final union refuses before first start.

Live PostgreSQL/PgBouncer/process-death qualification remains separate; do not stub the exact continuation/render boundaries whose behavior is under review.

## Existing follower runtime refresh

Use one read-only follower-runtime planner for preflight, guard refresh and effective verification. Current inherited descriptors use candidate source definitions and the actual database name inside the follower. Retained runtime requirements come from actual installed SQL and retained guard evidence resolved through snapshots, not every historical source-stanza variant. Current selected ordinary settings supersede their previous ordinary runtime values; historical variants still govern historical restores. Unknown/ambiguous required data facts refuse. Planned primary DROP must not preserve a removed library forever, nor authorize an unsafe unload without sufficient evidence.

Validate metadata independently of files/preloads: every selected descriptor, including SQL-only extensions, requires compatible copy safety/mandatory settings and standby certification. Invoke before history/save/package/DROP in direct commands, config set, plain apply and follower-scoped early-return paths. Awaiting followers validate candidate metadata without fabricated database facts. File installation/verification follows authorization.

Follower configuration recomposes and installs the complete winning guard; never remove it or hand off mandatory suppression. Config Check detects guard-only drift through the same read-only plan. Transition completion verifies all expected effective libraries/settings, including mandatory overrides, before primary SQL; pending restart remains unfinished work.

## Continuation envelope and permanent-proof lexer

Classify apply from configuration plus an activation-record snapshot before global locking. Any pending/routing continuation requires exclusive global locking; unreadable records never fall back to shared. Retain the actual acquisition decision. Reload config and records under lock and refuse changed requirements before effects, without lock upgrading. Hold exclusive serialization through whole-config persistence and route finalization.

Use one whole-line PostgreSQL configuration lexer for proof/preloads/relevant guard parsing: ASCII-fold names, optional equals, complete decoded single-quoted values with doubled quotes/PG escapes, comments outside quotes only, and full trailing-token validation. Preserve value case/quoted whitespace. Decode once: actual decoded data must not be quote-stripped again into a false equality. Includes consume decoded values/comments at their actual position; unsupported syntax/topology fails unproven. Real rendered escaped values, uppercase operator overrides, quoted hashes, commented include_dir and fresh pending operations require adversarial tests.

## Deferred plain apply: authoritative boundary

The approved plan preserves ordinary config/apply's deferred-restart model. Earlier agent advice/tests demanding every restart token before any plain-apply mutation were overbroad; this correction does not change user-approved direction.

- Direct ext add/remove still require the complete exact authorization set before every mutation, including history/package/config/SQL effects.
- Independent plain apply safely stages configuration and reports pending work without a restart token, withholding dependent SQL until required libraries/settings are effective.
- Source/follower plain apply may stage safe propagated follower configuration/guards and package prerequisites without all tokens. Only authorized targets restart. Withhold primary configuration/reload/runtime work and SQL if it would cross unmet follower prerequisites; simply deferring primary SQL is insufficient. A bounded implementation may stage followers and return pending before source work until follower readiness is proven.
- Missing tokens alone are not proof staging is unsafe. Specific incompatible/unprovable/floor or immediate runtime hazards may refuse. Explicit db/step selection cannot bypass graph dependencies.
- Required settings readiness matters, not only preload presence; unrelated pending settings are not a universal SQL blocker. Effective reload-only prerequisites do not need synthetic restart authorization.

Tests distinguish direct-command missing-each-token all-effects-zero from plain-apply safe pending staging, authorized follower-first retry, primary-effect withholding and settings-only readiness. Multi-source metadata/lifecycle/safety preflight errors remain before effects, but a missing plain-apply restart token is deferred work rather than a blanket mutation prohibition.
