# First v1 alpha: ordered live qualification plan

Status: archived after main-scenario execution, 2026-10-19. Prepared 2026-10-16. This is the original test procedure, not a current to-do list or authorization to rerun destructive steps. Current outcomes and coverage limits are in [MANUAL-TESTING.md](../MANUAL-TESTING.md); remaining publication steps are in [PLAN-v1-release.md](../PLAN-v1-release.md).

## Objective and boundaries

Qualify the original v1 fixes and all six follow-up review fixes before the first alpha. Do not touch major-upgrade design/implementation or Phases 6–7. Do not create branches/tags, change production release pointers, commit, or push without explicit authorization. Do not rebuild/destroy the VPS, remove contexts, or delete bucket objects without separate explicit approval.

Run non-destructive checks on the existing server first. Use disposable databases only; never modify application data. Stop on unexpected failures, capture the exact error, diagnose the root cause, and agree the next action rather than hiding a failure with retries or repair commands.

The agent runs eru, curl and SSH checks from thelab. The operator handles only passwords/TOTP, interactive prompts, sudo, provider actions and the other machine. Give the operator one complete command at a time. Never capture or print passwords, credentials, recovery identities, transfer bundles or authenticator setup secrets in chat or test logs. Filter database-creation credentials before output. Do not automate around human-factor gates with artificial terminals or generated codes.

## Historical starting evidence (2026-10-16)

- Fresh fetch found local `master` and `origin/master` at `9d3ae9ec3ff450b52ff26b9c03e521c8ab8dbd1e`; the six follow-up code fixes are already committed and pushed. Preserve the existing modified testing log and untracked review document.
- Workflow `38913519999` was still building when inspected. A short watch timed out; no successful publication is established by this session.
- CLI and live node: `edge-65c68b7d3b01`. Context `nobucket`, VPS `46.224.129.96`, hostname `db2.mbvlabs.com`, Ubuntu 26.04.1 LTS. Baseline doctor: 38 pass, 2 expected no-backup warnings, 5 skip, 0 fail.
- `app` was unprotected; TOTP was not configured. `eru_admin` held `pg_maintain`, `pg_signal_backend` and `app_owner`. That is not evidence of a protected-policy upgrade regression test.
- Before the pause, disposable `alpha_policy` was created with the old release, successfully. No application data was changed, no upgrade/apply occurred, and no TOTP setup was performed by this session. Leave this database in place pending review.

## Evidence standard

For every test, append date/time, context, CLI/node versions, exact non-secret command, expected and actual outcome, and pass/fail/blocked to `docs/MANUAL-TESTING.md`. Include structured error codes, exact failure messages and relevant job/workflow IDs. Record expected refusals as passes only when their reason and lack of side effects are verified. Keep historical failures intact; add dated outcomes rather than rewriting them as successes.

## 1. Review and release readiness gate

1. Review this plan before any further live mutation.
2. Recheck git status, recent commits and freshly fetched `origin/master`; inventory pending changes without overwriting them. Ask before any needed commit or push.
3. Watch the edge workflow associated with the intended commit through completion with a sufficient timeout. Confirm the published edge manifest identifies that commit. Do not upgrade while the build is pending, failed, or publishes another commit.
4. Keep stable publishing blocked until an authorized alpha tag. No synthetic tags or pointer changes for testing.

## 2. Preserve a genuine pre-upgrade protection fixture

This step deliberately precedes CLI/node upgrade. It needs operator interaction and approval to configure TOTP on the live server, protecting only the disposable database.

1. Recheck the old CLI/node versions and `alpha_policy` state. Create a small fixture table and index there while it is unprotected; use clearly named rows and retain a count/checksum baseline. Do not touch `app`.
2. Operator runs old-release `protect init --db alpha_policy`, privately scans the authenticator secret and confirms it. Do not save the secret or code.
3. Observe protection status and actual `eru_admin` memberships on `alpha_policy` before upgrading. Record the old grants, completed roles-step journal evidence and unchanged database configuration using narrowly selected, non-secret fields.
4. Demonstrate the old lock hole only against the disposable table with a short lock timeout and immediate rollback; never hold an application lock.
5. If protection cannot be established before upgrading, mark the existing-protected-database upgrade case blocked. A fresh protected database on the new release cannot substitute for that evidence.

## 3. Deploy the published edge and record baseline

1. Agent runs thelab's `eru upgrade --yes` only after publication succeeds. Check context selection so node upgrades do not target the stale `mbvlabs` context.
2. Run `eru -c nobucket apply --auto-upgrade-nodes`, preserving the step outcomes. Confirm the normal apply path re-applies `postgres-roles-v1@alpha_policy` without changing its configuration or manually invalidating the journal.
3. Run doctor and capture exact CLI/node versions, OS, PostgreSQL health, pending restarts and warning totals. Confirm `app` remains healthy with no data mutations.
4. Read actual memberships on the existing protected fixture: `pg_maintain`, `pg_signal_backend` and owner membership must be absent. Combine this with the recorded pre-state and normal apply evidence to decide the upgrade regression result.

## 4. SQL and protected maintenance on the existing server

1. Multiple SELECTs: check human results appear in statement order. Check JSON includes ordered `results`, each statement's columns/rows/tags, with the final result also at the backward-compatible top level. Check single-statement JSON remains compatible.
2. Mixed statements on a disposable unprotected fixture: CREATE/INSERT/UPDATE/SELECT return the correct command tags and SELECT results.
3. Transactional failure: insert a unique marker followed by a deliberate failing statement; verify the call fails and the marker is absent afterward. Do not use explicit transaction-control statements in this rollback fixture.
4. Protected `--write`: confirm JSON, no-input, no-TTY and `--yes` refuse without a mutation. Operator uses a fresh valid code for a multi-statement write and verifies its marker and tags.
5. As protected `eru_admin`, explicitly begin a read-write transaction and attempt `LOCK TABLE` on the disposable fixture. Expect permission denial, not merely a read-only-transaction refusal; rollback promptly. Recheck forbidden memberships.
6. Run vacuum, analyze and reindex against the protected fixture; follow detachable jobs to completion and confirm health/index validity. Check maintenance reports still read successfully.
7. Check protected `maint kill` refusal without a code; if exercising success, target only a disposable non-superuser session and obtain an operator code. Never kill application work.
8. Operator tests `psql --write` and `console --write` separately, each with a fresh code: inspect current/session role, write one fixture marker, read it back and exit normally. Check read-only modes still work without a code.
9. Operator unprotects only `alpha_policy`; verify owner/maintenance memberships return and disposable writes work. Protect it again and verify memberships are removed and lock denial returns.

## 5. Secrets gate matrix

All successful exports and login handoffs remain entirely in the operator's terminal or explicitly chosen private files. Log only results, factor/prompt behavior, modes and equality booleans—not document contents, hashes or passphrases.

1. Before setting a password, use the configured TOTP-only `nobucket` context: valid code succeeds; replay that code immediately and confirm rejection; test one invalid code without approaching lockout. Check protection failure counters and allow recovery from an unexpected lockout only through documented behavior.
2. On `mbvlabs`, first confirm its stale context will not reach the unrelated current server. Operator sets a master password and exercises password-only offline export, then one wrong password. Do not upgrade or apply this stale context.
3. Operator sets a password on `nobucket`. With both factors present, confirm the first prompt offers TOTP, a fresh valid code succeeds, and Enter selects password and succeeds. Wrong password must refuse.
4. Test offline password fallback using supported tunnel controls or an isolated context-directory copy with an unreachable endpoint; never alter the live context credentials or server firewall. Document the isolation and restore only changes made by the test. Verify discovery failure does not prevent password export and does not claim TOTP verification.
5. For each relevant context, confirm `--json`, `--no-input`, stdin without TTY and `--yes` refuse with `secret_gate_required`, before revealing secrets or contacting a node unnecessarily.
6. Operator exports and imports through normal `eru login` on another machine or a separate context-directory namespace, without overwriting/removing existing contexts. Verify privately that the password hash is carried over (report equality only) and that the imported password gate works. Never paste the transfer bundle into chat.
7. Malformed/unbounded hash rejection remains a local test, not a mutation of real credentials: run focused password/secrets-document tests, including zero rounds, excessive costs and invalid import with no context created.
8. Backup setup's replacement handoff gate is tested only after separate approval to attach a bucket. Test unattended refusal and interactive authorization at the actual replacement-handoff boundary, noting any setup changes that precede refusal. Do not treat export alone as evidence for this path.

## 6. Queries, top, CLI UX and idle tunnel

1. Generate bounded, recognizable work as a disposable application's role and more expensive work as an internal role in the same disposable database. Do not reset shared statistics or print role passwords.
2. Inspect all-session statistics to prove internal work actually ranks above application work. Then `queries --limit 1` must return application work, while `--all-sessions --limit 1` includes the higher-ranked internal statement.
3. Inspect top's busiest statements and activity, not just sessions: both hide internal roles by default; both include them with `--all-sessions`. Preserve non-secret evidence of role/query identity and rankings. If a live TUI is needed, hand the operator one command.
4. Check human `config get maintenance` renders meaningful TOML/key-value values and JSON renders an object, not a Go struct. Check `metrics show` names `--reveal` without revealing credentials.
5. Repack against the disposable database lacking the extension must refuse for the missing precondition before confirmation. Do not install extensions or enter deferred implementation work.
6. Inspect long error output at narrow terminal widths: line widths and borders remain intact. Exercise operator-machine `node unlock-ssh` refusal and its server-side/root guidance; do not unlock public SSH.
7. Leave the shared tunnel idle well beyond a keepalive interval with no polling against that context. Record idle interval, daemon state and subsequent handshake evidence; run SQL from an independent process and time recovery. Repeated idling can strengthen evidence but must not disguise an initial failure.
8. Test failure hints only with isolated operator-side tunnel state or supported controls, never by breaking the server's network. Distinguish absent daemon, non-answering daemon and genuine peer timeout. Mark unexercised hint branches local-test-only rather than manufacturing a live success.

## 7. Bucket-backed copy tests: approval gate

The current live server has no bucket, while `mbvlabs` has an existing recovery kit. Do not reuse that recovery namespace or overwrite its epoch/state for this server.

1. After existing-server checks, agree whether to attach a separately approved isolated test prefix to `nobucket`, or defer these tests to the fresh-server phase. Obtain explicit approval for bucket configuration and any needed resources; no bucket deletion is part of this plan.
2. If attaching backups now, exercise backup setup's replacement handoff gate and capture initial upload/verify evidence without printing the recovery kit. Ensure setup reaches a healthy backup state before continuing.
3. Take an encrypted dump of protected disposable `alpha_policy`, restore it to its inspection copy through normal commands, and verify the copy inherits protection and the live owner's policy.
4. Operator supplies a fresh TOTP code for a multi-statement write to the dump copy; verify it succeeds and no permission error remains. Check unattended write refusal too.
5. Promote only the disposable database's copy after the required confirmation/code, verify protection persists, and recheck maintenance and health. Never restore/promote `app`.

## 8. Fresh-server phase: separately arranged with the operator

Do not start until existing-server results have been reviewed and the operator explicitly approves the provider action and exact data impact. Preserve required recovery material privately. Existing contexts and bucket contents stay intact unless separately authorized.

1. Agree the fresh-server sequence and isolated bucket prefix. A bootstrap/upload test, lockdown-revert test, local-init test and recovery-refusal test may require separate fresh starts; do not imply they can all run on one already initialized server. Obtain approval for each reinstall/rebuild, not blanket implied permission.
2. For bucket-backed remote init, operator uses the desktop's provider key `~/.ssh/mbvlabs_prod_one` and the full agreed command. Observe successful initial upload and verification before completion; bucket epoch is 1 and first bundle name contains `-e1-`. Record bundle metadata and verification result, never contents.
3. Before creating any databases, run doctor: no false `host.disk_data` warning; report honest pass/skip and actual measured filesystem.
4. Create a disposable backed-up database. Follow stanza creation, check, first full and archiver state. Watch for exit-50 lock contention; an internally documented bounded wait is not the same as manually retrying a failed create. On failure capture the lock-holder evidence and stop.
5. For `db.write-access` awaiting-recovery refusal, use an explicitly approved normal restore/rebuild that actually provides a protected awaiting database. Test through the normal request path without creating fake records or manually repairing state. Verify refusal reason and no short-lived write role is created before recovery. If no rebuild provides the state, mark blocked.
6. On a separately approved fresh bootstrap, follow documented lockdown self-revert: intentionally interrupt operator-side WireGuard during commit-confirm, wait the documented revert window, verify public SSH restored and inspect revert evidence; then use documented resume. Coordinate provider access first. Do not perform this on the current serving server.
7. On a separately approved fresh server, operator follows documented `server init --local` from the provider console. Keep public SSH during the 30-minute pending window as documented, transfer the handoff privately with login, then agent verifies normal operator access confirms lockdown, plan/doctor health and public SSH closure. Do not capture the handoff.

## 9. macOS and release-tool evidence

1. After the target edge is published, provide one complete macOS install command, then inspect version/platform evidence the operator reports. Keep their existing contexts safe.
2. Exercise a genuine install-to-newer-edge upgrade across two published versions without manufacturing a release; record exact before/after versions and signature-verification results. If the macOS machine already runs the latest build and no newer publication is authorized, mark upgrade blocked rather than claiming a same-version no-op passed.
3. Run existing local promotion regression tests for root and per-major channels: missing object versus failed listing/read, malformed manifest, propagated tool errors, out-of-order publication, prerelease precedence and final-release protection. Record these explicitly as local logic evidence.
4. Actual tag ancestry checks, signing, immutable upload and root/line promotion in `publish-stable.yml` remain untested until the operator authorizes the first alpha tag. Never move pointers to create evidence.

## 10. Closeout and alpha decision

1. Run final health/plan checks; account for all disposable databases, copies, operator-side test namespaces and private files. Obtain approval before cleanup that removes contexts or bucket objects; do not silently drop evidence or leave changes undocumented.
2. Record every test with exact versions and distinguish pass, fail, expected refusal and blocked. Record old-policy upgrade evidence separately from fresh protection behavior.
3. Summarize remaining alpha requirements in three short lists: passed, failed, blocked. A failed security gate, failed normal policy upgrade, failed backup/recovery path or unresolved fresh-init failure is a blocker, not a warning to waive silently.
4. Ask before committing testing documentation or any diagnosed code fixes. Any fix requires verification and an authorized publish/redeploy before the affected live test is rerun. No alpha branch/tag action is part of this plan's execution authorization.
