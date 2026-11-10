# Phase 6 live run sheet (LV-01)

**Status: preparation complete, execution blocked. No live permission is present
and no command in this document has been run.** This run sheet is source-based
documentation for the follower feature reviewed in
[REVIEW-phase-6-final.md](REVIEW-phase-6-final.md). It does not qualify the
product, does not replace [MANUAL-TESTING.md](MANUAL-TESTING.md), and does not
authorize publication, deployment, a bucket switch, a rebuild or any fixture.

Every checklist case in the Phase 6 section of
[MANUAL-TESTING.md](MANUAL-TESTING.md) maps to a bounded case below or to an
explicit `BLOCKED` case. Missing runtime inventory below is a readiness gap for
execution, not a failed product and not a pass. In short: LV-01 documentation
coverage is complete with explicit gaps; live execution readiness is blocked on
the permission and inventory gate.

## Overview for approvers

The live qualification has two serial passes:

1. **Pass A, no bucket.** A disposable source is followed with
   `pg_basebackup`. This proves the no-backup path, alias routing on 6432, 443
   and the session pooler on 5432, guards, protection independence, lag and slot
   visibility, stopped and paused states, unfollow, and resize ordering and
   refusals.
2. **Pass B, bucket.** A disposable source with a stanza is followed from the
   stanza, unfollowed with its own stanza and first full, then used for source
   restore divergence and a rebuild/recovery cycle. This pass needs the bucket,
   a separately approved server rebuild, and the human desktop root key.

The passes are strictly serial. Pass B does not start until Pass A's evidence is
recorded and separately approved. No case may run in parallel with another
live case. Every case stops at the first unexpected disk, WAL, slot, restart or
data effect.

**Outcomes:** a bounded live record for the no-bucket and stanza follow paths,
both alias ports and the session pooler, guards and protection independence,
unknown-vs-zero diagnostics, stopped/paused states, unfollow and access
retirement, resize ordering/refusal/defer, source restore divergence, and
rebuild recovery. Cases marked blocked produce source-regression evidence and a
named live gap, not a pass.

Before either pass, a named-scope permission and read-only inventory must be
recorded. Activity qualification (AC-01) must be passed or explicitly accepted
in the main testing record before any follower deployment. Publication (PUB-01)
requires its own permission and this documented run-sheet/gap map; publishing an
edge artifact does not satisfy the activity gate or authorize deployment.

The run sheet deliberately has no built-in byte, row, time or size budgets:
those values are recorded in the approval table below. It also has no live
target, version, headroom figure or fixture existence claim. Those are unknown
until the approved inventory is taken.

## Execution contract

These rules bind the agent or tester executing the run sheet.

- **Templates are not executable.** Every `<...>` is unresolved. Replace it
  from the approved inventory, write the resolved command into the evidence
  log, then run it. Never paste an unresolved template into a terminal, and
  never hand one to a user as a command.
- **One live owner.** Only one operator or agent drives a pass. One case runs
  at a time. The source database is the only write target, except the explicit
  unfollow migration and recovery cases.
- **No repair, no hooks, no corruption.** Do not add hooks, patch generated
  configuration, edit `records.json`, corrupt a file, `chmod` a directory,
  use `ALTER SYSTEM`, or use a timing-only kill as proof of an internal
  boundary. A fault that cannot be proved to have happened is `INCONCLUSIVE`,
  not a pass.
- **No hidden budgets.** Every write, WAL, time, size, disk and stop bound is a
  named approval value. If a value is absent, the case is `BLOCKED`; do not
  pick a "reasonable" substitute.
- **Stop means stop.** On a bound, an unexpected effect on an unrelated slot or
  database, disk below the approved reserve, a failed job with unknown outcome,
  or a credential exposure, stop the case, preserve non-secret evidence, and
  report the state. Do not heal the server or retry destructively.
- **Evidence.** Record UTC start/end, context, server identity, binary
  version/commit/channel, exact resolved command, exit status, job ID, JSON or
  text observation, and the approved evidence path. Never record passwords,
  tokens, secret handoffs or full credential blocks. Durable results belong in
  the repository's [MANUAL-TESTING.md](MANUAL-TESTING.md), not in a temporary
  narrative report.
- **Status vocabulary.** `READY FOR APPROVAL` means the procedure is complete
  but not authorized. `BLOCKED` means no supported live mechanism exists or a
  required approval is missing. `NOT RUN` means exactly that. `PASS`, `FAIL`,
  `INCONCLUSIVE` are only for an executed case.

## 1. Readiness gate: permission and inventory

### 1.1 Named-scope permission record

Complete every field before any live command. A missing field blocks the whole
run, not just the case that needs it.

| Field | Required value | Recorded value |
| --- | --- | --- |
| Permission owner and UTC | Person authorizing the live scope | |
| Context A | Exact `nobucket`-style context name; verified current | |
| Context B | Exact bucket-backed context name; verified current | |
| Target identity A | Verified hostname, public address and node name for Pass A | |
| Target identity B | Verified hostname, public address and node name for Pass B | |
| Binary identity | Signed release version, commit and channel for CLI and node | |
| Activity gate | AC-01 pass or explicitly accepted gap in main testing record | |
| Plan/doctor baseline | Pending steps, restarts, doctor counts and warnings recorded | |
| Backup/recovery inventory | Stanzas, restore windows, master bundles and recovery options recorded | |
| Protection inventory | Protection flags, factor state and lockout state recorded | |
| Fixture names | `FIX-SRC`, `FIX-FOL`, `FIX-AUX`, `FIX-PROBE` verified absent | |
| Fixture sizes | `SIZE-SRC`, `SIZE-FOLLOWER` measured against the budget | |
| Write bounds | `CAP-WRITE-ROWS`, `CAP-WRITE-BATCHES`, `CAP-WRITE-SECONDS` | |
| WAL bounds | `CAP-WAL-BYTES`, `CAP-WAL-SECONDS` | |
| Slot cap | `CAP-SLOT` for the temporary `max_slot_wal_keep_size` | |
| Disk reserve | `DISK-RESERVE` free bytes that must remain at all times | |
| Stop duration | `CAP-STOP-SECONDS` for a stopped follower | |
| Resize sizes | `SIZE-GROW`, `SIZE-GROW2`, `SIZE-SHRINK`, `SIZE-FLOOR-PROBE` | |
| Restore target | `RESTORE-TARGET` point in time for source divergence | |
| Rebuild window | `REBUILD-WINDOW` and accepted data-loss boundary | |
| Bucket | Exact endpoint, bucket and prefix, verified by an approved read | |
| Human steps | TOTP, sudo, root-key rebuild and interactive config edit approved | |
| Destructive fixture actions | B8 slot drop and any fixture drop/rebuild explicitly approved | |
| Cleanup | Explicit permission to drop the named fixtures and retained resources | |
| Evidence path | Owner-only directory outside the repository, server-side path known | |

### 1.2 Read-only inventory

These commands are read-only but still require the named permission. They are
templates: fill `<ctx>` and `<source>` from the approved record. Run them one at
a time and store the non-secret JSON.

```sh
# TEMPLATE ONLY — resolve <ctx> before running.
vaire version --json
vaire -c <ctx> status --json
vaire -c <ctx> db list --json
vaire -c <ctx> protect status --json
vaire -c <ctx> plan --json
vaire -c <ctx> doctor --json
vaire -c <ctx> backup list --json
vaire -c <ctx> server backup list --json
vaire -c <ctx> restore status --json
vaire -c <ctx> tune show --json
vaire -c <ctx> tune explain --db <source> --json
vaire -c <ctx> activity health --json
vaire -c <ctx> config get clusters.<source>.settings.max_slot_wal_keep_size
vaire -c <ctx> config get postgres.settings.max_slot_wal_keep_size
vaire -c <ctx> sql --db <source> "show wal_level; show max_slot_wal_keep_size; show hot_standby;"
vaire -c <ctx> sql --db <source> "select slot_name, active, wal_status, restart_lsn, safe_wal_size from pg_replication_slots order by slot_name;"
vaire -c <ctx> ssh --command "hostname; id; cat /etc/os-release; df -B1 <data-root>; free -b; systemctl --version | head -1"
```

Record:

- `status.vaire_version`, context identity and the session's successful version
  handshake. Compare with `vaire version --json` and the signed release. The OS
  must be Debian 13 or Ubuntu 26.04 LTS with PostgreSQL 18; anything else stops
  the run as unsupported.
- `status.backups_configured`, bucket reachability and the exact bucket/prefix.
- Every database's name, size, protection, stanza, follows and state.
- `plan` counts and any pending restart, especially `wal_level`.
- Doctor pass/warn/fail/skip counts and any follower check already present.
- Backup/restore state and the recovery options before any rebuild.
- `tune show` budget fields and `df -B1` free bytes at the data root.
- The exact current effective `wal_level`, `max_slot_wal_keep_size`,
  `hot_standby`, and every `vaire_*` slot.
- Whether the cluster key `clusters.<source>.settings.max_slot_wal_keep_size`
  exists and its exact string, or the exact error that says it is absent; also
  the inherited `postgres.settings` value, if any.

If the target identity does not match the approved record, stop. If the context
is not the one named, stop. Do not "fix" a context or repoint it.

### 1.3 Derived values

Resolve and record these from the inventory. Do not guess.

| ID | Meaning | Resolution rule | Value |
| --- | --- | --- | --- |
| `FIX-SRC` | Source fixture database | Proposed name below; confirmed absent | |
| `FIX-FOL` | Follower fixture database | Proposed name below; confirmed absent | |
| `FIX-AUX` | Unrelated fixture for convergence checks | Proposed name below; confirmed absent | |
| `FIX-PROBE` | Fresh-floor probe follower | Proposed name below; confirmed absent | |
| `SIZE-SRC` | Source size | Must fit `tune show` budget; no default `large` | |
| `SIZE-FOLLOWER` | Follower size | Must fit budget with the source; no default `large` | |
| `SIZE-GROW` | First growth | Larger than source, within budget | |
| `SIZE-GROW2` | Second growth | Larger again, within budget; else case blocked | |
| `SIZE-SHRINK` | Refusal probe | Smaller than source; else case blocked | |
| `SIZE-FLOOR-PROBE` | Fresh-floor probe | Approved size whose floors are below the source's running values; else case blocked | |
| `CAP-SLOT` | Temporary source slot cap | Small enough that approved WAL can exceed it, large enough to preserve approved headroom; exact PostgreSQL value | |
| `CAP-WAL-BYTES` | Maximum WAL generated | At most the approved free space minus `DISK-RESERVE`; no universal number | |
| `CAP-WAL-SECONDS` | Maximum WAL-generation time | Approved duration | |
| `CAP-WRITE-ROWS` | Rows per write batch | Approved count | |
| `PAYLOAD-BYTES` | Bytes per write row | Approved test-data size, not a resource budget | |
| `CAP-WRITE-BATCHES` | Maximum batches in a loop | Approved count | |
| `CAP-WRITE-SECONDS` | Maximum write-loop time | Approved duration | |
| `CAP-STOP-SECONDS` | Maximum stopped-follower time | Approved duration | |
| `DISK-RESERVE` | Free-space floor | Approved bytes that must remain | |
| `RESTORE-TARGET` | Source restore point | RFC 3339 time, inside the verified restore window | |
| `REBUILD-WINDOW` | Accepted rebuild downtime | Approved duration | |

### 1.4 Readiness verdict

**BLOCKED FOR EXECUTION.** No runtime inventory, named live approval, target
identity, headroom measurement, fixture confirmation, cap, version or bucket
verification has been supplied to this document. Do not execute Pass A or Pass
B until every permission and derived-value field is complete. This is a
documentation gap, not a product failure.

The activity prerequisite is a recorded AC-01 outcome from the main testing
record, not merely an `activity health` read. If AC-01 is not passed or its
gaps explicitly accepted there, follower deployment and both passes remain
blocked.

## 2. Proposed fixtures

These names are **planned, not created or verified**. Confirm each is absent
from `db list`, `pg_lsclusters`, the data root and `pg_replication_slots` before
creating it. If a name exists for any reason, choose another approved name; do
not adopt or delete the existing object.

| Fixture | Purpose | Planned form |
| --- | --- | --- |
| `p6lv1src` | Source database | Database with a small disposable table |
| `p6lv1fol` | Follower database | Follower of `p6lv1src` |
| `p6lv1aux` | Unrelated database | Convergence and aggregate-skip checks |
| `p6lv1probe` | Fresh-floor probe follower | Short-lived follower for the executable-hint case |
| `p6lv1wal` | Write table inside the source | Bounded write load and WAL generation |
| `p6lv1mig` | Migration marker inside the unfollowed copy | Rehearsal-only table/column change |

The historical `app`, `other` and `app_follower` names in
[MANUAL-TESTING.md](MANUAL-TESTING.md) are **not usable fixtures**. They may
belong to old evidence or an existing server. Do not mutate them, and do not
assume `large` is a safe size: it is only a size name, not a budget.

## 3. Pass A — no bucket, `pg_basebackup`

**Preconditions:** Pass A permission, complete inventory, AC-01 gate recorded,
`status.backups_configured` false, fixtures absent, `wal_level` verified, and an
approved write/WAL/disk budget. If `backups_configured` is true, stop: wrong
context.

### A0 — Upgrade and baseline

**Maps:** "The upgrade" except the final restart observation.

1. Confirm the CLI and node run the approved signed release. A version mismatch
   or an unverifiable binary is `BLOCKED`, not a reason to install an ad hoc
   build.
2. Run `plan` and record every pending step and restart. `wal_level` may report
   `restart_pending`; do not confirm it yet.
3. Run `apply` without restart confirmations. Record the `restart_pending`
   warning and the named confirm token.
4. Do not run `SHOW` or `tune explain` against a fixture that does not exist
   yet. A2 step 5 verifies the effective `wal_level` and slot cap on `FIX-SRC`
   after it is created.
5. The "followers stream before the restart" observation needs a source that
   existed before the `wal_level` restart and now has that restart pending.
   Fresh fixtures created after the upgrade do not have one. If the approved
   inventory has no such source, record that sub-case `BLOCKED` (or not
   applicable) and do not fabricate it. Do not reuse `app`/`other` for it.

**Expected:** plan is understood and apply does not restart anything without a
token; `wal_level` and slot cap are verified later on the actual fixture, not
assumed.
**Stop:** any unplanned restart, any change to a database not named in the
approved scope, or any restart token consumed without approval.
**Cleanup:** none.

### A1 — Fixtures and bounded write load

**Maps:** setup for "Follow under write load".

1. Create `FIX-SRC` and `FIX-AUX` at `SIZE-SRC`; record their generated
   credentials only in the approved secret handoff.
2. Create `p6lv1wal` and a few rows in `FIX-SRC`.
3. Start the bounded write load on `FIX-SRC`: batches of `CAP-WRITE-ROWS`,
   at most `CAP-WRITE-BATCHES` or `CAP-WRITE-SECONDS`, and stop immediately if
   free space falls below `DISK-RESERVE`.
4. Record the source's current LSN and the write-loop start/end UTC.

```sh
# TEMPLATE ONLY — resolve every <...> and record the resolved command first.
vaire -c <ctx> sql --db <source> --write "create table if not exists p6lv1wal (id bigint generated always as identity, payload text);"
vaire -c <ctx> sql --db <source> --write "insert into p6lv1wal (payload) select repeat('x', <PAYLOAD-BYTES>) from generate_series(1, <CAP-WRITE-ROWS>);"
vaire -c <ctx> sql --db <source> "select pg_current_wal_lsn()::text;"
```

**Expected:** both databases are healthy, the write load is bounded, and no
other database changed. A new database's first configuration restarts its own
new cluster; that is expected and is not an unplanned restart of an existing
database.
**Stop:** disk below reserve, unexpected existing-database change, or a write
failure that is not the intended case.
**Cleanup:** stop the loop. Keep the fixtures for the next case.

### A2 — Follow without backups

**Maps:** "Follow without backups"; the basic follow/stream/load observations
of "Follow under write load (from the stanza)" for the no-bucket path.

1. With the bounded write load running, run `db follow` for `FIX-FOL` at
   `SIZE-FOLLOWER`. A budget refusal is a valid separate result; do not add
   `--allow-overcommit` unless approved.
2. Record the job ID, `data.from` and the result. `from` must be `base_backup`
   because this context has no bucket.
3. Verify `db list` shows the follower row's STANZA cell as `follows
   FIX-SRC` with a measured lag suffix; `status --db FIX-FOL` reports the
   source and self sides; lag reaches zero under the bounded load. If lag does
   not reach zero, record the exact measured state; do not wait forever and do
   not call unknown zero.
4. Verify the source slot `vaire_<follower>` exists and is active, and the
   follower's cluster is in recovery.
5. If `status` or `plan` shows the source fixture has a pending `wal_level`
   restart, confirm it now with `apply --db <source> --confirm
   <source>/restart` and the approved token, while the follower streams. Verify
   `SHOW wal_level` is `replica`, `SHOW max_slot_wal_keep_size` matches the
   current `tune explain`, and the follower is still streaming after the
   restart. If no restart is pending for the fixture, record that the effective
   value was already verified and the pre-restart streaming sub-case from A0
   step 5 is not applicable or blocked.

```sh
# TEMPLATE ONLY.
vaire -c <ctx> db follow <source> --name <follower> --size <SIZE-FOLLOWER>
vaire -c <ctx> db list --json
vaire -c <ctx> status --db <follower> --json
vaire -c <ctx> sql --db <source> "select slot_name, active, wal_status, restart_lsn, safe_wal_size from pg_replication_slots where slot_name = 'vaire_<follower>';"
vaire -c <ctx> sql --db <follower> "select pg_is_in_recovery(), pg_get_wal_replay_pause_state();"
```

**Expected:** a `db-follow` job, `FIX-FOL` following `FIX-SRC`, streaming, lag
measured and zero when caught up, an active `vaire_<follower>` slot.
**Proves:** the no-bucket copy path and the basic streaming contract.
**Stop:** the job fails before the follower starts; record the rollback and do
not leave a half-created entry. If the entry remains, treat it as an unfinished
follow and do not run a general `apply`.
**Cleanup:** stop the write loop after the case.

### A3 — Node-side facts

**Maps:** the node inspection bullet of "Follow under write load".

With read-only sudo approval only, inspect:

- `/etc/postgresql/18/<follower>/conf.d/60-vaire-follower.conf` contains the
  hot-standby floors, `hot_standby = on`, `primary_conninfo` and
  `primary_slot_name`; without backups, `restore_command` is absent rather than
  assumed present.
- `postgresql.auto.conf` has no `restore_command`.
- `pg_stat_replication` on the source shows the follower with `state=streaming`.
- The slot name is `vaire_<follower>`, active.

```sh
# TEMPLATE ONLY — read-only sudo inspection; separate approval required.
vaire -c <ctx> ssh --command "sudo -n sed -n '1,200p' /etc/postgresql/18/<follower>/conf.d/60-vaire-follower.conf"
vaire -c <ctx> ssh --command "sudo -n grep -n 'restore_command' /var/lib/postgresql/18/<follower>/postgresql.auto.conf || true"
vaire -c <ctx> sql --db <source> "select application_name, state, replay_lag from pg_stat_replication order by backend_start desc;"
```

**Expected:** the file matches the documented follower configuration and the
slot is streaming.
**Stop:** a path, owner or setting differs from the documented contract; record
the diff and stop. Do not edit the file by hand.
**Cleanup:** none.

### A4 — Alias reads and write refusal on 6432, 443 and 5432

**Maps:** the alias bullet of "Follow under write load".

Use the fixture's source credentials from the approved secret handoff. Never
put a password in argv, a file tracked by git, or the evidence log. The
transaction pooler is 6432, direct TLS is 443 with `sslnegotiation=direct`, and
the session pooler is 5432.

1. Through 6432, connect as `<source>_ro` to database `<follower>` and read the
   newest rows. Then attempt an `insert`; it must fail with the standbys'
   read-only error.
2. Through 443 with `sslnegotiation=direct`, repeat the read and the refused
   write.
3. Through the session pooler on 5432, run `vaire sql --db <follower>` and read.
   Then run `vaire sql --db <follower> --write`; it must be refused with
   `is_follower` before any write.
4. Optionally, through `vaire tunnel --db <follower> --target postgres`, connect
   as `<source>_ro` and repeat the read.

```sh
# TEMPLATE ONLY — psql prompts for the secret; do not put it in argv.
psql "postgres://<source>_ro@<node-host>:6432/<follower>?sslmode=verify-full"
psql "postgres://<source>_ro@<node-host>:443/<follower>?sslmode=verify-full&sslnegotiation=direct"
vaire -c <ctx> sql --db <follower> "select count(*) from p6lv1wal;"
vaire -c <ctx> sql --db <follower> --write "insert into p6lv1wal (payload) values ('must-not-write');"
```

**Expected:** reads succeed on all three paths; direct SQL writes fail as
read-only; operator `--write` is refused with `is_follower`.
**Proves:** alias routing, TLS path and write refusal on the follower.
**Stop:** a write succeeds, a path reaches the wrong database, or a credential
appears in output. Stop and report a security finding.
**Cleanup:** close sessions.

### A5 — Doctor, metrics and unknown diagnostics

**Maps:** the doctor/metrics bullet and the state matrix bullet of "Follow under
write load".

1. While both sides are running, run `doctor --db <follower> --json`; record
   `follower.streaming`, `follower.lag`, `follower.slot`, and the `backup.*`
   results (`skip` for a follower). Record the expected zero exit status.
2. Record `vaire metrics show` and, with approved metrics credentials, scrape
   `/metrics/node`; look for
   `vaire_follower_lag_bytes{db="<follower>",source="<source>"}` and the other
   documented follower metrics. Unknown samples must be absent, never zero.
3. Run `status --json --db <follower>` and `db list --json`; record
   `source_known`, `self_known`, lag and slot fields.
4. Stop only the source, within `CAP-STOP-SECONDS`. Record both commands:
   `source_known: false`, `source_error`, and the omission of `streaming`,
   `slot_exists`, `slot_active`, `slot_safe_bytes` and `slot_uncapped`.
5. Restart the source. Stop only the follower. `status --db <follower>` must
   still report the source's slot while omitting `in_recovery` and
   `replay_paused`, with `self_known: false`; `db list` must show the row
   `stopped`, STANZA `follows <source>`, with no `follower` object.
6. Stop both. Neither side's facts may appear, and no unknown lag may be shown
   as zero.
7. For a slot whose allowance is unknown (missing, lost, or a NULL/malformed
   `safe_wal_size` without proved uncapped evidence), the JSON must omit both
   `slot_safe_bytes` and `slot_uncapped`. A measured zero or negative value must
   stay. A proved uncapped slot reports `slot_safe_bytes: -1` and
   `slot_uncapped: true`. The live missing/lost part of this sub-case belongs to
   B8; do not fake it here. Proving the genuinely uncapped branch needs an
   approved source with effective `max_slot_wal_keep_size = -1` through
   supported configuration; if that is not approved, mark the branch `BLOCKED`
   and never infer uncapped from a NULL alone.

```sh
# TEMPLATE ONLY.
vaire -c <ctx> doctor --db <follower> --json
vaire -c <ctx> metrics show --json
vaire -c <ctx> status --db <follower> --json
vaire -c <ctx> db list --json
# Stop/start commands are node actions; use only the approved unit names.
```

**Expected:** the documented JSON omissions and independent `source_known` /
`self_known`; no zero for an unknown.
**Proves:** Q3 acquisition and unknown-vs-zero behavior on real responses.
**Stop:** doctor panic, a zero rendered for an unmeasured lag, or an omitted
fact that should be known while that side is readable. Preserve the JSON.
**Cleanup:** restore both services to running and verify lag.

### A6 — Budget refusal

**Maps:** "The budget".

1. From `tune show`, choose a requested size or memory that exceeds the
   remaining budget, with no `--allow-overcommit`.
2. Run `db follow` for a proposed follower name that is absent. A refusal
   before any entry, slot or copy is required.
3. Record `memory_overcommitted`, `details.largest_size` and the complete
   suggested command. Do not run the suggestion unless separately approved.
4. If the refusal is `would_not_start` or another reason, record it and do not
   claim the budget case. If no size exceeds the budget, record `BLOCKED`.

**Expected:** `memory_overcommitted` with the size that fits; no follower,
slot, copy or `cluster.toml` entry left behind.
**Proves:** admission refusal and the actionable budget message.
**Stop:** a partial entry or slot exists after the refusal.
**Cleanup:** none.

### A7 — Guards

**Maps:** the guard matrix of "Guards and protection"; Q2 is A12.

1. With `FIX-FOL` following, run the guard matrix. Each must be refused before
   a destructive effect. Use the confirm token where the command requires one,
   so the refusal is the follower/source guard rather than a missing token:
   - `db drop <source> --confirm <source>`: `has_followers`, naming the
     follower.
   - `backup now --db <follower>`: `is_follower`.
   - `restore <follower> --latest --confirm <follower>/restore`: `is_follower`.
   - `db fork <follower>`: `is_follower`.
   - `db credentials <follower> --rotate --confirm <follower>`: `is_follower`.
   - `maint vacuum --db <follower>`: `is_follower`.
   - `db follow <follower>`: `follows_follower`.
   - `vaire sql --db <follower> --write`: `is_follower`.
   - `backup now` without `--db`: skips the follower and backs up the others.
2. Verify the source still serves writes and reads after every refusal.
3. Leave the source unprotected for A8–A10 so the shrink refusal in A10 reaches
   `has_followers` rather than `database_protected`. Q2 protection is applied in
   A12 before the unfollow, where the follower already exists; that case
   verifies the unprotected copy and independent protection.
4. Do not lift a protection with an agent or non-interactive command. A human
   TOTP step is a separate approval.

**Expected:** every guard code exactly as documented, with the source still
serving.
**Proves:** guard precedence and aggregate skip. Protection independence is
proved in A12.
**Stop:** any destructive command proceeds.
**Cleanup:** none; leave the pair in place.

### A8 — Stopped follower and slot pressure

**Maps:** "A follower that falls behind".

1. With the bounded write load running, stop only
   `postgresql@18-<follower>` for at most `CAP-STOP-SECONDS`. The write load may
   use the application role or the unprotected fixture's `vaire sql --write`.
2. Run `doctor --db <follower> --json`: `follower.streaming` and
   `follower.lag` skip; `follower.slot` still reads the source. The slot's
   retained bytes may grow.
3. Run `status --db <follower> --json` and `db list --json`: the source's slot
   is visible in `status`, retained bytes are present when measured,
   `slot_safe_bytes` is present when measured; `db list` shows the row
   `stopped`, STANZA `follows <source>`, with no follower object.
4. Stop the write load if the slot allowance, disk or `CAP-STOP-SECONDS` bound
   is reached. Start the follower again and verify lag returns to zero.
5. If the slot reaches `unreserved` or `lost`, record the actual state and stop
   this case; B8 owns the separate slot-state procedure.

**Expected:** stopped-side omission, source-side slot pressure, recovery to
streaming.
**Proves:** stopped follower diagnostics and slot retention.
**Stop:** disk below `DISK-RESERVE`, slot lost unexpectedly, or the source
becomes unreadable.
**Cleanup:** restart the follower, verify streaming.

### A9 — Paused replay

**Maps:** the paused-replay bullet of "A follower that falls behind".

1. Keep the bounded write load running. With the follower streaming, run
   `select pg_wal_replay_pause()` in the follower.
2. `db list` must show `replay PAUSED`; `doctor` must fail
   `follower.streaming` (expected exit 5); the source slot must retain WAL.
3. Bound the pause by `CAP-STOP-SECONDS`; stop if disk or slot allowance
   approaches the approved reserve/cap.
4. Run `select pg_wal_replay_resume()`; verify lag returns to zero and doctor
   passes.

**Expected:** paused state visible, doctor failure, resumed streaming.
**Proves:** replay-pause diagnostics and no false healthy value.
**Stop:** the pause cannot be resumed or the slot state becomes unsafe.
**Cleanup:** resume replay and verify.

### A10 — Resize: growth, retry and shrink refusal

**Maps:** "Resize a source".

1. With the follower streaming, run the approved growth to `SIZE-GROW` with
   `--defer-restart`. Expected: the followers' configuration is applied first,
   the source's reload-only changes apply, and any needed restarts remain
   pending; the warning names the follower's token and says replay may pause.
   No restart may happen in this step. If the growth needs no restart, record
   that and skip step 2; do not claim a pending restart that did not occur.
2. Complete the deferred restart in the supported manual order:
   `apply --db <follower> --confirm <follower>/restart` first, then
   `apply --db <source> --confirm <source>/restart`. Record UTC order and
   verify the follower keeps streaming and both new sizes are effective. This
   is the supported completion for a deferred floor; do not use a plain
   `apply` for the ordering proof.
3. If a budget-safe `SIZE-GROW2` exists, run a second growth with both tokens in
   one `db resize`. Record the restart order: follower first, then source. If
   no second growth fits, mark this sub-case `BLOCKED`.
4. Shrink refusal: run `db resize <source> --size <SIZE-SHRINK>` with no
   confirm tokens. Expected `has_followers` naming the follower and the floor
   that would fall, before any mutation. If no smaller size exists, `BLOCKED`.

**Expected:** deferred resizes warn and stay pending, the manual completion is
follower-first, a second growth is follower-first, and a shrink is refused.
**Proves:** Q1a ordering, deferral warning and completion, shrink refusal.
**Stop:** source restarts before the follower, a shrink proceeds, or a restart
happens without the approved token.
**Cleanup:** record final sizes; do not shrink back unless separately approved.

### A10b — Privileged configuration-write failure retry

**Maps:** "A retry after a failed follower update". **BLOCKED (live).**

The configuration writer runs as root. Making a follower's `conf.d` unwritable
for `postgres` does not block that writer, so the checklist's chmod example is
not a fault at all. Source investigation found no supported deterministic hook
for a privileged configuration-write failure. Do not add a hook, change
ownership or permissions, corrupt a file or inject a host-level fault.

The regression evidence is `TestApplyFollowerConfigsStopsOnAFailure`,
`TestApplyFollowerConfigsRunsEachFollowerAlone`,
`TestResizeAppliesFollowersFirstOnRetry` and the follower-first retry tests.
The normal deferred retry is A10; this blocked case is specifically the
unavailable privileged write failure. A live run needs a separately approved,
reversible test-only mechanism that proves the write failed at the intended
boundary.

### A11 — Fresh floor refusal names real settings

**Maps:** "A fresh floor refusal is executable" (corrective-pass semantics).

1. Ensure no follower named `FIX-PROBE` exists and the source's running floors
   are above the floors of a proposed follower at `SIZE-FLOOR-PROBE`. If no
   such size exists, mark `BLOCKED`.
2. Run a fresh `db follow` with `FIX-PROBE` and `SIZE-FLOOR-PROBE` and
   `--dry-run`. Expected
   `source_floors_above_follower`, naming the source's running values and
   `vaire config set clusters.<source>.settings.<setting> <running value>`
   commands. No entry, slot or copy may exist. If the refusal is a different
   reason (budget, space, name), record it and stop; do not treat it as this
   case.
3. Record each named setting's original desired value, or its absence. Run the
   exact suggested `config set` commands on the existing source. They must
   succeed; no restart is expected.
4. Retry the same `db follow --dry-run`; it must be admitted. If the real-follow
   step is approved, run the same follow for real and verify streaming. Drop it
   only with cleanup approval; otherwise leave it and report it. A dry-run
   admission alone does not prove the full retry if the real step was not
   approved; record that limit.
5. Restore each setting through supported configuration: set the original value
   if it existed; if it was absent, remove the temporary override through
   `config edit` (interactive, human-approved) rather than leaving a permanent
   explicit value. Apply and verify the effective source values are unchanged.

**Expected:** the hint is executable against the real persisted source, and the
retry becomes admissible without restart.
**Proves:** C2-A4/B2-02 remediation path; source regression
`TestFreshMembershipFloorMismatchSuggestsSourceSetting` exercises the real
setter and subsequent admission.
**Stop:** the hint names the not-yet-existing follower, the `config set` is
refused, or a restart is triggered.
**Cleanup:** restore the original settings/absence and apply.

### A12 — Unfollow, access retirement and migration

**Maps:** "Unfollow and rehearse a migration" for the no-bucket context; the
stanza/first-full part is B2.

1. Stop the write loop. Q2 protection: ensure the protection factor is
   initialized, otherwise record Q2 `BLOCKED` (`protect_not_initialized`).
   Protect the source and confirm `db list` shows the source `PROTECTED yes`
   and the still-following `FIX-FOL` `PROTECTED no`.
2. Run `db unfollow <follower>` with the confirm token. Record the new
   credentials only in the secret handoff; never in evidence.
3. Expected: the follower is promoted, no longer follows, has new roles, and
   the source is untouched. Both poolers route `<follower>` to its own
   database. The copy is still `PROTECTED no` while the source stays protected
   (Q2). The slot `vaire_<follower>` is gone from the source; if it is not,
   record `slot_not_dropped` and the SQL, do not run a repair.
4. Access retirement: the old `<source>_app` and `<source>_ro` credentials must
   fail against `<follower>`; the new `<follower>_app` credentials must work;
   the source's own credentials must still work on the source. Check that
   `db list` shows `follows` empty and the copy still `PROTECTED no`.
5. Run the migration rehearsal through the session pooler as
   `<follower>_migrate` (or an approved equivalent), then verify it applied.
6. Optional exact protect-before-follow order: only if dropping and re-following
   are approved, drop the independent copy, then follow the protected source
   again under the same name. Verify the newly created follower is
   `PROTECTED no`. If those destructive steps are not approved, record the
   limit: the follower existed before protection, so creation-after-protection
   is not live-proved.
7. Protect the independent copy with `vaire db protect <follower>` and confirm
   both source and copy show protected. This is the independent-protection
   check, not inheritance.
8. Drop the unfollowed copy only with explicit cleanup approval and the exact
   confirm token. If cleanup is not approved, leave it and report it as a
   remaining fixture. If the copy is protected, a human TOTP unprotect step is
   separate.

**Expected:** promotion, credential retirement, alias retirement, source
untouched, migration works on the independent copy, and protection is not
inherited.
**Proves:** Q2, unfollow access retirement, and the rehearsal.
**Stop:** a source credential works on the copy, the source changes, or the
slot is still present and the warning is unexplained.
**Cleanup:** drop the copy only under the approved cleanup scope.

### A13 — Cleanup Pass A

Only with explicit cleanup permission:

1. Drop the follower and any probe follower first, then the source, then the
   auxiliary database, using their confirm tokens. Never drop a source with a
   follower. If the source or a copy is protected, a human TOTP unprotect step
   is separate; without it, leave the protected object and report it.
2. Remove the write table with the database drop; do not hand-edit data.
3. Restore any temporary source setting, slot cap override, protection flag and
   deferred restart through supported configuration.
4. Stop every write loop and background shell.
5. Record every remaining resource (databases, slots, stanzas, pooler entries,
   timers, evidence files). If cleanup is not authorized, leave the fixtures
   and report them; do not delete anything.

## 4. Pass B — bucket, stanza, restore and rebuild

**Preconditions:** Pass A complete and approved; AC-01 gate recorded;
`status.backups_configured` true; bucket context identity, endpoint, bucket and
prefix verified; binary identity verified; a separate approval for the server
rebuild and its accepted
data-loss/downtime boundary; the human desktop root key and any TOTP/sudo steps
approved. The documented `mbvlabs` bucket and prefix are candidates, not
verified live facts. If the bucket or a live server is absent, Pass B is
`BLOCKED`. Each case that needs an awaiting-recovery state needs its own
approved rebuild; a single rebuild cannot serve B4 and B5 after one of them
has consumed that state. Record how many rebuilds are approved, and mark any
case without one `BLOCKED`.

### B1 — Stanza-backed follow under write load

**Maps:** "Follow under write load (from the stanza)".

1. Create `FIX-SRC` at `SIZE-SRC` under the bucket context and take a full
   backup. Verify `backup list --db <source>` shows the full before following.
2. Start the bounded write load as in A1.
3. Run `db follow <source> --name <follower>`. Record `data.from`; it must be
   `stanza`. If it is `base_backup`, the stanza was not available; record the
   reason and do not call the case passed.
4. Repeat the A2/A3 observations: streaming, lag zero, slot active, and
   `60-vaire-follower.conf` with the stanza's `restore_command`; there must be
   no `restore_command` in `postgresql.auto.conf`.
5. Repeat the A4 alias checks and A5 diagnostics, including the known/unknown
   matrix, then restore both services and verify streaming. Stop the write load
   only after the follower is healthy again.

**Expected:** stanza-backed copy, streaming, alias and diagnostics identical to
the no-bucket path.
**Proves:** the stanza copy path and its config rendering.
**Stop:** fallback to base backup without a recorded reason, or a config that
does not match the stanza-backed contract.
**Cleanup:** keep the follower for B2.

### B2 — Unfollow with its own stanza and first full

**Maps:** the remaining "Unfollow and rehearse a migration" observations.

1. Stop the write loop. Run `db unfollow <follower>` with its token.
2. Verify new credentials are printed once, the promoted copy has its own
   stanza and a first full (`backup list --db <follower>`), and its timers
   exist.
3. Compare row counts and each sequence's `last_value` with the source at the
   promote moment. Verify `<follower>_app` connects with the new password and
   `<source>_app` does not connect to the copy; the slot is gone from the
   source.
4. Run the migration rehearsal and drop the copy only with cleanup approval.

**Expected:** own stanza/first full/timers, data and sequence equality, access
retirement.
**Proves:** stanza-backed unfollow completion.
**Stop:** `first_backup_failed`, missing timers, a source credential accepted,
or data mismatch; record FAIL rather than a partial pass.
**Cleanup:** as approved.

### B3 — Restore a source with a follower

**Maps:** "Restore a source with a follower". **Bucket only.**

1. Recreate the source/follower pair if B2 dropped it. Ensure the source has a
   verified restore window and `RESTORE-TARGET` inside it.
2. If the source is protected, a human must unprotect it with TOTP and
   re-protect it afterwards; without that separate approval, use an
   unprotected fixture and do not claim the protected variant.
3. Run `restore <source> --time <RESTORE-TARGET> --confirm <source>/restore`,
   then `restore promote <source> --confirm <source>/restore`.
4. Expected: `followers_diverged` warning; doctor fails `follower.streaming`;
   the follower keeps serving the old timeline read-only. Drop the follower and
   follow again; verify it returns.
5. No-bucket execution cannot prove this case. If only a no-bucket context is
   approved, mark it `BLOCKED`; do not simulate the divergence.

**Expected:** the documented divergence warning and recovery by drop/re-follow.
**Proves:** source restore divergence visibility.
**Stop:** the follower writes, the source loses unrelated data, or the warning
is absent.
**Cleanup:** re-protect the source if it was unprotected by a human; drop and
re-follow as approved.

### B4 — Rebuild, awaiting recovery and source-first `db recover`

**Maps:** the first bullet of "Recovery after a rebuild".

1. With the source/follower pair healthy, perform the approved rebuild with
   `server init --restore` from the human desktop. The command must be one
   complete command with the verified `root@<address>`, context, identity,
   runtime options and `--confirm <server>/replace` resolved. The documented
   desktop identity is a candidate, not a live fact; verify and approve it. Do
   not hand a placeholder command to the user.
2. After the rebuild, `db list` must show both databases
   `awaiting_recovery`. `db recover <follower>` must be refused with
   `source_awaiting_recovery`.
3. Run `db recover --all`. It must recover the source first, then the follower
   as a follower, and take a full of the source only.
4. Verify the follower streams again and its backup checks are skipped.

**Expected:** both awaiting, refusal, source-first order, source-only full.
**Proves:** rebuild recovery ordering.
**Stop:** the follower recovers before the source, a follower full is taken, or
the source is not serving after its recovery.
**Cleanup:** record the rebuilt state; no pre-existing resource deletion.

### B5 — Interrupt recovery during the copy

**Maps:** "Kill recovery during the copy".

1. Use an awaiting follower from a fresh approved rebuild. If none is
   available, mark the case `BLOCKED`; do not fabricate awaiting state.
2. Start `db recover <follower>` and observe the job's ownership record
   `follow-<follower>.json` phase `copying` (read-only root inspection,
   separately approved). This is only a candidate window: the marker precedes
   slot preparation and may outlive copying. It does not prove where the
   process stops.
3. Stop the job unit only within the separately approved candidate window.
   After termination, collect evidence of the actual stopped state, partial-copy
   artifacts and persisted progress. A pre-stop marker or elapsed time alone
   cannot distinguish the intended boundary.
4. Verify `records.json` still marks the database awaiting, and
   `db recover <follower> --dry-run` reports `resume: false` with the partial
   copy removal on retry.
5. Rerun the recovery; it must finish, with `60-vaire-follower.conf`, the slot
   and streaming present.
6. If post-termination evidence cannot distinguish copying from preparation or
   adoption, mark the case `INCONCLUSIVE` or `BLOCKED`, even if retry succeeds.
   Do not claim proof from the pre-stop marker or timing alone.

**Expected:** awaiting persists, partial copy is removed, retry finishes.
**Proves:** recovery at the copy boundary only with adequate post-termination
evidence; otherwise retry behavior without boundary proof.
**Stop:** a foreign cluster is reused or the source is written.
**Cleanup:** complete the recovery or leave awaiting with approval.

### B6 — Interrupt recovery during adoption

**Maps:** "Kill recovery during adoption". **BLOCKED.**

There is no supported deterministic live hook that proves the process died
inside `Adopt` between the copy and the first start. A timing-only kill is not
proof. `TestRecoverAdoptedCopyRequiresRetainedSlot` and the lifecycle
regressions cover the owned-copy decision; live proof is blocked until a
deterministic supported boundary or a separately approved test-only mechanism
exists. Consequently the adopted-copy reuse-versus-recopy split after this
boundary remains live-pending. Do not add a hook, modify the job, or kill by
feel.

### B7 — Interrupt before the adopted marker

**Maps:** "Kill recovery before the adopted marker". **BLOCKED.**

The interval between a running `18/<follower>` and the durable adopted marker
cannot be observed deterministically from outside. Timing-only proof is invalid.
Do not edit `records.json`, the ownership file or the journal.
`TestRecoverAdoptedCopyRequiresRetainedSlot` and the marker/ownership
regressions cover the expected retry semantics. A separately approved
deterministic mechanism is required before this case can run.

### B8 — Running-follower slot diagnostics through supported configuration

**Maps:** slot-allocation diagnostics only. Retained-copy recovery qualification
is separately blocked below. This is the supported cap route; `ALTER SYSTEM`
is disabled and must not be used.

1. Begin with a verified materialised, streaming disposable follower and an
   existing source slot. Capture their identity and health before stopping it.
   A freshly rebuilt awaiting entry proves neither completed local copy nor
   retained slot and is not this fixture.
2. Capture the original desired state:
   - `clusters.<source>.settings.max_slot_wal_keep_size` exists or is absent,
     with its exact value or exact absence error;
   - inherited `postgres.settings.max_slot_wal_keep_size`, if any;
   - effective `SHOW max_slot_wal_keep_size`;
   - current slot `wal_status`, `restart_lsn`, `safe_wal_size`, retained bytes.
3. Set the temporary cap through supported configuration:
   `vaire config set clusters.<source>.settings.max_slot_wal_keep_size
   <CAP-SLOT>`, then `apply --db <source>`. Verify `SHOW
   max_slot_wal_keep_size` and `select setting, pending_restart from pg_settings
   where name = 'max_slot_wal_keep_size'`. This key is not a hot-standby floor;
   if the change needs a restart or touches a floor setting, stop and restore
   before proceeding.
4. Stop the follower. Generate only `CAP-WAL-BYTES` within `CAP-WAL-SECONDS`,
   monitoring `pg_wal_lsn_diff` and free space. Use the source's application
   role or another approved write path; a protected source's `vaire sql
   --write` needs a human TOTP and must not be forced. Stop early if free space
   falls below `DISK-RESERVE` or the slot reaches a state the approval did not
   cover.
5. Observe `pg_replication_slots.wal_status` after generation. If it has not
   advanced, one approved `CHECKPOINT` on the source (as `postgres`, with the
   same approval) may be needed; record its LSN/time and do not loop it. Record
   the actual state:
   - `reserved`/`extended`: not the target case; stop, restore and report.
   - `unreserved`: record status/doctor/slot facts as actually observed; this
     does not qualify retained-copy recovery preview or recopy decisions.
   - `lost`: record it as `lost`, not `unreserved`. Do not relabel it or generate
     more WAL to force a different label.
   - no row: record `missing`; use the missing-slot sub-case below rather than
     claiming that the absent row came from this generation.
   - any other or no observed transition: record it as `INCONCLUSIVE` for the
     intended case; do not relabel the run.
6. Restore the original state through supported configuration: set the original
   value if it existed; if it was absent, remove the temporary key with
   `config edit` (interactive, human-approved) so inheritance is restored.
   Apply and verify the effective value matches the original semantics.
7. Never leave a permanent low cap, never restore a different value, and never
   use `ALTER SYSTEM`.

**Missing-slot sub-case (separate destructive approval).** With the follower
stopped and the original cap restored, drop only the fixture's slot as
`postgres` on the node:

```sh
# TEMPLATE ONLY — resolve verified source cluster socket/port and fixture slot.
# Separate sudo, destructive-action and fixture cleanup approval required.
vaire -c <ctx> ssh --command "sudo -n -u postgres psql -h /var/run/postgresql -p <source-port> -d postgres -v ON_ERROR_STOP=1 -c \"select pg_drop_replication_slot('vaire_<follower>');\""
```

Verify `<source-port>` from source inventory, not pooler port 5432 or a guessed
default. Record `slot_exists: false` and omitted allowance fields, not recovery
preview/recopy results. Never use a non-fixture source. Without destructive
approval and approved public drop/re-follow fixture cleanup, mark `BLOCKED`.

**Expected:** approved bounded cap transition, actual slot observations and
original-state restoration, with no recovery-preview claim.
**Proves:** supported slot-cap diagnostics and actual lost/unreserved separation.
`TestParseFollowerSlotSafetyUnknown` covers parser branches; this case observes
PostgreSQL state, not retained-copy recovery safety.
**Stop:** disk below reserve, an unrelated slot changes, the cap change needs a
restart not in the approval, or the original state cannot be restored.
**Cleanup:** restore original cap semantics. If a fixture slot is lost/dropped,
use only the separately approved public drop/re-follow fixture lifecycle and
verify streaming; do not repair slots manually or claim recover admission.

**Retained-copy recovery reuse/recopy — BLOCKED.** Fresh rebuild awaiting state
supplies neither completed owned local copy nor necessarily retained slot.
Slot-based `recopy_reason` qualification requires proving a completed owned
copy/adoption while awaiting recovery remains. No supported deterministic setup
is established here; B6/B7 remain blocked. An ordinary stopped follower or
copying marker is not a substitute. Missing/lost/unreserved retained-copy
preview and safe recopy remain blocked until supported setup or a separately
approved test design proves those preconditions.

### B9 — Unread slot preview

**Maps:** "A dry run whose slot cannot be read reports `resume_conditional`".
**BLOCKED.** No supported live mechanism makes only the slot unreadable without
corrupting access to the source or the records. Do not revoke permissions,
damage a file or stop a service to imitate it. Source regression coverage is
the evidence until an approved deterministic mechanism exists.

### B10 — Prepared follow, generation and ordinary retry

**Maps:** "A prepared follow is never a general-apply cluster" and the ordinary
retry semantics. **BLOCKED (live).**

The required boundary is a fresh `db follow` killed after its prepared entry is
written and before any copy. There is no supported deterministic live hook for
that point; a timing-only kill is not proof. Do not insert a hook or edit
`cluster.toml`/`records.json`. Source regressions
`TestApplyDoesNotInitialisePreparedFollower` and the ordinary-retry regressions
cover the intended behavior. Record the source evidence and leave the live
case blocked.

### B11 — An unfollow owns its target

**Maps:** "An unfollow owns its target". **BLOCKED (live).**

The required boundary is an unfollow interrupted after promotion and before the
renames. Timing alone does not prove it. Do not add a sleep, hook or manual
state edit. Source regressions `TestOrdinaryFollowCannotResumeAfterUnfollowBegins`,
`TestUnfollowFenceFailureRefusesPromotion`,
`TestUnfollowSettlesAccessBeforeExposingTheAlias` and
`TestUnfollowFenceKeepsOtherUnfinishedAliasesExcluded` cover the intended
refusal. A separately approved deterministic mechanism is required before this
live case can run.

### B12 — Unread transition records fail closed

**Maps:** "Unread transition records fail closed at the poolers". **BLOCKED.**

Making `records.json` unreadable or corrupt is not an approved live technique,
even on a disposable node. The blocked case is unread or corrupt records; a
missing records file remains the normal empty initial state and is not this
fault. Do not `chmod`, truncate, replace or hand-edit it. The source regressions
`TestPgbouncerRenderingFailsClosedOnUnreadUnfollowState` and the direct-fence
regressions are the evidence. A live run requires a separately approved,
reversible mechanism that proves the read failure without damaging records.

### B13 — Cleanup Pass B

Only with explicit cleanup and rebuild-boundary approval:

1. Recover or drop the follower and source in the supported order; never drop a
   source with a follower.
2. Keep or remove the bucket test objects only as approved; do not delete the
   bucket or another context's data.
3. Leave the rebuilt host in a recorded state. If the approval was for a
   disposable VPS, record whether it remains allocated or is to be rebuilt
   again; do not run an unauthorized destructive cleanup.
4. Remove temporary config overrides and restore protection flags through
   supported configuration. A human TOTP step is separate.
5. Record every remaining database, slot, stanza, timer, pooler entry, bucket
   object and evidence file.

## 5. Coverage map

| MANUAL-TESTING case | Run-sheet case | Status |
| --- | --- | --- |
| The upgrade | A0, then the restart step in A2 | READY FOR APPROVAL |
| Follow under write load (stanza) | B1, with A2–A5 observations | READY FOR APPROVAL (B pass) |
| Follow without backups | A2 | READY FOR APPROVAL |
| The budget | A6 | READY FOR APPROVAL |
| Guards and protection | A7 guards, A12 protection/unfollow | READY FOR APPROVAL |
| Unknown, missing or lost slot diagnostics | A5, B8 | A5 ready; B8 needs slot approval |
| Falls behind and paused replay | A8, A9 | READY FOR APPROVAL |
| Unfollow and rehearse a migration | A12 (no-bucket), B2 (stanza) | READY FOR APPROVAL |
| Resize a source | A10 | READY FOR APPROVAL / sub-case blocked if no safe size |
| Retry after a failed follower update | A10b | BLOCKED (live) |
| Restore a source with a follower | B3 | BLOCKED until bucket approval |
| Recovery after a rebuild | B4 | BLOCKED until rebuild approval |
| Kill recovery during the copy | B5 | BOUNDED, requires rebuild approval |
| Kill recovery during adoption | B6 | BLOCKED |
| Kill recovery before the adopted marker | B7 | BLOCKED |
| An unreserved slot | B8 | Bounded diagnostics with approval; retained-copy preview/recopy BLOCKED until owned completed-copy/awaiting setup is proven |
| Dry run whose slot cannot be read | B9 | BLOCKED |
| Prepared follow is never general apply | B10 | BLOCKED (live) |
| Unfollow owns its target | B11 | BLOCKED (live) |
| Fresh floor refusal is executable | A11 | READY FOR APPROVAL |
| Unread transition records fail closed | B12 | BLOCKED |

## 6. Publication boundary

This document can support LV-01 readiness only. PUB-01 still needs separate
commit, push and pull-request permission, the reviewed source/test tree, the
full pre-commit chain, and an AC-01 outcome accepted in the main testing
record. Before merge readiness, reconcile main's AC-01 `MANUAL-TESTING.md`
changes with this follower checklist line by line, preserve both dated evidence
sets and keep unknowns explicit; unresolved evidence conflicts block merge.
Nothing here authorizes a merge, tag, publication or deployment.

## 7. Evidence and completion

A case is complete only when all of these are recorded:

- the approved permission and bound values it used;
- the exact resolved command and its UTC start/end;
- the context, server identity and binary version/commit/channel;
- the job ID or unit, exit status and first actual failure;
- the observation that proves the intended state, including JSON fields or
  slot rows, not only a human summary;
- the cleanup result, or the exact remaining resource and why it remains;
- whether the result is `PASS`, `FAIL`, `INCONCLUSIVE` or `BLOCKED`.

Update [MANUAL-TESTING.md](MANUAL-TESTING.md) with dated per-case outcomes and
explicit gaps. Do not claim a live pass from source tests, a healthy read, a
successful check command, or a case that was not executed. Do not check a box
because the procedure exists.

Completion of this run sheet as documentation means only that LV-01 has a
bounded mapping with explicit blocked cases. It is not a live qualification,
not a publication approval and not a merge-ready claim. Publication still
follows [PLAN-v1-release.md](PLAN-v1-release.md) and the separate PUB-01 gate;
activity qualification still precedes deployment; all live and destructive
permissions remain separate.
