# Evidence artifact index

The table indexes sanitized case reports and tools. The independent
`evidence-hygiene-v3.md` audit covers the local verification logs and other
repository evidence; this index does not extend permission to private raw
captures or operator credentials.

## Safe to commit

| Artifact | Contents | Why safe |
| --- | --- | --- |
| `findings-84f71bb.md` | Sanitized findings EFIN11, EFIN1, EFIN12, EFIN13 | Contains result messages, error codes, paths, counts and source locations only. Pattern audit found no key/credential values. |
| `results-84f71bb.md` | X1-X11 case results, commands and limits | Contains result summaries, package/version facts, SQL identifiers, counts and build hashes. Request secrets were never printed; no credential files. |
| `results-efin1-efin11-overlay.md` | Exact baseline+overlay identity and live cron unfollow/fork proof | Contains build/file SHA-256 values, job IDs, timestamps, result fields and log lines. No secrets or keys. |
| `extension-namespaces-readonly.md` | Read-only pg_extension/control/object-schema facts for EFIN8 | Contains catalog names, versions, schema names and ACL text only; no credentials or key material. |
| `results-final-candidate-v1-superseded.md` | V1 frozen-build diagnostics, EFIN16 live order failure, EFIN14/15 distinction, recovery bound | Sanitized case results, build/source hashes, commands, counts and error text; no request secrets or credential files. V1 is not final acceptance. |
| `extension-preload-order-readonly.md` | Exact recovered-guest `shared_preload_libraries` values and assignment lines | Contains only `SHOW`/`pg_settings` order facts and the exact managed/guard assignment lines; no auto.conf credentials or recovery settings. |
| `results-final-candidate-v3-blocked.md` | V3 build identity and host-side Firecracker/KVM blocker | Sanitized build hashes, host state and exact unblock command; no guest credentials or raw streams. |
| `results-final-candidate-v3-state-check.md` | Read-only V3 lab state check and pristine-baseline STOP | Directory metadata, host status and hashes only; no guest writes, credentials or raw streams. |
| `results-final-candidate-v3.md` | Integrated V3 Linux qualification results | Sanitized case outcomes, UTC commands/job IDs, build/source hashes, package/schema facts, limits and remaining fixtures; no secrets or raw streams. |
| `review-final-candidate.md` | Independent final Linux source verdicts and exact candidate identity | Source/test outcomes, manifest identity and qualification limits only. |
| `evidence-hygiene-v3.md` | Repository evidence safety audit | Filename/pattern findings and scope exclusions only; no secret values. |
| `node-rpc-case.sh` | Reusable direct node-RPC harness | No embedded credentials; derives client identity and recursively removes credential-bearing result fields while preserving non-secret restart tokens. |
| `MANIFEST.md` | This manifest | No secret values. |

## Withheld from the repository

These raw guest captures were **removed from the worktree** and kept only in
private lab state:

- `raw-guest-84f71bb.tgz`
- `raw-guest-efin-overlay.tgz`
- `raw-guest-efin13-retest.tgz`

Current private location: `/tmp/opencode/vaire-extension-evidence-raw/`.
Historical guest copies were disposable and are not assumed to survive a
clean-slate run. Host private captures are not
committed because they contain complete NDJSON/RPC and run/postgresql log
streams that were not provably sanitized at capture time.

## Audit method (no secret values read)

- Audited only the safe artifacts above, by filename and pattern count for:
  private-key headers, `password`, `secret`, `credential`, `token`,
  `id_ed25519`, `wg.key`, `.env`.
- All sensitive-keyword matches were generic field words or prose
  (`passwords_may_have_changed`, "restart token", "no credentials", "request
  secrets"). No key material, passwords or generated credentials were present.
  Some reports retain non-secret local account names, private-range lab
  addresses and workspace labels as operational context.
- No secret/key/credential file was opened to perform this audit. No values
  were printed or copied into the manifest.
- The raw `.tgz` files were moved out of the repository before this manifest
  was written; `specs/evidence/extensions/` now contains only text artifacts
  plus the other workers' logs.

## Sanitized capture method used for the committed reports

- `vaire node rpc` requests were built on the guest with `jq`; generated role
  passwords lived only in a mode-0600 temporary request file that was deleted
  immediately after the call. Values were never echoed.
- Results were filtered with `jq` to the single result envelope and then to
  explicit fields (extension, package, version, reason, counts, hashes, etc.).
  No request object, secret map, or raw event stream is in the committed text.
- Package/control/library facts came from `dpkg-query`, `ls` on the extension
  directories, and PostgreSQL catalog/settings queries.
- No SSH private keys, WireGuard keys, operator context files, tokens or
  environment files were copied into any committed artifact.
