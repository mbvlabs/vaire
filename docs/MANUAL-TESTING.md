# Manual testing checklist

Checks that haven't been run on a real VPS yet. Phases 1 and 2 were marked done on 2026-10-12 with these still open; they don't block Phase 3. Tick each one off with the date, eru version and VPS (provider, OS, size), and fix whatever it turns up straight away.

## Phase 1: Bootstrap

- [ ] **A broken tunnel during lockdown reverts by itself**
    - On a fresh VPS, start `eru cluster init --host root@<vps>` and break WireGuard during the commit-confirm move of SSH to wg0 (for example, block the WireGuard UDP port on the laptop).
    - Expected: the node restores public SSH on 22 by itself after the confirm window, and `eru cluster init --resume` can carry on.
- [ ] **`cluster init --local`**
    - On a fresh VPS, install with `curl | sudo bash`, run `eru cluster init --local`, paste the transfer bundle into `eru login` on the laptop, and run `eru plan` from there.

## Phase 2: First cluster

- [ ] **A second `eru apply` changes nothing**
    - Run `eru apply` (the `wal_level = logical` change restarts PostgreSQL once), then `eru apply` again. Expected: every step skips with `journal`, and there's no restart or reload.
- [ ] **`db drop` with only `--yes` is refused**
    - `eru db create scratch`, then `eru db drop scratch --yes`. Expected: refused, asking for the confirm token. `eru db drop scratch --confirm scratch` then drops it and warns `no_backup_before_drop` (without backups configured; from Phase 3 it takes a final dump instead).
- [ ] **`eru console`**
    - `eru console <db>` downloads usql when it's missing (checksum verified), opens a session over the tunnel, and `select current_user, ssl from pg_stat_ssl where pid = pg_backend_pid()` shows the read-only role and `ssl = t`. Ctrl-C cancels a running query without leaving the console.
- [ ] **The migrate role is refused by PgBouncer**
    - After `eru apply`, connect as `<db>_migrate` to 6432 from an allowlisted address. Expected: an authentication error. The same URL on 5432 through `eru tunnel --target postgres` connects, and `select current_user` shows `<db>_owner`. `<db>_app` on 6432 still connects. `eru console <db> --write` opens and `show role` is `<db>_owner`.
- [ ] **Changing the domain follows through to the certificate and the context**
    - Add a DNS record for a new name (DNS only), then `eru config set domain <new>`. Expected: a `context_tls_updated` warning, and `grep domain ~/.config/eru/contexts/<name>/context.toml` shows the new name. `eru apply` re-runs `tls-v1`. Within the hour (or after `eru ssh --command 'sudo eru node run certs-renew'`, which logs `installed the certificate for <new>`) `eru psql --db <db>` connects without a hostname mismatch. A second `sudo eru node run certs-renew` logs that the certificate is current until its expiry.
- [ ] **`eru node run` off a node is refused**
    - On the operator machine, `eru node run certs-renew`. Expected: `precondition_failed` saying this machine is not an eru node, with the `eru ssh` command as the hint, and no "ran certs-renew".
- [ ] **Direct TLS on 443**
    - From an allowlisted address, connect with `sslnegotiation=direct` (PostgreSQL 17+ libpq or pgx 5.7+) to port 443. Expected: it reaches PgBouncer. Without `sslnegotiation=direct` it fails with "server refused TLS connection", and from a non-allowlisted address it's refused.

## Phase 0: Foundations

- [ ] **Edge install and upgrade**
    - Install from the edge channel once on Linux and once on macOS, then run `eru upgrade` against a second edge release.

## Passed

- 2026-10-12, Hetzner, Ubuntu 26.04: `cluster init` from a laptop to public 22 closed and SSH over WireGuard; the secrets handoff accepted by `eru login`; ACME certificate issued; `eru sql` with verify-full; `eru apply --auto-upgrade-nodes`; an app (`andurel db seed`) connecting to 6432 with `sslmode=verify-full` through PgBouncer.

## Phase 3: Database backups & restore

Run on a real VPS against Cloudflare R2. Phase 3 stays 🟡 in [`MASTER.md`](./MASTER.md) until all of these pass.

- [ ] **Backup setup on an existing cluster**
    - On a Phase 2 cluster with databases `app` and `other`, run `eru backup setup` first. Expected: refused with `node_outdated`. Run `eru apply --auto-upgrade-nodes` (it re-runs `postgres-packages-v1` and installs pgBackRest), then `eru backup setup` against R2. Expected: one PostgreSQL restart (the prompt says so and asks for `<cluster>/restart`), a first full backup, and `eru backup list --db app` shows a restore window.
    - `eru secrets export` carries the recovery kit (bucket, keys, cipher pass, dump age identity), and `eru login` on a second machine accepts it.
- [ ] **Rewind one database while another keeps serving**
    - Under a write loop on `other`, write rows to `app`, note the time, write more, then `eru restore app --time <t> --confirm app/restore`. Expected: it ends ready; `eru sql --db app__restoring "select count(*) from …"` shows only the early rows; `eru restore status app` compares row estimates.
    - `eru restore promote app --confirm app/restore` leaves only the early rows in `app`, the app reconnects with the same credentials, and the write loop on `other` never stopped.
- [ ] **Undo and cleanup**
    - `eru restore undo app --confirm app/restore` brings the newer rows back; `eru restore cleanup app --confirm app/cleanup` removes the leftover.
- [ ] **Abort**
    - `eru restore app --time <t> --confirm app/restore`, then `eru restore abort app`. Expected: `app` unchanged, `app__restoring` gone.
- [ ] **Detach and re-attach**
    - Close the laptop lid during `eru backup now --type full`; later `eru job follow <id>` (or `eru job list`) shows it finished.
- [ ] **Drill**
    - `eru restore drill` passes and shows in `eru backup list`.
- [ ] **Fork**
    - `eru db fork app --at <t>` creates a new database with its own credentials and the rows as of `t`; `eru db fork app` without `--at` copies the live database.
- [ ] **Long-retention dumps**
    - `eru config set databases.app.backup.dump_schedule "*-*-* *:0/15:00"`, `eru config set databases.app.backup.dump_retention 3`, `eru apply`: a scheduled dump of `app` lands in the bucket; `eru dump get app <id> --out app.dump` decrypts it and `pg_restore --list app.dump` reads it.
    - `eru db drop <scratch db> --confirm <db>` leaves a `-final` dump (`eru dump list`).
- [ ] **Idempotence**
    - A second `eru apply` changes nothing.
- [ ] **No unconfirmed restarts**
    - `eru config set` on a reload-only setting (for example `postgres.work_mem`), then `eru apply`. Expected: a reload, and `pg_postmaster_start_time()` unchanged. A restart-only setting reports a pending restart and waits for `<cluster>/restart`.

Found while testing (2026-10-12):

- Fixed: `postgres-config-v1` restarted PostgreSQL on every change without the confirm token and recorded `restart: none`.
- Fixed: `backup list` showed `IN REPO 0 B`, because pgBackRest 2.59 omits `repository.size`.
- Fixed: `backup setup` showed the cluster-password warning for a new recovery kit.
- Fixed: `< /dev/null` did not count as a non-terminal stdin.
- Fixed: count-based retention. With `retention_full = 1`, a manual `backup now --type full` expired the previous full and its WAL, which shrank the restore window. Retention is now time-based (`repo1-retention-full-type=time`, weeks × 7 days).
- Open: `restore` and `db fork` report `must be owner of extension pg_stat_statements` from `pg_restore` (harmless, but it marks every restore with a warning).
- Fixed: a failed restore blocked the next one until `eru restore abort`; a new restore now clears a failed one itself.
- Open: `eru job follow` on a finished job prints only its state, not its log, and drops the error details (`details.output_tail` is null).
- Fixed: the restore window started at the oldest backup's stop second, which pgBackRest refuses (`[075] unable to find backup set with stop time less than …`); it now starts one second later.
- Open: `eru ssh --command "sudo …" | jq` mixes sudo's password prompt and the PTY's `\r` into stdout. Workaround: write to a file with sudo, then read it back with `eru ssh --no-input`.
