# Documentation

## Current references

- [MASTER.md](MASTER.md): decisions, architecture and current roadmap/status.
- [IMPLEMENTATION.md](IMPLEMENTATION.md): implementation details; `AGENTS.md` at the repository root defines working rules.
- [EXTENSION-DEFINITIONS.md](EXTENSION-DEFINITIONS.md): extension descriptor fields, resolution rules, custom provider examples, copy/recovery safety and command behavior.
- [MANUAL-TESTING.md](MANUAL-TESTING.md): qualification summary, coverage limits and chronological live evidence. Historical failures are retained, not current blockers unless explicitly listed as unresolved.
- [PLAN-v1-release.md](PLAN-v1-release.md): remaining release gates and authorized-release procedure.
- [RELEASE-v1.0.0-beta.1.md](RELEASE-v1.0.0-beta.1.md): draft beta notes; Phase 7 and the beta qualification are pending.
- [Extensions finishing checklist](../specs/extensions-finish.md): current evidence, final findings, preserved requirements and remaining acceptance gates. Replaces the older Phase 7 plans, handoffs, notes and status ledgers, which remain in Git history.
- [RUN-phase-6-7-live.md](RUN-phase-6-7-live.md): combined follower/extension qualification procedure; admission and existing live evidence must be reconciled before execution.

## Followers (Phase 6)

- `vaire db follow <db> [--name <new>] [--size <size>] [--allow-overcommit]` makes a read-only physical standby of one database on the same node, streaming through a slot capped by `max_slot_wal_keep_size`. It is read at the node's host with the follower's name in the path and the source's credentials; writes fail.
- `vaire db unfollow <follower> --confirm <follower>/unfollow` promotes it into an independent database with its own roles (new passwords, printed once), stanza and first full.
- A follower and an unfollowed copy do not inherit the source's protection; an unfollowed copy keeps only the protection the operator gave it (`vaire db protect`).
- Details are in [IMPLEMENTATION.md](IMPLEMENTATION.md); qualification cases are retained in the [combined run sheet](RUN-phase-6-7-live.md).

## Historical records

Older plans, handoffs, review ledgers and archived procedures have been removed
from the working tree. Their text remains in Git history; they are not current
instructions. Dated observations in `MANUAL-TESTING.md` remain historical
evidence, not authorization to rerun destructive procedures.
