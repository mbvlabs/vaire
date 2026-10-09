# eru M2: Engine & node runtime — implementation plan

> Executes [`M2-ENGINE-NODE-RUNTIME.md`](./M2-ENGINE-NODE-RUNTIME.md) (the spec). The spec is the source of truth for *what*. This plan is the *order of work* and how each step is checked.
> Branch: `feat/m2-engine-node-runtime` off `master`. Status: approved, ready to execute · 2026-10-09

## Ground rules

- Tasks run in order. Each ends with a **Check** that must pass before the next task starts.
- No `_test.go` outside `e2e/`. Checks are `just lint`, `just build`, runs of the built binary, and E26.
- Commit after each task (`feat(m2): …`), so a bad step can be reverted on its own.
- Spec references are written `S§n`.
- M2 is node-only. Checks before T6 run an `e2etest` build as root in a throwaway incus VM, since the node side touches `/var/lib/eru`, `/var/log/eru` and `/run/lock`.

---

## T1. Foundations: secrets, rpc, config

1. `internal/secrets/registry.go`: `Registry` (mutex, `Add`, `Values`), `Redact(string)` longest-first with `«redacted»`, `RedactValue(any)` that walks maps, slices and structs via a JSON round-trip. A package-level `Default` registry.
2. `internal/rpc/types.go`: `Request` (S§4.1), `Schema = "eru-rpc/v1"`, `NewRequestID()` (ULID), `DecodeRequest(io.Reader)` with the 1 MiB cap and strict decode.
3. `internal/config/node.go` and `cluster.go`: `LoadNode(path)`, `LoadCluster(path)` per S§8. `cluster_e2etest.go` adds the `[e2e]` table.

**Check:** `just lint`. `GOOS=darwin go build ./...` still builds.

---

## T2. Engine core: journal, lock, run log, shell

1. `internal/engine/journal.go`: types per §9.3, `Load` (missing → empty, corrupt → `precondition_failed`/`journal_corrupt`), `Save` with atomic write + fsync of file and dir.
2. `internal/engine/lock.go`: `Acquire(ctx, mode, method, requestID, timeout) (Release, error)` per S§5.4, the holder info file, PID liveness via `syscall.Kill(pid, 0)`.
3. `internal/engine/runlog.go`: `OpenRunLog(method, requestID)` per S§5.6. All writes go through `secrets.Redact`.
4. `internal/engine/shell.go`: `Shell.Run` with line streaming, env-key validation, redaction, the DryRun refusal, and SIGTERM on context cancel (S§5.7).

**Check:** `just lint`. In a VM, a scratch `go run` (deleted after) proves: two exclusive acquirers → the second times out with the holder; shared + shared succeeds; a killed holder → `holder: null`; a corrupted journal file → `journal_corrupt`.

---

## T3. Runner, plan, catalogue

1. `internal/engine/step.go`: `Step`, `Runtime`, `CheckResult`, `ApplyResult`, `RestartNeed`, `Reporter`, `Restarter`, the ID regexp.
2. `internal/engine/runner.go`: `Apply(ctx, steps, cfg, rt, opts)` and `Plan(...)` per S§5.2–5.3: step filtering, restart aggregation, interrupted handling, duplicate-ID refusal.
3. `internal/steps/catalog.go` (`!e2etest`): `Catalog()` returns nil, `Restarter()` returns nil.
4. `internal/steps/catalog_e2etest.go` + `scripts/stub-*.sh`: the five stub steps and the stub restarter (S§5.7).
5. `eru commands --check-steps` (hidden flag): validates IDs and uniqueness and prints the count. Add it and the `ERU_TEST` symbol check to `just lint` (S§2).

**Check:** `just lint`; `go build -tags e2etest ./...`; `shellcheck` covers the stub scripts. On the production build, `--check-steps` reports 0 and `go tool nm bin/eru | grep -c ERU_TEST` is 0.

---

## T4. Node dispatcher and node commands

1. `internal/node/dispatch.go`: S§4.2 steps 1–8 in order, the method table (S§4.3), panic recovery, the redacting and flushing event writer, exit with the envelope's code.
2. `internal/node/methods.go`: `plan` and `apply` wired to the engine with `config.LoadCluster` and the confirm token `{cluster}/restart`.
3. `internal/node/selfcheck.go`: checks per S§7.1.
4. `internal/cli/node.go`: `node rpc` and `node selfcheck` replace `notImplemented`. Update their `CommandMeta` (`ErrorCodes`, `Prerequisites`).

**Check:** In a VM with an `e2etest` build, `node.toml` and `cluster.toml`, piping hand-written requests into `eru node rpc` as root: `plan` → 5 × `apply`; `apply` → journal + run log; second `apply` → all skipped; as `nobody` → exit 11; unknown method → exit 1; wrong `client_commit` → exit 10; wrong confirm → exit 4; the secret never appears in stdout or the log (`grep -c`).

---

## T5. `upgrade.install`

1. `internal/node/install.go`: S§7.2 steps 1–6, exempt from the version check, under the exclusive lock.

**Check:** In the T4 VM, with a second `e2etest` build pushed to `/tmp/eru-b`: a wrong sha → exit 14 and an unchanged binary (sha256 before/after); the right sha → the new version is installed and `eru.prev` runs the old one; a build whose selfcheck fails (temporarily `chmod 000 /var/lib/eru` from the test shell) → rolled back with `selfcheck_failed_rolled_back`.

---

## T6. E2E: E26

1. Harness:
   - `vm.ExecStdin(stdin, args...)` for piping a request into `incus exec`;
   - `harness.NodeBuild(t, vm, commit)` builds `e2etest` linux binaries, with `ldflags` overriding `Commit` only;
   - `harness.PrepareNode(t, vm)` pushes the binary and writes `node.toml`, `cluster.toml` and `/etc/eru-e2e/`;
   - `harness.RPC(t, vm, method, params, opts) RPCResult` builds the request from `eru node version`, sends it, and returns the parsed events, the result envelope and the exit code. It fails the test on any malformed line or a missing or non-final `result`.
2. `e2e/node_runtime_test.go` (E26), with sub-tests exactly as S§9. Snapshot after `PrepareNode`; `Restore` before sub-tests that need a clean journal, as E25 does.

**Check:** `just e2e` passes locally. The PR's e2e job is green on Debian 13.

---

## T7. Docs and close-out

1. `docs/DEVIATIONS.md`: every S§10 entry.
2. `docs/PHASE-1-IMPLEMENTATION.md`: §22.2 adds E26; §24 marks M2 done, changes M2's scenario to E26, and adds the handshake, binary push, E18 and E27 to M3; §7.4 points at the request additions.
3. `internal/guide/agents.md`: what `lock_held` and `restart_pending` mean, and that `node_version_mismatch` means the operator and node builds differ (handled from M3 on).
4. Final `just lint` + `just e2e`.

**Done when:** every check above passes and the spec's S§0 exit criteria hold.

---

## Risks during execution

| Risk | Mitigation |
| --- | --- |
| `incus exec` stdin handling differs from SSH | `vm.ExecStdin` closes stdin after the request, like the M3 client will. M3 re-runs the same requests over SSH |
| Event buffering hides progress | Flush after each event line; T4 checks that `step started` for `stub-slow-v1` arrives before the step finishes |
| The old binary's `upgrade.install` is the upgrade path forever | Its params are frozen in S§4.4 now; M3's E27 runs it from the older build |
| `flock` semantics differ on `/run/lock` tmpfs | Linux `flock(2)` on tmpfs is fine; T2's VM check confirms it |
| Stub catalogue leaks into production | Build-tag split plus the `ERU_TEST` symbol check and `--check-steps` count in `just lint` |
