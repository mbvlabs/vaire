# Phase 7: extensions on edge, then qualify with followers

## Outcome

Finish Phase 7 against today's follower-enabled master, publish it through a reviewed pull request to edge, and prepare one coordinated Phase 6 + Phase 7 qualification run. Do not wait for follower live qualification to finish before building or publishing extensions.

This changes the earlier release plan's **test each feature before merging the next** order. It does not waive activity-history qualification before deployment, follower tests, extension tests, or beta readiness gates.

## What is known

| Evidence | What it establishes |
| --- | --- |
| Current checkout and remote master agree | Followers have reached master; no extension pull request is open. |
| Follower publication record | Followers were reviewed and published to edge; deployment and live qualification remain pending. This is historical evidence, not a new test pass. |
| Current extension implementation | Only the baseline contrib catalogue exists; Phase 7 still needs integration. |
| Preserved extension snapshot | Substantial older implementation and tests are available, but use the old project identity, predate current security/activity/follower changes, and have unusable Git provenance. They are reference material, not a branch to merge wholesale. |
| Fresh package-index research | PostgreSQL 18 packages exist across the supported Debian/Ubuntu and amd64/arm64 matrix for all planned packaged entries, including PGDG's Apache-2-only Timescale variant. Metadata availability is not installation qualification. |

No source changes, builds, server checks or live tests have been performed by this planning task.

## Scope and decisions

### 1. Recover selectively, integrate into current architecture

Preserve the old snapshot unchanged. Inventory and fingerprint useful source/tests, then adapt only Phase 7 behavior into current code. Do not replace current application, security, release, activity or follower infrastructure with older snapshot versions. **Backward compatibility is not a release constraint before v1:** do not add legacy formats, migrations or shims just to preserve old extension behavior. Preserve current security and data-safety guarantees, not obsolete interfaces.

### 2. Any compatible packaged extension, with a helpful built-in catalogue

- Implement extension listing, addition and removal, package discovery/installation, per-cluster configuration and diagnostics.
- Keep existing defaults. Accept documented CLI aliases, but store canonical names in desired state.
- The built-in catalogue is a convenience, **not an allowlist**. Support any PostgreSQL 18-compatible extension supplied by an explicitly selected APT package from an already configured, trusted repository, or already installed by the operator. For example, pgvector installs from its available package without building its GitHub source.
- For entries outside the built-in catalogue, persist explicit installation and runtime metadata (package or preinstalled source, extension name, required preload/settings and optional schema), so apply, followers and rebuild/recovery can reproduce the same intent. Validate real availability and compatibility; do not infer a native library's preload requirements from its name.
- Install packages on demand and hold them; never uninstall packages during extension/database removal. Do not restrict the general mechanism to PGDG or Apache-only extensions. Built-in entries may retain verified PGDG defaults; alternate packages must be selected explicitly rather than silently replacing an installed provider. Package conflicts fail visibly.
- No automated source builds, arbitrary install scripts, or automatic repository/key provisioning. Missing repository setup is an explicit prerequisite, especially after rebuild. Omit the optional built-in partition-manager background worker.
- Refuse missing restart authorization **before mutation** for direct extension commands. Confirm every affected existing cluster separately; permission to restart a source never authorizes a follower restart. Unrelated databases must not restart.
- Addition may run on protected databases without weakening their existing access policy. Removal refuses protected databases, requires target confirmation, explains dependent objects and extension-owned data, and never cascades without explicit authorization.
- Re-adding an enabled extension may update its SQL version to the installed package's default through a supported update path; this must be visible in preview/results, not disguised as a no-op. No package upgrade or new update command.
- A plain configuration edit/apply retains the existing deferred-restart model: report pending preload work, never create a preload-dependent extension prematurely or silently restart it.

### 3. Extensions must work with followers and recovery

- Followers inherit the source's extension intent and libraries; SQL installation/removal is performed only on the primary and reaches followers through WAL. Reject direct extension mutation on a follower.
- Source extension changes synchronize follower configuration with explicit restart authorization, safe ordering, current-state validation and retryable partial-failure reporting. Preserve follower generations, ownership, protection independence and hot-standby floor rules.
- Packages must be available before any recovered, forked or temporary cluster starts. Copies retain required preloads but run no copied scheduled jobs.
- Custom extension metadata must explicitly declare copy/standby safety and any settings needed to disable background work. Unknown safety blocks the affected copy/follower operation with an actionable prerequisite; accepting arbitrary native extensions does not imply knowing how to suppress every extension's external side effects.
- Forks and unfollowed independent copies keep copied cron jobs paused and retargeted to the new database. Restore/recovery reconcile desired configuration and actual restored extensions without silently deleting restored data.
- Historical restore points with extensions removed from today's desired state need explicit coverage; retained packages alone are not proof that their historical preloads are usable after a rebuild.

### 4. Finish extension-dependent maintenance honestly

- Implement manual repack and opt-in scheduled repack using existing detachable jobs, locks, maintenance windows and protected-database rules. Refuse unsupported tables or inadequate space; never kill another backend to obtain a lock.
- Add the promised table-scoped `--exact` bloat report with **full-scan pgstattuple measurements**, not the preserved implementation's approximate function. Explain scan cost and concurrent-update limits; do not call measured free space a universal exact bloat figure.
- Preserve estimated reports by default. Do not enable automatic repack by default or grant broad write access merely to make a maintenance client work.

## Work streams and order

```text
Snapshot inventory + current contracts + package evidence
                         ↓
Catalogue / configuration / package / role foundations
                         ↓
Commands + follower/recovery integration + maintenance
                         ↓
Independent cumulative reviews → fixes → fresh full local gate
                         ↓
Authorized PR / edge publication → verify exact signed artifact
                         ↓
Authorized combined qualification: activity gate → no-bucket → bucket/rebuild
```

Read-only research and reviews may run concurrently. Shared application/integration areas have one writer. One live owner drives the single VPS serially; combined qualification means one release and coordinated fixtures, not concurrent destructive tests.

## Acceptance and verification

- Useful preserved behavior is accounted for, but no old identity, stale infrastructure or unrelated snapshot changes leak into the port.
- Commands have current structured output, discovery metadata, dry-run behavior, confirmation/protection checks and activity integration.
- Regression tests cover package/render compatibility, missing authorization with zero mutation, dependent-object removal, partial failures/retries, follower ordering/token isolation, restore/fork/recovery, cron suppression and protected maintenance.
- Two independent whole-change reviews accept the integrated result; all required build, vet, unit, lint, metadata, shell and Darwin cross-build checks pass on the final reviewed tree.
- Publication is recorded separately from deployment and qualification. The coordinated live run sheet preserves all existing follower cases and adds extension/maintenance/recovery cases with bounded fixtures and explicit gaps.
- Record actual OS/architecture coverage. A single Ubuntu amd64 VPS cannot establish real-install qualification for every supported platform.

## Risks and boundaries

The largest risks are stale snapshot assumptions, follower restart/retry ordering, privileged extension grants, background jobs escaping from copies, historical preload recovery, and repack locking/disk use. Resolve these with current-code integration, adversarial regression tests and independent reviews—not repair commands or relaxed safeguards.

No local containers or scratch VMs; no new agents, source-build automation, repository/key installers, background-worker tuning redesign, Phase 8 upgrade tooling, HA work, stable-line merge or beta tag. Native extensions are trusted server code, not a sandbox: explicit package selection is privileged installation authorization. Do not claim historical or local checks as fresh live passes.

Approval authorizes the planning direction and writing the separate orchestrator spec. It is not permission for the planner to implement, nor permission to create branches/worktrees, commit, push, merge, deploy, restart, rebuild or destroy resources. The orchestrator must obtain and record the relevant execution permissions.
