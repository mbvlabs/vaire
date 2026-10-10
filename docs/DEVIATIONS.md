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

## M2: engine and node runtime

Entries from [`M2-ENGINE-NODE-RUNTIME.md`](./M2-ENGINE-NODE-RUNTIME.md) §10:

- **Dev builds compare `commit`.** The node-side version check (§4.2) requires `client_version` and `client_channel` to equal the node's; for dev builds (`channel = "dev"`) `client_commit` must match too, because every dev build reports `version = "dev"`. The M3 handshake will use the same rule.
- **RPC request additions.** `eru-rpc/v1` adds `client_commit`, `client_channel` and `lock_timeout_s` (default 30; timers pass 0) to the §7.4 request.
- **`eru-rpc/v1` and `upgrade.install` are frozen.** From M3 on, a newer operator calls `upgrade.install` on an older node, so request fields and the `upgrade.install` params (`path`, `sha256`, `version`, `commit`, `channel`) and result (`previous`, `installed`, `rolled_back`) only gain optional fields.
- **`cluster.toml` is decoded leniently until M4.** M2 reads `[cluster] name` (and `[e2e] value` in `e2etest` builds) and ignores unknown keys. M4 adds the schema and a strict decode.
- **`eru.lock.info` names only the most recent shared holder.** With several shared holders, a timed-out caller sees the last one to acquire.
- **§24 milestone split.** M2 is node-only and its scenario is E26. The version handshake, RPC client, binary push, `--auto-upgrade-nodes`, E18 and E27 move to M3.

Implementer choices made while building M2:

- **A wrong confirm token is refused up front.** §4.3 says the `apply` token is checked "only when a step needs a restart and `confirm` is set". Whether a restart is needed is known only after steps run, and dispatcher step 6 runs before the lock. A non-empty `confirm` that doesn't equal `{cluster}/restart` is therefore refused (exit 4) before any step runs, whether or not a restart turns out to be needed. An empty `confirm` never fails; it leaves the restart pending.
- **Pending restarts persist in the journal.** The journal gains an optional top-level `restart_pending` list of step IDs. Without it, a step that needs a restart would skip on the journal in the next run and the confirmed apply would have nothing to restart. A confirmed apply performs the restart once and clears the list. `plan` reports it too.
- **No restarter is always an engine error.** When a step needs a restart and the build has no restarter (production M2), apply fails with `error` whether or not `confirm` was given, rather than reporting a restart that can never happen.
- **"Strict" request decoding.** The request must be exactly one JSON object within 1 MiB, then EOF, with `schema = "eru-rpc/v1"`, a well-formed `method` and a ULID `request_id` (it names the run log). Unknown top-level fields are allowed so that a newer operator can add optional fields (§4.4). Method params decode strictly (`DisallowUnknownFields`), except `upgrade.install`, which crosses versions and decodes leniently.
- **Production test-string check.** §2 asks for `go tool nm bin/eru | grep -c ERU_TEST`. `bin/eru` is built with `-s -w`, so `nm` finds no symbols and the check would always pass. `just lint` greps the binary's bytes for `ERU_TEST` and `stub-*-v<N>` instead, which also covers string data, and checks that `eru commands --check-steps` reports 0 steps.
- **`upgrade.install` stages before it verifies.** The pushed file in `/tmp` is owned by the SSH user, who could change it between the sha256 check and the install. The node copies it into a root-only `/usr/local/bin/eru.new` (0700) while hashing, verifies and preflights that copy, and only then makes it 0755 and renames it into place. The copy and mode change are done in Go rather than with `install(1)`; the effect is the same. The `/tmp` source is deleted on every outcome after it passes the path and owner checks. Without an existing binary, a selfcheck failure removes the new one.
- **The `dirs` selfcheck also checks modes.** Root can write a directory with mode 000, so a pure write probe would pass. `dirs` also fails when the owner lacks `rwx` on `/var/lib/eru`, `/var/log/eru` or `/run/lock`. Missing directories are created (0700, 0750, 1777).
- **Run logs.** `plan` and `apply` write a run log; `selfcheck` and `apply` with `dry_run` don't. Lock and version refusals happen before the run log opens, so they leave none.
- **Scripts get a fixed environment.** `Shell` runs `/bin/bash -s` with a fixed `PATH`, `LANG`, `HOME` and `DEBIAN_FRONTEND` plus the step's validated keys, and doesn't inherit the caller's environment. The script runs in its own process group, which gets SIGTERM on cancel and SIGKILL 10 s later.
- **Exclusive holders remove `eru.lock.info` on release.** Shared holders leave it, and a holder whose PID is gone is reported as `null`.
- **Stub inputs.** `stub-fail-v1`'s inputs include whether `/etc/eru-e2e/fail` exists (as `stub-restart-v1`'s do for its file), so creating the file re-runs a completed step and E26 step 7 can fail it.

## Not yet chosen

These are filled in when the milestone that needs them lands:

- Exporter versions, and whether `pgbackrest_exporter` is replaced by eru textfile metrics (M8).
- Bloat estimation query source, vendored with licence and attribution (M9).
- Catalog extensions dropped because no PostgreSQL 18 package exists (M9).
