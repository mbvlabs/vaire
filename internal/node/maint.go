package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/maint"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/promfile"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
	"github.com/mbvlabs/vaire/internal/tune"
)

// MaintStatePath keeps the last maintenance results (wraparound ages, bloat
// totals) for vaire-maint.prom. Regenerable; not part of the master backup.
const MaintStatePath = "/var/lib/vaire/maint.json"

// maintStatePath is MaintStatePath for tests; production keeps the fixed path.
var maintStatePath = MaintStatePath

// maintRunTimeout bounds one vacuum, analyze or reindex statement.
const maintRunTimeout = 24 * time.Hour

// maintState is maint.json.
type maintState struct {
	LastDaily  string                        `json:"last_daily,omitempty"`
	LastWeekly string                        `json:"last_weekly,omitempty"`
	Wraparound map[string]map[string]int64   `json:"wraparound,omitempty"`
	Bloat      map[string]map[string]int64   `json:"bloat,omitempty"`
	Analyzed   map[string]int                `json:"analyzed,omitempty"`
	Repack     map[string]maint.RepackCounts `json:"repack,omitempty"`
}

func loadMaintState() maintState {
	st := maintState{}
	if raw, err := os.ReadFile(maintStatePath); err == nil {
		_ = json.Unmarshal(raw, &st)
	}
	if st.Wraparound == nil {
		st.Wraparound = map[string]map[string]int64{}
	}
	if st.Bloat == nil {
		st.Bloat = map[string]map[string]int64{}
	}
	if st.Analyzed == nil {
		st.Analyzed = map[string]int{}
	}
	if st.Repack == nil {
		st.Repack = map[string]maint.RepackCounts{}
	}
	return st
}

// saveMaintState writes maint.json and vaire-maint.prom.
func saveMaintState(cfg config.Cluster, st maintState) {
	// A dropped database's numbers go.
	for _, m := range []map[string]map[string]int64{st.Wraparound, st.Bloat} {
		for db := range m {
			if _, ok := cfg.ClusterByName(db); !ok {
				delete(m, db)
			}
		}
	}
	for db := range st.Repack {
		if _, ok := cfg.ClusterByName(db); !ok {
			delete(st.Repack, db)
		}
	}
	if raw, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = engine.WriteFileAtomic(maintStatePath, append(raw, '\n'), 0o600)
	}
	_ = promfile.Write(promfile.Maint, maintFamilies(st))
}

// maintFamilies are vaire-maint.prom's families.
func maintFamilies(st maintState) []*promfile.Family {
	xid := promfile.Gauge("vaire_wraparound_age", "Oldest transaction ID age per database (maint-daily).")
	mxid := promfile.Gauge("vaire_mxid_age", "Oldest multixact ID age per database (maint-daily).")
	for _, db := range promfile.SortedKeys(st.Wraparound) {
		xid.Add(float64(st.Wraparound[db]["xid"]), promfile.L("db", db)...)
		mxid.Add(float64(st.Wraparound[db]["mxid"]), promfile.L("db", db)...)
	}
	bloat := promfile.Gauge("vaire_bloat_estimated_bytes", "Estimated bloat per database and kind (maint-weekly, ioguix estimate).")
	for _, db := range promfile.SortedKeys(st.Bloat) {
		for _, kind := range promfile.SortedKeys(st.Bloat[db]) {
			bloat.Add(float64(st.Bloat[db][kind]), promfile.L("db", db, "kind", kind)...)
		}
	}
	analyzed := promfile.Gauge("vaire_maint_analyzed_tables", "Tables maint-daily analyzed per database on its last run.")
	for _, db := range promfile.SortedKeys(st.Analyzed) {
		analyzed.Add(float64(st.Analyzed[db]), promfile.L("db", db)...)
	}
	repacked := promfile.Gauge("vaire_maint_repack_tables", "Tables auto-repack considered per database on maint-weekly's last run, by outcome.")
	for _, db := range promfile.SortedKeys(st.Repack) {
		counts := st.Repack[db]
		for _, outcome := range []struct {
			name string
			n    int
		}{
			{maint.RepackOutcomeRepacked, counts.Repacked},
			{maint.RepackOutcomeFailed, counts.Failed},
			{maint.RepackOutcomeUnknown, counts.Unknown},
			{maint.RepackOutcomeSkipped, counts.Skipped},
			{maint.RepackOutcomeBusy, counts.Busy},
		} {
			repacked.Add(float64(outcome.n), promfile.L("db", db, "outcome", outcome.name)...)
		}
	}
	last := promfile.Gauge("vaire_maint_last_run_timestamp_seconds", "Last run of each maintenance timer job.")
	for job, at := range map[string]string{"maint-daily": st.LastDaily, "maint-weekly": st.LastWeekly} {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			last.Add(float64(t.Unix()), promfile.L("job", job)...)
		}
	}
	sort.SliceStable(last.Samples, func(i, j int) bool { return last.Samples[i].Labels[0].Value < last.Samples[j].Labels[0].Value })
	return []*promfile.Family{xid, mxid, bloat, analyzed, repacked, last}
}

// --- reports ---

type maintReportParams struct {
	Kind      string `json:"kind"`
	DB        string `json:"db,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
	MinBytes  int64  `json:"min_bytes,omitempty"`
	Table     string `json:"table,omitempty"`
	Exact     bool   `json:"exact,omitempty"`
}

// maintReportDB is one database's report.
type maintReportDB struct {
	DB    string              `json:"db"`
	Rows  []map[string]string `json:"rows,omitempty"`
	Bloat *maint.BloatReport  `json:"bloat,omitempty"`
	Exact *maint.TableStats   `json:"exact,omitempty"`
	Error string              `json:"error,omitempty"`
}

// maintReportAdminPG is the default read-only maintenance report connection:
// vaire_admin over the cluster socket, with pg_monitor and no superuser.
func maintReportAdminPG(spec config.ClusterSpec) sqlRunner { return steps.AdminPG(spec) }

// maintReportPG is the read-only connection a maintenance report uses. It is
// a seam so tests can prove the exact report's source database and read-only
// SQL without a PostgreSQL host.
var maintReportPG = maintReportAdminPG

// handleMaintReport runs a read-only report in one or every database as
// vaire_admin (Phase 4 D8). No lock: reports only read.
func handleMaintReport(ctx context.Context, env *Env) (Result, error) {
	var p maintReportParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !maint.ValidReport(p.Kind) {
		return Result{}, output.NewError(output.CodeUsage, "unknown report "+p.Kind, output.ExitUsage,
			"Use one of "+strings.Join(maint.ReportKinds, ", ")+".")
	}
	if p.Exact {
		return exactMaintReport(ctx, env, p)
	}
	if p.Table != "" {
		return Result{}, output.NewError(output.CodeUsage, "--table is only for maint report bloat --exact", output.ExitUsage, "")
	}
	older := 300
	if p.OlderThan != "" {
		d, err := time.ParseDuration(p.OlderThan)
		if err != nil || d < 0 {
			return Result{}, output.NewError(output.CodeUsage, "--older-than must be a duration such as 5m", output.ExitUsage, "")
		}
		older = int(d.Seconds())
	}
	specs, err := targetClusters(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	sql, _ := maint.ReportSQL(p.Kind, older)
	out := []maintReportDB{}
	for _, spec := range specs {
		r := maintReportDB{DB: spec.Name}
		if !clusterRunning(spec) {
			r.Error = "the cluster is not running"
			out = append(out, r)
			continue
		}
		rows, err := maintReportPG(spec).Query(ctx, spec.DatabaseName(), maint.ReportSetup+sql, nil)
		switch {
		case err != nil:
			r.Error = err.Error()
		case p.Kind == maint.ReportBloat:
			b := maint.ParseBloat(spec.Name, rows, p.MinBytes)
			r.Bloat = &b
		default:
			if p.Kind == maint.ReportWraparound {
				for _, row := range rows {
					row["status"] = maint.WraparoundStatus(atoi64(row["xid_age"]))
				}
			}
			r.Rows = rows
		}
		out = append(out, r)
	}
	return Result{Data: map[string]any{"kind": p.Kind, "databases": out}}, nil
}

// exactMaintReport measures exactly one ordinary table with the full
// pgstattuple scan. It is read-only, uses the existing report permissions and
// timeouts, and never falls back to pgstattuple_approx or the superuser.
func exactMaintReport(ctx context.Context, env *Env, p maintReportParams) (Result, error) {
	if p.Kind != maint.ReportBloat || p.DB == "" || p.Table == "" {
		return Result{}, output.NewError(output.CodeUsage,
			"--exact is for the bloat report of one table: --db and --table", output.ExitUsage, "")
	}
	spec, err := clusterOf(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	if !clusterRunning(spec) {
		return Result{}, repackNotRunning(spec.Name)
	}
	stats, err := exactTableStats(ctx, spec, p.Table)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"kind": p.Kind, "databases": []maintReportDB{{DB: spec.Name, Exact: &stats}}}}, nil
}

func exactTableStats(ctx context.Context, spec config.ClusterSpec, table string) (maint.TableStats, error) {
	q, err := maint.QuoteRelation(table)
	if err != nil {
		return maint.TableStats{}, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	local := maintReportPG(spec)
	rows, err := local.Query(ctx, spec.DatabaseName(), maint.PgstattupleInstalledSQL, nil)
	if err != nil {
		return maint.TableStats{}, exactBloatFailed(spec.Name, table, "read pgstattuple's catalog facts", err)
	}
	if len(rows) != 1 {
		return maint.TableStats{}, repackExtensionMissing(spec.Name, "pgstattuple",
			"--exact measures the table with pgstattuple, which needs its extension in the database")
	}
	installed := rows[0]["extversion"]
	rows, err = local.Query(ctx, spec.DatabaseName(), maint.PgstattupleSchemaSQL, nil)
	if err != nil {
		return maint.TableStats{}, exactBloatFailed(spec.Name, table, "read pgstattuple's function schema", err)
	}
	if len(rows) != 1 || rows[0]["schema"] == "" {
		return maint.TableStats{}, output.NewError(output.CodePrecondition,
			"pgstattuple is installed in "+spec.Name+" at "+installed+", but its function schema is unavailable", output.ExitPrecondition,
			"Recreate the extension from the matching PostgreSQL 18 package; vaire does not guess a function schema.").
			WithDetails(map[string]any{"reason": maint.RepackReasonExactFailed, "database": spec.Name, "table": table, "extension_version": installed})
	}
	schema := rows[0]["schema"]
	rows, err = local.Query(ctx, spec.DatabaseName(), maint.RelationFactsSQL(q), nil)
	if err != nil {
		return maint.TableStats{}, exactBloatFailed(spec.Name, table, "read the relation's kind and storage", err)
	}
	if len(rows) == 0 {
		return maint.TableStats{}, output.NewError(output.CodePrecondition,
			table+" does not exist in "+spec.Name, output.ExitPrecondition,
			"Check the table name with vaire sql --db "+spec.Name+" '\\dt'.").
			WithDetails(map[string]any{"reason": maint.RepackReasonRelationNotFound, "database": spec.Name, "table": table})
	}
	facts := maint.ParseRelationFacts(rows[0])
	if reason, message, unsupported := facts.UnsupportedOrdinaryTable(); unsupported {
		return maint.TableStats{}, output.NewError(output.CodePrecondition, message, output.ExitPrecondition,
			"Measure one ordinary permanent heap table; partition leaves and inheritance children are supported.").
			WithDetails(map[string]any{"reason": reason, "database": spec.Name, "table": table})
	}
	sql, err := maint.ExactBloatSQL(table, schema)
	if err != nil {
		return maint.TableStats{}, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	rows, err = local.Query(ctx, spec.DatabaseName(), maint.ReportSetup+sql, nil)
	if err != nil {
		return maint.TableStats{}, exactBloatFailed(spec.Name, table, "run the full pgstattuple scan", err)
	}
	stats, err := maint.ParseTableStats(spec.Name, facts.Schema, facts.Table, rows)
	if err != nil {
		return maint.TableStats{}, exactBloatFailed(spec.Name, table, "read the pgstattuple result", err)
	}
	return stats, nil
}

func exactBloatFailed(db, table, what string, cause error) error {
	return output.NewError(output.CodePrecondition,
		"measuring "+table+" in "+db+" failed: "+cause.Error(), output.ExitPrecondition,
		"The full pgstattuple scan needs vaire_admin's pg_monitor (pg_stat_scan_tables) and must finish within the report's 60s statement timeout and 5s lock timeout; vaire does not escalate to the superuser.").
		WithDetails(map[string]any{"reason": maint.RepackReasonExactFailed, "database": db, "table": table, "step": what, "error": cause.Error()})
}

func atoi64(s string) int64 {
	var n int64
	_, _ = fmt.Sscan(strings.TrimSpace(s), &n)
	return n
}

// --- vacuum, analyze, reindex, repack ---

// handleMaintRun checks a maintenance run and starts it as a detachable job
// (vacuum and reindex can outlive the SSH session).
func handleMaintRun(ctx context.Context, env *Env) (Result, error) {
	var p maint.Options
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := p.Validate(); err != nil {
		return Result{}, output.WrapError(output.CodeUsage, err, output.ExitUsage, "")
	}
	cfg := env.Cluster
	if p.Kind == maint.Repack {
		fresh, err := reloadJobCluster()
		if err != nil {
			return Result{}, err
		}
		cfg = fresh
	}
	spec, err := clusterOf(cfg, p.DB)
	if err != nil {
		return Result{}, err
	}
	if p.Kind == maint.Repack {
		if !env.Req.DryRun {
			if err := refuseAnyActivationFence(cfg, spec.Name); err != nil {
				return Result{}, err
			}
		}
		plan, err := planRepack(ctx, cfg, spec, &p, time.Now(), false)
		if err != nil {
			return Result{}, err
		}
		if env.Req.DryRun {
			return Result{Data: map[string]any{
				"db": p.DB, "kind": p.Kind, "tables": plan.Tables, "skipped": plan.Skipped,
				"versions": plan.Versions, "version_check": plan.VersionCheck, "space": plan.Space,
				"dry_run": true, "heavy_io": true,
			}}, nil
		}
		if env.Req.Confirm == "" {
			return Result{}, confirmRequired("a repack rewrites "+p.DB+"'s tables and their indexes", p.DB)
		}
		env.Cluster = cfg
		return runJobRPC(ctx, env, "maint", p, nil)
	}
	if !env.Req.DryRun {
		if err := refuseAnyActivationFence(env.Cluster, spec.Name); err != nil {
			return Result{}, err
		}
	}
	statements, _ := p.Statements()
	if env.Req.DryRun {
		data := map[string]any{"db": p.DB, "kind": p.Kind, "statements": statements, "dry_run": true, "heavy_io": p.Heavy()}
		if len(statements) == 0 {
			data["command"] = "vacuumdb " + strings.Join(vacuumdbArgs(spec, p, vacuumJobs(env.Cluster)), " ")
		}
		return Result{Data: data}, nil
	}
	return runJobRPC(ctx, env, "maint", p, nil)
}

// vacuumJobs is vacuumdb's --jobs: the cluster's CPU share, at most 4.
func vacuumJobs(cfg config.Cluster) int {
	host, _ := hostinfo.Detect(cfg.Server.DataRoot)
	share := tune.CPUShare(host.VCPU, len(cfg.Clusters))
	if share > 4 {
		share = 4
	}
	if share < 1 {
		share = 1
	}
	return share
}

// maintPG is how maintenance reaches spec's database, with the SQL that
// starts each of its sessions: as vaire_admin, with its pg_maintain. On a
// protected database vaire_admin holds no pg_maintain, which would let it
// LOCK TABLE the application's tables without a code
// (steps.AdminAccessSQL): there the superuser runs it with SET ROLE
// pg_maintain, which may vacuum, analyze and reindex every table and write
// to none.
func maintPG(spec config.ClusterSpec) (pg.Local, string) {
	if spec.Protected {
		return steps.ClusterPG(spec), "SET ROLE pg_maintain;\n"
	}
	return steps.AdminPG(spec), ""
}

// vacuumdbArgs are vacuumdb's arguments for p on spec's cluster: as
// vaire_admin, or on a protected database as postgres, whose sessions
// vacuumdbCommand starts with role pg_maintain (maintPG).
func vacuumdbArgs(spec config.ClusterSpec, p maint.Options, jobs int) []string {
	user := "vaire_admin"
	if spec.Protected {
		user = "postgres"
	}
	return p.VacuumdbArgs(steps.PGSocketDir, user, spec.Port, jobs)
}

// vacuumdbCommand runs vacuumdb with args on spec's cluster. root logs in
// as vaire_admin (pg_ident); on a protected database it runs as the postgres
// OS user, and every one of its sessions starts as role pg_maintain.
func vacuumdbCommand(spec config.ClusterSpec, args []string) engine.Command {
	if spec.Protected {
		return engine.Command{Name: "runuser", Args: append([]string{"-u", "postgres", "--", "vacuumdb"}, args...),
			Env: map[string]string{"PGOPTIONS": "-c lock_timeout=5s -c role=pg_maintain"}}
	}
	return engine.Command{Name: "vacuumdb", Args: args, Env: map[string]string{"PGOPTIONS": "-c lock_timeout=5s"}}
}

// maintHeavy reports whether a maint job takes the heavy-I/O lock.
func maintHeavy(env *jobEnv) bool {
	if env.Job != "maint" {
		return false
	}
	var p maint.Options
	if json.Unmarshal(env.Params, &p) != nil {
		return false
	}
	return p.Heavy()
}

// jobMaint runs one vacuum, analyze or reindex as vaire_admin, or on a
// protected database as role pg_maintain (maintPG).
func jobMaint(ctx context.Context, env *jobEnv) (any, error) {
	var p maint.Options
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	spec, err := clusterOf(env.Cluster, p.DB)
	if err != nil {
		return nil, err
	}
	if p.Kind == maint.Repack {
		freshCfg, freshSpec, err := reloadRepackTarget(spec)
		if err != nil {
			return nil, err
		}
		env.Cluster = freshCfg
		if err := refuseAnyActivationFence(freshCfg, freshSpec.Name); err != nil {
			return nil, err
		}
		if freshSpec.Protected {
			return nil, ProtectedRepackError(freshSpec.Name)
		}
		if freshSpec.IsFollower() {
			return nil, followerError(freshSpec, "maintenance")
		}
		return jobRepack(ctx, env, p, freshSpec)
	}
	if err := refuseAnyActivationFence(env.Cluster, spec.Name); err != nil {
		return nil, err
	}
	res := map[string]any{"db": p.DB, "kind": p.Kind}
	statements, _ := p.Statements()
	local, setup := maintPG(spec)
	local.Timeout = maintRunTimeout
	var runErr error
	if len(statements) == 0 {
		args := vacuumdbArgs(spec, p, vacuumJobs(env.Cluster))
		res["command"] = "vacuumdb " + strings.Join(args, " ")
		logLine(env.Report, env.Job, "vacuumdb "+strings.Join(args, " "))
		runErr = env.shell().Exec(ctx, env.Job, vacuumdbCommand(spec, args), env.Report)
	} else {
		res["statements"] = statements
		for _, stmt := range statements {
			logLine(env.Report, env.Job, stmt)
			if runErr = local.Exec(ctx, p.DB, setup+maint.SessionSetup+stmt+";\n", nil); runErr != nil {
				break
			}
		}
	}
	if p.Kind == maint.Reindex {
		invalid, dropped := reindexLeftovers(ctx, local, leftoverDropSetup(spec), p)
		if len(invalid) > 0 {
			res["invalid_indexes"] = invalid
			res["warnings"] = []output.Warning{{Code: "reindex_left_invalid",
				Message: "a concurrent reindex left invalid indexes in " + p.DB + ": " + strings.Join(invalid, ", ") + "; vaire maint reindex --db " + p.DB + " --cleanup-invalid drops them"}}
		}
		if len(dropped) > 0 {
			res["dropped"] = dropped
		}
	}
	if runErr != nil {
		return res, output.WrapError(output.CodeExternalCommand, runErr, output.ExitExternal,
			"Read the job log (vaire job follow). A lock timeout means a long transaction held the table; retry in a quieter moment.")
	}
	logLine(env.Report, env.Job, p.Kind+" of "+p.DB+" finished")
	return res, nil
}

// leftoverDropSetup starts the session that drops a concurrent reindex's
// leftover index, which only the table's owner may do: vaire_admin is a
// member of <db>_owner, and on a protected database, where it is not, the
// superuser acts as <db>_owner.
func leftoverDropSetup(spec config.ClusterSpec) string {
	if spec.Protected {
		return "SET ROLE " + pg.Quote(spec.Name+"_owner") + ";\n"
	}
	return ""
}

// reindexLeftovers lists the invalid indexes a concurrent reindex left, and
// with --cleanup-invalid drops them, each session starting with setup.
func reindexLeftovers(ctx context.Context, local interface {
	Query(context.Context, string, string, map[string]string) ([]map[string]string, error)
	Exec(context.Context, string, string, map[string]string) error
}, setup string, p maint.Options) (invalid, dropped []string) {
	rows, err := local.Query(ctx, p.DB, maint.InvalidIndexesSQL, nil)
	if err != nil {
		return nil, nil
	}
	for _, r := range rows {
		invalid = append(invalid, r["name"])
	}
	if !p.CleanupInvalid {
		return invalid, nil
	}
	var left []string
	for _, name := range invalid {
		stmt, err := maint.DropInvalidSQL(name)
		if err == nil {
			err = local.Exec(ctx, p.DB, setup+maint.SessionSetup+stmt+";\n", nil)
		}
		if err != nil {
			left = append(left, name)
			continue
		}
		dropped = append(dropped, name)
	}
	return left, dropped
}

// --- kill ---

type maintKillParams struct {
	DB        string `json:"db"`
	PID       int    `json:"pid,omitempty"`
	IdleInTx  bool   `json:"idle_in_tx,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
}

// maintKillAdminPG and maintKillClusterPG are handleMaintKill's connections
// to the database's cluster: vaire_admin's, and the superuser's for a
// protected database. Variables so a test sees which database each query
// names.
var (
	maintKillAdminPG   = func(spec config.ClusterSpec) sqlRunner { return steps.AdminPG(spec) }
	maintKillClusterPG = func(spec config.ClusterSpec) sqlRunner { return steps.ClusterPG(spec) }
)

// killToken is vaire maint kill's confirm token: <db>/kill (Phase 4 D12).
func killToken(_ config.Cluster, params json.RawMessage) string {
	var p maintKillParams
	if json.Unmarshal(params, &p) != nil || p.DB == "" {
		return ""
	}
	return p.DB + "/kill"
}

// handleMaintKill terminates backends of one database as vaire_admin, whose
// pg_signal_backend can't reach a superuser's. On a protected database
// vaire_admin holds no pg_signal_backend (steps.AdminAccessSQL), and the
// request carries a code from the authenticator app (requireWriteCode): it
// terminates as the superuser with SET ROLE pg_signal_backend, which can't
// reach a superuser's backend either.
func handleMaintKill(ctx context.Context, env *Env) (Result, error) {
	var p maintKillParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if p.DB == "" || (p.PID == 0) == !p.IdleInTx {
		return Result{}, output.NewError(output.CodeUsage, "maint kill needs --db and either --pid or --idle-in-tx", output.ExitUsage, "")
	}
	spec, err := clusterOf(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	if !env.Req.DryRun {
		if err := refuseAnyActivationFence(env.Cluster, spec.Name); err != nil {
			return Result{}, err
		}
	}
	opts := maint.KillOptions{PID: p.PID, IdleInTx: p.IdleInTx}
	if p.OlderThan != "" {
		d, err := time.ParseDuration(p.OlderThan)
		if err != nil || d < 0 {
			return Result{}, output.NewError(output.CodeUsage, "--older-than must be a duration such as 10m", output.ExitUsage, "")
		}
		opts.OlderThanSecond = int64(d.Seconds())
	}
	// The cluster holds spec.DatabaseName(), which is its source's name for a
	// follower: p.DB only names the cluster, in the token and the output.
	local := maintKillAdminPG(spec)
	rows, err := local.Query(ctx, spec.DatabaseName(), maint.KillCandidatesSQL, nil)
	if err != nil {
		return Result{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	selected, refused := maint.SelectBackends(maint.ParseBackends(rows), opts)
	if len(refused) > 0 {
		return Result{}, output.NewError(output.CodePrecondition, fmt.Sprintf("backend %d is a superuser's; vaire maint kill never terminates it", refused[0].PID),
			output.ExitPrecondition, "Terminate it from a superuser session if you must (vaire ssh).").
			WithDetails(map[string]any{"reason": "protected_backend", "pid": refused[0].PID})
	}
	if p.PID != 0 && len(selected) == 0 {
		return Result{}, output.NewError(output.CodePrecondition, fmt.Sprintf("no client backend %d in %s", p.PID, p.DB), output.ExitPrecondition, "vaire top --db "+p.DB).
			WithDetails(map[string]any{"reason": "backend_not_found", "pid": p.PID})
	}
	data := map[string]any{"db": p.DB, "targets": selected, "dry_run": env.Req.DryRun}
	if env.Req.DryRun || len(selected) == 0 {
		return Result{Data: data}, nil
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("terminating backends of "+p.DB+" rolls back their transactions", p.DB+"/kill")
	}
	pids := make([]int, 0, len(selected))
	for _, b := range selected {
		pids = append(pids, b.PID)
	}
	killer, terminate := local, maint.TerminateSQL(pids)
	if spec.Protected {
		killer, terminate = maintKillClusterPG(spec), "SET ROLE pg_signal_backend;\n"+terminate
	}
	out, err := killer.Query(ctx, spec.DatabaseName(), terminate, nil)
	if err != nil {
		return Result{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	data["terminated"] = out
	return Result{Data: data}, nil
}

// --- timers ---

// windowLeft reports whether now (local time) is inside the maintenance
// window and how much is left. A run outside the window (by hand) gets the
// whole window's length.
func windowLeft(cfg config.Cluster, now time.Time) (bool, time.Duration) {
	start, end, err := config.ParseWindow(cfg.Maintenance.Window)
	if err != nil {
		return false, 3 * time.Hour
	}
	in, left := config.InWindow(now, start, end)
	if !in {
		return false, config.WindowLength(start, end)
	}
	return true, left
}

// jobMaintDaily runs at the window's start: per database the wraparound
// ages and an ANALYZE of stale tables, stopping at the window's end, then
// the restarts db resize --defer-restart left for the window (Phase 4 D11).
func jobMaintDaily(ctx context.Context, env *jobEnv) (any, error) {
	cfg := env.Cluster
	_, left := windowLeft(cfg, time.Now())
	deadline := time.Now().Add(left)
	st := loadMaintState()
	res := map[string]any{}
	analyzed := map[string][]string{}
	for _, spec := range cfg.Clusters {
		// A follower is read-only: its source's ANALYZE replicates (Phase 6).
		if !clusterRunning(spec) || spec.IsFollower() {
			continue
		}
		if err := refuseAnyActivationFence(cfg, spec.Name); err != nil {
			logLine(env.Report, env.Job, "skipping maintenance of "+spec.Name+": "+err.Error())
			continue
		}
		local, setup := maintPG(spec)
		if rows, err := local.Query(ctx, spec.Name, "SELECT max(age(datfrozenxid))::bigint AS xid, max(mxid_age(datminmxid))::bigint AS mxid FROM pg_database", nil); err == nil && len(rows) == 1 {
			st.Wraparound[spec.Name] = map[string]int64{"xid": atoi64(rows[0]["xid"]), "mxid": atoi64(rows[0]["mxid"])}
		}
		rows, err := local.Query(ctx, spec.Name, maint.StaleTablesSQL, nil)
		if err != nil {
			logLine(env.Report, env.Job, "reading stale tables in "+spec.Name+" failed: "+err.Error())
			continue
		}
		local.Timeout = time.Hour
		st.Analyzed[spec.Name] = 0
		for _, r := range rows {
			if time.Now().After(deadline) {
				logLine(env.Report, env.Job, "the maintenance window ended; the rest waits for tomorrow")
				break
			}
			q, err := maint.QuoteRelation(r["name"])
			if err != nil {
				continue
			}
			if err := local.Exec(ctx, spec.Name, setup+maint.SessionSetup+"ANALYZE "+q+";\n", nil); err != nil {
				logLine(env.Report, env.Job, "ANALYZE "+r["name"]+" in "+spec.Name+" failed: "+err.Error())
				continue
			}
			analyzed[spec.Name] = append(analyzed[spec.Name], r["name"])
			st.Analyzed[spec.Name]++
		}
	}
	res["analyzed"] = analyzed
	res["restarted"], res["restart_errors"] = runDeferredRestarts(ctx, env)
	st.LastDaily = stamp()
	saveMaintState(cfg, st)
	return res, nil
}

// Deferred-restart host seams. Production uses the node's state file and the
// real engine; a test can prove a refused narrow transition never reaches
// configuration, restart or deferred-state clearing.
var (
	deferredLoadState      = engine.LoadState
	deferredApply          = engine.Apply
	deferredClear          = clearDeferred
	deferredAcquireCluster = engine.AcquireCluster
)

// runDeferredRestarts restarts the clusters db resize --defer-restart left
// for the window, one at a time with the pooler paused, under each
// cluster's lock exclusive. A busy cluster waits for the next night.
func runDeferredRestarts(ctx context.Context, env *jobEnv) ([]string, map[string]string) {
	st, err := deferredLoadState(engine.StatePath)
	if err != nil || len(st.DeferredRestarts) == 0 {
		return nil, nil
	}
	cfg := env.Cluster
	rt := jobRuntime(env)
	var done []string
	failed := map[string]string{}
	for _, db := range promfile.SortedKeys(st.DeferredRestarts) {
		if _, ok := cfg.ClusterByName(db); !ok {
			deferredClear(db)
			continue
		}
		if err := refuseAnyActivationFence(cfg, db); err != nil {
			failed[db] = err.Error()
			logLine(env.Report, env.Job, db+": the restart waits: "+failed[db])
			continue
		}
		release, err := deferredAcquireCluster(ctx, db, engine.LockExclusive, env.Job, rpc.NewRequestID(), 0)
		if err != nil {
			failed[db] = "its lock is busy; the restart waits for tomorrow's window"
			logLine(env.Report, env.Job, db+": "+failed[db])
			continue
		}
		if err := refuseNarrowExtensionTransition(ctx, cfg, db); err != nil {
			release()
			failed[db] = err.Error()
			logLine(env.Report, env.Job, db+": the restart waits: "+failed[db])
			continue
		}
		data, _, err := deferredApply(ctx, steps.Catalog(), cfg, rt, engine.Options{
			DB: db, Confirm: engine.Confirmation{Clusters: []string{db}}, Restarter: steps.RestarterFor(cfg, rt, env.Report), Report: env.Report,
		})
		release()
		if err != nil {
			failed[db] = err.Error()
			logLine(env.Report, env.Job, "restarting "+db+" failed: "+err.Error())
			continue
		}
		for _, r := range data.Restarts {
			logLine(env.Report, env.Job, fmt.Sprintf("restarted %s (%d ms down)", r.Cluster, r.DowntimeMS))
		}
		deferredClear(db)
		done = append(done, db)
	}
	return done, failed
}

// recordDeferred notes that db's restart waits for the window.
func recordDeferred(db string) {
	_ = engine.UpdateState(func(st *engine.State) error {
		if st.DeferredRestarts == nil {
			st.DeferredRestarts = map[string]string{}
		}
		st.DeferredRestarts[db] = stamp()
		return nil
	})
}

// clearDeferred drops db's deferred restart.
func clearDeferred(dbs ...string) {
	_ = engine.UpdateState(func(st *engine.State) error {
		for _, db := range dbs {
			delete(st.DeferredRestarts, db)
		}
		return nil
	})
}

// jobMaintWeekly estimates every database's bloat for vaire-maint.prom and,
// when auto_repack is enabled, repacks eligible tables one at a time.
func jobMaintWeekly(ctx context.Context, env *jobEnv) (any, error) {
	cfg, err := reloadJobCluster()
	if err != nil {
		return nil, err
	}
	env.Cluster = cfg
	st := loadMaintState()
	reports := map[string]maint.BloatReport{}
	var errs []error
	for _, spec := range cfg.Clusters {
		// A follower's tables are its source's (Phase 6).
		if !clusterRunning(spec) || spec.IsFollower() {
			continue
		}
		rows, err := maintReportPG(spec).Query(ctx, spec.Name, maint.ReportSetup+maint.BloatSQL(), nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		r := maint.ParseBloat(spec.Name, rows, 0)
		reports[spec.Name] = r
		st.Bloat[spec.Name] = maint.TotalBloat(r)
	}
	res := map[string]any{"bloat": reports}
	if cfg.Maintenance.AutoRepack {
		runs := autoRepack(ctx, env, reports, autoRepackNow())
		st.Repack = make(map[string]maint.RepackCounts, len(runs))
		for db, run := range runs {
			st.Repack[db] = run.Counts
		}
		res["repack"] = runs
	} else {
		st.Repack = nil
	}
	st.LastWeekly = stamp()
	saveMaintState(cfg, st)
	return res, errors.Join(errs...)
}
