# v1.0.0-alpha.1 — draft release notes

**Unreleased.** Prepared 2026-10-19 from qualification through `65c4f91` (`edge-65c4f9157253`). The final release commit, tag and publication checks remain to be approved and recorded.

## Scope

The first alpha covers Phases 0–5: a single hardened PostgreSQL 18 server, one cluster per database, operated through the vaire CLI over WireGuard and SSH.

- Remote and local bootstrap, resumable setup, TLS certificates and automatic unconfirmed-lockdown revert.
- Database sizes, tuning, resource limits, two PgBouncers and per-database lifecycle operations.
- SQL with ordered multi-statement results, interactive psql/usql, and TOTP-gated writes to protected databases.
- Per-database encrypted pgBackRest backups, continuous WAL, PITR, dump restore, forks and restore drills.
- Encrypted master-state bundles, rebuild with preserved identity, awaiting-recovery guards and database recovery followed by a full backup.
- Authenticated metrics, status/doctor, query/top filtering and maintenance.
- Human-only secrets reveals using a master password or a node-verified authenticator code. This gates commands; it does not encrypt local contexts at rest.

Followers, extension management, major upgrades, multi-node and HA are not part of this alpha.

## Qualification and fixes

Live evidence is recorded in [MANUAL-TESTING.md](MANUAL-TESTING.md), including genuine old-policy upgrade, protected maintenance and copy writes, fresh initialization, database-free doctor, disaster recovery, local init and journal-proven SSH self-revert.

Two regressions found during qualification were fixed, published through edge and retested:

- `eaa1a73`: hide administrative work from default queries/top output before limiting results; retain it with `--all-sessions`.
- `65c4f91`: install backup job service templates even with no active databases or schedules, so the first recovered database can start its required full backup.

The corrected recovery run completed without warnings, took its automatic full and passed independent backup verification. Local build/vet/tests/lint and Darwin cross-build passed. Real servers tested include Debian 13 and Ubuntu 26.04; the latest fresh-run evidence is Ubuntu 26.04.1 LTS. macOS live install/upgrade was excluded, not passed.

## Alpha limitations

- Persisted formats, secrets documents, bundles, JSON interfaces and step behaviour are not frozen. A later alpha may require rebuilding a server and restoring its databases; there is no stable compatibility promise yet.
- No exhaustive live fault-injection claim: forced stalled-handshake success and absent/non-answering-daemon branches lack live evidence; normal idle reuse and a full unreachable-peer timeout passed.
- Imported master-password hash equality passed; separate imported-context password authorization, independent terminal flag checks and definitive backup-setup replacement-handoff completion remain evidence gaps.
- Master-bundle verification can report `behind` after backup telemetry changes even when no durable change marked it dirty. Bundle cryptographic verification and recovery passed; exact telemetry freshness is not promised by that check.
- A clean fresh first-database backup passed without pgBackRest lock errors; the transient lock-contention branch was not deliberately induced.
- The last interrupted-init run proved automatic revert and supported resume. Its final DNS/TLS completion was not recorded; it does not replace the earlier completed bootstrap tests.
- Stable signing, immutable upload and root/major-line channel promotion require verification during the first authorized alpha publication; local promotion tests are not publication evidence.

## Installation and channels

Until publication, use the signed [edge installer](https://vaire.mbvlabs.com/edge/install.sh). After an authorized successful release, `/1.x/install.sh` installs the v1 line; binaries from that line follow `stable-1.x`. `master` remains edge. The root channel follows the highest eligible release.

Do not run release branch/tag or stable-install steps until publication is explicitly authorized and verified. See the [release checklist](PLAN-v1-release.md).
