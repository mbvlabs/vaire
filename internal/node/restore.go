package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/hostinfo"
	"github.com/mbvlabs/eru/internal/pg"
	"github.com/mbvlabs/eru/internal/secrets"
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
func clearFailedRestore(cfg config.Cluster, db string) error {
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
	if err := dropRestoring(cfg, db, nil); err != nil {
		return err
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		if cur, ok := s.Restores[db]; ok && cur.ID == r.ID {
			delete(s.Restores, db)
		}
		return nil
	})
}

// dropRestoring drops the temporary cluster <db>__restoring, if any.
func dropRestoring(cfg config.Cluster, db string, report engine.Reporter) error {
	name := backup.RestoringName(db)
	return backup.DropTemporaryCluster(engine.Shell{Secrets: secrets.Default}, "restore.abort", report, name, filepath.Join(cfg.Server.DataRoot, name))
}

// checkRestorable checks db is a configured database.
func checkRestorable(cfg config.Cluster, db string) error {
	if _, ok := cfg.ClusterByName(db); !ok {
		return databaseNotFound(db)
	}
	return nil
}

// restoringInTheWay refuses a restore while a cluster named
// <db>__restoring exists that no open restore owns: a leftover of a crash,
// or a database of that name.
func restoringInTheWay(ctx context.Context, cfg config.Cluster, db string) error {
	name := backup.RestoringName(db)
	_, statErr := os.Stat(filepath.Join(cfg.Server.DataRoot, name))
	if !steps.ClusterExists(ctx, name) && statErr != nil {
		return nil
	}
	hint := "Drop it with eru restore abort " + db + "."
	if _, ok := cfg.ClusterByName(name); ok {
		hint = "It is the database " + name + "; rename one of them or drop it with eru db drop " + name + "."
	}
	return output.NewError(output.CodePrecondition, "cluster 18/"+name+" is in the way of a restore of "+db, output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": "restoring_exists", "cluster": name})
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
		err = clearFailedRestore(cfg, p.Database)
	}
	if err == nil {
		err = restoreInProgress(p.Database, "a new restore")
	}
	if err == nil {
		err = restoringInTheWay(ctx, cfg, p.Database)
	}
	var space []backup.SpaceNeed
	if err == nil {
		space, err = restoreSpace(ctx, cfg, p.Database, false)
	}
	timeline := ""
	if err == nil {
		timeline, err = restoreTimeline(cfg, p)
	}
	releaseCluster()
	release()
	if err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "target": target, "timeline": timeline, "space": space, "dry_run": true}}, nil
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		if _, open := s.Restores[p.Database]; open {
			return restoreInProgress(p.Database, "a new restore")
		}
		s.Restores[p.Database] = backup.Restore{
			ID: env.Req.RequestID, Database: p.Database, Kind: p.Kind, Target: target,
			Phase: backup.PhaseRestoring, StartedAt: stamp(), Promote: p.Promote, Timeline: timeline,
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

// restoreTimeline is the timeline a restore of p's database follows: the one
// restore undo recorded for its stanza, or pgBackRest's latest (empty). A
// dump has none.
func restoreTimeline(cfg config.Cluster, p restoreParams) (string, error) {
	if p.Kind == targetDump {
		return "", nil
	}
	spec, err := clusterOf(cfg, p.Database)
	if err != nil {
		return "", err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return "", err
	}
	return bs.TargetTimeline(p.Database, spec.Stanza), nil
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
			return "", olderStanzaHint(err, p.Database, spec.Stanza)
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

// olderStanzaHint names db's dropped stanza in a target-outside-the-window
// refusal when one is still kept under another name: after a restore from a
// dump, the history from before the dump is in the old stanza, which a
// restore doesn't search.
func olderStanzaHint(err error, db, stanza string) error {
	bs, lerr := backup.LoadRecords()
	if lerr != nil {
		return err
	}
	d, ok := bs.Dropped(db)
	if !ok || d.Stanza == stanza {
		return err
	}
	return withHint(err, "Points before "+d.DroppedAt+" are in the older stanza "+d.Stanza+" (kept until "+d.ExpiresAt+"; eru backup list --db "+db+"), which eru restore doesn't read.")
}

// restoreSpace checks room beside the live data directory for one copy of
// db: the temporary cluster of a restore or a fork. scratch adds the old
// fork's scratch instance until db fork moves onto the materialised cluster.
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

// jobRestore materialises the target into the temporary cluster
// <db>__restoring beside the live database, which keeps serving, adds it to
// the session pooler and leaves the restore ready for inspection, or
// promotes it straight away with --promote.
func jobRestore(ctx context.Context, env *jobEnv) (any, error) {
	var p restoreParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	port, warnings, err := materialiseRestore(ctx, env, p)
	if err != nil {
		failRestore(env, err)
		return nil, err
	}
	readyAt := stamp()
	_ = backup.UpdateRecords(func(s *backup.Records) error {
		r := s.Restores[p.Database]
		r.Phase, r.ReadyAt, r.Warnings, r.Error, r.Port = backup.PhaseReady, readyAt, warnings, "", port
		s.Restores[p.Database] = r
		return nil
	})
	rt := jobRuntime(env)
	res := map[string]any{"database": p.Database, "restoring": backup.RestoringName(p.Database), "port": port, "phase": backup.PhaseReady, "warnings": warnings}
	if p.Promote {
		promoted, err := promote(ctx, env.Cluster, p.Database, rt, env.Report)
		if err != nil {
			return res, err
		}
		res["phase"] = "promoted"
		res["promoted"] = promoted
	} else if err := applyPoolers(ctx, env.Cluster, rt, env.Report); err != nil {
		res["pooler_warning"] = "the session pooler was not reloaded, so eru sql --db " + backup.RestoringName(p.Database) + " fails until eru apply: " + err.Error()
	}
	steps.WriteMetrics(env.Cluster)
	return res, nil
}

// jobRuntime is the step runtime for a job's applies: request secrets aren't
// needed, the steps it runs read theirs from the node.
func jobRuntime(env *jobEnv) engine.Runtime {
	host, _ := hostinfo.Detect(env.Cluster.Server.DataRoot)
	return engine.Runtime{Shell: env.shell(), Secrets: secrets.Default, Host: host}
}

// applyPoolers runs pgbouncer-v1, which renders both poolers' entries,
// including the session pooler's for ready restores, and reloads them.
func applyPoolers(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.PoolerStepIDs(), Report: report})
	return err
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

// materialiseRestore builds the temporary cluster <db>__restoring: a
// pgBackRest restore of the database's stanza paused at the target (a
// standby following the archive for latest), or a new cluster loaded from a
// dump. It returns the cluster's port. A failure drops what was built.
func materialiseRestore(ctx context.Context, env *jobEnv, p restoreParams) (int, []string, error) {
	cfg := env.Cluster
	spec, err := clusterOf(cfg, p.Database)
	if err != nil {
		return 0, nil, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return 0, nil, err
	}
	m, err := backup.NewMaterialised(cfg.Server.DataRoot, backup.RestoringName(p.Database), p.Database, spec.Port, env.shell(), env.Job, env.Report)
	if err != nil {
		return 0, nil, err
	}
	var warnings []string
	if p.Kind == targetDump {
		warnings, err = loadFromDump(ctx, env, p, spec, m)
	} else {
		opts := backup.RestoreOptions{Kind: p.Kind, Set: p.Set, Pause: true, Timeline: bs.Restores[p.Database].Timeline}
		if p.Kind == backup.TargetTime {
			opts.Time, _ = time.Parse(time.RFC3339, p.Time)
		}
		err = m.Restore(ctx, driver(spec, env.Job, env.Report), opts)
		if err == nil {
			if t := m.RecoveredTo(ctx); t != "" {
				logLine(env.Report, env.Job, "recovered to "+t)
			}
		}
	}
	if err != nil {
		_ = m.Drop()
		return 0, warnings, err
	}
	return m.Port, warnings, nil
}

// loadFromDump creates the empty cluster m, copies the live cluster's roles
// into it, creates the database with eru's grants, then downloads and
// decrypts the dump with the age identity the operator sent for this job
// and loads it.
func loadFromDump(ctx context.Context, env *jobEnv, p restoreParams, spec config.ClusterSpec, m *backup.Materialised) ([]string, error) {
	t, _, err := steps.BackupTarget(env.Cluster)
	if err != nil {
		return nil, err
	}
	r, err := backup.OpenDump(ctx, t, p.Database, p.Dump, env.Secrets[steps.SecretBackupAgeIdentity])
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	if err := m.Initdb(ctx, env.Cluster.Postgres.LocaleProvider, env.Cluster.Postgres.Locale); err != nil {
		return nil, err
	}
	if err := m.CopyRoles(ctx); err != nil {
		return nil, err
	}
	local := m.PG()
	sql := steps.AuthFunctionsSQL() + fmt.Sprintf("CREATE DATABASE %s OWNER %s;\n", pg.Quote(spec.Name), pg.Quote(spec.Name+"_owner")) +
		steps.DatabaseConnectSQL(spec.Name, spec.Name)
	if err := local.Exec(ctx, "postgres", sql, nil); err != nil {
		return nil, err
	}
	if err := local.Exec(ctx, spec.Name, steps.DatabaseGrantsSQL(spec), nil); err != nil {
		return nil, err
	}
	logLine(env.Report, env.Job, "loading dump "+p.Dump+" into "+m.Name)
	warnings, err := backup.LoadDump(ctx, env.shell(), env.Job, env.Report, io.Reader(r), local, spec.Name, spec.Name+"_owner")
	if err != nil {
		return warnings, err
	}
	return warnings, finishLoad(ctx, local, spec.Name, spec)
}

// finishLoad re-runs the grants after a load and analyzes, so restore
// status can compare row estimates.
func finishLoad(ctx context.Context, local pg.Local, target string, db config.ClusterSpec) error {
	if err := local.Exec(ctx, target, steps.DatabaseGrantsSQL(db), nil); err != nil {
		return err
	}
	return local.Exec(ctx, target, "ANALYZE;\n", nil)
}

type restoreDBParams struct {
	Database string `json:"database"`
}

// lockForCutover takes the node lock shared and db's cluster lock
// exclusive: a promote or undo stops the live cluster.
func lockForCutover(ctx context.Context, env *Env, db, method string) (func(), error) {
	release, err := engine.Acquire(ctx, engine.LockShared, method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		return nil, err
	}
	releaseCluster, err := engine.AcquireCluster(ctx, db, engine.LockExclusive, method, env.Req.RequestID, env.Req.LockTimeout())
	if err != nil {
		release()
		return nil, err
	}
	return func() { releaseCluster(); release() }, nil
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
	release, err := lockForCutover(ctx, env, p.Database, "restore.promote")
	if err != nil {
		return Result{}, err
	}
	defer release()
	data, err := promote(ctx, env.Cluster, p.Database, runtimeFor(env, false), env.Report)
	if err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	if w, ok := data["warning"].(output.Warning); ok {
		warnings = append(warnings, w)
		delete(data, "warning")
	}
	return Result{Data: data, Warnings: warnings}, nil
}

// promote cuts db over to its restored copy (RFC 10.1): the transaction
// pooler pauses db, the live cluster and the copy stop, the copy's data
// directory takes the live one's place and the old one becomes the stopped
// leftover cluster <db>__pre_restore_<ts>. 18/<db> starts with its own
// configuration, cgroup and archive_command, replays to the target again
// and is promoted onto a new timeline, archiving to the same stanza. A
// restore from a dump has a new system identifier, so db gets a new stanza
// and the old one is kept as a dropped stanza. Then a full backup starts.
// A failed backup start is data["warning"], not an error: the cut-over is
// done.
func promote(ctx context.Context, cfg config.Cluster, db string, rt engine.Runtime, report engine.Reporter) (map[string]any, error) {
	bs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	r := bs.Restores[db]
	spec, err := clusterOf(cfg, db)
	if err != nil {
		return nil, err
	}
	const step = "restore.promote"
	aside := backup.PreRestoreName(db, time.Now())
	logLine(report, step, "cutting "+db+" over to "+backup.RestoringName(db)+"; the old data becomes the cluster "+aside)
	start := time.Now()
	resume := steps.PauseDatabase(ctx, db)
	err = backup.Swap(ctx, rt.Shell, step, report, cfg.Server.DataRoot, db, backup.RestoringName(db), aside, r.Port)
	if err == nil {
		until := backup.UntilPaused
		if r.Kind == backup.TargetLatest || r.Kind == targetDump {
			until = backup.UntilOpen
		}
		err = backup.StartCluster(ctx, rt.Shell, step, report, db, cfg.Server.DataDirectory(db), spec.Port, until)
	}
	if err == nil {
		err = backup.Promote(ctx, steps.ClusterPG(spec))
	}
	resume()
	if err != nil {
		return nil, withHint(err, "Read eru logs postgres --db "+db+" and eru restore status "+db+"; the old data is in the cluster "+aside+".")
	}
	took := time.Since(start)
	newStanza := ""
	if r.Kind == targetDump {
		if cfg, newStanza, err = switchStanza(cfg, db, ""); err != nil {
			return nil, err
		}
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, db)
		delete(s.Timelines, db)
		s.Leftovers = append(s.Leftovers, backup.Leftover{Database: db, Name: aside, CreatedAt: stamp(), RestoreID: r.ID, Port: r.Port, Stanza: spec.Stanza})
		if newStanza != "" {
			s.DroppedStanzas = append(s.DroppedStanzas, droppedStanza(cfg, spec))
		}
		return nil
	}); err != nil {
		return nil, err
	}
	data := map[string]any{"database": db, "pre_restore": aside, "target": r.Target, "swap_ms": took.Milliseconds()}
	var warn error
	if err := applyPoolers(ctx, cfg, rt, report); err != nil {
		warn = err
	}
	if newStanza != "" {
		data["stanza"] = newStanza
		if err := applyStanza(ctx, cfg, db, rt, report); err != nil {
			warn = err
		}
	} else if err := startFullBackup(ctx, cfg, rt, db, step, report); err != nil {
		warn = err
	}
	if warn != nil {
		data["warning"] = output.Warning{Code: "after_cutover_failed", Message: db + " is restored, but " + warn.Error() + "; run eru apply, then eru backup now --db " + db + " --type full"}
	}
	steps.WriteMetrics(cfg)
	return data, nil
}

// droppedStanza records spec's stanza as dropped now, kept for its window.
func droppedStanza(cfg config.Cluster, spec config.ClusterSpec) backup.DroppedStanza {
	now := time.Now().UTC()
	return backup.DroppedStanza{Database: spec.Name, Stanza: spec.Stanza, DroppedAt: now.Format(time.RFC3339),
		ExpiresAt: now.AddDate(0, 0, 7*cfg.RetentionFull(spec)).Format(time.RFC3339)}
}

// switchStanza gives db the stanza to (a new one when empty) in
// cluster.toml: an exception to "stanza immutable" that only restore makes,
// when the cluster's system identifier changes (a restore from a dump, or
// undo of one). It returns the saved configuration.
func switchStanza(cfg config.Cluster, db, to string) (config.Cluster, string, error) {
	store := state.FileStore{}
	cur, _, err := store.Load()
	if err != nil {
		return cfg, "", err
	}
	if to == "" {
		if to, err = config.NewStanza(db); err != nil {
			return cfg, "", err
		}
	}
	for i := range cur.Clusters {
		if cur.Clusters[i].Name == db {
			cur.Clusters[i].Stanza = to
		}
	}
	raw, err := cur.Encode()
	if err != nil {
		return cfg, "", err
	}
	if err := store.Save(raw); err != nil {
		return cfg, "", err
	}
	return cur, to, nil
}

// applyStanza points db's archive_command at its stanza (its own steps, a
// reload), then runs pgbackrest-v1 (the stanza's section, stanza-create,
// check and a first full when it has none) and timers-v1.
func applyStanza(ctx context.Context, cfg config.Cluster, db string, rt engine.Runtime, report engine.Reporter) error {
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{DB: db, Report: report}); err != nil {
		return err
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...), Report: report})
	return err
}

// startFullBackup starts db's full backup job through its timer's template
// service without waiting for it. After a promote or undo the new timeline
// needs a full of its own.
func startFullBackup(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db, step string, report engine.Reporter) error {
	if !cfg.BackupConfigured() {
		return nil
	}
	return rt.Shell.Exec(ctx, step, engine.Command{Name: "systemctl", Args: []string{"start", "--no-block", "eru-backup-full@" + db + ".service"}}, report)
}

func handleRestoreUndo(ctx context.Context, env *Env) (Result, error) {
	var p restoreDBParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	if env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("undo swaps "+p.Database+" back to its data from before the restore", p.Database+"/restore")
	}
	if err := restoreInProgress(p.Database, "undo"); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	spec, err := clusterOf(cfg, p.Database)
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
	release, err := lockForCutover(ctx, env, p.Database, "restore.undo")
	if err != nil {
		return Result{}, err
	}
	defer release()
	const step = "restore.undo"
	rt := runtimeFor(env, false)
	local := steps.ClusterPG(spec)
	abandoned, _ := backup.Timeline(ctx, local)
	aside := backup.PreRestoreName(p.Database, time.Now())
	if aside == back.Name {
		aside += "b"
	}
	logLine(env.Report, step, "swapping "+p.Database+" back to "+back.Name+"; the restored data becomes the cluster "+aside)
	resume := steps.PauseDatabase(ctx, p.Database)
	err = backup.Swap(ctx, rt.Shell, step, env.Report, cfg.Server.DataRoot, p.Database, back.Name, aside, back.Port)
	if err == nil {
		err = backup.StartCluster(ctx, rt.Shell, step, env.Report, p.Database, cfg.Server.DataDirectory(p.Database), spec.Port, backup.UntilOpen)
	}
	resume()
	if err != nil {
		return Result{}, withHint(err, "Read eru logs postgres --db "+p.Database+"; the restored data is in the cluster "+aside+".")
	}
	timeline, err := backup.Timeline(ctx, local)
	if err != nil {
		return Result{}, err
	}
	// Undo of a restore from a dump brings back the cluster of the old
	// stanza: it returns to cluster.toml, and the dump's stanza is dropped.
	stanza := spec.Stanza
	if back.Stanza != "" && back.Stanza != spec.Stanza {
		if cfg, stanza, err = switchStanza(cfg, p.Database, back.Stanza); err != nil {
			return Result{}, err
		}
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		kept := s.Leftovers[:0]
		for _, l := range s.Leftovers {
			if l.Name != back.Name {
				kept = append(kept, l)
			}
		}
		s.Leftovers = append(kept, backup.Leftover{Database: p.Database, Name: aside, CreatedAt: stamp(), RestoreID: "undo-" + back.RestoreID, Port: back.Port, Stanza: spec.Stanza})
		if stanza != spec.Stanza {
			dropped := s.DroppedStanzas[:0]
			for _, d := range s.DroppedStanzas {
				if d.Stanza != stanza {
					dropped = append(dropped, d)
				}
			}
			s.DroppedStanzas = append(dropped, droppedStanza(cfg, spec))
		}
		s.Timelines[p.Database] = backup.TimelineSwitch{Stanza: stanza, Timeline: timeline, SwitchedAt: stamp(), Abandoned: abandoned}
		return nil
	}); err != nil {
		return Result{}, err
	}
	data := map[string]any{"database": p.Database, "restored_from": back.Name, "pre_undo": aside, "timeline": timeline, "stanza": stanza}
	var warnings []output.Warning
	if stanza != spec.Stanza {
		err = applyStanza(ctx, cfg, p.Database, rt, env.Report)
	}
	if err == nil {
		err = startFullBackup(ctx, cfg, rt, p.Database, step, env.Report)
	}
	if err != nil {
		warnings = append(warnings, output.Warning{Code: "after_cutover_failed",
			Message: p.Database + " is back, but " + err.Error() + "; run eru apply, then eru backup now --db " + p.Database + " --type full"})
	}
	steps.WriteMetrics(cfg)
	return Result{Data: data, Warnings: warnings}, nil
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
			return Result{}, output.NewError(output.CodePrecondition, "the restore of "+p.Database+" is still materialising", output.ExitPrecondition,
				"Wait for it (eru job follow "+r.ID+"), then abort.").WithDetails(map[string]any{"reason": "restore_running", "job_id": r.ID})
		}
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "drop": backup.RestoringName(p.Database), "dry_run": true}}, nil
	}
	if err := dropRestoring(env.Cluster, p.Database, env.Report); err != nil {
		return Result{}, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, p.Database)
		return nil
	}); err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	if err := reloadPoolers(ctx, env, env.Cluster); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the session pooler still lists " + backup.RestoringName(p.Database) + " until eru apply: " + err.Error()})
	}
	steps.WriteMetrics(env.Cluster)
	return Result{Data: map[string]any{"database": p.Database, "dropped": backup.RestoringName(p.Database), "live_untouched": true}, Warnings: warnings}, nil
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
		info := leftoverInfo{Leftover: l, Exists: steps.ClusterExists(ctx, l.Name)}
		info.SizeBytes = dirSize(filepath.Join(env.Cluster.Server.DataRoot, l.Name))
		items = append(items, info)
	}
	if !p.Drop || env.Req.DryRun {
		return Result{Data: map[string]any{"leftovers": items, "dropped": false}}, nil
	}
	if p.Database == "" {
		return Result{}, output.NewError(output.CodeUsage, "name the database whose leftovers to drop", output.ExitUsage, "")
	}
	if env.Req.Confirm == "" {
		return Result{}, confirmRequired("cleanup drops the pre-restore clusters of "+p.Database+"; restore undo is no longer possible", p.Database+"/cleanup")
	}
	var dropped []string
	for _, l := range items {
		if err := backup.DropTemporaryCluster(runtimeFor(env, false).Shell, "restore.cleanup", env.Report, l.Name,
			filepath.Join(env.Cluster.Server.DataRoot, l.Name)); err != nil {
			return Result{}, err
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

// dirSize is the apparent size of the files under dir; 0 when it is gone.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
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
			OnPromote: "PgBouncer pauses " + db + ", its cluster and the copy stop, the copy's data directory takes the live one's place and the old one is kept as the cluster " +
				db + "__pre_restore_<time>; " + db + " starts, reaches the target and is promoted (a few seconds plus the replay), then PgBouncer resumes and a full backup starts. Credentials don't change."}
		if r.Kind == targetDump {
			st.OnPromote += " A restore from a dump gives " + db + " a new stanza; the old one is kept for its window."
		}
		if r.Phase == backup.PhaseReady && r.Port != 0 {
			if local, err := clusterPG(env.Cluster, db); err == nil {
				st.Tables = compareRows(ctx, local, pg.Local{Host: steps.PGSocketDir, Port: r.Port}, db)
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
// database db on the live cluster and on the restored copy. Cheap,
// approximate, and enough to see that a rewind took.
func compareRows(ctx context.Context, live, restoring pg.Local, db string) []tableRows {
	read := func(local pg.Local) map[string]int64 {
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

// loadFromScratch restores db at opts into a scratch instance and copies it
// into target on the cluster at dst, as owner, for db fork --at until it
// moves onto the materialised cluster (unit 31). The scratch instance is
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
		warnings, err = loadFromScratch(ctx, env, backup.RestoreOptions{Kind: backup.TargetTime, Time: t}, p.Source, dst, p.Name, owner)
	}
	if err != nil {
		return nil, err
	}
	if err := finishLoad(ctx, dst, p.Name, db); err != nil {
		return nil, err
	}
	return map[string]any{"source": p.Source, "name": p.Name, "at": p.At, "warnings": warnings}, nil
}

// dropLeftovers drops db's restore leftover clusters and their records: db
// drop takes them with the database, as they were its data before a
// promote or undo.
func dropLeftovers(env *Env, db string) error {
	bs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	for _, l := range bs.LeftoversOf(db) {
		if err := backup.DropTemporaryCluster(runtimeFor(env, false).Shell, "db.drop", env.Report, l.Name,
			filepath.Join(env.Cluster.Server.DataRoot, l.Name)); err != nil {
			return err
		}
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		kept := s.Leftovers[:0]
		for _, l := range s.Leftovers {
			if l.Database != db {
				kept = append(kept, l)
			}
		}
		s.Leftovers = kept
		delete(s.Timelines, db)
		return nil
	})
}
