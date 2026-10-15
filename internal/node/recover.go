package node

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/masterbackup"
	"github.com/mbvlabs/eru/internal/steps"
)

// db recover (Phase 5, D8): bring back a database awaiting recovery after
// server init --restore, from its stanza. It is a restore into a database
// whose cluster is an empty scaffold, on the Phase 3 path: the database's
// own cluster and configuration steps build 18/<db> empty (archiving off),
// the stanza is materialised into <db>__restoring beside it, and promote
// swaps the copy in, so 18/<db> starts with its own configuration, limits
// and archive_command and opens on a new timeline archiving to the same
// stanza. The scaffold promote set aside is dropped at once.

// recoverHeavyIOWait bounds the wait for the heavy-I/O lock: a backup of a
// database recovered earlier may hold it.
const recoverHeavyIOWait = 6 * time.Hour

// recoverParams are db.recover's params.
type recoverParams struct {
	Database  string `json:"database"`
	Time      string `json:"time,omitempty"`
	NoPromote bool   `json:"no_promote,omitempty"`
	NoFull    bool   `json:"no_full,omitempty"`
}

// recoverToken is <db>/recover, needed only for --time: a recovery to a
// time leaves out what came after it.
func recoverToken(_ config.Cluster, params json.RawMessage) string {
	var p recoverParams
	if json.Unmarshal(params, &p) != nil || p.Database == "" || p.Time == "" {
		return ""
	}
	return p.Database + "/recover"
}

// recoverAtTime reports whether a db.recover request targets a time, which
// a protected database refuses (D10).
func recoverAtTime(params json.RawMessage) bool {
	var p recoverParams
	return json.Unmarshal(params, &p) == nil && p.Time != ""
}

func (p recoverParams) restore() restoreParams {
	r := restoreParams{Database: p.Database, Kind: backup.TargetLatest}
	if p.Time != "" {
		r.Kind, r.Time = backup.TargetTime, p.Time
	}
	return r
}

func handleDBRecover(ctx context.Context, env *Env) (Result, error) {
	var p recoverParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	cfg := env.Cluster
	spec, err := clusterOf(cfg, p.Database)
	if err != nil {
		return Result{}, err
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		return Result{}, err
	}
	if !recs.IsAwaiting(p.Database) {
		return Result{}, output.NewError(output.CodePrecondition, "database "+p.Database+" isn't awaiting recovery", output.ExitPrecondition,
			"db recover is for databases a rebuilt master hasn't recovered. To go back in time on a running database use eru restore "+p.Database+".").
			WithDetails(map[string]any{"reason": "not_awaiting_recovery", "database": p.Database})
	}
	if !cfg.BackupConfigured() {
		return Result{}, output.NewError(output.CodePrecondition, "there is no bucket, so there is no stanza to recover "+p.Database+" from", output.ExitPrecondition,
			"The data is lost with the old server. Create the database again: eru db drop "+p.Database+" --confirm "+p.Database+", then eru db create "+p.Database+".").
			WithDetails(map[string]any{"reason": "no_offsite_backup", "database": p.Database})
	}
	if p.Time != "" && env.Req.Confirm == "" && !env.Req.DryRun {
		return Result{}, confirmRequired("a recovery to a time leaves out everything "+p.Database+" wrote after it", p.Database+"/recover")
	}
	if err := requireNotFenced(ctx, cfg, env.Report); err != nil {
		return Result{}, err
	}
	if err := stanzaHasBackup(ctx, spec, recs); err != nil {
		return Result{}, err
	}
	if err := stanzaStillWritten(ctx, cfg, spec); err != nil {
		return Result{}, err
	}
	rp := p.restore()
	target, err := checkRestoreTarget(ctx, cfg, rp, env.Req.Secrets)
	if err != nil {
		return Result{}, err
	}
	timeline := recs.TargetTimeline(p.Database, spec.Stanza)
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "stanza": spec.Stanza, "target": target, "timeline": timeline, "dry_run": true}}, nil
	}
	if err := clearFailedRestore(cfg, p.Database); err != nil {
		return Result{}, err
	}
	if err := restoreInProgress(p.Database, "a recovery"); err != nil {
		return Result{}, err
	}
	if err := restoringInTheWay(ctx, cfg, p.Database); err != nil {
		return Result{}, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores[p.Database] = backup.Restore{
			ID: env.Req.RequestID, Database: p.Database, Kind: rp.Kind, Target: target,
			Phase: backup.PhaseRestoring, StartedAt: stamp(), Promote: !p.NoPromote, Timeline: timeline,
			Recover: true, NoFull: p.NoFull,
		}
		return nil
	}); err != nil {
		return Result{}, err
	}
	// The recovered database's steps (postgres-roles-v1, metrics-v1,
	// pgbackrest-v1) run inside the job: a rebuilt master has no service
	// password files until they write them from these.
	return runJobRPC(ctx, env, "recover", p, stepSecrets(env.Req.Secrets))
}

// stanzaHasBackup refuses a stanza without a backup set: a database created
// moments before the loss, or a dump-restored one whose first full never
// finished.
func stanzaHasBackup(ctx context.Context, spec config.ClusterSpec, recs backup.Records) error {
	info, err := driver(spec, "db.recover", nil).Info(ctx, "")
	if err != nil {
		return err
	}
	if _, ok := info.Latest(); ok {
		return nil
	}
	hint := "Create it again: eru db drop " + spec.Name + " --confirm " + spec.Name + ", eru db create " + spec.Name +
		", then restore a long-retention dump with eru restore " + spec.Name + " --dump <id> (eru dump list " + spec.Name + ")."
	if d, ok := recs.Dropped(spec.Name); ok && d.Stanza != spec.Stanza {
		hint += " Older history is in the stanza " + d.Stanza + " (kept until " + d.ExpiresAt + ")."
	}
	return output.NewError(output.CodePrecondition, "the stanza "+spec.Stanza+" of "+spec.Name+" has no backup to recover from", output.ExitPrecondition, hint).
		WithDetails(map[string]any{"reason": "no_backup_in_stanza", "database": spec.Name, "stanza": spec.Stanza})
}

// stanzaStillWritten is the split-brain check (D8): a WAL segment that
// reached the stanza after the restore wrote the epoch came from the old
// master, which is still running. Without a local epoch time there is
// nothing to compare.
func stanzaStillWritten(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec) error {
	e, err := masterbackup.ReadLocalEpoch("")
	if err != nil {
		return err
	}
	fence := e.WrittenTime()
	if fence.IsZero() {
		return nil
	}
	// The bucket's own time of the epoch object, when it is still ours:
	// written_at comes from the operator's clock.
	if t, _, err := steps.BackupTarget(cfg); err == nil {
		if cur, err := backup.ReadEpoch(ctx, t); err == nil && cur != nil && cur.InstanceID == e.InstanceID && cur.Epoch == e.Epoch {
			if at, err := backup.EpochModified(ctx, t); err == nil && !at.IsZero() {
				fence = at
			}
		}
	}
	return stillWrittenSince(ctx, cfg, spec, fence)
}

func stillWrittenSince(ctx context.Context, cfg config.Cluster, spec config.ClusterSpec, fence time.Time) error {
	if fence.IsZero() {
		return nil
	}
	t, _, err := steps.BackupTarget(cfg)
	if err != nil {
		return err
	}
	key, newest, err := backup.NewestArchive(ctx, t, spec.Stanza)
	if err != nil {
		return err
	}
	if !writtenAfterFence(newest, fence) {
		return nil
	}
	return output.NewError(output.CodePrecondition,
		"the stanza "+spec.Stanza+" received WAL at "+newest.UTC().Format(time.RFC3339)+", after this master took over: the old master is still running",
		output.ExitPrecondition,
		"Power off or destroy the old server first; its fence check stops it within five minutes when it can reach the bucket. Then run eru db recover "+spec.Name+" again.").
		WithDetails(map[string]any{"reason": "stanza_still_written", "database": spec.Name, "stanza": spec.Stanza, "object": key, "written_at": newest.UTC().Format(time.RFC3339)})
}

// writtenAfterFence reports whether an archive object written at newest
// came after the fence (a minute of slack for clock skew).
func writtenAfterFence(newest, fence time.Time) bool {
	return !newest.IsZero() && newest.After(fence.Add(time.Minute))
}

// scaffoldSteps build the database's empty cluster with its own
// configuration; its roles and database come with the recovered data.
func scaffoldSteps(db string) []string {
	return []string{steps.KernelStepIDs()[0], engine.InstanceID("postgres-cluster-v1", db), steps.ConfigStepID(db)}
}

// jobRecover recovers one database (D8).
func jobRecover(ctx context.Context, env *jobEnv) (any, error) {
	var p recoverParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	res, err := recoverDatabase(ctx, env, p)
	if err != nil {
		failRestore(env, err)
		return nil, err
	}
	return res, nil
}

func recoverDatabase(ctx context.Context, env *jobEnv, p recoverParams) (map[string]any, error) {
	cfg := env.Cluster
	db := p.Database
	spec, err := clusterOf(cfg, db)
	if err != nil {
		return nil, err
	}
	logLine(env.Report, env.Job, "waiting for the heavy-I/O lock")
	acquired, err := engine.AcquireHeavyIOWait(ctx, env.Job, env.ID, recoverHeavyIOWait)
	if err != nil {
		return nil, err
	}
	// promote releases it before it starts the full backup, whose job takes
	// the same lock; a release runs once.
	release := sync.OnceFunc(acquired)
	defer release()
	started := time.Now()
	rt := jobRuntime(env)

	// 1. The scaffold: 18/<db> empty, on its port, with its configuration
	// but no archiving into the stanza (an empty cluster's WAL must never
	// reach it). Its steps run on every attempt, so a retry after a failure
	// part way through (the cluster made, its configuration step failed)
	// finishes the scaffold; on a complete one they change nothing.
	logLine(env.Report, env.Job, "building the empty cluster 18/"+db+" for the recovered data")
	scaffold := cfg
	scaffold.Backup.Provider = ""
	if _, _, err := engine.Apply(ctx, steps.Catalog(), scaffold, rt, engine.Options{
		Only: scaffoldSteps(db), Recovering: db, Restarter: steps.Restarter(scaffold), Report: env.Report,
	}); err != nil {
		return nil, err
	}

	// 2. Materialise the stanza into <db>__restoring.
	port, warnings, err := materialiseRestore(ctx, env, p.restore())
	if err != nil {
		return nil, err
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		r := s.Restores[db]
		r.Phase, r.ReadyAt, r.Warnings, r.Error, r.Port = backup.PhaseReady, stamp(), warnings, "", port
		s.Restores[db] = r
		return nil
	}); err != nil {
		return nil, err
	}

	// The scaffold was built archiving nowhere, and promote keeps
	// /etc/postgresql/18/<db>: the recovered data must archive to the stanza
	// from its first start (its new timeline's history file above all), here
	// and when restore promote finishes a --no-promote run. Stop the empty
	// scaffold first, so no reload can make it archive into the stanza.
	if err := armArchiving(ctx, env, cfg, spec, rt); err != nil {
		return nil, err
	}

	res := map[string]any{"database": db, "stanza": spec.Stanza, "restoring": backup.RestoringName(db), "port": port, "warnings": warnings}
	if p.NoPromote {
		// 3. Ready to inspect. The database is no longer awaiting recovery:
		// the steps see it, and restore promote finishes the run.
		if _, err := clearAwaiting(db); err != nil {
			return nil, err
		}
		applyRecoveredTimers(ctx, cfg, rt, env, res)
		if err := applyPoolers(ctx, cfg, rt, env.Report); err != nil {
			res["pooler_warning"] = err.Error()
		}
		res["phase"] = backup.PhaseReady
		return res, nil
	}

	// 4. The split-brain check again, just before the copy opens. A failure
	// here leaves the database awaiting recovery, for db recover again.
	if err := stanzaStillWritten(ctx, cfg, spec); err != nil {
		return res, err
	}

	// 5. Promote: the copy takes 18/<db>'s place, the scaffold is set aside.
	// The steps see the database from here on (promote renders its pooler
	// entries); a promote that fails before its swap leaves it awaiting
	// recovery again, so db recover retries it from the start.
	prev, err := clearAwaiting(db)
	if err != nil {
		return nil, err
	}
	promoted, err := promote(ctx, cfg, db, rt, env.Report, release)
	if err != nil {
		if recs, lerr := backup.LoadRecords(); lerr == nil {
			r := recs.Restores[db]
			if r.Aside == "" || backup.SwapState(cfg.Server.DataRoot, db, backup.RestoringName(db), r.Aside) == backup.SwapNotStarted {
				_ = backup.UpdateRecords(func(s *backup.Records) error {
					if s.AwaitingRecovery == nil {
						s.AwaitingRecovery = map[string]backup.Awaiting{}
					}
					s.AwaitingRecovery[db] = prev
					return nil
				})
			}
		}
		return res, err
	}
	applyRecoveredTimers(ctx, cfg, rt, env, res)
	res["phase"] = "recovered"
	res["promoted"] = promoted
	if aside, _ := promoted["pre_restore"].(string); aside != "" {
		if err := dropScaffold(env, cfg, db, aside); err != nil {
			res["scaffold_warning"] = "the empty cluster " + aside + " is left; drop it with eru restore cleanup " + db + ": " + err.Error()
		}
	}

	// 6. The database's own steps (roles and grants are in the data, so this
	// only reconciles and writes the service password files a rebuilt master
	// lacks), then metrics. The data is back either way; a failure here is
	// reported as what is left, with the command that finishes it.
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{DB: db, Restarter: steps.Restarter(cfg), Report: env.Report}); err != nil {
		res["steps_warning"] = "the data is recovered, but its configuration did not finish: " + err.Error() + "; eru apply finishes it"
	}
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.MetricsStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report}); err != nil {
		res["metrics_warning"] = "the data is recovered, but its metrics did not finish: " + err.Error() + "; eru apply finishes them"
	}
	if t := recoveredTo(ctx, spec); t != "" {
		res["recovered_to"] = t
	}
	res["duration_s"] = int64(time.Since(started).Seconds())
	steps.WriteMetrics(cfg)
	writeMasterMetrics(cfg)
	return res, nil
}

// clearAwaiting records that db no longer awaits recovery; it returns the
// record it removed.
func clearAwaiting(db string) (backup.Awaiting, error) {
	var prev backup.Awaiting
	err := backup.UpdateRecords(func(s *backup.Records) error {
		prev = s.AwaitingRecovery[db]
		delete(s.AwaitingRecovery, db)
		return nil
	})
	return prev, err
}

// applyRecoveredTimers brings back the recovered database's timers.
func applyRecoveredTimers(ctx context.Context, cfg config.Cluster, rt engine.Runtime, env *jobEnv, res map[string]any) {
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.TimersStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report}); err != nil {
		res["timers_warning"] = err.Error()
	}
}

// armArchiving stops the empty scaffold 18/<db>, renders pgbackrest.conf
// with the database's stanza section, then writes the database's real
// 40-eru-backup.conf, which the recovered data starts with. In that order:
// the archive_command passes archive-push a WAL path relative to the data
// directory (%p), which pgBackRest resolves with the section's pg1-path, so
// a conf without the section fails the very first push (the new timeline's
// history file). server init --restore rendered it with no stanza at all,
// every database awaiting recovery then.
func armArchiving(ctx context.Context, env *jobEnv, cfg config.Cluster, spec config.ClusterSpec, rt engine.Runtime) error {
	if err := env.shell().Exec(ctx, env.Job, engine.Command{Name: "systemctl", Args: []string{"stop", "postgresql@18-" + spec.Name}}, env.Report); err != nil {
		return err
	}
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, stanzaConfOptions(cfg, spec.Name, env.Report)); err != nil {
		return err
	}
	return steps.WriteBackupConf(cfg, spec)
}

// stanzaConfOptions is the apply of pgbackrest-v1 that gives db, still
// awaiting recovery, its section in pgbackrest.conf (pg1-path, pg1-port,
// retention) beside the databases recovered before it. Its stanza holds a
// full backup (stanzaHasBackup), so the step only renders the file.
func stanzaConfOptions(cfg config.Cluster, db string, report engine.Reporter) engine.Options {
	return engine.Options{Only: steps.PgBackRestStepIDs(), Recovering: db, Restarter: steps.Restarter(cfg), Report: report}
}

// dropScaffold drops the empty cluster promote set aside and its leftover
// record: it never held data.
func dropScaffold(env *jobEnv, cfg config.Cluster, db, aside string) error {
	if err := backup.DropTemporaryCluster(env.shell(), env.Job, env.Report, aside, filepath.Join(cfg.Server.DataRoot, aside)); err != nil {
		return err
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		kept := s.Leftovers[:0]
		for _, l := range s.Leftovers {
			if l.Database != db || l.Name != aside {
				kept = append(kept, l)
			}
		}
		s.Leftovers = kept
		return nil
	})
}

// recoveredTo is the recovered cluster's last replayed commit, as far as it
// can be read.
func recoveredTo(ctx context.Context, spec config.ClusterSpec) string {
	rows, err := steps.ClusterPG(spec).Query(ctx, "postgres",
		"SELECT coalesce(to_char(pg_last_xact_replay_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"'), '') AS t", nil)
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0]["t"]
}
