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

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/maint"
	"github.com/mbvlabs/eru/internal/promfile"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/tune"
)

// MaintStatePath keeps the last maintenance results (wraparound ages, bloat
// totals) for eru-maint.prom. Regenerable; not part of the master backup.
const MaintStatePath = "/var/lib/eru/maint.json"

// maintRunTimeout bounds one vacuum, analyze or reindex statement.
const maintRunTimeout = 24 * time.Hour

// maintState is maint.json.
type maintState struct {
	LastDaily  string                      `json:"last_daily,omitempty"`
	LastWeekly string                      `json:"last_weekly,omitempty"`
	Wraparound map[string]map[string]int64 `json:"wraparound,omitempty"`
	Bloat      map[string]map[string]int64 `json:"bloat,omitempty"`
	Analyzed   map[string]int              `json:"analyzed,omitempty"`
}

func loadMaintState() maintState {
	st := maintState{}
	if raw, err := os.ReadFile(MaintStatePath); err == nil {
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
	return st
}

// saveMaintState writes maint.json and eru-maint.prom.
func saveMaintState(cfg config.Cluster, st maintState) {
	// A dropped database's numbers go.
	for _, m := range []map[string]map[string]int64{st.Wraparound, st.Bloat} {
		for db := range m {
			if _, ok := cfg.ClusterByName(db); !ok {
				delete(m, db)
			}
		}
	}
	if raw, err := json.MarshalIndent(st, "", "  "); err == nil {
		_ = engine.WriteFileAtomic(MaintStatePath, append(raw, '\n'), 0o600)
	}
	_ = promfile.Write(promfile.Maint, maintFamilies(st))
}

// maintFamilies are eru-maint.prom's families.
func maintFamilies(st maintState) []*promfile.Family {
	xid := promfile.Gauge("eru_wraparound_age", "Oldest transaction ID age per database (maint-daily).")
	mxid := promfile.Gauge("eru_mxid_age", "Oldest multixact ID age per database (maint-daily).")
	for _, db := range promfile.SortedKeys(st.Wraparound) {
		xid.Add(float64(st.Wraparound[db]["xid"]), promfile.L("db", db)...)
		mxid.Add(float64(st.Wraparound[db]["mxid"]), promfile.L("db", db)...)
	}
	bloat := promfile.Gauge("eru_bloat_estimated_bytes", "Estimated bloat per database and kind (maint-weekly, ioguix estimate).")
	for _, db := range promfile.SortedKeys(st.Bloat) {
		for _, kind := range promfile.SortedKeys(st.Bloat[db]) {
			bloat.Add(float64(st.Bloat[db][kind]), promfile.L("db", db, "kind", kind)...)
		}
	}
	analyzed := promfile.Gauge("eru_maint_analyzed_tables", "Tables maint-daily analyzed per database on its last run.")
	for _, db := range promfile.SortedKeys(st.Analyzed) {
		analyzed.Add(float64(st.Analyzed[db]), promfile.L("db", db)...)
	}
	last := promfile.Gauge("eru_maint_last_run_timestamp_seconds", "Last run of each maintenance timer job.")
	for job, at := range map[string]string{"maint-daily": st.LastDaily, "maint-weekly": st.LastWeekly} {
		if t, err := time.Parse(time.RFC3339, at); err == nil {
			last.Add(float64(t.Unix()), promfile.L("job", job)...)
		}
	}
	sort.SliceStable(last.Samples, func(i, j int) bool { return last.Samples[i].Labels[0].Value < last.Samples[j].Labels[0].Value })
	return []*promfile.Family{xid, mxid, bloat, analyzed, last}
}

// --- reports ---

type maintReportParams struct {
	Kind      string `json:"kind"`
	DB        string `json:"db,omitempty"`
	OlderThan string `json:"older_than,omitempty"`
	MinBytes  int64  `json:"min_bytes,omitempty"`
}

// maintReportDB is one database's report.
type maintReportDB struct {
	DB    string              `json:"db"`
	Rows  []map[string]string `json:"rows,omitempty"`
	Bloat *maint.BloatReport  `json:"bloat,omitempty"`
	Error string              `json:"error,omitempty"`
}

// handleMaintReport runs a read-only report in one or every database as
// eru_admin (Phase 4 D8). No lock: reports only read.
func handleMaintReport(ctx context.Context, env *Env) (Result, error) {
	var p maintReportParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if !maint.ValidReport(p.Kind) {
		return Result{}, output.NewError(output.CodeUsage, "unknown report "+p.Kind, output.ExitUsage,
			"Use one of "+strings.Join(maint.ReportKinds, ", ")+".")
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
		rows, err := steps.AdminPG(spec).Query(ctx, spec.Name, maint.ReportSetup+sql, nil)
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
	spec, err := clusterOf(env.Cluster, p.DB)
	if err != nil {
		return Result{}, err
	}
	if p.Kind == maint.Repack {
		return Result{}, repackRefusal(ctx, spec)
	}
	statements, _ := p.Statements()
	if env.Req.DryRun {
		data := map[string]any{"db": p.DB, "kind": p.Kind, "statements": statements, "dry_run": true, "heavy_io": p.Heavy()}
		if len(statements) == 0 {
			data["command"] = "vacuumdb " + strings.Join(p.VacuumdbArgs(steps.PGSocketDir, spec.Port, vacuumJobs(env.Cluster)), " ")
		}
		return Result{Data: data}, nil
	}
	return runJobRPC(ctx, env, "maint", p, nil)
}

// repackRefusal is maint repack until Phase 7 brings pg_repack (Phase 4).
func repackRefusal(ctx context.Context, spec config.ClusterSpec) error {
	rows, err := steps.AdminPG(spec).Query(ctx, spec.Name, maint.HasRepackSQL, nil)
	installed := err == nil && len(rows) == 1 && rows[0]["n"] != "0"
	reason, msg := "extension_missing", "pg_repack is not installed in "+spec.Name
	if installed {
		reason, msg = "repack_unavailable", "eru runs pg_repack from Phase 7 on"
	}
	return output.NewError(output.CodePrecondition, msg, output.ExitPrecondition,
		"pg_repack arrives with extensions in Phase 7: eru ext add pg_repack --db "+spec.Name+". Until then, eru maint vacuum reclaims space for reuse without shrinking the table.").
		WithDetails(map[string]any{"reason": reason, "extension": "pg_repack", "database": spec.Name})
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

// jobMaint runs one vacuum, analyze or reindex as eru_admin.
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
	res := map[string]any{"db": p.DB, "kind": p.Kind}
	statements, _ := p.Statements()
	local := steps.AdminPG(spec)
	local.Timeout = maintRunTimeout
	var runErr error
	if len(statements) == 0 {
		args := p.VacuumdbArgs(steps.PGSocketDir, spec.Port, vacuumJobs(env.Cluster))
		res["command"] = "vacuumdb " + strings.Join(args, " ")
		logLine(env.Report, env.Job, "vacuumdb "+strings.Join(args, " "))
		runErr = env.shell().Exec(ctx, env.Job, engine.Command{Name: "vacuumdb", Args: args, Env: map[string]string{"PGOPTIONS": "-c lock_timeout=5s"}}, env.Report)
	} else {
		res["statements"] = statements
		for _, stmt := range statements {
			logLine(env.Report, env.Job, stmt)
			if runErr = local.Exec(ctx, p.DB, maint.SessionSetup+stmt+";\n", nil); runErr != nil {
				break
			}
		}
	}
	if p.Kind == maint.Reindex {
		invalid, dropped := reindexLeftovers(ctx, local, p)
		if len(invalid) > 0 {
			res["invalid_indexes"] = invalid
			res["warnings"] = []output.Warning{{Code: "reindex_left_invalid",
				Message: "a concurrent reindex left invalid indexes in " + p.DB + ": " + strings.Join(invalid, ", ") + "; eru maint reindex --db " + p.DB + " --cleanup-invalid drops them"}}
		}
		if len(dropped) > 0 {
			res["dropped"] = dropped
		}
	}
	if runErr != nil {
		return res, output.WrapError(output.CodeExternalCommand, runErr, output.ExitExternal,
			"Read the job log (eru job follow). A lock timeout means a long transaction held the table; retry in a quieter moment.")
	}
	logLine(env.Report, env.Job, p.Kind+" of "+p.DB+" finished")
	return res, nil
}

// reindexLeftovers lists the invalid indexes a concurrent reindex left, and
// with --cleanup-invalid drops them.
func reindexLeftovers(ctx context.Context, local interface {
	Query(context.Context, string, string, map[string]string) ([]map[string]string, error)
	Exec(context.Context, string, string, map[string]string) error
}, p maint.Options) (invalid, dropped []string) {
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
			err = local.Exec(ctx, p.DB, maint.SessionSetup+stmt+";\n", nil)
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

// killToken is eru maint kill's confirm token: <db>/kill (Phase 4 D12).
func killToken(_ config.Cluster, params json.RawMessage) string {
	var p maintKillParams
	if json.Unmarshal(params, &p) != nil || p.DB == "" {
		return ""
	}
	return p.DB + "/kill"
}

// handleMaintKill terminates backends of one database as eru_admin, whose
// pg_signal_backend can't reach a superuser's. On a protected database
// eru_admin holds no pg_signal_backend (steps.AdminAccessSQL), and the
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
	opts := maint.KillOptions{PID: p.PID, IdleInTx: p.IdleInTx}
	if p.OlderThan != "" {
		d, err := time.ParseDuration(p.OlderThan)
		if err != nil || d < 0 {
			return Result{}, output.NewError(output.CodeUsage, "--older-than must be a duration such as 10m", output.ExitUsage, "")
		}
		opts.OlderThanSecond = int64(d.Seconds())
	}
	local := steps.AdminPG(spec)
	rows, err := local.Query(ctx, p.DB, maint.KillCandidatesSQL, nil)
	if err != nil {
		return Result{}, output.WrapError(output.CodeExternalCommand, err, output.ExitExternal, "")
	}
	selected, refused := maint.SelectBackends(maint.ParseBackends(rows), opts)
	if len(refused) > 0 {
		return Result{}, output.NewError(output.CodePrecondition, fmt.Sprintf("backend %d is a superuser's; eru maint kill never terminates it", refused[0].PID),
			output.ExitPrecondition, "Terminate it from a superuser session if you must (eru ssh).").
			WithDetails(map[string]any{"reason": "protected_backend", "pid": refused[0].PID})
	}
	if p.PID != 0 && len(selected) == 0 {
		return Result{}, output.NewError(output.CodePrecondition, fmt.Sprintf("no client backend %d in %s", p.PID, p.DB), output.ExitPrecondition, "eru top --db "+p.DB).
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
		killer, terminate = steps.ClusterPG(spec), "SET ROLE pg_signal_backend;\n"+terminate
	}
	out, err := killer.Query(ctx, p.DB, terminate, nil)
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
		if !clusterRunning(spec) {
			continue
		}
		local := steps.AdminPG(spec)
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
			if err := local.Exec(ctx, spec.Name, maint.SessionSetup+"ANALYZE "+q+";\n", nil); err != nil {
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

// runDeferredRestarts restarts the clusters db resize --defer-restart left
// for the window, one at a time with the pooler paused, under each
// cluster's lock exclusive. A busy cluster waits for the next night.
func runDeferredRestarts(ctx context.Context, env *jobEnv) ([]string, map[string]string) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil || len(st.DeferredRestarts) == 0 {
		return nil, nil
	}
	cfg := env.Cluster
	rt := jobRuntime(env)
	var done []string
	failed := map[string]string{}
	for _, db := range promfile.SortedKeys(st.DeferredRestarts) {
		if _, ok := cfg.ClusterByName(db); !ok {
			clearDeferred(db)
			continue
		}
		release, err := engine.AcquireCluster(ctx, db, engine.LockExclusive, env.Job, rpc.NewRequestID(), 0)
		if err != nil {
			failed[db] = "its lock is busy; the restart waits for tomorrow's window"
			logLine(env.Report, env.Job, db+": "+failed[db])
			continue
		}
		data, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{
			DB: db, Confirm: engine.Confirmation{Clusters: []string{db}}, Restarter: steps.Restarter(cfg), Report: env.Report,
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
		clearDeferred(db)
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

// jobMaintWeekly estimates every database's bloat for eru-maint.prom.
// auto_repack waits for pg_repack (Phase 7).
func jobMaintWeekly(ctx context.Context, env *jobEnv) (any, error) {
	cfg := env.Cluster
	st := loadMaintState()
	reports := map[string]maint.BloatReport{}
	var errs []error
	for _, spec := range cfg.Clusters {
		if !clusterRunning(spec) {
			continue
		}
		rows, err := steps.AdminPG(spec).Query(ctx, spec.Name, maint.ReportSetup+maint.BloatSQL(), nil)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", spec.Name, err))
			continue
		}
		r := maint.ParseBloat(spec.Name, rows, 0)
		reports[spec.Name] = r
		st.Bloat[spec.Name] = maint.TotalBloat(r)
	}
	if cfg.Maintenance.AutoRepack {
		logLine(env.Report, env.Job, "auto_repack is set, but pg_repack arrives with extensions in Phase 7; nothing was repacked")
	}
	st.LastWeekly = stamp()
	saveMaintState(cfg, st)
	res := map[string]any{"bloat": reports}
	return res, errors.Join(errs...)
}
