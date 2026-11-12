# C07 fresh final-tree verification (evidence)

Status: COMPLETED. All four required command groups passed at the frozen tree. This is
build/vet/unit-test/lint evidence only; it is not live PG18, TLS, systemd or deployment
qualification.

## Target

- Worktree: `/home/mbv/.local/share/opencode/worktree/0283f0/feat/external-database-copy`
- Branch: `feat/external-database-copy`
- Base: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552`
- HEAD: `0d8e1fcc493cd30ca4afb89b54d6bd050302f552` (no commits; working tree diff)
- Observation window (UTC): `2026-11-12T14:20:56Z` to `2026-11-12T14:23Z` (approx.)

## Tool versions

- go: `go1.27.1 linux/amd64`
- golangci-lint: `2.14.0` (built with `go1.27.1`)
- just: `1.58.0`
- git: `2.53.0`
- shellcheck and actionlint present via mise shims
- Host kernel: `Linux thelab 7.0.0-38-generic #38-Ubuntu SMP PREEMPT_DYNAMIC x86_64`
- Modules resolved with `GOPROXY=off` before testing (`go list -m all` succeeded), so the
  checks ran from the local module cache.

## Diff identity (source frozen)

Identity command (as specified), excluding the REVIEW/STATE files and `docs/evidence/`:

```sh
( git diff --binary -- . ':!docs/REVIEW-external-database-copy.md' ':!docs/STATE-external-database-copy.md'; \
  git ls-files --others --exclude-standard \
  | grep -v -E '^docs/(REVIEW-external-database-copy\.md|STATE-external-database-copy\.md|evidence/)' \
  | while IFS= read -r file; do git diff --no-index --binary -- /dev/null "$file" || test "$?" -eq 1; done ) | sha256sum
```

- Expected: `c5cf7e0b9eff817825e5b24853d77df83fed2ac449eb66b473706d9791f377a7`
- Before checks: `c5cf7e0b9eff817825e5b24853d77df83fed2ac449eb66b473706d9791f377a7`
- After checks: `c5cf7e0b9eff817825e5b24853d77df83fed2ac449eb66b473706d9791f377a7`
- Verdict: MATCH; source/diff stable across the whole run.

Identity captures: `source-identity-before.txt`, `source-identity-after.txt`.
These were compacted after the run to metadata, the exact identity command and the
resulting hash; the full piped diff stream was omitted to limit repository size. The
recorded hashes were computed from that exact stream and are unchanged.

## Required commands and results

### 1. Full build/vet/test/lint/cross-build chain

```sh
go build ./... && go vet ./... && go test ./... && golangci-lint run ./... && GOOS=darwin GOARCH=arm64 go build ./... && just lint
```

- UTC: start `2026-11-12T14:21:26Z`, end `2026-11-12T14:21:39Z` (13 s)
- Exit code: `0`
- Every stage ran and passed; no chained step was skipped.
- `go test ./...` completed from the Go test cache (29 cached package results); uncached
  proof is command 3.
- `golangci-lint run ./...`: 0 issues (outer run and the run inside `just lint`).
- `just lint`: `golangci-lint` 0 issues, `just build` succeeded, command metadata complete,
  17 steps, shellcheck passed, actionlint passed, versions/install.sh check passed.
- `GOOS=darwin GOARCH=arm64 go build ./...`: compile-only cross-build, no macOS runtime.
- Log: `check-01-full-chain.log`

### 2. Whitespace check

```sh
git diff --check
```

- UTC: `2026-11-12T14:21:45Z`
- Exit code: `0` (no output)
- Log: `check-02-git-diff-check.log`

### 3. Uncached full test suite

```sh
go test -count=1 ./...
```

- UTC: start `2026-11-12T14:21:49Z`, end `2026-11-12T14:22:14Z` (25 s)
- Exit code: `0`; no failures, no cached results (0 `(cached)` lines).
- Log: `check-03-go-test-count1-all.log`

### 4. Uncached targeted packages

```sh
go test -count=1 ./internal/backup ./internal/node ./internal/steps ./internal/cli ./internal/config ./internal/remote ./internal/transport
```

- UTC: start `2026-11-12T14:22:18Z`, end `2026-11-12T14:22:25Z` (6 s)
- Exit code: `0`
- All seven packages `ok`: backup 2.816s, node 5.883s, steps 0.089s, cli 1.422s,
  config 0.007s, remote 0.016s, transport 1.712s.
- Log: `check-04-go-test-count1-targeted.log`

## Failures, skips and blockers

- None. No command failed, so no later chained check was skipped.
- Not run / not proven: real PostgreSQL 18 archive/restore/ownership/grants, TLS and
  provider behavior, systemd crash cleanup, disk faults, detached-job credentials,
  Debian/arm64 installation and macOS execution. Those remain C08/live-pending.
- Unit tests use fakes and local processes; passing them is not runtime proof.

## Test-induced changes and cleanup

- Created only under `docs/evidence/external-copy/` (this summary plus four command logs
  and two identity captures).
- `just lint` rebuilt the pre-existing, gitignored `bin/vaire` artifact; it was left in
  place (ignored, not part of the diff identity).
- No tracked or untracked source/document files changed; no Git state changes, commits,
  network operations, secrets reads, installs or cache deletions.
