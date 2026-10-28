# Plan: cutting the v1 release

Status, 2026-10-19: main live qualification for Phases 0–5 is complete, with explicit coverage limits in [MANUAL-TESTING.md](MANUAL-TESTING.md). No alpha has been tagged or published by this qualification. This is the release procedure, not authorization to execute it.

## Release strategy (updated 2026-10-19)

- **Branch model (Laravel-style, merge up):**
  - `master` stays the development branch and edge channel (`edge-<sha>` on every push);
  - `1.x` is the whole v1 release line: alphas, final, compatible minor features and patch fixes. Later major lines use `2.x`, `3.x`, etc.;
  - every change reaches `1.x` and `master` through a pull request. A change targets the oldest branch it belongs on: a v1 fix targets `1.x`, work for an unreleased line targets `master`;
  - `1.x` is then merged up into `master` with a pull request, so every v1 fix reaches master without being copied. Nothing is cherry-picked in either direction;
  - breaking changes stay out of `1.x` once compatibility freezes at `v1.0.0` final. They ship on a later major line cut from master. During alpha, the documented compatibility caveats still apply.
- **Version names:** v1 is an alpha, so `1.x` is tagged `v1.0.0-alpha.1`, `v1.0.0-alpha.2`, …, then `v1.0.0-beta.1`, … once Phases 6 and 7 and activity history are on the line, then `v1.0.0` when the formats freeze.
- **Release gate:** review the documented evidence limits and draft notes, commit the documentation with authorization, then separately authorize the v1 branch/tag and publication.
- **Secrets export gets a gate:** `vaire secrets export` needs the master password or a TOTP code. If the secrets document is lost, it's lost; there's no bypass.
- **v1.0.0 scope (decided 2026-10-28):** `v1.0.0` is Phases 0–7 plus local activity history. The published alphas (`v1.0.0-alpha.1`, `v1.0.0-alpha.2`) are Phases 0–5. Activity history, then Phase 6 and Phase 7, merge into master one at a time, each live-tested on edge. Master is then merged into `1.x` with a merge-commit pull request and tagged `v1.0.0-beta.1`. Betas replace the alphas from then on; `v1.0.0` final follows qualification of the betas. This supersedes the earlier plan to ship followers and extensions after final as `v1.1.0`.

### Version numbers and branches

- Roadmap phase numbers do not determine release numbers; minor releases are sequential.
- After `v1.0.0`, the next compatible feature release is `v1.1.0`, then `v1.2.0`, and so on.
- A backward-compatible feature release increments the minor and resets the patch; a fixes-only release increments the patch (for example `v1.1.1`). Breaking changes require a new major after final. Alpha releases remain `v1.0.0-alpha.N` until the first final.
- `1.x` is a moving branch; `v1.1.0`, `v1.1.1` and `v1.2.0` are immutable release tags on it. No minor or patch branch is needed by default. Maintaining an older minor independently would require a separately agreed support policy and tooling changes; the current tooling publishes one channel per major.

### The v1 compatibility contract

From `v1.0.0` final, every later `1.x` release keeps these working unchanged for an existing v1 install. Anything that breaks one of them needs a new major line. Before final the contract is not in force (see MASTER.md on alpha compatibility).

- **Files and documents:** `cluster.toml` (existing keys keep their meaning; new keys are optional with defaults matching the old behaviour), the secrets document `vaire-secrets/v1`, the master bundle `vaire-master-bundle/v1`, context files under `~/.config/vaire/contexts/`, and backups and WAL archives written by earlier 1.x releases (still restorable).
- **Machine output:** the `vaire/v1` JSON (fields are added, never removed, renamed or retyped) and step IDs.
- **CLI:** existing commands, flags and their meaning, and exit codes. New commands and flags may be added; deprecations warn and keep working within the line.
- **Upgrade path:** `vaire upgrade` followed by `vaire apply --auto-upgrade-nodes` moves any 1.x server to a later 1.x release.

What an upgrade may and may not require within the line:

- **Allowed in a minor release:** a database restart, provided `vaire apply` asks for it with `--confirm <db>/restart`, does nothing until confirmed, and the release notes name it (no known case after `v1.0.0`; Phase 6's `wal_level = replica` restart lands before final, in the beta).
- **Breaking (major only):** anything that needs a rebuild, a fresh `server init`, a restore, a manual migration or editing a file by hand, or that changes data already stored.

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
- **Excluded/deferred:** macOS live tests were excluded by the operator; cross-build passed. Major-upgrade design is not part of this preparation. Phases 6–7 and activity history were outside the alpha qualification; they are qualified for the beta.

No unresolved reproducible code blocker remains from the main qualification run. Publication readiness still requires reviewing the stated limits and completing the cut checklist, not silently treating missing evidence as passed.

## Toward the beta

1. Live-test activity history on edge (committed as `4df4665`).
2. Merge Phase 6 into master (rebase, conflict resolution, its manual test), then Phase 7. Expect conflicts in node/methods.go, db.go, restore.go, recover.go, steps/postgres_roles.go, restarter.go and doctor.
3. Open a pull request from `master` into `1.x` and merge it with a merge commit; tag `v1.0.0-beta.1` on `1.x` (with authorization) and qualify it as in the cut checklist.

- Until then, v1 fixes still target `1.x` and are merged up into master.
- Phase 6's `wal_level = replica` means a restart is pending on every database when upgrading from an alpha to the beta. `vaire apply` asks with `--confirm <db>/restart`, and the beta's release notes have to say so.

## Cut checklist

1. Review the testing summary, coverage limits and draft release notes. Identify the intended live test context before any further deployment: the VPS was rebuilt repeatedly, so old contexts are not interchangeable.
2. Finalize the draft notes and documentation cleanup; with authorization, run the complete `AGENTS.md` verification sequence and commit. Push master only when authorized; require its edge workflow to succeed and record the candidate commit. The current notes describe qualification through `65c4f91`, not an automatically selected final release commit.
3. Check remote branch/tag state and stable-environment configuration/protections; confirm the first intended version is unused. With explicit authorization: `git branch 1.x master && git push origin 1.x` (only if the branch does not already exist).
4. Confirm `RELEASE-v1.0.0-alpha.1.md` on `1.x` matches the approved scope and preserves the untested/alpha-compatibility disclosures. If source or notes need changes, verify and commit them with authorization before tagging; record the exact final tagged commit externally rather than embedding a self-referential commit hash in its own source.
5. With explicit authorization: `git tag -a v1.0.0-alpha.1 1.x && git push origin v1.0.0-alpha.1`. Watch `publish-stable.yml`; record its result. No synthetic tags or release-pointer manipulation for testing.
6. Verify the immutable artifacts/signatures and both root and `/1.x/` manifests identify the approved alpha. Install from the published line on a clean Linux machine. Explicitly select `--channel stable-1.x` when moving an edge CLI to the alpha; require a real version transition, not a no-op. Upgrade the agreed node normally with `apply --auto-upgrade-nodes`, then doctor and plan. Do not label stable publish/install/upgrade passed before this evidence exists.
7. Update MASTER.md, README, release notes and the testing log with the actual version/commit/workflow and release-verification results. Switch the README quick-start installer from edge to the published v1 line only after publication succeeds.

## Releasing

### Cutting `1.x`

Only when the user asks: `git branch 1.x master && git push origin 1.x`. From then on `master` is the edge channel (every push publishes `edge-<sha>`), and `1.x` carries the v1 line. A later major line is cut the same way (`2.x` from `master` when 2.0 is ready).

### Tagging an alpha

1. `git tag -a v1.0.0-alpha.N 1.x && git push origin v1.0.0-alpha.N` (never without the user).
   Tags are annotated, not GPG-signed: the release artifacts and manifests carry sigstore signatures from the workflow's OIDC identity, which is what installs verify.
2. `publish-stable.yml` runs: it checks the tag's commit is on `origin/<major>.x` (a `v1.*` tag on `origin/1.x`), then `gofmt`, `go vet` and `just lint`, then builds with `scripts/build-release.sh <version> stable-<major>.x <base-url>`. The manifest's channel is `stable`; the binary's `Channel` is `stable` and its `Version` is the tag, so `vaire upgrade` with no `--channel` follows `stable-<major>.x`.
3. It signs and uploads immutable `/releases/<version>/` artifacts, refusing an existing version prefix. The line channel `/1.x/manifest.json`, `/1.x/manifest.json.sigstore.json` and `/1.x/install.sh` moves only when the candidate passes the same monotonic/prerelease promotion policy as root. Read/list/parse errors abort rather than masquerading as an empty channel.
4. It moves the **root** channel (`/manifest.json` and `/install.sh`) only when the new version should be the highest: `internal/release/cmd/promote CURRENT CANDIDATE` prints yes when the candidate is newer by semver precedence, and a pre-release moves the root only while the root has no final release (so `1.0.0-alpha.2` moves the root off `1.0.0-alpha.1`, but `1.0.0` never moves it back down below a `2.x` final).
5. Watch the run (`gh run watch <id> --repo mbvlabs/vaire --exit-status`), then check both manifests show the version and install from `stable-1.x` on a clean machine.

### Promoting changes between branches

- **A v1 fix:** branch from `1.x`, open a pull request into `1.x`, merge it. Then open a merge-up pull request from `1.x` into `master` (`gh pr create --base master --head 1.x`) and merge it with a **merge commit**, never squash or rebase: the merge commit is what tells git the fix is already in master, so the next merge-up does not conflict on it again. Conflicts are resolved in the merge-up, against master's newer code.
- **Merge up often:** after every fix, or at the latest before each tag on `1.x`, so master never falls far behind the line.
- **Edge testing of a v1 fix:** the merge-up push to master publishes `edge-<sha>`, which carries the fix together with master's newer work. That tests the fix on edge, not the exact `1.x` tree; the release candidate itself is checked with a pre-release tag (below).
- **Compatible features after final:** while master holds only work that is compatible with the v1 contract, bring it into the line with a pull request from `master` into `1.x`, also merged with a merge commit. Once master holds breaking work, this stops: from then on compatible v1 features target `1.x` directly (and are merged up), and breaking work stays on master for the next major.
- **Qualifying a line release:** before a final or minor release, tag a pre-release on `1.x` (`v1.1.0-rc.1`) and run the manual tests against it. After `v1.0.0` a pre-release does not move the `1.x` or root channels, so it is installed explicitly from `/releases/<version>/`. Before final the alpha and beta tags play this role.
- Release notes name the features and fixes included in each release.

### Which channel an install follows

- **Root install** (`https://…/install.sh`): tracks the root manifest, the highest stable line. After `2.0.0`, that is `2.x`.
- **Line install** (`https://…/1.x/install.sh`): pins `1.x`; it keeps installing 1.x patches even after `2.0.0`.
- **`vaire upgrade`**: with no `--channel`, a 1.x binary follows `stable-1.x` (its own line) and a 2.x binary `stable-2.x`; a dev build must pass `--channel`. `vaire upgrade --channel stable` moves to the highest stable line explicitly; `--channel stable-1.x` pins the line; `--channel edge` follows `master`.
- **Edge install**: `https://…/edge/install.sh`, the edge manifest on every push to `master`.

### GitHub-side settings

- The `edge` and `stable` environments each hold the `R2_*` secrets (`R2_ENDPOINT`, `R2_ACCESS_KEY_ID`, `R2_SECRET_ACCESS_KEY`); `RELEASE_BUCKET` and `RELEASE_BASE_URL` are repository variables. `stable` has to be created, with its secrets, before the first tag.
- Branch protection on `1.x` and `master`: no force-push, no deletion, changes through pull requests only; a stable tag is pushed by a maintainer only.
- Merge commits must stay enabled in the repository settings (they are today, alongside squash and rebase): merge-up pull requests between `1.x` and `master` always use them. Feature and fix pull requests into a branch may be squashed.
- A pull-request workflow running the `AGENTS.md` verification sequence (build, vet, test, golangci-lint, darwin cross-build, `just lint`) should exist and be a required check on both branches; today only the edge and stable publish workflows exist, so a pull request has no automated check.
- Tag protection for `v*`: only maintainers may create or delete a release tag, so `publish-stable.yml` cannot be pointed at an arbitrary commit.
- The `stable` environment may require a reviewer if more than one person can push tags. The release workflow's OIDC identity is what `install.sh` and `vaire upgrade` pin, so the environment name and workflow path must not change.

## Open questions

- Is anyone besides you going to install the alpha? If so, where do they report issues (GitHub issues on `mbvlabs/vaire`, or private)?
- Edge continues publishing on every push to master after the cut, as already decided above.
