# eru M1: Release & install — implementation plan

> Executes [`M1-RELEASE-INSTALL.md`](./M1-RELEASE-INSTALL.md) (the spec). The spec is the source of truth for *what*. This plan is the *order of work* and how each step is checked.
> Branch: `v1-0-0` (base `9d969fb`, M0). Status: ready to execute · 2026-10-09

## Ground rules

- Tasks run in order. Each ends with a **Check** that must pass before the next task starts.
- No `_test.go` outside `e2e/`. Checks are `just lint`, `just build`, shell runs against `dist/`, and E25.
- Commit after each task (`feat(m1): …`), so a bad step can be reverted on its own.
- Spec references below are written `S§n`.

---

## T1. Contract changes (codes, platform gate, domain)

The small, cross-cutting edits other tasks depend on.

1. `internal/cli/output/codes.go`: add `CodeReleaseVerification = "release_verification_failed"`, `ExitReleaseVerification = 14`, and the map entry.
2. `internal/guide/agents.md`: add the exit-14 row to the exit table.
3. `e2e/schema/eru-v1.json`: allow the new code. Add an optional `platforms` array to the command-meta schema.
4. `internal/buildinfo/buildinfo.go`: `ReleaseBaseURL = "https://eru.mbvlabs.com"`.
5. `internal/cli/meta.go`: add `LinuxOnly bool` → JSON `"platforms": ["linux"]` (omitted when false). Make `--check-meta` fail on any `Audience == "node"` command without `LinuxOnly`.
6. `internal/cli/tree.go`: set `LinuxOnly` on every node-audience command, on `cluster init` (gated only when `--local` is set; see 7), and on `wg export` (gated only with `--install`).
7. `internal/cli/platform.go` (new): a root `PersistentPreRunE` hook that returns `precondition_failed`, `details{reason: "node_mode_requires_linux", os}`, when `LinuxOnly` applies and `runtime.GOOS != "linux"`. Flag-scoped cases (`--local`, `--install`) use a `LinuxOnlyFlags []string` meta field mirroring `DestructiveFlags`. `--check-meta` verifies those flags exist.
8. `internal/cli/root.go`: wire the hook in and chain it with any existing persistent pre-run. Add the hidden global `--escalated` flag in `flags.go`.

**Check:** `just lint` green. `GOOS=darwin go build ./cmd/eru` succeeds. `bin/eru commands --json | jq '[.. | objects | select(.audience=="node") | .platforms] | unique'` → `[["linux"]]`.

---

## T2. Pinned versions (`internal/versions`)

1. `internal/versions/versions.go`: `PostgresMajor = 18`, `CosignVersion` (latest stable 3.x ≥ 3.1.3) and `CosignSHA256` map keyed `"<os>-<arch>"` for linux/darwin × amd64/arm64, taken from the release's `cosign_checksums.txt`.
2. `internal/versions/cmd/print/main.go`: prints `cosign_version=…` and `cosign_sha256_<os>_<arch>=…` lines for the lint check.
3. Bump `mise.toml` `cosign` to the same version.

**Check:** `go run ./internal/versions/cmd/print` prints 5 lines. Manually `curl` each cosign binary and confirm its sha256 matches.

---

## T3. `internal/release`: manifest, identity, verification, cache

1. `go get github.com/sigstore/sigstore-go@latest`. `go mod tidy`.
2. `manifest.go`: `Manifest`, `Artifact`, `ChannelRoot(channel)` (stable → base, edge → base+`/edge`), version regexps (S§2.3), `Validate(m, channel, wantVersion)` with the full S§4.2 step-3 rule set.
3. `identity.go`: `Identity(channel, version)` → exact SAN (S§2.4). Issuer constant.
4. `fetch.go`: HTTPS-only `http.Client` (30 s timeout; a `CheckRedirect` that rejects off-host redirects and any non-https redirect), `getCapped(url, max)` for 1 MiB metadata, and errors mapped to `remote_unreachable` with `details.url`. A 404 on a version manifest maps to `validation_failed`.
5. `verify.go`: a `Verifier` interface with `VerifyBlob(ctx, content, bundle, identity) error`. The production impl uses a sigstore-go TUF client (cache `$XDG_CACHE_HOME/eru/sigstore-tuf`), `verify.NewVerifier(trustedRoot, WithTransparencyLog(1), WithObserverTimestamps(1))`, and a policy of `WithArtifact` + `WithCertificateIdentity(issuer, SAN exact)`. Every failure maps to `release_verification_failed`.
6. `client.go`: `Resolve(ctx, channel, version)` implementing S§4.2. Peek the untrusted version for the channel manifest, verify, then do strict decode + `Validate`. `Fetch(ctx, m, goos, goarch)` implementing S§4.3: cache path under `$XDG_CONFIG_HOME/eru/cache/releases/<v>/<os>-<arch>/`, re-hash on hit, streamed download with a 256 MiB cap, `subtle.ConstantTimeCompare`, temp + rename, modes 0700/0600.
7. `verify_e2etest.go` (`//go:build e2etest`): the `Verifier` impl that trusts a cosign public key from `ERU_TEST_RELEASE_PUBKEY` (sigstore-go `root.NewTrustedPublicKeyMaterial`). `ReleaseBaseURL` is overridable by `ERU_TEST_RELEASE_BASE_URL`. `verify_prod.go` (`//go:build !e2etest`) selects the production verifier. Neither env var is read in a non-e2etest build.

**Check:** `just lint`. `go build -tags e2etest ./...`. Confirm with `go tool nm bin/eru | grep -i ERU_TEST` (expect nothing) that the production binary has no `ERU_TEST_` strings. A throwaway `go run` against any public sigstore-signed blob (for example a cosign release's own bundle) passes with the right identity and fails with a wrong one. Delete the throwaway afterwards.

---

## T4. `scripts/build-release.sh` + `just release-local`

1. Write the script per S§3.1: validation, 4 cross-builds with ldflags (Version/Commit/Channel), portable sha256 helper, `jq -n` version manifest, byte-identical channel copy, `install.sh` render with the `@@CHANNEL_ROOT@@`/`@@CHANNEL@@` substitution, and the leftover-`@@` guard.
2. `justfile`: `release-local VERSION CHANNEL BASE_URL`.
3. Check `jq` presence in the script (fail with a clear message if it's missing).

**Check:** `just release-local edge-000000000001 edge https://eru.mbvlabs.com` produces the S§2.1 tree under `dist/release-edge/`. `cmp` channel vs version manifest. Each binary's `file` output shows the right OS/arch. `dist/…/linux/amd64/eru version --json` reports `edge-000000000001`/`edge`. Stable with `1.2.3` puts `install.sh` and `manifest.json` at the root. Bad inputs (`edge-xyz`, `http://…`, `stable 1.2`) are rejected. `shellcheck -x` clean.

---

## T5. `scripts/install.sh` template

1. Port `deploycrate-ce/scripts/install-channel.sh` per S§5: Linux + Darwin, bash 3.2-safe, pinned cosign per `<os>-<arch>` in a temp dir (not persisted), manifest verify (anchored identity regexp per channel), `sed` extraction, the channel/`releaseBaseUrl` checks, artifact cosign verify + manifest-sha check, smoke-run `version --output json`.
2. Install target: root → `/usr/local/bin`, else `${ERU_INSTALL_DIR:-$HOME/.local/bin}`. Always temp + `mv` in the same dir. Print the PATH hint per shell (`bash`/`zsh`/`fish`) when needed.
3. `ERU_INSTALL_ONLY=1` suppresses the next-step text. No `sudo`, no cluster-init prompt (M3).
4. Lint pin check in `justfile`: compare `scripts/install.sh` cosign version and checksums against `go run ./internal/versions/cmd/print`.
5. For E25: the e2etest render substitutes the `cosign verify-blob` identity flags with `--key "$ERU_TEST_RELEASE_PUBKEY_URL"`. Implement it as a third placeholder `@@COSIGN_TRUST_ARGS@@` that `build-release.sh` fills with the production identity args by default and with `--key …` when `ERU_RELEASE_TEST_KEY` is set. The guard keeps a production render from ever shipping the key form (it rejects `--key` when the base URL is `buildinfo.ReleaseBaseURL`).

**Check:** `just lint` (shellcheck + pin check). Serve a locally built, key-signed `dist/release-edge` (`cosign sign-blob --key`) over HTTPS with a local CA, run the rendered installer as root and non-root on Linux, and on a Mac: correct paths, correct version, PATH hint shown. Then flip one artifact byte → the installer fails, and the old binary is untouched.

---

## T6. Sudo escalation (`internal/localenv/escalate.go`)

1. `Escalate(cmd, reason) (handled bool, err error)` per S§6.4: root or `--escalated` → false; `sudo -n true` → re-exec; TTY + human → prompt (reusing the M0 prompt helpers in `internal/cli/prompts.go`, exposed through a small interface so `localenv` doesn't import `cli`); otherwise `precondition_failed` with `root_required` / `sudo_password_required` and `details.command`.
2. Re-exec: `sudo [-n] <os.Executable()> <args…> --escalated` plus caller-supplied extra args. Inherit stdio, propagate the child's exit code, and print no parent envelope.
3. Hook the exit-code propagation into `cli.Main()`: a sentinel error type carrying the child's exit code, which the output layer passes through silently.

**Check:** In a Debian VM: `/usr/local/bin` owned by root, run as a user with passwordless sudo → re-execs. With a password-required sudo and `--json` → exit 11 `root_required`, with the exact `sudo` command. With `--sudo` → exit 11 `sudo_password_required`. Covered automatically by E25 step 5 in T9.

---

## T7. `internal/upgrade` + `eru upgrade`

1. `internal/upgrade/plan.go`: resolve self (`os.Executable` + `EvalSymlinks`), the package-manager-prefix refusal, channel selection (`dev` without `--channel` → `input_required`), and the decision table from S§6.2 step 4 (same → no-op; stable auto-downgrade → no-op; explicit downgrade → `downgrade` warning; channel switch → `channel_changed` warning). Semver compare is a tiny local parser; don't add a dependency.
2. `internal/upgrade/swap.go`: pre-flight the cached binary (`version --output json`, 10 s, version + channel match, else `release_verification_failed`). Hard-link `target` → `target.prev` (replacing an old `.prev`; on failure, a `no_previous_kept` warning). Copy to `.eru.new-<rand>` in the target dir, chown to the old binary's uid/gid when root, chmod 0755, fsync the file and dir, rename.
3. `internal/cli/upgrade.go`: `runUpgrade` wiring plan → dry-run → TTY confirm (`--yes` skips) → `localenv.Escalate` when the dir isn't writable (passing `--version <resolved> --channel <resolved>`) → `release.Fetch` → swap → envelope per S§6.3. Unless `--skip-nodes`, a `nodes_not_upgraded` warning and `nodes: "skipped"`.
4. `tree.go`: point `upgrade` at `runUpgrade`, add the `--yes` flag and `ErrorCodes`. Set `Next: ["eru version"]` for the M1 behaviour (M10 restores `eru node upgrade`).

**Check:** Run against a local HTTPS release server serving two key-signed builds made with `-tags e2etest`: upgrade, no-op re-run, `--dry-run` (no cache writes; check with `ls` of the cache dir), `--version` absent → exit 7, a tampered artifact → exit 14 and an unchanged binary (sha256 before/after), `.prev` present and runnable. On macOS: the upgraded binary execs fine (no SIGKILL).

---

## T8. Signing, upload, workflows

1. `scripts/sign-release.sh RELEASE_ROOT`: sign + verify both manifests and the 4 binaries with the exact `GITHUB_WORKFLOW_REF` identity (S§3.2).
2. `scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT`: immutable `releases/` → `install.sh` → manifest bundle → manifest last, with the cache headers and content types from S§3.3.
3. `.github/workflows/publish-edge.yml` per S§7.1: SHA-pinned actions, `mise-action`, a config validation step including the `RELEASE_BASE_URL == buildinfo.ReleaseBaseURL` grep, `just lint`, build/sign/upload.
4. `.github/workflows/publish-stable.yml` per S§7.2: ancestor-of-`origin/master` check, `gofmt`/`go vet`, `just lint`, build/sign, refuse to overwrite, upload. No `go test`.
5. `just lint` already shellchecks `scripts/*.sh`. Add `actionlint` to `mise.toml` and `lint` if available; otherwise skip and note it.

**Check:** `shellcheck` + `actionlint` clean. Dry-run `upload-release.sh` with `aws` shimmed to `echo`; the printed order matches S§3.3. Real validation happens in T10.

---

## T9. E25 (`e2e/release_test.go`)

1. Harness `harness.ReleaseServer(t)`: a Caddy container on the CI host serving a temp dir over HTTPS with a Pebble-issued certificate (reuse M0's Pebble setup), returning its URL. The VM trusts Pebble's root (existing wiring).
2. Harness `harness.BuildRelease(t, version, url, keyDir)`: run `cosign generate-key-pair` once per test (empty password via `COSIGN_PASSWORD=`), run `scripts/build-release.sh` with `ERU_RELEASE_TEST_KEY` and `go build -tags e2etest`, and sign with `cosign sign-blob --key --bundle`.
3. Test steps 1–9 exactly as listed in S§8, each asserting envelope code, exit code and on-disk state (sha256 before/after, `.prev`, cache dir contents).
4. Make sure the e2e workflow has cosign (it already uses `mise-action`, which installs it from `mise.toml`).

**Check:** `just e2e` passes locally (incus + docker), and the PR's e2e job is green on Debian 13.

---

## T10. Infrastructure, first edge release, manual scenarios

Needs the maintainer (not code):

1. Create the R2 bucket and the `eru.mbvlabs.com` custom domain (or confirm the final name and redo T1.4 if it differs).
2. Create GitHub environments `edge` and `stable` with `RELEASE_BUCKET`, `RELEASE_BASE_URL` (vars) and `R2_ENDPOINT`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY` (secrets).
3. Merge to `master` → first edge publish. Check the bucket layout against S§2.1 and `curl -I` the cache headers.
4. Run **M1-A** (Debian 13 VPS, root + user) and **M1-B** (macOS: install, `version`, `eru node version` → exit 11 `node_mode_requires_linux`).
5. Push a second commit → **M1-C**: `eru upgrade` on both machines, then a no-op re-run.
6. Record date, versions and machines in the PR description.

---

## T11. Docs and close-out

1. `docs/DEVIATIONS.md`: replace the `get.eru.sh` note with `eru.mbvlabs.com`, and add every S§10 entry (channel layout + version manifest, `master` vs `main`, exit 14, cluster-init prompt deferred, cosign version, local `.prev`, package-manager refusal, Linux-only node mode + `platforms`).
2. `docs/PHASE-1-IMPLEMENTATION.md`: mark M1 done in §24 (a status column or note), and point §19.1/§23.2 at the deviations.
3. `internal/guide/agents.md`: add `eru upgrade` to the "upgrade" workflow (local only until M10), plus a line that `release_verification_failed` must never be retried and must be reported to a human.
4. Final `just lint` + `just e2e`.

**Done when:** every check above passes, M1-A/B/C are recorded, and the spec's S§0 exit criteria hold.

---

## Risks during execution

| Risk | Mitigation |
| --- | --- |
| sigstore-go API churn vs examples | Pin the version in `go.mod`. Keep all sigstore-go use inside `verify.go` |
| TUF first fetch fails on restricted networks | Error maps to `remote_unreachable` with the TUF URL in `details`. The cache persists after the first success |
| Key-based bundles from `cosign sign-blob --key` not accepted by the sigstore-go public-key path | Spike this first thing in T3.7. If it doesn't work, fall back to verifying test signatures with cosign's own `verify-blob --key` behind the same `e2etest` tag |
| macOS can't be tested in CI | M1-B is manual. Keep darwin-specific code to the rename path and the platform gate |
