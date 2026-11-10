# Follower lifecycle: two additional safety corrections

## Approval requested

You authorized both outcomes in chat. This review confirms their behavioral
scope before implementation. These additions are separate from the nine
already authorized final-review fixes.

## Safe retry after an interrupted ordinary follow

An interrupted initial follow can currently leave a partly built follower
without an admitted public retry. Extend durable operation ownership and public
retry to ordinary follow, not just rebuild recovery.

- Repeating follow for the same unfinished, owned operation resumes safely.
- Reuse a completed copy only when its retained slot is valid; otherwise recopy
  only artifacts proven to belong to that operation.
- Refuse foreign paths, changed operation identity and completed followers.
- Preserve progress across interruption without storing credentials in history.

This fulfills the existing progress guarantee. It does not add refresh-in-place,
adoption of arbitrary existing clusters or a repair command. The affected areas
are lifecycle admission, durable ownership, copy/retry and job cleanup.

## Refuse unsafe new followers after a deferred source shrink

After a source shrink, desired standby floors can be below the source's still
effective requirements if its restart was deferred. Before admitting a new
follow, compare the proposed floors against the source's effective requirements.

- Refuse only a demonstrated unsafe mismatch, with a clear explanation that
  the source's explicit restart is required before retrying.
- Do not block an unrelated pending restart merely because one exists.
- If the necessary safety facts cannot be established, refuse admission rather
  than inventing safe values.
- Never restart automatically, reuse a confirmation for another target, or wait
  for follower data catch-up.

This keeps the configuration-derived floor model instead of adding temporary
runtime-derived configuration that later apply could undo. The tradeoff is
delaying new-follow admission until the operator explicitly completes the
necessary source restart. Existing follower resize policy stays unchanged.

## Verification and boundaries

Add regressions for interrupted public retry, ownership/foreign-path refusal,
retained-slot reuse and recopy, and unsafe/safe/unknown admission facts. Both
independent reviewers reassess the lifecycle guarantees after implementation;
the full local verification chain must pass.

No new production fault hooks, local VM/container harnesses or healing routines.
Live crash, slot and PostgreSQL startup qualification remain explicitly pending.
This approval authorizes scoped code/tests and documentation only. It does not
authorize commits, branches/tags, publication, deployment, live operations or
resource deletion.
