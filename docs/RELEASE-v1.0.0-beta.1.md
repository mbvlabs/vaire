# v1.0.0-beta.1 — draft release notes

**Unreleased.** Drafted 2026-10-29 from the Phase 6 work. Phase 7 and the beta qualification have not run, so these notes are incomplete and the tag is not authorized.

## Scope

`v1.0.0-beta.1` is the first beta of the v1 line: Phases 0–7 plus local activity history, merged into `1.x` from `master`. The published alphas were Phases 0–5.

- Followers (Phase 6): read-only physical standbys of one database, on the same node, with `db follow`, `db unfollow` and recovery after a rebuild.
- Local activity history and portable incident evidence.
- Extensions (Phase 7) are not built yet and are not described here until they are.

## Upgrading from an alpha

- The first `vaire apply` after the upgrade leaves every database's `postgres-config-v1` in `restart_pending`: Phase 6 changes `wal_level` to `replica`. Nothing restarts by itself. Run `vaire apply --confirm <db>/restart` for each database, or `--confirm <server>/restart` for all of them, one at a time.
- Until a database restarts it keeps `wal_level = logical`, which physical followers still use; `SHOW wal_level` shows `replica` only afterwards.
- `max_slot_wal_keep_size` arrives in the same apply and is a reload: it caps every replication slot at 10% of the cluster's disk share (2–64 GiB).
- The upgrade needs no rebuild and no fresh `server init`.
- A follower and an unfollowed copy do not inherit the source's protection; protect the copy with `vaire db protect` if it needs it.

## Qualification

Pending. Live evidence goes in [MANUAL-TESTING.md](MANUAL-TESTING.md); the release procedure is [PLAN-v1-release.md](PLAN-v1-release.md). Do not treat this draft as a released document.
