# Exploration: an activity history and diagnostic timeline for vaire

> Scope selected in review and implementation authorized on 2026-10-26. Current implementation/qualification status lives in `MASTER.md`; no release or deployment authorization is implied.
> Based on source commit `a7075d8d4859`, examined 2026-10-26.
> This is a repository investigation and architectural recommendation; no server changes, load tests or backend evaluations were performed.

## Recommendation in plain English

**Yes: vaire should give an operator or agent one place to ask “what happened, on which node, and why?”** We have useful pieces already, but not that unified experience.

**Do not create a default PostgreSQL database for it.** Diagnostics must still work when the database or pooler being diagnosed is broken, being restored, or under heavy load.

The initial scope selected in review is:

- A small, structured **activity history on each node**, stored as bounded, owner-only files, independent of PostgreSQL.
- A **single read-only CLI interface** that searches that history and assembles a timeline, with correlation IDs linking commands, jobs and steps.
- An **on-demand, portable incident bundle** that an agent can inspect without receiving an operator context or server credentials.
- **Allowlisted operational metadata for agents by default**, clearly separated from potentially sensitive raw diagnostic logs.

**Deferred:** continuous runtime-log ingestion/search, off-node collection or archiving, and any guarantee that logs survive loss of the VPS. No external collector, log database or export service is required for the initial deliverable.

**Accepted limitation:** evidence still on a lost VPS is lost. An incident bundle already collected onto another machine remains available, but collecting one is explicit and is not automatic off-node protection.

This is logging, not a new distributed locking system. Existing operation locks remain separate; their waits and refusals should become visible in the activity history.

## Metrics, activity history and raw logs answer different questions

```text
Metrics                         Activity history
"Are things healthy?"           "What happened?"

CPU, pool saturation,           apply started, backup skipped,
latency, backup age              certificate renewed, access refused
           |                              |
           +---------------+--------------+
                           |
                   identify an incident
                           |
                           v
                    Diagnostic evidence
            service errors, PostgreSQL logs,
             version/config metadata, job detail
```

Logs are technically telemetry too; the intended distinction here is from vaire's existing **metrics/exporter pipeline**. Logs need their own access policy, storage, retention and query interface. They should not be added to `/metrics` responses or exposed with the same credentials by default.

An activity history records operations and outcomes. It does **not** mean recording every SQL statement, result row, network packet or shell keystroke. That would be expensive, privacy-sensitive and still not a reliable account of everything root can do.

## What already exists in the repository

| Existing mechanism | What it gives us | What it does not give us |
| --- | --- | --- |
| Per-RPC run logs in `/var/log/vaire/` | Request metadata, emitted progress, script output and final results for selected methods; filenames include request IDs | Complete operation coverage, a queryable fleet timeline, or guaranteed secret-free content |
| systemd journal | Timer/job events and service diagnostics | One normalized history across vaire, PostgreSQL, poolers and the operator machine |
| `state.json` job records | IDs, state, times, unit/invocation, recent outcomes | Long-term history: it keeps only 50 jobs; some frequent tasks deliberately do not enter that list |
| `records.json` | Durable backup/restore-related facts needed to operate/recover correctly | A general activity/event store |
| Step journal | Applied steps, input hashes and pending restarts | A complete chronological record of attempts, refusals and operator activity |
| `vaire logs` | Reads selected run-log, journal or PostgreSQL sources over SSH | Fleet-wide selection, request lookup, stable pagination or a sanitized incident export |
| Local `tunnel.log` | Operator-side WireGuard/daemon diagnostics | Automatic inclusion in server-side troubleshooting |
| Master backup bundles | Preserve the server identity and selected durable state | A log archive: diagnostic log files are not in the bundled file set |

There is already a good resilience measure: RPC events are written to the local run log **before** they are queued for SSH stdout. A stalled or disconnected client therefore does not stop the handler just because the output stream fails. Preserve that design.

Source: `internal/engine/runlog.go`, `internal/node/dispatch.go`, `internal/node/jobs.go`, `internal/engine/state.go`, `internal/engine/journal.go`, `internal/cli/tunneld.go`, `internal/masterbackup/files.go`.

### Concrete gaps that matter when an agent investigates a failure

- **Early refusals can leave no run log.** Version checks, confirmation checks and lock acquisition happen before the dispatcher opens its selected run log. Those are precisely the failures an agent may need to explain.
- **Some sensitive operations intentionally have no run log.** Examples include TOTP initialization/verification and temporary database write-access issuance. We should record safe outcomes, not solve this by logging their requests or responses.
- **`vaire logs vaire` chooses only the newest run-log file.** It cannot currently select the failed request by ID or search all operations over an incident window.
- **Time filtering is inconsistent.** `--since` is applied to journal sources, but file/run-log readers just tail a line count. Following one file is not the same as following subsequent operations or rotated files.
- **Structured output is not a uniform log-query contract.** The CLI prints log lines directly before its final result, including in JSON mode. Agents do not get a clean, bounded page of normalized log records.
- **Correlation exists in pieces.** An RPC-started detachable job reuses the request ID, and records include unit/invocation IDs. But the generic event type has no dedicated node, operation, request or job fields. Job replay also clears the original event timestamp before re-emitting it; replay time must not masquerade as occurrence time in a future incident history.
- **Local evidence has limits.** journald is configured for persistent storage with a 1 GiB maximum and 14-day maximum retention, subject to disk pressure. Run logs have daily logrotate rules with 30 rotations and `maxage 30`, not a unified byte-budgeted incident store.
- **A file existing does not prove every write succeeded.** `RunLog.Line` currently discards write errors. That is acceptable to identify as a gap, not a basis for claiming complete durable audit coverage.

Normal SQL traffic and interactive SSH commands are not comprehensively audited by these RPC run logs. PostgreSQL slow-query logging can provide other evidence, but also carries data-disclosure risk.

Source: `internal/node/dispatch.go`, `internal/node/methods.go`, `internal/node/logs.go`, `internal/cli/logs.go`, `internal/cli/output/output.go`, `internal/node/jobs.go`, `internal/steps/host_baseline.go`, `internal/cli/sql.go`, `internal/cli/ssh.go`.

## “Centralized” has two meanings; only the first is in initial scope

### One place to investigate, without requiring one central server

```text
Operator / agent
      |
      v
vaire activity / diagnostics         PROPOSED read-only commands
      |
      +--- existing SSH transport ---> node A: local activity files
      +--- existing SSH transport ---> node B: local activity files
      +--- existing SSH transport ---> node C: local activity files
      |
      v
one merged result, with source, time and coverage for every node
```

That is **logical centralization**. Start with the one supported master today; add node selection/fan-out when fleet transport actually exists. Do not pretend worker discovery/routing is implemented already.

If a node is unreachable, the result must say **unreachable / missing evidence**, not “no errors found.” A laptop querying nodes is not continuous collection and cannot retrieve evidence from a destroyed node.

### Deferred: a durable central copy outside the database fleet

The following is future context, not an initial implementation requirement:

```text
node A: local bounded files --+
node B: local bounded files --+--> optional collector / log backend
node C: local bounded files --+        outside the database failure domain
        retryable, outbound export                 |
                                                  v
                                        operator / agent queries

collector unavailable -> nodes keep local evidence within stated limits
database unavailable -> evidence collection does not depend on SQL
```

That is **physical centralization**. It is needed if we want incident evidence after a server disappears, and is especially useful for unattended production fleets.

The master can participate in a combined view, but should not be the only fleet archive: losing the master must not also erase all troubleshooting evidence. A collector on the same VPS is not off-node protection.

## Storage options: files by default, not an application cluster

| Option | Strength | Tradeoff | Recommendation |
| --- | --- | --- | --- |
| Bounded structured files on each node | No database dependency; inspectable from the provider console; fits vaire's current architecture | Need rotation, indexing/pagination and explicit concurrency handling | **Default foundation** |
| SQLite index of those files | Efficient local lookup without running PostgreSQL | Adds index lifecycle and single-writer coordination | Optional later optimization; rebuildable, never the only evidence copy |
| A PostgreSQL logging database on the managed node/fleet | Familiar SQL queries | Depends on the system being diagnosed; adds WAL, backups, resources and failure coupling | **Do not create by default** |
| Dedicated external log backend | Continuous cross-node search and independent retention | Another service, credentials, network cost and access policy | Deferred; not in initial scope |
| Encrypted object-storage archive | Off-node retention and portable incident evidence | Not a low-latency search engine; upload gaps and retention need handling | Deferred; not in initial scope |

Examples of external-backend families are Loki, OpenSearch and hosted log services. No vendor is selected or benchmarked here. An independent external PostgreSQL service is possible too; the objection is coupling required diagnostics to vaire's own database fleet, not that PostgreSQL cannot store events.

Prefer versioned NDJSON segments for the new activity stream, with a private directory and files (`0700` / `0600`). Multiple RPC/job processes need a defined local append/rotation protocol, with short bounded coordination—not a global operation lock held for logging and never a fleet-wide lock. Final locking/segment mechanics belong in the implementation design.

Do not add a continuously running vaire control service just for the initial feature. Existing invocations can emit events; read-only queries and on-demand bundle collection can use the existing SSH transport. A scheduled segment exporter or runtime-log collector would be a separate, deferred integration decision.

## The security model: safe events first, raw diagnostics separately

**“Redacted” is not equivalent to “contains no secrets.”**

Today vaire registers known secret values and replaces exact occurrences in strings. The registry ignores values shorter than six characters. It cannot remove an unknown application password, sensitive SQL literal, encoded/transformed secret or confidential row value just because it appears in a log.

`Request.Loggable()` removes the request's `secrets` values, which is useful, but still includes general params. RPC results and free-form script/error output are also written for logged methods. Copying those existing files wholesale into an agent-facing central store would be an unsafe default.

In particular, a log-reading RPC does not automatically know all secrets used by earlier processes or by PostgreSQL. Do not advertise the current reader as a complete privacy filter. This is a limitation of the mechanism, not evidence that a particular credential has leaked.

### Two deliberately different evidence classes

| Default operational events | Restricted diagnostic evidence |
| --- | --- |
| Typed operation names, bounded resource identifiers, times, outcomes, stable error/reason codes, durations, counts, revisions | PostgreSQL statements/errors, service output, script output tails, stack traces, config diffs and other free text |
| Built from explicit per-event field allowlists | Requires source-specific sanitization and an explicit access/export policy |
| Suitable starting point for automated agent investigation | Local/restricted by default; not bulk-exported just because a collector is configured |
| No generic request, response or `details` dump | Unknown/unparseable fields withheld from the shareable view, with omissions visible |

Default events must omit passwords, keys, TOTP values, verifier hashes, environment dumps, credential-bearing URLs, SQL text, query parameters and result rows. Recording “temporary write access granted” is useful; recording the login password or authenticator secret is not.

Retain registered-value redaction as defense in depth, but **minimize what we record before redacting**. Unknown fields do not become automatically exportable when a dependency or RPC schema changes. Resource names and hostnames may themselves identify customers; “agent-shareable” still needs a deployment-specific privacy policy and is not anonymization.

For raw PostgreSQL logs, preserve useful codes, timings and session metadata where reliably parseable. Do not promise that regexes or normalized SQL can remove every secret. SQL text and arbitrary error detail should stay out of the default shared stream; more detailed capture is an explicit, bounded investigation mode.

Source: `internal/secrets/registry.go`, `internal/rpc/types.go`, `internal/node/dispatch.go`, `internal/engine/shell.go`, `internal/tune/tune.go`, `internal/render/templates/20-vaire-security.conf.tmpl`.

### Access boundaries and agent safety

- Agent-readable history must not grant configuration changes, database writes, arbitrary file reads or shell execution. Implement permissions on the node/collector, not just flags in the CLI.
- Today's operator identity has powerful SSH/sudo capabilities; adding a read-only command does **not** turn that identity into a restricted diagnostic account. True separate operator scopes are later roadmap work. A curated incident export is the initial way to share less evidence without sharing the whole context.
- Treat log text as untrusted input. An application can emit a line that looks like instructions to an agent. Return it as quoted evidence, never as instructions or executable suggested commands.
- Protect incident-bundle files with owner-only permissions and share them explicitly. No new external export credentials are needed initially. If off-node shipping is added later, keep its credentials separate from database/recovery credentials, with per-node write-only ingestion rights and separate read access.
- Root on a node can alter local files. This is an operational debugging history, **not a tamper-proof compliance audit**. Stronger audit guarantees require independent identity/retention controls and a separately designed threat model.

## The correlation contract

An agent needs to follow one operation across commands, steps and nodes without guessing from timestamps.

Proposed record shape, shown as a JSON schema sketch rather than an existing output:

```json
{
  "schema": "vaire-activity/v1",
  "event_id": "globally-unique-event-id",
  "occurred_at": "RFC3339 UTC",
  "server_id": "stable-setup-id",
  "node_id": "stable-node-id",
  "node_instance_id": "changes-on-rebuild",
  "boot_id": "current-boot-id",
  "sequence": 42,
  "operation_id": "one-logical-operation",
  "request_id": "this-RPC-attempt",
  "parent_request_id": "optional-calling-request",
  "job_id": "optional-detachable-job",
  "source": "rpc|job|step|service|operator",
  "action": "backup.full",
  "database": "optional-resource-name",
  "outcome": "started|succeeded|failed|refused|skipped|unknown",
  "error_code": "optional-stable-code",
  "reason_code": "optional-stable-reason",
  "duration_ms": 1250,
  "actor_kind": "operator|timer|system",
  "actor_label": "claimed-or-verified-label",
  "actor_verified": false,
  "node_version": "version-and-commit"
}
```

Optional fields are omitted when not applicable. Stable setup/node identifiers and ordering scope need explicit design; display names alone are insufficient across renames and rebuilds. An implementation should use tested ID formats, not these illustrative strings.

- Keep the existing request/job relationship, but introduce an **operation ID** for one action spanning multiple RPCs or future nodes. Retrying creates a new request attempt; it must not look like a second independent logical operation by accident.
- Record source occurrence time separately from collector receipt time. Add per-source ordering and preserve systemd invocation/boot metadata where available. Clocks across nodes are not a perfect global causal order.
- Generate final events at the node where work actually finishes, not merely when a follower receives a result. No terminal event after a crash means **unknown/incomplete**, not automatically success or failure.
- Record actor labels honestly. The current RPC `operator` field is client-supplied; do not turn it into proof of which human acted. Until separate identities are bound to authenticated sessions, mark that limitation.
- Record authorization/protection decisions without the factor, and lock wait/skip/refusal with a structured reason. Do not record secret request/response bodies to gain coverage.
- An operator-side connection failure may never reach the node. Include safe client-side attempt events or an explicitly collected client diagnostic summary; a server-only history cannot explain every WireGuard failure.

Source for the current building blocks: `internal/rpc/types.go`, `internal/remote/remote.go`, `internal/node/jobs.go`, `internal/engine/lock.go`, `internal/cli/output/output.go`.

## The agent experience we should aim for

The following are **proposed interfaces**, not commands to run today:

```text
activity list     -> bounded, filtered event pages with stable cursors
activity show     -> one operation and its steps/jobs/refusals
diagnostics collect -> explicit incident window and selected sources
diagnostics inspect -> work from an already exported incident bundle
```

Useful filters: time window, node, database, operation/request/job, action, outcome and severity. Return clean JSON/NDJSON, not a mixture of raw log lines and a final JSON object.

Each result should report:

- Which sources/nodes were queried, their coverage windows and version metadata.
- Which were unreachable, forbidden, rotated away, truncated or not collected.
- Whether a returned limit was reached and how to continue without rereading the entire history.
- Whether an operation is ongoing, complete, or missing its terminal evidence.
- Proven facts and evidence references, separately from possible explanations.

An incident bundle should default to typed activity events and allowlisted health/version/config **metadata**. It should never contain contexts, credentials, private keys, TOTP state, database dumps or whole config trees. Raw logs require explicit selection and sanitization. Sensitive live views such as SQL text in `top`/`queries` are not automatically safe diagnostic inputs.

This would let an agent answer “the backup was skipped because the heavy-I/O lock was held by another job,” instead of searching unrelated newest-log tails. It should not claim “everything was healthy” when the relevant node or interval is missing.

## High traffic: make evidence bounded, not a second workload

- Record control-operation lifecycle events and important service transitions by default. Do not enable blanket SQL/connection detail capture to achieve “all activity.” At high traffic even connection logs can be a substantial workload.
- Separate low-volume lifecycle events from noisy progress/debug lines. Coalesce repetitive events and sample suitable diagnostic traffic; do not silently sample away operation outcomes or security decisions.
- Bound local bytes, retention, segment size, event size, read windows and incident-bundle size. Logs must not fill the PostgreSQL filesystem. Retention loss and dropped evidence must be visible.
- On-demand collection must use bounded reads and explicit timeouts; it must not block database serving or require an operation-wide lock. Future background export would need independent queues, retry/checkpoint/deduplication behavior and outage handling; none of that is required initially.
- For important interactive mutations, design a checked local intent write before work begins. If that write fails, refuse before side effects. Safety-critical work is an explicit availability exception: backups, verification, restore copies/cut-over/undo, drills, dumps, database recovery, master backups and maintenance job starts continue and report missing evidence, because diagnostic storage must not become a new way to block disaster recovery. If a completion write fails after side effects, report the operation/evidence uncertainty; never replay or undo the operation just to make the log look complete.
- Reads, health checks and database serving must not acquire the global operation lock just to emit logs. Benchmark local event-writing and incident-collection overhead on realistic workloads before choosing defaults; collector/exporter resource limits belong to any future integration.

Zero evidence loss during disk failure or crash cannot be guaranteed by bounded local files. Surviving VPS loss is explicitly not a requirement for this initial scope. State the durability contract instead of claiming an exhaustive audit trail.

## A sensible scope before expanding the product

### Selected foundation

1. Define the event/privacy/correlation contract and inventory the current emission paths.
2. Add typed local lifecycle/outcome events for RPCs, jobs and steps, including safe early refusals and protection decisions. Keep existing diagnostic logs separate.
3. Add operation lookup, bounded history queries and explicit coverage reporting over the existing SSH channel.
4. Add a sanitized, portable incident export that an agent can inspect without being given the whole operator context.
5. Add storage/error/drop health checks so evidence availability is itself observable.

This is valuable with one server and does not require workers, HA, an HTTP control API, a log database, an MCP server or an always-running vaire node daemon.

### Deferred: continuous off-node collection and runtime-log search

The review chose activity history plus an on-demand incident bundle instead. Do not select a backend, add an exporter, implement continuous raw-log indexing, upload historic run logs or expand the master backup bundle for this initial work. Revisit external storage and its retention, credentials, encryption and failure budgets only when the requirement changes.

### Carry the contract into fleet work

When multi-node transport arrives, propagate operation/parent IDs and add per-node querying over the supported transport. An independent archive remains separately deferred, not an automatic fleet dependency. Future read-only agent/operator identities should enforce access to the intended evidence class.

The review selected this foundation and its default evidence policy; the operator subsequently authorized implementation. This does not authorize commits, deployment, roadmap reordering or release-gate changes. Any subsequent v1 changes follow the repository's PR/release rules.

## Verification requirements for an eventual implementation

- Unit/regression tests: typed-field allowlists; secret canaries (including SQL literals, encoded values, multiline keys and errors); early refusals; timer/RPC correlation; client disconnect; crash/incomplete outcomes; concurrent append/rotation; bounded queries/cursors; timestamp preservation; truncated evidence; bundle size/permissions and offline inspection; full-disk/write errors; permissions and hostile log content.
- Test that adding a new request/result field cannot make it appear in agent exports by default. Test that diagnostic collection itself does not recursively produce an unbounded log stream.
- Build/vet/test/lint and Darwin cross-build using the repository's standard verification. No Docker, Incus or scratch-VM harness.
- Manual qualification on the test VPS only after an approved implementation/release: incident lookup during real jobs, reconnect, retention/rotation, bounded collection, safe bundle review/offline inspection and representative traffic overhead. No manufactured production faults or credential-bearing evidence pasted into chat.

## Decisions recorded from review

| Decision | Selected scope |
| --- | --- |
| Must evidence survive loss of the VPS now? | **No.** Local-only history is acceptable; automatic off-node storage is deferred. |
| Continuous runtime-log search or activity history plus an incident bundle? | **Activity history plus an on-demand incident bundle.** No continuously indexed runtime-log backend initially. |
| What may an agent see by default? | **Allowlisted operational metadata.** Raw PostgreSQL/service logs and arbitrary request/result/error bodies stay outside the default shared view. |

Keep the architecture independent of PostgreSQL: bounded local structured files, operation correlation, one read-only investigation interface and explicit evidence-coverage reporting. A collected bundle enables offline agent investigation without access to the full operator context.

No external collector, default PostgreSQL logging cluster, automatic raw-log export, distributed locking system or local credential-vault change is included.

The selected direction is: **local activity history, on-demand incident evidence, and metadata-first agent access.** Implementation is authorized; current progress and qualification belong in `MASTER.md`, not this design record.
