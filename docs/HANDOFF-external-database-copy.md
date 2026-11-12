# Execution spec: external PostgreSQL database copy

**Orchestrator entry point.** Implement the approved direction in `docs/PLAN-external-database-copy.md`. Plannotator returned `decision=approved`, zero annotations, on 2026-11-12. Approved plan SHA-256: `144ffd5efc0916c53e30c798fc09344862e45f728d798aee0f8426cff2616902`.

This is a separate execution specification, not implementation or authorization to publish. All work briefs below are self-contained when supplied with this execution contract and the approved plan. No agent-definition changes are proposed.

## 1. Objective and immutable behavior

Add `vaire db copy`: interactively collect external PostgreSQL connection details, new database name and vaire size; confirm; run a detachable node-side export/import; hand over normal newly generated vaire database credentials. Support a thin non-interactive path and structured output under existing CLI conventions.

The source supplies schema/objects/data, never destination cluster settings, roles, passwords, grants or tablespace placement. A source owner such as `deploycrate_ce_admin` is replaced with `<new>_owner` for ordinary imported objects. Extension member ownership may legitimately differ under vaire's existing extension policy. Source remains unchanged apart from normal read/export workload and locks.

**Never import into an existing database.** No overwrite/replace/force flag, clean/drop/recreate operation, automatic failed-target deletion, import replay, or implicit resume. A failed copy's target may remain partial; another copy uses a new unused name, or the operator separately authorizes normal explicit drop first.

No source-role reference rewriting or bespoke role-reference preflight. Such SQL may fail strictly. No continuous replication, application cutover, arbitrary package/repository installation, provider adapters, file import, local-source tunnel, non-PostgreSQL source, new size system or v1 backport. Do not add protected-copy UX in this version: create the ordinary unprotected default database; existing explicit protection commands remain available afterward.

## 2. Repository, checkpoint and permissions

- Repository/current planning worktree: `/home/mbv/work/vaire`; remote project `mbvlabs/vaire`.
- Observed branch/base: `master`, HEAD and local master `0d8e1fcc493cd30ca4afb89b54d6bd050302f552`. Local `1.x`: `ac742025e5e9c5a015e6217657bcb2c0d2b29b19`.
- Another worktree exists: `/home/mbv/work/vaire-phase-7`, branch `feat/phase-7-extensions`, observed short HEAD `8c11370`. It is not this feature's implementation worktree and must not be changed.
- Planning tree was already dirty: tracked edits in `docs/MASTER.md`, `docs/PLAN-v1-release.md`, `docs/RELEASE-v1.0.0-beta.1.md`; untracked cleanup, follower/Phase-7/planner handoffs/plans, Phase-6 publication notes, alpha release notes and state-of-union notes. Preserve all of them. This planning task adds only the external-copy plan and this handoff.
- **Revalidate mutable state before acting:** status, branches, worktrees, current sources, plan digest, pending work and applicable instructions. Do not reset/stash/overwrite unrelated work or fetch/rebase silently.
- New feature targets master/edge through a PR, not direct master push or `1.x`. No branch/worktree creation, commits, push, PR publication, tag, merge, release, upgrade, server mutation, provider connection or destructive cleanup is authorized by the plan approval. Obtain explicit implementation/location permission before source writes and separate permission for any such Git/live actions. Proposed branch name, if authorized: `feat/external-database-copy`.
- Work in the user-authorized worktree. Record its absolute path, branch, base SHA and initial status in `docs/STATE-external-database-copy.md`; that record supersedes mutable checkpoint assumptions here, not approved behavior.
- Do not read `.env`, credential/token files, SSH/GPG keys or key-bearing environment variables. Use approved interfaces or ask for input. No local Docker, Incus or scratch-VM harness; no repair commands for defective servers.

### Source-of-truth inputs

Read repository/global `AGENTS.md`; all current `docs/HANDOFF-*.md` for conflicts/dependencies; `docs/MASTER.md`, `docs/IMPLEMENTATION.md`, `docs/MANUAL-TESTING.md`, `docs/PLAN-v1-release.md`; approved copy plan; this spec. Phase-7 extension changes are concurrent work, **not a prerequisite to silently merge or reproduce**. Use the extension interfaces present in the authorized base; reconcile an API change if Phase 7 lands first. Stop if overlapping writes are active.

Prerequisites: repository-pinned Go/tooling from `mise.toml`/`justfile`; installed PG18 tools on destination for eventual runtime use; source PostgreSQL supported by PG18 pg_dump (upstream range 9.2–18), sufficient source read/export privileges, destination-to-source connectivity, encrypted source transport and trusted source contents. Do not install PostgreSQL clients on the operator or add an archive-upload control protocol.

## 3. Detailed behavioral contract

### 3.1 CLI/input/output

Command shape: `vaire db copy [name]`. Interactive missing values are prompted only when `MustNotPrompt` permits. Let the operator choose masked full URL or individual host/port/database/user/password/TLS fields; name and size are separate destination inputs. Size choices come from `config.SizeNames`/resolved resources, not a duplicate list. Include custom memory and preserve `--allow-overcommit` as explicit, never automatic. Explain size is memory/CPU/connections, not storage quota. Display destination context/name/effective resources and a sanitized source endpoint/database before confirmation.

Non-interactive inputs: destination positional name, existing `--size`, `--memory`, `--profile`, `--allow-overcommit` conventions; source via **`--source-url-stdin`**, one newline-terminated PostgreSQL URI from stdin, no password-bearing argv flag. Interactive individual fields normalize into the same internal source representation. Support custom source CA content via an explicit `--source-ca-file` read only by the operator, passed as secret content rather than a remote/local path in the URL. This is intentionally supplied certificate input, not permission to discover credential files. Do not support client private-key file loading in v1; refuse mTLS-required sources clearly.

Require explicit confirmation of the new target using existing `Confirm`/`--confirm <name>` behavior after summary; do not treat `--yes` as that token. Do not label copy as destructive to the source or existing targets. In JSON/no-input/non-TTY modes, missing inputs return structured `input_required`, never hang/prompt. Password/URI prompt cancellation has zero provisioning/export effects.

Dry-run validates destination input/config/resources and reports intended work only: no export, destination mutation, job creation, secret generation, or source connection. It can report source compatibility/reachability as **not checked**. Actual read-only source preflight occurs only for an actual copy invocation and is repeated by the job.

Generate destination passwords on the operator using `newDatabaseSecret`; carry service/recovery secrets as ordinary create/fork does. Reuse `credentialsMessage`, `createInterrupted` and fork unknown-outcome behavior after checking exact current semantics. Once a job may have received the passwords, every detach/unknown-outcome path must preserve the one-time handoff and job ID, not discard them on a generic error. `job follow` returns sanitized progress/outcome only; it cannot recover passwords. Do not put credentials into persistent job results, activity or ordinary JSON telemetry; intentional one-time credential output uses the existing explicit command-output policy.

### 3.2 Source transport and validation

Run PG18 preflight/export on the **destination node**; source allowlists must permit that server. No operator pg_dump dependency and no new network control port. URI/password travels in RPC `Secrets`, never params. Register raw URI, decoded password and any separately supplied sensitive values with redaction on operator and node, before parsing/logging/errors.

Accept PostgreSQL TCP URIs (`postgres://`, `postgresql://`) for one host/database; validate port, names and percent-encoding using a real parser. Reject Unix socket URLs, multi-host failover strings and arbitrary libpq options (`options`, service/passfile paths, source-controlled certificate/key paths, replication and startup overrides). Allow only explicitly modeled connection fields and supported TLS parameters; do not pass untrusted URI query strings through to subprocesses. Reject unknown fields with safe errors, not echoed values.

TLS: no plaintext fallback. Default to `verify-full` with system roots or explicitly supplied CA; permit explicit `require` for a provider URL that requests encryption without identity verification, but surface a clear verification warning. Reject `disable`, `allow`, `prefer` and any mode that weakens encryption. Honor `verify-ca` with its hostname-verification limitation reported. Never silently change a requested stronger mode. Use a bounded connection timeout and non-prompting pg_dump (`--no-password`); no automatic credential retries. Source password goes in a private short-lived passfile (correct `:`/backslash escaping), not `PGPASSWORD` or argv. Nonsecret host/port/database/user args and TLS environment are rebuilt from validated fields with an allowlisted process environment; clear inherited libpq connection/auth overrides. Disable GSS preference when necessary to enforce the declared TLS mode.

Source preflight reports server version, current DB, permissions sufficient to attempt export, database-size estimate and extension inventory. Do not promise permission completeness before pg_dump succeeds; do not enable row-security partial export or silently omit unreadable objects. Default foreign-table contents and special extension state limitations must be documented, not mislabeled a full physical clone. Refuse newer source major versions before destination creation.

### 3.3 Node job/lifecycle/locks

Add read-only preflight RPC `db.copy.preflight`, mutating start RPC `db.copy` and RPC-only detachable job `db-copy`. Source connection details are secrets in both calls. Preflight validates target name/absence/resources without reserving/configuring it. Start validates cheap inputs and launches a job without calling `addDatabase` first. No existing target or active same-target copy may be reused as a resume.

Job lock order: **node exclusive → heavy-I/O exclusive → destination cluster exclusive**. Take the cluster-name lock even while target is not configured; current `jobClusters` filters unconfigured names, so add a narrow copy-specific case. Never call RPC dispatch or reacquire these locks recursively from the job. Busy/skipped/start failures clean their copy secrets/workspace and truthfully indicate no import ran. Holding node-exclusive during export intentionally serializes desired-state changes for the first version; document this limitation rather than invent unsafe lock-release/reacquire state machines.

Job phases: `preflight → exporting → provisioning → importing → finalizing → succeeded`, with failed phase/actual target presence recorded in sanitized structured outcomes. Reread config after acquiring locks. Check target absent in config, all PostgreSQL major-version cluster inventory, expected data directory from `Server.DataDirectory(name)`, PostgreSQL configuration directory and any vaire lifecycle reservation that owns that name. Use non-following checks; existing files, dangling symlinks or unreadable evidence fail closed. Recheck immediately before config insertion. Namespace failure must have zero mutation callbacks for that target; do not clean unknown/orphaned directories.

Export a custom archive with PG18 `pg_dump -Fc --quote-all-identifiers`, no globals/roles or source credentials in argv, into private owned disk storage. Explicitly check export exit status and archive readability; only then create the target. No pg_dump/pg_restore shell pipeline that loses one exit code. Use a source-size/free-space estimate before export and again before provisioning on the archive/data filesystems; estimates are advisory, enforce the existing host safety floors, and treat disk/write errors as failures. Never auto-resize, delete data or continue from a truncated archive.

Reuse/factor `createDatabase` with a narrow internal option to defer its initial backup until after import; ordinary create remains unchanged. It can update other databases' rendered resource shares exactly as ordinary create already does, not alter their data. Preserve its existing safe pre-cluster rollback and post-cluster partial-state behavior, but report that `apply` can finish **provisioning only**, never a failed copy's data import. Never recommend `apply` as proof that data was copied. Keep completed/partial destination/config on post-creation failures; no general new rollback/repair mechanism.

### 3.4 Extension, ownership and grant behavior

Preflight source extensions against vaire's current resolver/catalog and node installation prerequisites. Destination extension intent is default vaire instrumentation plus supported source extensions, recorded in its normal spec. No source superuser, extension binary, APT repo, tablespace or cluster configuration is imported. Refuse unavailable/incompatible requirements before provisioning whenever detectable. No automatic extension version conversion; require a compatible installed version/schema for retained extension state or stop with a supported limitation.

Use a **new strict archive loader**, not `backup.LoadDump`, which currently downgrades pg_restore failures. Connect to the explicit new local database, restore as `<name>_owner` with `--no-owner --no-privileges --no-tablespaces --exit-on-error`. Omit `--clean`, `--create`, source roles, global role copying and database-level source creation/ownership. Run SQL import with owner privileges, not unrestricted postgres; connection may use local postgres solely to SET ROLE before archive SQL executes. No superuser fallback to pass unsupported objects.

Destination creation already installs supported extensions. Generate a private `pg_restore --list`/`--use-list` selection that omits **only** verified matching preprovisioned extension-definition and extension-comment TOC entries, plus archive database-definition entries that cannot apply to the explicitly named target. Preserve extension configuration data and ordinary objects. Do not regex-delete source SQL or broadly suppress schema/table errors. Inventory/TOC parsing must handle names safely and fail closed on unsupported forms; no line injection through names. Leave public-schema handling to normal PG18 archive semantics; verify fixture behavior. If strict restore encounters an unexpected existing-schema conflict, fix the precisely identified provisioning/selection cause, not `--clean`, permissive error filtering or blanket public-schema omission.

After import, restore vaire database connect/admin policy and default privileges. Existing `DatabaseGrantsSQL` primarily grants future public objects, so add a copy-specific helper to apply the same ordinary access policy to **existing owner-owned imported application objects** in every imported application schema: schema USAGE for app/ro, app SELECT/INSERT/UPDATE/DELETE and sequence USAGE/SELECT, ro SELECT, ordinary function EXECUTE for app, and corresponding future-owner default privileges. Migration role retains normal owner membership. Quote discovered identifiers; skip PostgreSQL/system schemas and extension-owned internal objects; do not grant owner/superuser/global privileges or assume every object can be owned/altered. Preserve ordinary vaire function defaults; do not invent extra ro write/execution rights.

Verify ordinary imported object owners and absence of imported source role identities, run ANALYZE with supported owner/maintenance privileges, then initial backup when configured. Preserve normal backup/metrics warning policy: a backup warning is not a data-import success override. Required grants/ownership/restore failures are fatal; no false `succeeded`. Record backup readiness separately from successful logical import. No archive SQL is considered safe just because owners/grants are excluded: require trusted-source confirmation text.

### 3.5 Private workspace and interruption hygiene

Use a fixed copy-owned disk root such as `/var/lib/vaire/copies/<validated-job-id>/`, restrictive directory 0700, files 0600 unless postgres execution needs narrowly scoped ownership/access. Do not use `/run` for a potentially huge archive or world-readable shared temp files. Register explicit operation ownership before writing; no caller-chosen paths, symlink following or path traversal. Keep source passfile/CA only until export ends; remove archive/TOC after restore or failure. Existing job-spec handoff is briefly private on disk and removed at read; do not persist source details in config/state/records.

Require cleanup on success, export failure, import failure, cancellation and start refusal. Cover abrupt unit death by a narrowly scoped unit cleanup hook or stale-workspace pruning tied to verified terminal/inactive copy job IDs; never delete active/unknown workspaces or scan/remove database directories. Pick the implementation mechanism in C03 and record it; it must satisfy these invariants, not add a user-facing repair command. Uncertain ownership or cleanup failure is reported sanitized and blocked, never hidden. A crash can leave a partial destination; reaping records unknown outcome, never success/replay. Temporary credentials/archive cleanup is hygiene, not authorization to delete a target database.

## 4. Ordered work items and delegation briefs

All worker briefs: read approved plan plus sections 1–3; work only in the authorized worktree and assigned paths, preserve unrelated changes; no secrets, live servers, Git/publication or destructive actions. Return changed files, implementation decisions, exact tests/results and limitations. Names below are roles, not new agent definitions. One implementation writer at a time in this shared lifecycle surface. Read-only research can run in parallel; freeze writes for integrated review/test evidence.

### C01 — Revalidate and freeze contract

**Owner:** orchestrator + read-only scout. **Depends:** none. **Targets/inputs:** current repo/instructions/source-of-truth docs, approved plan and checkpoint above; `docs/STATE-external-database-copy.md`.

Run `git status --short`, `git branch --show-current`, `git rev-parse HEAD master 1.x`, `git worktree list`, `sha256sum docs/PLAN-external-database-copy.md`. Read applicable handoffs; compare phase-7 API state without modifying its worktree. Obtain implementation location/branch permission. Record baseline, ownership, exact copy contract and pending external/live prerequisites. Resolve only internal mechanics under section 3; material changes go back to Plannotator.

**Output/done:** durable state record, authorized worktree and assigned file ownership; no unexplained overlapping changes. A missing permission blocks writes, not read-only investigation.

### C02 — Secure source model and strict PostgreSQL primitives

**Owner:** backend worker. **Depends:** C01. **Targets:** new `internal/backup/external.go`, `internal/backup/external_test.go` (or a narrowly scoped copy package if current boundaries require); existing `internal/backup/{dumps,scratch}.go`, `internal/pg`, `internal/engine/shell.go`, `internal/secrets` as pattern inputs, not permission to rewrite them.

Implement section 3.2 normalized/allowlisted source fields, safe parser/errors, TLS/passfile construction, preflight, export, disk estimates, archive list/selection and strict loader. Separate subprocess/filesystem seams for rootless unit tests. Add bounded diagnostics that do not echo source URI/password, raw source SQL or sensitive data. Leave existing tolerant restore behavior unchanged. Specify and record source extension-version/schema matching against actual available PG18 metadata.

**Tests:** percent-encoded passwords and escaping; raw/decoded redaction; malformed URI/query/path/host; inherited libpq override rejection; TLS modes/CA/require warning; PG19 refusal; source permission/export failure; complete exit-code propagation; argv/env/file permissions; archive selection preserving user/extension data; strict first SQL error, no clean/create/superuser fallback; disk failures and exclusive file creation.

**Output/done:** reusable copy-only primitives and passing rootless unit tests; all process invocations constructed from validated inputs, no source secret in argv/results/logs. Fake process tests are not claimed as real PG ownership/TLS proof.

### C03 — Detached node lifecycle and normal provisioning integration

**Owner:** node worker, single writer. **Depends:** C02. **Targets:** new `internal/node/dbcopy.go`/`dbcopy_test.go`; `internal/node/{db,jobs,methods,dispatch,activity}.go` and relevant tests; `internal/steps/postgres_roles.go`/tests; current catalog/config and `internal/steps/postgres_cluster.go` for namespace/extension facts.

Implement `db.copy.preflight`, `db.copy`, `db-copy` and section 3.3–3.5. Add job registration, RPC-only/exclusive/heavy classification, target mapping/activity redaction and the unconfigured target cluster lock. Build a job `Env` from fresh config/secrets without nested dispatch. Reuse create helpers with narrowly scoped deferred initial backup, normal service/recovery secrets and unchanged ordinary-create semantics. Provide strict copy-specific post-restore grants/ownership verification, explicit phases and sanitized actual target status on failure. Implement owned workspace lifecycle and abrupt-death hygiene; prove the chosen cleanup mechanism cannot touch a database or active/unknown operation.

**Tests:** lock order/modes/one acquisition; name collisions in config, any PG major, orphan paths, symlinks and concurrent copy; preflight does not reserve; export error has zero target mutation; late collision recheck; job startup failure/skipped locks; provisioning rollback vs partial cluster; strict import/grant/analyze failures leave partial target with phase; no replay/no auto-drop; budget/allow-overcommit; supported/missing/conflicting extensions; preexisting extension TOC; imported tables/sequences/custom schemas and quoted identifiers; no global source roles; normal backup warnings separated; disconnect/crash reaping and temporary cleanup. Re-run create/fork/follower/restore/job regressions for shared helpers.

**Output/done:** complete node workflow with passing targeted tests, lock/cleanup design recorded, truthful terminal outcomes and preserved existing behavior. Source code tests alone do not qualify real systemd or provider copying.

### C04 — Interactive CLI, sizing, confirmation and credentials

**Owner:** CLI worker, serial with C03. **Depends:** C01; integrate after C03. **Targets:** new `internal/cli/dbcopy.go`/tests; `internal/cli/{tree,meta,prompts,dbcreds,restore}.go`, `internal/cli/ui/prompt.go`, command-schema/discovery tests and generated artifacts where required by existing lint.

Implement section 3.1 on node APIs. Reuse UI masking/input helpers, `MustNotPrompt`, size resource summaries, normal progress/session/version checks and credential handoff. URL secret read from stdin must not conflict with prompts or be accidentally consumed twice. Do not offer a source-URL argv flag. Carry custom CA content privately. Preflight errors before confirmation/export must not provision. Display source-needs-node-connectivity, snapshot and trusted-SQL limitations. Keep dry-run explicit about unverified connectivity. Update command metadata, command discovery and structured schemas using existing generation process.

**Tests:** URL/details inputs converge; prompt ordering/cancel/no-input/JSON; custom size memory and invalid sizes; explicit overcommit; redacted summary/confirmation mismatch; dry-run zero mutation/job/credential generation; source stdin EOF; service/recovery secret maps; job progress; success handoff; Ctrl-C/lost-start reply/unknown-outcome all preserve destination credentials when possibly used; `job follow` never contains secrets; normal output modes. Retain existing create/fork credential regressions.

**Output/done:** interactive and non-interactive command tests pass; command metadata lint accepts new operation; no missing-input hang, unsafe argv, silent size change or password loss.

### C05 — Documentation and runtime qualification run sheet

**Owner:** orchestrator/doc worker. **Depends:** draft after C01, finalize C02–04. **Targets:** `docs/MASTER.md`, `docs/IMPLEMENTATION.md`, `docs/MANUAL-TESTING.md`, README/embedded CLI guide as current conventions require; new `docs/RUN-external-database-copy.md`.

Document actual command syntax/input, sizes vs storage, node reachability/allowlists, TLS warnings/CA, credentials/detach, source-version/extension/foreign-data limitations, trusted SQL, no overwrite/no replay, partial-target handling and serializing node lock. Preserve existing status edits and dated manual evidence; mark feature built/live-pending only when built. Do not rewrite historical passes as copy evidence or imply release.

Run sheet case IDs **COPY-01** normal source with non-vaire owner; **COPY-02** named/custom sizes and budget refusal; **COPY-03** existing/config/orphan collision; **COPY-04** wrong password/unreachable/TLS mismatch/newer server; **COPY-05** supported/missing extension and strict restore failure; **COPY-06** app/ro/migrate privileges, sequences, multiple schemas/default privileges; **COPY-07** detach/re-follow/unknown credentials; **COPY-08** export disk/start/import/crash failure with transient cleanup and partial-state accuracy; **COPY-09** source unchanged/snapshot boundary and backup readiness.

For each supply actual implemented command forms, source/destination prerequisites, permission, expected result, evidence and cleanup boundary. Resolve fixture/context names and all command placeholders before running or handing commands to the user. These docs authorize no live action. Some source-version/crash/disk cases may be blocked by fixture availability; say so explicitly, never use unsafe faults or local harnesses to fill gaps.

**Output/done:** accurate operator docs and full case map; every acceptance behavior has unit evidence or explicit live-pending case, with no fabricated results.

### C06 — Independent integrated review, fix and re-review

**Owner:** independent reviewer; orchestrator assigns fixes to scoped workers. **Depends:** C02–05 complete, frozen diff. **Targets:** entire feature diff, tests/docs, `docs/REVIEW-external-database-copy.md`.

Self-contained review brief: use the exact authorized worktree/base/HEAD+diff supplied by orchestrator, read approved plan and this contract, inspect the entire change read-only. No edits, secrets or network/live actions. Audit no-overwrite/namespace/locking/replay, trusted archive execution identity, ownership/grants/extensions, secret argv/env/log/state paths, workspace crash cleanup, source TLS/version/permissions, credentials on detach, normal create/fork/restore/follower regressions and honest docs. Return severity, file:line, concrete failure, violated contract, root-cause fix and regression proposal, whole-change accept/block verdict and live limits. Passing tests alone are not correctness proof.

Orchestrator records every finding/disposition, fixes coherent minimal batches with one writer, gets original reviewer re-review and reruns C07 after fixes. Material new behavior needs renewed Plannotator direction approval. Stop/ask after two failed correction cycles for the same defect, unavailable independent reviewer, or unexplained cross-worktree conflicts.

**Output/done:** integrated report with no unresolved blocker/major findings, documented minor-risk disposition and an independent accept verdict at the final diff.

### C07 — Fresh final-tree verification and handoff

**Owner:** tester; orchestrator validates. **Depends:** frozen C02–05; can run alongside read-only C06. **Targets:** authorized worktree; review report and state record.

Run exactly:

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint
git diff --check
go test -count=1 ./...
go test -count=1 ./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport
```

Record UTC times, tool versions, worktree/branch/base/HEAD and diff identity, each command/exit code and durable sanitized output/evidence references in `docs/REVIEW-external-database-copy.md`. A chained failure means later steps did not run. Missing tool/environment issue is blocked, never passed; no sudo install/cache destruction without permission. Re-run the full required chain on the final reviewed tree and before any separately authorized commit.

**Output/done:** required checks pass fresh at reviewed tree; report completed/blocked IDs, source/docs changed, unit evidence, live gaps and next permission. No commits/deployment implied. Linux/macOS cross-build is not macOS runtime or Debian/arm64 installation verification.

### C08 — Optional separately authorized real-VPS qualification

**Owner:** one live tester/orchestrator; user handles desktop/root-key/sudo/TOTP/interactive actions. **Depends:** C06/C07 and named live/source/deployment permissions; normal reviewed PR/release process if deployment needed. **Targets:** runtime run sheet and `docs/MANUAL-TESTING.md`.

Revalidate context/server/operator/node identities and source trust/access, inventory, headroom and existing jobs read-only before mutation. Historical test VPS: `46.224.129.96`, `db2.mbvlabs.com`, Ubuntu 26.04; `nobucket` historically live, `mbvlabs` historically bucket-only. These are records, not verified current targets or permission. Ask for a trusted external PostgreSQL fixture/access; never request or print its URL in chat/logs. Do not read saved provider credential files.

Use normal signed distribution after separately authorized publication; do not upload a dev binary, switch contexts, rebuild a server or modify provider allowlists without permission. Run resolved COPY cases serially, bound disk/WAL/workload, verify source untouched, inspect owners/access through approved queries, follow jobs and verify no leaked temporary credentials/archive. Do not force disk exhaustion, kill a real workload or run source writes to simulate failures without separately approved safe fault design. Cleanup of destination fixtures uses ordinary separately authorized explicit drop; failed copies are not auto-healed.

**Output/done:** dated PASS/FAIL/BLOCKED/NOT RUN per case, exact candidate/context/commands/times/job IDs, sanitized facts and residual fixtures. Any defect requires fix/review/release/upgrade/retest, not manual repair. Qualification is complete only when required cases pass or gaps are explicitly accepted by the user; no stable tag/backport in this item.

## 5. Sequencing, limits and completion audit

```text
C01 → C02 → C03 → C04 → finalize C05
              C05 drafting may overlap read-only
                         ↓
                  C06 review || C07 tests
                         ↓
                fixes → re-review → fresh C07
                         ↓
           ready for separate Git/live permissions
                         ↓
                 optional authorized C08
```

Single writer owns shared node/CLI/role/job files at any time. Do not run changing-source tests and call them frozen final evidence. Orchestrator alone integrates docs and handles authorized Git/publication. No commits, branch changes, tags, PRs, releases or server mutations by delegated workers.

Stop for a source requiring unsupported privileged restore/extensions/TLS, inability to preserve credentials or safe owner execution, ambiguous namespace/workspace ownership, new permission requirements, conflicting concurrent changes, missing tools/reviewer, material direction changes or any unsafe live observation. Do not silently expand into role rewriting, tunnels, extension infrastructure or resumable copy.

Approved work-stream mapping: CLI/contract → C01/C04; source export/preflight → C02; node lifecycle/provisioning → C03; strict import/access/cleanup → C02/C03; documentation/regression/verification → C05–C07; separately authorized live evidence → C08. All have concrete inputs, owners, dependencies, outputs and completion gates. No historical automated or live pass is presented as verification of this feature.

Final orchestrator report: worktree/base/actual checkpoint, completed/blocked work IDs, reviewed diff and verdict, exact fresh check results, behavior/coverage limitations, current fixtures if any, actual authorized Git/live actions, and next permission needed. Distinguish implementation-ready, publication-ready, published, deployed and qualified. The approved plan is context; **this file is the execution entry point**.

## 6. Primary technical references and observed integration facts

- PostgreSQL 18 pg_dump: https://www.postgresql.org/docs/18/app-pgdump.html (snapshot, version range, privileges and executable-dump warning).
- pg_restore: https://www.postgresql.org/docs/18/app-pgrestore.html (ownership/ACL/role, exit-on-error, create/clean semantics).
- libpq TLS/URI: https://www.postgresql.org/docs/18/libpq-connect.html ; password files: https://www.postgresql.org/docs/18/libpq-pgpass.html .
- Extensions: https://www.postgresql.org/docs/18/extend-extensions.html .
- Observed `internal/node/db.go`: `createDatabase` provisions and starts first backup; `addDatabaseFrom` checks config/PG inventory and budgets but not every orphan path. Revalidate before factoring.
- Observed `internal/node/jobs.go`: private read-and-delete secret spec, job lock classifications, configured-only cluster-lock selection, `startJob`/`followJob` and unknown-outcome records.
- Observed `internal/backup/scratch.go`: `LoadDump` is deliberately permissive and filters extension-comment errors. Do not change existing restore promises by replacing it globally.
- Observed `internal/steps/postgres_roles.go`: `DatabaseConnectSQL`, `AdminRoleSQL`, `DatabaseGrantsSQL`, owner NOLOGIN and migrate membership. Existing grants target public future objects, not full imported-object privileges.
- Observed `internal/cli/{db,dbcreds,restore,prompts}.go`: ordinary creation secrets, interrupted/fork credential handling, sizing and structured-input conventions.

These observations were read-only planning evidence at the checkpoint above, not fresh test passes. Exact external-provider compatibility, extension TOC collisions, real peer/SET ROLE behavior, TLS/systemd interruption and disk bounds remain runtime qualification requirements.
