# Final corrected Linux source review

Date: 2026-11-16. This records two independent reviewer verdicts reported to the
coordinating agent. It is source acceptance, not release or deployment approval.

## Exact source

- Base: `84f71bbaa1804fb9b7d581ca8a109192dc6375a9`, plus finishing corrections.
- Freeze: `2026-11-16T18:01:49Z`.
- Manifest: [final-source-manifest.sha256](final-source-manifest.sha256).
- Manifest SHA-256:
  `66ae70f96f88f0577dac3388b9da6f26add06a897ad047950e762ab8c27ff7ea`.
- Both reviewers verified all 687 entries before and after their checks.

## Independent verdicts

| Lane | Verdict | Observed verification |
| --- | --- | --- |
| A, architecture/reasoning reviewer | **ACCEPT whole-change Linux source** | Independent removal regressions and source diff checks passed; incorporates prior independent whole-change review of unchanged files |
| B, implementation reviewer | **ACCEPT whole-change Linux source** | Independent full uncached suite, removal regressions, build and vet passed; exact three-file correction delta verified |

The reviewers did not consult each other's current verdict before reporting.
Neither reviewer edited source or accessed the lab.

## Final correction and preserved closures

The previous candidate was blocked because later removal errors could report
`sql_dropped:true` when SQL was already absent. Both reviewers verified that
direct and follower removal now set the flag only after successful DROP
execution and preserve that actual state through config, restart, retry,
replay and verification errors. Positive and negative regressions cover both
executed-DROP and no-DROP outcomes.

The independent whole-change reviews also accepted:

- resolver/provider admission, prerequisites and explicit/implicit settings;
- namespace checks before grants and intent persistence;
- exact restart tokens, target locks and truthful partial startup outcomes;
- outgoing history → preparation → incoming history → save → runtime ordering;
- owned continuation, registration and durable retirement before routing;
- retained copy suppression, cron wire parsing and owned-session draining;
- desired preload order and retained provenance without lexical normalization;
- typed/unit-aware guard verification with strict missing-setting failures;
- repack admission and exact-report OID binding.

No privileged statistics probe or grant expansion was added. The earlier
auto-repack statistics finding was withdrawn after correcting fixture ownership.

## Verification and qualification boundaries

The dedicated [Linux verification summary](99-V3-FINAL-SUMMARY.log) passed on
this exact source. Strict lint remains red at 142 findings versus 131 baseline;
macOS is deferred. Publication is not green.

The separately executed [clean-slate lab run](results-final-candidate-v3.md)
records native PostgreSQL/systemd and CLI/WireGuard results. Protected/TOTP,
deterministic crash boundaries, drill, bucket/rebuild, other platforms,
forced OID races and legacy order-only guard restart-token qualification
remain partial or blocked. No reviewer source verdict claims those cases.
