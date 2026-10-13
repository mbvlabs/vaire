package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/state"
	"github.com/mbvlabs/eru/internal/steps"
)

// Restore source kinds beyond the pgBackRest targets.
const targetDump = "dump"

// restoreParams start a restore of one database.
type restoreParams struct {
	Database string `json:"database"`
	// Kind is latest, time, set or dump.
	Kind    string `json:"kind"`
	Time    string `json:"time,omitempty"`
	Set     string `json:"set,omitempty"`
	Dump    string `json:"dump,omitempty"`
	Promote bool   `json:"promote,omitempty"`
}

// restoreToken is <db>/restore.
func restoreToken(_ config.Cluster, params json.RawMessage) string {
	var p struct {
		Database string `json:"database"`
	}
	if json.Unmarshal(params, &p) != nil || p.Database == "" {
		return ""
	}
	return p.Database + "/restore"
}

// cleanupToken is <db>/cleanup, needed only when the request drops.
func cleanupToken(_ config.Cluster, params json.RawMessage) string {
	var p restoreCleanupParams
	if json.Unmarshal(params, &p) != nil || !p.Drop {
		return ""
	}
	return p.Database + "/cleanup"
}

// restoreInProgress refuses an operation on a database with an open restore.
func restoreInProgress(db, action string) error {
	bs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	r, ok := bs.OpenRestore(db)
	if !ok {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		"a restore of "+db+" is open ("+r.Phase+"); "+action+" waits until it is promoted or aborted",
		output.ExitPrecondition,
		"Finish it with eru restore promote "+db+" or eru restore abort "+db+".").
		WithDetails(map[string]any{"reason": "restore_in_progress", "database": db, "restore_id": r.ID})
}

// clearFailedRestore drops what a failed restore of db left behind, so the
// next restore can start without an abort first. Only a failed restore whose
// job has finished is cleared; a ready one holds the operator's copy.
func clearFailedRestore(ctx context.Context, cfg config.Cluster, db string) error {
	bs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	r, ok := bs.OpenRestore(db)
	if !ok || r.Phase != backup.PhaseFailed {
		return nil
	}
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return err
	}
	if job, ok := st.Job(r.ID); ok && !job.Final() {
		return nil
	}
	local, err := clusterPG(cfg, db)
	if err != nil {
		return err
	}
	if err := local.Exec(ctx, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);\n", pg.Quote(backup.RestoringName(db))), nil); err != nil {
		return err
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		if cur, ok := s.Restores[db]; ok && cur.ID == r.ID {
			delete(s.Restores, db)
		}
		return nil
	})
}

// checkRestorable checks db can be restored: configured, a normal name, and
// short enough for the names beside it.
func checkRestorable(cfg config.Cluster, db string) error {
	if config.IsRestoreName(db) {
		return output.NewError(output.CodeValidation, db+" is one of eru restore's own databases", output.ExitConfig,
			"Restore the database it belongs to.")
	}
	if _, ok := cfg.ClusterByName(db); !ok {
		return databaseNotFound(db)
	}
	if len(db) > backup.MaxRestorableName {
		return output.NewError(output.CodePrecondition,
			fmt.Sprintf("database %s is longer than %d characters, so its restore names would not fit PostgreSQL's 63-character limit", db, backup.MaxRestorableName),
			output.ExitPrecondition, "Fork it instead with eru db fork "+db+" --at <time> --name <short name>.").
			WithDetails(map[string]any{"reason": "name_too_long"})
	}
	return nil
}

func handleRestoreStart(ctx context.Context, env *Env) (Result, error) {
	var p restoreParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	if err := requireBackups(cfg); err != nil {
		return Result{}, err
	}
	if err := checkRestorable(cfg, p.Database); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("a restore replaces "+p.Database+" once promoted", p.Database+"/restore")
	}
	release, err := engine.Acquire(ctx, engine.LockShared, "restore.start", env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return Result{}, err
	}
	releaseCluster, err := engine.AcquireCluster(ctx, p.Database, engine.LockShared, "restore.start", env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		release()
		return Result{}, err
	}
	target, err := checkRestoreTarget(ctx, cfg, p, env.Req.Secrets)
	if err == nil && !env.Req.DryRun {
		err = clearFailedRestore(ctx, cfg, p.Database)
	}
	if err == nil {
		err = restoreInProgress(p.Database, "a new restore")
	}
	if err == nil {
		if exists, existsErr := databaseExists(ctx, cfg, p.Database, backup.RestoringName(p.Database)); existsErr == nil && exists {
			err = output.NewError(output.CodePrecondition, backup.RestoringName(p.Database)+" already exists", output.ExitPrecondition,
				"Drop it with eru restore abort "+p.Database+".").WithDetails(map[string]any{"reason": "restoring_exists"})
		}
	}
	var space []backup.SpaceNeed
	if err == nil {
		space, err = restoreSpace(ctx, cfg, p.Database, p.Kind != targetDump)
	}
	releaseCluster()
	release()
	if err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "target": target, "space": space, "dry_run": true}}, nil
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		if _, open := s.Restores[p.Database]; open {
			return restoreInProgress(p.Database, "a new restore")
		}
		s.Restores[p.Database] = backup.Restore{
			ID: env.Req.RequestID, Database: p.Database, Kind: p.Kind, Target: target,
			Phase: backup.PhaseRestoring, StartedAt: stamp(), Promote: p.Promote,
		}
		return nil
	}); err != nil {
		return Result{}, err
	}
	steps.WriteMetrics(cfg)
	jobSecrets := map[string]string{}
	if v := env.Req.Secrets[steps.SecretBackupAgeIdentity]; v != "" {
		jobSecrets[steps.SecretBackupAgeIdentity] = v
	}
	return runJobRPC(ctx, env, "restore", p, jobSecrets)
}

// checkRestoreTarget validates the target and describes it.
func checkRestoreTarget(ctx context.Context, cfg config.Cluster, p restoreParams, sec map[string]string) (string, error) {
	spec, err := clusterOf(cfg, p.Database)
	if err != nil {
		return "", err
	}
	d := driver(spec, "restore", nil)
	switch p.Kind {
	case backup.TargetLatest:
		info, err := d.Info(ctx, "")
		if err != nil {
			return "", err
		}
		if _, ok := info.Latest(); !ok {
			return "", output.NewError(output.CodePrecondition, "the repository has no backup", output.ExitPrecondition,
				"Take one with eru backup now --type full.").WithDetails(map[string]any{"reason": "no_backups"})
		}
		return "latest", nil
	case backup.TargetTime:
		t, err := time.Parse(time.RFC3339, p.Time)
		if err != nil {
			return "", output.NewError(output.CodeUsage, "time must be RFC 3339", output.ExitUsage, "")
		}
		info, err := d.Info(ctx, "")
		if err != nil {
			return "", err
		}
		archive, err := d.ArchiveStatus(ctx)
		if err != nil {
			return "", err
		}
		if err := inWindow(restoreWindow(info, archive), t); err != nil {
			return "", err
		}
		return t.UTC().Format(time.RFC3339), nil
	case backup.TargetSet:
		info, err := d.Info(ctx, "")
		if err != nil {
			return "", err
		}
		if _, ok := info.Find(p.Set); !ok {
			return "", output.NewError(output.CodePrecondition, "there is no backup set "+p.Set, output.ExitPrecondition,
				"List the sets with eru backup list.").WithDetails(map[string]any{"reason": "set_not_found"})
		}
		return "set " + p.Set, nil
	case targetDump:
		if sec[steps.SecretBackupAgeIdentity] == "" {
			return "", output.NewError(output.CodeInputRequired, "a restore from a dump needs the age identity from the recovery kit", output.ExitInput,
				"Pass "+steps.SecretBackupAgeIdentity+".").WithDetails(map[string]any{"secret": steps.SecretBackupAgeIdentity})
		}
		target, _, err := steps.BackupTarget(cfg)
		if err != nil {
			return "", err
		}
		dumps, err := backup.ListDumps(ctx, target, p.Database)
		if err != nil {
			return "", err
		}
		for _, dump := range dumps {
			if dump.ID == p.Dump {
				return "dump " + p.Dump, nil
			}
		}
		return "", output.NewError(output.CodePrecondition, "there is no dump "+p.Dump+" of "+p.Database, output.ExitPrecondition,
			"List the dumps with eru dump list "+p.Database+".").WithDetails(map[string]any{"reason": "dump_not_found"})
	}
	return "", output.NewError(output.CodeUsage, "kind must be latest, time, set or dump", output.ExitUsage, "")
}

// restoreSpace checks room for a scratch copy of db plus the restored copy
// beside the live one.
func restoreSpace(ctx context.Context, cfg config.Cluster, db string, scratch bool) ([]backup.SpaceNeed, error) {
	local, err := clusterPG(cfg, db)
	if err != nil {
		return nil, err
	}
	size := databaseSize(ctx, local, db)
	dataDir := cfg.Server.DataDirectory(db)
	var needs []backup.SpaceNeed
	if scratch {
		needs = append(needs, backup.SpaceNeed{Path: backup.ScratchRoot(dataDir), Need: backup.ScratchNeed(size), Purpose: "the scratch restore of " + db})
	}
	needs = append(needs, backup.SpaceNeed{Path: dataDir, Need: backup.ScratchNeed(size), Purpose: "the restored copy of " + db})
	return backup.CheckSpace(needs)
}

// jobRestore loads the target into <db>__restoring beside the live
// database, which keeps serving, and leaves the restore ready for inspection,
// or promotes it straight away with --promote.
func jobRestore(ctx context.Context, env *jobEnv) (any, error) {
	var p restoreParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	warnings, err := loadRestore(ctx, env, p)
	if err != nil {
		failRestore(env, err)
		return nil, err
	}
	readyAt := stamp()
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		r := s.Restores[p.Database]
		r.Phase, r.ReadyAt, r.Warnings, r.Error = backup.PhaseReady, readyAt, warnings, ""
		s.Restores[p.Database] = r
		return nil
	})
	res := map[string]any{"database": p.Database, "restoring": backup.RestoringName(p.Database), "phase": backup.PhaseReady, "warnings": warnings}
	if p.Promote {
		promoted, err := promote(ctx, env.Cluster, p.Database, env.Report)
		if err != nil {
			return res, err
		}
		res["phase"] = "promoted"
		res["promoted"] = promoted
	}
	steps.WriteMetrics(env.Cluster)
	return res, nil
}

// failRestore records a failed restore so abort can clean it up.
func failRestore(env *jobEnv, cause error) {
	var p restoreParams
	if json.Unmarshal(env.Params, &p) != nil || p.Database == "" {
		return
	}
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		if r, ok := s.Restores[p.Database]; ok {
			r.Phase, r.Error = backup.PhaseFailed, cause.Error()
			s.Restores[p.Database] = r
		}
		return nil
	})
}

// loadRestore creates <db>__restoring and fills it from a scratch restore or
// a dump.
func loadRestore(ctx context.Context, env *jobEnv, p restoreParams) ([]string, error) {
	cfg := env.Cluster
	db, ok := cfg.ClusterByName(p.Database)
	if !ok {
		return nil, databaseNotFound(p.Database)
	}
	local := steps.ClusterPG(db)
	target := backup.RestoringName(p.Database)
	if err := createLoadTarget(ctx, local, target, db); err != nil {
		return nil, err
	}
	var warnings []string
	var err error
	if p.Kind == targetDump {
		warnings, err = loadFromDump(ctx, env, p, local, target)
	} else {
		opts := backup.RestoreOptions{Database: p.Database, Kind: p.Kind, Set: p.Set}
		if p.Kind == backup.TargetTime {
			opts.Time, _ = time.Parse(time.RFC3339, p.Time)
		}
		warnings, err = loadFromScratch(ctx, env, opts, p.Database, local, target, p.Database+"_owner")
	}
	if err != nil {
		return warnings, err
	}
	return warnings, finishLoad(ctx, local, target, db)
}

// createLoadTarget creates database target in the cluster at local, owned
// by db's owner, with db's grants and default privileges in place before
// anything is loaded.
func createLoadTarget(ctx context.Context, local pg.Local, target string, db config.ClusterSpec) error {
	sql := fmt.Sprintf("CREATE DATABASE %s OWNER %s;\n", pg.Quote(target), pg.Quote(db.Name+"_owner")) +
		steps.DatabaseConnectSQL(target, db.Name)
	if err := local.Exec(ctx, "postgres", sql, nil); err != nil {
		return err
	}
	return local.Exec(ctx, target, steps.DatabaseGrantsSQL(db), nil)
}

// finishLoad re-runs the grants after the load and analyzes, so restore
// status can compare row estimates.
func finishLoad(ctx context.Context, local pg.Local, target string, db config.ClusterSpec) error {
	if err := local.Exec(ctx, target, steps.DatabaseGrantsSQL(db), nil); err != nil {
		return err
	}
	return local.Exec(ctx, target, "ANALYZE;\n", nil)
}

// loadFromScratch restores db at opts into a scratch instance and copies it
// into target on the cluster at dst, as owner. The scratch instance is
// removed afterwards.
func loadFromScratch(ctx context.Context, env *jobEnv, opts backup.RestoreOptions, db string, dst pg.Local, target, owner string) ([]string, error) {
	spec, err := clusterOf(env.Cluster, db)
	if err != nil {
		return nil, err
	}
	scratch, err := backup.NewScratch(env.Cluster.Server.DataDirectory(db), env.ID, env.shell(), env.Job, env.Report)
	if err != nil {
		return nil, err
	}
	defer func() { _ = scratch.Remove() }()
	logLine(env.Report, env.Job, "restoring "+db+" into a scratch instance")
	if err := scratch.Restore(ctx, driver(spec, env.Job, env.Report), opts); err != nil {
		return nil, err
	}
	if err := scratch.Start(ctx); err != nil {
		return nil, err
	}
	if t := scratch.RecoveredTo(ctx); t != "" {
		logLine(env.Report, env.Job, "recovered to "+t)
	}
	logLine(env.Report, env.Job, "copying "+db+" into "+target)
	return backup.CopyDatabase(ctx, env.shell(), env.Job, env.Report, scratch.PG(), db, dst, target, owner)
}

// loadFromDump downloads and decrypts a dump with the age identity the
// operator sent for this job, and loads it into target.
func loadFromDump(ctx context.Context, env *jobEnv, p restoreParams, dst pg.Local, target string) ([]string, error) {
	t, _, err := steps.BackupTarget(env.Cluster)
	if err != nil {
		return nil, err
	}
	r, err := backup.OpenDump(ctx, t, p.Database, p.Dump, env.Secrets[steps.SecretBackupAgeIdentity])
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	logLine(env.Report, env.Job, "loading dump "+p.Dump+" into "+target)
	return backup.LoadDump(ctx, env.shell(), env.Job, env.Report, io.Reader(r), dst, target, p.Database+"_owner")
}

type restoreDBParams struct {
	Database string `json:"database"`
}

func handleRestorePromote(ctx context.Context, env *Env) (Result, error) {
	var p restoreDBParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("promote replaces "+p.Database+" with its restored copy", p.Database+"/restore")
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	r, ok := bs.OpenRestore(p.Database)
	if !ok || r.Phase != backup.PhaseReady {
		return Result{}, output.NewError(output.CodePrecondition, "there is no restore of "+p.Database+" ready to promote", output.ExitPrecondition,
			"Start one with eru restore "+p.Database+", and check eru restore status.").WithDetails(map[string]any{"reason": "no_ready_restore"})
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "restore": r, "dry_run": true}}, nil
	}
	data, err := promote(ctx, env.Cluster, p.Database, env.Report)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data}, nil
}

// promote swaps <db>__restoring in for db, keeps the old one as a leftover
// and closes the restore.
func promote(ctx context.Context, cfg config.Cluster, db string, report engine.Reporter) (map[string]any, error) {
	bs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	r := bs.Restores[db]
	local, err := clusterPG(cfg, db)
	if err != nil {
		return nil, err
	}
	aside := backup.PreRestoreName(db, time.Now())
	logLine(report, "restore.promote", "swapping "+backup.RestoringName(db)+" in for "+db+"; the old copy becomes "+aside)
	start := time.Now()
	if err := backup.Swap(ctx, local, db, backup.RestoringName(db), aside); err != nil {
		return nil, err
	}
	took := time.Since(start)
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, db)
		s.Leftovers = append(s.Leftovers, backup.Leftover{Database: db, Name: aside, CreatedAt: stamp(), RestoreID: r.ID})
		return nil
	}); err != nil {
		return nil, err
	}
	steps.WriteMetrics(cfg)
	return map[string]any{"database": db, "pre_restore": aside, "target": r.Target, "swap_ms": took.Milliseconds()}, nil
}

func handleRestoreUndo(ctx context.Context, env *Env) (Result, error) {
	var p restoreDBParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("undo swaps "+p.Database+" back to its copy from before the restore", p.Database+"/restore")
	}
	if err := restoreInProgress(p.Database, "undo"); err != nil {
		return Result{}, err
	}
	local, err := clusterPG(env.Cluster, p.Database)
	if err != nil {
		return Result{}, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	leftovers := bs.LeftoversOf(p.Database)
	if len(leftovers) == 0 {
		return Result{}, output.NewError(output.CodePrecondition, "there is no pre-restore copy of "+p.Database+" to go back to", output.ExitPrecondition,
			"restore cleanup may already have dropped it.").WithDetails(map[string]any{"reason": "no_leftover"})
	}
	back := leftovers[len(leftovers)-1]
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "restore_from": back.Name, "dry_run": true}}, nil
	}
	aside := backup.PreRestoreName(p.Database, time.Now())
	if aside == back.Name {
		aside += "b"
	}
	if err := backup.Swap(ctx, local, p.Database, back.Name, aside); err != nil {
		return Result{}, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		kept := s.Leftovers[:0]
		for _, l := range s.Leftovers {
			if l.Name != back.Name {
				kept = append(kept, l)
			}
		}
		s.Leftovers = append(kept, backup.Leftover{Database: p.Database, Name: aside, CreatedAt: stamp(), RestoreID: "undo-" + back.RestoreID})
		return nil
	}); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"database": p.Database, "restored_from": back.Name, "pre_undo": aside}}, nil
}

func handleRestoreAbort(ctx context.Context, env *Env) (Result, error) {
	var p restoreDBParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	r, ok := bs.OpenRestore(p.Database)
	if !ok {
		return Result{}, output.NewError(output.CodePrecondition, "there is no open restore of "+p.Database, output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "no_open_restore"})
	}
	if r.Phase == backup.PhaseRestoring {
		st, err := engine.LoadState(engine.StatePath)
		if err != nil {
			return Result{}, err
		}
		if job, ok := st.Job(r.ID); ok && !job.Final() {
			return Result{}, output.NewError(output.CodePrecondition, "the restore of "+p.Database+" is still loading", output.ExitPrecondition,
				"Wait for it (eru job follow "+r.ID+"), then abort.").WithDetails(map[string]any{"reason": "restore_running", "job_id": r.ID})
		}
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "drop": backup.RestoringName(p.Database), "dry_run": true}}, nil
	}
	local, err := clusterPG(env.Cluster, p.Database)
	if err != nil {
		return Result{}, err
	}
	if err := local.Exec(ctx, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);\n", pg.Quote(backup.RestoringName(p.Database))), nil); err != nil {
		return Result{}, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, p.Database)
		return nil
	}); err != nil {
		return Result{}, err
	}
	steps.WriteMetrics(env.Cluster)
	return Result{Data: map[string]any{"database": p.Database, "dropped": backup.RestoringName(p.Database), "live_untouched": true}}, nil
}

type restoreCleanupParams struct {
	Database string `json:"database"`
	Drop     bool   `json:"drop"`
}

type leftoverInfo struct {
	backup.Leftover
	SizeBytes int64 `json:"size_bytes"`
	Exists    bool  `json:"exists"`
}

func handleRestoreCleanup(ctx context.Context, env *Env) (Result, error) {
	var p restoreCleanupParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	items := []leftoverInfo{}
	for _, l := range bs.LeftoversOf(p.Database) {
		info := leftoverInfo{Leftover: l}
		if local, err := clusterPG(env.Cluster, l.Database); err == nil {
			info.Exists, _ = databaseIn(ctx, local, l.Name)
			if info.Exists {
				info.SizeBytes = databaseSize(ctx, local, l.Name)
			}
		}
		items = append(items, info)
	}
	if !p.Drop || env.Req.DryRun {
		return Result{Data: map[string]any{"leftovers": items, "dropped": false}}, nil
	}
	if p.Database == "" {
		return Result{}, output.NewError(output.CodeUsage, "name the database whose leftovers to drop", output.ExitUsage, "")
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("cleanup drops the pre-restore copies of "+p.Database+"; restore undo is no longer possible", p.Database+"/cleanup")
	}
	var dropped []string
	for _, l := range items {
		if l.Exists {
			local, err := clusterPG(env.Cluster, l.Database)
			if err != nil {
				return Result{}, err
			}
			if err := local.Exec(ctx, "postgres", fmt.Sprintf("DROP DATABASE IF EXISTS %s WITH (FORCE);\n", pg.Quote(l.Name)), nil); err != nil {
				return Result{}, err
			}
		}
		dropped = append(dropped, l.Name)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		kept := s.Leftovers[:0]
		for _, l := range s.Leftovers {
			if !containsString(dropped, l.Name) {
				kept = append(kept, l)
			}
		}
		s.Leftovers = kept
		return nil
	}); err != nil {
		return Result{}, err
	}
	return Result{Data: map[string]any{"leftovers": items, "dropped": true, "names": dropped}}, nil
}

type tableRows struct {
	Table     string `json:"table"`
	Live      *int64 `json:"live_rows"`
	Restoring *int64 `json:"restoring_rows"`
}

type restoreStatus struct {
	backup.Restore
	Restoring string      `json:"restoring_database"`
	Tables    []tableRows `json:"tables,omitempty"`
	OnPromote string      `json:"on_promote"`
}

func handleRestoreStatus(ctx context.Context, env *Env) (Result, error) {
	var p restoreDBParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	var out []restoreStatus
	for db, r := range bs.Restores {
		if p.Database != "" && db != p.Database {
			continue
		}
		st := restoreStatus{Restore: r, Restoring: backup.RestoringName(db),
			OnPromote: "PgBouncer pauses " + db + ", its connections end, " + db + " becomes " + db + "__pre_restore_<time> and " +
				backup.RestoringName(db) + " becomes " + db + "; then PgBouncer resumes. Credentials don't change."}
		if r.Phase == backup.PhaseReady {
			if local, err := clusterPG(env.Cluster, db); err == nil {
				st.Tables = compareRows(ctx, local, db, backup.RestoringName(db))
			}
		}
		out = append(out, st)
	}
	return Result{Data: map[string]any{"restores": out, "leftovers": bs.LeftoversOf(p.Database)}}, nil
}

const rowEstimateSQL = `SELECT n.nspname || '.' || c.relname AS t, c.reltuples::bigint AS r
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p') AND n.nspname NOT IN ('pg_catalog', 'information_schema') AND n.nspname NOT LIKE 'pg_toast%'`

// compareRows lists the row estimates (pg_class.reltuples) of every table in
// both databases of the cluster at local. Cheap, approximate, and enough to
// see that a rewind took.
func compareRows(ctx context.Context, local pg.Local, live, restoring string) []tableRows {
	read := func(db string) map[string]int64 {
		rows, err := local.Query(ctx, db, rowEstimateSQL, nil)
		out := map[string]int64{}
		if err != nil {
			return out
		}
		for _, r := range rows {
			n, _ := strconv.ParseInt(r["r"], 10, 64)
			out[r["t"]] = n
		}
		return out
	}
	a, b := read(live), read(restoring)
	names := map[string]bool{}
	for k := range a {
		names[k] = true
	}
	for k := range b {
		names[k] = true
	}
	var out []tableRows
	for name := range names {
		row := tableRows{Table: name}
		if v, ok := a[name]; ok {
			v := v
			row.Live = &v
		}
		if v, ok := b[name]; ok {
			v := v
			row.Restoring = &v
		}
		out = append(out, row)
	}
	sortTables(out)
	return out
}

func sortTables(rows []tableRows) {
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].Table < rows[j-1].Table; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
}

// --- db fork ---

type dbForkParams struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	At     string `json:"at,omitempty"`
}

// handleDBFork creates the new database through the db create path (its
// [[clusters]] entry and four roles, with operator-generated passwords),
// then copies the source into it as a detachable job: from the live database
// without --at, from a scratch restore at that time with it.
func handleDBFork(ctx context.Context, env *Env) (Result, error) {
	var p dbForkParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	src, ok := cfg.ClusterByName(p.Source)
	if !ok {
		return Result{}, databaseNotFound(p.Source)
	}
	if err := config.CheckDatabaseName(p.Name); err != nil {
		return Result{}, output.NewError(output.CodeValidation, "database name "+p.Name+" "+err.Error(), output.ExitConfig, "Pass a shorter --name.")
	}
	if err := restoreInProgress(p.Source, "db fork"); err != nil {
		return Result{}, err
	}
	if p.At != "" {
		if err := requireBackups(cfg); err != nil {
			return Result{}, err
		}
		if _, err := checkRestoreTarget(ctx, cfg, restoreParams{Database: p.Source, Kind: backup.TargetTime, Time: p.At}, nil); err != nil {
			return Result{}, err
		}
	}
	if _, err := restoreSpace(ctx, cfg, p.Source, p.At != ""); err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"source": p.Source, "name": p.Name, "at": p.At, "dry_run": true}}, nil
	}
	release, err := engine.Acquire(ctx, engine.LockExclusive, "db.fork", env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return Result{}, err
	}
	created, warnings, err := createDatabase(ctx, env, config.ClusterSpec{Name: p.Name, Size: src.Size, Memory: src.Memory, Profile: src.Profile,
		StatementTimeout: src.StatementTimeout, Extensions: src.Extensions}, false)
	release()
	if err != nil {
		return Result{}, err
	}
	env.Cluster, _, _ = state.FileStore{}.Load()
	res, err := runJobRPC(ctx, env, "db-fork", p, nil)
	if err != nil {
		return res, err
	}
	if m, ok := res.Data.(map[string]any); ok {
		m["database"] = created
	}
	res.Warnings = append(res.Warnings, warnings...)
	return res, nil
}

func jobFork(ctx context.Context, env *jobEnv) (any, error) {
	var p dbForkParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	db, ok := env.Cluster.ClusterByName(p.Name)
	if !ok {
		return nil, databaseNotFound(p.Name)
	}
	src, err := clusterPG(env.Cluster, p.Source)
	if err != nil {
		return nil, err
	}
	dst := steps.ClusterPG(db)
	owner := p.Name + "_owner"
	var warnings []string
	if p.At == "" {
		logLine(env.Report, env.Job, "copying the live "+p.Source+" into "+p.Name)
		warnings, err = backup.CopyDatabase(ctx, env.shell(), env.Job, env.Report, src, p.Source, dst, p.Name, owner)
	} else {
		t, perr := time.Parse(time.RFC3339, p.At)
		if perr != nil {
			return nil, perr
		}
		warnings, err = loadFromScratch(ctx, env, backup.RestoreOptions{Database: p.Source, Kind: backup.TargetTime, Time: t}, p.Source, dst, p.Name, owner)
	}
	if err != nil {
		return nil, err
	}
	if err := finishLoad(ctx, dst, p.Name, db); err != nil {
		return nil, err
	}
	return map[string]any{"source": p.Source, "name": p.Name, "at": p.At, "warnings": warnings}, nil
}

// restoreNameRefusal refuses eru restore's own databases in db commands.
func restoreNameRefusal(name string) error {
	if !config.IsRestoreName(name) {
		return nil
	}
	db := strings.SplitN(strings.TrimSuffix(name, config.RestoringSuffix), config.PreRestoreMarker, 2)[0]
	return output.NewError(output.CodeValidation, name+" belongs to eru restore", output.ExitConfig,
		"Use eru restore abort "+db+" or eru restore cleanup "+db+".").WithDetails(map[string]any{"reason": "reserved_name"})
}
