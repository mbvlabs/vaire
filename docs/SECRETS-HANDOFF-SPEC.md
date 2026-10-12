# Spec: print the secrets handoff, don't write a file

Status: approved and implemented (2026-10-12). Came up after the round 3 manual test of Phase 2.

## Problem

`cluster init` ends by writing `./eru-<name>.secrets.age` on the operator machine (and `/root/eru-<name>.secrets.age` on the server for `--local`). This is the cluster's master document, but it ends up as a file next to wherever init happened to run, and the operator still has to move both the file and the passphrase into a password manager by hand. A `--local` init leaves the encrypted file on the server it protects.

## Goal

eru never writes the secrets document to disk by default. At the end of init it prints one **master handoff** in the terminal, and the operator copies it into their password manager. It has two parts:

1. **Explicit secrets**: every secret in plain text, labelled and grouped, so each can be copied into its own password-manager field or entry.
2. **Transfer bundle**: the same content as an ASCII-armored `age` block, plus its passphrase, so that `eru login` and `cluster init --restore` on another machine can take it by paste or stdin.

The operator then types `stored`, as today.

## The master handoff

Printed after the certificate wait, in this order, each part in its own panel. In a TTY the live area is suspended and every value is printed on its own line with no wrapping, so a triple-click copies one value.

### Part 1: explicit secrets

Grouped the way a password manager entry is laid out (title, username, password, URL, notes):

| Group | Values |
| --- | --- |
| Cluster | name, domain, stanza, eru version, created at |
| Node `<name>` | public address, WireGuard port, WireGuard address, WireGuard public key, SSH host key fingerprints |
| Operator `<name>` | admin user, sudo password, WireGuard address, WireGuard private key, SSH private key (multi-line block) |
| Database admin | `eru_admin` user and password |
| Database `<db>` (one per database) | `migrate`, `app`, `ro` users and passwords; `app` URL (6432), `migrate` URL (5432), direct TLS URL (443) |
| Services | `pgbouncer_auth` and `eru_monitor` passwords |
| Backups (Phase 3+) | provider, endpoint, region, bucket, prefix, access key ID, secret access key, pgBackRest cipher pass, restic password |
| Metrics (Phase 6+) | base URL, username, password |
| Break-glass | the break-glass text, as today |

Groups with nothing yet (backups and metrics in Phase 2) are left out, not printed empty.

### Part 2: transfer bundle

- `-----BEGIN AGE ENCRYPTED FILE-----` … `-----END AGE ENCRYPTED FILE-----` (`age` armor), scrypt with a new 8-word passphrase, same JSON schema as today (`eru-secrets/v1`).
- The passphrase is printed under the block, labelled "Bundle passphrase".
- One line under it: "On another machine: `eru login` and paste the block, or `eru login < bundle.txt`."

### Confirmation

`Type 'stored' once you've saved both parts in your password manager`. After `stored`, init asks whether to clear the screen (default yes in a TTY) so the secrets don't stay in the visible scrollback.

## Command changes

- **`cluster init`**
    - Default: print the handoff, write no file.
    - `--secrets-out <path>` stays as an explicit opt-in. It writes the armored bundle (not the plain secrets) with 0600, and the handoff then prints Part 1 and the passphrase only.
    - `--passphrase-out` is removed. The passphrase only ever appears in the handoff.
    - Non-interactive / structured output: see the decisions below.
    - A resumed init that stopped before `stored` prints a new handoff with a new passphrase. The old bundle still decrypts with its old passphrase, because the secrets inside haven't changed.
- **`cluster init --local`**: the same handoff is printed on the server console. Nothing is written to `/root`. The follow-up line becomes: "On your machine: `eru login` and paste the bundle, then run `eru status` within 30 minutes to confirm the lockdown."
- **`eru login [file|-]`**
    - No argument in a TTY: prompt "Paste the bundle", read until the `-----END AGE ENCRYPTED FILE-----` line, then prompt for the passphrase.
    - `-` reads the bundle from stdin. A file path still works.
    - Accepts both armored and binary `age`, so old `.secrets.age` files keep working.
- **`cluster init --restore [file|-]`**: same input rules as `login`.
- **`eru secrets export`**: prints the same handoff (both parts, new passphrase) and writes nothing unless `--out <path>` is given. Same `stored` confirmation in a TTY.

## Spec and code touched

- IMPLEMENTATION.md: the secrets document section (format, passphrase, login, export), the secrets document step of the remote init sequence, the `--local` section, the DR restore section, the flag list in the init section, and the command table rows for `login`, `secrets export` and `cluster init`.
- MASTER.md: the secrets document rows, the "leave the server exactly once" principle (now: shown once, never stored by eru), and the setup steps summary.
- Code: the secrets writer and the end of init in `internal/cli/clusterinit.go`, the summary card and a new handoff panel in `internal/cli/ui`, `login`, `secrets export`, and the `--restore` input.
- `agents.md`: agents get only the armored bundle via `--secrets-out`, and the passphrase only with `--reveal-passphrase`.

## Decisions

- **Non-interactive and structured runs** (agents, `--output json`, no TTY) must pass `--secrets-out <path>`. eru writes only the armored bundle there (0600). The passphrase appears in `data.passphrase` only with an explicit `--reveal-passphrase`; without it the run fails with `input_required` naming the flag. Plain secrets (Part 1) are never put in structured output. `--confirm-secrets-stored` is still required.
- **Part 1 includes the operator SSH private key and WireGuard private key**, so it is the complete master record.
- **Clear screen**: after `stored`, init asks to clear the screen, default yes.
