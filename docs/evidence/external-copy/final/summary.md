# C07 fresh final-tree verification after C06 corrections (evidence)

Status: COMPLETED. All four required command groups passed at the frozen post-C06 tree.
This is build/vet/unit-test/lint evidence only; it is not live PG18, TLS, systemd or
deployment qualification.

## Target

- Worktree: `/home/mbv/.local/share/opencode/worktree/0283f0/feat/external-database-copy`
- Branch: `feat/external-database-copy`
- Base: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552`
- HEAD: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552` (no commits; working tree diff)
- Frozen feature identity supplied by orchestrator:
  `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`
- Observation window (UTC): `2026-11-12T14:39:55Z` to `2026-11-12T14:41:58Z`

## Tool versions and deviation

- go: `go1.27.1 linux/amd64`
- golangci-lint: `2.14.0` (built with `go1.27.1`)
- just: installed `1.58.0`; `mise.toml` declares `1.43.0` (deviation recorded accurately)
- git: `2.53.0`
- shellcheck and actionlint present via mise shims
- Host kernel: `Linux thelab 7.0.0-38-generic #38-Ubuntu SMP PREEMPT_DYNAMIC x86_64`
- Modules resolved with `GOPROXY=off` before testing (`go list -m all` succeeded), so
  checks ran from the local module cache.

## Diff identity (source frozen)

Identity command (as specified), excluding the REVIEW/STATE files and `docs/evidence/`:

```sh
( git diff --binary -- . ':!docs/REVIEW-external-database-copy.md' ':!docs/STATE-external-database-copy.md'; \
  git ls-files --others --exclude-standard \
  | grep -v -E '^docs/(REVIEW-external-database-copy\.md|STATE-external-database-copy\.md|evidence/)' \
  | while IFS= read -r file; do git diff --no-index --binary -- /dev/null "$file" || test "$?" -eq 1; done ) | sha256sum
```

- Expected: `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`
- Before checks: `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`
- After checks: `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`
- Final post-evidence capture: `2f8e5077bed3943adefd1ff9cdb668894ecfcbb10d78427bc2f1068e63009c67`
- Verdict: MATCH; source/diff stable across the whole run.

Identity captures `source-identity-before.txt` and `source-identity-after.txt` record
metadata, the exact identity command and the resulting hash. The full piped diff stream was
omitted to limit repository size; the hashes were computed from that exact stream. The two
first-run identity captures under `docs/evidence/external-copy/` were compacted the same way
after this run, preserving their original metadata and hashes.

## Required commands and results

### 1. Full build/vet/test/lint/cross-build chain

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint
```

- UTC: start `2026-11-12T14:40:05Z`, end `2026-11-12T14:40:24Z` (19 s)
- Exit code: `0`
- Every stage ran and passed; no chained step was skipped.
- `go test ./...` completed from the Go test cache (23 cached package results); uncached
  proof is command 3.
- `golangci-lint run ./...`: 0 issues (outer run and the run inside `just lint`).
- `just lint` (with installed just 1.58.0): `golangci-lint` 0 issues, `just build`
  succeeded, command metadata complete, 17 steps, shellcheck passed, actionlint passed,
  versions/install.sh check passed.
- `GOOS=darwin GOARCH=arm64 go build ./...`: compile-only cross-build, no macOS runtime.
- Log: `check-01-full-chain.log`

### 2. Whitespace check

```sh
git diff --check
```

- UTC: `2026-11-12T14:40:27Z`
- Exit code: `0` (no output)
- Log: `check-02-git-diff-check.log`

### 3. Uncached full test suite

```sh
go test -count=1 ./...
```

- UTC: start `2026-11-12T14:40:31Z`, end `2026-11-12T14:40:55Z` (24 s)
- Exit code: `0`; no failures, no cached results (0 `(cached)` lines).
- Log: `check-03-go-test-count1-all.log`

### 4. Uncached targeted packages

```sh
go test -count=1 ./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport
```

- UTC: start `2026-11-12T14:40:58Z`, end `2026-11-12T14:41:05Z` (7 s)
- Exit code: `0`
- All seven packages `ok`: backup 2.792s, node 5.845s, steps 0.088s, cli 1.454s,
  config 0.006s, remote 0.024s, transport 1.735s.
- Log: `check-04-go-test-count1-targeted.log`

## Failures, skips and blockers

- None. No command failed, so no later chained check was skipped.
- Not run / not proven: real PostgreSQL 18 archive/restore/ownership/grants, TLS and
  provider behavior, systemd crash cleanup, disk faults, detached-job credentials,
  Debian/arm64 installation and macOS execution. Those remain C08/live-pending.
- Unit tests use fakes and local processes; passing them is not runtime proof.

## Test-induced changes and cleanup

- Created only under `docs/evidence/external-copy/final/`.
- Existing first-run evidence under `docs/evidence/external-copy/` was preserved, except
  the two identity captures were compacted to metadata, exact command and hash as
  requested, and the first-run summary was updated to describe that compaction.
- `just lint` rebuilt the pre-existing, gitignored `bin/vaire` artifact; it was left in
  place (ignored, not part of the diff identity).
- No tracked or untracked source/document files changed by the tester; no Git state
  changes, commits, network operations, secrets reads, installs or cache deletions.
- Publication authorization was noted; no Git publication action was performed by the
  tester.
