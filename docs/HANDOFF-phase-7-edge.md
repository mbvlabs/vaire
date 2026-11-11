# Orchestrator execution spec: Phase 7 extensions on edge

**Execution entry point · 2026-11-10 · planning complete, implementation not performed**

**Start the orchestrator with `/home/mbv/work/vaire/docs/HANDOFF-phase-7-edge.md`.** The approved high-level plan is context, not the execution entry point.

## 1. Mandate, approval and boundaries

Implement the direction approved without annotations in Plannotator for [PLAN-phase-7-edge.md](PLAN-phase-7-edge.md), approved-text SHA-256 `9c212ad24f101f4f4a89334fbee0fe045b1ab2d3b29bc70fd2941fc2700d18f7`. Revalidate its digest in E7-01. The first review was annotated, not approved; the second review was approved. The decisive corrections incorporated into the second version are:

- No backward-compatibility requirement before v1; no legacy extension shims/migrations just for compatibility. Preserve current security, ownership, data-safety and regression guarantees.
- The catalogue is not an allowlist. Support any compatible PostgreSQL 18 extension through an explicit APT package from an already configured trusted repository, or an operator-preinstalled extension. The user confirmed **packages are enough**; no source-build automation.
- Do not impose a PGDG-only or Apache-only restriction. PGDG defaults are useful; alternate providers require explicit selection. Do not automatically configure repositories/keys.
- Build and publish Phase 7 before follower live qualification; later qualify both features on the same edge candidate. Activity qualification or explicit acceptance of its gaps still gates deployment. This supersedes the earlier handoff's requirement to finish follower live tests before starting extension work.

The planner has made no source/configuration changes and has not run builds, tests, installs, live checks or deployments. This spec is separate from the approved plan and requires no conversation context.

**Authorization:** plan approval authorizes direction, not Git or live operations. Before implementation, confirm the orchestrator's implementation authorization and working location. Ask before branch/worktree creation; suggested name `feat/phase-7-extensions`, base current validated `master`. No commits, push, PR creation/merge, tags, installed-binary replacement, deployment, restart, rebuild, bucket switch, fixture destruction or cleanup of preserved work without the corresponding explicit permission. Do not interpret the user's desired edge outcome as permission for every intermediate operation. Existing scoped authorization, if supplied to the orchestrator, should be recorded rather than redundantly requested.

## 2. Repository and observed checkpoint

| Resource | Observation during planning |
| --- | --- |
| Repository/current worktree | `/home/mbv/work/vaire` |
| Branch/base/HEAD | `master`, `0d8e1fcc493cd30ca4afb89b54d6bd050302f552` |
| Remote | `https://github.com/mbvlabs/vaire.git` |
| Remote branches, freshly queried | `master` at that SHA; `1.x` at `ac742025e5e9c5a015e6217657bcb2c0d2b29b19` |
| Registered worktrees | Main checkout only |
| Open PRs, freshly queried | None |
| Preserved snapshot | `/home/mbv/work/vaire/.claude/preserved/2026-11-10/phase-7-extensions` |
| Snapshot Git provenance | Old/broken Git pointer; not a healthy branch/worktree or verified commit diff |
| Historical follower publication | PR #14; edge workflow `42854898441`; `edge-0d8e1fcc493c`; documented signed Linux/amd64 artifact verification; no deployment or live qualification |

Main had no tracked modifications. Untracked documents to preserve and exclude from implicit staging: `docs/CLEANUP-branches-worktrees-2026-11-10.md`, `docs/HANDOFF-followers-final-gate.md`, `docs/HANDOFF-planner-handoff-workflow.md`, `docs/PLAN-planner-handoff-workflow.md`, `docs/PUBLICATION-phase-6-edge.md`, `docs/STATE-OF-THE-UNION-2026-11-10.md`, plus this task's plan and handoff. Revalidate mutable state before acting. Do not reset, overwrite, prune or repair anything to match this table.

## 3. Required reading and truth precedence

1. Repository `AGENTS.md` and applicable parent instructions.
2. Approved `docs/PLAN-phase-7-edge.md` and this execution spec.
3. `docs/MASTER.md`, `docs/IMPLEMENTATION.md`, `docs/PHASE-4-WORK-UNITS.md`, `docs/PHASE-5-WORK-UNITS.md` for extension-dependent maintenance and recovery commitments.
4. `docs/PHASE-6-WORK-UNITS.md`, `docs/REVIEW-phase-6-final.md`, `docs/FINDINGS-phase-6-review.md`, `docs/RUN-phase-6-live.md`, `docs/PUBLICATION-phase-6-edge.md`; preserve their current contract, not obsolete opening status text.
5. `docs/MANUAL-TESTING.md`, `docs/ACTIVITY.md`, `docs/PLAN-v1-release.md`, `docs/RELEASE-v1.0.0-beta.1.md`.
6. Preserved `docs/PHASE-7-WORK-UNITS.md` and relevant source/tests as historical implementation references only.

Current code/tests establish what exists; historical reports establish only the recorded checkpoint/outcome. Older preserved decisions were not reviewed and are not blanket approval. This spec supersedes preserved catalogue-only validation, missing follower integration, approximate `--exact`, old project naming and pre-security/activity infrastructure. Do not replace current documents wholesale with snapshot documents. A material new choice returns to the user through Plannotator; a concrete implementation refinement within this contract does not need another high-level approval.

## 4. Concrete implementation contract

### 4.1 Catalogue and custom definitions

Use one config-aware resolver throughout configuration, rendering, roles, operations, doctor and data paths. Do not leave a second static allowlist in validation or silently skip unknown enabled entries.

Keep `ClusterSpec.Extensions` as canonical names. Add top-level `extensions` definitions to `config.Cluster`, keyed by canonical name, and document them. Each custom definition carries:

- `ext_name` (defaults to key), `source` (`apt` or `preinstalled`), `package` (required for `apt`, absent for `preinstalled`), `preload` (optional library name), `settings` (string map), optional `schema` and human note.
- `copy_safety`: `unknown` (default), `safe` (operator asserts no unsafe copied/background effects), or `disabled` (requires nonempty `copy_settings` that suppress background effects).
- `standby_safe`: explicit boolean for custom definitions; absent/false blocks follow with an actionable prerequisite. These assertions describe trusted native code; they are not a security sandbox or automatic compatibility proof.
- No SQL hooks, arbitrary grant snippets, shell commands, Git URLs, scripts, repository credentials or keys in descriptors. Quote all SQL identifiers/literals; pass APT/command arguments as argv, never through untrusted shell interpolation. Validate package tokens against Debian package-name syntax, library/schema identifiers and setting keys/values against the existing renderer's safety rules. Disallow descriptors from overriding reserved managed settings or disabling security policy through extension metadata.

Built-in entries supply their known runtime/safety metadata. A root definition with the same key is an **explicit provider override**, not an implicit fallback: resolve built-in defaults plus supplied installation fields, but require renewed custom safety declarations if package/provider changes. Retain immutable default-module behavior for `auto_explain` and default preload handling for `pg_stat_statements`; provider overrides must not bypass those rules. Reject duplicate SQL extension names selected within one database and conflicting providers/library settings across databases on the same host. Detect descriptor edits affecting enabled sources/followers in plan/apply; they are runtime intent changes, not just catalogue text changes.

Unknown names can be registered and enabled with direct `ext add` options: `--package <name>` or `--preinstalled` (mutually exclusive), optional `--preload`, repeated `--setting key=value`, `--schema`, `--copy-safety`, repeated `--copy-setting key=value`, and `--standby-safe`. Existing custom definitions can instead be edited through supported config editing and selected by name. CLI aliases canonicalize before persistence. Direct options become durable definitions only after full validation/authorization. Do not silently install arbitrary similarly named packages or guess preload requirements.

Built-in list: current contrib modules plus `vector` (CLI alias `pgvector`, package `postgresql-18-pgvector`), `postgis` (`postgresql-18-postgis-3`), `pg_cron` (`postgresql-18-cron`), `pgaudit` (`postgresql-18-pgaudit`), `pg_repack` (`postgresql-18-repack`), `pg_partman` (`postgresql-18-partman`, no optional background worker), `hypopg` (`postgresql-18-hypopg`), and default `timescaledb` (`postgresql-18-timescaledb`). Describe this default's edition limitations without forbidding explicitly chosen alternatives. No automated repository/key installation or source build.

### 4.2 Installation, configuration and diagnostics

Install/hold the resolved package union server-wide before any cluster starts with a required library; never uninstall on remove/drop. Empty package union must not change existing step input hashes. Do not bump all step IDs or force unrelated steps to rerun. Package side effects are not transactional: failure can leave installed packages; never say “nothing changed” without proof.

Verify a package-backed extension's PostgreSQL 18 control/library availability after installation. Preinstalled mode performs no install and reports missing files explicitly. Use real control/catalog facts rather than package-name inference as a compatibility check; actual load/runtime compatibility remains a live qualification obligation. APT signed-repository trust stays with system configuration; unknown repositories are prerequisites, not an offer to import keys. On rebuild, custom repositories and preinstalled files may need operator preparation before recover; report this precisely and preserve awaiting state.

`ext list [--db]` merges built-ins/custom definitions, package/control facts and per-database catalog facts; show enabled, installed version/default version, update availability, provider, preload/restart and unused retained packages. Report inaccessible/stopped/awaiting database facts as unavailable, not absent or version zero. List unmanaged installed extensions separately, excluding `plpgsql`. Doctor adds package/state/update checks and communicates unknown facts and pending restart independently.

Create extensions as `postgres`, optional schema first, built-in minimum grants only to application owner roles. Do not grant custom extension functions or schema write access to `vaire_admin` on protected databases. Check actual effective preloads: skip preload-dependent creation while restart is pending, emit `extension_pending_restart`, and leave the roles step incomplete so retry creates it. Default modules retain existing behavior. No automatic package upgrades. Re-add may execute an explicitly reported supported SQL `ALTER EXTENSION UPDATE` path to installed default; differing versions alone do not prove an upgrade path or ordering. No automatic downgrade.

### 4.3 Mutation, followers, locking and failures

`ext add/remove` target a named independent primary; infer `--db` only with exactly one eligible database, otherwise structured usage error. Refuse direct follower mutation, awaiting-recovery mutation, open-restore conflicts and unfinished unsafe unfollow states before effects. Protected add is allowed as privileged configuration, without a new TOTP gate, but cannot weaken existing grants or secretly schedule jobs. Protected remove refuses. Removal requires `<db>`; a preload change additionally requires every affected existing cluster's `<name>/restart`. `--yes` and a source token never authorize follower restarts. Direct extension commands do not accept broad server restart authorization as a substitute for the enumerated affected-cluster tokens. Dry run needs no mutation tokens and performs no persistence/package/restart/SQL effects.

Use repeatable `--confirm`, existing RPC `Confirm`/`Confirms`, node method-token validation and engine confirmation structures. Discover/preflight the complete source/follower target set and required tokens before mutation; reload/revalidate under lock. Lock order stays global → heavy-I/O when needed → sorted cluster locks. Never recursively reacquire flock via nested RPC/engine helpers. A source mutation includes all its followers' desired extension metadata; direct follower edits of inherited extension intent are refused. Plain config/apply must use the same propagation checks, not bypass them.

Add order: preflight/authorize all targets → persist consistent desired intent and required recovery metadata → install packages → configure/restart required followers first → configure/restart primary → primary CREATE/UPDATE SQL → verify. Never CREATE on standby. Remove order: full dependency/owned-data preview and authorization → primary DROP while library loaded → persist reduced intent for source/followers → apply primary then followers. Packages and recovery requirements remain. Library-setting conflicts or required standby-floor changes that this phase cannot safely order are refused; do not change the tuning formula or automatically raise persistent floors to hide them.

Reuse current restart pause/resume machinery. Restart only the enumerated clusters; unrelated clusters remain untouched. Reads/current checks can fail; do not fabricate success. Missing authorization has zero mutations. Failure after desired-state persistence reports exact completed/pending targets and supported retry, not a false rollback. When desired-state rollback is genuinely safe before runtime effects, check its save result and surface rollback failure. Remove must be retryable after DROP succeeded but saving/reconfiguring failed: enabled-but-SQL-absent is a reconciliation state, not a terminal refusal. Package install success with later failure never triggers uninstall. Re-add must finish already-enabled pending work, not treat intent equality as completion. Preserve current generation/ownership and ABA defenses; do not invent healing commands.

### 4.4 Copies, historical requirements and scheduled jobs

Before materialisation, resolve required packages/preloads/safety settings from current intent **and retained historical requirements**. Retaining installed packages alone is insufficient after rebuild or removal from current preload intent.

Add a small durable `extension_requirements` section to `backup.Records`, keyed by immutable primary stanza, containing deduplicated resolved descriptor snapshots previously needed by that stanza. Record requirements before extension intent/runtime removal/change and before extension-bearing backup/materialisation; include this section in `durableHash` and master-bundle preservation/pruning. Keep it with dropped/replaced stanzas for their recovery lifetime. Requirements are metadata, not credentials. Use existing serialized records update discipline, not a new database or broad transaction framework. Conservative retention is acceptable; no automatic pruning optimization in this phase. Record the UTC start of observed coverage; discovering today's extension list does not prove completeness for older restore points.

Materialisation uses a compatible union of retained package/preload requirements with safe copy settings. If providers/settings are incompatible, requirements are unknown, repositories/preinstalled files are missing, or custom copy/standby safety is undeclared, fail before starting the unsafe copy and name the prerequisite. Never silently discard requirements or load all catalogue libraries as a guess. For older/preexisting backups without sufficient metadata, disclose the gap and require explicit operator-supplied compatible definition/requirements through supported configuration: add `ClusterSpec.RecoveryExtensions` (`recovery_extensions`, canonical definition-name list) as the operator's explicit complete requirement declaration for those otherwise unknown restore points. Resolve that list through the same definitions and apply the same safety/conflict checks; persist it in bundles and propagate it through source-derived copies. Absence is not an assertion that an unknown historical point used no extensions; an explicitly empty list must remain distinguishable from absence. No automatic legacy migration is required. If this requirement cannot be implemented safely without a new user-facing recovery design, stop and return that decision rather than ship historical recovery claims unsupported by code.

Temporary restore/drill/fork/recover/follow copies keep necessary libraries and disable `pg_cron` launches with `cron.launch_active_jobs=off`. Custom `copy_safety=disabled` applies declared `copy_settings` before first start. For fork/unfollow independent copies, persist those disabling settings in the new database's explicit settings until the operator deliberately changes them; do not guess scheduler-specific SQL. Built-in cron additionally deactivates copied jobs and retargets database names before user routing/background execution is enabled, with counts/warning. Guard the promotion-to-job-deactivation gap: retain launch suppression throughout promotion, rename and privilege settlement. A bare temporary-config override that disappears before deactivation is insufficient.

Rename terminates only sessions in the owned copy with bounded waits, preserving existing failure/ownership rules. Source jobs remain untouched. Restore/promote/recover reconcile desired extensions missing in restored data, report extra restored extensions, and never drop user data merely to match intent. Unknown unsafe unmanaged extensions block the relevant activation rather than being silently certified. Recovery metadata survives rebuild, drop/recreate and retries without attaching to a replacement database by name alone. Preserve follower retained-slot, generation, awaiting, protection settlement and source-first recovery guarantees.

### 4.5 Maintenance

Manual repack remains a detachable `maint` job. Require existing target confirmation, extension/client/server SQL-version match, supported ordinary table with primary key or suitable non-null unique index, window or `--now`, and verified free space at least table+indexes plus the existing host reserve. Missing extension is reported before asking for a destructive token. Preserve safe argv quoting, explicit PG18 executable/socket/port/database/user, `--no-kill-backend`, bounded lock wait, heavy-I/O locking, per-database locking and detach/result/activity semantics. Never log credentials. Restrict selection to supported ordinary tables; report unsupported partition parents, foreign tables and special extension storage rather than assuming compatibility.

Protected repack must not run as unrestricted `postgres`. Implement the current protected-maintenance principle: a verified limited-privilege owner path if pg_repack can satisfy it without widening access; otherwise **refuse protected repack** with an explicit supported limitation. Do not add a new TOTP bypass path or grant broad permanent access. The refusal is the bounded fallback within the approved “preserve protected rules” direction. Apply the same rule inside jobs and weekly automation, not only CLI preflight. Ordinary vacuum/analyze/reindex behavior is unchanged.

Auto-repack is opt-in (`auto_repack=false` default): supported tables over configured estimated ratio and at least 64 MiB estimated bloat, biggest first, one at a time, nonblocking heavy-I/O acquisition, stop starting tables at window end; report busy/skipped/failures and accurate per-run success counts in maintenance state/metrics. Revalidate space/version/key/protection/window immediately before each table. Do not kill a running table operation at window end by unreviewed new cancellation behavior.

`maint report bloat --db <db> --table <table> --exact` requires installed `pgstattuple`, resolves exactly one supported ordinary table safely, and calls **`pgstattuple`**, not `pgstattuple_approx`. Use existing monitor permissions/timeouts and disclose full scan/read lock/concurrent-update semantics. Return measured tuple/dead/free bytes/percentages with measurement method; do not relabel them as exact reclaimable bloat. Default estimated table/index reports remain. No database-wide exact scan or exact index/columnstore guarantee in this phase.

## 5. Work items and delegation

Every brief below includes outcome, inputs/targets, steps, outputs and a done gate. Owners are roles, not new agent definitions. Use available worker/tester/reviewer equivalents; do not silently replace two independent reviews with one orchestrator read. All workers receive sections 1–4, the approved plan, their work item and current checkpoint. No worker commits or performs live/Git publication actions. One writer at a time in a shared worktree; parallelize read-only research/reviews only unless separate authorized worktrees have disjoint ownership.

### E7-01 — Freeze checkpoint, authorization and snapshot inventory

**Owner:** orchestrator with read-only scout. **Depends:** none. **Targets:** current/preserved paths above; new `docs/STATE-phase-7-preserved-work.md`.

1. Read instructions/source-of-truth docs. Run `git status --short`, `git branch -vv`, `git worktree list`, `git rev-parse HEAD master 1.x`, `git remote -v`, and `gh pr list --repo mbvlabs/vaire --state open --json number,title,baseRefName,headRefName,url`. Requery branches with `gh api repos/mbvlabs/vaire/branches --paginate --jq '.[] | {name: .name, sha: .commit.sha}'`. Do not fetch/reset/rebase silently.
2. Record approved plan digest, current SHAs, untracked work and permissions. Obtain implementation/location authorization and branch/worktree permission if needed. Do not use nonexistent historical feature branches as a base.
3. Inventory selected snapshot source/test/doc files read-only using `find`, `diff`/`diff --no-index`, `sha256sum`, `wc`, and current source comparisons. Exclude `.git`, secrets, caches, `bin`, credentials and build output; do not execute a preserved binary. No blanket recursive copy. The preservation command/original base is unknown; record that, never fabricate provenance.
4. Classify every useful Phase 7 delta as adapt/rewrite/reject, note current conflicts. Preserve the snapshot unchanged. Compare individual files as snapshots, not as a trustworthy historical branch diff.

**Outputs:** inventory with selected-path hashes/sizes, reuse classification, authorization/checkpoint record and affected-area ownership. **Verify/done:** all intended port inputs identified; unrelated source is excluded; unexplained concurrent changes block writes. Network failure leaves remote state unknown and blocks publication, not local investigation.

### E7-02 — Config-aware catalogue and custom package definitions

**Owner:** worker A; orchestrator owns design/merge. **Depends:** E7-01. **Targets:** `internal/catalog/catalog.go`, new catalogue tests; `internal/config/{cluster,clusters,fields,keys}.go` and tests as applicable; `internal/render/render.go`, extension template/tests; `internal/node/{restore,follower}.go` cloning/view integration.

Implement section 4.1 with typed definitions, deterministic config-aware resolution and deep copies of all nested slices/maps. Also implement section 4.4's `RecoveryExtensions` field with explicit presence tracking (such as a pointer to the name slice), preserving absent versus explicitly empty through strict TOML decode/encode, config editing, cloning/views and master-bundle round trips. Update validation, strict TOML decoding, field discovery/editing, canonicalization and unknown/duplicate errors. Resolver errors propagate; no unknown-entry skipping. Ensure routing/awaiting/follower views retain global definitions without sharing mutable nested metadata unexpectedly. Test roundtrip via cluster TOML, explicit provider override, custom APT/preinstalled definition, aliases, package/settings injection attempts, reserved-setting refusals, duplicate ext names/provider conflicts, missing custom safety, descriptor changes and copy isolation. Include separate absent, explicitly empty and populated `recovery_extensions` round-trip/edit tests; none may collapse into another state. Render stable preload ordering, default settings and per-database substitutions; do not depend on map iteration.

**Outputs:** resolver/config/render implementation and tests, documented descriptor example using a real package, touched-file summary. **Done:** built-in and custom paths use one resolver; invalid intent fails before writes; unchanged baseline hashes/rendering remain stable where behavior did not change. No legacy migration layer.

### E7-03 — Packages, role creation and effective-preload checks

**Owner:** worker A, serial after E7-02. **Depends:** E7-02. **Targets:** `internal/steps/{postgres_packages,postgres_roles}.go`, new/adapted `extensions.go`, `internal/steps/scripts/postgres-packages.sh`, step tests; call sites in `internal/node/{db,restore,recover,follower}.go`.

Adapt only relevant preserved package/extension SQL helpers to current vaire imports and paths. Install/hold resolved package union before config/scaffold starts; preinstalled verifies only. Include awaiting/historical requirements where needed rather than losing them through engine views. Preserve empty-union inputs and step IDs. Implement schema/grants, safe effective-preload wait and incomplete check/retry. Add injectable test seams around command/catalog facts, not production fault hooks. Test package union/hold argv, custom provider, absent repository/files, partial APT failure honesty, initial db creation, missing effective preload, post-confirm retry, protected grants and non-CREATE default module behavior.

**Outputs:** package/role foundation plus regressions and remaining host-only verification gaps. **Done:** all creation/recovery entry points install before load; no unintended privilege broadening; pending preload does not cause premature CREATE or false step completion.

### E7-04 — Extension commands, node mutations, diagnostics and activity

**Owner:** worker B, one writer. **Depends:** E7-03. **Targets:** new/adapted `internal/cli/ext.go`, `internal/node/extensions.go`, `internal/doctor/extensions.go`; `internal/cli/{tree,meta,prompts}.go`, `internal/node/{methods,dispatch,activity,doctor}.go`, relevant RPC output types/tests.

Implement sections 4.1–4.3 for standalone primary first, with integration hooks for E7-05. Register CLI metadata/read vs mutating/destructive/dry-run/prerequisites and repeatable tokens. Use current dispatch/version/protection/activity wrappers. Add list facts and doctor checks with explicit unknowns; bound dependent-object preview to first 50 with truncation/count and extension-owned tables. PostgreSQL DROP dependency enforcement remains authoritative when the preview misses a dependency; no automatic CASCADE retry. Updates must check supported SQL update paths and report actual resulting version, not assume the default was installed after any command.

Build regressions that record mutation callbacks and prove zero effects for missing tokens, protected remove, follower target, awaiting/open restore, invalid package/descriptor. Cover control parsing, down-cluster reads, unmanaged entries, aliases, cascades, absent SQL but enabled removal retry, package/save/rollback/apply failures and explicit update behavior. Test activity records method/target/outcome but no params/settings/SQL payload leakage; descriptor strings are not safe telemetry by default.

**Outputs:** command/RPC/doctor implementation and tests; current command-schema examples. **Done:** command metadata checks pass, standalone behavior is complete, every mutation failure has accurate supported retry and no stale snapshot behavior. Follower-enabled mutation remains gated until E7-05 is complete.

### E7-05 — Follower propagation, copies and historical recovery requirements

**Owner:** worker C, single writer; orchestrator reconciles shared node files. **Depends:** E7-03/E7-04. **Targets:** `internal/node/{extensions,follower,followerownership,db,restore,recover,awaiting}.go`, `internal/backup/{materialise,follower,state}.go`, new/adapted `internal/backup/extensions.go`, masterbackup/pruning integration, `internal/steps/{follower,restarter}.go`, lifecycle and adoption tests.

Implement the full source/follower target-set preflight, locks, sequencing and retry contract from 4.3. Reuse sorted locking/current generation checks; no nested lock reacquisition. Apply inherited extension intent through both direct command and config/apply paths. Preserve protected policy, hot-standby floors, retained-slot validation, owned recopy and source-first recover. Test missing *each* follower token, stale target membership, stopped follower, restart failure/retry, source failure after followers, direct follower config attempt and unknown facts.

Implement 4.4 requirements records and master-backup dirty/hash/pruning preservation, consuming E7-02's presence-aware `RecoveryExtensions` input. Record snapshots under existing serialized record updates before discarding runtime/desired requirements; make retries idempotent. Test records survive master restore and retain immutable stanza binding across name reuse/drop/recreate. Test an extension removed from current intent remains available to a historical copy after simulated rebuild; incompatible provider/history prerequisites fail before unsafe start. Test an unknown historical point refuses with the declaration absent, accepts an explicit empty declaration only as that operator assertion, and resolves/checks a populated declaration; prove the distinction survives the rebuild bundle and copied specs.

Adapt cron-safe temporary configuration and independent-copy deactivation/retargeting. Cover both backup-seeded and pg_basebackup follow paths; follow standby must not execute SQL installation. Test fork, dump restore, PITR, drill, recovery, promotion and unfollow interruption before/after promotion/rename/settlement, proving launch suppression persists until copied jobs are disabled. Custom disabled settings persist in independent copies; unknown safety refuses before start. Do not relax ownership to make a retry pass. Existing lifecycle tests model some faults; report real filesystem/process-death coverage separately.

**Outputs:** integration code, targeted regression index and limitations. **Done:** combined follower/extension behavior passes all regression cases and retained historical requirements are not merely a prose claim. Material new recovery/provider conflict decisions stop for Plannotator rather than being silently deferred.

### E7-06 — Repack, auto-repack and genuinely measured exact reports

**Owner:** worker D, single writer. **Depends:** E7-03, then rebase conceptually on E7-05's completed shared files without unauthorized Git history operations. **Targets:** `internal/maint/{run,bloat}.go`, new/adapted `repack.go` and tests; `internal/node/{maint,repack}.go`, `internal/cli/maint.go`, maintenance state/metrics tests.

Implement 4.5; do not port preserved approximate `--exact` or unrestricted protected repack. Trace actual socket/peer authentication and client path/version instead of copying command strings blindly. Add table resolution/key/version/space/window/protection tests, heavy-I/O/job-lock checks, safe argv for quoted table names and unsupported relation types. Verify no backend-kill option is enabled. Test no-table selection, scheduled ordering/minimum/busy/window behavior, per-run counts and failures. Exact report tests assert the full-scan function and measured field meanings, limited one-table scope and permission/timeout failures. Existing follower maintenance guards must still refuse writes/repack and handle read report source database names correctly.

**Outputs:** maintenance implementation/tests, protected fallback disclosure, runtime verification cases. **Done:** all safety checks occur both before submission and at execution as appropriate; no unattended protected superuser repack or bogus exact-bloat claim; default automation remains off.

### E7-07 — Canonical docs and combined live run sheet

**Owner:** orchestrator with tester/scout read-only input. **Depends:** draft alongside E7-02–06, finalize after them. **Targets:** new `docs/PHASE-7-WORK-UNITS.md`, `docs/RUN-phase-6-7-live.md`; reconcile `MASTER.md`, `IMPLEMENTATION.md`, `MANUAL-TESTING.md`, `PLAN-v1-release.md`, beta notes, README, embedded guide and `schema/vaire-v1.json` where actual output additions require it.

Document current behavior, user-supplied definitions/repository/preinstalled rebuild prerequisites, native-code trust, protection, explicit tokens, retry boundaries, historical requirements and exact-scan costs. Preserve old dated evidence. Mark Phase 7 built/live-pending only after implementation gate; Phase 6 remains live-pending. Correct the old “each phase live-tested before next merge” order to the approved combined candidate; do not alter stable/beta authorization rules.

Combined run sheet references every existing follower run-sheet case by stable case ID (do not delete blocked cases) and adds a mapping for extension coverage. One signed candidate, one live owner, serial activity/no-bucket/bucket passes. Per case supply prerequisite inventory, exact command shapes from implemented metadata, required permissions, expected observations, fixture names resolved at execution, time/WAL/disk budgets, stop conditions and cleanup. Unresolved templates are preparation, never executable commands. Required extension cases: built-in vector; a non-built-in definition using an actual available compatible extension package or a preinstalled extension not resolved through built-in metadata; missing repository/preinstalled prerequisite; add/remove dependencies/cascade; isolated preload restart; source+follower tokens/propagation; cron no-copy-side-effects on fork/drill/unfollow; partial failure/retry; extension history after removal and rebuild; manual/auto repack with bounded writes and no backend killing; measured exact report. If no safe real custom package fixture is available, mark it blocked and ask—testing only pgvector does not prove general extension support.

**Outputs:** reconciled docs and complete case map. **Done:** all approved behavior has test evidence or an explicit not-run/blocked live case; no historical pass is rewritten as fresh evidence. No live action occurs in this item.

### E7-08A / E7-08B — Independent cumulative source reviews

**Owner:** two independent reviewers, read-only. **Depends:** E7-02–07 complete and frozen diff. **Concurrency:** A/B and E7-09 can run together with no writer active.

**Common self-contained brief:** Review the orchestrator-provided exact branch SHA/base and uncommitted diff identity in the authorized worktree against `docs/PLAN-phase-7-edge.md` and this spec, current repository instructions and Phase 6 final review contracts. Audit the **entire integrated change**, not only preserved files or assigned emphasis. No edits, commits, server contact or secret reads. Return severity, file:line, actual failure scenario, violated contract, minimal root-cause correction and specific regression proposal; separate design questions, historical evidence and live coverage limits. Do not infer correctness from passing tests. Give a whole-change accept/block verdict.

**A emphasis:** custom resolver/package/provider trust, config validation/escaping, protected grants and RPC/refusal ordering, restart token isolation, locks/current membership, source/follower order, partial failures and re-add/remove retries, activity redaction, repack privileges/version/disk/locking.

**B emphasis:** materialise/follow/fork/restore/recover/unfollow lifecycle, generation/ownership/slot regressions, crash/retry boundaries, cron suppression through promotion, arbitrary-extension safety declarations, historical requirement records/master-bundle survival/name reuse, exact report semantics and docs-to-code agreement. Also independently inspect A's emphasis; no reading the other review before initial submission.

**Outputs:** two reports incorporated by orchestrator into `docs/REVIEW-phase-7-final.md`; findings ledger `docs/FINDINGS-phase-7-review.md`. **Done:** both whole-change verdicts returned and every finding dispositioned; missing reviewer is a stop/ask condition, not an implicit waiver.

### E7-09 — Fresh final-tree verification

**Owner:** tester, orchestrator validates evidence. **Depends:** implementation/docs freeze; rerun after every fix and immediately before every authorized commit. **Targets:** authorized worktree, never preserved binaries or the server.

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint
git diff --check
go test -count=1 ./...
```

For finding closure also run focused uncached packages, e.g. `go test -count=1 ./internal/catalog ./internal/config ./internal/render ./internal/steps ./internal/node ./internal/backup ./internal/maint ./internal/doctor ./internal/cli ./internal/masterbackup`. Record exact command, UTC times, tool versions, SHA+diff identity, exit code and output location. `just lint` includes command metadata, steps, shellcheck, actionlint and version-pin consistency; it is not optional. A failed chained command means later commands were not run. Tool/resource failure is blocked, not passed; no sudo installation/cache destruction without permission. Builds never authorize running the dev binary against contexts. Historical activity race/compiler gap remains separate.

**Outputs:** verification evidence in final review report. **Done:** full required chain, diff check and uncached suite pass at the final reviewed tree; otherwise gate remains blocked.

### E7-10 — Fix/review loop and edge publication

**Owner:** orchestrator; scoped workers/reviewers. **Depends:** E7-08A/B and E7-09.

Deduplicate findings without losing scenarios. Assign coherent minimal root-cause batches under the approved scope, one writer; changed behavior outside that scope returns to Plannotator. Require focused re-review by original lane(s), both lanes for cross-cutting lifecycle/security/locking changes, plus fresh E7-09. Stop after two failed corrections of the same unit and ask. Zero unresolved blocker/major findings; minor risks need explicit disposition, not silence.

When ready, obtain scoped commit/push/PR permissions and include exact files (never `git add .`), Conventional Commit subject, current gate evidence and pending live limitations. New feature targets `master`, not `1.x`. User merge permission is separate; no direct master push, force-push, cherry-pick or history rewrite. Recheck existing PR/remote base before creating one. Reconcile newer manual testing evidence line-by-line rather than overwriting it from a feature worktree.

After authorized merge, resolve actual master merge SHA and matching workflow:

```sh
gh run list --repo mbvlabs/vaire --branch master --limit 10 --json databaseId,headSha,status,conclusion,workflowName
```

Watch the actual matched ID with `gh run watch <resolved-id> --repo mbvlabs/vaire --exit-status` (resolve placeholders before execution/user handoff). Verify signed channel and immutable manifests/artifact using the current release-verification implementation/trust identity and cosign pin; record version/commit/channel/checksum and tested artifact platform. Workflow success alone is not installed identity. No manual channel-pointer manipulation on failure.

**Outputs:** reviewed source checkpoint, findings dispositions, commit/PR/merge/workflow/artifact evidence in `docs/PUBLICATION-phase-7-edge.md`. **Done:** either clearly ready awaiting named publication permission, or exact integrated candidate is proven published to edge. Neither outcome claims deployment or live pass.

### E7-11 — Combined qualification, separately authorized

**Owner:** one live orchestrator/tester; user supplies desktop root-key, sudo, TOTP and interactive actions. **Depends:** E7-10 signed candidate, E7-07 run sheet and separate named-context/deployment/test permissions. No local harnesses.

Revalidate actual CLI/channel/node/context identity, pending plan, doctor, database inventory/protection, headroom and recovery options read-only. Recorded contexts are `nobucket` on test VPS `46.224.129.96` (`db2.mbvlabs.com`, Ubuntu 26.04), and bucket context `mbvlabs` (R2 `vaire-backups-test`, prefix `mbvlabs`, previously no live server); do not assume those records remain true or interchange/repoint contexts. Do not read credential/token/key files; use approved existing interfaces or ask.

Activity gate remains the prior AC-01 qualification or explicit acceptance of named gaps. Obtain permission for any upgrade needed for its run. Then upgrade the operator to the **verified edge** candidate through normal signed distribution, apply node upgrade on the approved context and verify both identities. Explicitly select edge when leaving a stable-line installation; normal `vaire upgrade` on `1.x` does not imply edge. Do not install/upload locally built binaries.

Run serial no-bucket then separately approved bucket/rebuild passes, reusing bounded fixtures deliberately but not letting one feature obscure another's failure. All follower cases remain required/explicitly blocked as before, with extension additions mapped in E7-07. No unbounded WAL/load generators, `ALTER SYSTEM`, hand-edited generated configs, production fault hooks, timing-only crash proof or repair commands. Stop on unexpected data, disk, WAL, slot, restart, privilege or background-job effects. A code defect is fixed/reviewed/published/upgraded, not healed on the VPS.

Desktop bootstrap/rebuild requires the user's only root key, `~/.ssh/mbvlabs_prod_one`, and an actual runtime-verified complete command targeting `root@46.224.129.96`; provide one user step at a time, never unresolved placeholders. Bucket switch/rebuild/resource destruction needs its own scope. Linux/macOS cross-build is not macOS live qualification; one Ubuntu amd64 VPS does not prove Debian/arm64 package installs.

**Outputs:** dated per-case PASS/FAIL/INCONCLUSIVE/BLOCKED/NOT RUN in `docs/MANUAL-TESTING.md`, exact release/context/times/commands/job IDs, sanitized observations, cleanup/remaining fixtures and accepted gaps; raw non-secret runtime evidence in an owner-only `/tmp/opencode/` run directory, durable narrative only in repo docs. **Done:** all required combined cases accounted for and critical failures fixed/retested or readiness honestly blocked. Beta remains a later separately authorized release activity; no `1.x` merge/tag in this work item.

## 6. External evidence and known limits

Planning research inspected PGDG `Packages.gz` indexes at these URLs for all eight packaged built-ins, including exact `postgresql-18-timescaledb`:

- `https://apt.postgresql.org/pub/repos/apt/dists/trixie-pgdg/main/binary-amd64/Packages.gz`
- `https://apt.postgresql.org/pub/repos/apt/dists/trixie-pgdg/main/binary-arm64/Packages.gz`
- `https://apt.postgresql.org/pub/repos/apt/dists/resolute-pgdg/main/binary-amd64/Packages.gz`
- `https://apt.postgresql.org/pub/repos/apt/dists/resolute-pgdg/main/binary-arm64/Packages.gz`

All names were present. Timescale highest indexed versions were `2.31.0+dfsg-1.pgdg13+1` and `2.31.0+dfsg-1.pgdg26.04+1`; package description says Apache-licensed. This is metadata evidence, not install/dependency/runtime verification; recheck before relying on mutable versions. Do not pin these observations as required package versions. Alternate official Timescale packages are allowed through custom provider selection, not automatic repository provisioning.

Primary references: `https://reorg.github.io/pg_repack/` (version match, owner/superuser requirements, key/disk/locking and backend-kill behavior); `https://www.postgresql.org/docs/18/pgstattuple.html` (scan/permissions/measurement limits); `https://www.tigerdata.com/docs/get-started/choose-your-path/timescaledb-editions` (edition differences). Source-level support does not prove vaire's current peer/auth/protection path. Do not label all Timescale storage variants repack-compatible from a historical upstream fix; special storage remains explicitly outside ordinary-table repack coverage.

Preserved useful starting points include catalogue/config/render/steps tests, `internal/node/extensions{,_test}.go`, `internal/backup/extensions{,_test}.go`, `internal/maint/repack{,_test}.go`, node repack, doctor extensions and `docs/PHASE-7-WORK-UNITS.md`. Their imports, managed paths, metrics names, guide text and binaries use `eru`; adapt only intentional feature pieces to `github.com/mbvlabs/vaire` and vaire paths. Never run global replacement over current code or import old security/follower files wholesale.

## 7. Sequencing, stop gates and final report

```text
E7-01 → E7-02 → E7-03 → E7-04 → E7-05 → E7-06
              E7-07 drafts alongside, finalizes after integration
                          ↓
                E7-08A || E7-08B || E7-09
                          ↓
               E7-10 fixes / re-review / re-test
                          ↓
                authorized PR / edge publication
                          ↓
             E7-11 activity gate → no-bucket → bucket
```

Read-only independent research may overlap implementation only when it does not depend on changing source positions. Freeze writes for whole-diff reviews and final tests. Orchestrator owns canonical reports/docs and Git actions. Workers receive a file ownership list before editing shared node/CLI/config code. Do not concurrently install/change the shared operator binary or contexts.

Stop/ask for changed approved direction; inability to express safe custom or historical requirements; package/provider conflicts requiring replacement; unknown repository/credential prerequisite; unavailable independent reviewer; unexplained concurrent edits; unsafe/ineffective live fault design; two failed corrections; or missing Git/live/destructive permission. Do not waive safeguards to pass a test.

Final report lists completed/blocked work IDs, actual repository/worktree/branch/base/HEAD/diff identity, inventory/reuse decisions, reviewer verdicts/finding dispositions, exact fresh verification results, authorized Git/publication actions, signed artifact identity, live evidence/gaps if authorized, remaining fixtures and next permission/decision. Clearly distinguish **implementation-ready**, **publication-ready**, **published**, **deployed** and **qualified**.

**Completeness audit:** snapshot recovery maps to E7-01; arbitrary packaged catalogue/config to E7-02; foundations to E7-03; commands/diagnostics to E7-04; followers/history/cron/data paths to E7-05; maintenance to E7-06; coordinated qualification/docs to E7-07/E7-11; independent reviews/local verification to E7-08/E7-09; fixes/edge to E7-10. Permissions and dependencies are explicit. The approved plan is context; this file is the orchestrator's execution entry point.
