# Deviations and implementer choices

The Phase 1 spec asks for a note here when an implementer chooses an option it leaves open, or departs from a SHOULD.

## Release domain

`ReleaseBaseURL` is `https://eru.mbvlabs.com` (`internal/buildinfo.ReleaseBaseURL`). The Phase 1 placeholder was `https://get.eru.sh`.

## Channel URL layout

Stable channel files (`install.sh`, `manifest.json`, `manifest.json.sigstore.json`) live at the domain root. Edge files live under `/edge/`. Every version also has an immutable tree at `/releases/<version>/`, including that version's `manifest.json`. The channel pointer is a byte-for-byte copy of the version manifest.

## Default branch for edge publishes

§19.1 says edge publishes from `main`. This repository's default branch is `master`. `publish-edge.yml` runs on push to `master`, and the keyless identity is `https://github.com/mbvlabs/eru/.github/workflows/publish-edge.yml@refs/heads/master`.

## `release_verification_failed`

Signature, identity, sha256, and preflight failures use `release_verification_failed` (exit 14) in `internal/cli/output/codes.go`, the agent guide exit table, and `e2e/schema/eru-v1.json`.

## `install.sh` cluster-init prompt

§23.2 asks the installer, when root on a supported Linux server with a TTY, to offer `eru cluster init --local`. That prompt is deferred to M3. `ERU_INSTALL_ONLY=1` still suppresses post-install next-step text.

## Cosign version

§23.3 floors cosign at `3.0.6`. `install.sh` and `mise.toml` pin `3.1.3`, with per-OS/arch checksums in `internal/versions`.

## Local `.prev`

The Phase 1 spec requires keeping `.prev` on nodes. `eru upgrade` also keeps `<target>.prev` locally via a hard link before the atomic rename.

## Package-manager-owned binaries

`eru upgrade` refuses to replace a binary under `/opt/homebrew/Cellar`, `/usr/local/Cellar`, or `/nix/store` (`precondition_failed`, `details.reason = "managed_by_package_manager"`).

## Linux-only node mode

Operator mode runs on Linux and macOS. Node-audience commands, plus `--local` cluster init and `wg export --install`, fail on non-Linux with `precondition_failed` (`node_mode_requires_linux`). `eru commands --json` includes `"platforms": ["linux"]` on those commands.

## top sampling

`eru top` reads PostgreSQL with pgx over the WireGuard tunnel. `top.sample` is the RPC used when the operator runs `--local` on the server.

## Flag-level destructiveness in CommandMeta

§5.4 lists the `CommandMeta` fields. eru adds `MutatingFlags` and `DestructiveFlags` (`mutating_flags`, `destructive_flags` in JSON). Some commands are destructive only with one flag: `queries --reset`, `db credentials --rotate`, `node upgrade --rollback`. Marking the whole command destructive would make an agent ask for approval before a plain `eru queries`. `--check-meta` requires every listed flag to exist, and requires `ConfirmToken` when `DestructiveFlags` is set.

## Not yet chosen

These are filled in when the milestone that needs them lands:

- Exporter versions, and whether `pgbackrest_exporter` is replaced by eru textfile metrics (M8).
- Bloat estimation query source, vendored with licence and attribution (M9).
- Catalog extensions dropped because no PostgreSQL 18 package exists (M9).
