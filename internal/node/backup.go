package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/steps"
)

// driver is the pgBackRest driver for spec's stanza.
func driver(spec config.ClusterSpec, stepID string, report engine.Reporter) backup.PgBackRest {
	return steps.BackupDriver(spec, engine.Shell{Secrets: secrets.Default}, stepID, report)
}

// repoDriver is a driver bound to no stanza, for the commands that read or
// change the repository as a whole (pgbackrest info over every stanza) or a
// stanza with no cluster.
func repoDriver(stepID string, report engine.Reporter) backup.PgBackRest {
	return driver(config.ClusterSpec{}, stepID, report)
}

// firstCluster is the cluster the drill's RPC still covers without a
// database: the first. The drill becomes per database in unit 32.
func firstCluster(cfg config.Cluster) config.ClusterSpec {
	if len(cfg.Clusters) == 0 {
		return config.ClusterSpec{}
	}
	return cfg.Clusters[0]
}

// requireBackups refuses a backup method on a cluster without a bucket.
func requireBackups(cfg config.Cluster) error {
	if cfg.BackupConfigured() {
		return nil
	}
	return output.NewError(output.CodePrecondition, "backups are not configured on this cluster", output.ExitPrecondition,
		"Run eru backup setup first.").WithDetails(map[string]any{"reason": "backup_not_configured"})
}

// oneDatabase is the usage error of a backup method that runs one job: the
// CLI calls it once per database, one after another, because every job takes
// the heavy-I/O lock.
func oneDatabase(method string) error {
	return output.NewError(output.CodeUsage, method+" runs for one database: name it in params.database", output.ExitUsage,
		"eru backup now and eru backup verify without --db call it once per database.")
}

type backupNowParams struct {
	Type     string `json:"type"`
	Database string `json:"database"`
}

// backupPlan is one database's backup in backup.now's dry run.
type backupPlan struct {
	Database string `json:"database"`
	Type     string `json:"type"`
}

// handleBackupNow takes one backup of params.database. A dry run without a
// database plans one per cluster. Without a type, a stanza with a full gets
// an incremental and one without gets a full.
func handleBackupNow(ctx context.Context, env *Env) (Result, error) {
	var p backupNowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	switch p.Type {
	case "", backup.TypeFull, backup.TypeDiff, backup.TypeIncr:
	default:
		return Result{}, output.NewError(output.CodeUsage, "type must be full, diff or incr", output.ExitUsage, "")
	}
	specs := env.Cluster.Clusters
	if p.Database != "" {
		spec, ok := env.Cluster.ClusterByName(p.Database)
		if !ok {
			return Result{}, databaseNotFound(p.Database)
		}
		specs = []config.ClusterSpec{spec}
	} else if !env.Req.DryRun {
		return Result{}, oneDatabase("backup.now")
	}
	var infos map[string]backup.Info
	if p.Type == "" && len(specs) > 0 {
		var err error
		if infos, err = repoDriver("backup.now", nil).InfoAll(ctx); err != nil {
			return Result{}, err
		}
	}
	plan := make([]backupPlan, 0, len(specs))
	for _, spec := range specs {
		kind := p.Type
		if kind == "" {
			kind = backup.TypeIncr
			if !infos[spec.Stanza].HasFull() {
				kind = backup.TypeFull
			}
		}
		plan = append(plan, backupPlan{Database: spec.Name, Type: kind})
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"backups": plan, "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "backup-"+plan[0].Type, map[string]string{"database": plan[0].Database}, nil)
}

// spec is the cluster of a template-unit job; RunJob has checked it.
func (j *jobEnv) spec() config.ClusterSpec {
	spec, _ := j.Cluster.ClusterByName(j.DB)
	return spec
}

// jobBackup takes one backup of the job's database. A backup another process
// holds pgBackRest's lock for is skipped, not failed.
func jobBackup(ctx context.Context, env *jobEnv, kind string) (any, error) {
	if err := requireBackups(env.Cluster); err != nil {
		return nil, err
	}
	d := driver(env.spec(), env.Job, env.Report)
	info, err := d.Info(ctx, "")
	if err != nil {
		return nil, err
	}
	if info.BackupLockHeld {
		logLine(env.Report, env.Job, "another pgBackRest backup is running; skipping")
		return map[string]any{"skipped": "backup_running"}, fmt.Errorf("%w: another pgBackRest backup is running", errSkipped)
	}
	if kind != backup.TypeFull && !info.HasFull() {
		logLine(env.Report, env.Job, "the repository has no full backup yet; taking a full backup instead")
		kind = backup.TypeFull
	}
	set, err := d.Backup(ctx, kind)
	if err != nil {
		return nil, err
	}
	steps.RecordBackup(env.Cluster, env.DB, set)
	logLine(env.Report, env.Job, fmt.Sprintf("backup %s (%s) of %s finished: %s", set.Label, set.Type, env.DB, humanSize(set.Size)))
	return map[string]any{"db": env.DB, "set": set}, nil
}

type backupVerifyParams struct {
	Database string `json:"database"`
}

// handleBackupVerify verifies params.database's stanza.
func handleBackupVerify(ctx context.Context, env *Env) (Result, error) {
	var p backupVerifyParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	if p.Database == "" {
		return Result{}, oneDatabase("backup.verify")
	}
	if _, ok := env.Cluster.ClusterByName(p.Database); !ok {
		return Result{}, databaseNotFound(p.Database)
	}
	return runJobRPC(ctx, env, "backup-verify", p, nil)
}

func jobVerify(ctx context.Context, env *jobEnv) (any, error) {
	if err := requireBackups(env.Cluster); err != nil {
		return nil, err
	}
	return verify(ctx, env.Cluster, env.spec(), env.Job, env.Report)
}

// verify runs pgbackrest verify on spec's stanza and records the result. A
// verify that finds problems is recorded and then returned as
// external_command_failed.
func verify(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, stepID string, report engine.Reporter) (backup.VerifyResult, error) {
	res, err := driver(spec, stepID, report).Verify(ctx)
	if err != nil {
		return res, err
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.LastVerify[spec.Name] = res
		return nil
	})
	steps.WriteMetrics(cfg)
	if !res.OK {
		return res, output.NewError(output.CodeExternalCommand, "pgbackrest verify found problems in the stanza of "+spec.Name, output.ExitExternal,
			"Read details.output, then take a new full backup with eru backup now --db "+spec.Name+" --type full.").
			WithDetails(map[string]any{"output": res.Output, "db": spec.Name})
	}
	return res, nil
}

type backupListParams struct {
	Database string `json:"database"`
}

// Window is the restore window: any time between From and To can be
// restored.
type Window struct {
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	LagSeconds int64  `json:"archive_lag_seconds"`
}

// backupList is backup.list's result for one database (--db).
type backupList struct {
	Stanza   string                    `json:"stanza"`
	Status   string                    `json:"status"`
	Window   Window                    `json:"restore_window"`
	Archive  backup.ArchiveStatus      `json:"archive"`
	Sets     []backup.Set              `json:"sets"`
	Database string                    `json:"database"`
	Dropped  *backup.DroppedStanza     `json:"dropped,omitempty"`
	Verify   *backup.VerifyResult      `json:"last_verify,omitempty"`
	Drill    *backup.Drill             `json:"last_drill,omitempty"`
	Dumps    []backup.Dump             `json:"dumps"`
	Policies map[string]dumpPolicy     `json:"dump_policies,omitempty"`
	Restores map[string]backup.Restore `json:"restores,omitempty"`
}

// backupRow is one database's row of backup.list without a database: every
// cluster, then every dropped stanza still in its window.
type backupRow struct {
	Database string               `json:"database"`
	Stanza   string               `json:"stanza"`
	Status   string               `json:"status"`
	Window   Window               `json:"restore_window"`
	Sets     int                  `json:"sets"`
	Latest   *backup.Set          `json:"latest,omitempty"`
	Verify   *backup.VerifyResult `json:"last_verify,omitempty"`
	Drill    *backup.Drill        `json:"last_drill,omitempty"`
	Dump     *backup.Dump         `json:"last_dump,omitempty"`
	// DroppedAt and ExpiresAt are set on a dropped database's row.
	DroppedAt string `json:"dropped_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
}

type dumpPolicy struct {
	Schedule  string `json:"schedule"`
	Retention int    `json:"retention"`
}

func handleBackupList(ctx context.Context, env *Env) (Result, error) {
	var p backupListParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	rs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	if p.Database != "" {
		return backupListOne(ctx, env.Cluster, rs, p.Database)
	}
	return backupListAll(ctx, env.Cluster, rs)
}

// backupListAll is one row per database, from one pgbackrest info over every
// stanza and each live cluster's pg_stat_archiver.
func backupListAll(ctx context.Context, cfg config.Cluster, rs backup.Records) (Result, error) {
	infos, err := repoDriver("backup.list", nil).InfoAll(ctx)
	if err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	rows := make([]backupRow, 0, len(cfg.Clusters)+len(rs.DroppedStanzas))
	empty := 0
	for _, spec := range cfg.Clusters {
		info := infos[spec.Stanza]
		archive, err := driver(spec, "backup.list", nil).ArchiveStatus(ctx)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "archive_status_unavailable",
				Message: "could not read pg_stat_archiver of " + spec.Name + ": " + err.Error()})
		}
		row := listRow(spec.Name, spec.Stanza, info, archive, rs)
		if row.Sets == 0 {
			empty++
		}
		rows = append(rows, row)
	}
	for _, d := range rs.DroppedStanzas {
		row := listRow(d.Database, d.Stanza, infos[d.Stanza], backup.ArchiveStatus{}, rs)
		row.Verify, row.Drill, row.Dump = nil, nil, nil
		row.DroppedAt, row.ExpiresAt = d.DroppedAt, d.ExpiresAt
		rows = append(rows, row)
	}
	if empty > 0 {
		warnings = append(warnings, output.Warning{Code: "no_backups",
			Message: fmt.Sprintf("%d database(s) have no backup yet; run eru backup now --type full", empty)})
	}
	return Result{Data: map[string]any{"databases": rows, "restores": rs.Restores}, Warnings: warnings}, nil
}

// listRow sums up one stanza for backup.list without a database. A dropped
// stanza has no live cluster, so its window ends at its newest set.
func listRow(db, stanza string, info backup.Info, archive backup.ArchiveStatus, rs backup.Records) backupRow {
	row := backupRow{Database: db, Stanza: stanza, Status: info.Status, Window: restoreWindow(info, archive)}
	if row.Status == "" {
		row.Status = "not in the repository"
	}
	for _, set := range info.Sets {
		if !set.Error {
			row.Sets++
		}
	}
	if latest, ok := info.Latest(); ok {
		row.Latest = &latest
	}
	if v, ok := rs.LastVerify[db]; ok {
		row.Verify = &v
	}
	if d, ok := rs.LastDrill(db); ok {
		row.Drill = &d
	}
	if d, ok := rs.LastDump[db]; ok {
		row.Dump = &d
	}
	return row
}

// backupListOne describes one database: its sets, window, last verify and
// drill, dumps and dump policy. A dropped database still in its window is
// listed from its kept stanza.
func backupListOne(ctx context.Context, cfg config.Cluster, rs backup.Records, db string) (Result, error) {
	spec, live := cfg.ClusterByName(db)
	out := backupList{Database: db}
	if !live {
		dropped, ok := rs.Dropped(db)
		if !ok {
			return Result{}, databaseNotFound(db)
		}
		out.Dropped = &dropped
		spec = config.ClusterSpec{Name: db, Stanza: dropped.Stanza}
	}
	d := driver(spec, "backup.list", nil)
	info, err := d.Info(ctx, "")
	if err != nil {
		return Result{}, err
	}
	if live {
		if out.Archive, err = d.ArchiveStatus(ctx); err != nil {
			return Result{}, err
		}
	}
	out.Stanza, out.Status, out.Sets = info.Stanza, info.Status, info.Sets
	out.Window = restoreWindow(info, out.Archive)
	var warnings []output.Warning
	if live {
		if v, ok := rs.LastVerify[db]; ok {
			out.Verify = &v
		}
		if drill, ok := rs.LastDrill(db); ok {
			out.Drill = &drill
		}
		if r, ok := rs.Restores[db]; ok {
			out.Restores = map[string]backup.Restore{db: r}
		}
		if spec.Backup.DumpSchedule != "" {
			out.Policies = map[string]dumpPolicy{db: {Schedule: spec.Backup.DumpSchedule, Retention: spec.Backup.DumpRetention}}
		}
	}
	out.Dumps = []backup.Dump{}
	if target, _, err := steps.BackupTarget(cfg); err == nil {
		dumps, err := backup.ListDumps(ctx, target, db)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "dumps_unavailable", Message: "could not list the dumps: " + err.Error()})
		} else {
			out.Dumps = dumps
		}
	}
	if len(out.Sets) == 0 {
		warnings = append(warnings, output.Warning{Code: "no_backups", Message: "the stanza of " + db + " holds no backup yet; run eru backup now --db " + db + " --type full"})
	}
	return Result{Data: out, Warnings: warnings}, nil
}

// restoreWindow is from just after the end of the oldest backup to the last
// archived WAL segment.
func restoreWindow(info backup.Info, archive backup.ArchiveStatus) Window {
	w := Window{LagSeconds: archive.LagSeconds}
	if oldest, ok := info.Oldest(); ok {
		// pgBackRest picks a set whose stop time is strictly before the
		// target, and stores it in whole seconds: the first restorable
		// second is the one after.
		w.From = oldest.Stop
		if stop, err := time.Parse(time.RFC3339, oldest.Stop); err == nil {
			w.From = stop.Add(time.Second).UTC().Format(time.RFC3339)
		}
	}
	w.To = archive.LastArchivedTime
	if w.To == "" {
		if latest, ok := info.Latest(); ok {
			w.To = latest.Stop
		}
	}
	return w
}

// inWindow checks a point-in-time target against the window.
func inWindow(w Window, t time.Time) error {
	from, errFrom := time.Parse(time.RFC3339, w.From)
	to, errTo := time.Parse(time.RFC3339, w.To)
	if errFrom != nil || errTo != nil {
		return output.NewError(output.CodePrecondition, "there is no restore window yet: the repository has no backup", output.ExitPrecondition,
			"Take a backup first with eru backup now --type full.").WithDetails(map[string]any{"reason": "no_backups"})
	}
	if t.Before(from) || t.After(to) {
		return output.NewError(output.CodePrecondition,
			fmt.Sprintf("%s is outside the restore window %s to %s", t.UTC().Format(time.RFC3339), w.From, w.To),
			output.ExitPrecondition,
			"Pick a time inside the window (eru backup list). A time just now becomes restorable once its WAL is archived, within about a minute; older points need a long-retention dump (restore --dump).").
			WithDetails(map[string]any{"reason": "outside_window", "window": w})
	}
	return nil
}

func containsString(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func databaseNotFound(name string) error {
	return output.NewError(output.CodePrecondition, "database "+name+" is not in cluster.toml", output.ExitPrecondition,
		"Run eru db list to see the databases.").WithDetails(map[string]any{"reason": "database_not_found", "database": name})
}

// databaseSize returns pg_database_size of db on the cluster at local.
func databaseSize(ctx context.Context, local pg.Local, db string) int64 {
	rows, err := local.Query(ctx, "postgres", "SELECT pg_database_size("+quoteLiteral(db)+") AS s", nil)
	if err != nil || len(rows) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(rows[0]["s"], 10, 64)
	return n
}

// liveDatabases lists the connectable non-template databases.
func liveDatabases(ctx context.Context, local pg.Local) ([]string, error) {
	rows, err := local.Query(ctx, "postgres", "SELECT datname FROM pg_database WHERE NOT datistemplate AND datallowconn ORDER BY datname", nil)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r["datname"])
	}
	return out, nil
}

// --- restore drill ---

type drillParams struct {
	Time     string `json:"time"`
	Database string `json:"database,omitempty"`
}

func handleRestoreDrill(ctx context.Context, env *Env) (Result, error) {
	var p drillParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	spec := firstCluster(env.Cluster)
	if p.Database != "" {
		var ok bool
		if spec, ok = env.Cluster.ClusterByName(p.Database); !ok {
			return Result{}, databaseNotFound(p.Database)
		}
	}
	if p.Time != "" {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return Result{}, output.NewError(output.CodeUsage, "time must be RFC 3339", output.ExitUsage, "")
		}
		d := driver(spec, "restore.drill", nil)
		info, err := d.Info(ctx, "")
		if err != nil {
			return Result{}, err
		}
		archive, err := d.ArchiveStatus(ctx)
		if err != nil {
			return Result{}, err
		}
		if err := inWindow(restoreWindow(info, archive), t); err != nil {
			return Result{}, err
		}
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"time": p.Time, "dry_run": true}}, nil
	}
	p.Database = spec.Name
	return runJobRPC(ctx, env, "restore-drill", p, nil)
}

// jobDrill restores the job's cluster into a scratch directory (the path
// server init --restore relies on), opens every database, runs pg_amcheck,
// compares the database list with the live cluster and tears it down. A
// drill never pushes WAL: the scratch instance runs with archive_mode off.
func jobDrill(ctx context.Context, env *jobEnv) (any, error) {
	var p drillParams
	if len(env.Params) > 0 {
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return nil, err
		}
	}
	cfg := env.Cluster
	if err := requireBackups(cfg); err != nil {
		return nil, err
	}
	drill := backup.Drill{ID: env.ID, Database: env.DB, StartedAt: stamp(), Target: backup.TargetLatest}
	opts := backup.RestoreOptions{Kind: backup.TargetLatest}
	if p.Time != "" {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return nil, err
		}
		opts = backup.RestoreOptions{Kind: backup.TargetTime, Time: t}
		drill.Target = t.UTC().Format(time.RFC3339)
	}
	record := func() {
		drill.FinishedAt = stamp()
		_ = backup.UpdateRecords(func(s *backup.Records) error {
			s.Drills = append(s.Drills, drill)
			return nil
		})
		steps.WriteMetrics(cfg)
	}
	first := env.spec()
	d := driver(first, env.Job, env.Report)
	info, err := d.Info(ctx, "")
	if err != nil {
		return nil, err
	}
	latest, ok := info.Latest()
	if !ok {
		drill.Result, drill.Reason = backup.DrillSkipped, "no_backups"
		record()
		return drill, fmt.Errorf("%w: the repository has no backup", errSkipped)
	}
	drill.BackupLabel = latest.Label
	firstPG := steps.ClusterPG(first)
	dataDir := cfg.Server.DataDirectory(first.Name)
	live, err := liveDatabases(ctx, firstPG)
	if err != nil {
		return nil, err
	}
	var size int64
	for _, db := range live {
		size += databaseSize(ctx, firstPG, db)
	}
	if _, err := backup.CheckSpace([]backup.SpaceNeed{{Path: backup.ScratchRoot(dataDir), Need: backup.ScratchNeed(size), Purpose: "the drill's scratch restore"}}); err != nil {
		drill.Result, drill.Reason = backup.DrillSkipped, "insufficient_space"
		record()
		logLine(env.Report, env.Job, err.Error())
		return drill, fmt.Errorf("%w: %s", errSkipped, err.Error())
	}
	scratch, err := backup.NewScratch(dataDir, "drill-"+env.ID, env.shell(), env.Job, env.Report)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scratch.Remove() }()
	fail := func(name string, err error) (any, error) {
		drill.Checks = append(drill.Checks, backup.DrillCheck{Name: name, OK: false, Detail: secrets.Redact(err.Error())})
		drill.Result = backup.DrillFail
		record()
		return drill, output.WrapError(output.CodeExternalCommand, fmt.Errorf("the restore drill failed at %s: %w", name, err), output.ExitExternal,
			"Read the job log with eru job follow "+env.ID+".")
	}
	if err := scratch.Restore(ctx, d, opts); err != nil {
		return fail("restore", err)
	}
	drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "restore", OK: true})
	if err := scratch.Start(ctx); err != nil {
		return fail("start", err)
	}
	drill.RecoveredTo = scratch.RecoveredTo(ctx)
	drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "start", OK: true, Detail: "recovered to " + drill.RecoveredTo})
	restored, err := liveDatabases(ctx, scratch.PG())
	if err != nil {
		return fail("list databases", err)
	}
	ok = true
	for _, db := range restored {
		if _, err := scratch.PG().Query(ctx, db, "SELECT 1", nil); err != nil {
			drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "open " + db, OK: false, Detail: secrets.Redact(err.Error())})
			ok = false
			continue
		}
		amctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err := env.shell().Exec(amctx, env.Job, engine.Command{Name: "runuser", Args: []string{
			"-u", "postgres", "--", "/usr/lib/postgresql/18/bin/pg_amcheck", "-h", scratch.Socket, "-p", strconv.Itoa(scratch.Port),
			"--install-missing", "--database=" + db,
		}}, env.Report)
		cancel()
		check := backup.DrillCheck{Name: "amcheck " + db, OK: err == nil}
		if err != nil {
			check.Detail = secrets.Redact(err.Error())
			ok = false
		}
		drill.Checks = append(drill.Checks, check)
	}
	var missing []string
	if p.Time == "" && !containsString(restored, first.Name) {
		missing = append(missing, first.Name)
	}
	listCheck := backup.DrillCheck{Name: "database list", OK: len(missing) == 0}
	if len(missing) > 0 {
		listCheck.Detail = "missing from the restore: " + strings.Join(missing, ", ")
		ok = false
	} else {
		extra := diffList(restored, live)
		gone := diffList(live, restored)
		if len(extra)+len(gone) > 0 {
			listCheck.Detail = fmt.Sprintf("only in the restore: %v; only live: %v", extra, gone)
		}
	}
	drill.Checks = append(drill.Checks, listCheck)
	drill.Result = backup.DrillPass
	if !ok {
		drill.Result = backup.DrillFail
	}
	record()
	if !ok {
		return drill, output.NewError(output.CodeExternalCommand, "the restore drill failed its checks", output.ExitExternal,
			"Read the checks in the job result with eru job follow "+env.ID+".").WithDetails(map[string]any{"drill": drill})
	}
	return drill, nil
}

// diffList returns the entries of a not in b.
func diffList(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !containsString(b, v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// --- dropped-stanza expiry ---

// jobStanzaExpiry deletes the stanzas of dropped databases whose restore
// window has passed (RFC 9.1): pgbackrest stanza-delete --force, then the
// record. A stanza the repository doesn't hold only loses its record; one
// whose database has an open restore waits for the next run. A failed delete
// keeps its record, so the next run tries again.
func jobStanzaExpiry(ctx context.Context, env *jobEnv) (any, error) {
	cfg := env.Cluster
	if err := requireBackups(cfg); err != nil {
		return nil, err
	}
	rs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	due := rs.Expired(time.Now())
	expired := []backup.DroppedStanza{}
	if len(due) == 0 {
		logLine(env.Report, env.Job, "no dropped stanza has passed its window")
		return map[string]any{"expired": expired}, nil
	}
	infos, err := repoDriver(env.Job, env.Report).InfoAll(ctx)
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, d := range due {
		if _, open := rs.Restores[d.Database]; open {
			logLine(env.Report, env.Job, "keeping "+d.Stanza+": a restore of "+d.Database+" is open")
			continue
		}
		if _, ok := infos[d.Stanza]; ok {
			drv := repoDriver(env.Job, env.Report)
			drv.Stanza = d.Stanza
			if err := drv.StanzaDelete(ctx, cfg.Server.DataDirectory(d.Database)); err != nil {
				logLine(env.Report, env.Job, "deleting "+d.Stanza+" failed: "+err.Error())
				failed = append(failed, d.Stanza)
				continue
			}
			logLine(env.Report, env.Job, "deleted "+d.Stanza+" of dropped database "+d.Database+" (expired "+d.ExpiresAt+")")
		}
		if err := backup.UpdateRecords(func(r *backup.Records) error {
			r.DroppedStanzas = slices.DeleteFunc(r.DroppedStanzas, func(x backup.DroppedStanza) bool { return x.Stanza == d.Stanza })
			return nil
		}); err != nil {
			return map[string]any{"expired": expired}, err
		}
		expired = append(expired, d)
	}
	res := map[string]any{"expired": expired}
	if len(failed) > 0 {
		return res, output.NewError(output.CodeExternalCommand, "deleting the stanzas "+strings.Join(failed, ", ")+" failed", output.ExitExternal,
			"Read the job log; the records stay, so the next eru-stanza-expiry run tries again.").
			WithDetails(map[string]any{"failed": failed, "expired": expired})
	}
	return res, nil
}

// --- long-retention dumps ---

type dumpCreateParams struct {
	Database string `json:"database"`
}

func handleDumpCreate(ctx context.Context, env *Env) (Result, error) {
	var p dumpCreateParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	if _, ok := env.Cluster.ClusterByName(p.Database); !ok {
		return Result{}, databaseNotFound(p.Database)
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "db-dump", p, nil)
}

// jobDump streams one database's dump into the bucket, encrypted to the age
// recipient, then prunes its scheduled dumps beyond dump_retention.
func jobDump(ctx context.Context, env *jobEnv) (any, error) {
	db := env.DB
	if len(env.Params) > 0 {
		var p dumpCreateParams
		if err := json.Unmarshal(env.Params, &p); err != nil {
			return nil, err
		}
		db = p.Database
	}
	cfg := env.Cluster
	if err := requireBackups(cfg); err != nil {
		return nil, err
	}
	entry, ok := cfg.ClusterByName(db)
	if !ok {
		return nil, databaseNotFound(db)
	}
	dump, err := dumpToBucket(ctx, cfg, env.shell(), env.Job, env.Report, db, false)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"dump": dump}
	if entry.Backup.DumpRetention > 0 {
		target, _, err := steps.BackupTarget(cfg)
		if err != nil {
			return nil, err
		}
		removed, err := backup.PruneDumps(ctx, target, db, entry.Backup.DumpRetention)
		if err != nil {
			return nil, err
		}
		res["pruned"] = removed
	}
	return res, nil
}

// dumpToBucket takes one dump and records it.
func dumpToBucket(ctx context.Context, cfg config.Cluster, sh engine.Shell, stepID string, report engine.Reporter, db string, final bool) (backup.Dump, error) {
	target, recipient, err := steps.BackupTarget(cfg)
	if err != nil {
		return backup.Dump{}, err
	}
	if recipient == "" {
		return backup.Dump{}, output.NewError(output.CodePrecondition, "the node has no age recipient for dumps", output.ExitPrecondition,
			"Run eru backup setup again; it sends the recipient.").WithDetails(map[string]any{"reason": "no_dump_recipient"})
	}
	local, err := clusterPG(cfg, db)
	if err != nil {
		return backup.Dump{}, err
	}
	logLine(report, stepID, "dumping "+db+" to the bucket")
	dump, err := backup.DumpToBucket(ctx, sh, stepID, report, target, local, db, recipient, final)
	if err != nil {
		return dump, err
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.LastDump[db] = dump
		return nil
	})
	logLine(report, stepID, fmt.Sprintf("dump %s of %s uploaded (%s)", dump.ID, db, humanSize(dump.Size)))
	return dump, nil
}

// DumpStream writes pg_dump -Fc of db to w. It backs eru node dump, which
// eru dump --to local runs over SSH to stream an archive to the laptop.
func DumpStream(ctx context.Context, db string, w io.Writer) error {
	if os.Geteuid() != 0 {
		return rootRequired("eru node dump")
	}
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return err
	}
	local, err := clusterPG(cfg, db)
	if err != nil {
		return err
	}
	return backup.PgDump(ctx, engine.Shell{Secrets: secrets.Default}, "dump", nil, local, db, w)
}

// NoOffsiteWarning is the no_offsite_backup warning for a server without a
// bucket: server init ends with it, and every apply carries it until eru
// backup setup runs.
func NoOffsiteWarning(cfg config.Cluster) []output.Warning {
	if cfg.BackupConfigured() {
		return nil
	}
	return []output.Warning{{
		Code:    "no_offsite_backup",
		Message: "no backups leave this server: a lost disk loses every database. Run eru backup setup to send them to an S3-compatible bucket.",
	}}
}
