# Read-only shared_preload_libraries order facts (V1 recovered guest)

Bound period: V1 frozen candidate after recovery, before V2. No config change,
restart, start override, install or source edit was made for this check. Read
with `SHOW`, `pg_settings`, and `grep` of only the exact
`shared_preload_libraries` assignment lines. Other `postgresql.auto.conf` lines
(including credentials/recovery settings) were not printed.

## Effective values and origins

| Cluster | `SHOW shared_preload_libraries` | `pg_settings.sourcefile:sourceline` |
| --- | --- | --- |
| finalsrc (5437) | `pg_stat_statements,auto_explain,pg_stat_kcache` | `/etc/postgresql/18/finalsrc/conf.d/30-vaire-extensions.conf:2` |
| finalfollower (5438) | `auto_explain,pg_stat_kcache,pg_stat_statements` | `/var/lib/postgresql/18/finalfollower/postgresql.auto.conf:5` |

## Exact assignments

Managed `30-vaire-extensions.conf`:

```text
finalsrc:      shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_stat_kcache'
finalfollower: shared_preload_libraries = 'pg_stat_statements,auto_explain,pg_stat_kcache'
```

Owned copy guard in the follower's `postgresql.auto.conf` (only this exact line
was read):

```text
finalfollower: shared_preload_libraries = 'auto_explain,pg_stat_kcache,pg_stat_statements'
```

`finalsrc` has no `shared_preload_libraries` assignment in its auto.conf.

## Conclusion

The managed renderer order is correct:
`pg_stat_statements, auto_explain, pg_stat_kcache`.

The follower's owned copy guard wins because `postgresql.auto.conf` is read
after `conf.d`, and **the guard order is lexically sorted**:
`auto_explain, pg_stat_kcache, pg_stat_statements`. `pg_stat_kcache` therefore
loads before `pg_stat_statements`; without an explicit
`pg_stat_statements.max` placeholder it exits with the native FATAL observed in
V1.

This is **EFIN16 (guard load order)**, distinct from **EFIN14 (source pending
SQL/readiness)**. The V1 report's EFIN7 failure attribution is corrected to
name EFIN16 as the observed startup-order root while retaining the original
recovery record. No workaround is claimed as a pass.
