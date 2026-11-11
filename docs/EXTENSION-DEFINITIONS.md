# Extension definitions

`internal/catalog` carries the built-in PostgreSQL extension descriptors. The
built-in list is a convenience, not an allowlist: a database may select any
PostgreSQL 18 extension through an explicit `[extensions.<name>]` definition,
provided its package is in an already configured trusted APT repository or its
files are already installed by the operator.

All names in `[[clusters]].extensions` and `[[clusters]].recovery_extensions`
are canonical. CLI aliases (for example `pgvector` for `vector`) are accepted
only at the command boundary and are written to `cluster.toml` in canonical
form. A canonical key may contain internal hyphens (`[extensions.uuid-ossp]`);
leading/trailing hyphens and `--` are refused. `ext_name` may be any safe
PostgreSQL extension name, including mixed case or punctuation such as
`uuid-ossp` or `Mixed.Ext!`; it is quoted in SQL. Path separators, `..`,
control characters, `--`, leading/trailing hyphens and names over 63 bytes are
refused.

## Descriptor fields

```toml
[extensions.<name>]
ext_name      = "..."        # CREATE EXTENSION name; defaults to <name>
source        = "apt"        # apt | preinstalled; required for a new entry
package       = "..."        # required for apt; absent for preinstalled
preload       = "..."        # optional shared_preload_libraries value
settings      = { ... }      # optional; {db} is the database inside the cluster
schema        = "..."        # optional schema created before the extension
note          = "..."        # human documentation only
copy_safety   = "unknown"    # unknown | safe | disabled
copy_settings = { ... }      # required when copy_safety is disabled
standby_safe  = false        # explicit; false or absent blocks follow
```

- `copy_safety = "unknown"` is the default. A copy, drill, fork or follower
  that needs the extension must stop with an actionable prerequisite until the
  operator declares `safe`, or `disabled` with settings that suppress the
  extension's background effects.
- `standby_safe` describes trusted native code; it is not a security sandbox or
  a proof of compatibility. A new definition defaults to false.
- Descriptors cannot carry SQL hooks, grants, shell commands, Git URLs,
  scripts, repository credentials or keys. Unknown descriptor keys are
  rejected by strict TOML decoding.
- Package names, library names, schemas, setting keys and setting values are
  validated. A setting value is a single-quoted string literal, a bare
  identifier or a signed decimal integer. Floats, units, comma lists and
  punctuation must be quoted; comments, semicolons, backslashes, control
  characters, malformed numbers and unterminated quotes are refused.
  Descriptors cannot inject free-form configuration fragments.
- A descriptor may not override vaire-managed settings such as
  `shared_preload_libraries`, `listen_addresses`, the `20-vaire-security.conf`
  logging/TLS policy, the three rendered auto_explain settings
  (`auto_explain.log_min_duration`, `auto_explain.sample_rate`,
  `auto_explain.log_analyze`), `ssl*`, `archive_*` or `hot_standby`.
- Two extensions in one database may not assign the same setting different
  effective values (including `{db}` substitution); equal values are allowed.
  A descriptor's own copy settings may suppress its own normal value, but copy
  requirements from different descriptors must agree. `copy_settings` is only
  valid with `copy_safety = "disabled"`.
- Host provider validation includes the implicit default modules. An explicit
  definition that targets the same SQL extension through a different package
  or source conflicts with a default provider even when the default is not
  listed in `extensions`. Definitions that target the same SQL extension must
  also agree on a nonempty preload library and on the normalized value of every
  setting key they both state; definitions that load the same preload library
  must agree on their provider and on overlapping settings. An omitted preload
  or setting is no requirement, not a contradiction, and `{db}` remains a
  per-database placeholder, so identical placeholder requirements agree across
  databases. A matching provider is compatible; the explicit `CREATE
  EXTENSION` selection itself is unchanged.
- A built-in key may be overridden explicitly, for example to select an
  alternate package. A changed source, package or preload requires fresh
  `copy_safety` and `standby_safe` declarations; the built-in safety
  certification is not inherited. The default modules `auto_explain` and
  `pg_stat_statements` keep their vaire-managed preload behavior. Their
  implicit preload selection also contributes any selected APT package, even
  when no database names them; the ordinary preinstalled baseline has an empty
  package union.
- A descriptor edit that changes an enabled database's or recovery
  declaration's semantic intent is reported by `config set` as
  `extension_intent_changed`, and the roles step input carries the changed
  identity. Note-only and unused-definition edits do not churn the plan.

## Installation and first start

The effective selections, including implicit default modules, present
`recovery_extensions` declarations and databases awaiting recovery, determine
the server-wide APT package union. `postgres-packages-v1` installs and holds
that union; preinstalled entries are verified only and never installed. After
installation vaire checks the real PostgreSQL 18 control file, the control
file's declared module library and any declared preload library, rather than
inferring availability from the package name. An empty APT union leaves the
package step's input hash unchanged.

Before a materialised restore, fork, drill or follower copy starts, the same
package/file prerequisites are installed and verified. A missing repository,
package or preinstalled file fails before the copy starts and never uninstalls
packages already installed. These prestart extension-only installs do not add
repository or key configuration; the existing PGDG setup for the base
PostgreSQL packages is unchanged. The selected config, cluster and roles
steps install and verify the target database's current extension prerequisites
before their first generated write, start, SQL or password file. A confirmed
restart does the same for its named target before PAUSE or the restart command.
Direct restore promote/undo, unfollow/retry and copy starts run the same
target-scoped preflight before their first durable record update, pause, stop,
swap or start. These guards add no server steps and restart nothing themselves.

A target preflight resolves every effective name that has a SQL control file or
a preload library, including the implicit preload-only `auto_explain`. An APT
provider override of a default module therefore installs and holds its package
even when no database names it, and a missing preinstalled `auto_explain.so`
stops the target before any write or restart. SQL creation is a separate
selection: only entries with a `CREATE EXTENSION` name are created, so
`auto_explain` is never created.

A copy's final `shared_preload_libraries` union is composed from current intent,
retained history and the operator's own `postgresql.conf`/`conf.d`/plain
`postgresql.auto.conf` assignment, never from a previous vaire guard block. The
complete union is validated before the copy first starts: every library must
match a definition with explicit copy/standby safety and present package,
control and library files. An unknown, ambiguous or contradictory library
refuses before PostgreSQL loads it; a prior guard entry is not independent
evidence, and a deliberately removed operator library stays removed while a
retained requirement remains. The guard is appended after the operator's plain
assignment, so the guard wins while present and the underlying assignment
applies again when it is deliberately released.

A descriptor's own mandatory `copy_settings` overlay its own ordinary
`settings` (for example the built-in Timescale 2→0 suppression); different
descriptors with different effective values refuse, and an inherited ordinary
destination value never beats a mandatory suppression. A historical-only
suppression survives later ordinary applies until the actual underlying
permanent configuration supplies the equivalent value. The check reads
`postgresql.conf`, `conf.d` in precedence order and the plain
`postgresql.auto.conf` assignment while excluding the generated guard block; an
include form or file it cannot prove fails closed. The released key is scoped
to the real installation instance and immutable stanza, and a fresh pending
copy ignores an earlier handoff and re-establishes its suppression. A later
deliberate operator change is not overridden by a reconstructed guard.

Every generated guard records the origin of each preload (`retained`,
`desired` or `operator`). A previous guard entry with a proven obsolete
operator or current-intent origin is dropped; an unexplained entry, including a
valid guard from before this provenance existed, refuses before any overwrite
or first start instead of being silently erased or loaded. A later deliberate
operator change is not overridden by a reconstructed guard, and a handoff is
honored only with actual higher-precedence permanent configuration and an
effective released key; unknown facts refuse.

Copied `pg_cron` jobs are suppressed from the copy's first start through
promotion, rename and privilege settlement. Deactivation and retargeting use
the actual copied job metadata and database identity (including OID changes
across a rename), never a guessed scheduler query; an unreadable `cron.job`
refuses rather than reporting zero. Durable instance-, stanza- and
operation-bound activation checkpoints keep ordinary convergence, public SQL
and both poolers from exposing unsettled data; safety completion precedes
verified routing finalization and checkpoint retirement.

A continuously following cluster is planned read-only from its source's current
inherited descriptors, the SQL extensions actually present in its data, the
existing guard's retained libraries and the current operator preloads. Current
descriptors replace their previous ordinary settings; genuinely retained extras
keep their library, settings and safety metadata; a planned primary SQL drop is
excluded. Every selected descriptor, SQL-only included, is checked for known
copy safety, compatible copy settings and explicit standby safety before any
history, save, package, configuration, restart or DROP effect. The follower's
complete winning guard is refreshed before reload/restart and is never removed
or handed off while it follows. A guard-only drift is detected by the
configuration Check's same read-only plan.

Built-in extension grants use the resolved schema. An accepted `schema`
override for an entry such as `pg_partman` therefore grants that schema, never
the built-in default schema.

A preload-dependent `CREATE EXTENSION` is not run while its library is not
loaded. When `shared_preload_libraries` is pending a restart, the roles step
emits `extension_pending_restart`, records itself incomplete and continues;
the confirmed restart loads the library, and a later apply creates the SQL.
The configuration step owns the restart; the roles step never restarts a
cluster. If the library is missing and no restart is pending to load it, vaire
reports the actual missing prerequisite instead of promising that a restart
will cure it.

## Example: a real package outside the built-in list

`pg_stat_kcache` is available from PGDG for PostgreSQL 18 as
`postgresql-18-pg-stat-kcache`. This example is a descriptor, not a claim that
the package is present on a particular host; verify package availability and
the extension's actual runtime behavior during live qualification.

```toml
[extensions.pg_stat_kcache]
source       = "apt"
package      = "postgresql-18-pg-stat-kcache"
preload      = "pg_stat_kcache"
settings     = { "pg_stat_kcache.track" = "all" }
copy_safety  = "safe"
standby_safe = true
note         = "kernel-level query statistics from PGDG; optional companion to pg_stat_statements"

[[clusters]]
name        = "app"
# ...
extensions  = ["pg_stat_statements", "pg_stat_kcache"]
```

A preinstalled extension is declared with `source = "preinstalled"` and no
`package`:

```toml
[extensions.operator_ext]
source       = "preinstalled"
ext_name     = "operator_ext"
standby_safe = false
note         = "installed by the operator; copies and followers require a safety declaration"
```

## Commands

The built-in catalogue is read from the same resolver as configuration:

```sh
vaire ext list
vaire ext list --db app
```

`ext list` reports package and control-file availability, the default version,
each database's installed version and update availability, pending preload
restarts, unmanaged installed extensions (excluding `plpgsql`) and installed
packages no current selection uses. A stopped, awaiting or unreadable database
is shown as unavailable; it is not reported as having no extensions or version
zero. Doctor reports package/file prerequisites, SQL/update state and pending
restarts separately. Neither output copies descriptor settings, SQL or package
lists into activity history; activity keeps method, target and outcome
metadata only.

Add a built-in or an existing custom definition by canonical name. `pgvector`
is accepted as an alias for `vector` and stored as `vector`:

```sh
vaire ext add vector --db app
vaire ext add pg_cron --db app --confirm app/restart
```

An unknown name can be registered and selected in one step. The definition is
persisted only after the whole descriptor, target, tokens and protection checks
pass:

```sh
vaire ext add pg_stat_kcache --db app \
  --package postgresql-18-pg-stat-kcache \
  --preload pg_stat_kcache \
  --setting pg_stat_kcache.track=all \
  --copy-safety safe --standby-safe \
  --confirm app/restart
```

Use `--preinstalled` instead of `--package` for files the operator already
installed. `--schema`, repeated `--setting`, repeated `--copy-setting` and
`--copy-safety disabled` follow the descriptor rules above. `--db` is inferred
only when exactly one independent database exists.

`[extensions]` definitions are shared by every database, including implicit
default modules and recovery declarations. Direct flags are a partial edit: an
existing built-in override keeps its provider, safety, schema and unspecified
settings. If the proposed definition would change any database other than the
named target, `ext add` refuses before saving, installing packages, restarting
or running SQL; edit the shared definition and apply every affected database
deliberately instead.

A source with followers propagates an extension change through the complete
target set. The preflight validates every selected descriptor's metadata
before history, save, package, configuration, restart or SQL; an awaiting
follower validates metadata only, while a materialized follower must answer
its actual SQL catalog. Each affected cluster's exact restart token is
required, a source token never authorizes a follower restart, and a direct
change on a follower is always refused. Followers are configured and verified
before the primary; SQL runs only on the primary. Ordinary configuration/apply
may stage safe follower work without every restart token, but unmet follower
runtime requirements withhold dependent primary configuration and SQL rather
than bypassing them. This source behavior is accepted at the cumulative E7-05
source boundary; it is not a live PostgreSQL/PgBouncer qualification.

Changing a definition's source, package or preload requires both
`--copy-safety` and `--standby-safe` to be supplied again; an earlier safety
declaration is not inherited across that provider change. An explicit
`--standby-safe=false` counts as supplied. Settings, schema and safety-only
edits keep the previous declarations.

Removal requires the database name and its confirm token. A preload change also
requires that database's restart token; a server-wide token is never a
substitute. The preview lists at most the first 50 dependent objects and the
extension's own tables with their total counts. Dependent objects block removal
unless `--cascade` is explicit; the extension's own tables always go with it:

```sh
vaire ext remove vector --db app --confirm app
vaire ext remove pg_cron --db app --confirm app --confirm app/restart
vaire ext remove vector --db app --cascade --confirm app
```

`--dry-run` needs no mutation tokens and performs no persistence, package
installation, restart or SQL. A protected database may take an extension add
without a new TOTP gate; its removal is refused. Re-adding an enabled extension
finishes pending SQL or a supported `ALTER EXTENSION UPDATE` to the installed
package's default and reports the actual resulting version. A no-op is allowed
only when the configuration files, journal, runtime and every pending
postmaster setting are converged; otherwise the command needs the target's
restart token before it reconciles them. Removing an extension that contributes
postmaster settings also requires that restart token before the SQL drop or
configuration save. An automatic update is refused when numeric dotted versions
cannot establish that the installed package default is not a downgrade. No
package upgrade or extension downgrade is performed. Installed packages are
retained after removal or an intent change, and `--cascade` is the only way
dependent objects are dropped.

The follower runtime is planned from current inherited descriptors, actual
installed SQL, retained guard evidence and explicit outgoing removal evidence.
Current descriptors win their older ordinary runtime variants; genuinely
retained extras keep strict historical checks. The plan carries the complete
expected ordinary-plus-mandatory-copy settings and verifies them before primary
configuration or SQL. A removed extension's old guard obligations are retired
only after the follower catalog proves its SQL absent; a lagging replay keeps
the old library and suppression and reports pending work. A complete preflight
runtime plan is carried by identity into the post-effect guard and verification
boundary. If the follower catalog becomes unreadable, that plan preserves every
SQL-only and mandatory obligation and marks the result pending; without a
matching complete plan vaire stops before the guard write, reload, start or
restart.

A standalone primary may use a custom descriptor whose copy safety is still
unknown. The copy/standby gate applies when the data is actually followed,
forked or restored; those operations refuse unknown safety before effects. A
source with a current or awaiting follower gates all selected metadata,
including SQL-only descriptors.

A plain `apply` stages follower configuration, guards and packages without
requiring every restart token. It does not apply primary configuration or SQL
while a follower's required runtime is not effective. Missing tokens leave the
affected target pending with the supported retry tokens; only direct `ext add`
and `ext remove` require the whole target set's tokens before any effect.
Dependent extension SQL waits until its own required library and actual setting
values are effective; a reload-only old value or operator override defers the
SQL without inventing a restart token, while an unrelated pending PostgreSQL
setting does not block it. A supported `ALTER EXTENSION UPDATE` uses the same
effective primary check and leaves the extension at its installed version until
the value is corrected. Expected runtime values are decoded once: descriptor
config literals are decoded before merging, while `[postgres.settings]` and
`[clusters.settings]` raw values are used as-is because the renderer quotes
them. The serialized guard settings remain config literals. Comparison uses the
target's decoded `current_setting` value against the decoded expectation, so
`off` and the literal quote content `'off'` stay distinct. A source-and-follower
add verifies the
source against the resolved primary expectation, including supported
`[postgres.settings]` and `[clusters.settings]` overrides, while a follower's
expected runtime keeps its separate mandatory copy overlay. A roles-only policy
change with every extension SQL already installed does not query or gate on
follower extension state.

## Recovery declarations

`[[clusters]].recovery_extensions` is the operator's explicit, complete
extension requirement for restore points older than vaire's recorded coverage.
It is resolved through the same definitions and checked with the same safety
and conflict rules.

- Absent `recovery_extensions` means unknown. It does not assert that an old
  restore point used no extensions.
- `recovery_extensions = []` is an explicit assertion that the old point used
  none. It is not the same state as an absent key.
- A populated list names canonical definitions:

```toml
[[clusters]]
name                = "app"
# ...
recovery_extensions = ["pg_stat_statements", "pg_stat_kcache"]
```

The absent, explicitly empty and populated states survive cluster TOML,
`config get`/`config set`/`config edit`, cloning and routing views, and the
master-bundle build/read/restore path together with global custom definitions.
A descriptor edit changes runtime intent only when the definition is used by an
enabled database or recovery declaration; a human `note` change does not.

Retained requirements are bound to the immutable primary stanza, not to a
database name, and coverage starts only when vaire first observes that stanza's
requirements; it is never backdated. Coverage is checked against the full base
backup at the root of the selected set and its prior chain, not merely the
restore target time. A point before observed coverage needs the declaration; a
known conflict, missing repository/file or unknown safety refuses before the
copy starts. An unbound dump is never associated with a stanza by a reused
database name. Descriptor snapshots survive dropped/replaced stanzas for their
recovery lifetime and are metadata, not credentials.
