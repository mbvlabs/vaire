# Plan: cutting the v1 release

Status, 2026-10-19: main live qualification for Phases 0–5 is complete, with explicit coverage limits in [MANUAL-TESTING.md](MANUAL-TESTING.md). No alpha has been tagged or published by this qualification. This is the release procedure, not authorization to execute it.

## Decided (2026-10-16)

- **Branch model (Laravel-style):**
  - a `1.x` branch is the v1 line, and its tags are the stable releases;
  - `master` stays the edge channel (Phases 6 and 7 and beyond, `edge-<sha>` on every push);
  - fixes are cherry-picked from master to `1.x` and the other way round (`git cherry-pick -x`).
- **Version names:** v1 is an alpha, so `1.x` is tagged `v1.0.0-alpha.1`, `v1.0.0-alpha.2`, …, then `v1.0.0` when the formats freeze.
- **Release gate:** review the documented evidence limits and draft notes, commit the documentation with authorization, then separately authorize the v1 branch/tag and publication.
- **Secrets export gets a gate:** `eru secrets export` needs the master password or a TOTP code. If the secrets document is lost, it's lost; there's no bypass.
- **Phases 6 and 7** merge into master after `1.x` is cut, one at a time, and ship in a later line.

## What exists today, and what the model needs

- Edge is published on every push to `master`. Stable tooling accepts `vMAJOR.MINOR.PATCH[-alpha.N|-beta.N|-rc.N]` on its major line, stores immutable artifacts under `/releases/<version>/`, and promotes the major-line/root channels only forward under the prerelease policy. The first alpha publication is still pending.
- **What blocked the model, now fixed:**
  - `publish-stable.yml` checks that a `v<major>.*` tag's commit is on `origin/<major>.x`, so tags on `1.x` publish;
  - pre-release versions (`-alpha.N`, `-beta.N`, `-rc.N`) parse and order by semver precedence everywhere (`build-release.sh`, `release.ParseStable`, `Identity`, `install.sh`'s identity regexp, `upgrade.cmpSemver`);
  - a per-major channel manifest (`/1.x/manifest.json`) is published beside the version, and a binary from a 1.x tag follows its line by default;
  - the root manifest moves only for the highest stable version.
- **Version lockstep:** a node runs exactly the CLI's build, so each install moves its CLI and nodes along one line.

## Qualification outcome

- **Implemented and verified:** the original v1 fixes and all six review findings, with Go regression tests and full local verification. The [archived review](archive/REVIEW-v1-fixes.md) retains the findings and resolutions.
- **Main live scenarios completed:** genuine existing-protected-policy upgrade, SQL/results/rollback, protected maintenance and interactive writes, secrets factor/refusal matrix, query/top filtering, CLI UX, idle tunnel and peer timeout, fresh bucket-backed init and database-free doctor, first database/full/verify, protected dump copy/promotion, genuine awaiting-recovery refusal, rebuild/recovery, local init and automatic lockdown revert with journal evidence.
- **Live-discovered fixes published and retested:** `eaa1a73` (administrative query/top filtering) and `65c4f91` (backup service templates available before first recovery full). Latest code qualification: `edge-65c4f9157253`, edge workflow `39234654792` succeeded.
- **Not claimed:** every historical fault-injection/checklist variant. Exact limitations, including imported-password authorization, forced handshake branches, handoff completion, telemetry freshness and final resumed DNS/TLS output, remain in the testing log and [draft release notes](RELEASE-v1.0.0-alpha.1.md).
- **Excluded/deferred:** macOS live tests were excluded by the operator; cross-build passed. Actual stable publication is pending authorization. Major-upgrade design and Phases 6–7 are not part of this preparation.

No unresolved reproducible code blocker remains from the main qualification run. Publication readiness still requires reviewing the stated limits and completing the cut checklist, not silently treating missing evidence as passed.

## After the cut

- Merge Phase 6 into master (rebase, conflict resolution, its manual test), then Phase 7. Expect conflicts in node/methods.go, db.go, restore.go, recover.go, steps/postgres_roles.go, restarter.go and doctor.
- Phase 6's `wal_level = replica` means a restart is pending on every database when upgrading to it. `eru apply` asks with `--confirm <db>/restart`, and the release notes have to say so.

## Cut checklist

1. Review the testing summary, coverage limits and draft release notes. Identify the intended live test context before any further deployment: the VPS was rebuilt repeatedly, so old contexts are not interchangeable.
2. Finalize the draft notes and documentation cleanup; with authorization, run the complete `AGENTS.md` verification sequence and commit. Push master only when authorized; require its edge workflow to succeed and record the candidate commit. The current notes describe qualification through `65c4f91`, not an automatically selected final release commit.
3. Check remote branch/tag state and stable-environment configuration/protections; confirm the first intended version is unused. With explicit authorization: `git branch 1.x master && git push origin 1.x` (only if the branch does not already exist).
4. Confirm `RELEASE-v1.0.0-alpha.1.md` on `1.x` matches the approved scope and preserves the untested/alpha-compatibility disclosures. If source or notes need changes, verify and commit them with authorization before tagging; record the exact final tagged commit externally rather than embedding a self-referential commit hash in its own source.
5. With explicit authorization: `git tag -s v1.0.0-alpha.1 1.x && git push origin v1.0.0-alpha.1`. Watch `publish-stable.yml`; record its result. No synthetic tags or release-pointer manipulation for testing.
6. Verify the immutable artifacts/signatures and both root and `/1.x/` manifests identify the approved alpha. Install from the published line on a clean Linux machine. Explicitly select `--channel stable-1.x` when moving an edge CLI to the alpha; require a real version transition, not a no-op. Upgrade the agreed node normally with `apply --auto-upgrade-nodes`, then doctor and plan. Do not label stable publish/install/upgrade passed before this evidence exists.
7. Update MASTER.md, README, release notes and the testing log with the actual version/commit/workflow and release-verification results. Switch the README quick-start installer from edge to the published v1 line only after publication succeeds.

## Releasing

### Cutting `1.x`

Only when the user asks: `git branch 1.x master && git push origin 1.x`. From then on `master` is the edge channel (every push publishes `edge-<sha>`), and `1.x` carries the v1 line. A later major line is cut the same way (`2.x` from `master` when 2.0 is ready).

### Tagging an alpha

1. `git tag -s v1.0.0-alpha.N 1.x && git push origin v1.0.0-alpha.N` (never without the user).
2. `publish-stable.yml` runs: it checks the tag's commit is on `origin/<major>.x` (a `v1.*` tag on `origin/1.x`), then `gofmt`, `go vet` and `just lint`, then builds with `scripts/build-release.sh <version> stable-<major>.x <base-url>`. The manifest's channel is `stable`; the binary's `Channel` is `stable` and its `Version` is the tag, so `eru upgrade` with no `--channel` follows `stable-<major>.x`.
3. It signs and uploads immutable `/releases/<version>/` artifacts, refusing an existing version prefix. The line channel `/1.x/manifest.json`, `/1.x/manifest.json.sigstore.json` and `/1.x/install.sh` moves only when the candidate passes the same monotonic/prerelease promotion policy as root. Read/list/parse errors abort rather than masquerading as an empty channel.
4. It moves the **root** channel (`/manifest.json` and `/install.sh`) only when the new version should be the highest: `internal/release/cmd/promote CURRENT CANDIDATE` prints yes when the candidate is newer by semver precedence, and a pre-release moves the root only while the root has no final release (so `1.0.0-alpha.2` moves the root off `1.0.0-alpha.1`, but `1.0.0` never moves it back down below a `2.x` final).
5. Watch the run (`gh run watch <id> --repo mbvlabs/eru --exit-status`), then check both manifests show the version and install from `stable-1.x` on a clean machine.

### Cherry-picking

- A fix lands on `master` first, then goes to the line: `git checkout 1.x && git cherry-pick -x <sha> && git push origin 1.x`.
- A fix that only concerns a line (for example a 1.x-only compatibility shim) is cherry-picked back to `master` the same way (`-x`), so both trees keep the commit's provenance.
- A commit that is 2.x-only is not back-ported; the `1.x` line only receives fixes the release notes can name.

### Which channel an install follows

- **Root install** (`https://…/install.sh`): tracks the root manifest, the highest stable line. After `2.0.0`, that is `2.x`.
- **Line install** (`https://…/1.x/install.sh`): pins `1.x`; it keeps installing 1.x patches even after `2.0.0`.
- **`eru upgrade`**: with no `--channel`, a 1.x binary follows `stable-1.x` (its own line) and a 2.x binary `stable-2.x`; a dev build must pass `--channel`. `eru upgrade --channel stable` moves to the highest stable line explicitly; `--channel stable-1.x` pins the line; `--channel edge` follows `master`.
- **Edge install**: `https://…/edge/install.sh`, the edge manifest on every push to `master`.

### GitHub-side settings

- The `stable` and `edge` environments (already used) hold `R2_*` secrets; `RELEASE_BUCKET` and `RELEASE_BASE_URL` are repository variables.
- Branch protection on `1.x` (and `master`): no force-push, no deletion, changes through pull requests; a stable tag is pushed by a maintainer only.
- Tag protection for `v*`: only maintainers may create or delete a release tag, so `publish-stable.yml` cannot be pointed at an arbitrary commit.
- The `stable` environment may require a reviewer if more than one person can push tags. The release workflow's OIDC identity is what `install.sh` and `eru upgrade` pin, so the environment name and workflow path must not change.

## Open questions

- Is anyone besides you going to install the alpha? If so, where do they report issues (GitHub issues on `mbvlabs/eru`, or private)?
- Edge continues publishing on every push to master after the cut, as already decided above.
