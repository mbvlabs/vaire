package maint

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/pg"
)

// sqlFile reads an embedded query without its trailing semicolon, so it can
// be wrapped as a subquery.
func sqlFile(name string) string {
	raw, err := sqlFS.ReadFile("sql/" + name)
	if err != nil {
		panic("missing embedded SQL " + name)
	}
	return strings.TrimSuffix(strings.TrimSpace(string(raw)), ";")
}

// BloatSQL estimates the bloat of every table and btree index of the
// connected database with the vendored ioguix queries (Phase 4 D3), as one
// result: kind, schema, table, index, real size, estimated bloat, ratio,
// fillfactor and whether the estimate is unreliable (is_na: no statistics,
// or a "name" column).
func BloatSQL() string {
	return `SELECT 'table' AS kind, schemaname, tblname AS table_name, '' AS index_name,
  real_size::bigint AS real_size, bloat_size::bigint AS bloat_bytes, round(bloat_pct::numeric, 1) AS bloat_pct, fillfactor, is_na
FROM (` + sqlFile("table_bloat.sql") + `
) t WHERE schemaname NOT IN ('pg_catalog', 'information_schema') AND schemaname NOT LIKE 'pg_toast%'
UNION ALL
SELECT 'index', schemaname, tblname, idxname, real_size::bigint, bloat_size::bigint, round(bloat_pct::numeric, 1), fillfactor, is_na
FROM (` + sqlFile("btree_bloat.sql") + `
) b WHERE schemaname NOT IN ('pg_catalog', 'information_schema') AND schemaname NOT LIKE 'pg_toast%'`
}

// BloatRow is one table or index in the bloat report.
type BloatRow struct {
	DB         string  `json:"db"`
	Kind       string  `json:"kind"`
	Schema     string  `json:"schema"`
	Table      string  `json:"table"`
	Index      string  `json:"index,omitempty"`
	RealSize   int64   `json:"real_size"`
	BloatBytes int64   `json:"est_bloat_bytes"`
	Ratio      float64 `json:"ratio"`
	Fillfactor int     `json:"fillfactor"`
}

// BloatReport is a database's bloat estimate: the relations with at least
// minBytes of estimated bloat, biggest first, and those it couldn't
// estimate.
type BloatReport struct {
	Rows        []BloatRow `json:"rows"`
	Unestimated []string   `json:"unestimated,omitempty"`
}

// ParseBloat shapes BloatSQL rows of db.
func ParseBloat(db string, rows []map[string]string, minBytes int64) BloatReport {
	out := BloatReport{Rows: []BloatRow{}}
	for _, r := range rows {
		name := r["schemaname"] + "." + r["table_name"]
		if r["index_name"] != "" {
			name += "." + r["index_name"]
		}
		if r["is_na"] == "t" || r["is_na"] == "true" {
			out.Unestimated = append(out.Unestimated, name)
			continue
		}
		bytes, _ := strconv.ParseInt(r["bloat_bytes"], 10, 64)
		if bytes < minBytes || bytes <= 0 {
			continue
		}
		size, _ := strconv.ParseInt(r["real_size"], 10, 64)
		pct, _ := strconv.ParseFloat(r["bloat_pct"], 64)
		ff, _ := strconv.Atoi(r["fillfactor"])
		out.Rows = append(out.Rows, BloatRow{DB: db, Kind: r["kind"], Schema: r["schemaname"], Table: r["table_name"], Index: r["index_name"],
			RealSize: size, BloatBytes: bytes, Ratio: pct / 100, Fillfactor: ff})
	}
	sort.SliceStable(out.Rows, func(i, j int) bool { return out.Rows[i].BloatBytes > out.Rows[j].BloatBytes })
	sort.Strings(out.Unestimated)
	return out
}

// TotalBloat sums a report's estimated bloat by kind.
func TotalBloat(r BloatReport) map[string]int64 {
	out := map[string]int64{"table": 0, "index": 0}
	for _, row := range r.Rows {
		out[row.Kind] += row.BloatBytes
	}
	return out
}

// PgstattupleInstalledSQL distinguishes an installed pgstattuple from one
// whose functions are missing or unreadable.
const PgstattupleInstalledSQL = `SELECT extversion FROM pg_extension WHERE extname = 'pgstattuple'`

// PgstattupleSchemaSQL resolves the schema of pgstattuple's function, so the
// report calls it without relying on search_path.
const PgstattupleSchemaSQL = `SELECT n.nspname AS schema
FROM pg_proc p
JOIN pg_namespace n ON n.oid = p.pronamespace
JOIN pg_depend d ON d.classid = 'pg_proc'::regclass AND d.objid = p.oid AND d.deptype = 'e'
JOIN pg_extension e ON e.oid = d.refobjid
WHERE e.extname = 'pgstattuple' AND p.proname = 'pgstattuple'
LIMIT 1`

// ExactBloatSQL is the full-scan pgstattuple measurement of one table. It
// never uses pgstattuple_approx. schema is the extension's schema; the table
// is a SQL-quoted regclass input.
func ExactBloatSQL(table, schema string) (string, error) {
	q, err := QuoteRelation(table)
	if err != nil {
		return "", err
	}
	if schema == "" {
		return "", fmt.Errorf("pgstattuple schema is empty")
	}
	return `SELECT table_len::bigint AS table_len,
  tuple_count::bigint AS tuple_count,
  tuple_len::bigint AS tuple_len,
  tuple_percent::float8 AS tuple_percent,
  dead_tuple_count::bigint AS dead_tuple_count,
  dead_tuple_len::bigint AS dead_tuple_len,
  dead_tuple_percent::float8 AS dead_tuple_percent,
  free_space::bigint AS free_space,
  free_percent::float8 AS free_percent
FROM ` + pg.Quote(schema) + `.pgstattuple(` + literal(q) + `::regclass)`, nil
}

// TableStats is one table's measured pgstattuple result. FreeBytes is the
// measured free space in the heap, not a promise that it can be reclaimed;
// TableBytes includes page overhead, so the components need not add up.
type TableStats struct {
	DB               string  `json:"db"`
	Schema           string  `json:"schema"`
	Table            string  `json:"table"`
	Method           string  `json:"method"`
	TableBytes       int64   `json:"table_bytes"`
	TupleCount       int64   `json:"tuple_count"`
	TupleBytes       int64   `json:"tuple_bytes"`
	TuplePercent     float64 `json:"tuple_percent"`
	DeadTupleCount   int64   `json:"dead_tuple_count"`
	DeadTupleBytes   int64   `json:"dead_tuple_bytes"`
	DeadTuplePercent float64 `json:"dead_tuple_percent"`
	FreeBytes        int64   `json:"free_bytes"`
	FreePercent      float64 `json:"free_percent"`
}

// ParseTableStats reads exactly one pgstattuple row.
func ParseTableStats(db, schema, table string, rows []map[string]string) (TableStats, error) {
	if len(rows) != 1 {
		return TableStats{}, fmt.Errorf("pgstattuple returned %d rows for %s.%s, want one", len(rows), schema, table)
	}
	r := rows[0]
	parseInt := func(name string) (int64, error) {
		v, err := strconv.ParseInt(r[name], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return v, nil
	}
	parseFloat := func(name string) (float64, error) {
		v, err := strconv.ParseFloat(r[name], 64)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", name, err)
		}
		return v, nil
	}
	var out TableStats
	out.DB, out.Schema, out.Table, out.Method = db, schema, table, "pgstattuple"
	for name, dst := range map[string]*int64{
		"table_len": &out.TableBytes, "tuple_count": &out.TupleCount, "tuple_len": &out.TupleBytes,
		"dead_tuple_count": &out.DeadTupleCount, "dead_tuple_len": &out.DeadTupleBytes, "free_space": &out.FreeBytes,
	} {
		v, err := parseInt(name)
		if err != nil {
			return TableStats{}, err
		}
		*dst = v
	}
	for name, dst := range map[string]*float64{
		"tuple_percent": &out.TuplePercent, "dead_tuple_percent": &out.DeadTuplePercent, "free_percent": &out.FreePercent,
	} {
		v, err := parseFloat(name)
		if err != nil {
			return TableStats{}, err
		}
		*dst = v
	}
	return out, nil
}
