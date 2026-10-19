# Handoff: fixes before the v1 alpha (2026-10-16)

> **Archived 2026-10-19: completed work record, not active instructions.** The original fixes and review follow-ups are implemented; main live qualification is complete. Two live-discovered regressions were fixed and retested in `eaa1a73` and `65c4f91`. Current status and coverage limits: [MASTER](../MASTER.md), [testing evidence](../MANUAL-TESTING.md), [release checklist](../PLAN-v1-release.md). The steps below describe the historical handoff; they do not authorize further commits, deployments or release actions.

If this session stops (usage limit, crash), a new session can continue from this file. The release plan and the decisions are in `docs/PLAN-v1-release.md`.

## State when written

- master is at `25ce402` and clean before the work below. thelab's CLI is `edge-65c68b7`.
- **Test VPS 46.224.129.96** (`db2.mbvlabs.com`): context `nobucket` (no bucket), with one database `app`. Context `mbvlabs` (R2 bucket `eru-backups-test`, prefix `mbvlabs`) has no live server; it can be restored from the bucket.
- **Seven sub-agents were started in parallel**, all told not to commit. Six edit the master working tree (`/home/mbv/work/eru`) and the seventh only writes a doc.

| Area | Scope | Main files |
| --- | --- | --- |
| TOTP gaps | `db.write-access` refused while awaiting recovery; `pg_maintain` lets `eru_admin` `LOCK TABLE` on a protected database; a TOTP write to a dump copy of a protected database fails with permission denied | internal/node/awaiting.go, protectwrite.go, maint.go, steps/postgres_roles.go |
| Tunnel | The daemon dial times out during a WireGuard re-handshake after the tunnel has been idle; the hint blames UDP 51820 and suggests break-glass; `node unlock-ssh` gives a confusing error on an operator machine | internal/transport |
| Master backup | `server init` must upload the first bundle and write the epoch; the first bundle is named `-e0-` while the epoch is 1; doctor `host.disk_data` warns with no databases; the pgBackRest exit-50 lock race at `stanza-create` | internal/node/masterbackup.go, cli/serverinit*, internal/doctor |
| CLI UX | Error boxes wrap; `metrics show` mentions `--reveal`; `eru sql` runs several statements; `maint repack` checks before it confirms; `config get` prints TOML/JSON, not Go structs; `queries`/`top` hide eru's own sessions | internal/cli, internal/cli/ui |
| Secrets export gate | Master password (argon2id hash in the context, set at `server init` or with a new command) OR a TOTP code verified by the node; no flag or environment variable (agents can't export); carried over by `eru login`; same gate on other full-document reveals | internal/cli/secrets.go, contexts, a node verify method |
| Release tooling | `1.x` branch model: `-alpha.N`/`-beta.N`/`-rc.N` versions with semver precedence; publish-stable checks the tag is on `origin/<major>.x`; per-major channel `/1.x/manifest.json`; the root manifest only for the highest version; fills "Releasing" in PLAN-v1-release.md | .github/workflows, scripts, internal/release, internal/upgrade |
| Major upgrades RFC | Design only: `docs/RFC-major-upgrades.md` (format inventory, the major-version promise, the upgrade flow, migrations, testing, what must land in 1.x) | docs only |

## Specs per area

The working rules are in `AGENTS.md` at the repo root. Other agents may be editing the same tree, so touch only the files your fix needs, never run `gofmt -w` over the whole tree, and don't revert changes you didn't make.

### TOTP gaps
Read "Writing to a protected database" in IMPLEMENTATION.md and the protect/TOTP code (internal/node/protecttotp.go, protectwrite.go, sql.go, maint.go, awaiting.go, steps/postgres_roles.go).
1. `db.write-access`, the RPC that makes a short-lived `eru_write_<hex>` role after a TOTP code, must be refused while the database awaits recovery, like the other methods in awaiting.go. Add a test.
2. `eru_admin` holds `pg_maintain` for the `maint` commands, which on a protected database still lets it `LOCK TABLE` the application's tables. That's a denial-of-service hole past the TOTP gate. Close it at the root (for example no `pg_maintain` on protected databases, with `maint` running through a path that still works). Protect, unprotect and `maint` must all keep working. Update the grants in IMPLEMENTATION.md.
3. A TOTP-gated `eru sql --db <copy> --write` to a dump copy of a protected database (a dump loaded into a new cluster by `restore <db> --dump …`, `db fork` or wherever copies are made) fails with "permission denied" even after a valid code. Find why (the copy's roles or grants, the write-access role's membership, or protection inherited without its owner role) and make the write work, or make the copy unprotected if that's what's intended.

### Tunnel
Read docs/PLAN-shared-tunnel.md and internal/transport (shared.go and friends).
1. The shared daemon (`eru tunnel serve`) owns the context's WireGuard device, and every command dials through its unix socket. After the tunnel has been idle, a command failed with `cannot reach 10.42.0.1:22: the tunnel daemon did not answer: read unix …/tunnel.sock: i/o timeout [transport_failed]`; seconds later the handshake completed and the same command worked. The dial and the client's read deadline must allow for a WireGuard handshake (REKEY_TIMEOUT 5 s, with retries), or the daemon replies as soon as it starts dialling. Also check whether the daemon blocks on the handshake (a single-threaded accept loop, or a netstack dial with a short timeout).
2. The hint must match the cause: daemon not running, versus the peer not answering after a real handshake timeout. Not UDP 51820 and break-glass for a transient timeout.
3. `eru node unlock-ssh` on an operator machine must say clearly that it runs on the server (as root, for example from the provider's console) and what to do from the operator machine instead.

### Master backup and doctor
Read docs/PHASE-5-WORK-UNITS.md.
1. `server init` with a bucket must make and upload the first master bundle, and write epoch 1, before it reports ready. If the upload fails, init says so clearly. Code: internal/node/masterbackup.go, internal/cli/serverinit*.go, step `master-backup-v1`.
2. That first bundle is named `…-e0-…` while the bucket's epoch is 1, because it's made before the epoch is written. Check whether verify, restore or the fence trip on it, and fix the order so a bundle's name carries its epoch.
3. Doctor's `host.disk_data` warns "cannot read the data root's filesystem" on a server with no databases (the data root likely doesn't exist yet). It should pass or skip honestly.
4. pgBackRest exited 50 (lock busy) once at `stanza-create` during a create or apply on a fresh server. Find what holds the lock at that moment (archive-push, `check`, a concurrent step) and fix the order or wait properly. A blind retry is acceptable only if the holder is transient and unavoidable, and then document why.

### CLI UX
1. Error boxes (internal/cli/ui) wrap their content to the terminal width, keeping the border intact.
2. `eru metrics show` says how to reveal the password (`--reveal`).
3. `eru sql --db app "stmt1; stmt2"` works, where it failed with "cannot insert multiple commands into a prepared statement". Use the simple query protocol when there are no parameters, print each statement's result or tag, and keep the TOTP `--write` path working.
4. `eru maint repack` checks its precondition (the extension is missing) before asking for `--confirm <db>`.
5. `eru config get maintenance` prints TOML or `key = value` lines in human output and JSON objects with `--json`, not a Go struct (`{02:00-05:00 false 0.4}`).
6. `eru queries` and `eru top` hide eru's own sessions (by application_name or eru's roles) by default, with a flag to include them if that fits the flag style.

### Secrets export gate
The user's requirement: `eru secrets export` (internal/cli/secrets.go, `runSecretsExport`) needs a master password OR a TOTP code. If the secrets document is lost, too bad: no bypass, no "edit this file" hint, no reset flow.

**Decided design: a command gate, not encryption at rest.**
- **Master password:**
  - asked during `eru server init`, or set for an existing context with a new command (for example `eru secrets password set`; fit it to internal/cli/tree.go);
  - changing it needs the current password, or a TOTP code if protection is set up;
  - only an argon2id hash (salt and params, `golang.org/x/crypto/argon2`) is stored in the context files (0600), with a sensible minimum length;
  - `eru login` (importing a transfer bundle on another machine) carries the hash over.
- **TOTP:** when the server has protection set up (a confirmed TOTP secret, internal/node/protecttotp.go), a code verified by the node over RPC also works. Reuse the existing verify path and its replay protection. If a new node method is needed, register it like the others (a test covers every registered method).
- **Export:**
  - accepts either factor; when both exist it asks for the TOTP code first, with an option to use the password;
  - with neither, it refuses and names the command that sets a master password;
  - there's no flag or environment variable for the password or code: with `--json`, `--no-input` or no TTY it refuses with a new structured error code (for example `secret_gate_required` in internal/output, listed in tree.go and schema/eru-v1.json where codes are listed). Agents must not be able to export.
- **Other commands** that reveal the whole document or a new handoff (for example `showContextHandoff`, used by `eru backup setup`) take the same gate. Decide which belong to this class and record why.
- **Docs:** IMPLEMENTATION.md, the command help, and internal/guide/agents.md, which should say agents can't export secrets.
- **Live test:** the `mbvlabs` and `nobucket` contexts on thelab have no password yet.

### Release tooling
Files: .github/workflows/publish-edge.yml, publish-stable.yml, scripts/build-release.sh, sign-release.sh, upload-release.sh, install.sh, internal/release (identity.go, manifest.go, client.go), internal/upgrade (plan.go).
1. **Versions:** stable versions may be `MAJOR.MINOR.PATCH` with an optional `-alpha.N`, `-beta.N` or `-rc.N`, everywhere they're checked: build-release.sh, `release.matchVersion`, `Identity` (the SAN for `refs/tags/v<version>`) and install.sh's identity regexp. `upgrade.cmpSemver` follows semver precedence: `1.0.0-alpha.2 < 1.0.0-alpha.10 < 1.0.0-beta.1 < 1.0.0-rc.1 < 1.0.0`. Add table tests.
2. **publish-stable.yml:** the tag's commit must be on `origin/<major>.x` (`v1.*` tags on `origin/1.x`), instead of being an ancestor of master.
3. **Channels:**
   - Every stable release goes to `/1.x/manifest.json` (plus its sigstore bundle and install.sh) and to `/releases/<version>/`.
   - The root manifest moves only for the highest stable version; while no final release exists, alphas may move it.
   - Add a per-major channel (for example `stable-1.x`) to `eru upgrade --channel` and `release.Client.Resolve`.
   - A binary built from a 1.x tag defaults to its line's channel; moving to 2.x is explicit.
   - install.sh works for both the root and the per-major channel.
4. **Docs:** fill the "Releasing" section in PLAN-v1-release.md (cutting `1.x`, tagging an alpha, which way to cherry-pick, which channel each install follows), update the release row in MASTER.md, and list any GitHub-side settings needed (environments, tag protection).

### Major upgrades RFC
Design only, written to `docs/RFC-major-upgrades.md`, with no code. It covers eru's own 1.x → 2.x upgrade, not PostgreSQL majors:
1. an inventory of every persisted and wire format (cluster.toml, `eru-secrets/v1`, contexts, journal, records.json, step IDs, `eru-rpc/v1` and the handshake, `eru/v1` JSON, the release manifest, the tunnel socket protocol, `eru-master-bundle/v1`), where each lives and whether it's versioned;
2. what a major version promises within a line and across lines;
3. the upgrade flow end to end, with preflight, ordering, a point of no return, rollback, a second operator machine still on 1.x, and old bundles in the bucket;
4. the migration mechanics: an ordered, idempotent chain per format, backups, the record of which migrations ran, and lockstep during the window;
5. testing without docker (golden files of old formats, a manual run from the last 1.x tag on the VPS);
6. what must land in 1.x before `1.0.0` final;
7. open questions.

## How to continue

1. Look at `git status` and `git diff --stat` to see what landed. For each area above, check whether its change is complete: read the diff, then build and test.
2. Finish any area that's missing or half done, using the scope in the table. Fix root causes; no local docker or test harnesses; don't change unrelated files.
3. Verify the whole tree: `go build ./...`, `go vet ./...`, `go test ./...`, `golangci-lint run ./...`, `GOOS=darwin GOARCH=arm64 go build ./...`, `just lint`.
4. Commit (no Claude attribution or Co-Authored-By; split per area when the files separate cleanly), push master, then `gh run watch <id> --repo mbvlabs/eru --exit-status`.
5. On thelab: `eru upgrade --yes`, `eru -c nobucket apply --auto-upgrade-nodes`, `eru -c nobucket doctor`.
6. Open the RFC for review: `plannotator annotate docs/RFC-major-upgrades.md --tailscale` (run it in the background and give the user only the link).
7. **Live re-tests** (log them in `docs/MANUAL-TESTING.md`):
   - `eru psql --db app --write` and `eru console app --write`;
   - the TOTP gaps;
   - the tunnel after idling;
   - a fresh `server init` with a bucket uploading its first bundle;
   - the secrets export gate on both contexts;
   - the macOS install;
   - the lockdown revert and `server init --local`.
   The root SSH key is only on the user's desktop, so `server init` runs there (`--identity ~/.ssh/mbvlabs_prod_one`).
8. Then follow the cut checklist in `docs/PLAN-v1-release.md`. Don't cut `1.x` or tag without the user.

Not part of this work: Phase 6 (worktree `.claude/worktrees/phase-6-followers`) and Phase 7 (`.claude/worktrees/agent-phase7-extensions`) wait for the cut and the user's review.
