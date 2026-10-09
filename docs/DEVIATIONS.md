# Deviations and implementer choices

The Phase 1 spec asks for a note here when an implementer chooses an option it leaves open, or departs from a SHOULD.

## Release domain

`ReleaseBaseURL` is `https://get.eru.sh`, the placeholder in §19.1. The constant lives in `internal/buildinfo.ReleaseBaseURL`. Change the constant and this note together if the production domain differs.

## top sampling

`eru top` reads PostgreSQL with pgx over the WireGuard tunnel. `top.sample` is the RPC used when the operator runs `--local` on the server.

## Flag-level destructiveness in CommandMeta

§5.4 lists the `CommandMeta` fields. eru adds `MutatingFlags` and `DestructiveFlags` (`mutating_flags`, `destructive_flags` in JSON). Some commands are destructive only with one flag: `queries --reset`, `db credentials --rotate`, `node upgrade --rollback`. Marking the whole command destructive would make an agent ask for approval before a plain `eru queries`. `--check-meta` requires every listed flag to exist, and requires `ConfirmToken` when `DestructiveFlags` is set.

## Not yet chosen

These are filled in when the milestone that needs them lands:

- Exporter versions, and whether `pgbackrest_exporter` is replaced by eru textfile metrics (M8).
- Bloat estimation query source, vendored with licence and attribution (M9).
- Catalog extensions dropped because no PostgreSQL 18 package exists (M9).
