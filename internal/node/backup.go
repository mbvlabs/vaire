package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
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

// driver is the pgBackRest driver for the node's cluster.
func driver(cfg config.Cluster, stepID string, report engine.Reporter) backup.PgBackRest {
	return backup.PgBackRest{Stanza: cfg.Name(), Shell: engine.Shell{Secrets: secrets.Default}, StepID: stepID, Report: report}
}

// requireBackups refuses a backup method on a cluster without a bucket.
func requireBackups(cfg config.Cluster) error {
	if cfg.BackupConfigured() {
		return nil
	}
	return output.NewError(output.CodePrecondition, "backups are not configured on this cluster", output.ExitPrecondition,
		"Run eru backup setup first.").WithDetails(map[string]any{"reason": "backup_not_configured"})
}

type backupNowParams struct {
	Type string `json:"type"`
}

func handleBackupNow(ctx context.Context, env *Env) (Result, error) {
	var p backupNowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	kind := p.Type
	if kind == "" {
		kind = backup.TypeIncr
		info, err := driver(env.Cluster, "backup.now", nil).Info(ctx, "")
		if err != nil {
			return Result{}, err
		}
		if !info.HasFull() {
			kind = backup.TypeFull
		}
	}
	switch kind {
	case backup.TypeFull, backup.TypeDiff, backup.TypeIncr:
	default:
		return Result{}, output.NewError(output.CodeUsage, "type must be full, diff or incr", output.ExitUsage, "")
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"type": kind, "dry_run": true}}, nil
	}
	return runJobRPC(ctx, env, "backup-"+kind, nil, nil)
}

// jobBackup takes one backup. A backup another process holds pgBackRest's
// lock for is skipped, not failed.
func jobBackup(ctx context.Context, env *jobEnv, kind string) (any, error) {
	if err := requireBackups(env.Cluster); err != nil {
		return nil, err
	}
	d := driver(env.Cluster, env.Job, env.Report)
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
	steps.RecordBackup(env.Cluster, set)
	logLine(env.Report, env.Job, fmt.Sprintf("backup %s (%s) finished: %s", set.Label, set.Type, humanSize(set.Size)))
	return map[string]any{"set": set}, nil
}

func handleBackupVerify(ctx context.Context, env *Env) (Result, error) {
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	return runJobRPC(ctx, env, "backup-verify", nil, nil)
}

func jobVerify(ctx context.Context, env *jobEnv) (any, error) {
	if err := requireBackups(env.Cluster); err != nil {
		return nil, err
	}
	return verify(ctx, env.Cluster, env.Job, env.Report)
}

// verify runs pgbackrest verify and records the result. A verify that finds
// problems is recorded and then returned as external_command_failed.
func verify(ctx context.Context, cfg config.Cluster, stepID string, report engine.Reporter) (backup.VerifyResult, error) {
	res, err := driver(cfg, stepID, report).Verify(ctx)
	if err != nil {
		return res, err
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		s.LastVerify = &res
		return nil
	})
	steps.WriteMetrics(cfg)
	if !res.OK {
		return res, output.NewError(output.CodeExternalCommand, "pgbackrest verify found problems in the repository", output.ExitExternal,
			"Read details.output, then take a new full backup with eru backup now --type full.").
			WithDetails(map[string]any{"output": res.Output})
	}
	return res, nil
}

type backupListParams struct {
	Database string `json:"database"`
}

// Window is the restore window: any time between From and To can be
// restored, for every database.
type Window struct {
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	LagSeconds int64  `json:"archive_lag_seconds"`
}

// backupList is backup.list's result.
type backupList struct {
	Stanza   string                    `json:"stanza"`
	Status   string                    `json:"status"`
	Window   Window                    `json:"restore_window"`
	Archive  backup.ArchiveStatus      `json:"archive"`
	Sets     []backup.Set              `json:"sets"`
	Database string                    `json:"database,omitempty"`
	Verify   *backup.VerifyResult      `json:"last_verify,omitempty"`
	Drill    *backup.Drill             `json:"last_drill,omitempty"`
	Dumps    []backup.Dump             `json:"dumps"`
	Policies map[string]dumpPolicy     `json:"dump_policies,omitempty"`
	Restores map[string]backup.Restore `json:"restores,omitempty"`
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
	cfg := env.Cluster
	if err := requireBackups(cfg); err != nil {
		return Result{}, err
	}
	if p.Database != "" {
		if _, ok := cfg.ClusterByName(p.Database); !ok {
			return Result{}, databaseNotFound(p.Database)
		}
	}
	d := driver(cfg, "backup.list", nil)
	info, err := d.Info(ctx, "")
	if err != nil {
		return Result{}, err
	}
	archive, err := d.ArchiveStatus(ctx)
	if err != nil {
		return Result{}, err
	}
	out := backupList{Stanza: info.Stanza, Status: info.Status, Archive: archive, Database: p.Database, Window: restoreWindow(info, archive)}
	var warnings []output.Warning
	for _, set := range info.Sets {
		if p.Database != "" {
			detail, err := d.Info(ctx, set.Label)
			if err == nil {
				if full, ok := detail.Find(set.Label); ok {
					set.Databases = full.Databases
				}
			}
			if !containsString(set.Databases, p.Database) {
				continue
			}
		}
		out.Sets = append(out.Sets, set)
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	out.Verify = bs.LastVerify
	if drill, ok := bs.LastDrill(); ok {
		out.Drill = &drill
	}
	out.Restores = bs.Restores
	out.Policies = map[string]dumpPolicy{}
	for _, db := range cfg.Clusters {
		if db.Backup.DumpSchedule != "" && (p.Database == "" || p.Database == db.Name) {
			out.Policies[db.Name] = dumpPolicy{Schedule: db.Backup.DumpSchedule, Retention: db.Backup.DumpRetention}
		}
	}
	out.Dumps = []backup.Dump{}
	if target, _, err := steps.BackupTarget(cfg); err == nil {
		dumps, err := backup.ListDumps(ctx, target, p.Database)
		if err != nil {
			warnings = append(warnings, output.Warning{Code: "dumps_unavailable", Message: "could not list the dumps: " + err.Error()})
		} else {
			out.Dumps = dumps
		}
	}
	if len(out.Sets) == 0 {
		warnings = append(warnings, output.Warning{Code: "no_backups", Message: "the repository holds no backup yet; run eru backup now --type full"})
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

// databaseSize returns pg_database_size of db on the live cluster.
func databaseSize(ctx context.Context, db string) int64 {
	rows, err := (pg.Local{}).Query(ctx, "postgres", "SELECT pg_database_size("+quoteLiteral(db)+") AS s", nil)
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
	Time string `json:"time"`
}

func handleRestoreDrill(ctx context.Context, env *Env) (Result, error) {
	var p drillParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if err := requireBackups(env.Cluster); err != nil {
		return Result{}, err
	}
	if p.Time != "" {
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return Result{}, output.NewError(output.CodeUsage, "time must be RFC 3339", output.ExitUsage, "")
		}
		d := driver(env.Cluster, "restore.drill", nil)
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
	return runJobRPC(ctx, env, "restore-drill", p, nil)
}

// jobDrill restores the whole cluster into a scratch directory (the path
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
	drill := backup.Drill{ID: env.ID, StartedAt: stamp(), Target: backup.TargetLatest}
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
	d := driver(cfg, env.Job, env.Report)
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
	live, err := liveDatabases(ctx, pg.Local{})
	if err != nil {
		return nil, err
	}
	var size int64
	for _, db := range live {
		size += databaseSize(ctx, db)
	}
	if _, err := backup.CheckSpace([]backup.SpaceNeed{{Path: backup.ScratchRoot(cfg.Server.DataDirectory()), Need: backup.ScratchNeed(size), Purpose: "the drill's scratch restore"}}); err != nil {
		drill.Result, drill.Reason = backup.DrillSkipped, "insufficient_space"
		record()
		logLine(env.Report, env.Job, err.Error())
		return drill, fmt.Errorf("%w: %s", errSkipped, err.Error())
	}
	scratch, err := backup.NewScratch(cfg.Server.DataDirectory(), "drill-"+env.ID, env.shell(), env.Job, env.Report)
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
			"-u", "postgres", "--", "/usr/lib/postgresql/18/bin/pg_amcheck", "-h", scratch.Socket, "-p", strconv.Itoa(backup.ScratchPort),
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
	for _, db := range cfg.Clusters {
		if p.Time == "" && !containsString(restored, db.Name) {
			missing = append(missing, db.Name)
		}
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

// diffList returns the entries of a not in b, skipping eru restore names.
func diffList(a, b []string) []string {
	var out []string
	for _, v := range a {
		if !containsString(b, v) && !config.IsRestoreName(v) {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
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
	logLine(report, stepID, "dumping "+db+" to the bucket")
	dump, err := backup.DumpToBucket(ctx, sh, stepID, report, target, db, recipient, final)
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
	if _, ok := cfg.ClusterByName(db); !ok {
		return databaseNotFound(db)
	}
	return backup.PgDump(ctx, engine.Shell{Secrets: secrets.Default}, "dump", nil, "", 0, db, w)
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
