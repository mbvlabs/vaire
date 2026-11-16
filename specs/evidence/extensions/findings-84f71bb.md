# Extension lab findings at 84f71bb (pre-fix candidate)

Candidate: `bin/vaire-extension-lab`, built from worktree
`/home/mbv/work/vaire-phase-7` at `84f71bbaa180`, `dev`/`dev`/`dev`.
Binary SHA-256: `7f5fab783bd314ce09cec55a7d27fd65e7226634c1a4ed3de6f91eca5ce40ddc`.
Guest: Debian 13.7, 4 vCPU, 4 GiB, 20 GiB; no production state. All requests
below are direct `vaire node rpc` requests executed as root on the lab guest.
The CLI transport was not used: public SSH is closed on eth0, and the existing
lab workspace keys do not match the guest's current operators. Direct node RPC
is the production dispatcher and job path; systemd jobs still execute
`/usr/local/bin/vaire`, so the fixed overlay below was installed there for the
job runs.

## EFIN11 (parent label; earlier called EFIN4 in this file): pg_cron settlement always reports no cron.job

`cronJobTable` compared the CSV boolean to `"true"`:

```go
rows, err := local.Query(ctx, metadataDB, "SELECT to_regclass('cron.job') IS NOT NULL AS present", nil)
...
if len(rows) == 0 || rows[0]["present"] != "true" {
    return false, cronUnknown("the copy's cron metadata database " + metadataDB + " has no cron.job table")
}
```

`psql --csv` returns `t`/`f` for a boolean. On the guest, with the table
present:

```
present
t
```

Every `deactivateCopiedCronJobsAt` therefore failed before it could deactivate
or retarget a copied job. Observed on a real pg_cron follow:

```
db.follow extcron -> extcronfol: succeeded (base_backup, streaming)
db.unfollow extcronfol --confirm extcronfol/unfollow:
  rc=11
  code=precondition_failed
  reason=cron_metadata_unknown
  message=the copy's cron metadata database extcron has no cron.job table
```

Source: `internal/node/extensionactivation.go` (`cronJobTable`). The parent
fixed this to `(to_regclass('cron.job') IS NOT NULL)::text AS present` plus an
exact one-row check; see the fixed-overlay result file.

## EFIN1: unfollow rename had no connection gate or drain

With a real client session held open on the promoted follower's source-named
database (`extsrc` on port 5439), `db.unfollow extfol` reached the rename and
failed:

```
psql:<stdin>:1: ERROR:  database "extsrc" is being accessed by other users
DETAIL:  There is 1 other session using the database.
job: db-unfollow
passwords_may_have_changed: true
```

A later retry after terminating the held session completed. This is the same
`renameFollower` path that pg_cron launches hold open. The parent fix gates
`datallowconn`, captures the database OID, drains only that OID's sessions and
compensates the flag; the fixed overlay completed a real cron-bearing unfollow
by terminating the live `pg_cron launcher` backend (PID 51622) at 15:23:46 and
renaming successfully.

## EFIN12 (assigned by parent; earlier called LAB-A, major): failed extension prerequisites pollute requirement history

A failed `ext.add` recorded the proposed descriptor in
`/var/lib/vaire/records.json` even when the package/file prerequisite failed and
`cluster.toml` was restored. Two deliberately failing X3 attempts on `extlab`
left conflicting requirements:

```
hypopg (preinstalled, since 14:43:02)
hypopg (apt package postgresql-18-hypopg-nonexistent, since 14:43:20)
```

Both requests returned `package_install_failed` with
`config_rollback=restored`, `intent_saved=false`, and no SQL/package effect.
The next `db.follow extlab --name extfollower` then failed with:

```
extension_provider_conflict
extension requirements conflict: hypopg (preinstalled) and hypopg (apt package
postgresql-18-hypopg-nonexistent preload )
```

Source suspicion: `handleExtAdd` calls `extensionCaptureRequirements` before
`extensionPrepareStart`/package convergence, but the package-failure rollback
only restores `cluster.toml`, not the requirement snapshot. A failed add should
not leave an incoming provider that blocks unrelated copy/follow work.

The polluted `extlab` requirement history is intentionally retained as the
evidence fixture and was not edited or deleted. Unrelated follow/fork/unfollow
cases used fresh fixtures (`extcron`/`extcronfol`/`extcroncopy`) instead.

## EFIN13 (assigned by parent; earlier called LAB-B): NOT reproduced for ordinary app-owned tables

The first auto-repack failure was real, but the fixture was misleading: the
tables (`ext_autorepack`, `ext_autorepack2`) were created and owned by
`postgres`, not by the database's app owner. `pg_stats` only exposes a table's
statistics to its owner or a role with column `SELECT`, so those postgres-owned
tables were invisible to `vaire_admin`:

```
has_table_privilege('vaire_admin','ext_autorepack','SELECT') = f
select count(*) from pg_stats where tablename='ext_autorepack'  -- as vaire_admin
  0
```

A diagnostic lab-only `GRANT SELECT ON ext_autorepack2 TO vaire_admin` made the
same table visible and selected; that was a **fixture diagnostic, not a
production workaround or a pass**.

Retest with a fresh table owned by the database's app owner and **no explicit
grant**:

```sql
CREATE TABLE ext_final_auto (...);
-- insert 10,000 rows, delete 9,500, then
ALTER TABLE ext_final_auto OWNER TO extlab_owner;
ANALYZE ext_final_auto;
```

`vaire_admin` is a member of `extlab_owner`, so at 15:50:49Z `maint-weekly`
selected and repacked it without any explicit table grant:

```
public.ext_final_auto  83140608  77987840  0.938
counts: selected=1 repacked=0 unknown=1 skipped=0
```

Conclusion: **EFIN13 is not reproduced for ordinary app-owned tables.** The
diagnostic grant was revoked with `REVOKE SELECT ... FROM vaire_admin` before
this retest, and the fresh app-owned table needed no grant. The finding is
narrowed to tables owned by roles outside the app-owner membership (for example
the postgres-owned lab fixtures), which is not the ordinary vaire application
table shape. The earlier "auto-repack can never select" conclusion is withdrawn.

Source: `internal/maint/bloat.go`/`table_bloat.sql`, `pg_stats` visibility and the
`vaire_admin` role membership. This is separate from EFIN6 (global setting
canonicalisation). Whether vaire should estimate tables outside the app-owner
membership is a product/source question; it is not an ordinary app-table
blocker, and no production grant workaround is claimed.
