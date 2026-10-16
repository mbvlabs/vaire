# Plan: cutting the v1 release

Phases 0–5 have passed their manual tests. This file records how v1 is released and what has to land first.

## Decided (2026-10-16)

- **Branch model (Laravel-style):**
  - a `1.x` branch is the v1 line, and its tags are the stable releases;
  - `master` stays the edge channel (Phases 6 and 7 and beyond, `edge-<sha>` on every push);
  - fixes are cherry-picked from master to `1.x` and the other way round (`git cherry-pick -x`).
- **Version names:** v1 is an alpha, so `1.x` is tagged `v1.0.0-alpha.1`, `v1.0.0-alpha.2`, …, then `v1.0.0` when the formats freeze.
- **Not ready yet:** v1 isn't cut until everything under "Must land before the first alpha" is fixed and re-tested.
- **Secrets export gets a gate:** `eru secrets export` needs the master password or a TOTP code. If the secrets document is lost, it's lost; there's no bypass.
- **Phases 6 and 7** merge into master after `1.x` is cut, one at a time, and ship in a later line.

## What exists today, and what the model needs

- Edge is published on every push to `master`. Stable is published from a `vX.Y.Z` tag into the site root, and every version is kept under `/releases/<version>/`. Nothing has been published to stable yet.
- **What blocked the model, now fixed:**
  - `publish-stable.yml` checks that a `v<major>.*` tag's commit is on `origin/<major>.x`, so tags on `1.x` publish;
  - pre-release versions (`-alpha.N`, `-beta.N`, `-rc.N`) parse and order by semver precedence everywhere (`build-release.sh`, `release.ParseStable`, `Identity`, `install.sh`'s identity regexp, `upgrade.cmpSemver`);
  - a per-major channel manifest (`/1.x/manifest.json`) is published beside the version, and a binary from a 1.x tag follows its line by default;
  - the root manifest moves only for the highest stable version.
- **Version lockstep:** a node runs exactly the CLI's build, so each install moves its CLI and nodes along one line.

## Must land before the first alpha

**Fixes (agents working on them now):**
- **TOTP protection:**
  - `db.write-access` isn't in the awaiting-recovery refusal list;
  - `pg_maintain` still allows `LOCK TABLE` on a protected database;
  - a write to a dump copy of a protected database fails with permission denied.
- **The secrets export gate:**
  - a master password (argon2id hash in the context) set at `server init`, or by a new command for existing contexts;
  - export accepts it, or a TOTP code verified by the node when protection is set up;
  - no flag or environment variable, so agents can't export.
- **Tunnel:** the daemon's dial gives up during a WireGuard re-handshake after the tunnel has been idle. The hint blames UDP 51820 and suggests break-glass. `node unlock-ssh` gives a confusing error on an operator machine.
- **Master backup and doctor:**
  - after `server init` the first bundle isn't uploaded until the next change;
  - the first bundle is named `-e0-` while the epoch is 1;
  - doctor's `host.disk_data` warns on a server with no databases;
  - the pgBackRest exit-50 lock race at `stanza-create`.
- **CLI UX:**
  - error boxes don't wrap;
  - `metrics show` doesn't mention `--reveal`;
  - `eru sql` rejects several statements;
  - `maint repack` asks for confirmation before refusing;
  - `config get` prints Go structs;
  - `queries` and `top` show eru's own queries.

**Re-tests on the test VPS after the fixes:**
- `eru psql --db app --write` and `eru console app --write`.
- Each fix above that has live behaviour.
- The macOS install from the release channel and an `eru upgrade` from it.
- The lockdown revert and `server init --local`.

**Upgrades between major versions:** designed in `docs/RFC-major-upgrades.md`. Whatever it says must land in 1.x before `1.0.0` final, such as format version fields and the migration skeleton, gets added here.

## After the cut

- Merge Phase 6 into master (rebase, conflict resolution, its manual test), then Phase 7. Expect conflicts in node/methods.go, db.go, restore.go, recover.go, steps/postgres_roles.go, restarter.go and doctor.
- Phase 6's `wal_level = replica` means a restart is pending on every database when upgrading to it. `eru apply` asks with `--confirm <db>/restart`, and the release notes have to say so.

## Cut checklist

1. All fixes above on master; CI green; thelab and the test VPS upgraded; re-tests logged in `MANUAL-TESTING.md`.
2. Release tooling merged on master and published through edge once.
3. `git branch 1.x master && git push origin 1.x`.
4. Release notes: what works, what's untested, and the alpha promise (formats may change, and a later alpha may need a rebuild and restore).
5. `git tag -s v1.0.0-alpha.1 1.x && git push origin v1.0.0-alpha.1`; watch `publish-stable.yml`.
6. Check the root and `/1.x/` manifests show `1.0.0-alpha.1`. Install from stable on a clean machine, then `eru upgrade` on thelab to the alpha, `eru apply --auto-upgrade-nodes`, and `eru doctor`.
7. Update MASTER.md's status: Phases 0–5 released as 1.0.0-alpha.1.

## Releasing

### Cutting `1.x`

Only when the user asks: `git branch 1.x master && git push origin 1.x`. From then on `master` is the edge channel (every push publishes `edge-<sha>`), and `1.x` carries the v1 line. A later major line is cut the same way (`2.x` from `master` when 2.0 is ready).

### Tagging an alpha

1. `git tag -s v1.0.0-alpha.N 1.x && git push origin v1.0.0-alpha.N` (never without the user).
2. `publish-stable.yml` runs: it checks the tag's commit is on `origin/<major>.x` (a `v1.*` tag on `origin/1.x`), then `gofmt`, `go vet` and `just lint`, then builds with `scripts/build-release.sh <version> stable-<major>.x <base-url>`. The manifest's channel is `stable`; the binary's `Channel` is `stable` and its `Version` is the tag, so `eru upgrade` with no `--channel` follows `stable-<major>.x`.
3. It signs and uploads `/releases/<version>/` and the line channel `/1.x/manifest.json`, `/1.x/manifest.json.sigstore.json` and `/1.x/install.sh`.
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
- Should edge keep publishing on every push to master once stable exists?
