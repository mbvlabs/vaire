# v1.0.0-beta.1 — draft release notes

**Unreleased and not authorized.** Drafted 2026-10-29 from the Phase 6 work; updated for the Phase 7 local source checkpoint. Phase 7 has not passed its two whole-change reviews or fresh final checks, and the beta has not been published, deployed or live-qualified. These notes are incomplete and the tag is not authorized.

## Scope

`v1.0.0-beta.1` is the first beta of the v1 line: Phases 0–7 plus local activity history, merged into `1.x` from `master`. The published alphas were Phases 0–5.

- Followers (Phase 6): read-only physical standbys of one database, on the same node, with `db follow`, `db unfollow` and recovery after a rebuild.
- Local activity history and portable incident evidence.
- Extensions (Phase 7, local stage-reviewed through E7-06; final acceptance pending): a built-in convenience catalogue plus explicit APT or operator-preinstalled custom definitions; `vaire ext list|add|remove`; protected-policy-aware addition and refusal of protected removal; source/follower propagation with exact per-cluster restart tokens; retained stanza-bound historical requirements and presence-aware recovery declarations; suppression and deactivation of copied `pg_cron` jobs through independent-copy activation; detachable `maint repack`, opt-in auto-repack and a one-table full-scan `pgstattuple` report. No automatic repository/key setup, source build or package upgrade; native extensions are trusted code, not a sandbox.
- Extensions are **not live-qualified** in these notes. No completion or compatibility claim is made from unit, model or synthetic-bundle evidence.

## Upgrading from an alpha

- The first `vaire apply` after the upgrade leaves every database's `postgres-config-v1` in `restart_pending`: Phase 6 changes `wal_level` to `replica`. Nothing restarts by itself. Run `vaire apply --confirm <db>/restart` for each database, or `--confirm <server>/restart` for all of them, one at a time.
- Until a database restarts it keeps `wal_level = logical`, which physical followers still use; `SHOW wal_level` shows `replica` only afterwards.
- `max_slot_wal_keep_size` arrives in the same apply and is a reload: it caps every replication slot at 10% of the cluster's disk share (2–64 GiB).
- The upgrade needs no rebuild and no fresh `server init`.
- A follower and an unfollowed copy do not inherit the source's protection; protect the copy with `vaire db protect` if it needs it.
- Extension APT packages are explicit prerequisites. A configured trusted repository, or the preinstalled files, must exist on the host; vaire does not add extension-specific repositories or keys. An extension whose copy/standby safety is unknown is still valid on a standalone primary but blocks a copy or follower until declared. An existing backup taken before vaire recorded extension coverage needs an explicit `recovery_extensions` declaration for recovery; absent, explicitly empty and populated declarations mean different things.

## Qualification and platform limits

Pending. Live evidence goes in [MANUAL-TESTING.md](MANUAL-TESTING.md); the release procedure is [PLAN-v1-release.md](PLAN-v1-release.md). Do not treat this draft as a released document.

- No Phase 7 live result exists yet. Repack and the exact report have known runtime limits: protected databases refuse repack; only ordinary permanent heap tables with a key and `pg_default` storage are supported; completion may be reported `unknown`; the exact report is one full-scan table under a read lock and does not prove reclaimable space.
- Package names have metadata evidence across the supported OS/architecture matrix, not installation or runtime evidence. A single Ubuntu amd64 VPS does not establish Debian 13 or arm64 installs.
- macOS live operation is not qualified; only the Darwin cross-build is local evidence. The supported server platforms remain Debian 13 and Ubuntu 26.04.
- Activity qualification or explicit acceptance of its named gaps still gates deployment.
