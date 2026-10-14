# eru for agents

eru is a single CLI that installs, secures, backs up, restores, and maintains PostgreSQL 18 databases on a server, each database in its own PostgreSQL cluster. The same binary runs on the operator machine and on the server. After bootstrap, remote actions go over SSH through WireGuard. The secrets handoff (shown once at the end of init, with a transfer bundle for `eru login`) is the credential an operator needs.

## Output

Use `--output agent` or `--json` for every call. `--json` is `--output json`. Read `ok` first.

- When `ok` is true, use `data` and follow `next` when the next step is unclear.
- When `ok` is false, read `error.code`, `error.message`, `error.hint`, and `error.details`.
- `--output agent` is the JSON envelope plus that command's `next` suggestions and a short `message`.
- `--jq` filters the envelope.
- `--stream` prints NDJSON events. The last line has `"type": "result"` and the envelope.

Human output is for a person at a terminal. Agents should use `agent` or `json`.

## Discovery

Run `eru commands --json` before choosing flags. Each catalog entry includes `meta`:

- `mutating`
- `destructive`
- `mutating_flags` and `destructive_flags`: flags that make an otherwise read-only or non-destructive command mutate or destroy, such as `eru queries --reset`
- `confirm_token`
- `needs_restart` (`never`, `maybe`, or `always`)
- `supports_dry_run`
- `audience` (`operator` or `node`)
- `remote`

`eru commands --path <command>` returns one command. Node commands are omitted from `eru --help` and carry `"audience": "node"`.

Pass `--dry-run` on mutating commands that set `supports_dry_run` before applying a change.

## Confirmation

Take confirmation tokens from the tool. When a command needs one, `error.code` is `unsafe_action_requires_confirmation` and `error.details.confirm_token` is the exact string. Ask the human before passing `--confirm` for a destructive command, or for a command with one of its `destructive_flags`. `--yes` never satisfies a destructive confirmation.

## Exit codes

| Code | Exit |
| --- | --- |
| success | 0 |
| `usage_error`, `invalid_output_mode` | 1 |
| `context_not_found`, `cluster_not_initialized` | 2 |
| `missing_tool` | 3 |
| `unsafe_action_requires_confirmation` | 4 |
| `step_failed`, `partial_failure` | 5 |
| `external_command_failed` | 6 |
| `config_error`, `validation_failed` | 7 |
| `ambiguous_input`, `input_required` | 8 |
| `remote_unreachable`, `transport_failed`, `host_key_mismatch`, `endpoint_unreachable`, `bucket_not_found`, `backup_auth_failed` | 9 |
| `node_version_mismatch`, `update_required` | 10 |
| `precondition_failed` | 11 |
| `lockdown_failed` | 12 |
| `secrets_error` | 13 |
| `release_verification_failed` | 14 |
| `error` | 70 |

## Warnings

A success envelope may carry `warnings[]`, each with a stable `code` and a `message`. Act on them; they never fail the command.

- `secrets_document_stale`: new database passwords were generated on the operator. Ask the human to run `eru secrets export` in a terminal to refresh the stored secrets.
- `tls_pending`: the ACME certificate is not issued yet. `eru-certs.timer` retries every hour; once DNS points at the server (DNS only, not proxied) it is issued without any action.
- `dns_mismatch` / `dns_proxied`: the domain doesn't resolve to the server yet, or is proxied by Cloudflare. Ask the human to fix the record (DNS only); init continues either way.
- `tls_unverified`: a connection used `sslmode=require` (self-signed mode) or DNS was not verified before continuing.
- `no_backup_before_drop`: `eru db drop` removed a database without a final dump. Its stanza is still kept (`data.stanza_kept`), so earlier pgBackRest backups stay restorable until they expire.
- `records_not_updated`: `eru db drop` kept the dropped database's stanza but couldn't record it in `records.json`. Report it to a human.
- `no_backups`: `eru backup list` found a database (or, with `--db`, that database) whose stanza holds no backup yet. Take one with `eru backup now --db <name> --type full`.
- `archive_status_unavailable`: `eru backup list` couldn't read a database's `pg_stat_archiver` (its cluster is likely stopped), so that row's window ends at its newest backup.
- `orphaned_database`: `eru db list` found a PostgreSQL cluster on the node that `cluster.toml` doesn't list. Ask the human before dropping it with `eru db drop`.
- `restart_pending`: a step needs a PostgreSQL restart. `data.restart_pending` lists the steps, as `<step>@<db>` for a database's cluster, and `data.confirm_tokens` the tokens that restart them.
- `context_tls_updated`: `cluster.toml` on the node has a different `domain` or `tls.mode` than the local context, and the context now follows it. After a domain change, eru's connections fail with a hostname mismatch until the node has a certificate for the new name: `eru-certs.timer` issues it within the hour, or the human runs `eru ssh --command 'sudo eru node run certs-renew'`.
- `no_offsite_backup`: the server has no bucket, so no backup leaves it. `server init` ends with it and every `eru apply` repeats it. Ask the human to run `eru backup setup`; the age recovery identity already exists, so setup reuses it. Setup restarts nothing: every database switches its WAL archiving to the bucket with a reload, gets its own pgBackRest stanza and takes a first full backup.
- `memory_overcommitted`: the databases' sizes add up to more than the server's cluster budget (RAM less the reserve); under load the databases are throttled and the page cache shrinks. `eru db create`, `db fork` and a growing `db resize` refuse to get there (see the node error below) unless the human passed `--allow-overcommit`; then they, and `plan`, `apply`, `config set` and `tune show`, carry this warning. Tell the human the numbers in the message and the options: shrink one with `eru db resize`, move one to its own node, or a bigger server.
- `cpu_crowded`: more than two databases per vCPU, or `cpu_quota` values that add up to more than the server's CPU. Informational: CPU weights are relative.
- `disk_crowded`: the data disk's used space plus every database's `max_wal_size` and the archive spool is over 80% of the disk.
- `first_backup_failed`: `eru db create` (or `db fork`) made the database, but its pgBackRest stanza, first full backup or backup timers failed, so it isn't backed up yet. The database works. Run `eru apply --output agent` to retry; if it fails again, read the step's error.
- `after_cutover_failed`: `eru restore promote` or `undo` switched the database over, but reloading the poolers, pointing it at its stanza or starting its full backup failed. The database serves the data it was switched to. Run `eru apply --output agent`, then `eru backup now --db <db> --type full`.
- `restore_dump_errors`: `pg_restore` reported errors while `eru restore <db> --dump` loaded the dump into `<db>__restoring`. Inspect the copy with `eru sql --db <db>__restoring` before promoting it.
- `pooler_reload_failed`: a restore became ready, or was aborted, but the session pooler wasn't reloaded, so its `<db>__restoring` entry is missing (`eru sql --db <db>__restoring` fails) or stays behind. From `eru db fork`: the fork exists, but neither pooler routes to it yet. Run `eru apply --output agent`.
- `other_config_failed`: `eru db create`, `db fork` or `db drop` finished, but applying the other databases' changed configuration (their new share of the server's disk and CPU) failed. Those databases keep serving with their old settings. Run `eru apply --output agent` to retry.
- `shrink_below_use`: `eru db resize` set a database's memory below what its cluster uses right now. It goes ahead; expect throttling until the cluster settles.
- `context_sync_failed`: the command succeeded, but the local context could not be updated from `cluster.toml`. `eru apply` retries it.

## Node errors

- `precondition_failed` with `details.reason = "memory_overcommitted"`: `eru db create` (with or without `--size`), `db fork` or a growing `db resize` would push the databases' sizes past the server's memory budget, so nothing was changed. The message has the numbers. `details.largest_size` and `details.suggested_command`, when present, are the largest size that still fits and the complete command for it; offer that to the human. Without them no size fits: the human can resize another database down or use a bigger server. Only the human decides to overcommit; then add `--allow-overcommit`.
- `precondition_failed` with `details.reason = "would_not_start"`: the database's PostgreSQL couldn't start, because its shared memory doesn't fit in what is left of the kernel's `CommitLimit`, or it requires huge pages the pool can't supply. `eru db create`, `db fork` and `db resize` refuse it before changing anything. `details.smallest_size`, when present, is the smallest size that would start; offer it to the human.

- `precondition_failed` with `details.reason = "lock_held"`: another eru operation holds the node lock. `details.holder` names its PID, method, request ID and start time, or is `null` when that process is gone. Wait and retry; don't kill the holder. A backup job skipped with `details.result.lock = "heavy_io"` met another database's full, diff, verify, drill or dump: each database's timers run at their own offset in the maintenance window, and only one of those jobs runs at a time.
- `restart_pending` warning: the change is applied, but a step needs a PostgreSQL restart that hasn't happened. `data.restart_pending` lists the steps. Each database is its own PostgreSQL cluster and restarts on its own: ask the human for the tokens in `data.confirm_tokens` (`<db>/restart`, or `{server}/restart` for every database, one at a time) before re-running with one `--confirm` per token. `eru apply --db <db>` limits a run to one database's steps.
- `node_version_mismatch` (exit 10): the operator and node builds differ. `details.client` and `details.node` show both. Once the operator connects to nodes, it handles this with `eru upgrade` or `--auto-upgrade-nodes`.
- `precondition_failed` with `details.reason = "journal_corrupt"`: the node's step journal is unreadable. eru never resets it. Report it to a human.

## Workflows

Status: `eru status --output agent`. Follow with `eru doctor --output agent` when a check needs detail.

Backup: `eru backup now --type full --output agent`, then `eru backup verify --output agent`. Each database has its own stanza; without `--db` both run once per database, one after another, and fail at the end if any database failed (`details.failed`). `eru backup list` shows one row per database with its PITR window, then the dropped databases whose stanzas are kept until they expire; `--db <name>` shows that database's sets. A dropped database's stanza is deleted by the daily `eru-stanza-expiry` timer once its restore window (`retention_full` at the drop) has passed.

Point-in-time restore of one database: `eru restore <db> --time <timestamp> --confirm <db>/restore`. eru restores the database's stanza into the temporary cluster `<db>__restoring`, paused at the target, while the live database keeps serving. Inspect it with `eru sql --db <db>__restoring` and `eru restore status <db>` (row estimates against live), then `eru restore promote <db> --confirm <db>/restore` (an in-place cut-over: same URL and credentials, a few seconds of downtime plus the replay to the target, then a full backup starts) or `eru restore abort <db>`. `eru restore undo <db>` swaps the old data back while its `<db>__pre_restore_<time>` cluster exists; `eru restore cleanup <db>` drops it. When only some rows are needed, prefer `eru db fork <db> --at <timestamp>`: a separate database at that point, without touching the live one. A restore from a dump (`--dump <id>`, for points older than the window) gives the database a new stanza on promote; points before it are in the old stanza, which `eru restore` doesn't read.

Add a database: `eru db create <name> --output agent`. Each database is its own PostgreSQL cluster with a size (`--size nano|small|medium|large|xlarge|custom`, `--memory 768MiB` to override the size's memory, `--profile oltp|mixed|analytics`). Without `--size` or `--memory` it gets `[postgres] default_size`. A size that doesn't fit the server's free budget is refused (`memory_overcommitted`, with the largest size that fits); `eru tune show` prints the server's budget sheet first if you want to choose. Resize one with `eru db resize <db> --size <size>` or `--memory <amount>`: limits and reload-only settings change live, and the restart that `shared_buffers`, `max_connections` and `huge_pages` need restarts that database only, so ask the human for `--confirm <db>/restart` or use `--defer-restart`. `eru db list` shows each database's node, host, size, limits, stanza and state. The result carries the role passwords and connection URLs once; eru does not keep them, so hand them to the human or the app's secret store right away. The app and ro URLs go through the transaction pooler (PgBouncer) on 6432. The migrate URL goes through the session pooler on 5432 and only works there: the transaction pooler refuses the migrate role (it fails as an authentication error), because transaction pooling breaks advisory locks, session `SET` and temporary tables. Both poolers route by the database name in the URL, and each database runs in its own PostgreSQL cluster that listens on a socket only. The URL's host is the hostname of the node the database runs on, `<node>.<domain>` (`db1.<domain>` for the master), and the database is the path: one DNS name and one certificate per node, so creating or dropping a database never touches DNS or the certificate. Run migrations from a machine with eru through `eru tunnel --db <name> --target postgres`, or from CI with `network.expose_postgres = true` and the runner's address in `network.client_allowlist`; changing those settings opens a port, so ask the human first. A lost password is replaced with `eru db credentials <name> --rotate --role <role> --confirm <name>`, which breaks clients still using the old one: ask the human first.

Commands may run in parallel on one context. They share the context's WireGuard tunnel: the first command starts a background tunnel daemon, and it exits after 10 minutes with no command using it. `eru tunnel status` shows it, and `eru tunnel stop` ends it. If a command loses its connection, the node finishes the work anyway; job commands re-attach by themselves.

Query a database: `eru sql --db <name> "<sql>" --output agent`. It connects as `eru_admin` and is read-only unless `--write`, which acts as the database's owner role so new tables get the app's default grants. `eru sql`, `psql`, `console` and `tunnel --target postgres` reach the session pooler on 5432 of the node the database runs on (eru finds it in the context's cache), and `--db` picks the database; `tunnel --target pgbouncer` reaches that node's transaction pooler on 6432. `eru psql` and `eru console` are interactive and for humans only; `eru console` refuses to run without a terminal.

Add an extension: `eru ext add <name> --db <name> --output agent`. When the result says a restart is required, ask the human for the confirm token before continuing.

Maintenance report: `eru maint report bloat --db <name> --output agent`.

Upgrade: `eru upgrade --output agent` updates only the local binary for now; node upgrades come with `eru node upgrade`. `release_verification_failed` must never be retried; report it to a human. Rolling a node back with `eru node upgrade --rollback` needs the human's confirm token.

## Refusals

- Printing the secrets handoff, the transfer bundle or its passphrase. Without a terminal, init and `secrets export` need `--secrets-out`/`--out` (the bundle is written there) and `--reveal-passphrase`; only pass `--reveal-passphrase` when the human asked for it.
- `eru restore cleanup` or `eru db drop` without explicit human approval of that action.
- `--sudo` or `--install-missing` without human approval of that escalation.
