# Review: the 17 v1-fix commits

> **Archived 2026-10-19: original review and resolution record.** The original verdict below applies to the reviewed commit range, not today's candidate. Current evidence: [manual testing](../MANUAL-TESTING.md); release status: [release checklist](../PLAN-v1-release.md).

Reviewed 2026-10-16: `2a05830` through `779c07f`, against `docs/HANDOFF-v1-fixes.md` and `AGENTS.md`. The initial working tree was clean. This is a source review, not completion of the handoff's deployment and live-test steps.

## Verdict

Most requested changes are present, but the work is not ready to approve. The protection fix does not reach existing installations through the normal upgrade/apply path, and several other integration cases are missing.

**Resolution:** all six code findings were addressed with regression tests in `9d3ae9e`. Subsequent live testing found additional administrative roles missing from query/top filtering; `eaa1a73` fixed those and passed live retesting. Protected-policy upgrades, factor selection and query/top behaviour have live evidence; release-pointer failure/ordering and malformed-hash cases have local regression-test evidence, not stable-publication evidence. Major-upgrade design observations remain deferred. The original findings and verdict are retained unchanged below.

## Findings

### P1: existing protected databases keep pg_maintain after upgrade

- Commit: `2a05830`.
- Locations: `internal/steps/postgres_roles.go:18`, `:65-73`, `:121-134`, `:364-365`; `internal/engine/runner.go:448-453`.
- The new check and revocation are correct in isolation, but the step ID and its serialized inputs are unchanged from the parent of the reviewed range. `postgresRoles` does not implement `Rechecker` either.
- For a completed roles step with unchanged configuration, the engine returns `ReasonJournal` without calling `Check` or `Apply`. Upgrading and running `eru apply --auto-upgrade-nodes` therefore leaves the old `pg_maintain` grant in place: the protected database still permits the reported table-lock denial of service.
- `docs/MANUAL-TESTING.md:428` incorrectly implies the normal apply will re-run that step.
- Required correction: make the access-policy change invalidate the roles step's inputs, version the step appropriately, or deliberately recheck it. Cover an upgrade with an old completed journal entry, not just generated SQL strings.

### P2: secrets export cannot use TOTP when a master password exists

- Commit: `840ebd8`.
- Locations: `internal/cli/secrets.go:31-41`; `internal/cli/secretsgate.go:67-83`.
- Export opens a node session only when the stored password hash is empty. When both factors exist, it passes nil session/progress to the gate, so the gate never reads protection status and always selects the password branch.
- A valid TOTP code cannot authorize export in this state, and the specified TOTP-first prompt with password fallback is unreachable. The wrong-password hint nevertheless promises that a code still works.
- Required correction: offer the node-verified factor when available while retaining an offline password path. Test export with both factors, TOTP only, password only, and an unreachable node.

### P2: root release promotion treats read failures as an empty channel

- Commit: `c90e2cc`.
- Locations: `.github/workflows/publish-stable.yml:93-101`; `internal/release/version.go:123-132`.
- `aws s3 cp ... | jq ... || true` swallows bucket read failures and invalid JSON. An empty result is passed to `Promotes`, where an empty channel accepts any valid candidate.
- If the root already points to `2.0.0`, a transient read failure while publishing `1.0.1` or a prerelease can replace the root with that older release, provided subsequent uploads succeed. This violates the highest-version requirement.
- Required correction: distinguish a positively identified missing object from authentication, network, service, parsing, and missing-version errors. Abort promotion on the latter. Also propagate promotion-tool errors instead of reporting a successful no-op.

### P2: the per-major release pointer can move backward

- Commit: `c90e2cc`.
- Locations: `.github/workflows/publish-stable.yml:74-83`; `scripts/upload-release.sh:40-50`.
- Only root promotion compares versions. Every tag unconditionally replaces its line's manifest, signature and installer.
- Publishing a previously unpublished `1.0.1` maintenance tag after `1.1.0` moves `/1.x/manifest.json` backward. Publishing `1.2.0-alpha.1` after a final release also makes line installations select that prerelease automatically. Fresh installs receive the wrong version, and existing clients may stop seeing updates because their upgrade planner rejects an older pointer.
- Required correction: separate immutable-version upload from channel promotion and define/enforce monotonic line promotion with an explicit prerelease policy. Test out-of-order publication.

### P2: query filtering loses application results, and top still shows internal statements

- Commit: `2a55793`.
- Locations: `internal/node/queries.go:33-40`, `:96-101`, `:134-138`; `internal/cli/topcmd.go:100-109`; `internal/cli/top/top.go:31-36`.
- `queries` fetches only the first `limit` rows, then removes eru roles in Go. For `--limit 1`, if the most expensive statement belongs to eru, the result is empty even when application statements exist. With the default limit, internal statements can similarly crowd out the application's actual top statements.
- `top` filters its activity/session list, but not its busiest-statements list. That SQL does not select a role at all, so eru's own polling and maintenance statements remain eligible there regardless of `--all-sessions`.
- Required correction: filter statement statistics by role before ordering/limiting, and apply the same inclusion policy to top's statement samples. Test internal statements ranking ahead of application statements.

### P2: a malformed master-password hash can crash verification

- Commit: `840ebd8`.
- Locations: `internal/password/password.go:59-75`; `internal/secretsdoc/context.go:109`.
- The verifier parses stored Argon2 parameters but does not reject zero rounds or bound memory, rounds, salt length and digest length before invoking `argon2.IDKey`. For example, `$argon2id$v=19$m=65536,t=0,p=4$c2FsdA$aGFzaA` reaches the library's explicit zero-round panic instead of returning false. Very large accepted memory/round counts can exhaust resources.
- The hash is also imported from transfer documents without parameter validation. This is input-validation hardening, not evidence that the normal generated hash fails.
- Required correction: validate the complete encoding and safe parameter/length bounds before deriving a key. Add zero-round, oversized-cost, malformed and truncated-hash tests.

## Documentation observations

- The major-upgrades RFC covers the requested subjects, but its format inventory incorrectly says the step journal has no version marker (`docs/RFC-major-upgrades.md:18`). `internal/engine/journal.go:17-30` already defines `JournalSchema = 1` and writes a `schema` field; its loader rejects other versions. The proposed pre-final checklist consequently asks to add something already present.
- The RFC's within-line promise says old readers tolerate additive fields, while current configuration parsing is deliberately strict, and its rollback discussion also allows refusing newer migrations. The intended minor-version backward-read/downgrade guarantee needs to be made consistent before this becomes an implementation contract.
- The existing handoff and pending-live-test entry honestly record that deployment and live re-tests have not happened. Passing local verification is not evidence for the idle-tunnel, fresh-init upload, protected maintenance/copy-write, interactive gate or macOS-install outcomes.

## Verification

Passed on the reviewed HEAD:

- `go build ./...`
- `go vet ./...`
- `go test ./...`
- `golangci-lint run ./...`
- `GOOS=darwin GOARCH=arm64 go build ./...`
- `just lint` (including metadata, step catalog, shellcheck and actionlint)
- An additional uncached `go test -count=1 ./...`

No code fixes, commits, pushes, tags, deployments or live mutations were performed. This file is the only review-authored source-tree change.
