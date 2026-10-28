# Documentation

## Current references

- [MASTER.md](MASTER.md): decisions, architecture and current roadmap/status.
- [IMPLEMENTATION.md](IMPLEMENTATION.md): implementation details; `AGENTS.md` at the repository root defines working rules.
- [WIREGUARD-OVERVIEW.md](WIREGUARD-OVERVIEW.md): illustrated walkthrough of private networking, managed credential files and the encrypted handoff, grounded in the code.
- [MANUAL-TESTING.md](MANUAL-TESTING.md): qualification summary, coverage limits and chronological live evidence. Historical failures are retained, not current blockers unless explicitly listed as unresolved.
- [PLAN-v1-release.md](PLAN-v1-release.md): remaining release gates and authorized-release procedure.
- [RELEASE-v1.0.0-alpha.1.md](RELEASE-v1.0.0-alpha.1.md): draft release notes; no alpha has been published by this qualification.

## Design and implementation history

Keep these as rationale and implementation references, not as parallel status trackers:

- [RFC-cluster-per-database.md](RFC-cluster-per-database.md): accepted cluster-per-database model.
- [PHASE-3-WORK-UNITS.md](PHASE-3-WORK-UNITS.md), [PHASE-4-WORK-UNITS.md](PHASE-4-WORK-UNITS.md), [PHASE-5-WORK-UNITS.md](PHASE-5-WORK-UNITS.md): implementation units and original exit procedures.
- [PLAN-shared-tunnel.md](PLAN-shared-tunnel.md): shared operator tunnel design; current evidence is in the testing log.
- [RFC-major-upgrades.md](RFC-major-upgrades.md): deferred design, not an alpha compatibility promise or current implementation task.

## Archived work records

Preserved for provenance; their commands and pending items are historical, not instructions to execute:

- [Original v1 fixes handoff](archive/HANDOFF-v1-fixes.md).
- [v1 fixes review and resolution](archive/REVIEW-v1-fixes.md).
- [Executed alpha live-test procedure](archive/PLAN-v1-alpha-live-tests.md).
- [Superseded shared-cluster Phase 3 plan](archive/PHASE-3-PLAN.md).
