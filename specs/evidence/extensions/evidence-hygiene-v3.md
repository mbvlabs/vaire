# Evidence hygiene audit — V3 local artifacts

Date: 2026-11-16
Audit snapshots: base 2026-11-16T18:10Z; addendum 2026-11-16T18:55Z.
Later edits to shared/live-owned artifacts are outside this snapshot.
Scope: read-only hygiene audit of `specs/evidence/extensions/` and the new
untracked spec `specs/extensions-finish.md`, ahead of the final PR.
No production, test, build or source edits, and no edits to existing
docs/evidence. No commits, deletes or moves.

## Method

- Filename/suffix inventory and text/encoding check for every artifact.
- Targeted pattern counts only; no broad secret scan and no credential, env or
  key files opened.
- Keyword hits were reviewed with minimal non-secret context. Values were not
  printed, copied or included in this report.
- Source manifests were validated structurally (line shape, relative paths,
  dotfiles, sensitive suffixes), not by reading listed source contents.
- `specs/extensions-finish.md` was updated by other workers during the audit
  (18:08:39Z and 18:21:51Z). Its current content was re-audited with the same
  pattern checks before this addendum was finalized.
- Addendum covered the new/updated V3 lab and review reports and the
  `MANIFEST.md` entries for the lab reports: integrated qualification,
  historical blocker/ownership correction, read-only state check, and final
  source review.

## Result

No hygiene blocker found. No raw archives, binaries, private keys, connection
URLs/DSNs, database credential output or request-secret values were found in the
audited artifacts. The only potential infrastructure redaction item is one
private-range lab address in the spec; it is not credential material. The new
V3 lab and review reports contain no credential, URL, handoff-value or
raw-stream secrets. Local paths/usernames are noted separately as optional.

## Allowlist

85 files in `specs/evidence/extensions/` are now allowlisted (the original 81
plus four V3 lab/review reports), plus `specs/extensions-finish.md` = 86 audited
artifacts. The first 82 artifacts were accepted previously; this addendum covers
the delta.

| Group | Count | Contents | Result |
| --- | --- | --- | --- |
| `00-SUMMARY.log`, `01-*`…`16-*` | 17 | Early local verification logs (build/vet/lint/format/diff/tool/pins) | Safe |
| `99-*FINAL-SUMMARY.log` | 3 | V1/V2/V3 gate summaries | Safe |
| `FINAL-20261116T1640Z-*`, `FINAL-20261116T1648Z-*` | 18 | V1 final gate logs and manifest checks | Safe |
| `V2-20261116T1746Z-*` | 16 | V2 local gate logs | Safe |
| `V3-20261116T1802Z-*` | 16 | V3 local gate logs and focused regression log | Safe |
| Original report set (findings, X-case, overlay, V1, two read-only) | 6 | Sanitized findings, case results, read-only catalog facts | Safe |
| `results-final-candidate-v3.md`, `-blocked.md`, `-state-check.md`, `review-final-candidate.md` | 4 | Integrated V3 results, historical blocker/ownership correction, read-only state check, final source review | Safe; non-secret context notes below |
| `*.sha256` | 3 | V1/V2/V3 complete source manifests | Safe |
| `node-rpc-case.sh` | 1 | Lab RPC harness; no embedded credentials | Safe |
| `MANIFEST.md` | 1 | Updated sanitized artifact index (new V3 entries audited) | Safe |
| `specs/extensions-finish.md` | 1 | Finishing tracker/spec | Safe except address item below |

## Pattern audit (no values)

Base-scope line counts (original 82 artifacts; may overlap); all hits were
generic prose, source/package identifiers, test names, or non-secret
boilerplate. Addendum reports are dispositioned individually below.

| Pattern area | Hits | Disposition |
| --- | --- | --- |
| Archive suffixes (`.tgz`, `.tar`, `.gz`, `.zip`, etc.) | 0 | No raw captures |
| Binary/non-text or invalid UTF-8 files | 0 | All artifacts are text |
| Private-key headers (`-----BEGIN`, `PRIVATE KEY`) | 0/1 prose | Only a manifest sentence saying no keys were copied |
| `postgres://`, `postgresql://`, connection strings | 0 | No connection URLs; `DSN` appears only as a redaction key name/prose |
| `PGPASSWORD`, `password=`, `passwd=`, `sslmode=` | 0 | No credential assignments |
| `Authorization:`, `Bearer`, API/access/client secret tokens | 0 | No auth material |
| `password` / `credential` / `secret` / `token` words | 31 / 56 / 159 / 29 | Package and file names, test names, restart-token prose, generic result fields |
| `.env`, `id_ed25519`, `wg.key` | 2 / 1 / 1 | Manifest audit-method prose only; one `.env` is a substring of `.envelope` in the harness |
| JWT-like (`eyJ…`) or long non-hex base64 blobs | 0 | No encoded secrets observed |
| 64-hex values | 2129 | Checksums and manifest digests only |

Notable benign hits checked individually:

- `findings-84f71bb.md`: `passwords_may_have_changed: true` is a generic result
  boolean, not a password value.
- Lint logs: one `ConfirmToken:` source snippet shows the literal template
  `{db}/restart`; it is a confirmation-token format, not a secret.
- Lint logs: one gosec G101 message is a finding label; the referenced source
  line is a CLI help string, not a hardcoded credential.
- `extension-preload-order-readonly.md`: explicitly states that non-preload
  auto.conf lines, including credentials/recovery settings, were not read.
- `results-final-candidate-v3.md`: `token`/`credential`/`handoff` hits are
  prose (no-token refusals, "no other auto.conf lines or credentials were
  printed", and init/handoff output removal). Job IDs, DB/cluster names and
  build/source SHA-256 values are non-secret. Cleanup confirms the source cron
  job was unscheduled, no `pg_sleep` backend remained, and no own HTTP server
  remains. No raw NDJSON/RPC or run logs.
- `results-final-candidate-v3-blocked.md`: raw shell output is limited to host
  network/KVM/sudo status and stopped PIDs; no guest credentials or stream
  content. Historical ownership/inference correction is narrative only.
- `results-final-candidate-v3-state-check.md`: read-only `debugfs` directory
  metadata, host status and known hashes; no disk content, credentials or raw
  streams.
- `review-final-candidate.md`: two source-review verdicts and bounded closure
  statements; `token` hits are restart-token prose. No credentials, URLs or raw
  lab output.
- `MANIFEST.md`: the new V3 entries are descriptions only, with no embedded
  values.

## Source manifests

- `final-source-manifest.sha256`: 687 entries.
- `source-manifest-v2-superseded.sha256`: 686 entries.
- `source-manifest-v1-superseded.sha256`: 680 entries.
- Every line matches `64-hex  relative/path`; no absolute paths, `..`, `.git/`,
  evidence, docs or temporary paths.
- Only dotfiles are the three expected `.github/workflows/*.yml` files and
  `.golangci.yml`.
- No `.env`, key, certificate, archive or dump file type is listed. Paths whose
  names contain `secret`/`credential` are ordinary Go source and are expected.
  The two `.sql` entries are ordinary repository source under `internal/maint/sql/`.

## Exclusions / not audited

- The four V3 lab/review reports are no longer excluded; the addendum above
  covers them, and the three lab reports have new `MANIFEST.md` entries.
  `MANIFEST.md` entries for `review-final-candidate.md` and this audit summary
  are parent-owned and were not present at this snapshot; their absence is not
  treated as a hygiene gap.
- Private raw capture state outside the repository, the lab guest, `~/work/vps-lab`,
  `~/work/eregion`, and any credential/env/key file or production service: not
  accessed, by instruction.
- Modified tracked files under `docs/`: outside this assignment's new-untracked
  spec/evidence scope and not audited here.

## Pre-commit review items (non-secret)

1. `specs/extensions-finish.md`, section "Live evidence and important factual
   corrections", contains one RFC1918 lab-guest IPv4 address. It is not a
   credential or connection URL, but is infrastructure detail. If lab addressing
   is not intended for the public PR, replace it with a placeholder before commit.
2. The historical blocker report's "Actual host state"/"Unblock command" blocks
   and the state-check "Host state" block contain one private-range host address
   and a local account name. These are not credentials or URLs. Optional
   redaction if lab infrastructure/identity must not be published.
3. `MANIFEST.md`'s audit-method sentence says no operator-context values were
   present. That is now overbroad for the newly indexed lab-state reports, which
   include non-secret lab host/account context. Recommend narrowing the wording
   to credential/key values, or explicitly accepting the non-secret context.
4. The spec and several summaries/preflight logs contain local absolute worktree
   paths; the spec also contains internal agent session IDs and one build log
   contains a local file-owner name. These are operator context, not credential
   material. Optional scrub only if the PR policy treats local paths, session
   IDs or identities as sensitive.
5. `specs/extensions-finish.md` still records the earlier 82-artifact audit and
   says the V3 blocker report/MANIFEST entry are excluded. This addendum
   (86 audited artifacts) supersedes that. Update the tracker text if the spec
   is committed.

## Verification actions

- Inspection was read-only.
- One new file written: this audit summary.
- No existing artifact edited, deleted or moved.
- No commits.
