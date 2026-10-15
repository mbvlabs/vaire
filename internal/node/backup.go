package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
		"eru backup now, eru backup verify and eru restore drill without --db call it once per database.")
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
		steps.RecordBackupFailure(env.Cluster, env.DB, kind)
		return nil, err
	}
	steps.RecordBackup(env.Cluster, env.DB, set)
	// pgBackRest expires old sets at the end of the backup, so the window
	// and the stanza's size are read once now (Phase 4).
	steps.RecordRepo(ctx, env.Cluster, d, env.DB)
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
	d := driver(spec, stepID, report)
	res, err := d.Verify(ctx)
	if err != nil {
		return res, err
	}
	steps.RecordRepo(ctx, cfg, d, spec.Name)
	res.Stanza = spec.Stanza
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
	// UpToDate is set when the archiver has caught up (nothing waiting, no
	// failure since the last success): LagSeconds is then only how long
	// the cluster has been idle, not lag.
	UpToDate bool `json:"up_to_date"`
	// PendingWAL is the count of finished segments waiting to be archived,
	// when it could be read.
	PendingWAL *int64 `json:"pending_wal,omitempty"`
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
	// DroppedAt and ExpiresAt are set on a dropped database's row, and
	// Reason when its stanza was replaced rather than dropped
	// (backup.DroppedReasonReplaced).
	DroppedAt string `json:"dropped_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Reason    string `json:"reason,omitempty"`
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
		row := listRow(d.Database, d.Stanza, infos[d.Stanza], droppedArchive(d), rs)
		row.Verify, row.Drill, row.Dump = nil, nil, nil
		row.DroppedAt, row.ExpiresAt, row.Reason = d.DroppedAt, d.ExpiresAt, d.Reason
		rows = append(rows, row)
	}
	if empty > 0 {
		warnings = append(warnings, output.Warning{Code: "no_backups",
			Message: fmt.Sprintf("%d database(s) have no backup yet; run eru backup now --type full", empty)})
	}
	return Result{Data: map[string]any{"databases": rows, "restores": rs.Restores}, Warnings: warnings}, nil
}

// droppedArchive stands in for the pg_stat_archiver of a dropped stanza's
// cluster: the time it last archived WAL, recorded at the drop.
func droppedArchive(d backup.DroppedStanza) backup.ArchiveStatus {
	return backup.ArchiveStatus{LastArchivedTime: d.LastArchivedAt}
}

// listRow sums up one stanza for backup.list without a database. A dropped
// stanza has no live cluster, so its window ends at the WAL archived before
// the drop, or its newest set when that wasn't recorded.
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
	if v, ok := rs.VerifyOf(db, stanza); ok {
		row.Verify = &v
	}
	if d, ok := rs.DrillOf(db, stanza); ok {
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
	var dropped backup.DroppedStanza
	if !live {
		var ok bool
		if dropped, ok = rs.Dropped(db); !ok {
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
	archive := droppedArchive(dropped)
	if live {
		if out.Archive, err = d.ArchiveStatus(ctx); err != nil {
			return Result{}, err
		}
		archive = out.Archive
	}
	out.Stanza, out.Status, out.Sets = info.Stanza, info.Status, info.Sets
	out.Window = restoreWindow(info, archive)
	var warnings []output.Warning
	if live {
		if v, ok := rs.VerifyOf(db, spec.Stanza); ok {
			out.Verify = &v
		}
		if drill, ok := rs.DrillOf(db, spec.Stanza); ok {
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
// archived WAL segment, or the newest backup's end when the archive time is
// unknown or older. It never ends before it starts.
func restoreWindow(info backup.Info, archive backup.ArchiveStatus) Window {
	w := Window{LagSeconds: archive.LagSeconds, UpToDate: archive.UpToDate(), PendingWAL: archive.PendingWAL}
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
	if latest, ok := info.Latest(); ok && laterThan(latest.Stop, w.To) {
		w.To = latest.Stop
	}
	if laterThan(w.From, w.To) {
		w.To = w.From
	}
	return w
}

// laterThan reports whether RFC 3339 time a is after b, or b is empty or
// unparseable while a parses.
func laterThan(a, b string) bool {
	ta, err := time.Parse(time.RFC3339, a)
	if err != nil {
		return false
	}
	tb, err := time.Parse(time.RFC3339, b)
	return err != nil || ta.After(tb)
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

// --- restore drill ---

type drillParams struct {
	Time     string `json:"time"`
	Database string `json:"database,omitempty"`
}

// drillPlan is one database's drill in restore.drill's dry run.
type drillPlan struct {
	Database string `json:"database"`
	Target   string `json:"target"`
	Timeline string `json:"timeline,omitempty"`
}

// handleRestoreDrill starts the drill of params.database. A dry run without
// a database plans one per cluster; the CLI runs them one after another.
func handleRestoreDrill(ctx context.Context, env *Env) (Result, error) {
	var p drillParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	specs := env.Cluster.Clusters
	if p.Database != "" {
		spec, ok := env.Cluster.ClusterByName(p.Database)
		if !ok {
			return Result{}, databaseNotFound(p.Database)
		}
		specs = []config.ClusterSpec{spec}
	} else if !env.Req.DryRun {
		return Result{}, oneDatabase("restore.drill")
	}
	var at time.Time
	if p.Time != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, p.Time); err != nil {
			return Result{}, output.NewError(output.CodeUsage, "time must be RFC 3339", output.ExitUsage, "")
		}
	}
	rs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	plan := make([]drillPlan, 0, len(specs))
	for _, spec := range specs {
		target := backup.TargetLatest
		if p.Time != "" {
			d := driver(spec, "restore.drill", nil)
			info, err := d.Info(ctx, "")
			if err != nil {
				return Result{}, err
			}
			archive, err := d.ArchiveStatus(ctx)
			if err != nil {
				return Result{}, err
			}
			if err := inWindow(restoreWindow(info, archive), at); err != nil {
				return Result{}, err
			}
			target = at.UTC().Format(time.RFC3339)
		}
		plan = append(plan, drillPlan{Database: spec.Name, Target: target, Timeline: rs.TargetTimeline(spec.Name, spec.Stanza)})
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"drills": plan, "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "restore-drill", p, nil)
}

// jobDrill materialises the job's database at the latest point, or at
// params.time, into the temporary cluster <db>__drill (RFC 9.4): restored
// from its stanza on the timeline restore undo recorded, promoted there, and
// never archiving. It opens the database, runs pg_amcheck on it, compares
// its tables with the live cluster's and drops the copy.
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
	spec := env.spec()
	db := spec.Name
	rs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	drill := backup.Drill{ID: env.ID, Database: db, Stanza: spec.Stanza, StartedAt: stamp(), Target: backup.TargetLatest}
	opts := backup.RestoreOptions{Kind: backup.TargetLatest, Timeline: rs.TargetTimeline(db, spec.Stanza)}
	if p.Time != "" {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return nil, err
		}
		opts.Kind, opts.Time = backup.TargetTime, t
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
	d := driver(spec, env.Job, env.Report)
	info, err := d.Info(ctx, "")
	if err != nil {
		return nil, err
	}
	latest, ok := info.Latest()
	if !ok {
		drill.Result, drill.Reason = backup.DrillSkipped, "no_backups"
		record()
		return drill, fmt.Errorf("%w: the stanza has no backup", errSkipped)
	}
	drill.BackupLabel = latest.Label
	live := steps.ClusterPG(spec)
	if _, err := backup.CheckSpace([]backup.SpaceNeed{{Path: cfg.Server.DataDirectory(db), Need: backup.CopyNeed(databaseSize(ctx, live, db)),
		Purpose: "the drill's copy of " + db}}); err != nil {
		drill.Result, drill.Reason = backup.DrillSkipped, "insufficient_space"
		record()
		logLine(env.Report, env.Job, err.Error())
		return drill, fmt.Errorf("%w: %s", errSkipped, err.Error())
	}
	name := backup.DrillName(db)
	if err := dropKilledDrill(env, rs, db); err != nil {
		return nil, err
	}
	m, err := backup.NewMaterialised(cfg.Server.DataRoot, name, db, spec.Port, env.shell(), env.Job, env.Report)
	if err != nil {
		return nil, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Drilling[db] = name
		return nil
	}); err != nil {
		_ = m.Drop()
		return nil, err
	}
	defer func() {
		if err := m.Drop(); err != nil {
			logLine(env.Report, env.Job, "dropping "+name+" failed, the next drill of "+db+" drops it: "+err.Error())
			return
		}
		_ = backup.UpdateRecords(func(s *backup.Records) error {
			delete(s.Drilling, db)
			return nil
		})
	}()
	fail := func(check string, err error) (any, error) {
		drill.Checks = append(drill.Checks, backup.DrillCheck{Name: check, OK: false, Detail: secrets.Redact(err.Error())})
		drill.Result = backup.DrillFail
		record()
		return drill, output.WrapError(output.CodeExternalCommand, fmt.Errorf("the restore drill of %s failed at %s: %w", db, check, err), output.ExitExternal,
			"Read the job log with eru job follow "+env.ID+".")
	}
	if err := m.Restore(ctx, d, opts); err != nil {
		// pg_dropcluster removes the copy's log with it.
		for _, line := range m.LogTail(20) {
			logLine(env.Report, env.Job, name+": "+line)
		}
		return fail("restore", err)
	}
	drill.RecoveredTo = m.RecoveredTo(ctx)
	if drill.RecoveredTo == "" {
		// No transaction committed after the backup, so there is no replay
		// timestamp: the copy is consistent at the backup's end.
		drill.RecoveredTo, drill.NoWALReplayed = latest.Stop, true
	}
	drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "restore", OK: true, Detail: "recovered to " + drill.DescribeRecoveredTo()})
	copyPG := m.PG()
	if _, err := copyPG.Query(ctx, db, "SELECT 1", nil); err != nil {
		return fail("open "+db, err)
	}
	drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "open " + db, OK: true})
	amctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	err = env.shell().Exec(amctx, env.Job, engine.Command{Name: "runuser", Args: []string{
		"-u", "postgres", "--", "/usr/lib/postgresql/18/bin/pg_amcheck", "-h", copyPG.Host, "-p", strconv.Itoa(copyPG.Port),
		"--install-missing", "--database=" + db,
	}}, env.Report)
	cancel()
	ok = err == nil
	check := backup.DrillCheck{Name: "amcheck " + db, OK: ok}
	if err != nil {
		check.Detail = secrets.Redact(err.Error())
	}
	drill.Checks = append(drill.Checks, check)
	drill.Checks = append(drill.Checks, backup.DrillCheck{Name: "tables", OK: true, Detail: tableDiff(compareRows(ctx, live, copyPG, db))})
	drill.Result = backup.DrillPass
	if !ok {
		drill.Result = backup.DrillFail
	}
	record()
	if !ok {
		return drill, output.NewError(output.CodeExternalCommand, "the restore drill of "+db+" failed its checks", output.ExitExternal,
			"Read the checks in the job result with eru job follow "+env.ID+".").WithDetails(map[string]any{"drill": drill})
	}
	return drill, nil
}

// dropKilledDrill drops the <db>__drill cluster a killed drill left behind:
// records.json still names it as db's running drill, and the heavy-I/O lock
// this job holds means no other drill runs. A <db>__drill the records don't
// name is not the drill's, and NewMaterialised refuses it.
func dropKilledDrill(env *jobEnv, rs backup.Records, db string) error {
	name, ok := rs.Drilling[db]
	if !ok {
		return nil
	}
	logLine(env.Report, env.Job, "dropping "+name+", left by a drill that did not finish")
	if err := backup.DropTemporaryCluster(env.shell(), env.Job, env.Report, name, filepath.Join(env.Cluster.Server.DataRoot, name)); err != nil {
		return err
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Drilling, db)
		return nil
	})
}

// maxTableNames bounds the table names a drill's tables check lists.
const maxTableNames = 10

// tableDiff describes the tables of a drill's comparison with live: how
// many, and the ones only one side has. A table created or dropped since the
// target is expected, so it is a detail, not a failure.
func tableDiff(rows []tableRows) string {
	var onlyCopy, onlyLive []string
	for _, r := range rows {
		switch {
		case r.Live == nil:
			onlyCopy = append(onlyCopy, r.Table)
		case r.Restoring == nil:
			onlyLive = append(onlyLive, r.Table)
		}
	}
	names := func(list []string) string {
		if len(list) > maxTableNames {
			return strings.Join(list[:maxTableNames], ", ") + fmt.Sprintf(" and %d more", len(list)-maxTableNames)
		}
		return strings.Join(list, ", ")
	}
	out := fmt.Sprintf("%d tables", len(rows))
	if len(rows) == 1 {
		out = "1 table"
	}
	if len(onlyCopy) > 0 {
		out += "; only in the copy: " + names(onlyCopy)
	}
	if len(onlyLive) > 0 {
		out += "; only live: " + names(onlyLive)
	}
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
