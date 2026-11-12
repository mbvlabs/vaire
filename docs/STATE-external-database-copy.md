# External database copy execution state

## Authorized baseline

- User requested implementation of the execution handoff and explicitly selected a new feature branch/worktree.
- Worktree: `/home/mbv/.local/share/opencode/worktree/0283f0/feat/external-database-copy`.
- Branch: `feat/external-database-copy`.
- Base and initial HEAD: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552` (master).
- Initial feature worktree was clean. The approved plan and execution handoff were then copied into it from the planning worktree.
- Approved plan SHA-256: `144ffd5efc0916c53e30c798fc09344862e45f728d798aee0f8426cff2616902` (verified).
- Planning worktree remains untouched, including its pre-existing modified documentation and untracked planning artifacts. Phase-7 worktree is separate and untouched; its extension API changes are not merged or reproduced here.
- Authorized: implementation, rootless unit tests/build/lint, independent review, documentation in this worktree. Not authorized: commits, publication, releases, merges, live servers, external source connections or destructive cleanup.

## Contract and ownership

The approved plan and execution handoff govern behavior: new targets only; strict owner-privileged logical restore; encrypted node-side source access; no source secrets in params, argv, state or telemetry; detachable jobs with one-time credentials; no replay or automatic target cleanup; truthful partial outcomes; copy-owned temporary cleanup only.

One implementation writer at a time owns source changes. C02 implements secure source/archive primitives, C03 node lifecycle and grants, C04 CLI, and C05 operator documentation. Read-only investigation may overlap. C06 independent review and C07 fresh final-tree verification follow a frozen implementation. Orchestrator owns integration and this state record.

## Progress and external prerequisites

- C01: baseline and location permission complete; no active source overlap found.
- C02: complete in `internal/backup/external.go` and `external_test.go`; rootless unit tests pass. See the C02 mechanics record and the C02 correction record below. Verification at the C02 tree: `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...` and `just lint` all passed.
- C03: complete in `internal/node/dbcopy.go`/`dbcopy_test.go`, with narrow changes to `internal/node/{db,jobs,methods,activity}.go`, `internal/steps/{postgres_roles,postgres_cluster}.go` and `internal/backup/external.go`. See the C03 mechanics record below.
- C04: complete in `internal/cli/dbcopy.go`/`dbcopy_test.go`, with narrow changes to `internal/cli/tree.go`, `internal/remote`, `internal/node/dbcopy.go` and `internal/backup/external.go`. See the C04 mechanics record below.
- C05: operator documentation added in README, MASTER, IMPLEMENTATION, MANUAL-TESTING and the embedded agent guide; RUN maps COPY-01–09, all live NOT RUN. Exact live fixture commands remain blocked until approved context/source/name resolution. Final review pending.
- C06: integrated review verdict BLOCK at frozen feature diff `c5cf7e0b9eff817825e5b24853d77df83fed2ac449eb66b473706d9791f377a7`; E01–E03, original C03 findings and R1/R2 are resolved for their reported defects. C06-01..C06-03 corrections are implemented and unit/build-verified in the record below; original-reviewer re-review and fresh C07 are pending. Not accepted.
- C07: independent fresh verification passed all four required command groups at that frozen identity on 2026-11-12; evidence in `docs/evidence/external-copy/summary.md` and REVIEW. Rerun required after any source correction. Installed just was 1.58.0 rather than declared 1.43.0; other version facts are recorded in evidence.
- C08: not authorized. Real PostgreSQL ownership/TLS, provider compatibility, systemd crash cleanup, disk behavior and live credentials/detach require separately authorized qualification; unit fakes are not runtime proof.
- No live fixtures were created or changed.

## Publication authorization

After the user asked to ship, they explicitly selected **Ship to edge** in chat: once final review and checks pass, authorize commit, feature-branch push, PR creation/merge into master and edge workflow verification. No deployment, live source access or fixture mutation is included. User explicitly requested no Inbox; subsequent updates/questions stay in chat.

Final correction checkpoint: source frozen at `2026-11-12T14:39:24Z`, feature diff SHA-256 `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67` (same REVIEW/STATE/evidence exclusions). Original integrated reviewer re-review and independent fresh C07 rerun are running. Publication remains gated on acceptance and successful checks.

Subsequent user override: merge to master now and test afterward, explicitly waiving another pre-merge independent review. Finish the in-flight observer-cancellation/plain-prompt correction and mandatory pre-commit checks, then commit/push/PR/merge to master/edge. Do not claim independent acceptance or live qualification. Source and operator credentials, existing servers and unrelated work remain protected; no destructive live action is authorized.

## C02 mechanics record

C02 added only copy-specific primitives; the tolerant `backup.LoadDump` restore path, `internal/pg`, `internal/engine` and `internal/secrets` were not changed.

### Source parsing, TLS and process isolation

- A source is one of the two fixed PostgreSQL TCP URL schemes, one host, one database and a validated user. The only accepted query parameter is `sslmode`; every other query key, multi-host strings, Unix-socket paths, opaque URLs, fragments and control characters are rejected without echoing the value. Source database names that contain `=` or begin with `postgres://`/`postgresql://` are refused rather than handed to libpq's `--dbname` parser.
- The default TLS mode is `verify-full`; an explicit CA makes it use that CA content, otherwise PG18 libpq uses the system trust store via `sslrootcert=system`. `verify-ca` uses an explicit CA or the node's system CA bundle file; `require` encrypts without identity verification and returns a warning. `disable`, `allow` and `prefer` are refused, and a CA with `require` is refused. No plaintext fallback exists.
- Raw source URLs and decoded passwords are registered for redaction before validation. `Source` keeps the password and CA unexported, implements every fmt verb as the redacted URL, and never serializes secrets.
- Source clients run as the postgres OS account through `runuser` and `/usr/bin/env` with an allowlisted environment. `HOME` is a private workspace directory, so `~/.postgresql` client certificates, keys and root certificates are not loaded. `PGPASSFILE` is always set to a private 0600 file, even when no password is supplied. `PGGSSENCMODE=disable`, a bounded `PGCONNECT_TIMEOUT`, the modeled `PGSSLMODE`, and only the required root-certificate path are set. Source URIs, passwords, passfile contents and raw SQL are never in argv, the environment map or structured results.
- The passfile escapes `\` and `:` in every field; an IPv6 host and a database or user containing a colon therefore remain representable. Passwords containing NUL, CR or LF are refused because the file format cannot carry them.

### Preflight and export

- Preflight and export run on the destination node with the fixed PG18 `psql`, `pg_dump` and `pg_restore` binaries. `CheckPG18Tools` proves those binaries exist and are executable before a job uses them.
- Preflight is a read-only transaction. It uses `COPY ... TO STDOUT WITH CSV` rather than JSON aggregation so the same query works on the supported 9.2–18 source range. It reports server version, current database, database size, connection/catalog checks and the `pg_extension` name/version/schema inventory, and refuses PG19 or older than 9.2 before any destination work.
- Export is `pg_dump -Fc --quote-all-identifiers --no-password` with only the modeled host, port, database and user arguments. No globals, roles, `--create` or source credentials appear in argv. The archive is written through stdout to a new exclusive 0600 file in the private workspace. A non-zero exit is always an error, the file is synced and closed, and `pg_restore --list --create` must produce a readable CUSTOM TOC whose database matches the requested source before the archive is accepted. Partial or invalid archives are removed.

### TOC selection and strict loader

- TOC text from `pg_restore --list --create` is treated as unsafe because `pg_restore` does not quote names. Only the numeric id, catalog ids and a small description prefix are parsed; object names are never split on whitespace. Any control character in an entry the selector must inspect fails closed.
- The selector emits a use-list containing only `<id>;` lines, in original TOC order. Source-controlled names cannot inject lines or `pg_restore` options.
- Candidate entries are re-read with `pg_restore --create --use-list=<one id> --file=-`, and their first SQL statement is checked. The selector omits only: the archive database definition and database properties, a verified database comment/security label, and verified extension definitions/comments for the matched preprovisioned extension plan. Ordinary objects, ordinary comments and security labels, extension configuration data and user objects are kept. Duplicate, missing, unexpected or ambiguous database/extension entries fail closed.
- Extension compatibility is exact on name, version and schema. The destination plan must include every source extension, including `plpgsql`; destination-only default extensions are not passed to the selector. A missing extension, version mismatch or schema mismatch stops before provisioning.
- The strict loader runs `pg_restore` as the postgres OS account, with the archive on stdin and a private 0600 use-list, as `--role=<destination>_owner` with `--no-owner --no-privileges --no-tablespaces --exit-on-error --no-password -d <target>`. It never passes `--clean`, `--create`, `--superuser`, `--if-exists`, disable-trigger or other permissive options. Any non-zero exit is a failure; the tolerant `LoadDump` warning filter is not used. Structured restore diagnostics keep the PostgreSQL error lines but omit the failing source SQL and server DETAIL/HINT lines that may carry row data.
- The destination owner must not be `postgres` or another vaire administrative role; the loader refuses those roles rather than silently restoring with more privilege.

### Workspace, disk and test seams

- Fixed names live under `/var/lib/vaire/copies/<validated-job-id>/`. The job directory is 0700 and owned by postgres; the copy root is created 0711 so postgres can traverse to a job directory without being able to list it. Every file is exclusive-create 0600. File operations use an `os.Root`, so names cannot traverse or follow symlinks outside the job directory. Creating an existing job directory is a hard failure, not a resume.
- Source passfile and CA files exist only for the preflight/export call and are removed regardless of outcome. The archive, TOC list, use-list and private home are removed by the workspace cleanup; the workspace itself is removed only by an explicit lifecycle call. Cleanup failures are reported, never ignored.
- Disk checks reuse `backup.CopyNeed`/`CheckSpace` on both the workspace filesystem and the expected data directory; estimates remain advisory beyond the existing safety floor.
- Rootless tests use a recording `CopyExecutor`, the current-user workspace owner, and injectable space-check and system-CA paths. They cover URL parsing and redaction, percent-encoded passwords and passfile escaping, TLS modes and CA files, environment isolation, preflight/PG19/auth failures, export and exit-code propagation, TOC parsing and selection, strict loader arguments and first-error handling, extension matching, disk failures, exclusive files, symlink refusal and workspace cleanup. They do not prove real pg_dump/pg_restore behavior, TLS handshakes, extension config-data privileges or postgres-owned workspace behavior on a live node; those remain C03/C08 runtime work.

## C03 mechanics record

C03 added the node lifecycle only; there is no `vaire db copy` CLI yet (C04).

### RPCs and job contract

- `db.copy.preflight` is read-only and lockless: it validates the target namespace and resource preview without reserving/configured it, opens only a private copy workspace for the source access files, runs C02's read-only source preflight, resolves the destination extension plan, checks space and removes the workspace. It never starts a job or saves `cluster.toml`.
- `db.copy` validates target/size/budget and the role/source secrets, then starts the RPC-only `db-copy` job. It does not call `addDatabase` or any destination mutation first.
- Source input is carried only in secrets: `copy.source_url` (one PostgreSQL URL) or `copy.source_spec` (JSON with host, port, database, user, password, TLS mode and optional CA PEM). Destination role passwords use the existing `db.<name>.<role>` keys; service/step secrets use the existing `steps.StepSecrets` keys. The job spec's secrets are read once and its file removed at read.
- The job uses phases `preflight`, `exporting`, `provisioning`, `importing`, `finalizing`, `succeeded`. Failures carry the phase, `import_ran`, `target_present` and a `target` object (`in_config`, `cluster`, `database`, `data_directory`, `config_directory`, `known`) in sanitized job details. Success carries the redacted source facts, actual created specification, extension matches, target state, `imported: true` and a separate `backup` object (`configured`, `ready`, optional warning). Backup warnings never turn a successful logical import into a failure.
- `db.copy` returns the followed job envelope; its one-time credential handoff remains C04's responsibility and is never stored in node results.

### Locks, namespace and provisioning

- `db-copy` is RPC-only, node-exclusive, heavy-I/O exclusive and target-cluster exclusive. `jobClusters` includes the target even when it is not configured yet; the job therefore serializes with another copy and with all later desired-state writers. The job takes each lock once and never dispatches another RPC or reacquires them.
- Holding node-exclusive through export and import is intentional for this version: it serializes ordinary desired-state changes while a copy runs, rather than adding an unsafe lock-release/reacquire state machine around export.
- After the locks the job reloads `cluster.toml`. It refuses a target present in config, in any PostgreSQL major version's `pg_lsclusters` inventory, as an expected data directory, as the PostgreSQL configuration directory, or as the subject of an unfinished restore/drill/undo/unfollow/recovery record. Probes use `Lstat`, so a dangling symlink or unreadable evidence fails closed. The full check runs again immediately before the entry is saved (`addDatabaseFrom`'s `beforeSave`), after the export.
- `createDatabase` was split into `createDatabaseWithOptions`/`provisionDatabase` with one unexported `deferInitialBackup` option. Ordinary create behavior is unchanged; copy provisioning defers only the stanza, first full and timers, which the job runs after import. Rollback when the cluster was never created and partial-state behavior when it exists are the existing create behavior.
- Destination extension intent is `pg_stat_statements` plus the source's supported catalog extensions, ordered by control-file dependencies. `CopyExtensionPlan` reads PG18's installed `.control` and default-version SQL files: a missing extension, an unsupported name, a dependency cycle, or a version/schema that `CREATE EXTENSION` would not reproduce refuses before export/provisioning. The actual `pg_extension` rows are checked again after provisioning against C02's exact match before the strict import starts.

### Import, policy and verification

- The strict C02 loader runs in the new database as `<name>_owner` with `--no-owner --no-privileges --no-tablespaces --exit-on-error`, the private use-list, no clean/create/superuser fallback.
- After restore the job reapplies `DatabaseConnectSQL` and `AdminAccessSQL` for the new database, then runs `CopyObjectGrantsSQL`. That helper grants every imported application schema (excluding system, `vaire` and extension-owned schemas/objects): schema USAGE to app/ro, table DML to app, table SELECT to ro, sequence USAGE/SELECT to app, ordinary function EXECUTE to app, and the matching future-owner defaults. It quotes discovered identifiers and never grants owner or global privileges.
- `CopyObjectVerifySQL` counts database-owner mismatches, ordinary object-owner mismatches, missing schema/table/sequence/function access and returns any nonzero count as a fatal `copy_verify_failed`. `ANALYZE` then runs as the destination owner. Grant, policy, ownership, verify and ANALYZE failures are fatal and leave the actual partial target in place with its phase.
- A failed or partial target is never dropped or replayed automatically. A retry with the same name is refused by the config/path/inventory check; only an explicit operator `vaire db drop` or a new name is supported.

### Workspace and abrupt-death cleanup

- Each job owns `/var/lib/vaire/copies/<job-id>/`; preflight uses `/var/lib/vaire/copies/<request-id>/`. C02 creates the root 0711 and each job directory 0700/0600 files owned by postgres. C03 never uses the path-based `CopyWorkspace.Remove`.
- `copyCleanupWorkspace` closes the workspace and calls the new `backup.PruneCopyWorkspace`, which validates that the copy root is a plain directory owned by the vaire process and not group/other-writable, opens it and the job directory through `os.Root`, compares the opened directory's identity with the directory entry, and requires the job directory to be a 0700 directory owned by postgres. It removes only that held job directory; a symlink, another owner, another mode or changed identity is refused and reported.
- Every workspace carries an `operation` marker, written before any source access or archive work, that names its kind and exact job id. Stale pruning accepts only the two copy-owned kinds (`db-copy`, `db-copy-preflight`); an unmarked or mismatched directory is unknown and is preserved.
- Stale job pruning is tied to verified copy job records: a directory is removed only when its name is a valid copy workspace id, a `db-copy` job record exists for exactly that id, and the record is terminal or its PID/unit is proven gone. Directories with no record, a non-copy record, an active record, or an unverifiable root/directory are left untouched. A preflight workspace has no job record; it is removed only when its verified marker names `db-copy-preflight` and its directory is older than a bounded 15-minute preflight lifetime. It only lists the copy root and never scans or removes a database data directory. `job list` and the start of `db.copy` run this bounded pruning; `db.copy` also reports any prune refusal as a warning.
- A killed copy process can therefore leave a partial destination and a workspace; the next `vaire job list`/`db.copy` cleans the workspace when the job is verified inactive/terminal. It never treats the workspace cleanup as success or permission to delete/replay the database. An unknown or replacement workspace is preserved.
- The C02 access-file removal paths are still the serial C02 hardening item. C03's whole-workspace cleanup removes any leftover passfile/CA/archive on the normal failure paths; C02's path-based access removal and root creation races remain C02-owned.

### Temporary C02 review boundary

The independent early C02 review reported that raw stderr diagnostics can carry source rows or SQL, and that the copy root lacks owner/writable-mode validation with pathname cleanup races. C03 does not store raw subprocess output: it drops C02 log events before streaming, strips `output_tail`/`source_error`/other unclassified details from node outcomes, and classifies failures with fixed messages and stable reasons. The C03 lifecycle cleanup uses the anchored `PruneCopyWorkspace` above rather than C02's pathname removal. C02's diagnostic replacement, root creation/chown hardening and access-file error propagation remain the serial C02 fix, not silently claimed here. **Superseded by the C02 correction record at the end of this file.**

### C03 tests

Rootless tests cover lock order/modes and the unconfigured target lock; config/any-major/path/symlink/reservation collisions; preflight non-reservation; export failure with zero target mutation; late collision; skipped/start-failure truthfulness; provisioning rollback versus partial cluster; deferred ordinary first backup; budget/allow-overcommit; extension plan version/schema/missing/dependency/duplicate cases; TOC selection and strict-restore owner options; imported-object grant/verify SQL quoting and exclusions; policy/grant/verify/ANALYZE failures with partial phases; no replay; backup warnings separate from import success; success cleanup; preflight source redaction/cleanup; and anchored pruning for terminal, inactive/crashed, active, unknown, wrong-owner, unsafe-mode and symlink workspaces with a database-directory control. These are unit/environment tests, not a live PostgreSQL/TLS/systemd qualification.

## C02 correction record (early review E01–E03)

This correction is limited to `internal/backup/external.go`, `internal/backup/external_test.go` and this record. C03's anchored `PruneCopyWorkspace`/`CopyWorkspaceOperation` interfaces and operation-marker format are preserved. C03 lifecycle source was not changed.

### E01 — raw subprocess diagnostics

- Every C02 subprocess now runs behind `copyReporter`, which forwards only structured non-log events. No psql, pg_dump, pg_restore or local-restore log line reaches a node reporter, run log or job result.
- Returned errors no longer carry `output_tail`, `source_error`, causes, exception text, SQL, object names or server continuation lines. Preflight/export/archive/restore failures return only stable reasons, fixed messages/hints and numeric exit status. Restore failures map only to fixed families (`restore_permission_failed`, `restore_conflict`, `restore_failed`).
- Internal failure classification reads at most 64 KiB of the bounded `ScriptError` tail; that text is never returned or logged.
- Tests inject inline `ERROR`, multiline `DETAIL`/`STATEMENT`/`CONTEXT` and SQL/row sentinels through fake preflight, export, archive-list, inspect and restore paths, assert no log event or returned text carries them, and assert no `output_tail`/`source_error`. Oversized stdout and oversized error tails are tested for preflight, TOC listing, TOC inspection, export and restore with small test limits.

### E02 — workspace root and identity anchoring

- The copy root and its ancestors are verified before use. The root must be a plain directory owned by the effective process, not group/other-writable, and traversable by postgres. Ancestors must be plain non-symlink directories; non-sticky ancestors must be owned by the effective process or root, other-writable ancestors must be sticky, and group-writable ancestors are accepted only when they are owned by the process and its effective group. Existing symlink roots and symlink or unsafe ancestors fail closed.
- The root is opened through `os.Root` and its identity compared with the inspected directory. `NewCopyWorkspace` creates the job directory through the root handle, pins the opened job directory to the created entry with `os.SameFile`, and applies chmod/chown through the opened directory handle. The copy root itself is created and opened through a verified parent handle; no pathname chmod or chown is used. Directory and file ownership are applied through handles (`Root.Chown`/`File.Chown`). The old chown argument remains only as an ignored compatibility parameter for early C02/node test call sites.
- Creation failure cleanup removes a job directory only when the parent entry still names the exact created directory; a replacement directory or symlink is left untouched. `CopyWorkspace.Remove` verifies the stored directory identity against a verified parent handle before removing through that handle. A replaced directory or symlink returns `copy_workspace_unsafe` and is preserved. `Cleanup` continues to operate only through the original job handle.
- Rootless tests cover writable, symlink, symlink-ancestor, other-writable-ancestor and foreign-owner roots; replacement directories/symlinks injected after mkdir, after opening the job root, and before removal; and confirm outside/replacement/original paths are not touched. `PruneCopyWorkspace`, `CopyWorkspaceOperation` and the operation marker remain unchanged in behavior.

### E03 — cleanup failures during inspection and partial preparation

- `inspectTOCEntry` now joins `inspect.list` removal errors into its returned error, including after a successful SQL inspection. `SelectArchiveTOC` stops rather than continuing with a leaked inspection file.
- When the optional CA write fails, `prepareSourceAccess` joins passfile removal errors instead of discarding them. `Create`/`WriteFile` also propagate cleanup failures for their own partial files.
- Tests force a non-empty directory at `inspect.list` after inspection and assert `workspace_cleanup_failed` with exactly one inspection call; they verify passfile cleanup after a CA write failure and inject a failing passfile removal to assert the safe cleanup error is joined.

### Correction verification

- `go test -count=1 ./internal/backup ./internal/node` passed; the wider targeted set `./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport` and `go test -race -count=1 ./internal/backup` also passed.
- `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `golangci-lint run ./...` (0 issues), `GOOS=darwin GOARCH=arm64 go build ./...` and `just lint` were run after the correction; results are recorded in the worker report.
- These are rootless unit/environment tests only. Real pg_dump/pg_restore behavior, TLS, postgres-owned workspace chown, systemd abrupt death and live PostgreSQL privileges remain C08 qualification items. Independent re-review is still required; this record does not mark acceptance.

## C03 correction batch (C03-01..C03-05)

C02 E01–E03 are complete and their interfaces are preserved: copy subprocess diagnostics stay bounded/sanitized, workspace root/identity access uses the C02 opened-handle helpers, and cleanup failures are joined/propagated. This batch changed only the node/jobs/steps surfaces below plus narrow backup lifecycle lease/marker additions.

### Ambiguous launch and truthful copy liveness

- `startCopyJob` distinguishes prelaunch refusal from a `systemd-run` attempt:
  - before launch, errors are definite, the spec is removed, and no job ID is reported;
  - after the launch call, any error preserves the job ID and queued record, never writes a terminal record, and best-effort removes the unread private spec immediately. Removal is never treated as cancellation or proof; a sanitized `spec_cleanup_failed` detail is returned if it fails. C04 must retain and print generated credentials on `job_outcome_unknown`.
- Copy liveness is copy-specific; ordinary `jobGone`, `unitState`, follow, and reap behavior is unchanged. It requires a positively inactive/not-found unit, no live/indeterminate recorded worker/MainPID/ControlPID, and a successful `/proc` scan showing no `node run db-copy --job-id <id>` worker. `active`, `activating`, `reloading`, `deactivating`, query errors, malformed metadata, queued grace and unknown evidence retain. Terminal records are not an inactivity bypass. Rootless tests cover the recorded PID, MainPID and ControlPID branches (live, dead and indeterminate controls) and the worker scan; real systemd/PID-race behavior remains live-pending.
- `followJob` uses this copy liveness and only writes a synthetic crash through a conditional state update that cannot overwrite a worker terminal record. `reapJobs` uses the copy liveness only for `db-copy`. Private specs are removed only after that positive no-worker evidence; a retained spec is reported blocked.

### Lifetime lease and pruning

- Every workspace has a 0600 `lease` file and an operation marker containing kind, exact job ID, lease `device:inode`, and a random token. The parent holds the lease shared for the whole operation. Every C02 workspace subprocess executes under an identity-verified wrapper: it opens the lease, verifies fd identity and token, locks `/proc/self/fd/<n>` shared, re-verifies the pathname identity/token after acquiring the lock, and only then executes the original command. Deletion, replacement, same-inode token mutation and a deliberately interleaved pre-lock acquisition gap fail closed in rootless tests. A descendant keeps the shared lock after the parent lease is closed; actual parent-process death is not simulated by the unit test and remains live-pending.
- `PruneCopyWorkspace` opens verified root/job/lease handles, compares marker identity/token, takes a nonblocking exclusive lease lock, and holds it through removal. Busy returns `ErrCopyWorkspaceBusy` (retain); missing/mismatched evidence is a structured blocked warning (retain). There is no age-based inactivity: preflight workspaces are pruned only through the verified marker plus a free lease. Terminal job records still require the liveness and lease checks.
- Own cleanup closes the parent lease before pruning. A cleanup failure after successful import is fatal, not a success warning: the job fails with `reason copy_cleanup_failed`, `phase finalizing`, `import_ran:true`, `imported:true`, `target_present:true`, the actual target/backup state, `workspace_cleanup:"failed"` and `blocked:true`. The target is retained and never replayed or dropped. Early failures keep their original error plus `cleanup_failed` detail.

### SQL and extension corrections

- C03-01: `CopyObjectVerifySQL` now uses `has_table_privilege` for the foreign-table check; a generated-SQL regression forbids `has_foreign_table_privilege`. Live PG18 qualification remains C08.
- C03-02: extension availability resolves direct base scripts and base-plus-update install chains (`<ext>--from--to.sql`) to the control default version; broken chains refuse before provisioning. Tests cover direct, reachable chain, broken chain, missing control, dependency order and duplicate/unsupported cases.

### Correction tests

Rootless tests cover ambiguous launch ID/unknown/spec removal and cleanup failure, prelaunch definite refusal, copy liveness states (active/activating/reloading/deactivating/query error/masked/not-found/recorded PID/MainPID/ControlPID/worker scan), terminal no bypass, synthetic crash non-overwrite, lease busy/identity/same-inode token mutation/replacement/deletion/deliberate acquisition-gap/descendant-after-parent-lease-close, old-active preflight retention, spec cleanup after positive inactivity, fatal post-import cleanup failure with imported true/target retained, the supported privilege function, and direct/chain/broken extension install paths. Existing C02/C03 and shared create/fork/follower/restore/job regressions remain green in the interim verification.

### R1/R2 private-spec hygiene

- Copy job spec consumption is copy-specific and fail-closed: decoded secrets are registered before the unlink diagnostic; a failed unlink returns `secrets_error` with `reason=copy_job_spec_unlink_failed` and `job_id`, leaves the spec retained, writes a definite failed job record, and returns before config load, activity job start, source preflight, export or provisioning. Invalid copy specs are likewise sanitized and never executed. Ordinary `readJobSpec` behavior is unchanged.
- Private-spec cleanup is independent of the copy workspace root: state must be readable to verify inactivity, but an absent, unsafe or unreadable workspace root no longer suppresses cleanup of positively inactive copy specs. Active/unknown specs remain; removal failures produce `copy_job_spec_cleanup_failed` warnings. Tests cover absent, unsafe and simulated unreadable roots, active retention, inactive removal, and cleanup failure.
- R1/R2 rootless evidence does not prove a full root-executed `vaire node run db-copy` refusal path; the source ordering is inspected, and the spec-reader/cleanup functions are tested directly.

C03 correction verification at this tree: `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check` passed. This is still not independent re-review or live qualification.

## C04 mechanics record

C04 added the operator command only. It does not change C02/C03 lifecycle behavior, ordinary create/fork/restore/follower semantics or the node job contract beyond the narrow source-CA and request-ID seams below.

### Command, inputs and output

- `vaire db copy [name]` uses `--size`/`--memory`/`--profile`/`--allow-overcommit` exactly as `db create`, offers `config.SizeNames()` with resolved memory/CPU/connection labels when prompting, and reports size as memory/CPU/connections rather than a storage quota. A custom size requires memory. Name and size prompts appear only when `MustNotPrompt` allows them.
- `--source-url-stdin` reads exactly one newline-terminated PostgreSQL URL from stdin; it is never accepted in argv. A shared buffered stdin is installed before any prompt so the URL line cannot be lost or read twice. EOF without a newline, an empty line, CR/LF in the value or a missing source in JSON/no-input/non-TTY mode is structured `input_required`.
- Without `--source-url-stdin`, a prompt chooses a masked full URL or individual host, port, database, user, password and TLS mode. Password input uses the existing masked helpers. Both forms send exactly one of `copy.source_url` or `copy.source_spec`; the node parses both into the same validated `backup.Source`.
- `--source-ca-file` reads certificate content on the operator. Only the PEM content travels, in the new explicit `copy.source_ca` secret, never the local path. `--source-url-stdin` remains the thin non-interactive path; there is no URL or password argv flag and no mTLS private-key input.
- The real `db.copy.preflight` runs after source collection and before confirmation. Its redacted endpoint/database, server version, size estimate, TLS mode, extension count and destination effective resources are shown with the destination context/name, node-connectivity requirement, one-time snapshot boundary and trusted-SQL warning. Confirmation is `Confirm`/`--confirm <name>`; `--yes` never satisfies it.
- `--dry-run` calls only `db.copy` with `DryRun` and no secret map. It validates destination name/absence/resources in the node, makes no source connection, creates no job, writes no config and generates no database passwords; its output says source compatibility/reachability was not checked.

### Credentials, request ID and failures

- Destination role passwords are generated with `newDatabaseSecret` only after confirmation. The request also carries `serviceSecrets`, the ordinary recovery kit and one source form plus optional CA content. The new role passwords are printed once through `credentialsMessage`; success output also carries the sanitized source, actual target and separate backup state.
- The operator preselected the `db.copy` request ULID through the new narrow `remote.CallOptions.RequestID` field. The node already names the copy job after the request ID, so `job_id` is known before the reply. Ctrl-C detach, a lost reply and an ambiguous/unknown launch all keep the same job id and the generated credentials.
- The credential handoff is restricted to outcomes that may have used them: `transport_failed`, `credentials_may_be_used`, `job_outcome_unknown`, `target_present` or `imported`. A definite pre-launch refusal stays unchanged and prints no password. Unknown outcomes are never rewritten as definite failures.
- An `imported:true` cleanup failure remains a failure with the partial/complete target retained. Its note says vaire never replays the import or drops the target, and the hint does not advise `vaire apply` as a way to finish a copy.
- `vaire job follow` is suggested only for progress/outcome. It never receives source secrets and cannot recover passwords.

### Narrow interface changes

- `internal/remote.CallOptions.RequestID` selects a request ULID instead of generating one; an empty value preserves prior behavior. Tests cover preselection with and without a command operation id.
- `internal/node.CopySourceCASecret` (`copy.source_ca`) carries explicit PEM content. `copySourceFromSecrets` attaches it through the new `backup.Source.WithCACert`, which registers the content for redaction and revalidates TLS mode and PEM. Both URL and individual-field forms accept the separate CA; supplying a CA twice is refused. `copyJobSecrets` allowlists the CA key alongside the role, service and one source form.
- `backup.Source.WithCACert` refuses a second certificate instead of replacing one, rejects a CA with `sslmode=require`, and rejects invalid PEM. The existing URL parser and node URL branch are unchanged otherwise.

### Tests and limits

- Rootless CLI tests cover name validation and no-input/JSON refusal, strict stdin URL line handling and no double consumption, URL/field secret convergence, CA content versus path, custom sizes/overcommit and invalid resource inputs, redacted summaries, dry-run zero source/preflight/job/credential effects, preflight failure with no start, success and detach credential handoff, lost-reply and unknown-outcome job-id/credential retention, service/recovery secret maps, human and JSON output, and command metadata.
- Node tests cover URL and field forms converging with the separate CA, duplicate CA refusal, job-spec CA allowlisting, and the backup CA helper. Existing create/fork/follower/restore/job tests remain green.
- Rootless tests do not exercise a real terminal prompt sequence, real SSH loss/reconnect, a real detached systemd job or a live PostgreSQL source. Those remain C08 qualification items. `CopySourceCASecret` is not a general certificate-discovery feature; it is only the operator-supplied CA content for this command.

## C02 E02 root-ancestor correction

The earlier E02 fix skipped ancestor ownership and group-write checks whenever the effective uid was 0, and trusted every sticky directory. That left production root able to follow a foreign-owned ancestor and treated a foreign-owned sticky directory as safe even though its owner can replace entries.

- `verifyCopyAncestors` now always requires each ancestor owner to be root or the effective vaire process, including when vaire runs as root. A foreign-owned non-sticky ancestor and a foreign-owned sticky ancestor are both refused before the copy root or job directory is created.
- A trusted sticky directory such as a root-owned `/tmp` is accepted: unrelated users can create only their own entries, while the trusted owner controls replacement. Non-sticky directories still reject other-writable ancestors and group-writable ancestors unless the owner and group are the process's own.
- `copyEffectiveUID`/`copyEffectiveGID` are narrow seams for rootless tests to model an effective root process. Production still uses `os.Geteuid`/`os.Getegid`, and the existing current-user workspace tests are unchanged.
- Tests simulate effective uid 0 with a foreign-owned non-sticky, foreign-owned group-writable and foreign-owned sticky ancestor, plus a trusted owner with a foreign-group-writable non-sticky ancestor; each is refused before the copy root is created. A trusted root-owned sticky ancestor is accepted. This remains unit evidence only; real node ownership behavior is C08 work.

## C04 verification

At this tree, `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check` passed. These are rootless unit/build checks, not an independent review or live qualification.

## R1/R2 worker verification

At the R1/R2 worker tree on 2026-11-12, the same full command set passed: `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...` (0 issues), `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check`. The targeted node set also passed under `go test -race` (`TestReadCopyJobSpec*`, `TestPruneCopyWorkspacesCleans*`, `TestCopyJobLiveness*`, `TestCopyLease*`, `TestCopySyntheticCrash*`). This is worker verification only; independent review and live qualification remain outstanding. The targeted R1/R2, liveness and lease tests are the evidence named above; no full root-executed `vaire node run db-copy` refusal or real parent-process death was exercised.

## C06 correction batch (C06-01..C06-03)

Minimal integrated-review correction batch. R1/R2 private-spec fixes, C03 copy liveness/lease behavior and ordinary job/create/fork/restore semantics are preserved. No Git, live, network or secret-file action was performed.

### C06-01 — built-in plpgsql definition is optional

- `SelectArchiveTOC` still accepts and verifies a plpgsql `EXTENSION` definition when a dump contains one, and still omits only verified definitions/comments. Installed compatibility is unchanged: `MatchSourceExtensions` continues to require exact name/version/schema for every source extension, including plpgsql.
- The archive-definition requirement is now separate through `archiveExtensionDefinitionRequired`: plpgsql is the one built-in extension whose creation entry modern pg_dump omits, so an absent plpgsql definition is allowed. Every other planned extension missing its definition remains a strict `archive_invalid` failure; there is no general extension skip.
- Added an authentic modern-TOC regression (installed plpgsql, no plpgsql definition, ordinary table/data/sequence entries) proving ordinary objects remain selected and only the database definition is omitted. The existing plpgsql-definition fixture still verifies defined plpgsql, and the missing-definition case now includes plpgsql alongside a missing non-built-in extension to prove strictness is retained.

### C06-02 — observation failure after a started copy is unknown, not terminal

- `followJob` now wraps initial and poll `loadJobState` failures in a private `jobObservationError`, distinguishing a failure to observe a started job from its terminal `jobOutcome`. `jobFollowPoll` is a narrow default-1s test seam.
- `handleJobFollow` turns an observation failure into the typed `JobObservationFailed` error with `job_id` and `observation_failed`, no underlying diagnostics. A definite terminal job failure is not converted.
- `handleDBCopy` classifies a `jobObservationError` (and a missing job record immediately after a confirmed start) as an unknown outcome with the confirmed `start`, `phase:"unknown"`, `import_outcome:"unknown"`, `credentials_may_be_used:true`, the exact job ID and the actual target state. Terminal `jobOutcome` errors pass through unchanged, including a cleanup-failed/imported target. Nothing is stopped, replayed or dropped.
- The CLI credential predicate preserves the one-time handoff for `transport_failed`, `credentials_may_be_used`, `observation_failed`, `target_present`, `imported`, `job_outcome_unknown`, `job_observation_failed` and a missing started-job record. Human failures print the credentials; structured failures carry them only in `error.details.database`. A definite terminal copy failure that proves no target/import keeps its exact error and prints no password.
- Node tests exercise the actual `handleDBCopy` → `followJob` path with initial and poll state-read failures, asserting unknown details, known job ID, credentials flag and no source URL/password in the serialized error. A `job.follow` test covers the typed observation error. CLI tests cover human and structured node observation shapes, the reattach observation path known job ID, and the existing lost/ambiguous/terminal cases.

### C06-03 — exact-byte copy source password

- Added `ui.SecretExact`, which returns the masked prompt value byte for byte; `ui.Secret` keeps its existing trimming behavior for unrelated passphrase prompts.
- Added copy-specific `readCopyPassword` with injectable plain/rich readers. It preserves leading, trailing and all-space bytes and rejects NUL/CR/LF with `validation_failed`, matching the modeled source password rules. The individual-field source path uses it; URL input behavior is unchanged.
- Tests exercise both the plain and rich renderer seams with leading, trailing and all-space passwords, assert both encode/decode identically through `url.UserPassword`, and reject NUL/CR/LF in both paths. No real terminal is used.

### Verification and limits

- Targeted: `go test -count=1 ./internal/backup ./internal/node ./internal/cli` passed; `golangci-lint run ./internal/backup ./internal/node ./internal/cli` reported 0 issues.
- Full chain: `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check` passed (recorded below with the final C07 rerun).
- These are rootless unit/build checks. Real pg_dump/plpgsql behavior, real systemd observation failure/reattach, terminal password echo and live PostgreSQL remain C08/runtime items. Independent re-review and fresh C07 are required; this record is not acceptance.

C06 correction verification at this tree: targeted `go test -count=1 ./internal/backup ./internal/node ./internal/cli ./internal/steps ./internal/config ./internal/remote ./internal/transport` passed (seven packages); `golangci-lint run ./internal/backup ./internal/node ./internal/cli` reported 0 issues. The full chain `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check` passed; linter 0 issues, command metadata complete, 17 steps, shellcheck/actionlint/install-version checks passed. Uncached regression is the `-count=1 ./...` run. Independent re-review and fresh C07 remain required.

### Second C06-02 correction — observer cancellation and plain prompt label

- `followJob`'s observer `ctx.Done()` branch now wraps `ctx.Err()` as `jobObservationError` only after a best-effort check for an already-recorded terminal record. A real terminal outcome (including a recorded cancellation before any target) wins and stays definite; an interrupted observer with a running/absent record is observation failure. All other observation exits are the initial read, poll read and missing-record path already classified by `handleDBCopy`/`handleJobFollow`.
- `handleDBCopy` therefore maps observer cancellation to the same unknown outcome with `job_id`, `credentials_may_be_used:true`, `phase:"unknown"`, `import_outcome:"unknown"` and actual target state. `handleJobFollow` returns typed `JobObservationFailed` so CLI reattachment keeps the handoff. A recorded terminal cancellation with `target_present:false` remains the original definite `cancelled` failure with no credentials-may-be-used flag.
- The plain copy password prompt now writes `label: ` to the diagnostic writer before `term.ReadPassword`, prints its newline after it, and never writes the returned bytes. Tests assert label-before-reader order, exact diagnostics `Source password: \n`, no password echo, zero rich-path plain diagnostics, and still cover leading/trailing/all-space exact bytes plus NUL/CR/LF rejection.
- Tests added/updated: `TestDBCopyFollowObservationFailureIsUnknown/observer_cancellation_while_running`, `TestDBCopyRecordedTerminalCancellationStaysDefinite`, `TestJobFollowObservationFailureIsTyped/observer_cancellation_while_running`, and `TestCopyPasswordPromptPreservesExactBytes` prompt-order/no-echo assertions.
- Verification after this correction: targeted seven-package `go test -count=1` passed; full chain `go build ./...`, `go vet ./...`, `go test -count=1 ./...`, `golangci-lint run ./...` (0 issues), `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint` and `git diff --check` passed. No live/systemd/terminal qualification; no acceptance.
