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
    - `eru db create scratch`, then `eru db drop scratch --yes`. Expected: refused, asking for the confirm token. `eru db drop scratch --confirm scratch` then drops it and warns `no_backup_before_drop`.
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
