# Copy an external PostgreSQL database into vaire

**Status:** proposed for approval · 2026-11-12. This is the human-facing direction, not the execution handoff. After approval, a separate orchestrator spec will define the implementation work. No source changes, releases or server operations are authorized by this plan.

## Outcome

Add **`vaire db copy`**, a human-led operation that copies one external PostgreSQL database into a **brand-new vaire database**. The source supplies database objects and data; vaire supplies the destination cluster, sizing, configuration, users, passwords, ownership and normal access policy.

```text
Source URL or connection details       New name + vaire size
               │                              │
               └──────────────┬───────────────┘
                              ▼
                   Check source and destination
                              ▼
                  Confirm a redacted copy summary
                              ▼
                Export a consistent source snapshot
                              ▼
                  Create normal vaire database
                              ▼
              Import with vaire ownership and grants
                              ▼
             Verify → normal vaire credentials handoff
```

Existing databases are **never** import targets. There is no overwrite, replace or force option.

## Settled requirements

- Prompt for a source PostgreSQL URL, or separate host, port, database, username, password and TLS details. Mask passwords and password-bearing URLs.
- Ask for the new database name and offer vaire's existing sizes, including custom memory. Show the effective resource limits and preserve the existing budget checks and explicit overcommit policy. Size means vaire's memory/CPU/connection profile, **not a disk allocation or a promise that the source data fits**.
- Generate the same destination roles/passwords as ordinary database creation. For example, source objects owned by `deploycrate_ce_admin` become owned by the destination's normal vaire owner; source roles, passwords and grants are not imported.
- Leave the source unchanged. This is a one-time snapshot, not ongoing replication or an application cutover.
- Do not build special rewriting/detection of explicit source-role references. If PostgreSQL rejects such an object, report a failed copy rather than claim success.

## Current evidence

The existing database-creation area already handles sizes, budget checks, normal clusters and credential handoff. It is currently flag-driven, so the copy prompts are a new thin interactive layer, not reuse of an existing create wizard.

The backup/restore area already exports custom-format PostgreSQL archives and can restore without source owners or privileges. Those workflows assume vaire-managed sources and recovery state, however; the new copy must not reuse their destructive cutover behavior or role-copying assumptions. Some existing restore handling tolerates warnings; the copy needs strict failure reporting.

Node jobs already support detached execution, locks and following progress. Managed servers already have PostgreSQL 18 client tools. These are source-code observations; no provider copy or fresh live verification has been performed.

## Proposed approach and tradeoffs

### Connection and execution

Run the export and import **on the destination vaire server**, using its installed PostgreSQL 18 tools and existing detachable-job system. This keeps the operator CLI a standalone binary, avoids a second archive-transfer protocol and lets the operation continue after the terminal disconnects.

**Tradeoff:** the source must be reachable from the vaire server. Provider IP allowlists must allow that server; a source reachable only from the operator's laptop is not supported in this version. Explain that before starting. Use encrypted source connections by default, preserve certificate validation, and never silently fall back to plaintext.

Carry source secrets through the existing encrypted SSH secret channel. Keep temporary job secrets/access files private and short-lived, out of persistent configuration, job results, command arguments, logs and activity history. The destination server necessarily receives temporary source access.

### Export before provisioning

Check compatibility, source access, supported extension prerequisites, target-name absence, budget and disk headroom. After confirmation, export into a private temporary archive on the destination server **before creating the destination**. Recheck destination absence and capacity under the normal mutation locks before provisioning. This avoids leaving a new cluster behind after most source/export failures, and allows export and restore failures to be distinguished.

The archive needs temporary disk in addition to the restored database; estimates are advisory and runtime disk/write errors must fail safely. Clean transient files on normal completion/failure and provide bounded lifecycle cleanup for interrupted work—never delete unrelated files or databases.

### Normal destination, strict import

Reuse ordinary provisioning and its size/budget/role rules, without importing source cluster settings, tablespaces, ownership or access grants. Restore ordinary objects under the normal vaire owner, then apply normal vaire grants and analyze. Prepare supported extensions through vaire's supported mechanisms; do not install arbitrary packages, copy privileged provider internals or invent substitute extensions.

Restore only to the explicitly selected new database, without archive-directed database creation or destructive clean options. A PostgreSQL restore error makes the operation fail; never downgrade it to a successful copy with warnings. Copying a database is a trusted-source operation: a database dump can contain executable SQL.

### Failure and credential handoff

Before provisioning, failure leaves no destination database. After provisioning starts, failure may leave a new empty or partially imported database; report its actual state and job ID. Never automatically drop it, retry an import into it, or treat a repeated command as permission to overwrite it. A new copy attempt needs an unused destination name; intentional removal uses the existing explicit database-drop operation.

Use existing one-time credential handoff semantics, including interruption/unknown-outcome handling: the operator must retain generated credentials if a detached job may have installed them. Job history must never store credentials or pretend that following a job later can recover them.

## Scope boundaries

**In scope:** one source PostgreSQL database, interactive prompts, a thin non-interactive input path consistent with vaire's CLI conventions, normal sizing, secure source access, strict dump/import, progress and detached jobs, normal credential output, regression tests and a manual qualification run sheet.

**Not in scope:** non-PostgreSQL providers, continuous synchronization, zero-downtime migration, application/DNS cutover, replacing an existing database, source-role rewriting, copying source users/passwords or cluster settings, arbitrary extension installation, local-only source tunnels, file imports, automatic repair/retry, provider-specific adapters or a release/deployment in this planning task.

PostgreSQL 18 export tools reject newer source majors. Older sources within the tool's supported range are candidates, not a promise of every provider/schema being compatible. Missing export privileges, unavailable extensions, provider-only objects and unsupported TLS requirements receive actionable failures. Ordinary dumps do not copy foreign-server data or all extension-internal state; do not promise a byte-for-byte clone.

## Work streams and order

| Order | Area | Outcome |
| --- | --- | --- |
| 1 | CLI and copy contract | Prompts, secure inputs, sizing summary, confirmation and structured outcomes |
| 2 | Source export and preflight | Validated source connectivity, version/extension checks, private archive and safe errors |
| 3 | Node lifecycle and provisioning | Detached copy job, locks, new-name enforcement, ordinary destination setup and truthful failure state |
| 4 | Import and finalization | vaire ownership/access, strict restore, cleanup and credential handoff |
| 5 | Regression review and documentation | Automated evidence, operator guidance and real-VPS test instructions |

Contract and source research can overlap; integrate the job/provisioning/import changes serially because they share lifecycle state. Source review and automated verification run against a frozen implementation, then fixes are re-reviewed and re-tested.

## Acceptance and verification

- A human can supply source details, select an existing vaire size or custom memory, confirm a redacted summary, and obtain a working newly named vaire database and normal generated credentials.
- Imported ordinary objects have vaire ownership; standard app, read-only and migration access works. Source credentials and roles are not adopted.
- Existing/configured/orphaned target names and races cannot cause any existing cluster or database to be modified.
- Normal memory-budget rules apply; selected sizes never silently change. Disk failures do not become successful copies.
- Bad authentication/TLS, unsupported versions/extensions, export/restore errors and detach/crash cases produce accurate outcomes without leaking source details or losing the destination-credential handoff.
- Long jobs survive operator disconnect; following reports progress and outcome, not recoverable secrets. Interrupted copies cannot be replayed into an existing destination.
- Unit/regression coverage exercises command orchestration, subprocess arguments, redaction, lifecycle, locking, cleanup, credentials and ownership SQL. Run the repository's full build/vet/test/lint and macOS cross-build checks.
- No Docker, Incus or scratch-VM harness. Actual data/ownership/access/TLS/detach behavior requires a separately authorized real-VPS qualification with a trusted external PostgreSQL fixture. Record not-run/blocked cases honestly; historical test results are not evidence for this feature.

## Risks and authorization

Most complexity is safe lifecycle integration, not the export command: avoiding namespace races, preserving credentials across detach, strictly handling partial import and keeping credentials/data out of telemetry. Concurrent extension/follower work may alter the integration points; revalidate before implementation and preserve unrelated work.

Approval accepts the direction above and triggers a separate execution spec. Branch/worktree creation, commits, PR publication, deployment, source access, live fixture creation and any destructive cleanup still require their own authorization under repository rules. This is a new feature for the edge/master line, not an automatic v1 backport.
