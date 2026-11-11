# Agent notes: historical extension integration

Read-only oracle advice against the validated Phase 6 base on 2026-11-10. This is implementation guidance within the approved handoff, not a new approval or completed-code claim.

## Durable model

Use `Records.ExtensionRequirements`, keyed by immutable primary stanza, with observed UTC coverage start and deduplicated self-contained resolved descriptor snapshots. Include source/package, canonical/SQL identity, preload/settings/schema, copy safety/settings and standby safety. Keep substitution semantics and provider identity. Append-only retention is acceptable. A present known-empty record differs from absent history. Operator declarations do not backdate observed coverage.

Use existing `UpdateRecords` locking/atomic write/dirty marking and include the whole section in durableHash. Keep callbacks short; no lifecycle locks or SQL/packages within records callbacks. Derived independent stanzas conservatively retain selected source requirements, but begin their own observed coverage at their actual lifecycle start.

## Before-effect capture

- Config edit: resolve outgoing descriptors before replacement; preflight affected sources/followers; retain old/proposed requirements before save. Apply-only capture loses overwritten metadata.
- Add: full authorization, retain requirements/intent, packages, followers first, primary, primary SQL, verify.
- Remove: dependency preview/authorization, retain old requirements before DROP while loaded, DROP, reduced intent save, primary then followers.
- Drop/stanza replacement: retain outgoing stanza requirements before discarding identity.
- Backups/dumps: required capture before producer, not best-effort success recording. Include `node.jobBackup`, `steps.setupStanza` first full, bucket/final dump and local dump producers.
- Materialise: coverage/declaration resolution, compatible union/safety/prerequisite checks, retain metadata, install/verify, copy/configure, first start. Cover resumed/adopted starts too.

Capture failure stops the dependent action. Extra retained snapshots after a later failure are safe conservative retention and must not be rolled back. Revalidate current membership/stanza/generation/pending operations/tokens under existing global → heavy-I/O where needed → sorted cluster locking, without nested reacquisition.

## Historical completeness

RecoveryExtensions has absent, explicit empty, and populated states. Declarations supplement unknown knowledge; they never erase retained requirements. Checking only PITR target time is insufficient: the selected base backup/replay history must be covered. If unprovable, require the declaration, including latest. Current physical basebackup may establish only current-copy coverage. Awaiting current intent cannot certify historical completeness.

Existing dump metadata is database/name/time, not immutable stanza-bound. Never attach an old same-named dump to today's stanza by inference. Treat an unbound dump as unknown historical input through the already-approved declaration; no new selection UX needed.

## Common first-start plan

Produce a validated runtime plan in node orchestration: packages/control prerequisites, compatible retained preload union, ordinary/copy-safe settings, cron metadata database mapping, stanza and coverage/declaration evidence. Pass it explicitly to materialisation. Cover Restore, BaseBackup, Initdb, StandbyBaseBackup and adopted/resumed first starts. Refuse incompatible provider/settings, unknown copy/standby safety or missing files/repository before start. No CREATE on standby.

Reconcile desired missing SQL extensions and report extras after promotion before activation. Retained libraries may still be needed by restored data after removal from current intent. Carry them through adoption/convergence until a validated safe handoff; this does not mean CREATE every historical SQL extension.

## Durable copy suppression

Do not rely only on `99-materialise.conf`: Adopt removes managed conf files; Swap keeps destination live config; promotion can launch cron before post-promotion SQL; SettlePromoted restarts; unfollow retries can restart before rename; drills/historical forks may promote inside materialisation.

A copy-owned guard must survive those transitions and have winning PostgreSQL configuration precedence, including copied auto.conf. A narrowly managed guard in copied auto.conf fits existing recovery-file handling if it preserves recovery settings. Install after copy output and before first start. Carry retained preloads/settings, cron launch suppression and custom disabling settings through all starts. Verify effective settings, not merely file presence.

Independent fork/unfollow copies persist disabling settings in explicit destination settings before independent start. Remove the temporary guard only after permanent configuration supplies equivalent safety; never leave hidden precedence preventing deliberate later supported operator changes.

## Cron settlement

While launch suppression remains effective: inspect actual configured metadata database; deactivate all copied active jobs; perform owned-copy rename with bounded session termination; retarget only job database values matching the old application name; update cron.database_name if that metadata database was renamed; finish privileges/reconciliation/restarts; verify before routing/suppression release. Never touch source jobs or expose command payloads. SQL must be idempotent across partial rename/retry.

Use existing checkpoints: unfollow Settled includes cron safety; failed restore safety settlement must not unconditionally resume routing; generation/ownership/awaiting Adopted/aside identities remain authoritative. Establish suppression before any retry branch that starts a stopped copy.

## Bundles/pruning

Existing records/config bundle files suffice. PruneForRestore preserves history, including dropped/replaced stanzas. Do not remove it in name-based forgetDropped cleanup or reset coverage on rebuild. No automatic pruning optimization: dumps can outlive stanza windows. Missing older metadata remains unknown.

## Acceptance trace

Trace every first start and routing release through copy, adoption, swap, recovery-setting cleanup, rename, apply and retry. Tests must prove retained requirements and winning suppression survive each boundary. File/process-death coverage remains separate from model-based unit evidence. No demonstrated material blocker was found; unsafe/unprovable combinations use approved refusals, not provider arbitration or repair commands.

## Resolved implementation choices (2026-11-11)

Oracle confirmed the following are refinements within the approved contract, not new user-facing decisions:

- Require presence-aware recovery declarations for unbound dumps; no new Dump.Stanza model needed. Keep conservative since-only requirement snapshots; incompatible known historical variants refuse rather than selecting providers or erasing history.
- Mandatory copy-disabling settings override ordinary inherited destination settings with an explicit warning. Persist them in independent destinations until deliberate operator change. Conflicting mandatory requirements from different descriptors refuse; a hidden guard must not perpetually override supported changes after settlement.
- Require standby safety for every physical copy's recovery/standby start, even with immediate promotion. Logical dump initdb requires copy safety but not standby certification solely because it is a restore.
- Missing desired SQL extensions must reconcile before activation, after writable promotion while suppression remains effective. No CREATE on standby, no automatic SQL version upgrades, no historical-all CREATE, no dropping extras. Unsafe unmanaged extensions block activation. Failure retains ownership/suppression and reports the incomplete stage without false rollback or unconditional routing resume.
- Primary-SQL deferral alone does not satisfy follower-first ordering. Use one extension transition planner and caller-locked ordered executor shared by direct mutation and config/apply. Use a global-exclusive envelope before sorted target locks for source-affecting scoped work; no lock upgrade/nested dispatch. Revalidate full membership/identity/authorizations under lock.
- Add order remains history/consistent intent → packages → follower configuration/restart/effective verification → primary configuration/restart → primary SQL/verify. Remove remains preview/authorization/history → primary DROP while loaded → reduced intent → primary configuration/restart → followers. Explicit db/step selectors cannot bypass dependencies; execute the safe transition or refuse. Ordinary config removal cannot unload libraries before safe SQL removal; require the supported remove path where appropriate.
- Retained cron without descriptor database_name must not invent/pin a database. Start only with verified suppression, then inspect actual effective cron.database_name inside the owned copy and operate on that database. Unknown facts fail, never imply zero jobs. Retarget only matching old application names; never touch source jobs.

Required adversarial regressions include reversed cluster order, multiple/stopped followers, missing each token, stale membership, selective apply bypass, failed follower/source stages, declaration/base-backup coverage, winning copy guard across every first/retry start, desired-SQL failure withholding routing, different cron metadata database, and bundle/name-reuse retention.

## Integrated review correction architecture

Two initial E7-05 lanes exposed lifecycle defects despite passing models. Oracle confirmed the root correction remains within approved scope:

- Every winning guard preload assignment is the complete required current/retained/applicable-operator union, never a list of additions. Distinguish retained requirements from obsolete desired libraries when recomposing; malformed existing guards fail.
- Merge/validate retained ordinary settings as runtime requirements, not only packages/libraries/copy suppression. Preserve compatible historical-only settings through adoption/settlement/later apply; known conflicts refuse before start.
- Guard verification takes an explicit owned-copy endpoint before swap, destination endpoint afterward. Mandatory postmaster suppression must become effective through a safe owned restart/recheck before promotion; pending is not success.
- Persist a small activation fence with instance, immutable stanza/recorded transitions, operation and existing ownership evidence before publication/exposure. Both poolers and convergence honor it. Existing restore-promote/unfollow retries and adopted-fork scoped apply perform real validated continuation; no credentials in records or ordinary apply replay.
- Activation release order is durable pending identity → routing exclusion → guarded lifecycle → cron/rename/privileges/desired SQL → permanent config/effective checks → durable completion → route reload → client resume. Irreversible failures never imply rollback or unconditional resume.
- Record inspected cron metadata database identity and allowed rename mapping before irreversible rename. Retry resolves current name by owned-catalog identity rather than stale SHOW name or guesses.
- Refresh config/records after global lock acquisition. If the current envelope requirement differs, refuse before effects rather than lock-upgrading or acting on stale membership. Removal retry considers every current follower's unfinished work, not only primary SQL/intent.
- Preserve logical dump mode during promotion; copy safety applies, not physical standby certification.

Required regressions use actual temporary guard files across adopt/swap/settlement, durable activation reload and general routing/apply attempts, pending-postmaster worker suppression, cron rename interruptions, current token/lockset refresh and follower-only removal retry. They remain distinct from live process-death/GUC/PgBouncer qualification.
