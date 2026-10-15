package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/steps"
)

// statement is one row of eru queries.
type statement struct {
	DB      string  `json:"db"`
	Role    string  `json:"role"`
	QueryID string  `json:"queryid"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
	HitPct  float64 `json:"hit_pct"`
	Query   string  `json:"query"`
}

// queryOrders maps --order to the column it sorts by.
var queryOrders = map[string]string{"total": "total_exec_time", "mean": "mean_exec_time", "calls": "calls"}

// statementsSQL reads the cluster's top statements of its own database.
func statementsSQL(order string, limit int) string {
	return `SELECT r.rolname AS role, s.queryid::text AS queryid, s.calls,
  round(s.total_exec_time::numeric, 1) AS total_ms, round(s.mean_exec_time::numeric, 2) AS mean_ms, s.rows,
  coalesce(round((100.0 * s.shared_blks_hit / nullif(s.shared_blks_hit + s.shared_blks_read, 0))::numeric, 1), 100) AS hit_pct,
  left(regexp_replace(s.query, '\s+', ' ', 'g'), 500) AS query
FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())
ORDER BY s.` + queryOrders[order] + ` DESC LIMIT ` + strconv.Itoa(limit)
}

const hasStatementsSQL = `SELECT count(*) AS n FROM pg_extension WHERE extname = 'pg_stat_statements'`

type queriesParams struct {
	DB    string `json:"db,omitempty"`
	Limit int    `json:"limit,omitempty"`
	Order string `json:"order,omitempty"`
}

// queriesData is queries.top's result.
type queriesData struct {
	Order      string      `json:"order"`
	Statements []statement `json:"statements"`
	// Missing are the databases without pg_stat_statements.
	Missing []string `json:"missing,omitempty"`
}

// handleQueriesTop reads pg_stat_statements in one or every database as
// eru_admin (pg_monitor reads every role's statements) and merges them.
func handleQueriesTop(ctx context.Context, env *Env) (Result, error) {
	var p queriesParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.Order == "" {
		p.Order = "total"
	}
	if _, ok := queryOrders[p.Order]; !ok {
		return Result{}, output.NewError(output.CodeUsage, "--order must be total, mean or calls", output.ExitUsage, "")
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 20
	}
	specs, err := targetClusters(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	data := queriesData{Order: p.Order, Statements: []statement{}}
	perDB := map[string][]statement{}
	var warnings []output.Warning
	for _, spec := range specs {
		local := steps.AdminPG(spec)
		rows, err := local.Query(ctx, spec.Name, hasStatementsSQL, nil)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "database_unreachable", Message: spec.Name + ": " + err.Error()})
			continue
		}
		if len(rows) == 0 || rows[0]["n"] == "0" {
			data.Missing = append(data.Missing, spec.Name)
			continue
		}
		rows, err = local.Query(ctx, spec.Name, statementsSQL(p.Order, p.Limit), nil)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "database_unreachable", Message: spec.Name + ": " + err.Error()})
			continue
		}
		perDB[spec.Name] = parseStatements(spec.Name, rows)
	}
	data.Statements = mergeStatements(perDB, p.Order, p.Limit)
	if len(data.Missing) > 0 {
		if p.DB != "" {
			return Result{}, statementsMissing(p.DB)
		}
		warnings = append(warnings, output.Warning{Code: "extension_missing", Message: "pg_stat_statements is not installed in " + fmt.Sprint(data.Missing)})
	}
	return Result{Data: data, Warnings: warnings}, nil
}

func statementsMissing(db string) error {
	return output.NewError(output.CodePrecondition, "pg_stat_statements is not installed in "+db, output.ExitPrecondition,
		"Add it to the database's extensions (eru config set clusters."+db+".extensions pg_stat_statements, then eru apply --db "+db+"); eru ext add arrives in Phase 7.").
		WithDetails(map[string]any{"reason": "extension_missing", "extension": "pg_stat_statements", "database": db})
}

// targetClusters is db's cluster, or every cluster when db is empty.
func targetClusters(cfg config.Cluster, db string) ([]config.ClusterSpec, error) {
	if db == "" {
		return cfg.Clusters, nil
	}
	spec, err := clusterOf(cfg, db)
	if err != nil {
		return nil, err
	}
	return []config.ClusterSpec{spec}, nil
}

func parseStatements(db string, rows []map[string]string) []statement {
	out := make([]statement, 0, len(rows))
	for _, r := range rows {
		f := func(k string) float64 { v, _ := strconv.ParseFloat(r[k], 64); return v }
		i := func(k string) int64 { v, _ := strconv.ParseInt(r[k], 10, 64); return v }
		out = append(out, statement{DB: db, Role: r["role"], QueryID: r["queryid"], Calls: i("calls"),
			TotalMS: f("total_ms"), MeanMS: f("mean_ms"), Rows: i("rows"), HitPct: f("hit_pct"), Query: r["query"]})
	}
	return out
}

// mergeStatements merges every database's top statements and keeps the top
// limit by order.
func mergeStatements(perDB map[string][]statement, order string, limit int) []statement {
	all := []statement{}
	for _, rows := range perDB {
		all = append(all, rows...)
	}
	key := func(s statement) float64 {
		switch order {
		case "mean":
			return s.MeanMS
		case "calls":
			return float64(s.Calls)
		}
		return s.TotalMS
	}
	sort.SliceStable(all, func(i, j int) bool {
		if key(all[i]) != key(all[j]) {
			return key(all[i]) > key(all[j])
		}
		if all[i].DB != all[j].DB {
			return all[i].DB < all[j].DB
		}
		return all[i].QueryID < all[j].QueryID
	})
	if len(all) > limit {
		all = all[:limit]
	}
	return all
}

// statsToken is queries --reset's confirm token: <db>/stats for one
// database, <server>/stats for every database (Phase 4 D12).
func statsToken(cfg config.Cluster, params json.RawMessage) string {
	var p queriesParams
	_ = json.Unmarshal(params, &p)
	if p.DB == "" {
		return cfg.Name() + "/stats"
	}
	return p.DB + "/stats"
}

// handleQueriesReset clears pg_stat_statements in one or every database.
// The reset function is superuser-only, so it runs as postgres.
func handleQueriesReset(ctx context.Context, env *Env) (Result, error) {
	var p queriesParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	specs, err := targetClusters(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	if !env.Req.DryRun && env.Req.Confirm == "" {
		return Result{}, confirmRequired("resetting pg_stat_statements loses the statement history", statsToken(env.Cluster, env.Req.Params))
	}
	var reset []string
	for _, spec := range specs {
		if env.Req.DryRun {
			reset = append(reset, spec.Name)
			continue
		}
		if err := steps.ClusterPG(spec).Exec(ctx, spec.Name, "SELECT pg_stat_statements_reset()", nil); err != nil {
			if p.DB != "" {
				return Result{}, statementsMissing(spec.Name)
			}
			continue
		}
		reset = append(reset, spec.Name)
	}
	return Result{Data: map[string]any{"reset": reset, "dry_run": env.Req.DryRun}}, nil
}
