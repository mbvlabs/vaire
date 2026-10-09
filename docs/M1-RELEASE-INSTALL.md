# eru M1: Release & install — implementation spec

> Audience: the agent (or engineer) implementing milestone M1 of [`PHASE-1-IMPLEMENTATION.md`](./PHASE-1-IMPLEMENTATION.md) (§24).
> Builds on M0 (commit `9d969fb`): cobra tree, `output` port, error/exit codes, CommandMeta + `--check-meta`, e2e harness.
> Parent sections: §19 (Upgrades), §23 (Release engineering), §20.3 (Sudo escalation), §6.3 (operator files).
> Status: draft for review · 2026-10-09

---

## 0. Goal and exit criteria

M1 makes eru **shippable**: a commit on `master` produces a signed edge release, `curl … | bash` installs it on Linux and macOS, and an installed `eru` can replace itself with a newer signed release.

**Done when** (Debian 13 amd64 + `just lint`, per §24):

1. Pushing to `master` runs `publish-edge.yml`, which builds, signs and uploads an edge release for linux/darwin × amd64/arm64.
2. **Manual scenario M1-A** (Linux) and **M1-B** (macOS), recorded in §9: the edge `install.sh` installs a working `eru` as root and as a normal user; `eru version --json` reports `channel: "edge"` and the expected version.
3. **Manual scenario M1-C**: after a second push, `eru upgrade` moves the installed binary to the newer edge version, and a tampered manifest/artifact is refused.
4. Automated **E25** (§8) passes in CI.

**Not in M1:** node upgrade, rollback, binary push to nodes (M10/M2), `install.sh` → `cluster init --local` hand-off (M3, see §5.4), release notes/changelog.

---

## 1. Deliverables

| Path | What |
| --- | --- |
| `internal/versions/versions.go` | New. `CosignVersion` + per-OS/arch cosign sha256 (used by `install.sh`, checked by lint) |
| `internal/release/` | New. Manifest types, channel/identity policy, fetch, sigstore-go verification, release cache |
| `internal/upgrade/` | New. Local self-update: plan, download, verify, atomic swap |
| `internal/localenv/escalate.go` | New. Minimal §20.3 sudo escalation (just what `upgrade` needs; M3 extends it) |
| `internal/cli/upgrade.go` | `eru upgrade` implementation, replacing `notImplemented` |
| `internal/cli/flags.go` | Hidden global `--escalated` flag |
| `internal/cli/platform.go` | Platform gate: node mode is Linux-only (§6.5) |
| `scripts/build-release.sh` | Build 4 targets, sha256 files, channel + version manifests, channel-substituted `install.sh` |
| `scripts/sign-release.sh` | cosign keyless `sign-blob --bundle` for manifests and artifacts, then `verify-blob` |
| `scripts/upload-release.sh` | Upload immutable `releases/<v>/…` first, channel files last |
| `scripts/install.sh` | Installer template (Linux + macOS) |
| `.github/workflows/publish-edge.yml` | On push to `master` |
| `.github/workflows/publish-stable.yml` | On tags `vX.Y.Z` (wired now; first tag is not an M1 exit criterion) |
| `justfile` | `release-local VERSION CHANNEL BASE_URL`, lint additions (§7) |
| `e2e/release_test.go` + harness helpers | E25 (§8) |
| `docs/DEVIATIONS.md` | Entries listed in §10 |

New Go dependency: `github.com/sigstore/sigstore-go` (latest stable). It's already allowed by §2.1.

---

## 2. Release layout

### 2.1 Bucket and URLs

One R2 bucket behind the custom domain `ReleaseBaseURL` (`https://eru.mbvlabs.com`, still a placeholder: the maintainer doesn't own an `eru` domain; M1 changes `buildinfo.ReleaseBaseURL` from `eru.mbvlabs.com` to it).

```
https://eru.mbvlabs.com/
├── install.sh                              stable channel installer          (no-cache)
├── manifest.json                           stable channel pointer            (no-cache)
├── manifest.json.sigstore.json
├── edge/
│   ├── install.sh                          edge channel installer            (no-cache)
│   ├── manifest.json                       edge channel pointer              (no-cache)
│   └── manifest.json.sigstore.json
└── releases/<version>/                     immutable, cache 1y
    ├── manifest.json                       version manifest (same bytes as the channel copy at release time)
    ├── manifest.json.sigstore.json
    └── <os>/<arch>/
        ├── eru
        ├── eru.sha256                      "<hex>  eru"
        └── eru.sigstore.json
```

`<os>` ∈ `linux|darwin`, `<arch>` ∈ `amd64|arm64`.

**Decided:** stable sits at the root and edge under `/edge/`, with one bucket and one domain.

The **channel root** is `ReleaseBaseURL` for stable and `ReleaseBaseURL + "/edge"` for edge. `internal/release.ChannelRoot(channel)` is the only place that knows this.

### 2.2 Manifest

Schema from §19.1, unchanged:

```json
{
  "schemaVersion": 1,
  "channel": "edge",
  "version": "edge-9d969fb8cbe0",
  "releaseBaseUrl": "https://eru.mbvlabs.com/releases/edge-9d969fb8cbe0",
  "artifacts": {
    "linux/amd64":  {"url": "https://eru.mbvlabs.com/releases/edge-9d969fb8cbe0/linux/amd64/eru",  "sha256": "…"},
    "linux/arm64":  {"url": "…", "sha256": "…"},
    "darwin/amd64": {"url": "…", "sha256": "…"},
    "darwin/arm64": {"url": "…", "sha256": "…"}
  }
}
```

The **version manifest** (`releases/<v>/manifest.json`) is new relative to §19.1. It makes `eru upgrade --version <v>` possible without trusting an unsigned directory listing. Both copies are signed separately. They have the same content, and the signing identity is the same.

### 2.3 Versions

| Channel | Format | Ordering |
| --- | --- | --- |
| `stable` | `MAJOR.MINOR.PATCH` (tag `vX.Y.Z`) | semver |
| `edge` | `edge-<12-hex commit>` | none; "different" means "newer" |
| `dev` | `dev` (local `just build`) | never upgraded *to*; can upgrade *from* only with an explicit `--channel` |

The regexes MUST match deploycrate's `stableVersionPattern` / `edgeVersionPattern`.

### 2.4 Signing identity (MUST)

Keyless, GitHub Actions OIDC. Issuer `https://token.actions.githubusercontent.com`. The certificate SAN must equal, exactly:

| Channel | Identity |
| --- | --- |
| stable | `https://github.com/mbvlabs/eru/.github/workflows/publish-stable.yml@refs/tags/v<version>` |
| edge | `https://github.com/mbvlabs/eru/.github/workflows/publish-edge.yml@refs/heads/master` |

`install.sh` uses the equivalent anchored regexps (it doesn't know the version before it reads the manifest). The Go verifier uses the exact string for the version it's about to install. This pins a stable version to the tag it claims to be.

Note: §19.1 says edge publishes from `main`. The default branch is `master`. Recorded as a deviation (§10).

---

## 3. Release scripts

All scripts: `set -euo pipefail`, pass `shellcheck -x`, inputs via args/env validated with `${1:?}` / `: "${VAR:?}"`. They're adapted from `/home/mbv/work/deploycrate-ce/scripts/` with the frontend build removed.

### 3.1 `scripts/build-release.sh VERSION CHANNEL BASE_URL`

1. Validate the channel (`stable|edge`), the version against §2.3, and that `BASE_URL` is `https://` with no whitespace, quotes or backslashes (copy deploycrate's checks).
2. Compute `channel_root` (§2.1) and `out=dist/release-<channel>`. `rm -rf` it, then lay out `releases/<v>/<os>/<arch>/`.
3. For each of the 4 targets: `CGO_ENABLED=0 GOOS GOARCH go build -trimpath -ldflags "-s -w -X …Version=<v> -X …Commit=<git rev-parse --short=12 HEAD> -X …Channel=<channel>" -o …/eru ./cmd/eru`. Then write `eru.sha256` with a portable sha256 helper (`sha256sum`, falling back to `shasum -a 256`, so the script also runs on macOS).
4. Write `releases/<v>/manifest.json` with the exact §2.2 shape, using `jq -n` (not a heredoc), so values are escaped. Copy it byte-for-byte to `<out>/<channel-relative>/manifest.json`.
5. Render `install.sh` from `scripts/install.sh` by substituting the two placeholders `@@CHANNEL_ROOT@@` and `@@CHANNEL@@`. Then `grep` that no `@@` remains. Write it to the channel location, mode 0755.
6. Print the output tree.

`BASE_URL` is a build argument, not `buildinfo.ReleaseBaseURL`, so a release can be built for a test server (E25). Release workflows pass `vars.RELEASE_BASE_URL`, and the workflow fails if it differs from the constant in `internal/buildinfo` (a grep check), so a shipped binary never points somewhere other than where it was published.

### 3.2 `scripts/sign-release.sh RELEASE_ROOT`

As in deploycrate. Sign with `cosign sign-blob --yes --bundle <f>.sigstore.json <f>` and immediately `verify-blob` with the exact identity from `GITHUB_WORKFLOW_REF`. Files signed: both manifests and the 4 `eru` binaries. `install.sh` isn't signed; it's the trust anchor fetched over TLS, which is the same as deploycrate.

### 3.3 `scripts/upload-release.sh RELEASE_ROOT CHANNEL BUCKET ENDPOINT`

Order matters, so a reader never sees a channel manifest whose artifacts don't exist yet:

1. `releases/` recursive, `cache-control: public,max-age=31536000,immutable`.
2. Channel `install.sh` (`no-cache`, `text/x-shellscript`).
3. Channel `manifest.json.sigstore.json`, then channel `manifest.json` last (`no-cache`, `application/json`).

Stable additionally refuses to overwrite an existing `releases/<v>/`. That check lives in the workflow, as in deploycrate.

---

## 4. Verification in Go (`internal/release`)

### 4.1 API

```go
type Artifact struct{ URL, SHA256 string }
type Manifest struct {
    SchemaVersion  int                 `json:"schemaVersion"`
    Channel        string              `json:"channel"`
    Version        string              `json:"version"`
    ReleaseBaseURL string              `json:"releaseBaseUrl"`
    Artifacts      map[string]Artifact `json:"artifacts"`
}

func ChannelRoot(channel string) (string, error)
func Identity(channel, version string) (string, error)

// Resolve fetches and verifies the channel manifest (version == "") or the version manifest.
func (c *Client) Resolve(ctx context.Context, channel, version string) (Manifest, error)

// Fetch returns a verified local path for os/arch, using the release cache.
func (c *Client) Fetch(ctx context.Context, m Manifest, goos, goarch string) (path string, err error)
```

M2's binary push (§7.5) and M10's node upgrade reuse `Resolve` and `Fetch` for `linux/<arch>`. Keep them free of CLI concerns.

### 4.2 Manifest verification order (MUST)

1. GET the manifest and the `.sigstore.json` bundle. Each is capped at 1 MiB, HTTPS only, with no redirects off-host.
2. Verify the bundle over the **raw manifest bytes** with sigstore-go:
   - trusted material: Sigstore public-good root via TUF (`tuf.DefaultOptions()`, cache under `$XDG_CACHE_HOME/eru/sigstore-tuf`), using sigstore-go's embedded initial root;
   - policy: `verify.WithArtifact(bytes)`, certificate identity = `Identity(channel, version)` exact SAN plus issuer exact;
   - require ≥1 transparency-log entry and ≥1 observer timestamp (`WithTransparencyLog(1)`, `WithObserverTimestamps(1)`).
3. Only now `json.Unmarshal` (strict: `DisallowUnknownFields`) and validate: `schemaVersion == 1`; `channel` == requested; `version` matches §2.3 (and equals the requested version for a version manifest); `releaseBaseUrl == <ReleaseBaseURL>/releases/<version>`; every artifact URL equals `<releaseBaseUrl>/<os>/<arch>/eru`; every sha256 is 64 lowercase hex; all 4 targets present.

For a channel manifest, step 2 needs the version before parsing. Parse the untrusted `version` field first, use it **only** to build the expected identity, verify, and then run full validation. A lying version fails the identity check.

### 4.3 Artifact fetch

- Cache: `$XDG_CONFIG_HOME/eru/cache/releases/<version>/<os>-<arch>/eru` (§6.3), plus the verified `manifest.json` and bundle next to it. Directories are 0700 and files 0600 (the binary 0700).
- A cache hit is re-hashed against the manifest sha256 before use. A mismatch deletes it and re-downloads.
- Download streams to a temp file in the cache dir with a 256 MiB cap, hashes while writing, compares in constant time, then renames into place.
- The artifact's own sigstore bundle isn't verified in Go. The signed manifest pins its sha256, so it's covered. The bundle exists for `install.sh` and humans with cosign.

### 4.4 Errors

| Failure | Code |
| --- | --- |
| DNS/TCP/TLS/HTTP non-200 fetching a manifest, bundle or artifact | `remote_unreachable` (exit 9), `details.url` |
| Bad signature, wrong identity, missing tlog, malformed or inconsistent manifest, sha256 mismatch | **new** `release_verification_failed` (exit 14) |
| Unknown version (404 on a version manifest) | `validation_failed` (exit 7), `details.version` |

**Decided:** a new code `release_verification_failed` with a new **exit 14** (its own row in §5.3 of the Phase 1 spec and in the agent guide's exit table). Agents must not retry it blindly, and humans should treat it as a possible attack.

---

## 5. `install.sh`

Template at `scripts/install.sh`, rendered by `build-release.sh`. Adapted from deploycrate's `install-channel.sh` with these changes.

### 5.1 Platform

- `uname -s`: `Linux → linux`, `Darwin → darwin`, anything else fails. `uname -m`: `x86_64|amd64 → amd64`, `aarch64|arm64 → arm64`.
- sha256 helper: `sha256sum` or `shasum -a 256`. Download with `curl -fsSL --proto '=https' --tlsv1.2`.
- Works under `bash` 3.2 (the macOS system bash): no associative arrays, no `${var,,}`.

### 5.2 Verification

1. Download pinned cosign `v<CosignVersion>` for `<os>-<arch>` into a `mktemp -d` work dir (trap-removed) and check its pinned sha256. It's **not** installed persistently; eru verifies in-process from then on.
2. Fetch the channel manifest + bundle. `cosign verify-blob` with the anchored identity regexp for `@@CHANNEL@@` and the issuer.
3. Extract `channel`, `version`, `releaseBaseUrl` and the artifact URL/sha for this platform with `sed`, as deploycrate does (no `jq` dependency on target machines). Check `channel == @@CHANNEL@@` and that `releaseBaseUrl` starts with `<origin>/releases/`, where `<origin>` is `@@CHANNEL_ROOT@@` minus any `/edge` suffix.
4. Download the artifact, `cosign verify-blob` it against its bundle, and check it against the **manifest's** sha256, not only the `.sha256` file.
5. Smoke-run `<tmp>/eru version --output json` and check it reports the manifest version. This catches wrong-arch or truncated binaries before install.

### 5.3 Install location

| Who | Target | Notes |
| --- | --- | --- |
| root | `/usr/local/bin/eru` | `install -m 0755` via temp + `mv` in the same dir (atomic) |
| non-root | `${ERU_INSTALL_DIR:-$HOME/.local/bin}/eru` | `mkdir -p`. If the dir isn't on `PATH`, print the exact `export PATH=…` line for the detected shell |

Never call `sudo` from the script. The documented root form is `curl -fsSL https://eru.mbvlabs.com/install.sh | sudo bash`.

On macOS, replacing an existing binary MUST go through a new inode (temp + `mv`). Overwriting in place with `cp` invalidates the kernel's code-signature cache, and the next exec is SIGKILLed. Go ad-hoc signs darwin/arm64 binaries itself, and files fetched with curl get no quarantine xattr, so no extra signing step is needed.

### 5.4 Post-install

Print `eru <version> installed to <path>` and the next step (`eru --help`, or on a server, `eru cluster init --local` once M3 lands).

`ERU_INSTALL_ONLY=1` is accepted now and suppresses the next-step text. The §23.2 "Initialise a cluster on this server now?" prompt is **deferred to M3**, because `cluster init --local` only returns `not implemented` until then. E05 covers it in M3. Recorded as a deviation.

### 5.5 Pin consistency

`just lint` greps `scripts/install.sh` for the cosign version and the four sha256 values and compares them with `internal/versions`, printed by a tiny `go run ./internal/versions/cmd/print`. One source of truth (`internal/versions`); the script must agree.

Cosign: use the latest stable 3.x at implementation time (≥ `3.1.3`, matching `mise.toml`), not deploycrate's 3.0.6, and record the checksums from the release's `cosign_checksums.txt`.

---

## 6. `eru upgrade` (local self-update)

### 6.1 Flags and metadata

Existing flags stay: `--version`, `--channel`, `--skip-nodes`, `--dry-run`. Add `--yes` (skip the TTY confirmation; it isn't destructive, so `--yes` is enough). Meta stays as in M0, plus `ErrorCodes: [remote_unreachable, release_verification_failed, validation_failed, precondition_failed, input_required]`. Keep `Remote: true`, because M10 adds the node phase.

### 6.2 Algorithm

1. **Resolve self.** `os.Executable()` → `filepath.EvalSymlinks` → `target`. If `target` sits under a package-manager prefix we don't own (`/opt/homebrew/Cellar`, `/usr/local/Cellar`, `/nix/store`), return `precondition_failed` with `details.reason = "managed_by_package_manager"`.
2. **Channel.** `--channel`, else `buildinfo.Channel`. If that's `dev` and there's no `--channel`, return `input_required` naming `--channel` (a dev build has no channel to follow).
3. **Resolve the release.** `Resolve(channel, --version)`.
4. **Decide.**
   - same version → `ok: true`, `data.changed = false`, message "already on <v>". Exit 0.
   - stable, and the target < current without `--version` → no-op (never auto-downgrade). With an explicit `--version` older → allowed, plus `warnings: [{code: "downgrade"}]`.
   - channel switch (edge ↔ stable) → allowed, with a `channel_changed` warning.
5. **Dry run** → return the plan (§6.3) with `data.dry_run = true`. Don't download.
6. **Confirm** (TTY, human mode, no `--yes`): "Upgrade eru <from> → <to> (<channel>) at <target>? [y/N]". Structured or no-input mode proceeds without prompting, because `upgrade` isn't destructive.
7. **Privilege.** If `dir(target)` isn't writable by the process, use §6.4.
8. **Fetch** `<goos>/<goarch>` into the cache (§4.3).
9. **Pre-flight the new binary.** Run `<cached> version --output json` with a 10 s timeout and require `version == manifest.version` and `channel == manifest.channel`.
10. **Swap atomically.** Copy the cached file to `<dir>/.eru.new-<rand>` (0755, same owner as the old binary), `fsync` it and the dir, then `rename` over `target`. The previous binary is kept as `<target>.prev` via a hard link created before the rename. If the hard link fails (cross-device, permissions), continue without `.prev` and add a `no_previous_kept` warning.
11. **Nodes.** Unless `--skip-nodes`, add `warnings: [{code: "nodes_not_upgraded", message: "node upgrades arrive with eru node upgrade (M10)"}]`. M10 replaces this with a re-exec of the **new** binary: `<target> node upgrade --all …`.
12. Return the envelope.

Steps 9–10 make a half-written or wrong-arch binary impossible at `target`, and `rename(2)` is atomic on both Linux and macOS.

### 6.3 Output

```json
{
  "schema": "eru/v1", "ok": true, "command": "upgrade",
  "data": {
    "from": {"version": "edge-aaaaaaaaaaaa", "channel": "edge"},
    "to":   {"version": "edge-bbbbbbbbbbbb", "channel": "edge"},
    "path": "/usr/local/bin/eru",
    "previous_path": "/usr/local/bin/eru.prev",
    "changed": true,
    "dry_run": false,
    "escalated": true,
    "nodes": "skipped"
  },
  "warnings": [{"code": "nodes_not_upgraded", "message": "…"}],
  "next": ["eru version"]
}
```

Human mode: one line per action, then `eru upgraded <from> → <to>`.

### 6.4 Minimal sudo escalation (`internal/localenv/escalate.go`)

This implements §20.3 exactly, restricted to what M1 needs. M3 adds install offers and the other callers.

```go
// Escalate re-executes the current command under sudo, if needed and allowed.
// Returns handled=true when the child ran (its exit code is propagated).
func Escalate(cmd *cobra.Command, reason string) (handled bool, err error)
```

- Already root, or the hidden `--escalated` flag is set → return `false`, and the caller proceeds.
- `sudo -n true` succeeds → re-exec `sudo -n <self> <os.Args[1:]> --escalated`.
- Otherwise, TTY + human mode → prompt "eru needs root to <reason>. Escalate with sudo? [y/N]" → `sudo <self> … --escalated` with inherited stdio.
- No TTY / structured mode → `--sudo` given: only `sudo -n` was possible and it failed → `precondition_failed` (`details.reason = "sudo_password_required"`). Without `--sudo` → `precondition_failed` with `details.reason = "root_required"` and `details.command` (the exact `sudo eru …` line).
- In structured mode, the child's stdout is the envelope. The parent passes it through untouched and exits with the child's code. No second envelope is printed.

The escalated child re-resolves the release (step 3) itself. Root's cache is root's `$XDG_CONFIG_HOME`. Pass `--channel`/`--version` explicitly so parent and child agree on what they're installing: the parent appends the resolved `--version <v> --channel <c>` when re-executing.

### 6.5 Platform gate: node mode is Linux-only (MUST)

Operator mode runs on Linux and macOS. Node mode runs **only on Linux**. M1 adds the gate because M1 is where darwin binaries first ship.

- **Runtime gate.** The root `PersistentPreRunE` looks up the command's `CommandMeta`. If `Audience == "node"` and `runtime.GOOS != "linux"`, it returns `precondition_failed` with `details.reason = "node_mode_requires_linux"`, `details.os = runtime.GOOS` and the hint "node commands run on the Linux server; use the operator commands from this machine". This covers every `eru node …` command except `eru node upgrade`, which is operator-audience.
- **Other node-side entry points** get the same gate via a `linuxOnly` meta field, set on `eru cluster init --local` and `eru wg export --install` (system WireGuard is Linux-only in Phase 1), so the check isn't scattered through handlers. `--check-meta` requires `linuxOnly` to be set on every `Audience: "node"` command, so the rule can't be forgotten.
- **Discovery** stays the same on every OS: `eru commands --json` lists node commands on macOS too, with `"platforms": ["linux"]` (new meta JSON field, schema-compatible addition), so an agent knows the restriction without calling the command.
- **From M2 on** (recorded here so M2 follows it): `internal/node` (dispatcher and handlers) and `internal/steps` are compiled only under `//go:build linux`. A `_other.go` stub returns the same `precondition_failed`. Darwin binaries then can't contain node code at all; the runtime gate stays as the user-facing error.
- `install.sh` never offers `cluster init --local` on macOS (relevant once the M3 prompt lands).
- `eru upgrade` is an operator command and works on both platforms. On a Linux server it updates the same `/usr/local/bin/eru` that node mode uses. That's intended until M10 coordinates node upgrades.

---

## 7. CI and justfile

### 7.1 `publish-edge.yml`

On `push` to `master`. `permissions: {contents: read, id-token: write}`, `concurrency: publish-edge` (no cancel), `environment: edge`. Actions pinned by SHA, as in deploycrate.

Steps: checkout → `jdx/mise-action` (go, cosign from `mise.toml`) → validate config (`vars.RELEASE_BUCKET`, `vars.RELEASE_BASE_URL`, `secrets.R2_ENDPOINT`, `secrets.R2_ACCESS_KEY_ID`, `secrets.R2_SECRET_ACCESS_KEY` non-empty, and `RELEASE_BASE_URL` equal to `buildinfo.ReleaseBaseURL`) → `just lint` → `scripts/build-release.sh "edge-${GITHUB_SHA::12}" edge "$RELEASE_BASE_URL"` → `sign-release.sh` → `upload-release.sh`.

### 7.2 `publish-stable.yml`

On tags `v*`, as in deploycrate: `fetch-depth: 0`, the tag must be an ancestor of `origin/master`, `gofmt`/`go vet`/`just lint` clean, build/sign, refuse to overwrite `releases/<v>/`, then upload. There's no `go test` step (no unit tests, §0.1).

### 7.3 justfile

```
release-local VERSION CHANNEL BASE_URL   # scripts/build-release.sh, unsigned; for E25 and manual checks
```

`lint` additions: `shellcheck -x scripts/*.sh` (already globbed) and the pin-consistency check (§5.5).

### 7.4 Setup the maintainer does once (not code)

- R2 bucket + custom domain for `ReleaseBaseURL`. `install.sh` is served with `text/x-shellscript`.
- GitHub environments `edge` and `stable` with the vars and secrets from §7.1.
- Confirm the real domain/bucket name. If it isn't `eru.mbvlabs.com`, change `buildinfo.ReleaseBaseURL` and `DEVIATIONS.md` together.

---

## 8. Automated e2e: E25 (`e2e/release_test.go`)

M1's spec exit is manual, but the swap/verify logic is exactly what silently breaks later. E25 runs it in CI without GitHub OIDC.

**Decided:** test-only trust under the existing `e2etest` build tag. `internal/release` gets an `e2etest`-only file that trusts a cosign **key pair** generated per run (`cosign generate-key-pair`, `sign-blob --key --bundle`). Production builds can't contain it, and it runs both locally and in CI.

- **Harness:** `harness.ReleaseServer(t)` starts a Caddy (or `python3 -m http.server` behind Pebble-issued TLS) container serving `dist/release-<ch>` on the CI host. The VM trusts Pebble's root (already wired for the `tls` step). `ERU_TEST_RELEASE_BASE_URL` overrides `ReleaseBaseURL` **only** in `e2etest` builds.
- **Builds:** `just release-local edge-000000000001 edge <url>` and `…-000000000002`, signed with the per-run key.
- **Steps (in a Debian 13 VM):**
  1. `curl <url>/edge/install.sh | bash` as root, with an `install.sh` rendered to use `cosign verify-blob --key` (e2etest variant). → `/usr/local/bin/eru version` = `…01`.
  2. As a non-root user: same → `~/.local/bin/eru`, plus the PATH hint printed.
  3. Publish `…02` as the channel manifest. `eru upgrade --json` (root) → `changed: true`, `to.version = …02`, `eru.prev` exists and reports `…01`.
  4. `eru upgrade --json` again → `changed: false`, exit 0.
  5. Non-root `eru upgrade --json` on the root-owned binary without `--sudo` → `precondition_failed` (exit 11), `details.reason = "root_required"`. With `--sudo` and passwordless sudo configured → succeeds, `escalated: true`.
  6. Tamper: flip a byte in the `linux/amd64` artifact → `release_verification_failed`, and the installed binary is unchanged (sha256 before/after). Tamper the manifest → same.
  7. `--version edge-000000000099` (absent) → `validation_failed` (exit 7).
  8. `--dry-run` → plan returned, binary unchanged, nothing written to the cache.
  9. `eru commands --json` (linux build): every `audience: "node"` entry carries `"platforms": ["linux"]`. The darwin runtime refusal is covered manually by M1-B, since CI has no macOS VM.

---

## 9. Manual scenarios (§24 exit)

Run against the real edge channel after the first two `master` pushes that include M1. Record date, versions and machine in the PR.

| ID | Steps | Pass when |
| --- | --- | --- |
| M1-A | Fresh Debian 13 VPS: `curl -fsSL https://eru.mbvlabs.com/edge/install.sh \| sudo bash`; then as a user, without sudo | `/usr/local/bin/eru` and `~/.local/bin/eru` both report the edge version |
| M1-B | macOS (arm64; amd64 if available): `curl … \| bash` | `~/.local/bin/eru version` works; no Gatekeeper kill; `eru node version` → `precondition_failed` (`node_mode_requires_linux`, exit 11) |
| M1-C | After a newer edge push: `eru upgrade` on both machines; then `eru upgrade` again | Upgraded; second run is a no-op |

---

## 10. `docs/DEVIATIONS.md` entries

- Channel URL layout (§2.1), and the per-version manifest.
- Edge publishes from `master`, not `main`.
- `release_verification_failed` → exit 14, added to `internal/cli/output/codes.go`, the agent guide table and `e2e/schema/eru-v1.json`.
- `install.sh` cluster-init prompt deferred to M3.
- Cosign version bumped from the spec floor `3.0.6` to the version actually pinned.
- `eru upgrade` keeps `<target>.prev` locally (the spec only requires `.prev` on nodes).
- Package-manager-owned binaries are refused (`managed_by_package_manager`).
- Node mode is Linux-only: the runtime gate plus the `platforms` field in `eru commands --json` (§6.5). Release domain changed to `eru.mbvlabs.com` (replaces the M0 `get.eru.sh` note).

---

## 11. Implementation order

1. `internal/versions`, `internal/release` (manifest, identity, verify, cache).
2. `scripts/build-release.sh` + `just release-local`. Verify the output tree by hand.
3. `scripts/install.sh` template + render. Test against a local `release-local` dir on Linux and macOS.
4. `internal/localenv/escalate.go`, `internal/upgrade`, `eru upgrade`.
5. `sign-release.sh`, `upload-release.sh`, workflows.
6. E25 + harness release server.
7. First real edge push → M1-A/B/C.
8. `DEVIATIONS.md`, agent guide error table, `just lint` green.
