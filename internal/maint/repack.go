package maint

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/pg"
)

// pg_repack (Phase 7): the online VACUUM FULL. The client ships in
// postgresql-18-repack beside the extension and runs as postgres over the
// cluster's socket on an unprotected database. Protected databases are
// refused by the node; this package only builds the safe invocation and the
// catalog facts around it.

// RepackBin is the PostgreSQL 18 pg_repack client.
const RepackBin = "/usr/lib/postgresql/18/bin/pg_repack"

// RepackWaitSeconds bounds pg_repack's lock wait. With --no-kill-backend it
// never terminates another backend; it gives up on the table instead.
const RepackWaitSeconds = 60

// AutoRepackMinBytes is the least estimated bloat auto-repack acts on.
const AutoRepackMinBytes int64 = 64 << 20

// RepackArgs is runuser's argv for repacking table (name or schema.name) of
// database db on the cluster at socketDir and port. The table is passed as a
// SQL-quoted regclass input, because pg_repack compares --table with
// relid = $1::regclass. No password is used or logged.
func RepackArgs(socketDir string, port int, db, table string) ([]string, error) {
	if db == "" {
		return nil, errors.New("a repack needs a database")
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("repack port %d is invalid", port)
	}
	q, err := QuoteRelation(table)
	if err != nil {
		return nil, err
	}
	return []string{
		"-u", "postgres", "--", RepackBin,
		"-h", socketDir, "-p", strconv.Itoa(port),
		"-U", "postgres", "-d", db,
		"--table", q,
		"--no-kill-backend", "--wait-timeout=" + strconv.Itoa(RepackWaitSeconds),
	}, nil
}

// RepackCandidates are the tables of a bloat report whose estimated ratio is
// at least threshold and whose estimated bloat is at least minBytes, biggest
// first and deterministic on ties, as schema.name.
func RepackCandidates(r BloatReport, threshold float64, minBytes int64) []string {
	rows := append([]BloatRow(nil), r.Rows...)
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].BloatBytes != rows[j].BloatBytes {
			return rows[i].BloatBytes > rows[j].BloatBytes
		}
		if rows[i].Schema != rows[j].Schema {
			return rows[i].Schema < rows[j].Schema
		}
		return rows[i].Table < rows[j].Table
	})
	var out []string
	seen := map[string]bool{}
	for _, row := range rows {
		if row.Kind != "table" || row.Ratio < threshold || row.BloatBytes < minBytes || row.BloatBytes <= 0 {
			continue
		}
		name := row.Schema + "." + row.Table
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// RelationFacts are the catalog facts that decide whether a relation is an
// ordinary permanent heap table. Partition leaves and inheritance children are
// ordinary tables; a partition parent is not a target because it has no
// storage of its own.
type RelationFacts struct {
	OID            int64  `json:"oid"`
	Schema         string `json:"schema"`
	Table          string `json:"table"`
	Kind           string `json:"kind"`
	Persistence    string `json:"persistence"`
	AccessMethod   string `json:"access_method,omitempty"`
	IsPartition    bool   `json:"is_partition,omitempty"`
	HasParent      bool   `json:"has_parent,omitempty"`
	SystemSchema   bool   `json:"system_schema,omitempty"`
	RelationExists bool   `json:"-"`
}

// RelationFactsSQL resolves one relation by name or schema.name and returns
// its kind, persistence, access method and inheritance facts. to_regclass
// applies the session's search_path for an unqualified name.
func RelationFactsSQL(quoted string) string {
	return `SELECT c.oid::bigint AS oid, n.nspname AS schema, c.relname AS table,
  c.relkind AS kind, c.relpersistence AS persistence,
  coalesce(am.amname, '') AS access_method,
  c.relispartition AS is_partition,
  EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid) AS has_parent,
  (n.nspname IN ('pg_catalog', 'information_schema') OR n.nspname LIKE 'pg_toast%') AS system_schema
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_am am ON am.oid = c.relam
WHERE c.oid = to_regclass(` + literal(quoted) + `)`
}

// ParseRelationFacts reads RelationFactsSQL's one row.
func ParseRelationFacts(row map[string]string) RelationFacts {
	oid, _ := strconv.ParseInt(row["oid"], 10, 64)
	return RelationFacts{
		OID: oid, Schema: row["schema"], Table: row["table"],
		Kind: row["kind"], Persistence: row["persistence"], AccessMethod: row["access_method"],
		IsPartition: boolValue(row["is_partition"]), HasParent: boolValue(row["has_parent"]),
		SystemSchema: boolValue(row["system_schema"]), RelationExists: row["schema"] != "",
	}
}

// UnsupportedOrdinaryTable reports why facts do not describe a supported
// ordinary permanent heap table. Partition leaves and inheritance children are
// accepted; partition parents, foreign tables and non-heap AMs are not.
func (f RelationFacts) UnsupportedOrdinaryTable() (reason, message string, ok bool) {
	switch {
	case !f.RelationExists || f.Schema == "" || f.Table == "":
		return RepackReasonRelationNotFound, "the relation does not exist", true
	case f.SystemSchema:
		return RepackReasonSystemRelation, "system relations are not maintenance targets", true
	case f.Kind == "p":
		return RepackReasonPartitionParent, "a partitioned parent has no storage of its own; name an ordinary partition leaf instead", true
	case f.Kind == "f":
		return RepackReasonForeignTable, "foreign tables have no local table storage", true
	case f.Kind != "r":
		return RepackReasonUnsupportedRelation, "the relation is not an ordinary table", true
	case f.Persistence != "p":
		return RepackReasonPersistence, "the relation is not a permanent table", true
	case f.AccessMethod != "heap":
		return RepackReasonAccessMethod, "the relation does not use the heap access method", true
	}
	return "", "", false
}

// ExtensionChainSQL returns the extension-managed relation or namespace facts
// on the target and its inheritance ancestors. It is deliberately scoped: a
// plain table in a user schema returns no rows, while a relation or schema
// created by an extension (for example a Timescale chunk namespace) does.
func ExtensionChainSQL(quoted string) string {
	return `WITH RECURSIVE chain AS (
  SELECT c.oid, c.relnamespace, 0 AS depth
  FROM pg_class c WHERE c.oid = to_regclass(` + literal(quoted) + `)
  UNION ALL
  SELECT p.oid, p.relnamespace, ch.depth + 1
  FROM chain ch
  JOIN pg_inherits i ON i.inhrelid = ch.oid
  JOIN pg_class p ON p.oid = i.inhparent
)
SELECT DISTINCT e.extname AS extension, ch.depth AS depth,
  CASE WHEN d.classid = 'pg_class'::regclass THEN 'relation' ELSE 'schema' END AS scope
FROM chain ch
JOIN pg_depend d ON d.deptype = 'e'
  AND ((d.classid = 'pg_class'::regclass AND d.objid = ch.oid)
    OR (d.classid = 'pg_namespace'::regclass AND d.objid = ch.relnamespace))
JOIN pg_extension e ON e.oid = d.refobjid
ORDER BY depth, extension, scope`
}

// ExtensionDependency is one extension-managed relation or schema in the
// target's inheritance chain.
type ExtensionDependency struct {
	Extension string `json:"extension"`
	Depth     int    `json:"depth"`
	Scope     string `json:"scope"`
}

// ParseExtensionChain reads ExtensionChainSQL rows.
func ParseExtensionChain(rows []map[string]string) []ExtensionDependency {
	out := make([]ExtensionDependency, 0, len(rows))
	for _, row := range rows {
		depth, _ := strconv.Atoi(row["depth"])
		out = append(out, ExtensionDependency{Extension: row["extension"], Depth: depth, Scope: row["scope"]})
	}
	return out
}

// RepackKeySQL tells, as t or f, whether the relation has a primary key or a
// valid unique index on NOT NULL key columns, with no predicate and no
// expressions. Included columns may be nullable; only the key attributes
// through indnkeyatts are checked. This mirrors pg_repack's own primary_keys
// view.
func RepackKeySQL(quoted string) string {
	return `SELECT EXISTS (
  SELECT 1
  FROM pg_index i
  WHERE i.indrelid = ` + literal(quoted) + `::regclass
    AND i.indisunique
    AND i.indisvalid
    AND i.indpred IS NULL
    AND i.indexprs IS NULL
    AND 0 <> ALL(i.indkey)
    AND NOT EXISTS (
      SELECT 1 FROM pg_attribute a
      WHERE a.attrelid = i.indrelid
        AND a.attnum = ANY(i.indkey[0:i.indnkeyatts - 1])
        AND NOT a.attnotnull)
) AS ok`
}

// RelationSizeSQL is the table's size including indexes and TOAST.
func RelationSizeSQL(quoted string) string {
	return `SELECT pg_total_relation_size(` + literal(quoted) + `::regclass) AS n`
}

// RepackTablespaceSQL resolves the effective tablespace of the table, its
// TOAST relation, its indexes and its TOAST index. reltablespace = 0 means the
// database default tablespace, not necessarily PGDATA. Repack only supports
// pg_default destinations; an external tablespace is refused before execution.
func RepackTablespaceSQL(quoted string) string {
	return `WITH rels AS (
  SELECT 'table' AS kind, c.oid, c.relname, c.reltablespace
  FROM pg_class c WHERE c.oid = to_regclass(` + literal(quoted) + `)
  UNION ALL
  SELECT 'toast', t.oid, t.relname, t.reltablespace
  FROM pg_class c JOIN pg_class t ON t.oid = c.reltoastrelid
  WHERE c.oid = to_regclass(` + literal(quoted) + `)
  UNION ALL
  SELECT 'index', i.indexrelid, idx.relname, idx.reltablespace
  FROM pg_class c
  JOIN pg_index i ON i.indrelid = c.oid
  JOIN pg_class idx ON idx.oid = i.indexrelid
  WHERE c.oid = to_regclass(` + literal(quoted) + `)
  UNION ALL
  SELECT 'toast index', ti.indexrelid, tidx.relname, tidx.reltablespace
  FROM pg_class c
  JOIN pg_class t ON t.oid = c.reltoastrelid
  JOIN pg_index ti ON ti.indrelid = t.oid
  JOIN pg_class tidx ON tidx.oid = ti.indexrelid
  WHERE c.oid = to_regclass(` + literal(quoted) + `)
)
SELECT r.kind AS kind, r.relname AS name, t.spcname AS tablespace
FROM rels r
CROSS JOIN pg_database d
JOIN pg_tablespace t ON t.oid = CASE WHEN r.reltablespace = 0 THEN d.dattablespace ELSE r.reltablespace END
WHERE d.datname = current_database()
ORDER BY r.kind, r.relname`
}

// TablespaceFact is one table/TOAST/index destination.
type TablespaceFact struct {
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	Tablespace string `json:"tablespace"`
}

// ParseTablespaceFacts reads RepackTablespaceSQL rows.
func ParseTablespaceFacts(rows []map[string]string) []TablespaceFact {
	out := make([]TablespaceFact, 0, len(rows))
	for _, row := range rows {
		out = append(out, TablespaceFact{Kind: row["kind"], Name: row["name"], Tablespace: row["tablespace"]})
	}
	return out
}

// SupportedPGDATA reports whether the destination is the cluster's PGDATA
// tablespace. Repack does not attempt to prove an external filesystem.
func (f TablespaceFact) SupportedPGDATA() bool { return f.Tablespace == "pg_default" }

// RepackStorageSQL reads the table's storage identity before and after an
// attempt. A changed relfilenode is diagnostic evidence only; it cannot prove
// that the rewrite completed, and a zero-exit skip may leave it unchanged.
func RepackStorageSQL(quoted string) string {
	return `SELECT c.oid::bigint AS oid, c.relfilenode::bigint AS relfilenode,
  t.spcname AS tablespace
FROM pg_class c
CROSS JOIN pg_database d
JOIN pg_tablespace t ON t.oid = CASE WHEN c.reltablespace = 0 THEN d.dattablespace ELSE c.reltablespace END
WHERE c.oid = to_regclass(` + literal(quoted) + `) AND d.datname = current_database()`
}

// RepackStorage is one relation's storage identity.
type RepackStorage struct {
	OID         int64  `json:"oid"`
	RelFileNode int64  `json:"relfilenode"`
	TableSpace  string `json:"tablespace"`
}

// ParseRepackStorage reads RepackStorageSQL's one row.
func ParseRepackStorage(row map[string]string) RepackStorage {
	oid, _ := strconv.ParseInt(row["oid"], 10, 64)
	rel, _ := strconv.ParseInt(row["relfilenode"], 10, 64)
	return RepackStorage{OID: oid, RelFileNode: rel, TableSpace: row["tablespace"]}
}

// RepackRewritten reports whether the same relation OID changed relfilenode,
// the storage identity pg_repack swaps on a completed rewrite. It is a
// diagnostic signal, not proof that the rewrite completed.
func RepackRewritten(before, after RepackStorage) bool {
	return before.OID != 0 && before.OID == after.OID &&
		before.TableSpace == after.TableSpace &&
		before.RelFileNode != after.RelFileNode
}

// RepackDBDefaultTablespaceSQL reads the database default tablespace through
// the unprivileged catalog connection.
const RepackDBDefaultTablespaceSQL = `SELECT t.spcname AS tablespace
FROM pg_database d
JOIN pg_tablespace t ON t.oid = d.dattablespace
WHERE d.datname = current_database()`

// RepackInstalledSQL distinguishes an installed pg_repack from one whose
// version functions are missing or unreadable.
const RepackInstalledSQL = `SELECT extversion FROM pg_extension WHERE extname = 'pg_repack'`

// RepackExtensionSchemaSQL resolves the schema of pg_repack's version_sql
// function. The extension creates its own repack schema; extnamespace is not
// the function schema when the extension itself is installed in public.
const RepackExtensionSchemaSQL = `SELECT n.nspname AS schema
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid
WHERE e.extname = 'pg_repack' AND p.proname = 'version_sql'
LIMIT 1`

// RepackVersionsSQL reads the installed extension, library and SQL versions.
// schema is the schema holding pg_repack's version functions, normally repack.
func RepackVersionsSQL(schema string) string {
	s := pg.Quote(schema)
	return `SELECT e.extversion AS extension_version,
  ` + s + `.version() AS library_version,
  ` + s + `.version_sql() AS sql_version,
  dts.spcname AS db_default_tablespace,
  current_setting('default_tablespace') AS session_default_tablespace
FROM pg_extension e
JOIN pg_database d ON d.datname = current_database()
JOIN pg_tablespace dts ON dts.oid = d.dattablespace
WHERE e.extname = 'pg_repack'`
}

// RepackVersions are the four version strings that must agree, plus the
// database and actual postgres session default tablespace facts.
type RepackVersions struct {
	Client                   string `json:"client"`
	Extension                string `json:"extension"`
	Library                  string `json:"library"`
	SQL                      string `json:"sql"`
	DBDefaultTablespace      string `json:"db_default_tablespace,omitempty"`
	SessionDefaultTablespace string `json:"session_default_tablespace,omitempty"`
}

// NormalizeRepackVersion accepts pg_repack's "pg_repack X" and the catalog's
// bare "X" forms and returns X. Anything else is ambiguous and refused.
func NormalizeRepackVersion(value string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(value))
	switch {
	case len(fields) == 1:
		return fields[0], nil
	case len(fields) == 2 && strings.EqualFold(fields[0], "pg_repack"):
		return fields[1], nil
	}
	return "", fmt.Errorf("%q is not a pg_repack version", value)
}

// NormalizeRepackVersions normalizes all four version strings.
func NormalizeRepackVersions(client, extension, library, sql string) (RepackVersions, error) {
	out := RepackVersions{Client: client, Extension: extension, Library: library, SQL: sql}
	for name, value := range map[string]string{
		"client": client, "extension": extension, "library": library, "sql": sql,
	} {
		if _, err := NormalizeRepackVersion(value); err != nil {
			return out, fmt.Errorf("%s version: %w", name, err)
		}
	}
	return out, nil
}

// Match reports whether the four normalized versions agree.
func (v RepackVersions) Match() bool {
	c, err := NormalizeRepackVersion(v.Client)
	if err != nil {
		return false
	}
	for _, value := range []string{v.Extension, v.Library, v.SQL} {
		got, err := NormalizeRepackVersion(value)
		if err != nil || got != c {
			return false
		}
	}
	return true
}

// RepackSpaceNeed is the free space vaire requires for one repack: twice the
// table, index and TOAST footprint. backup.CheckSpace adds the host reserve.
func RepackSpaceNeed(size int64) (uint64, error) {
	if size < 0 {
		return 0, fmt.Errorf("negative relation size %d", size)
	}
	n := uint64(size)
	if n > ^uint64(0)/2 {
		return 0, fmt.Errorf("relation size %d overflows the repack space need", size)
	}
	return 2 * n, nil
}

// Repack outcome and reason tokens. They are fixed identifiers, safe for job
// results and activity metadata; table names are separate fields.
const (
	// RepackOutcomeRepacked is reserved for a future attributable positive
	// proof. The current classifier never returns it from process status or
	// storage changes alone.
	RepackOutcomeRepacked = "repacked"
	RepackOutcomeFailed   = "failed"
	RepackOutcomeSkipped  = "skipped"
	RepackOutcomeBusy     = "busy"
	RepackOutcomeUnknown  = "unknown"
)

const (
	RepackReasonProtected           = "protected_repack_unsupported"
	RepackReasonExtensionMissing    = "extension_missing"
	RepackReasonClientMissing       = "repack_client_missing"
	RepackReasonVersionMismatch     = "repack_version_mismatch"
	RepackReasonFailed              = "repack_failed"
	RepackReasonExactFailed         = "exact_bloat_failed"
	RepackReasonOutsideWindow       = "outside_window"
	RepackReasonWindowEnded         = "window_ended"
	RepackReasonHeavyIOBusy         = "heavy_io_busy"
	RepackReasonClusterBusy         = "cluster_lock_busy"
	RepackReasonNoKey               = "repack_needs_key"
	RepackReasonUnsupportedRelation = "unsupported_relation"
	RepackReasonPartitionParent     = "unsupported_partition_parent"
	RepackReasonForeignTable        = "unsupported_foreign_table"
	RepackReasonPersistence         = "unsupported_relation_persistence"
	RepackReasonAccessMethod        = "unsupported_access_method"
	RepackReasonExtensionStorage    = "extension_storage"
	RepackReasonSystemRelation      = "system_relation"
	RepackReasonRelationNotFound    = "relation_not_found"
	RepackReasonRelationChanged     = "relation_changed"
	RepackReasonTablespace          = "unsupported_tablespace"
	RepackReasonDefaultTablespace   = "unsupported_default_tablespace"
	RepackReasonInsufficientSpace   = "insufficient_space"
	RepackReasonSpaceUnknown        = "space_unknown"
	RepackReasonLockTimeout         = "repack_lock_timeout"
	RepackReasonInvalidIndex        = "repack_invalid_index"
	RepackReasonUnsafeDDL           = "repack_unsafe_ddl"
	RepackReasonCompletionUnknown   = "completion_unknown"
	RepackReasonClusterNotRunning   = "cluster_not_running"
	RepackReasonActivationFence     = "activation_fence_active"
)

// RepackSkip is one candidate that was not repacked, with a fixed reason.
type RepackSkip struct {
	Table   string `json:"table"`
	Reason  string `json:"reason"`
	Message string `json:"message,omitempty"`
}

// RepackCounts are one run's exact outcome counts.
type RepackCounts struct {
	Selected        int            `json:"selected"`
	Repacked        int            `json:"repacked"`
	Failed          int            `json:"failed"`
	Unknown         int            `json:"unknown"`
	Skipped         int            `json:"skipped"`
	Busy            int            `json:"busy"`
	SkippedByReason map[string]int `json:"skipped_by_reason,omitempty"`
	At              string         `json:"at,omitempty"`
}

// AddRepacked counts one table whose rewrite completion was confirmed.
func (c *RepackCounts) AddRepacked() { c.Selected++; c.Repacked++ }

// AddFailed counts one failed table.
func (c *RepackCounts) AddFailed() { c.Selected++; c.Failed++ }

// AddUnknown counts one attempted table whose rewrite completion could not be
// attributed from external process observables.
func (c *RepackCounts) AddUnknown() { c.Selected++; c.Unknown++ }

// AddSkipped counts one table skipped for reason.
func (c *RepackCounts) AddSkipped(reason string) {
	c.Selected++
	c.Skipped++
	if c.SkippedByReason == nil {
		c.SkippedByReason = map[string]int{}
	}
	c.SkippedByReason[reason]++
}

// AddBusy counts one table that could not start because a lock was busy.
func (c *RepackCounts) AddBusy() { c.Selected++; c.Busy++ }

// RepackRun is one database's repack outcome.
type RepackRun struct {
	DB       string            `json:"db"`
	Tables   []string          `json:"tables,omitempty"`
	Repacked []string          `json:"repacked,omitempty"`
	Failed   map[string]string `json:"failed,omitempty"`
	Unknown  map[string]string `json:"unknown,omitempty"`
	Skipped  []RepackSkip      `json:"skipped,omitempty"`
	Busy     []RepackSkip      `json:"busy,omitempty"`
	Counts   RepackCounts      `json:"counts"`
}

func literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

func boolValue(s string) bool { return s == "t" || s == "true" }
