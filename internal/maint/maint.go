// Package maint is eru's maintenance (Phase 4): the reports, the bloat
// estimate, the vacuum, analyze and reindex statements, and which backends
// eru maint kill may terminate. The node runs it as eru_admin over each
// database's socket; this package builds the SQL and shapes the results.
package maint

import (
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/pg"
)

//go:embed sql/*.sql sql/LICENSE.ioguix
var sqlFS embed.FS

// License is the ioguix bloat queries' BSD-2-Clause notice, which ships
// with the binary (eru version --licenses).
func License() string {
	raw, _ := sqlFS.ReadFile("sql/LICENSE.ioguix")
	return string(raw)
}

// Report kinds.
const (
	ReportBloat          = "bloat"
	ReportWraparound     = "wraparound"
	ReportUnusedIndexes  = "unused-indexes"
	ReportMissingIndexes = "missing-indexes"
	ReportLongQueries    = "long-queries"
	ReportLocks          = "locks"
)

// ReportKinds lists the report kinds in help order.
var ReportKinds = []string{ReportBloat, ReportWraparound, ReportUnusedIndexes, ReportMissingIndexes, ReportLongQueries, ReportLocks}

// ValidReport reports whether kind is a report kind.
func ValidReport(kind string) bool { return slices.Contains(ReportKinds, kind) }

// SessionSetup starts every maintenance session: a short lock timeout so a
// statement never queues behind a long lock and blocks the application
// behind it, and no statement timeout (vacuum and reindex can be long).
const SessionSetup = "SET lock_timeout = '5s';\nSET statement_timeout = 0;\n"

// ReportSetup starts a report session: read-only and bounded.
const ReportSetup = "SET default_transaction_read_only = on;\nSET lock_timeout = '5s';\nSET statement_timeout = '60s';\n"

// Thresholds of the wraparound report, the same as doctor's.
const (
	WraparoundWarn = 1_000_000_000
	WraparoundFail = 1_600_000_000
)

const wraparoundSQL = `SELECT 'database' AS kind, datname AS name, age(datfrozenxid)::bigint AS xid_age, mxid_age(datminmxid)::bigint AS mxid_age
FROM pg_database WHERE datallowconn
UNION ALL
(SELECT 'table', n.nspname || '.' || c.relname, age(c.relfrozenxid)::bigint, mxid_age(c.relminmxid)::bigint
 FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE c.relkind IN ('r', 'm', 't') AND c.relfrozenxid <> '0'
 ORDER BY 3 DESC LIMIT 20)`

const unusedIndexesSQL = `SELECT s.schemaname, s.relname AS table_name, s.indexrelname AS index_name,
  pg_relation_size(s.indexrelid) AS bytes, s.idx_scan
FROM pg_stat_user_indexes s JOIN pg_index i ON i.indexrelid = s.indexrelid
WHERE s.idx_scan = 0 AND NOT i.indisunique AND NOT i.indisprimary
  AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = s.indexrelid)
  AND pg_relation_size(s.indexrelid) > 1048576
  AND coalesce((SELECT stats_reset FROM pg_stat_database WHERE datname = current_database()), pg_postmaster_start_time()) < now() - interval '7 days'
ORDER BY bytes DESC`

const missingIndexesSQL = `SELECT schemaname, relname AS table_name, seq_scan, seq_tup_read, coalesce(idx_scan, 0) AS idx_scan, n_live_tup,
  seq_tup_read / greatest(seq_scan, 1) AS avg_seq_tup_read
FROM pg_stat_user_tables
WHERE seq_scan > 1000 AND seq_tup_read / greatest(seq_scan, 1) > 10000 AND n_live_tup > 100000
ORDER BY seq_tup_read DESC LIMIT 50`

const locksSQL = `SELECT a.pid, a.usename AS role, a.state, coalesce(a.wait_event_type || ':' || a.wait_event, '') AS wait,
  coalesce(extract(epoch FROM now() - a.query_start)::bigint, 0) AS seconds,
  array_to_string(pg_blocking_pids(a.pid), ',') AS blocked_by, left(a.query, 300) AS query
FROM pg_stat_activity a
WHERE cardinality(pg_blocking_pids(a.pid)) > 0
ORDER BY seconds DESC`

// longQueriesSQL lists sessions busy for longer than seconds.
func longQueriesSQL(seconds int) string {
	return `SELECT pid, usename AS role, state, coalesce(wait_event_type || ':' || wait_event, '') AS wait,
  extract(epoch FROM now() - coalesce(xact_start, query_start))::bigint AS seconds, left(query, 500) AS query
FROM pg_stat_activity
WHERE backend_type = 'client backend' AND state <> 'idle' AND pid <> pg_backend_pid()
  AND now() - coalesce(xact_start, query_start) > make_interval(secs => ` + strconv.Itoa(seconds) + `)
ORDER BY seconds DESC`
}

// ReportSQL is the query of a report kind (not bloat, which has its own).
func ReportSQL(kind string, olderThanSeconds int) (string, error) {
	switch kind {
	case ReportWraparound:
		return wraparoundSQL, nil
	case ReportUnusedIndexes:
		return unusedIndexesSQL, nil
	case ReportMissingIndexes:
		return missingIndexesSQL, nil
	case ReportLongQueries:
		if olderThanSeconds < 0 {
			olderThanSeconds = 0
		}
		return longQueriesSQL(olderThanSeconds), nil
	case ReportLocks:
		return locksSQL, nil
	case ReportBloat:
		return BloatSQL(), nil
	}
	return "", fmt.Errorf("unknown report %q", kind)
}

// WraparoundStatus is pass, warn or fail for a transaction ID age.
func WraparoundStatus(age int64) string {
	switch {
	case age > WraparoundFail:
		return "fail"
	case age > WraparoundWarn:
		return "warn"
	}
	return "pass"
}

// relationPattern is a table or index name eru accepts: name or
// schema.name, plain identifiers only.
var relationPattern = regexp.MustCompile(`^([a-zA-Z_][a-zA-Z0-9_$]*\.)?[a-zA-Z_][a-zA-Z0-9_$]*$`)

// QuoteRelation quotes a name or schema.name, refusing anything else.
func QuoteRelation(name string) (string, error) {
	if !relationPattern.MatchString(name) {
		return "", fmt.Errorf("%q is not a table or index name (name or schema.name)", name)
	}
	parts := strings.Split(name, ".")
	for i, p := range parts {
		parts[i] = pg.Quote(p)
	}
	return strings.Join(parts, "."), nil
}
