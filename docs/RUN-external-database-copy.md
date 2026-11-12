# External database copy runtime qualification

Status: source/archive primitives, node lifecycle and CLI implemented; independent reviewer acceptance not obtained before the user's explicit direction to merge and test afterward (see [review record](REVIEW-external-database-copy.md)). Every runtime case below is **NOT RUN**. This run sheet does not authorize publication, deployment, source access, fixture creation, fault injection or cleanup. Historical manual tests do not qualify this feature.

## Authorization and evidence gates

Before running, obtain permission for the exact reviewed candidate, destination context/server, trusted external source and destination fixture names. Resolve the installed command forms and fixture names before giving anyone commands. Do not paste source URIs/passwords into chat, argv, logs or evidence. Supply source access through the implemented private stdin/prompt interface. Only the operator reads an explicitly supplied source CA file.

Use normal signed distribution after separately authorized publication. No development-binary upload, local container/VM harness, server rebuild, provider allowlist change or context switch is implicit. Inspect current identities, jobs, namespace inventory, resource budget and disk headroom through approved read-only interfaces first.

Destination prerequisites include installed PostgreSQL 18 tools, supported extension scripts/control metadata, sufficient archive/data filesystem headroom and the supported node's util-linux/coreutils/shell tools used by workspace leases (`flock`, `stat`, `cat`, `sh`). Missing prerequisites fail closed; do not install arbitrary repositories/packages as a workaround. Source PostgreSQL 9.2–18 is a tool-supported candidate range, not a guarantee for every schema or provider. Sources requiring client private keys are unsupported.

Record UTC time, candidate identity, context/server, source version and sanitized identity, destination name, exact sanitized command, job ID, expected/actual outcome, evidence reference, transient cleanup and remaining fixtures for every case. Record PASS, FAIL, BLOCKED or NOT RUN, never infer a live pass from unit tests. Stop on any unsafe observation.

## Implemented invocation forms

- Interactive: `vaire db copy` asks for missing destination name, masked source URL or individual fields, size/custom memory and explicit confirmation.
- Unattended: destination positional name plus `--source-url-stdin`, `--size` (or `--memory`/custom), and `--confirm` equal to that name. Provide exactly one newline-terminated URI through private stdin. JSON output uses existing `--output json`.
- Custom trust: `--source-ca-file` names an explicitly supplied operator-local PEM CA file. Source TLS mode is modeled in the URI or interactive fields; default is `verify-full`, explicit `verify-ca` and `require` warn, weaker modes refuse.
- Compute: `--profile` selects the normal tuning profile; `--allow-overcommit` is only an explicitly authorized budget override. Named size or `--size custom --memory` selects memory/CPU/connections, not disk quota.
- Intent only: destination name/sizing plus `--dry-run`, with no source input needed. Expect no source connection, mutation, job or credential generation and source checks marked unchecked.
- Follow: the existing `vaire job follow` takes the actual returned/preselected job ID. Save one-time destination credentials from the original command; following cannot recover them.

No concrete context/source/fixture names are authorized yet, so executable live case commands are deliberately **BLOCKED pending fixture resolution**. Before running or handing off a command, replace this invocation description with the exact installed command and real approved values; do not hand the person unresolved placeholders. An actual unattended command must include `--source-url-stdin` and `--confirm`, not merely pipe a URL into an interactive invocation.

## Case map

| Case | Prerequisites and permission | Expected result and evidence | Status |
| --- | --- | --- | --- |
| COPY-01: ordinary source | Trusted source with non-vaire owner, application schema/data; approved unused destination | Strict logical import; new vaire owner and generated credentials; source identities not adopted; compare schema/data and ordinary object ownership | NOT RUN |
| COPY-02: sizing | Approved named/custom-size fixtures and resource budget | Effective memory/CPU/connections match selection; insufficient budget refuses unless explicitly permitted overcommit; no storage-quota claim | NOT RUN |
| COPY-03: collisions | Existing/configured target; separately approved orphan fixture if available | Refusal before target mutation, including any-major cluster inventory and non-following orphan evidence; no automatic cleanup | NOT RUN |
| COPY-04: source failures | Approved bad-auth/unreachable/TLS/version fixtures; no uncontrolled retry | Safe diagnostic and no provisioning for password, reachability, certificate mismatch or PostgreSQL newer than 18; no plaintext fallback or secret exposure | NOT RUN |
| COPY-05: extensions/strict restore | Supported and unavailable extension fixtures; trusted SQL-error fixture | Compatible extension state retained; detectable incompatibility refused before provisioning; restore error fatal with truthful partial target | NOT RUN |
| COPY-06: access policy | Multiple application schemas, tables, sequences, functions and quoted identifiers | App/ro/migrate access follows normal vaire policy for existing and future objects; no ro write or extra function rights; extension internals excluded | NOT RUN |
| COPY-07: detach/credentials | Approved copy long enough for disconnect; safe unknown-reply test if available | Job survives disconnect; one-time destination credentials preserved when possibly used; follow exposes only sanitized progress/outcome, never credentials | NOT RUN |
| COPY-08: failure hygiene | Separately approved safe startup/export/import/crash/disk fault design | Correct failed phase and actual target presence; owned transient credentials/archive cleaned, no active/unknown workspace or database deletion, no replay | NOT RUN |
| COPY-09: source/backup | Approved read-only source observation and bucket-backed destination if available | Snapshot boundary explained; source unchanged beyond normal export activity/locks; logical import distinguished from backup readiness/warnings | NOT RUN |

## Limits and cleanup boundary

Source PostgreSQL versions, provider TLS behavior, unusual archive objects, extension compatibility, systemd abrupt-death cleanup and disk faults may lack safe fixtures. Mark these BLOCKED with the missing prerequisite instead of improvising unsafe faults. Never exhaust a real disk, kill a workload, modify source data or install arbitrary extension infrastructure to complete a case.

An archive can execute SQL: use trusted sources only. Logical export is not a physical clone, continuous replication or application cutover. Foreign-table contents and special extension state are not automatically copied. Destination-node reachability and provider allowlisting are prerequisites. Size is a compute/connection profile, not storage allocation.

A failed copy may leave a partial target. Explicitly authorized normal database drop is the only destination-fixture removal path; retry uses an unused name. Applying desired state may complete provisioning, not a failed data import. Record residual fixtures; never repair a defective release manually or auto-delete a target. No fixtures have been created for this work.
