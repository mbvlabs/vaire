# Read-only extension namespace facts

Bounded READ-ONLY lab check while waiting for the final candidate freeze. No
install, grant, revoke, config change or source change was made. Queries ran as
`postgres` over the local socket against the existing lab clusters.

## `pg_extension.extnamespace`

Query used:

```sql
SELECT e.extname, n.nspname
FROM pg_extension e
LEFT JOIN pg_namespace n ON n.oid = e.extnamespace
WHERE e.extname IN
  ('pg_cron','pg_repack','pg_stat_statements','pg_stat_kcache','pgstattuple','pg_partman')
ORDER BY e.extname;
```

| Cluster (port) | Extension | `extnamespace` |
| --- | --- | --- |
| app (5433) | pg_stat_statements | public |
| other (5434) | pg_stat_statements | public |
| extlab (5435) | pg_repack | public |
| extlab (5435) | pg_stat_kcache | public |
| extlab (5435) | pg_stat_statements | public |
| extlab (5435) | pgstattuple | public |
| extcron (5436) | pg_cron | **pg_catalog** |
| extcron (5436) | pg_stat_statements | public |
| extcronfol (5437) | pg_cron | **pg_catalog** |
| extcronfol (5437) | pg_stat_statements | public |
| extcroncopy (5438) | pg_cron | **pg_catalog** |
| extcroncopy (5438) | pg_stat_statements | public |

`pg_partman` is not installed in any of these clusters, and its control file is
absent. No installation was attempted.

## Control-file keys (non-secret)

```text
pg_cron              default_version=1.6     relocatable=false  schema=pg_catalog
pg_repack            default_version=1.5.3   relocatable=false  schema=(absent)
pg_stat_statements   default_version=1.12    relocatable=true   schema=(absent)
pg_stat_kcache       default_version=2.3.2   relocatable=true   schema=(absent)
pgstattuple          default_version=1.5     relocatable=true   schema=(absent)
pg_partman           control file absent
```

## Actual extension-owned object schemas

Counts from `pg_depend ... deptype='e'`, grouped by extension and schema:

```text
extlab (5435)
  pg_repack           classes=2   procs=26   schema=repack
  pg_stat_kcache      classes=2   procs=2    schema=public
  pg_stat_statements  classes=2   procs=3    schema=public
  pgstattuple                     procs=9    schema=public

extcron (5436)
  pg_cron             classes=4   procs=7    schema=cron
  pg_stat_statements  classes=2   procs=3    schema=public
```

Relevant contrasts for the EFIN8 refinement:

- **pg_cron**: `extnamespace = pg_catalog` (control `schema = pg_catalog`), but
  every owned class/procedure is in the `cron` schema (`cron.job`,
  `cron.schedule`, ...). A descriptor schema check cannot equate
  `extnamespace` with the application-visible schema, and the default-grant
  target is `cron`, not `pg_catalog`.
- **pg_repack**: `extnamespace = public` and the control file has no `schema`
  key, but its owned objects are in the **`repack`** schema. Assuming `public`
  from `extnamespace` is wrong for schema validation and grants.
- **pg_stat_statements / pg_stat_kcache / pgstattuple**: `extnamespace =
  public`; their owned objects observed in `public`; all three control files
  are relocatable.
- **pg_partman**: not installed and no control file, so no live namespace fact
  exists; do not infer `public` from absence.

Observed `pg_cron` ACLs in extcron (not changed):

```text
cron schema: {postgres=UC/postgres,extcron_owner=U/postgres}
cron.job:    {postgres=arwdDxtm/postgres,=r/postgres}
```

So the metadata schema is `cron`, while `extnamespace` is `pg_catalog`; any
descriptor-schema validation or default-grant logic must use the actual object
schema and control facts, not `extnamespace` alone.
