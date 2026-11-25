package node

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/top"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/steps"
)

// statement is one row of vaire queries.
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
func statementsSQL(order string, limit int, allSessions bool) string {
	filter := ""
	if !allSessions {
		filter = "\n  AND NOT " + pg.VaireRoleSQL("r.rolname")
	}

	return `SELECT r.rolname AS role, s.queryid::text AS queryid, s.calls,
  round(s.total_exec_time::numeric, 1) AS total_ms, round(s.mean_exec_time::numeric, 2) AS mean_ms, s.rows,
  coalesce(round((100.0 * s.shared_blks_hit / nullif(s.shared_blks_hit + s.shared_blks_read, 0))::numeric, 1), 100) AS hit_pct,
  left(regexp_replace(s.query, '\s+', ' ', 'g'), 500) AS query
FROM pg_stat_statements s JOIN pg_roles r ON r.oid = s.userid
WHERE s.dbid = (SELECT oid FROM pg_database WHERE datname = current_database())` + filter + `
ORDER BY s.` + queryOrders[order] + ` DESC LIMIT ` + strconv.Itoa(limit)
}

const hasStatementsSQL = `SELECT count(*) AS n FROM pg_extension WHERE extname = 'pg_stat_statements'`

type queriesParams struct {
	DB    string `json:"db,omitempty"`
	Limit int    `json:"limit,omitempty"`
	Order string `json:"order,omitempty"`
	// AllSessions includes vaire's own sessions (its roles), which are left
	// out by default.
	AllSessions bool `json:"all_sessions,omitempty"`
	Sample      bool `json:"sample,omitempty"`
}

var queriesLocalQuery = func(ctx context.Context, spec config.ClusterSpec, sql string) ([]map[string]string, error) {
	return steps.ClusterPG(spec).Query(ctx, spec.DatabaseName(), sql, nil)
}

// queriesData is queries.top's result.
type queriesData struct {
	Order      string      `json:"order"`
	Statements []statement `json:"statements"`
	// Missing are the databases without pg_stat_statements.
	Missing []string `json:"missing,omitempty"`
}

// handleQueriesTop reads and merges statements from authorized databases.
func handleQueriesTop(ctx context.Context, env *Env) (Result, error) {
	var p queriesParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}

	if err := authorizeQueries(env, p); err != nil {
		return Result{}, err
	}

	if p.Sample {
		if !memberRead(env) {
			return Result{}, deviceAccessDenied()
		}

		return memberTopSamples(ctx, env, p.DB)
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

	cfg := env.Cluster
	if memberRead(env) {
		cfg = memberDatabases(env)
	}

	specs, err := targetClusters(cfg, p.DB)
	if err != nil {
		return Result{}, err
	}

	data, warnings := collectQueryStatements(ctx, env, specs, p)
	if len(data.Missing) > 0 {
		if p.DB != "" {
			return Result{}, statementsMissing(p.DB)
		}

		warnings = append(warnings, output.Warning{Code: "extension_missing", Message: "pg_stat_statements is not installed in " + fmt.Sprint(data.Missing)})
	}

	return Result{Data: data, Warnings: warnings}, nil
}

func collectQueryStatements(ctx context.Context, env *Env, specs []config.ClusterSpec, p queriesParams) (queriesData, []output.Warning) {
	data := queriesData{Order: p.Order, Statements: []statement{}}
	perDB := map[string][]statement{}

	var warnings []output.Warning

	for _, spec := range specs {
		rows, missing, err := queryDatabaseStatements(ctx, env, spec, p)
		if err != nil {
			message := spec.Name + ": " + err.Error()
			if memberRead(env) {
				message = spec.Name + ": diagnostic query failed"
			}

			warnings = append(warnings, output.Warning{Code: "database_unreachable", Message: message})

			continue
		}

		if missing {
			data.Missing = append(data.Missing, spec.Name)
			continue
		}

		perDB[spec.Name] = parseStatements(spec.Name, rows, p.AllSessions)
	}

	data.Statements = mergeStatements(perDB, p.Order, p.Limit)

	return data, warnings
}

func authorizeQueries(env *Env, p queriesParams) error {
	if !memberRead(env) {
		return nil
	}

	if p.AllSessions {
		return deviceAccessDenied()
	}

	if p.DB != "" && !memberOwns(env, p.DB) {
		return databaseNotFound(p.DB)
	}

	return nil
}

func queryDatabaseStatements(ctx context.Context, env *Env, spec config.ClusterSpec, p queriesParams) ([]map[string]string, bool, error) {
	query := func(sql string) ([]map[string]string, error) {
		if memberRead(env) {
			return queriesLocalQuery(ctx, spec, sql)
		}

		return steps.AdminPG(spec).Query(ctx, spec.DatabaseName(), sql, nil)
	}

	rows, err := query(hasStatementsSQL)
	if err != nil {
		return nil, false, err
	}

	if len(rows) == 0 || rows[0]["n"] == "0" {
		return nil, true, nil
	}

	rows, err = query(statementsSQL(p.Order, p.Limit, p.AllSessions))

	return rows, false, err
}

// memberTopSamples uses local postgres only after selecting owned clusters.
// It returns no pooler or host data, and never exposes cross-database blockers.
func memberTopSamples(ctx context.Context, env *Env, db string) (Result, error) {
	specs, err := targetClusters(memberDatabases(env), db)
	if err != nil {
		return Result{}, err
	}

	samples := map[string]top.Sample{}
	for _, spec := range specs {
		samples[spec.Name] = memberTopSample(ctx, spec)
	}

	return Result{Data: samples}, nil
}

func memberTopSample(ctx context.Context, spec config.ClusterSpec) top.Sample {
	s := top.Sample{DB: spec.Name, At: time.Now()}
	rows, err := queriesLocalQuery(ctx, spec, `SELECT (xact_commit + xact_rollback)::text AS xacts, blks_hit::text AS blks_hit, blks_read::text AS blks_read FROM pg_stat_database WHERE datname = current_database()`)

	if err != nil || len(rows) != 1 {
		s.Error = "diagnostic query failed"

		return s
	}

	s.Counters = top.ParseCounters(rows[0])

	rows, err = queriesLocalQuery(ctx, spec, top.ActivitySQL)
	if err != nil {
		s.Error = "diagnostic query failed"

		return s
	}

	s.Sessions = memberTopSessions(spec.Name, rows)
	rows, err = queriesLocalQuery(ctx, spec, top.HasStatementsSQL)

	if err == nil && len(rows) == 1 && rows[0]["n"] != "0" {
		if rows, err = queriesLocalQuery(ctx, spec, top.StatementsSQL(false)); err == nil {
			s.Statements = top.ParseStatements(rows)
		}
	}

	return s
}

func memberTopSessions(db string, rows []map[string]string) []top.Session {
	var sessions []top.Session

	visible := map[int]bool{}

	for _, session := range top.ParseSessions(db, rows) {
		if !pg.IsVaireSession(session.Role, session.App) {
			sessions = append(sessions, session)
			visible[session.PID] = true
		}
	}

	for i := range sessions {
		blocked := sessions[i].BlockedBy
		sessions[i].BlockedBy = nil

		for _, pid := range blocked {
			if visible[pid] {
				sessions[i].BlockedBy = append(sessions[i].BlockedBy, pid)
			}
		}
	}

	return sessions
}
func statementsMissing(db string) error {
	return output.NewError(output.CodePrecondition, "pg_stat_statements is not installed in "+db, output.ExitPrecondition,
		"Add it to the database's extensions (vaire config set clusters."+db+".extensions pg_stat_statements, then vaire apply --db "+db+"); vaire ext add arrives in Phase 7.").
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

// parseStatements reads the rows. Unless allSessions, it leaves out
// statements run by vaire's own roles (vaire queries, psql, maintenance, the
// exporters), so the application's statements stand out.
func parseStatements(db string, rows []map[string]string, allSessions bool) []statement {
	out := make([]statement, 0, len(rows))
	for _, r := range rows {
		if !allSessions && pg.IsVaireRole(r["role"]) {
			continue
		}

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
	if memberRead(env) {
		return Result{}, deviceAccessDenied()
	}

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

		if err := steps.ClusterPG(spec).Exec(ctx, spec.DatabaseName(), "SELECT pg_stat_statements_reset()", nil); err != nil {
			if p.DB != "" {
				return Result{}, statementsMissing(spec.Name)
			}

			continue
		}

		reset = append(reset, spec.Name)
	}

	return Result{Data: map[string]any{"reset": reset, "dry_run": env.Req.DryRun}}, nil
}
