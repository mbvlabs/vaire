# Live VPS qualification — edge

Prepared 2026-11-17. **Plan only; no VPS execution authorized or performed.**

## Goal and candidate

Qualify the actual signed edge release on the configured VPS, from installation
through recovery, without using real application data. Maximize assistant-run
work; consolidate human access, approvals, secrets and interactive checks at the
end of this plan.

- Checkout inspected: `010fdcb704880108f31d4b5f52196d843a1feeb8`.
- Public edge manifest observed: `edge-010fdcb70488`, **linux/amd64 only**.
- Published artifact SHA-256:
  `d1a89e7e27e5a11fcb47feae40d2eb27ecdc5397be72069560f3c5696fd47c10`.
- Immutable release root:
  `https://vaire.mbvlabs.com/releases/edge-010fdcb70488`.
- Fetching this manifest did **not** verify its signature or installed binaries.
  Re-resolve edge at execution admission. If it changes, explicitly select and
  record the new candidate or retain this immutable version; do not mix results.
- One VPS qualifies its actual OS/architecture only. Debian 13 and Ubuntu 26.04,
  other architectures, and upgrades from older installations need separate runs.

Historical Phase 0–5 evidence and recent unsigned Debian extension-lab passes
inform these cases; neither is a pass for this signed release. The current edge
workflow runs selected Go packages and mocked release tests, not this live suite.

## Execution model and safety

**Order:** local preparation → admission/bootstrap → activity gate → no-bucket
pass → bucket pass → newer-feature qualification → soak → optional disruptive
recovery → human acceptance → approved cleanup/final report.

Human tasks are listed last for convenience, but prerequisites cannot actually
be postponed until after dependent tests. I will run everything eligible, then
return one consolidated unblock list rather than asking you at every command.

Use one operator workspace and one execution owner. Before each mutation,
explicitly select the approved workspace, verify the node, inventory existing
databases and confirm fixture ownership. Do not reuse historical fixture names.
Proposed fixture prefix: `qual_`, subject to absence/name validation. Use a small
primary, an unrelated control database and temporary copies/followers in
**serial batches** rather than keeping the entire matrix resident.

Approval of the plan is not approval to execute every destructive test. Seek
one bounded authorization naming the host, fixture names, operations, restart/
outage window, load budgets and cleanup scope. Within that scope, I can supply
the exact CLI tokens myself; outside it, stop and ask. `--yes` is not a token.
Never use a broad server restart token in place of enumerated fixture tokens.

Stop the affected lane on:

- Unexpected host/workspace, pre-existing target, or unrelated data/service effect.
- Signature/checksum failure, secrets exposure, or unknown mutation outcome.
- Exhausted memory/disk/WAL/time/load budget, failed control-database probes,
  or an unapproved restart, privilege escalation or network change.
- Activity loss/unavailability that invalidates the evidence gate. Do not reset
  history, retry a mutation merely to obtain evidence, or bypass a protection gate.

Record state and preserve evidence before proposing repair. Never corrupt records,
edit generated configs/journals, use `ALTER SYSTEM`, install test fault hooks,
fill the disk, force OOM, or timing-kill an adoption boundary on this VPS.

## Evidence and verdicts

Create an owner-only run directory under
`specs/evidence/vps-edge/<UTC-run-id>/` for durable **sanitized** results. Never
save secrets, workspace files, bundles containing recovery credentials, raw
customer logs, SQL payloads or full credential-bearing command responses here.
Keep secret material in human-controlled storage, outside the repository.

For every case record: ID, candidate/operator/node identity, UTC times, target,
authorization/bounds, redacted command, exit code and JSON error/warnings, job/
operation/request IDs, independent SQL/runtime observations, control probe
results, elapsed time, cleanup and remaining resources.

Verdicts: **PASS / FAIL / BLOCKED / INCONCLUSIVE / NOT RUN**. A skipped job is not
a pass; a successful CLI response alone is insufficient. A missing measurement
is unknown, not zero. Preserve failures even after a successful retry.

Discover commands/flags/tokens from the installed candidate:

```sh
vaire version
vaire commands --json
vaire commands --check-meta
vaire commands --check-steps
vaire guide agents
```

Use `--workspace <approved-name>` and structured output for operational calls.
Use dry-run only where discovery advertises support. Credential-generating
responses need an agreed secure handoff, not ordinary captured stdout.
The command families below are case definitions, not an unattended shell script.

## A. Assistant-run preparation — no VPS needed

| ID | Work | Pass evidence |
| --- | --- | --- |
| A01 | Record clean/dirty checkout, commit, candidate manifest, platform support and release workflow. | Candidate pinned; no inference that published means verified/deployed. Unsupported operator/VPS platforms block installation. |
| A02 | Run `go test -count=1 ./...`, `go vet ./...`, `just lint`, `bash tests/versioned-installer.sh`, `bash tests/release-platforms.sh`; record tool versions. Run targeted race tests where supported, separating tooling/resource failures from defects. | Exact commands and results retained; do not assume edge CI ran the full suite. No release publishing or valuable `dist/` overwrite. |
| A03 | Verify actual installer/signature/checksum flow in an isolated install directory, without auto-init or replacing the normal CLI; inspect version and command metadata. | Real signed artifact verifies and matches candidate/checksum. Cover the current single-platform manifest, repeat install and clear unsupported-platform failure. Use mocked/unit tests for tampered artifacts, never weaken live verification. |
| A04 | Build case ledger from actual command discovery, separating implemented commands from stubs. Resolve fixture SQL, load generator, secure output handling and bounded probe procedure. | No guessed flags or undocumented commands. Planned source fixtures use synthetic trusted data. |

## B. Assistant-run VPS baseline — after admission/bootstrap

| ID | Work | Pass evidence |
| --- | --- | --- |
| B01 | Read-only host preflight: OS/arch, CPU/RAM/disk/free space, time sync, package sources/locks, hostname/DNS, provider and host firewall visibility, existing PostgreSQL/services. | Supported clean target, compatible network, measured budgets and no conflicting installation; otherwise block bootstrap, do not “clean up” an unknown host. |
| B02 | Observe approved `server init` or human bootstrap; verify operator/node release identity, workspace and node host key. Run status, doctor/security, plan, config validation and apply when approved. | Matching signed builds; expected services healthy; expected no-offsite-backup warning only; second apply converges without unrelated changes/restarts. `--resume` completes a naturally interrupted init; do not manufacture an unsafe failure. |
| B03 | Inspect sshd/UFW/systemd/package holds, listeners, file permission metadata and time sync without reading secret files. Probe from outside WireGuard as well as through it. | Public SSH closed; authorized WG SSH works; root/password login disabled; raw PostgreSQL socket-only; pooler public defaults deny; only intended 80/443/WG services public. Check IPv4 and IPv6 where configured. Provider firewall and UFW both accounted for. |
| B04 | Check DNS-only resolution, HTTP ACME challenge handling, certificate SAN/chain/expiry, cert timer, nginx routing and strict TLS clients. | Verified TLS succeeds on 5432/6432; PG17+ direct TLS works on 443 for allowed clients; wrong host/untrusted certificate fail. Normal PostgreSQL SSLRequest on 443 fails as documented; unrelated HTTPS paths are not an admin UI. Self-signed mode, if chosen, is explicitly unverified, not equivalent coverage. |
| B05 | Exercise workspace selection/precedence in isolated operator state, concurrent read commands, tunnel status/start/reuse/idle exit and loopback-only forwarding. Test stale connection/reconnect safely. | No cross-workspace targeting, concurrent commands share one tunnel, clean structured failures and no public local bind. Verify idle exit after its documented interval. Workspace removal/address changes only on disposable operator copies. |

## C. Activity/diagnostics gate — first live mutation evidence

AC-01 is a deployment gate in the existing run sheet. Perform the smallest
approved fixture mutation to establish it before broad qualification. A healthy
`activity health` response alone does not pass this gate.

| ID | Work | Pass evidence |
| --- | --- | --- |
| C01 | Correlate one success, one safe validation/confirmation refusal and one detachable job across activity list/show/health and job follow. | Correct operation/request/job relationships, bounded metadata, observed start/terminal outcome where applicable; refusal behavior matches the candidate contract. No fabricated success from an incomplete event pair. |
| C02 | Exercise filters, time bounds, pagination/cursor continuation and a deliberately small result limit. Collect and offline-inspect diagnostics with the same filters. | Coverage/truncation/continuation honest; no duplicate/missing evidence hidden as complete. Bundle works without node access and excludes credentials, SQL text, rows, raw logs and config bodies. Use synthetic recognizable canaries, never actual secrets. |
| C03 | Test missing/malformed incident input locally using harmless fixtures; inspect available activity-loss/rotation unit coverage. | Clear errors and bounded handling; evidence treated as data. Forced live history corruption/permission failure/rotation gaps remain BLOCKED unless a supported safe mechanism is approved. |

Any AC-01 gaps require your explicit named-gap acceptance before proceeding;
otherwise stop after preparation/baseline and report the blocker.

## D. Assistant-run no-bucket functional pass

Run this before backup setup; do not remove an existing bucket to obtain it.
Skip/block no-bucket variants if the server already has backups.

| ID | Work | Pass evidence |
| --- | --- | --- |
| D01 | Create synthetic primary/control databases; validate names/duplicates/invalid sizes and a safely chosen over-budget request. | Own clusters/socket dirs/cgroups/roles/routes; no effects on refused requests; actionable memory-budget hint; no automatic downsize or overcommit. Credentials handed off securely once. |
| D02 | Seed tables, schemas, sequences, indexes, views and representative rows as owner/migrate; exercise app, read-only and operator identities via both poolers and direct TLS. | App DML but no schema destruction; RO cannot write; migrate works on session 5432 and is refused on transaction pooler; operator reads by default and owner semantics with unprotected `--write`; default grants/ownership correct, cross-database isolation holds. `db access show` reports measured installed/pending/unknown coverage honestly. |
| D03 | SQL multi-statement ordered results, empty/null/error cases, JSON/agent/jq/stream output and missing input/workspace failures. | Correct envelope/exit/warnings; no hang on an unattended prompt. Safe missing/wrong confirmation and `--yes` tests cause no mutation. |
| D04 | Tune show/explain; budget-safe resize, defer restart, exact target restart and CPU-only resize if supported. Keep a continuous control-database probe and record service generations/settings. | Requested versus effective limits distinguished; reload/deferred change restarts nothing; confirmed restart affects only target; IOWeight “not enforced” is honest; unknown CPU is not unlimited. No overcommit without separate approval. |
| D05 | Fork without backups; basebackup follower; bounded source writes, follower reads/catch-up; unfollow with approved token. | Actual no-bucket copy path; row/sequence checks; follower read-only via aliases; unfollow independent with new roles/credentials, source credentials retired on copy, slot removed and source unchanged. Copy protection does not silently inherit. |
| D06 | Follower guards: follow-of-follower, source drop, follower write/backup/restore/fork/maintenance/rotation. | Intended guard wins with applicable confirmation supplied; no partial state. Aggregate operations skip followers correctly. Preserve source/control availability. |
| D07 | Approved bounded follower stop/restart and CPU spare/quota variants; inspect lag, slot/WAL and source/self-known facts. | Stop/unknown facts not reported as zero; bounded catch-up after resume; CPU policy measured on actual service; quota not advertised as spare capacity or an I/O limit. No forced slot loss. |
| D08 | Safe status/watch/top/queries/log inspection, database-specific doctor and logs. | Observations agree with independent service/SQL checks; stopped states remain identifiable. Use only synthetic data and curated output; query/log text is potentially sensitive. Stats reset and backend termination require named authorization. |

## E. Assistant-run bucket, restore and recovery-kit pass

Requires a dedicated test bucket/prefix, credentials entered by you, approved
storage/egress cost and recovery-kit custody. Never reuse a production prefix or
shorten a real retention window.

| ID | Work | Pass evidence |
| --- | --- | --- |
| E01 | Observe backup setup; full/diff/incr backups and WAL progress per primary; inspect timers/staggering and verify. | Bucket reachable from node; WAL enabled without cluster restart; first full/timers succeed; verify reads/checks actual objects; followers have no independent stanza. Recovery kit refreshed by you. |
| E02 | Synthetic known-value timeline: insert marker A, backup/archive, record UTC target/LSN, then marker B/change A. Restore at a demonstrably recoverable target, latest and selected set in serial runs. | Restored SQL contents match intended history, not just row estimates; live/control continue serving during copy; no simultaneous open restore for same DB. Wait for measured WAL coverage, not a guessed sleep. |
| E03 | Inspect/abort a copy; separately promote, test original URLs/roles, undo, then approved cleanup. | Expected data/timeline at each stage, target-only measured downtime, full backup after cutover, old copy retained until cleanup, no control restart. New restore after undo follows the intended timeline. |
| E04 | Dump to local secure destination/bucket, list/get, dump restore, promote and drill. | Real data/checksum/sequence checks; decrypted archive handled securely; dump promote has new stanza/system identity as expected; drill restores and runs checks, records result and removes owned temporary cluster. |
| E05 | Stanza-seeded follow/fork/unfollow; source restore with follower, if separately approved. | Actual stanza path (fallback is not a pass); independent stanza/first full on unfollow; divergence explicitly warned after source timeline change; approved drop/re-follow restores streaming. |
| E06 | Detach a long job by ending the client only; list/follow from a new client. Exercise competing heavy-I/O work and node mutation lock safely. | Node job survives; no duplicate mutation, truthful completed/failed/skipped result; correct contention behavior. Never kill a lock holder. |
| E07 | Server backup now/list/verify/export to a human-controlled secure destination. Drop only an approved fixture after backup. | Master bundle actually verifies, secrets never captured; dropped database removed from routes/units but stanza/window retained and final dump behavior correct. Long-term expiry is NOT RUN unless observed; don't alter clocks or production retention. |

## F. Assistant-run newest-feature qualification

Prioritize these gaps over repeating historical happy paths.

| ID | Work | Pass evidence |
| --- | --- | --- |
| F01 | Built-in vector alias/add/remove; real custom extension from an already trusted repo (e.g. compatible pg_stat_kcache); safe absent package/preinstalled-file case. | Durable descriptor, observed package/control/library/version; alias normalizes; package retained/held on removal; actionable missing prerequisite; no repo/key addition or build from source. |
| F02 | Preload extension with no token, wrong token, exact target token; source with follower missing/all tokens; direct follower change. | Unauthorized requests have zero package/config/SQL/restart effects; follower-first config/restart, primary-only SQL, effective preload before CREATE; unrelated control untouched. Plain config/apply does not bypass authorization. |
| F03 | Extension dependency preview/removal/cascade on disposable objects; repeat successful intent. | Bounded preview/count/truncation; no implicit cascade; approved cascade only; truthful pending/retry outcomes. Protected-removal refusal is repeated after human protection setup. |
| F04 | Bounded pg_cron synthetic job; fork/follow/unfollow/drill as supported; unknown/disabled copy-safety custom definition. | Copied jobs cannot launch from first startup through settlement; inactive/retargeted copy and unchanged source; disabling settings durable; unknown safety refuses unsafe copies. No unsafe real scheduled effects. |
| F05 | Back up extension-bearing data, remove extension, restore the historical point; inspect stanza-bound requirements and explicit absent/empty/populated recovery declarations using supported inputs. | Required libraries resolved before start; missing prerequisites fail safely; old history not associated merely by reused database name. Rebuild variant belongs to H below. |
| F06 | Maintenance vacuum/analyze/reindex, reports, exact one-table pgstattuple, eligible manual repack and opt-in scheduled repack. | Accurate job/lock/window/result facts, contents unchanged, control available; exact report is full measurement not estimate/reclaim guarantee. Repack has measured temporary space, no backend killing, truthful `completion_unknown` rather than false success. Restore original auto-maintenance settings. |
| F07 | External copy from a trusted synthetic PostgreSQL source reachable **from the VPS**; use stdin URL and custom CA if needed. Include schemas/sequence/large object/extension-supported data as applicable. | Verified TLS; source unchanged apart from bounded export load/locks; consistent snapshot and correct contents/ownership/grants/sequences; new ordinary target, new credentials; no imported source roles/settings; backup readiness separately proven. |
| F08 | External copy dry-run, existing target, invalid/unreachable source and certificate mismatch; detach/re-follow. | Dry-run does not contact source/create secrets/job; existing target untouched; no plaintext fallback or credential leak to argv/logs/activity; failed target retained and reported incomplete, never silently adopted/replayed by apply; job follow never reprints passwords. No deterministic mid-import failure means that subcase stays BLOCKED. |
| F09 | Where an approved older signed fixture provides a genuine pending owner-access transition, preview it and test missing/wrong authorization before exact `<db>/access` apply during migration quiescence. | Restart tokens/`--yes` do not authorize access expansion; no premature grants. Eligible owner-owned schemas/objects and future owner defaults behave as specified; other creators remain uncovered. Incomplete materialisations cannot be adopted by ordinary apply. Fresh database creation does not prove this upgrade case; without a suitable fixture, mark BLOCKED. |

Use the existing X1–X11/follower cases in
[`docs/RUN-phase-6-7-live.md`](../docs/RUN-phase-6-7-live.md) as detailed
assertions, not as evidence of completion. Unsupported deterministic failure
boundaries remain blocked; do not invent a live fault mechanism.

## G. Assistant-run soak, upgrades and closeout

| ID | Work | Pass evidence |
| --- | --- | --- |
| G01 | Approved bounded synthetic read/write workload while scheduled work runs; observe memory/CPU/disk/WAL, connections, service restarts, TLS, metrics and control probes. | No unexplained errors/leaks/restarts; actual observed timers succeed. Prefer an overnight/24-hour observation if practical; otherwise record shorter coverage and missed schedules. Never promise throughput without an agreed workload/SLO. |
| G02 | Inspect metrics endpoints/targets and database add/drop lifecycle; test unauthenticated and wrong-auth access, then authorized scrape through a human-configured client. | Auth required, correct exporter/DB discovery, no public exporter listener or leaked password; disabled metrics behavior only if approved. Rotation is a separate human-coordinated case. |
| G03 | Re-run install/upgrade of same pinned version, plan/apply/doctor, activity/diagnostics and backup verification. If an approved older signed installation exists, test mismatch refusal then normal signed node upgrade. | Idempotent no-op where appropriate, mismatch exit 10, verified matching node, pending restarts require tokens and no silent state-format loss. Fresh installation alone does not qualify an upgrade path or rollback. |
| G04 | Stop generators; inventory jobs, databases/copies, slots, stanzas, units/timers, settings, packages, network rules, local tunnels and test artifacts. Execute only approved cleanup. | Original settings restored through supported interfaces; no orphan slots/routes/temp clusters; expected retained backup objects/packages documented. Final doctor/status/plan and latest backup good; every case has a verdict. |

## H. Optional disruptive tests — assistant-executed only after separate approval

These are not part of routine testing on a configured live VPS.

- **Commit-confirm rollback:** dedicated disposable bootstrap host and provider
  console available. Induce only an agreed supported loss of the WG confirmation;
  independently verify timed restoration of SSH/firewall access. A successful
  bootstrap does not prove rollback. Leave BLOCKED without safe console/recovery.
- **Break-glass:** you open provider console; run the documented node unlock flow,
  verify its bounded public-SSH window and expiry, then WG-only hardening again.
  Do not leave a bypass or capture the generated password.
- **Reboot:** after verified backups and an approved outage, reboot once; prove
  service/timer/cgroup/firewall persistence, data checks, tunnel recovery and
  archived WAL continuity.
- **Full server loss/rebuild:** second disposable VPS, separately approved bucket
  ownership/fencing and DNS/IP changes. You power off old master first. Restore
  from verified master bundle, observe awaiting-recovery, refuse follower-first
  recovery, then recover source-first/all and prove rows/roles/extensions,
  protected DB state, follower streaming, TLS, workspace continuity and backups.
  Never run both masters as writers. Old-node fencing assertions need an agreed
  safe observation method; never reclaim/unfence merely to continue a test.
- **Credential/metrics rotation and older-build upgrade/rollback:** coordinate
  dependent clients and signed-build availability; prove old access stops and
  new access works. No ad hoc binary swap or assumption alpha formats downgrade.
- **Excluded from this VPS:** filesystem/record corruption, disk-full/OOM,
  deliberately lost replication slots, host-key substitution, signature bypass,
  timing-only crash tests and unsupported hooks. Cover with source regressions or
  a separately designed disposable lab; report live coverage gaps honestly.

## Acceptance and deliverable

Mandatory: A–C gates; baseline security/TLS; role/routing/isolation; no unintended
restart or data loss; budget/confirmation refusal; verified offsite backup and
SQL-verified restore; newer features selected for use; final healthy inventory.
If offsite recovery is not configured, report only the no-bucket subset as
qualified—**not production-recovery readiness**.

Optional/disruptive cases can remain explicitly deferred, but any such case
needed for the intended use blocks that use. A privacy/security failure, false
success, unrecoverable backup, wrong-target mutation or unrelated outage blocks
acceptance. Failures in optional features block those features, not silently
disappear from the report.

Deliver a concise pass/fail matrix, reproducible defects, sanitized evidence
links, measured limits/downtime, remaining resources and recommended go/no-go.
No production feature adoption or documentation edits are implied by test passes.
Updating `docs/MANUAL-TESTING.md` requires your separate approval.

## Your involvement — consolidated checklist, prerequisites first

### 1. One-time unblock before VPS execution

- [ ] Confirm this is a disposable qualification VPS, or name exactly what
  existing data/services must be preserved. Provide OS/architecture, public
  address, intended hostname/domain and workspace name; no passwords in chat.
- [ ] Provide an approved access path (existing authenticated workspace, or
  bootstrap session). Configure DNS/provider firewall; confirm provider console
  access works before public SSH lockdown. I can check observations afterwards.
- [ ] Approve named fixture mutations, package installs/sudo, fixture-only
  restarts/cutovers/drops/cleanup and bounded workloads. Resolve actual RAM/disk
  reserve, maximum rows/bytes/runtime/WAL, outage limit and cost from preflight.
  Unknown budgets block the affected test, not invite guessed limits.
- [ ] Perform interactive bootstrap answers and save the one-time secrets/
  transfer bundle/passphrase in your password manager. Approve a secure
  credential handoff for fixtures, forks, unfollows and copies; if none is
  available, you run those credential-generating steps yourself.
- [ ] For E/F bucket tests, supply a dedicated prefix and limited credentials
  through a secure prompt/input path, approve cost, and save the refreshed
  recovery kit. For external copy, approve a trusted disposable source and its
  VPS reachability/CA/authentication. I can operate synthetic fixtures afterwards.

### 2. Human-only checks after the unprotected assistant pass

- [ ] `protect init`: enroll/verify authenticator at your terminal. Select only
  approved fixture databases; don't accidentally protect all fixtures by default.
- [ ] I then run the negative agent checks: protected reads work, but writes,
  drop/shrink/rotation, removal/repack and cutover are refused as specified even
  with tokens/`--yes`; normal config/apply cannot remove protection.
- [ ] You test one authorized protected SQL write, psql/console write session,
  TOTP replay/wrong-code refusal, and unprotect/reprotect. Five-code/15-minute
  lockout is optional and separately agreed; don't lock out real work.
  I verify resulting data/roles and expired temporary-write access without
  reading authenticator state. Never share a code or lift protection as an agent
  workaround.
- [ ] Set/test the human secrets-export password or TOTP gate; save/export the
  refreshed handoff and test login in a second isolated operator profile.
  I can verify noninteractive refusal and file permission metadata, not read
  the secret document.
- [ ] Try the interactive init/login/psql/console/top/watch experiences, copy's
  masked interview if desired, and detach/re-attach usability. Confirm prompts,
  messages and one-time credential handling are understandable.
- [ ] If desired, configure a real metrics scraper using the stored password;
  coordinate any rotation. Approve and participate in H console/power/DNS/
  replacement-host actions separately.

### 3. Final decision

- [ ] Review defects and named evidence gaps; choose go/no-go and any feature
  exclusions. Confirm cleanup scope versus fixtures/evidence to retain.
- [ ] Approve any durable live-evidence update under `docs/` separately.

## Source grounding and known documentation gaps

Consulted README, command tree/agent guide, release scripts/workflows, existing
live run sheet, manual evidence and extension finishing/evidence records.
`README.md` links `docs/ACTIVITY.md`, `docs/RUN-external-database-copy.md` and
`docs/REVIEW-external-database-copy.md`, but those files are absent in this
checkout. Do not treat the missing review/run sheets as completed acceptance;
validate those features against installed metadata, implementation and actual
observations. This plan does not edit the protected `docs/` directory.
