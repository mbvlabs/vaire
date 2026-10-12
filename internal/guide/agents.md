# eru for agents

eru is a single CLI that installs, secures, backs up, restores, and maintains a PostgreSQL 18 cluster. The same binary runs on the operator machine and on the server. After bootstrap, remote actions go over SSH through WireGuard. The secrets handoff (shown once at the end of init, with a transfer bundle for `eru login`) is the credential an operator needs.

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
| `remote_unreachable`, `transport_failed`, `host_key_mismatch` | 9 |
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
- `no_backup_before_drop`: `eru db drop` removed a database without a backup (pgBackRest arrives in Phase 3).
- `orphaned_database`: a database exists on the node but is no longer in `cluster.toml`. Drop it with `eru db drop`.
- `restart_pending`: a step needs a PostgreSQL restart. `data.restart_pending` lists the steps.

## Node errors

- `precondition_failed` with `details.reason = "lock_held"`: another eru operation holds the node lock. `details.holder` names its PID, method, request ID and start time, or is `null` when that process is gone. Wait and retry; don't kill the holder.
- `restart_pending` warning: the change is applied, but a step needs a PostgreSQL restart that hasn't happened. `data.restart_pending` lists the steps. Ask the human for the `{cluster}/restart` token before re-running with it.
- `node_version_mismatch` (exit 10): the operator and node builds differ. `details.client` and `details.node` show both. Once the operator connects to nodes, it handles this with `eru upgrade` or `--auto-upgrade-nodes`.
- `precondition_failed` with `details.reason = "journal_corrupt"`: the node's step journal is unreadable. eru never resets it. Report it to a human.

## Workflows

Status: `eru status --output agent`. Follow with `eru doctor --output agent` when a check needs detail.

Backup: `eru backup now --type full --output agent`, then `eru backup verify --output agent`. `eru backup list` shows the PITR window.

Point-in-time restore: `eru restore --time <timestamp> --confirm <token>`. The cluster stays paused and PgBouncer stays stopped. Inspect with `eru sql`, then `eru restore promote` or `eru restore abort`.

Add a database: `eru db create <name> --output agent`.

Add an extension: `eru ext add <name> --db <name> --output agent`. When the result says a restart is required, ask the human for the confirm token before continuing.

Maintenance report: `eru maint report bloat --db <name> --output agent`.

Upgrade: `eru upgrade --output agent` updates only the local binary for now; node upgrades come with `eru node upgrade`. `release_verification_failed` must never be retried; report it to a human. Rolling a node back with `eru node upgrade --rollback` needs the human's confirm token.

## Refusals

- Printing the secrets handoff, the transfer bundle or its passphrase. Without a terminal, init and `secrets export` need `--secrets-out`/`--out` (the bundle is written there) and `--reveal-passphrase`; only pass `--reveal-passphrase` when the human asked for it.
- `eru restore cleanup` or `eru db drop` without explicit human approval of that action.
- `--sudo` or `--install-missing` without human approval of that escalation.
