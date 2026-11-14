package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
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
		"Finish it with vaire restore promote "+db+" or vaire restore abort "+db+".").
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
	if ok && r.Phase == backup.PhaseFailed && r.MaterialisationPending && r.Attempt == nil {
		return retireNeverStartedRestore(context.Background(), cfg, db, r)
	}
	if ok && (r.Attempt != nil || r.MaterialisationPending) {
		return incompleteMaterialisation(backup.RestoringName(db))
	}
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
		cur, ok := s.Restores[db]
		if !ok || cur.ID != r.ID || cur.Phase != backup.PhaseFailed || cur.Attempt != nil || cur.MaterialisationPending {
			return incompleteMaterialisation(backup.RestoringName(db))
		}
		delete(s.Restores, db)
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
	hint := "Drop it with vaire restore abort " + db + "."
	if _, ok := cfg.ClusterByName(name); ok {
		hint = "It is the database " + name + "; rename one of them or drop it with vaire db drop " + name + "."
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
	var target, timeline string
	var space []backup.SpaceNeed
	err := withRestorePreparation(ctx, env, p.Database, "restore.start", func() error {
		cfg = env.Cluster
		if err := requireBackups(cfg); err != nil {
			return err
		}
		if err := checkRestorable(cfg, p.Database); err != nil {
			return err
		}
		var err error
		target, err = restoreCheckTarget(ctx, cfg, p, env.Req.Secrets)
		if err != nil {
			return err
		}
		if !env.Req.DryRun {
			if err := clearFailedRestore(cfg, p.Database); err != nil {
				return err
			}
		}
		if err := restoreInProgress(p.Database, "a new restore"); err != nil {
			return err
		}
		if err := restoreCheckNamespace(ctx, cfg, p.Database); err != nil {
			return err
		}
		space, err = restoreCheckSpace(ctx, cfg, p.Database)
		if err != nil {
			return err
		}
		timeline, err = restoreTimeline(cfg, p)
		if err != nil || env.Req.DryRun {
			return err
		}
		instance, operation, err := restoreInvocationProvenance(env)
		if err != nil {
			return err
		}
		return restorePublish(func(s *backup.Records) error {
			if _, open := s.Restores[p.Database]; open {
				return incompleteMaterialisation(backup.RestoringName(p.Database))
			}
			s.Restores[p.Database] = backup.Restore{
				ID: env.Req.RequestID, Database: p.Database, Kind: p.Kind, Target: target,
				InstanceID: instance, OperationID: operation,
				Phase: backup.PhaseRestoring, StartedAt: stamp(), Promote: p.Promote, Timeline: timeline,
				MaterialisationPending: p.Kind != targetDump,
			}
			return nil
		})
	})
	if err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "target": target, "timeline": timeline, "space": space, "dry_run": true}}, nil
	}
	restoreWriteMetrics(cfg)
	jobSecrets := map[string]string{}
	if v := env.Req.Secrets[steps.SecretBackupAgeIdentity]; v != "" {
		jobSecrets[steps.SecretBackupAgeIdentity] = v
	}
	return restoreLaunch(ctx, env, "restore", p, jobSecrets)
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
				"Take one with vaire backup now --type full.").WithDetails(map[string]any{"reason": "no_backups"})
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
				"List the sets with vaire backup list.").WithDetails(map[string]any{"reason": "set_not_found"})
		}
		return "set " + p.Set, nil
	case targetDump:
		if sec[steps.SecretBackupAgeIdentity] == "" {
			return "", output.NewError(output.CodeInputRequired, "a restore from a dump needs the age identity from the recovery kit", output.ExitInput,
				"Run the restore with an vaire that has the recovery kit (vaire login from a transfer bundle made with vaire secrets export); it sends the identity with the request.").WithDetails(map[string]any{"secret": steps.SecretBackupAgeIdentity})
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
			"List the dumps with vaire dump list "+p.Database+".").WithDetails(map[string]any{"reason": "dump_not_found"})
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
	return withHint(err, "Points before "+d.DroppedAt+" are in the older stanza "+d.Stanza+" (kept until "+d.ExpiresAt+"; vaire backup list --db "+db+"), which vaire restore doesn't read.")
}

// restoreSpace checks room beside the live data directory for one copy of
// db: the temporary cluster of a restore or a fork.
func restoreSpace(ctx context.Context, cfg config.Cluster, db string) ([]backup.SpaceNeed, error) {
	local, err := clusterPG(cfg, db)
	if err != nil {
		return nil, err
	}
	size := databaseSize(ctx, local, db)
	dataDir := cfg.Server.DataDirectory(db)
	return backup.CheckSpace([]backup.SpaceNeed{{Path: dataDir, Need: backup.CopyNeed(size), Purpose: "the copy of " + db}})
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
	port, warnings, attempt, err := materialiseRestore(ctx, env, p)
	if err != nil {
		return nil, errors.Join(err, failRestoreChecked(env, err, attempt))
	}
	readyAt := stamp()
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		r, ok := s.Restores[p.Database]
		if !ok || !matchesRestoreInvocation(r, env, p.Database) || r.Phase != backup.PhaseRestoring ||
			(attempt != nil && (r.Attempt == nil || !sameAttemptIdentity(*r.Attempt, *attempt) || !restoreRecordMatchesAttempt(r, attempt))) ||
			(attempt == nil && r.Attempt != nil) {
			return incompleteMaterialisation(backup.RestoringName(p.Database))
		}
		r.Phase, r.ReadyAt, r.Warnings, r.Error, r.Port = backup.PhaseReady, readyAt, warnings, "", port
		r.Attempt = nil
		r.MaterialisationPending = false
		r.MaterialisationCleaned = false
		s.Restores[p.Database] = r
		return nil
	}); err != nil {
		return nil, err
	}
	rt := jobRuntime(env)
	res := map[string]any{"database": p.Database, "restoring": backup.RestoringName(p.Database), "port": port, "phase": backup.PhaseReady, "warnings": warnings}
	if p.Promote {
		promoted, err := promote(ctx, env.Cluster, p.Database, rt, env.Report, func() {})
		if err != nil {
			return res, err
		}
		res["phase"] = "promoted"
		res["promoted"] = promoted
		// A restored source opens on a new timeline, so its followers stop
		// following it (D12). They stay structured warnings: the CLI forwards
		// their followers_diverged code, not the dump warnings' label.
		res["follower_warnings"] = followersDiverged(env.Cluster, p.Database)
	} else if err := restoreApplyPoolers(ctx, env.Cluster, rt, "", env.Report); err != nil {
		res["pooler_warning"] = "the session pooler was not reloaded, so vaire sql --db " + backup.RestoringName(p.Database) + " fails until vaire apply: " + err.Error()
	}
	restoreWriteMetrics(env.Cluster)
	return res, nil
}

// jobRuntime is the step runtime for a job's applies. Its secret values are
// the ones the RPC that started the job put in its spec (stepSecrets): a
// step that finds a secret missing on the node (a service password file on
// a rebuilt master) takes it from them, as it would in apply. A timer run
// has none, and its steps read theirs from the node.
var jobRuntimeHostRead = hostinfo.Detect

func jobRuntime(env *jobEnv) engine.Runtime {
	host, _ := jobRuntimeHostRead(env.Cluster.Server.DataRoot)
	return engine.Runtime{Shell: env.shell(), Secrets: secrets.Default, SecretValues: env.Secrets, Host: host}
}

// applyPoolers runs pgbouncer-v1, which renders both poolers' entries,
// including the session pooler's for ready restores, and reloads them.
// recovering keeps a database that awaits recovery in the steps' view, so its
// entry is rendered while it is being recovered.
func applyPoolers(ctx context.Context, cfg config.Cluster, rt engine.Runtime, recovering string, report engine.Reporter) error {
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.PoolerStepIDs(), Recovering: recovering, Restarter: steps.Restarter(cfg), Report: report})
	return err
}

// failRestore records a failed restore so abort can clean it up.
func failRestore(env *jobEnv, cause error) {
	_ = failRestoreChecked(env, cause, nil)
}

func failRestoreChecked(env *jobEnv, cause error, expected *backup.MaterialisationAttempt) error {
	var p restoreParams
	if json.Unmarshal(env.Params, &p) != nil || p.Database == "" {
		return errors.New("cannot identify the failed restore")
	}
	return withLockedJobTarget(env, backup.RestoringName(p.Database), func() error {
		return backup.UpdateRecords(func(s *backup.Records) error {
			r, ok := s.Restores[p.Database]
			if !ok || (r.Phase != backup.PhaseRestoring && r.Phase != backup.PhaseReady) ||
				!matchesRestoreInvocation(r, env, p.Database) || (expected == nil && r.Attempt != nil) ||
				(expected != nil && (r.Attempt == nil || !sameAttemptIdentity(*r.Attempt, *expected) || !restoreRecordMatchesAttempt(r, expected))) {
				return incompleteMaterialisation(backup.RestoringName(p.Database))
			}
			r.Phase, r.Error = backup.PhaseFailed, cause.Error()
			if p.Kind != targetDump && r.Attempt == nil && !r.MaterialisationCleaned {
				r.MaterialisationPending = true
				r.MaterialisationCleaned = false
			}
			if reason := recoveryFailureReason(cause); reason != "" {
				r.Error = "recovery could not meet parameter requirements"
				if expected == nil || r.Attempt == nil {
					return incompleteMaterialisation(backup.RestoringName(p.Database))
				}
				r.Attempt.Reason = reason
			} else if r.MaterialisationCleaned && expected != nil {
				r.Attempt = nil
				r.MaterialisationPending = false
			}
			s.Restores[p.Database] = r
			return nil
		})
	})
}

// materialiseRestore builds the temporary cluster <db>__restoring: a
// pgBackRest restore of the database's stanza paused at the target (a
// standby following the archive for latest), or a new cluster loaded from a
// dump. It returns the cluster's port. A failure drops what was built.
func materialiseRestore(ctx context.Context, env *jobEnv, p restoreParams) (int, []string, *backup.MaterialisationAttempt, error) {
	cfg := env.Cluster
	spec, err := clusterOf(cfg, p.Database)
	if err != nil {
		return 0, nil, nil, err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return 0, nil, nil, err
	}
	if p.Kind != targetDump {
		if err := validateRestoreInvocation(env); err != nil {
			return 0, nil, nil, err
		}
		if err := withLockedJobTarget(env, backup.RestoringName(p.Database), func() error { return nil }); err != nil {
			return 0, nil, nil, err
		}
		initial, found := bs.Restores[p.Database]
		if !found {
			return 0, nil, nil, incompleteMaterialisation(backup.RestoringName(p.Database))
		}
		if err := withLockedJobTarget(env, backup.RestoringName(p.Database), func() error {
			return backup.UpdateRecords(func(s *backup.Records) error {
				r, ok := s.Restores[p.Database]
				if !ok || !reflect.DeepEqual(r, initial) || !matchesRestoreInvocation(r, env, p.Database) || r.Phase != backup.PhaseRestoring ||
					r.Kind != p.Kind || env.Job == "restore" && r.Promote != p.Promote || r.Attempt != nil {
					return incompleteMaterialisation(backup.RestoringName(p.Database))
				}
				r.MaterialisationPending = true
				s.Restores[p.Database] = r
				return nil
			})
		}); err != nil {
			return 0, nil, nil, err
		}
		initial.MaterialisationPending = true
		bs.Restores[p.Database] = initial
	}
	constructor := callerNewMaterialised
	if p.Kind != targetDump {
		constructor = callerNewRestoreOwned
	}
	var m *backup.Materialised
	if p.Kind == targetDump {
		m, err = constructor(cfg.Server.DataRoot, backup.RestoringName(p.Database), p.Database, spec.Port, env.shell(), env.Job, env.Report)
	} else {
		err = withLockedJobTarget(env, backup.RestoringName(p.Database), func() error {
			var err error
			m, err = constructor(cfg.Server.DataRoot, backup.RestoringName(p.Database), p.Database, spec.Port, env.shell(), env.Job, env.Report)
			return err
		})
	}
	if err != nil {
		return 0, nil, nil, err
	}
	if p.Kind != targetDump {
		defer m.ReleaseForkReservationAuthority()
	}
	var warnings []string
	var expected *backup.MaterialisationAttempt
	var birth *restoreBirth
	if p.Kind == targetDump {
		warnings, err = loadFromDump(ctx, env, p, spec, m)
	} else {
		m.DestinationConfig, err = prepareMaterialisationConfig(cfg, spec)
		if err != nil {
			return 0, nil, nil, err
		}
		kind := "restore"
		if bs.Restores[p.Database].Recover {
			kind = "recover"
		}
		attempt, aerr := newMaterialisationAttempt(env, m, spec.Stanza, kind)
		if aerr != nil {
			return 0, nil, nil, aerr
		}
		attempt.ReservationIdentity, aerr = m.ForkReservationIdentity()
		if aerr != nil {
			return 0, nil, nil, aerr
		}
		expected = &attempt
		if err := withLockedJobTarget(env, m.Name, func() error {
			return m.WithForkReservation(ctx, func() error {
				return backup.UpdateRecords(func(s *backup.Records) error {
					r, ok := s.Restores[p.Database]
					if !ok || !reflect.DeepEqual(r, bs.Restores[p.Database]) || !matchesRestoreInvocation(r, env, p.Database) || r.Phase != backup.PhaseRestoring ||
						r.Kind != p.Kind || env.Job == "restore" && r.Promote != p.Promote || r.Attempt != nil {
						return incompleteMaterialisation(m.Name)
					}
					attempt.RecordDigest = restoreRecordDigest(r)
					r.Attempt = &attempt
					s.Restores[p.Database] = r
					return nil
				})
			})
		}); err != nil {
			return 0, nil, nil, err
		}
		birth, aerr = newRestoreBirth(env, m, expected)
		if aerr != nil {
			return 0, nil, expected, aerr
		}
		defer birth.close()
		m.Ownership = birth
		opts := backup.RestoreOptions{Kind: p.Kind, Set: p.Set, Pause: true, Timeline: bs.Restores[p.Database].Timeline}
		if p.Kind == backup.TargetTime {
			opts.Time, _ = time.Parse(time.RFC3339, p.Time)
		}
		err = callerRestoreMaterialised(ctx, m, driver(spec, env.Job, env.Report), opts)
		if err == nil {
			if err := verifyRestoreAttempt(p.Database, env.ID, expected); err != nil {
				return 0, warnings, expected, err
			}
			if t := callerRecoveredTo(ctx, m); t != "" {
				logLine(env.Report, env.Job, "recovered to "+t)
			}
		}
	}
	if err != nil {
		if !backup.RetainRecoveryFailure(err) {
			if expected != nil {
				if checkErr := verifyRestoreAttempt(p.Database, env.ID, expected); checkErr != nil {
					return 0, warnings, expected, errors.Join(err, checkErr)
				}
			}
			dropErr := error(nil)
			if birth != nil {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				dropErr = birth.discard(cleanupCtx)
				cancel()
			} else {
				dropErr = m.Drop()
			}
			if dropErr == nil && p.Kind != targetDump {
				_ = backup.UpdateRecords(func(s *backup.Records) error {
					r, ok := s.Restores[p.Database]
					if !ok || !matchesRestoreInvocation(r, env, p.Database) || r.Attempt == nil || expected == nil || !sameAttemptIdentity(*r.Attempt, *expected) || !restoreRecordMatchesAttempt(r, expected) {
						return incompleteMaterialisation(m.Name)
					}
					r.MaterialisationCleaned = true
					r.Attempt.RecordDigest = restoreRecordDigest(r)
					expected.RecordDigest = r.Attempt.RecordDigest
					s.Restores[p.Database] = r
					return nil
				})
			}
		}
		return 0, warnings, expected, err
	}
	return m.Port, warnings, expected, nil
}

// loadFromDump creates the empty cluster m, copies the live cluster's roles
// into it, creates the database with vaire's grants, then downloads and
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
	// The copy holds the live database's roles, so its vaire_admin follows
	// the live database's protection: on a protected database it can only
	// read and a write needs a code (db.write-access), exactly as on the
	// live one, so a promote cannot launder the protection away.
	if err := local.Exec(ctx, steps.AuthDB, steps.AdminRoleSQL(spec), nil); err != nil {
		return nil, err
	}
	sql := steps.AuthFunctionsSQL() + fmt.Sprintf("CREATE DATABASE %s OWNER %s;\n", pg.Quote(spec.Name), pg.Quote(spec.Name+"_owner")) +
		steps.DatabaseConnectSQL(spec.Name, spec.Name)
	if err := local.Exec(ctx, steps.AuthDB, sql, nil); err != nil {
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
	// The grants again for what the dump created, and ANALYZE, so restore
	// status can compare row estimates.
	if err := local.Exec(ctx, spec.Name, steps.DatabaseGrantsSQL(spec), nil); err != nil {
		return warnings, err
	}
	return warnings, local.Exec(ctx, spec.Name, "ANALYZE;\n", nil)
}

type restoreDBParams struct {
	Database string `json:"database"`
}

// lockForCutover takes the node lock shared and db's cluster lock
// exclusive: a promote or undo stops the live cluster. The dispatcher takes
// no lock for these methods, so a dry run takes none. The release is safe to
// call more than once: the handlers release before starting the full backup,
// whose job takes the cluster lock shared without waiting, and again on
// return.
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
	return sync.OnceFunc(func() { releaseCluster(); release() }), nil
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
	if ok && (r.Attempt != nil || r.MaterialisationPending) {
		return Result{}, incompleteMaterialisation(backup.RestoringName(p.Database))
	}
	if !ok || r.Phase != backup.PhaseReady {
		return Result{}, output.NewError(output.CodePrecondition, "there is no restore of "+p.Database+" ready to promote", output.ExitPrecondition,
			"Start one with vaire restore "+p.Database+", and check vaire restore status.").WithDetails(map[string]any{"reason": "no_ready_restore"})
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "restore": r, "dry_run": true}}, nil
	}
	release, err := lockForCutover(ctx, env, p.Database, "restore.promote")
	if err != nil {
		return Result{}, err
	}
	defer release()
	data, err := promote(ctx, env.Cluster, p.Database, runtimeFor(env, false), env.Report, release)
	if err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	if w, ok := data["warning"].(output.Warning); ok {
		warnings = append(warnings, w)
		delete(data, "warning")
	}
	warnings = append(warnings, followersDiverged(env.Cluster, p.Database)...)
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
// done. unlock runs before the backup starts, so its job can take db's
// cluster lock.
var (
	promotePoolerFence  = steps.ApplyPoolerFence
	promotePause        = steps.PauseDatabase
	promoteSwap         = backup.Swap
	promoteStartCluster = backup.StartCluster
	promoteDatabase     = backup.Promote
	promoteSettle       = backup.SettlePromoted
	promoteApplyPoolers = applyPoolers
	promoteStartFull    = startFullBackup
)

func promote(ctx context.Context, cfg config.Cluster, db string, rt engine.Runtime, report engine.Reporter, unlock func()) (map[string]any, error) {
	bs, err := backup.LoadRecords()
	if err != nil {
		return nil, err
	}
	r, exists := bs.Restores[db]
	if !exists || r.ID == "" || r.Database != db || r.Phase != backup.PhaseReady {
		return nil, fmt.Errorf("restore record for %s is missing or not ready", db)
	}
	spec, err := clusterOf(cfg, db)
	if err != nil {
		return nil, err
	}
	if r.Recover {
		// Existing deferred recoveries may predate the retained-awaiting
		// fence. Remove any stale application alias before swapping data.
		if err := promotePoolerFence(ctx, cfg, rt, report); err != nil {
			return nil, err
		}
	}
	const step = "restore.promote"
	aside := r.Aside
	if aside == "" {
		// A promote from before the aside was recorded.
		aside = swappedAside(cfg, db, bs, r)
	}
	oldArchived := r.AsideArchivedAt
	switch {
	case aside == "":
		aside = backup.PreRestoreName(db, time.Now())
		if r.Kind == targetDump {
			// The old stanza is dropped below; its window ends at its last WAL.
			oldArchived = lastArchivedTime(ctx, spec)
		}
		// Recorded before any rename, so a promote that fails part way
		// resumes with the same aside, and abort knows the copy is in.
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			cur, ok := s.Restores[db]
			if !ok || cur.ID != r.ID {
				return fmt.Errorf("the restore of %s changed while promoting it", db)
			}
			cur.Aside, cur.AsideArchivedAt = aside, oldArchived
			s.Restores[db] = cur
			return nil
		}); err != nil {
			return nil, err
		}
		logLine(report, step, "cutting "+db+" over to "+backup.RestoringName(db)+"; the old data becomes the cluster "+aside)
	case backup.SwapState(cfg.Server.DataRoot, db, backup.RestoringName(db), aside) == backup.SwapNotStarted:
		logLine(report, step, "resuming the promote of "+db+": cutting it over to "+backup.RestoringName(db)+"; the old data becomes the cluster "+aside)
	default:
		logLine(report, step, "resuming the promote of "+db+": the data was swapped already, and the old data is the cluster "+aside)
	}
	start := time.Now()
	resume := promotePause(ctx, db)
	err = promoteSwap(ctx, rt.Shell, step, report, cfg.Server.DataRoot, db, backup.RestoringName(db), aside, r.Port)
	if err == nil {
		until := backup.UntilPaused
		if r.Kind == backup.TargetLatest || r.Kind == targetDump {
			until = backup.UntilOpen
		}
		err = promoteStartCluster(ctx, rt.Shell, step, report, db, cfg.Server.DataDirectory(db), spec.Port, until)
	}
	if err == nil {
		var dropped []string
		if dropped, err = promoteDatabase(ctx, steps.ClusterPG(spec), cfg.Server.DataDirectory(db)); err == nil {
			// Still paused: a recovery target dropped above leaves only with
			// a restart, which postgres-config-v1 would otherwise refuse.
			err = promoteSettle(ctx, rt.Shell, step, report, steps.ClusterPG(spec), db, cfg.Server.DataDirectory(db), spec.Port, dropped)
		}
	}
	if err == nil {
		err = provisionRecoveredAccess(ctx, cfg, db, r, rt, report)
	}
	resume()
	if err != nil {
		return nil, withHint(err, "Read vaire logs postgres --db "+db+" and vaire restore status "+db+"; the old data is in the cluster "+aside+".")
	}
	took := time.Since(start)
	newStanza := ""
	if r.Kind == targetDump {
		if cfg, newStanza, err = switchStanza(cfg, db, ""); err != nil {
			return nil, err
		}
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		current, ok := s.Restores[db]
		if !ok || current.ID != r.ID || current.Database != db || current.Recover != r.Recover || current.Phase != backup.PhaseReady {
			return fmt.Errorf("the restore of %s changed while promoting it", db)
		}
		if r.Recover {
			delete(s.AwaitingRecovery, db)
		}
		delete(s.Restores, db)
		delete(s.Timelines, db)
		s.Leftovers = append(s.Leftovers, backup.Leftover{Database: db, Name: aside, CreatedAt: stamp(), RestoreID: r.ID, Port: r.Port, Stanza: spec.Stanza})
		if newStanza != "" {
			s.DroppedStanzas = append(s.DroppedStanzas, replacedStanza(cfg, spec, oldArchived))
			// The last verify was of the old stanza.
			delete(s.LastVerify, db)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	data := map[string]any{"database": db, "pre_restore": aside, "target": r.Target, "swap_ms": took.Milliseconds()}
	var warn error
	runs := []engine.Options{{Only: steps.PoolerStepIDs()}}
	if newStanza != "" {
		runs = append(runs, stanzaRuns(db)...)
	}
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, runs...))
	if err := promoteApplyPoolers(ctx, cfg, rt, "", report); err != nil {
		warn = err
	}
	if newStanza != "" {
		data["stanza"] = newStanza
		if err := applyStanza(ctx, cfg, db, rt, report); err != nil {
			warn = err
		}
	} else if !r.NoFull {
		unlock()
		if err := promoteStartFull(ctx, cfg, rt, db, step, report); err != nil {
			warn = err
		}
	}
	if warn != nil {
		data["warning"] = output.Warning{Code: "after_cutover_failed", Message: db + " is restored, but " + warn.Error() + "; run vaire apply, then vaire backup now --db " + db + " --type full"}
	}
	steps.WriteMetrics(cfg)
	return data, nil
}

var recoverProvisionOwned = provisionOwnedTarget

// provisionRecoveredAccess is reached only after the restored target is
// writable. A durable recover record, not an RPC flag or restore token,
// authorizes exact-target provisioning. Ordinary restore never widens access.
func provisionRecoveredAccess(ctx context.Context, cfg config.Cluster, db string, r backup.Restore, rt engine.Runtime, report engine.Reporter) error {
	if r.ID == "" || r.Database != db || r.Phase != backup.PhaseReady {
		return fmt.Errorf("restore record for %s is missing or not ready", db)
	}
	if !r.Recover {
		return nil
	}
	spec, ok := cfg.ClusterByName(db)
	if !ok || spec.IsFollower() {
		return fmt.Errorf("recovered access target %s is not an independent database", db)
	}
	return recoverProvisionOwned(ctx, cfg, rt, db, db, report)
}

// swappedAside returns the cluster an earlier promote of restore r left the
// old data in when it failed after the swap, or "": the copy's data
// directory is gone, and a <db>__pre_restore_<ts> data directory named
// after r started is in no leftover record. Promote then resumes from the
// start of the live cluster.
func swappedAside(cfg config.Cluster, db string, bs backup.Records, r backup.Restore) string {
	if _, err := os.Stat(filepath.Join(cfg.Server.DataRoot, backup.RestoringName(db))); err == nil {
		return ""
	}
	started, err := time.Parse(time.RFC3339, r.StartedAt)
	if err != nil {
		return ""
	}
	dirs, _ := filepath.Glob(filepath.Join(cfg.Server.DataRoot, db+"__pre_restore_*"))
	aside := ""
	for _, dir := range dirs {
		name := filepath.Base(dir)
		if name < backup.PreRestoreName(db, started) || slices.ContainsFunc(bs.Leftovers, func(l backup.Leftover) bool { return l.Name == name }) {
			continue
		}
		aside = name
	}
	return aside
}

// droppedStanza records spec's stanza as dropped now, kept for its window.
// lastArchived is when its cluster last archived a WAL segment, the end of
// the stanza's window.
func droppedStanza(cfg config.Cluster, spec config.ClusterSpec, lastArchived string) backup.DroppedStanza {
	now := time.Now().UTC()
	return backup.DroppedStanza{Database: spec.Name, Stanza: spec.Stanza, DroppedAt: now.Format(time.RFC3339),
		ExpiresAt: now.AddDate(0, 0, 7*cfg.RetentionFull(spec)).Format(time.RFC3339), LastArchivedAt: lastArchived}
}

// replacedStanza records spec's stanza as replaced now by a restore that
// switched the database to a new stanza, kept for its window like a dropped
// one.
func replacedStanza(cfg config.Cluster, spec config.ClusterSpec, lastArchived string) backup.DroppedStanza {
	d := droppedStanza(cfg, spec, lastArchived)
	d.Reason = backup.DroppedReasonReplaced
	return d
}

// lastArchivedTime is when spec's running cluster last archived a WAL
// segment (pg_stat_archiver), or "" when it can't be read. pgbackrest info
// names a stanza's newest segment but not its time, so a stanza about to be
// dropped records it while its cluster still runs.
func lastArchivedTime(ctx context.Context, spec config.ClusterSpec) string {
	st, err := driver(spec, "backup.archive", nil).ArchiveStatus(ctx)
	if err != nil {
		return ""
	}
	return st.LastArchivedTime
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

// stanzaRuns are the applies applyStanza runs, for the request's step
// numbering.
func stanzaRuns(db string) []engine.Options {
	return []engine.Options{{DB: db}, {Only: append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...)}}
}

// applyStanza points db's archive_command at its stanza (its own steps, a
// reload), then runs pgbackrest-v1 (the stanza's section, stanza-create,
// check and a first full when it has none) and timers-v1.
func applyStanza(ctx context.Context, cfg config.Cluster, db string, rt engine.Runtime, report engine.Reporter) error {
	catalog := steps.Catalog()
	ids := existingSettlementSteps(catalog, cfg, db)
	if len(ids) == 0 {
		return fmt.Errorf("no settlement steps for %q", db)
	}
	if _, _, err := engine.Apply(ctx, catalog, cfg, rt, engine.Options{DB: db, Only: ids, Restarter: steps.Restarter(cfg), Report: report}); err != nil {
		return err
	}
	_, _, err := engine.Apply(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: append(steps.PgBackRestStepIDs(), steps.TimersStepIDs()...), Restarter: steps.Restarter(cfg), Report: report})
	return err
}

// startFullBackup starts db's full backup job through its timer's template
// service without waiting for it. After a promote or undo the new timeline
// needs a full of its own.
func startFullBackup(ctx context.Context, cfg config.Cluster, rt engine.Runtime, db, step string, report engine.Reporter) error {
	if !cfg.BackupConfigured() {
		return nil
	}
	return rt.Shell.Exec(ctx, step, engine.Command{Name: "systemctl", Args: []string{"start", "--no-block", "vaire-backup-full@" + db + ".service"}}, report)
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
	u, resuming, err := planUndo(bs, p.Database)
	if err != nil {
		return Result{}, err
	}
	back := u.From
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "restore_from": back.Name, "resume": resuming, "dry_run": true}}, nil
	}
	release, err := lockForCutover(ctx, env, p.Database, "restore.undo")
	if err != nil {
		return Result{}, err
	}
	defer release()
	const step = "restore.undo"
	rt := runtimeFor(env, false)
	local := steps.ClusterPG(spec)
	if resuming {
		logLine(env.Report, step, "resuming the undo of "+p.Database+": swapping it back to "+back.Name+"; the restored data becomes the cluster "+u.Aside)
	} else {
		u.Abandoned, _ = backup.Timeline(ctx, local)
		u.Aside = backup.PreRestoreName(p.Database, time.Now())
		if u.Aside == back.Name {
			u.Aside += "b"
		}
		if back.Stanza != "" && back.Stanza != spec.Stanza {
			// The restored data's stanza is dropped below; its window ends
			// at its last WAL.
			u.LastArchivedAt = lastArchivedTime(ctx, spec)
		}
		u.Stanza, u.StartedAt = spec.Stanza, stamp()
		// Recorded before any rename, so an undo that fails part way
		// resumes with the same names, and the aside is never lost.
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			s.Undoing[p.Database] = u
			return nil
		}); err != nil {
			return Result{}, err
		}
		logLine(env.Report, step, "swapping "+p.Database+" back to "+back.Name+"; the restored data becomes the cluster "+u.Aside)
	}
	aside, abandoned, oldArchived := u.Aside, u.Abandoned, u.LastArchivedAt
	if u.Stanza != "" {
		// A resumed undo may have switched cluster.toml's stanza already;
		// the restored data's stanza is the one recorded at the start.
		spec.Stanza = u.Stanza
	}
	resume := steps.PauseDatabase(ctx, p.Database)
	err = backup.Swap(ctx, rt.Shell, step, env.Report, cfg.Server.DataRoot, p.Database, back.Name, aside, back.Port)
	if err == nil {
		err = backup.StartCluster(ctx, rt.Shell, step, env.Report, p.Database, cfg.Server.DataDirectory(p.Database), spec.Port, backup.UntilOpen)
	}
	resume()
	if err != nil {
		return Result{}, withHint(err, "Read vaire logs postgres --db "+p.Database+", then run vaire restore undo "+p.Database+" again: it resumes; the restored data is in the cluster "+aside+".")
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
		s.Leftovers = append(kept, backup.Leftover{Database: p.Database, Name: aside, CreatedAt: stamp(), RestoreID: undoRestoreID(back.RestoreID), Port: back.Port, Stanza: spec.Stanza})
		if stanza != spec.Stanza {
			dropped := s.DroppedStanzas[:0]
			for _, d := range s.DroppedStanzas {
				if d.Stanza != stanza {
					dropped = append(dropped, d)
				}
			}
			s.DroppedStanzas = append(dropped, replacedStanza(cfg, spec, oldArchived))
			delete(s.LastVerify, p.Database)
		}
		s.Timelines[p.Database] = backup.TimelineSwitch{Stanza: stanza, Timeline: timeline, SwitchedAt: stamp(), Abandoned: abandoned}
		delete(s.Undoing, p.Database)
		return nil
	}); err != nil {
		return Result{}, err
	}
	data := map[string]any{"database": p.Database, "restored_from": back.Name, "pre_undo": aside, "timeline": timeline, "stanza": stanza}
	var warnings []output.Warning
	if stanza != spec.Stanza {
		ctx := engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, stanzaRuns(p.Database)...))
		err = applyStanza(ctx, cfg, p.Database, rt, env.Report)
	}
	if err == nil {
		// The full backup's job takes the cluster lock, so let it go.
		release()
		err = startFullBackup(ctx, cfg, rt, p.Database, step, env.Report)
	}
	if err != nil {
		warnings = append(warnings, output.Warning{Code: "after_cutover_failed",
			Message: p.Database + " is back, but " + err.Error() + "; run vaire apply, then vaire backup now --db " + p.Database + " --type full"})
	}
	steps.WriteMetrics(cfg)
	warnings = append(warnings, followersDiverged(cfg, p.Database)...)
	return Result{Data: data, Warnings: warnings}, nil
}

// undoPrefix marks the RestoreID of the leftover a completed undo leaves:
// the restored data it swapped out. cleanup drops it like any leftover, but
// undo never swaps it back in, so a second undo can't redo the restore.
const undoPrefix = "undo-"

func undoRestoreID(restoreID string) string { return undoPrefix + restoreID }

// fromUndo reports whether l is the restored data a completed undo set aside.
func fromUndo(l backup.Leftover) bool { return strings.HasPrefix(l.RestoreID, undoPrefix) }

// planUndo decides what an undo of db does. An undo recorded in Undoing
// stopped part way and resumes with its recorded names. Otherwise it swaps
// back the newest leftover, the data the last promote replaced. When the
// newest leftover is the aside of a completed undo, the last restore was
// undone already and there is nothing to undo.
func planUndo(bs backup.Records, db string) (backup.Undo, bool, error) {
	if u, ok := bs.Undoing[db]; ok {
		return u, true, nil
	}
	leftovers := bs.LeftoversOf(db)
	if len(leftovers) == 0 {
		return backup.Undo{}, false, output.NewError(output.CodePrecondition, "there is no pre-restore copy of "+db+" to go back to", output.ExitPrecondition,
			"restore cleanup may already have dropped it.").WithDetails(map[string]any{"reason": "no_leftover"})
	}
	last := leftovers[len(leftovers)-1]
	if fromUndo(last) {
		return backup.Undo{}, false, output.NewError(output.CodePrecondition, db+" was already undone; nothing to undo", output.ExitPrecondition,
			"The restored data it left is kept as the stopped cluster "+last.Name+"; vaire restore cleanup "+db+" drops it.").
			WithDetails(map[string]any{"reason": "already_undone", "aside": last.Name})
	}
	return backup.Undo{From: last}, false, nil
}

// noOpenRestore is abort's refusal when db has no open restore. It says
// what happened to the last one: an undo under way, promoted or undone.
func noOpenRestore(bs backup.Records, db string) error {
	reason, msg, hint := "no_open_restore", "there is no open restore of "+db, ""
	if _, ok := bs.Undoing[db]; ok {
		reason, msg = "undo_in_progress", "there is no open restore of "+db+"; an undo of its last restore stopped part way"
		hint = "Finish it with vaire restore undo " + db + " --confirm " + db + "/restore (it resumes)."
	} else if leftovers := bs.LeftoversOf(db); len(leftovers) > 0 {
		last := leftovers[len(leftovers)-1]
		if fromUndo(last) {
			reason, msg = "restore_undone", "there is no open restore of "+db+"; its last restore was promoted and then undone"
			hint = "The restored data is kept as the stopped cluster " + last.Name + "; vaire restore cleanup " + db + " drops it."
		} else {
			reason, msg = "restore_promoted", "there is no open restore of "+db+"; its last restore was promoted already"
			hint = "vaire restore undo " + db + " swaps the old data (the cluster " + last.Name + ") back in; vaire restore cleanup " + db + " drops it."
		}
	}
	return output.NewError(output.CodePrecondition, msg, output.ExitPrecondition, hint).WithDetails(map[string]any{"reason": reason})
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
		return Result{}, noOpenRestore(bs, p.Database)
	}
	if r.Phase == backup.PhaseRestoring {
		st, err := restoreAbsenceState()
		if err != nil {
			return Result{}, err
		}
		if job, ok := st.Job(r.ID); ok && !job.Final() {
			return Result{}, output.NewError(output.CodePrecondition, "the restore of "+p.Database+" is still materialising", output.ExitPrecondition,
				"Wait for it (vaire job follow "+r.ID+"), then abort.").WithDetails(map[string]any{"reason": "restore_running", "job_id": r.ID})
		}
	}
	if r.Attempt != nil || r.MaterialisationPending {
		if r.Attempt == nil && r.MaterialisationPending && !env.Req.DryRun {
			if err := retireNeverStartedRestore(ctx, env.Cluster, p.Database, r); err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"database": p.Database, "dropped": "", "never_started": true, "live_untouched": true}}, nil
		}
		if r.Attempt != nil && !env.Req.DryRun {
			cleanup, err := openRestoreCleanup(env.Cluster, r)
			if err != nil {
				return Result{}, err
			}
			defer cleanup.close()
			if err := cleanup.remove(ctx, env); err != nil {
				return Result{}, err
			}
			if err := cleanup.retire(ctx); err != nil {
				return Result{}, err
			}
			var warnings []output.Warning
			if err := restoreAbortReload(ctx, env, env.Cluster); err != nil {
				warnings = append(warnings, output.Warning{Code: "pooler_reload_failed", Message: "the session pooler still lists " + backup.RestoringName(p.Database) + " until vaire apply: " + err.Error()})
			}
			restoreWriteMetrics(env.Cluster)
			return Result{Data: map[string]any{"database": p.Database, "dropped": backup.RestoringName(p.Database), "live_untouched": true}, Warnings: warnings}, nil
		}
		return Result{}, incompleteMaterialisation(backup.RestoringName(p.Database))
	}
	aside := r.Aside
	if aside == "" {
		aside = swappedAside(env.Cluster, p.Database, bs, r)
	}
	if aside != "" && backup.SwapState(env.Cluster.Server.DataRoot, p.Database, backup.RestoringName(p.Database), aside) != backup.SwapNotStarted {
		// A promote that failed after its first rename: the copy is (being)
		// swapped in, and the old data is aside. Dropping the record would
		// lose both.
		return Result{}, output.NewError(output.CodePrecondition, "the restored copy of "+p.Database+" is already swapped in by a promote that stopped part way; the old data is the cluster "+aside,
			output.ExitPrecondition, "Finish it with vaire restore promote "+p.Database+" --confirm "+p.Database+"/restore (it resumes), then vaire restore undo "+p.Database+" to go back.").
			WithDetails(map[string]any{"reason": "restore_swapped_in", "aside": aside})
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"database": p.Database, "drop": backup.RestoringName(p.Database), "dry_run": true}}, nil
	}
	if err := dropRestoring(env.Cluster, p.Database, env.Report); err != nil {
		return Result{}, err
	}
	if r.Aside != "" {
		// A promote that stopped before its renames may have registered
		// the aside already.
		if err := backup.UnregisterAside(env.Cluster.Server.DataRoot, r.Aside); err != nil {
			return Result{}, err
		}
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		cur, ok := s.Restores[p.Database]
		if !ok || cur.ID != r.ID || cur.Attempt != nil || cur.MaterialisationPending {
			return incompleteMaterialisation(backup.RestoringName(p.Database))
		}
		delete(s.Restores, p.Database)
		return nil
	}); err != nil {
		return Result{}, err
	}
	var warnings []output.Warning
	if err := reloadPoolers(ctx, env, env.Cluster); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the session pooler still lists " + backup.RestoringName(p.Database) + " until vaire apply: " + err.Error()})
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
		info := leftoverInfo{Leftover: l, Exists: legacyLeftoverExists(ctx, l.Name)}
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
		a, err := openLegacyLeftover(env.Cluster, l.Leftover)
		if err != nil {
			return Result{}, err
		}
		err = a.remove(ctx, env)
		if err == nil {
			err = retireLegacyLeftover(env.Cluster, l.Leftover)
		}
		a.close()
		if err != nil {
			return Result{}, err
		}
		dropped = append(dropped, l.Name)
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
	Restoring        string      `json:"restoring_database"`
	Tables           []tableRows `json:"tables,omitempty"`
	OnPromote        string      `json:"on_promote"`
	ApplicationReady *bool       `json:"application_ready,omitempty"`
	AccessCoverage   string      `json:"access_coverage,omitempty"`
	AccessNotice     string      `json:"access_notice,omitempty"`
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
		if r.Recover {
			ready := false
			st.ApplicationReady = &ready
			st.AccessCoverage = "unknown"
			st.AccessNotice = "Restore readiness is for inspection; application access is not ready until promotion and access provisioning succeed."
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

// --- db fork ---

type dbForkParams struct {
	Source string `json:"source"`
	Name   string `json:"name"`
	At     string `json:"at,omitempty"`
	// Size replaces the source's size (and its memory override) for the
	// fork; empty keeps the source's.
	Size string `json:"size,omitempty"`
	// AllowOvercommit forks even when the fork's size pushes the databases'
	// sizes past the budget, with the memory_overcommitted warning.
	AllowOvercommit bool `json:"allow_overcommit,omitempty"`
}

// handleDBFork checks the source, the name, the window (with --at) and room
// for one copy, then, under the exclusive lock, adds the new database to
// cluster.toml as db create with an explicit size does (a port and a stanza
// of its own; the source's size, limits, pools, profile, statement timeout,
// extensions and settings, refused only when it could not start, with the
// oversubscription warnings otherwise) without creating its cluster. The detachable job
// db-fork makes the cluster from the source (RFC 10.2) with the role
// passwords from the request.
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
	spec := forkSpec(src, p.Name)
	if p.Size != "" {
		spec.Size, spec.Memory = p.Size, ""
	}
	if err := checkSpareCPU(ctx, spec.Name, spec.CPUMode); err != nil {
		return Result{}, err
	}
	if err := guardRecoveryFloorAddition(cfg, spec, runtimeFor(env, true).Host); err != nil {
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
	if _, err := forkSpace(ctx, cfg, p.Source); err != nil {
		return Result{}, err
	}
	var release engine.Release
	if !env.Req.DryRun {
		var err error
		release, err = forkAcquire(ctx, engine.LockExclusive, "db.fork", env.Req.RequestID, env.Req.LockTimeout())
		if err != nil {
			return Result{}, err
		}
	}
	saved, _, created, err := addDatabaseFrom(ctx, env, func(cfg config.Cluster) (config.ClusterSpec, error) {
		src, ok := cfg.ClusterByName(p.Source)
		if !ok {
			return config.ClusterSpec{}, databaseNotFound(p.Source)
		}
		spec := forkSpec(src, p.Name)
		if p.Size != "" {
			spec.Size, spec.Memory = p.Size, ""
		}
		if err := checkSpareCPU(ctx, spec.Name, spec.CPUMode); err != nil {
			return config.ClusterSpec{}, err
		}
		return spec, nil
	}, p.AllowOvercommit, forkCommand(p), func(cfg config.Cluster, placed config.ClusterSpec) error {
		source, ok := cfg.ClusterByName(p.Source)
		if !ok {
			return databaseNotFound(p.Source)
		}
		return forkPrepareOwnership(env, cfg, source, placed)
	})
	if release != nil {
		release()
	}
	if err != nil {
		return Result{}, err
	}
	if env.Req.DryRun {
		return Result{Data: map[string]any{"source": p.Source, "name": p.Name, "at": p.At, "dry_run": true}}, nil
	}
	env.Cluster = saved
	// The new roles' passwords for the rename, and what the fork's steps
	// read (the service passwords db fork sends, as db create does).
	jobSecrets := stepSecrets(env.Req.Secrets)
	for _, role := range steps.DatabaseRoles {
		key := steps.SecretDatabaseRole(p.Name, role)
		jobSecrets[key] = env.Req.Secrets[key]
	}
	id, err := forkStartJob(ctx, env, "db-fork", p, jobSecrets)
	if err != nil {
		return Result{}, err
	}
	res, err := followJob(ctx, env, id)
	if err != nil {
		return res, err
	}
	if m, ok := res.Data.(map[string]any); ok {
		m["database"] = created
	}
	res.Warnings = append(res.Warnings, oversubscriptionWarnings(saved, runtimeFor(env, false).Host)...)
	return res, nil
}

var (
	forkSpace    = restoreSpace
	forkAcquire  = engine.Acquire
	forkStartJob = startJob
)

// forkCommand is the db fork command of p at another size: what the
// memory_overcommitted refusal suggests.
func forkCommand(p dbForkParams) func(size string) string {
	return func(size string) string {
		cmd := "vaire db fork " + p.Source + " --name " + p.Name
		if p.At != "" {
			cmd += " --at " + p.At
		}
		return cmd + " --size " + size
	}
}

// forkSpec is the fork's cluster.toml entry before placeCluster gives it a
// node, port and stanza: the source's size and limits, pools, profile,
// statement timeout, extensions and [clusters.settings], so it runs as the
// source does. Its backup schedules are the defaults, as a new database's.
func forkSpec(src config.ClusterSpec, name string) config.ClusterSpec {
	return config.ClusterSpec{
		Name:             name,
		Size:             src.Size,
		Memory:           src.Memory,
		MemoryMax:        src.MemoryMax,
		CPUWeight:        src.CPUWeight,
		CPUMode:          src.CPUMode,
		CPUQuota:         src.CPUQuota,
		IOWeight:         src.IOWeight,
		Profile:          src.Profile,
		StatementTimeout: src.StatementTimeout,
		Extensions:       slices.Clone(src.Extensions),
		PoolSize:         src.PoolSize,
		SessionPoolSize:  src.SessionPoolSize,
		Settings:         maps.Clone(src.Settings),
	}
}

// jobFork makes the new database's cluster from the source (RFC 10.2):
// materialised into <data_root>/<new> from the source's stanza and promoted
// at --at, or copied with pg_basebackup of the live source without it
// (which needs no backups). The source's database and roles are renamed to
// the new name's, with the operator's new passwords, then the copy becomes
// 18/<new> with its own configuration, cgroup and port, the poolers route
// to it, and with backups on it gets its own stanza and first full. A
// failure before the copy is 18/<new> drops it and the cluster.toml entry.
var (
	forkMaterialise     = materialiseFork
	forkApplyKernel     = engine.Apply
	forkProvisionOwned  = provisionOwnedTarget
	forkApplyPoolers    = applyPoolers
	forkNewMaterialised = backup.NewForkMaterialised
	forkBaseBackup      = (*backup.Materialised).BaseBackup
	forkRestore         = restoreFork
	forkDrop            = (*backup.Materialised).Drop
)

func jobFork(ctx context.Context, env *jobEnv) (any, error) {
	var p dbForkParams
	if err := json.Unmarshal(env.Params, &p); err != nil {
		return nil, err
	}
	cfg := env.Cluster
	spec, ok := cfg.ClusterByName(p.Name)
	if !ok {
		return nil, databaseNotFound(p.Name)
	}
	authority, err := newForkMaterialisationAuthority(env, p.Name)
	if err != nil {
		return nil, err
	}
	defer authority.Close()
	if err := forkMaterialise(ctx, env, p, spec); err != nil {
		return nil, err
	}
	rt := jobRuntime(env)
	others := forkOtherConfigs(ctx, steps.Catalog(), cfg, rt, p.Name, "")
	ctx = engine.WithNumbering(ctx, engine.NewNumbering(steps.Catalog(), cfg, createRuns(cfg, p.Name, others)...))
	logLine(env.Report, env.Job, "starting "+p.Name+" with its own configuration")
	_, _, err = forkApplyKernel(ctx, steps.Catalog(), cfg, rt, engine.Options{Only: steps.KernelStepIDs(), Restarter: steps.Restarter(cfg), Report: env.Report})
	if err == nil {
		err = authority.withProvisionedForkTarget(ctx, func(owned context.Context) error {
			return forkProvisionOwned(owned, cfg, rt, p.Name, "", env.Report)
		})
	}
	if err != nil {
		return nil, withHint(err, "The incomplete fork remains blocked; inspect its job and retained state.")
	}
	if err := authority.markProvisioned(ctx, func() error { return forkVerifyProvision(ctx, cfg, rt, p.Name) }); err != nil {
		return nil, err
	}
	if err := authority.retire(ctx); err != nil {
		return nil, err
	}
	var warnings []output.Warning
	if err := forkApplyPoolers(ctx, cfg, rt, "", env.Report); err != nil {
		warnings = append(warnings, output.Warning{Code: "pooler_reload_failed",
			Message: "the poolers were not reloaded, so " + p.Name + " is not reachable through them until vaire apply: " + err.Error()})
	}
	if w := forkApplyMetrics(ctx, cfg, rt, env.Report, p.Name, ""); w != nil {
		warnings = append(warnings, *w)
	}
	warnings = append(warnings, forkApplyOtherConfigs(ctx, steps.Catalog(), cfg, rt, others,
		engine.Options{Restarter: steps.Restarter(cfg), Report: env.Report})...)
	if w := forkFirstBackup(ctx, cfg, rt, env.Report, p.Name); w != nil {
		warnings = append(warnings, *w)
	}
	forkWriteMetrics(cfg)
	return map[string]any{"source": p.Source, "name": p.Name, "at": p.At, "port": spec.Port, "stanza": spec.Stanza, "warnings": warnings}, nil
}

// materialiseFork builds the fork's data in the temporary cluster <new>,
// promotes it, renames it, and adopts it as 18/<new> on spec's port. A
// failure drops what was built.
func materialiseFork(ctx context.Context, env *jobEnv, p dbForkParams, spec config.ClusterSpec) error {
	cfg := env.Cluster
	src, err := clusterOf(cfg, p.Source)
	if err != nil {
		return err
	}
	authority := activeForkAuthority(env)
	if authority == nil {
		return errors.New("fork authority unavailable")
	}
	destination, err := prepareMaterialisationConfig(cfg, spec)
	if err != nil {
		return err
	}
	m, err := forkNewMaterialised(cfg.Server.DataRoot, p.Name, p.Source, src.Port, env.shell(), env.Job, env.Report)
	if err != nil {
		return err
	}
	defer m.ReleaseForkReservationAuthority()
	if err := authority.bindReservation(ctx, m); err != nil {
		return err
	}
	m.Ownership = authority
	m.DestinationConfig = destination
	if err := authority.advance(ctx, materialisationCopying, ""); err != nil {
		return err
	}
	if p.At == "" {
		err = forkBaseBackup(m, ctx)
	} else {
		err = forkRestore(ctx, env, p, src, m)
	}
	if err == nil {
		// The copy stops before Adopt starts it as 18/<new>, which drops
		// the recovery targets with it.
		_, err = forkPromote(m, ctx)
	}
	if err == nil {
		logLine(env.Report, env.Job, "renaming "+p.Source+" and its roles to "+p.Name)
		err = m.GuardOwnership(ctx, backup.MaterialisationRename, func() error {
			return forkRename(ctx, m.PG(), p.Source, p.Name, env.Secrets)
		})
	}
	if err == nil {
		err = forkAdopt(m, ctx, spec.Port)
	}
	if err != nil {
		if reason := recoveryFailureReason(err); reason != "" {
			return errors.Join(err, authority.advance(ctx, materialisationFailed, reason))
		}
		return errors.Join(err, cleanupFailedFork(ctx, authority, m, p.Name))
	}
	return authority.advance(ctx, materialisationProvisioning, "")
}

// restoreFork restores the source's stanza into m and promotes it at --at,
// on the timeline restore undo recorded for the stanza, if any.
func restoreFork(ctx context.Context, env *jobEnv, p dbForkParams, src config.ClusterSpec, m *backup.Materialised) error {
	t, err := time.Parse(time.RFC3339, p.At)
	if err != nil {
		return err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	opts := backup.RestoreOptions{Kind: backup.TargetTime, Time: t, Timeline: bs.TargetTimeline(p.Source, src.Stanza)}
	if err := m.Restore(ctx, driver(src, env.Job, env.Report), opts); err != nil {
		return err
	}
	if at := m.RecoveredTo(ctx); at != "" {
		logLine(env.Report, env.Job, "recovered to "+at)
	}
	return nil
}

// renameFork renames the source's database and roles to the fork's in the
// cluster at local (RFC 10.2). Ownership, grants, memberships, default
// privileges and per-role settings follow the role OIDs; only
// <name>_migrate's SET role names the owner, so it is rewritten. The login
// roles get the passwords the operator generated for the fork, as SCRAM
// verifiers: the source's no longer work. A fork's copy always has every old
// name, so it renames them all; db unfollow reads which are left instead.
func renameFork(ctx context.Context, local sqlRunner, source, name string, passwords map[string]string) error {
	statements, err := renameStatements(renameState{database: true, roles: renameRoles()}, source, name, passwords)
	if err != nil {
		return err
	}
	return local.Exec(ctx, "postgres", statements, nil)
}

// preflightDropLeftovers refuses incomplete work before db.drop touches the
// live cluster. In particular, no layout of an interrupted undo is treated as
// a completed leftover or as permission to remove a config-only aside.
func preflightDropLeftovers(cfg config.Cluster, db string) error {
	s, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	if u, ok := s.Undoing[db]; ok {
		if u.From.Database != db || !validGeneratedLeftoverName(db, u.From.Name) ||
			!validGeneratedLeftoverName(db, u.Aside) || u.From.Name == u.Aside {
			return errors.New("cannot inspect unverifiable undo layout")
		}
		layout := backup.SwapState(cfg.Server.DataRoot, db, u.From.Name, u.Aside)
		return fmt.Errorf("cannot drop %s while undo is incomplete (swap %s)", db, layout)
	}
	if _, ok := s.Drilling[db]; ok {
		return fmt.Errorf("cannot drop %s with an unfinished drill", db)
	}
	if _, ok := s.DrillAttempts[db]; ok {
		return fmt.Errorf("cannot drop %s with a retained drill attempt", db)
	}
	if r, ok := s.Restores[db]; ok && (r.Attempt != nil || r.MaterialisationPending) {
		return incompleteMaterialisation(backup.RestoringName(db))
	}
	for _, l := range s.LeftoversOf(db) {
		a, err := openLegacyLeftover(cfg, l)
		if err != nil {
			return err
		}
		a.close()
	}
	return nil
}

// dropLeftovers removes only currently verified completed leftovers. Retained
// or interrupted work is never downgraded to name-only cleanup.
func dropLeftovers(env *Env, db string) error {
	if err := preflightDropLeftovers(env.Cluster, db); err != nil {
		return err
	}
	bs, err := backup.LoadRecords()
	if err != nil {
		return err
	}
	for _, l := range bs.LeftoversOf(db) {
		a, err := openLegacyLeftover(env.Cluster, l)
		if err != nil {
			return err
		}
		err = a.remove(context.Background(), env)
		if err == nil {
			err = retireLegacyLeftover(env.Cluster, l)
		}
		a.close()
		if err != nil {
			return err
		}
	}
	return backup.UpdateRecords(func(s *backup.Records) error {
		_, drilling := s.Drilling[db]
		_, attempt := s.DrillAttempts[db]
		if len(s.LeftoversOf(db)) != 0 || drilling || attempt {
			return errors.New("database cleanup records changed")
		}
		if _, ok := s.Undoing[db]; ok {
			return errors.New("database undo appeared during cleanup")
		}
		if _, ok := s.Restores[db]; ok {
			return errors.New("database restore appeared during cleanup")
		}
		forgetDropped(s, db)
		return nil
	})
}

// forgetDropped removes what records.json holds of the dropped database db:
// its leftovers, timeline switch, drill, undo and its awaiting recovery
// entry. A database created again under its name starts with none of them;
// an awaiting entry would keep it out of every step, so it never got a
// cluster.
func forgetDropped(s *backup.Records, db string) {
	kept := s.Leftovers[:0]
	for _, l := range s.Leftovers {
		if l.Database != db {
			kept = append(kept, l)
		}
	}
	s.Leftovers = kept
	delete(s.Timelines, db)
	delete(s.Drilling, db)
	delete(s.Undoing, db)
	delete(s.AwaitingRecovery, db)
	// An unfollow that was under way for db goes with it: a database created
	// again under its name starts its own life.
	delete(s.Unfollowing, db)
}
