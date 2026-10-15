package maint

import (
	"sort"
	"strconv"
	"strings"
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
