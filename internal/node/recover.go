package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/steps"
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
			"db recover is for databases a rebuilt master hasn't recovered. To go back in time on a running database use vaire restore "+p.Database+".").
			WithDetails(map[string]any{"reason": "not_awaiting_recovery", "database": p.Database})
	}
	if spec.IsFollower() {
		// A follower follows its recovered source again (Phase 6, D14).
		return recoverFollower(ctx, env, p, spec, recs)
	}
	var target, timeline string
	err = withRestorePreparation(ctx, env, p.Database, "db.recover", func() error {
		cfg = env.Cluster
		spec, err = clusterOf(cfg, p.Database)
		if err != nil {
			return err
		}
		recs, err = backup.LoadRecords()
		if err != nil {
			return err
		}
		if spec.IsFollower() || !recs.IsAwaiting(p.Database) {
			return incompleteMaterialisation(backup.RestoringName(p.Database))
		}
		if !cfg.BackupConfigured() {
			return output.NewError(output.CodePrecondition, "there is no bucket, so there is no stanza to recover "+p.Database+" from", output.ExitPrecondition,
				"The data is lost with the old server. Create the database again: vaire db drop "+p.Database+" --confirm "+p.Database+", then vaire db create "+p.Database+".").
				WithDetails(map[string]any{"reason": "no_offsite_backup", "database": p.Database})
		}
		if p.Time != "" && env.Req.Confirm == "" && !env.Req.DryRun {
			return confirmRequired("a recovery to a time leaves out everything "+p.Database+" wrote after it", p.Database+"/recover")
		}
		if err := recoverCheckFence(ctx, cfg, env.Report); err != nil {
			return err
		}
		if err := recoverCheckBackup(ctx, spec, recs); err != nil {
			return err
		}
		if err := recoverCheckWritten(ctx, cfg, spec); err != nil {
			return err
		}
		rp := p.restore()
		target, err = restoreCheckTarget(ctx, cfg, rp, env.Req.Secrets)
		if err != nil {
			return err
		}
		timeline = recs.TargetTimeline(p.Database, spec.Stanza)
		if env.Req.DryRun {
			return nil
		}
		if err := clearFailedRestore(cfg, p.Database); err != nil {
			return err
		}
		if err := restoreInProgress(p.Database, "a recovery"); err != nil {
			return err
		}
		if err := restoreCheckNamespace(ctx, cfg, p.Database); err != nil {
			return err
		}
		instance, operation, err := restoreInvocationProvenance(env)
		if err != nil {
			return err
		}
		return restorePublish(func(s *backup.Records) error {
			if _, exists := s.Restores[p.Database]; exists || !s.IsAwaiting(p.Database) {
				return incompleteMaterialisation(backup.RestoringName(p.Database))
			}
			s.Restores[p.Database] = backup.Restore{
				ID: env.Req.RequestID, Database: p.Database, Kind: rp.Kind, Target: target,
				InstanceID: instance, OperationID: operation,
				Phase: backup.PhaseRestoring, StartedAt: stamp(), Promote: !p.NoPromote, Timeline: timeline,
				Recover: true, NoFull: p.NoFull, MaterialisationPending: true,
			}
			return nil
		})
	})
	if err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "stanza": spec.Stanza, "target": target, "timeline": timeline, "dry_run": true}}, nil
	}
	// The recovered database's steps (postgres-roles-v1, metrics-v1,
	// pgbackrest-v1) run inside the job: a rebuilt master has no service
	// password files until they write them from these.
	return restoreLaunch(ctx, env, "recover", p, stepSecrets(env.Req.Secrets))
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
	hint := "Create it again: vaire db drop " + spec.Name + " --confirm " + spec.Name + ", vaire db create " + spec.Name +
		", then restore a long-retention dump with vaire restore " + spec.Name + " --dump <id> (vaire dump list " + spec.Name + ")."
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
		"Power off or destroy the old server first; its fence check stops it within five minutes when it can reach the bucket. Then run vaire db recover "+spec.Name+" again.").
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

var recoverScaffoldApply = engine.Apply

// jobRecover recovers one database (D8).
func jobRecover(ctx context.Context, env *jobEnv) (any, error) {
	var p recoverParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	res, err := recoverDatabase(ctx, env, p)
	if err != nil {
		if backup.RetainRecoveryFailure(err) {
			return nil, err
		}
		return nil, errors.Join(err, failRestoreChecked(env, err, nil))
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
	if env.lease == nil {
		return nil, fmt.Errorf("recover requires the runner's lock lease")
	}
	// Promote releases only heavy I/O before starting the full backup. The
	// runner keeps the node and cluster locks until its finalizer.
	release := env.lease.releaseHeavyIO
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
	if _, _, err := recoverScaffoldApply(ctx, steps.Catalog(), scaffold, rt, engine.Options{
		Only: scaffoldSteps(db), Recovering: db, Restarter: steps.Restarter(scaffold), Report: env.Report,
	}); err != nil {
		return nil, err
	}

	// 2. Materialise the stanza into <db>__restoring.
	port, warnings, attempt, err := materialiseRestore(ctx, env, p.restore())
	if err != nil {
		return nil, errors.Join(err, failRestoreChecked(env, err, attempt))
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		r, ok := s.Restores[db]
		if !ok || !matchesRestoreInvocation(r, env, db) || r.Phase != backup.PhaseRestoring ||
			attempt == nil || r.Attempt == nil || !sameAttemptIdentity(*r.Attempt, *attempt) || !restoreRecordMatchesAttempt(r, attempt) {
			return incompleteMaterialisation(backup.RestoringName(db))
		}
		r.Phase, r.ReadyAt, r.Warnings, r.Error, r.Port = backup.PhaseReady, stamp(), warnings, "", port
		r.Attempt = nil
		r.MaterialisationPending = false
		r.MaterialisationCleaned = false
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
		// Inspection is ready, but the writable application target is still
		// awaiting recovery. Keep that durable routing fence until promotion
		// and exact-target access provisioning both succeed.
		return recoverInspectionReady(ctx, cfg, rt, db, env.Report, res), nil
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
	promoted, err := promote(ctx, cfg, db, rt, env.Report, release)
	if err != nil {
		return res, err
	}
	applyRecoveredTimers(ctx, cfg, rt, env, res)
	res["phase"] = "recovered"
	res["promoted"] = promoted
	if aside, _ := promoted["pre_restore"].(string); aside != "" {
		recoverScaffoldResult(ctx, env, cfg, db, aside, port, spec.Stanza, res)
	}

	// Per-database provisioning completed before promote published the target.
	if _, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.MetricsStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report}); err != nil {
		res["metrics_warning"] = "the data is recovered, but its metrics did not finish: " + err.Error() + "; vaire apply finishes them"
	}
	if t := recoveredTo(ctx, spec); t != "" {
		res["recovered_to"] = t
	}
	res["duration_s"] = int64(time.Since(started).Seconds())
	steps.WriteMetrics(cfg)
	writeMasterMetrics(cfg)
	return res, nil
}

func recoverScaffoldResult(ctx context.Context, env *jobEnv, cfg config.Cluster, db, aside string, port int, stanza string, result map[string]any) {
	if err := dropScaffold(ctx, env, cfg, db, aside, port, stanza); err != nil {
		result["scaffold_warning"] = "the empty cluster " + aside + " is left; drop it with vaire restore cleanup " + db + ": " + err.Error()
	}
}

var recoverInspectionPoolers = applyPoolers

func recoverInspectionReady(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db string, report engine.Reporter, res map[string]any) map[string]any {
	if err := recoverInspectionPoolers(ctx, cfg, rt, db, report); err != nil {
		res["pooler_warning"] = err.Error()
	}
	res["phase"] = backup.PhaseReady
	res["application_ready"] = false
	res["access_coverage"] = "unknown"
	res["access_notice"] = "The read-only restore is ready for inspection; application access is not ready until promotion and access provisioning succeed."
	return res
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
// 40-vaire-backup.conf, which the recovered data starts with. In that order:
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
func dropScaffold(ctx context.Context, env *jobEnv, cfg config.Cluster, db, aside string, port int, stanza string) error {
	return withLockedJobTarget(env, backup.RestoringName(db), func() error {
		var p recoverParams
		if env.Job != "recover" || json.Unmarshal(env.Params, &p) != nil || p.Database != db || p.NoPromote ||
			(env.lease.targets[db] != engine.LockShared && env.lease.targets[db] != engine.LockExclusive) {
			return errors.New("recover scaffold lease does not match")
		}
		instance, err := currentFollowInstance()
		if err != nil || instance != env.installID {
			return errors.New("recover scaffold install changed")
		}
		spec, ok := cfg.ClusterByName(db)
		current, currentOK := env.Cluster.ClusterByName(db)
		if !ok || !currentOK || cfg.Server.DataRoot != env.Cluster.Server.DataRoot ||
			spec.Stanza != stanza || current.Stanza != stanza || spec.Port != current.Port ||
			port < config.TemporaryPortMin || port > config.TemporaryPortMax {
			return errors.New("recover scaffold target changed")
		}
		records, err := backup.LoadRecords()
		if err != nil {
			return err
		}
		var l backup.Leftover
		matches := 0
		for _, candidate := range records.Leftovers {
			if candidate.Name == aside {
				matches++
				l = candidate
			}
		}
		if matches != 1 || strings.HasSuffix(aside, "b") || l.Database != db || l.RestoreID != env.ID || l.Port != port || l.Stanza != stanza {
			return errors.New("recover scaffold leftover mismatch")
		}
		a, err := openLegacyLeftover(cfg, l)
		if err != nil {
			return err
		}
		defer a.close()
		if err := a.remove(ctx, &Env{Cluster: cfg, Report: env.Report}); err != nil {
			return err
		}
		return retireLegacyLeftover(cfg, l)
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
