package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/steps"
)

// JobDir holds the specs of detachable jobs between the RPC that starts one
// and the unit that runs it. The secrets in a spec are read once and the file
// is deleted before the job does anything else.
const JobDir = "/var/lib/vaire/jobs"

// jobNames lists every job vaire node run accepts. restore, db-fork, db-copy
// and dump-create run only as detachable jobs an RPC starts (--job-id).
var jobNames = []string{
	"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill", "db-dump",
	"stanza-expiry", "restore", "db-fork", "maint", "recover",
	"maint-daily", "maint-weekly", "certs-renew", "metrics-refresh", "doctor",
	"master-backup", "fence-check", "db-follow", "db-unfollow", "db-copy",
}

// bucketWriteJobs write to or delete from the bucket, so they check the
// epoch fence first (Phase 5, D5).
var bucketWriteJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-incr": true, "db-dump": true,
	"stanza-expiry": true, "db-fork": true, "db-copy": true, "restore": true, "recover": true,
	// db unfollow creates the follower's stanza and its first full (Phase 6).
	"db-unfollow": true,
}

// locklessJobs take no node lock (Phase 5): the master backup must never
// wait for a long apply, and the fence check must run at boot whatever
// holds the lock. The master backup has its own lock.
var locklessJobs = map[string]bool{"master-backup": true, "fence-check": true}

// rpcOnlyJobs need a spec written by the RPC that starts them.
var rpcOnlyJobs = map[string]bool{"restore": true, "db-fork": true, "db-copy": true, "maint": true, "recover": true, "db-follow": true, "db-unfollow": true}

// quietJobs run often and are not recorded in state.json's job list, which
// keeps only the last 50 runs: metrics-refresh runs every minute and would
// push the backups out of it within the hour. They take the node lock
// shared and skip at once when it is busy (Phase 4).
var quietJobs = map[string]bool{"metrics-refresh": true, "doctor": true}

// databaseJobs are the template-unit jobs: each runs for one database, named
// by --db on a timer run (vaire-<job>@<db>) or by the RPC's params.
var databaseJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-incr": true, "backup-verify": true, "restore-drill": true, "db-dump": true,
}

// heavyIOJobs take the server's heavy-I/O lock (RFC 9.3), so at most one of
// them reads the disk and pushes to the bucket at a time. Incrementals are
// small and frequent; they only take their cluster's lock.
var heavyIOJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-verify": true, "restore-drill": true, "db-dump": true,
	"db-copy": true,
}

// exclusiveJobs change the desired state and a cluster's own identity, so they
// take the node lock exclusive and their clusters' locks exclusive: no other
// job or RPC writes cluster.toml while they run (db create, config set and db
// drop take the node lock exclusive the same way), a second unfollow of the
// same follower can't promote and rename beside the first, and an apply of that
// cluster waits. Nothing they run takes the node or cluster lock again, so the
// exclusive lock can't deadlock with themselves.
var exclusiveJobs = map[string]bool{
	"db-unfollow": true, "db-copy": true,
}

// JobOptions are vaire node run's flags.
type JobOptions struct {
	// ID names a detachable job whose spec an RPC wrote.
	ID string
	// DB is the database of a template-unit timer run (vaire-<job>@<db>).
	DB string
}

// jobSpec is JobDir/<id>.json.
type jobSpec struct {
	Job     string            `json:"job"`
	Params  json.RawMessage   `json:"params,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	// OperationID and ParentRequestID carry the RPC correlation into the
	// detached job so its events belong to the same logical operation.
	OperationID     string `json:"operation_id,omitempty"`
	ParentRequestID string `json:"parent_request_id,omitempty"`
}

// jobEnv is what a job function gets.
type jobEnv struct {
	ID              string
	Job             string
	Cluster         config.Cluster
	Params          json.RawMessage
	Secrets         map[string]string
	Report          engine.Reporter
	DB              string
	OperationID     string
	ParentRequestID string
}

func (j *jobEnv) shell() engine.Shell {
	return engine.Shell{Secrets: secrets.Default}
}

func knownJob(job string) bool { return slices.Contains(jobNames, job) }

// RunJob runs one job of vaire node run: a timer run or a detachable job an RPC
// started. It records the job in state.json, takes the node lock (shared),
// the heavy-I/O lock for heavy backup jobs and the clusters' locks, runs it
// and records the outcome. A busy lock records skipped and exits 0: at once
// for a job an RPC started or a run by hand, after up to jobLockWait for a
// timer run (certs-renew waits up to 20 minutes for the node lock). skipped
// is why a skipped job didn't run, empty when it ran.
func RunJob(ctx context.Context, job string, opts JobOptions, report engine.Reporter) (res any, skipped string, err error) {
	act := activityRecorderFactory()
	var ja *jobActivity
	// The activity finalizer is registered first, so every lock release
	// deferred below runs before its append. A failed recorder never changes
	// the job's result and never triggers a retry. A panic records unknown
	// and is re-raised: history must not turn an interrupted job into success.
	defer func() {
		r := recover()
		finishRunJobActivity(ja, err, skipped, r)
		if r != nil {
			panic(r)
		}
	}()

	if !knownJob(job) {
		err := output.NewError(output.CodeUsage, "unknown job "+job, output.ExitUsage, "Use one of details.jobs.").
			WithDetails(map[string]any{"jobs": jobNames})
		recordJobRefusal(act, job, opts.ID, err, "unknown_job", timerRun(opts))
		return nil, "", err
	}
	if _, err := os.Stat(config.ClusterPath); errors.Is(err, os.ErrNotExist) {
		err := output.NewError(
			output.CodePrecondition,
			"this machine is not an vaire node: "+config.ClusterPath+" does not exist",
			output.ExitPrecondition,
			"vaire node run runs on the server. From your machine: vaire ssh --command 'sudo vaire node run "+job+"'",
		).WithDetails(map[string]any{"reason": "not_a_node"})
		recordJobRefusal(act, job, opts.ID, err, "not_a_node", timerRun(opts))
		return nil, "", err
	}
	if os.Geteuid() != 0 {
		err := rootRequired("vaire node run")
		recordJobRefusal(act, job, opts.ID, err, "root_required", timerRun(opts))
		return nil, "", err
	}
	if job == "certs-renew" {
		id := activityJobID(opts.ID)
		ja = newJobActivity(act, job, id, id, "", "", timerRun(opts), opts.ID != "", false)
		rep, berr := ja.begin(ctx, report)
		if berr != nil {
			return nil, "", berr
		}
		release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), certsLockWait)
		if engine.IsLockHeld(err) {
			ja.pending(activity.OutcomeSkipped, output.CodePrecondition, "lock_held")
			logLine(rep, job, "the node lock is busy; skipping "+job)
			return map[string]any{"job": job, "skipped": "lock_busy"}, "the node lock was busy", nil
		}
		if err != nil {
			return nil, "", err
		}
		defer release()
		res, err := certsRenew(ctx, rep)
		return res, "", err
	}
	if locklessJobs[job] {
		id := activityJobID(opts.ID)
		ja = newJobActivity(act, job, id, id, "", "", timerRun(opts), opts.ID != "", false)
		rep, berr := ja.begin(ctx, report)
		if berr != nil {
			return nil, "", berr
		}
		cfg, err := config.LoadCluster(config.ClusterPath)
		if err != nil {
			return nil, "", err
		}
		if job == "fence-check" {
			res, err := runFenceCheck(ctx, cfg, rep)
			return res, "", err
		}
		res, err := runMasterBackup(ctx, cfg, rep, masterBackupOptions{debounce: true})
		if err == nil && res.Skipped != "" {
			ja.pending(activity.OutcomeSkipped, "", safeActivityToken(res.Skipped))
		}
		return res, "", err
	}
	if quietJobs[job] {
		id := activityJobID(opts.ID)
		ja = newJobActivity(act, job, id, id, "", "", timerRun(opts), opts.ID != "", false)
		rep, berr := ja.begin(ctx, report)
		if berr != nil {
			return nil, "", berr
		}
		release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), 0)
		if engine.IsLockHeld(err) {
			ja.pending(activity.OutcomeSkipped, output.CodePrecondition, "lock_held")
			logLine(rep, job, "the node lock is busy; skipping "+job)
			return map[string]any{"job": job, "skipped": "lock_busy"}, "the node lock was busy", nil
		}
		if err != nil {
			return nil, "", err
		}
		defer release()
		cfg, err := config.LoadCluster(config.ClusterPath)
		if err != nil {
			return nil, "", err
		}
		res, err := runQuietJob(ctx, job, cfg, rep)
		return res, "", err
	}
	if rpcOnlyJobs[job] && opts.ID == "" {
		err := output.NewError(output.CodeUsage, "job "+job+" is started by its operator command, not by hand", output.ExitUsage,
			"Use vaire restore, vaire db fork or vaire db copy from your machine.")
		recordJobRefusal(act, job, opts.ID, err, "job_spec_required", timerRun(opts))
		return nil, "", err
	}

	env := &jobEnv{ID: opts.ID, Job: job, Report: report, DB: opts.DB}
	if env.ID == "" {
		env.ID = rpc.NewRequestID()
	} else {
		var spec jobSpec
		var err error
		if job == "db-copy" {
			spec, err = readCopyJobSpec(env.ID)
		} else {
			spec, err = readJobSpec(env.ID)
		}
		if err != nil {
			reason := jobSpecErrorReason(err)
			if reason == "" {
				reason = "job_spec_missing"
			}
			recordJobRefusal(act, job, env.ID, err, reason, timerRun(opts))
			if job == "db-copy" {
				// Consumption of the private spec is a definite pre-execution
				// refusal, not an ambiguous launch: no job function, config
				// load, source preflight or provisioning has run.
				rec := finishedJobRecord(engine.JobRecord{ID: env.ID, Job: job, DB: rpcJobDatabase(job, spec.Params)},
					engine.JobFailed, "private copy spec was not consumed", nil, err)
				putFinishedJob(rec)
			}
			return nil, "", err
		}
		if spec.Job != job {
			err := output.NewError(output.CodeUsage, "job "+env.ID+" is a "+spec.Job+" job, not "+job, output.ExitUsage, "")
			recordJobRefusal(act, job, env.ID, err, "job_spec_mismatch", timerRun(opts))
			return nil, "", err
		}
		env.Params, env.Secrets = spec.Params, spec.Secrets
		env.OperationID, env.ParentRequestID = spec.OperationID, spec.ParentRequestID
	}
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		recordJobRefusal(act, job, env.ID, err, "config_error", timerRun(opts))
		return nil, "", err
	}
	env.Cluster = cfg
	if databaseJobs[job] {
		env.DB = jobDatabase(env)
		if env.DB == "" {
			err := output.NewError(output.CodeUsage, "job "+job+" runs for one database", output.ExitUsage,
				"Pass --db <database>, as the vaire-"+job+"@<database> timer does.")
			recordJobRefusal(act, job, env.ID, err, "database_required", timerRun(opts))
			return nil, "", err
		}
		if _, ok := cfg.ClusterByName(env.DB); !ok {
			err := databaseNotFound(env.DB)
			recordJobRefusal(act, job, env.ID, err, "database_not_found", timerRun(opts))
			return nil, "", err
		}
	}

	db := env.DB
	if db == "" {
		db = rpcJobDatabase(job, env.Params)
	}
	ja = newJobActivity(act, job, env.ID, env.OperationID, env.ParentRequestID, db, timerRun(opts), opts.ID != "", !jobSafetyJob(job))
	rep, berr := ja.begin(ctx, env.Report)
	if berr != nil {
		// The RPC already wrote a queued record and its spec has been read
		// and removed. A refused checked start must leave that record failed,
		// not queued until a follower synthesizes a generic crash; the spec is
		// never recreated or replayed.
		if rec, ok := activityRefusedJobRecord(opts.ID != "", env.ID, job, db, berr); ok {
			putFinishedJob(rec)
		}
		return nil, "", berr
	}
	env.Report = rep

	rec := engine.JobRecord{ID: env.ID, Job: job, DB: env.DB, State: engine.JobRunning, StartedAt: stamp(), InvocationID: os.Getenv("INVOCATION_ID"), PID: os.Getpid()}
	if rec.DB == "" {
		rec.DB = rpcJobDatabase(job, env.Params)
	}
	_ = engine.UpdateState(func(st *engine.State) error {
		if old, ok := st.Job(env.ID); ok {
			rec.CreatedAt, rec.Unit = old.CreatedAt, old.Unit
		}
		if rec.CreatedAt == "" {
			rec.CreatedAt = rec.StartedAt
		}
		st.PutJob(rec)
		return nil
	})

	release, busy, err := acquireJobLocks(ctx, env, &rec, timerRun(opts))
	if busy != nil {
		res := map[string]any{"job": job, "job_id": env.ID, "skipped": "lock_busy"}
		for k, v := range busy.fields {
			res[k] = v
		}
		copySkippedFields(job, res)
		detail := busy.what + " was busy"
		if err != nil {
			// systemctl stop while the run waited for the lock.
			detail = "stopped while waiting for " + busy.what
			res["skipped"] = "stopped"
		} else {
			logLine(env.Report, job, busy.log+"; skipping "+job)
		}
		ja.pending(activity.OutcomeSkipped, output.CodePrecondition, jobSkipReason(busy, err))
		finishJobWithActivity(rec, ja, engine.JobSkipped, detail, res, nil)
		notRun(ctx, env, errors.New(detail))
		return res, detail, nil
	}
	if err != nil {
		finishJobWithActivity(rec, ja, engine.JobFailed, "", nil, err)
		return nil, "", err
	}
	defer release()

	if bucketWriteJobs[job] {
		if err := requireNotFenced(ctx, cfg, env.Report); err != nil {
			code, reason := activityErrorTokens(err)
			if reason == "" {
				reason = "node_fenced"
			}
			ja.pending(activity.OutcomeRefused, code, reason)
			finishJobWithActivity(rec, ja, engine.JobFailed, "", nil, err)
			return nil, "", err
		}
	}
	res, err = runJobFunc(ctx, env)
	if err != nil {
		if errors.Is(err, errSkipped) {
			ja.pending(activity.OutcomeSkipped, "", "job_skipped")
			finishJobWithActivity(rec, ja, engine.JobSkipped, err.Error(), res, nil)
			return res, strings.TrimPrefix(err.Error(), errSkipped.Error()+": "), nil
		}
		finishJobWithActivity(rec, ja, engine.JobFailed, "", nil, err)
		return nil, "", err
	}
	finishJobWithActivity(rec, ja, engine.JobSucceeded, "", res, nil)
	return res, "", nil
}

// activityJobID gives a job invocation an id for its events even when it is a
// timer run that state.json does not keep (quiet and certificate jobs).
func activityJobID(id string) string {
	if id != "" {
		return id
	}
	return rpc.NewRequestID()
}

// jobLockWait is how long a timer run waits for its locks before it is
// recorded as skipped: an apply, a db create or another backup job holding
// them would otherwise cost the run until the next timer, a day for a daily
// dump or full.
const jobLockWait = 15 * time.Minute

// jobLockPoll is how often a waiting timer run tries its locks again.
const jobLockPoll = 2 * time.Second

// timerRun reports whether this run was started by a systemd timer: a run
// with no job id (not an RPC's job) inside a unit (not by hand).
func timerRun(opts JobOptions) bool {
	return opts.ID == "" && os.Getenv("INVOCATION_ID") != ""
}

// jobLockBusy is the lock a job found busy.
type jobLockBusy struct {
	// what names the lock, as in "the node lock was busy".
	what string
	// log is the line logged when the job skips.
	log string
	// fields go into the skipped job's result.
	fields map[string]any
}

// jobLockers are the node's lock acquisitions, as tryJobLocks takes them:
// nodeLockers holds the node's own, and a test passes ones that record the
// modes asked for and hold them, so the node's /run/lock files aren't needed.
type jobLockers struct {
	node    func(ctx context.Context, mode engine.LockMode, method, requestID string, timeout time.Duration) (engine.Release, error)
	heavyIO func(ctx context.Context, method, requestID string) (engine.Release, error)
	cluster func(ctx context.Context, cluster string, mode engine.LockMode, method, requestID string, timeout time.Duration) (engine.Release, error)
}

var nodeLockers = jobLockers{node: engine.Acquire, heavyIO: engine.AcquireHeavyIO, cluster: engine.AcquireCluster}

// jobLockMode is the mode a job takes the node lock and its clusters' locks in:
// shared for the common jobs, exclusive for the ones that change the desired
// state and a cluster's identity (exclusiveJobs). A follower follow or recovery
// records ownership of the deterministic data and configuration paths and may
// remove them, and a failed plain follow rolls back its cluster.toml entry, so
// both take the locks exclusive: no competing follow, drop or apply may race
// their record, their rebuild or the desired-state rollback.
func jobLockMode(env *jobEnv) engine.LockMode {
	if exclusiveJobs[env.Job] || env.Job == "db-follow" {
		return engine.LockExclusive
	}
	return engine.LockShared
}

// tryJobLocks takes a job's locks once each, in order: the node lock, the
// heavy-I/O lock for heavy jobs, then its clusters' locks, in the job's mode
// (shared, or exclusive for exclusiveJobs). On a busy lock it releases what it
// took and returns which one was busy.
func tryJobLocks(ctx context.Context, env *jobEnv) (engine.Release, *jobLockBusy, error) {
	return takeJobLocks(ctx, env, nodeLockers)
}

// takeJobLocks is tryJobLocks with the lock acquisitions given, so a test runs
// the real order and modes against its own locks.
func takeJobLocks(ctx context.Context, env *jobEnv, locks jobLockers) (engine.Release, *jobLockBusy, error) {
	var held []engine.Release
	releaseAll := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
	}
	mode := jobLockMode(env)
	release, err := locks.node(ctx, mode, env.Job, env.ID, 0)
	if engine.IsLockHeld(err) {
		return nil, &jobLockBusy{what: "the node lock", log: "the node lock is busy"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	held = append(held, release)
	if heavyIOJobs[env.Job] || maintHeavy(env) {
		release, err := locks.heavyIO(ctx, env.Job, env.ID)
		if engine.IsLockHeld(err) {
			releaseAll()
			return nil, &jobLockBusy{what: "the heavy-I/O lock", log: "another backup job holds the heavy-I/O lock",
				fields: map[string]any{"lock": "heavy_io", "db": env.DB}}, nil
		}
		if err != nil {
			releaseAll()
			return nil, nil, err
		}
		held = append(held, release)
	}
	for _, cluster := range jobClusters(env) {
		release, err := locks.cluster(ctx, cluster, mode, env.Job, env.ID, 0)
		if engine.IsLockHeld(err) {
			releaseAll()
			return nil, &jobLockBusy{what: "the lock of " + cluster, log: "the lock of " + cluster + " is busy",
				fields: map[string]any{"cluster": cluster}}, nil
		}
		if err != nil {
			releaseAll()
			return nil, nil, err
		}
		held = append(held, release)
	}
	return releaseAll, nil, nil
}

// acquireJobLocks takes a job's locks. Without wait a busy lock is returned
// at once. A timer run (wait) tries again every jobLockPoll for up to
// jobLockWait, logging and recording in rec what it waits for, and holds no
// lock between tries, so it never keeps an apply waiting for the node lock
// while it waits for the heavy-I/O lock. SIGTERM (systemctl stop) or SIGINT
// ends the wait: the busy lock comes back with the context's error.
func acquireJobLocks(ctx context.Context, env *jobEnv, rec *engine.JobRecord, wait bool) (engine.Release, *jobLockBusy, error) {
	release, busy, err := tryJobLocks(ctx, env)
	if busy == nil || err != nil || !wait {
		return release, busy, err
	}
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	deadline := time.Now().Add(jobLockWait)
	waitingFor := ""
	for {
		if busy.what != waitingFor {
			waitingFor = busy.what
			logLine(env.Report, env.Job, busy.log+"; waiting up to "+jobLockWait.String()+" for it before skipping "+env.Job)
			rec.Detail = "waiting for " + busy.what
			putJob(*rec)
		}
		select {
		case <-ctx.Done():
			return nil, busy, ctx.Err()
		case <-time.After(jobLockPoll):
		}
		release, next, err := tryJobLocks(ctx, env)
		if err != nil {
			return nil, nil, err
		}
		if next == nil {
			logLine(env.Report, env.Job, "got the locks; starting "+env.Job)
			rec.Detail = ""
			putJob(*rec)
			return release, nil, nil
		}
		busy = next
		if !time.Now().Before(deadline) {
			return nil, busy, nil
		}
	}
}

// putJob writes rec to state.json, keeping what the RPC recorded.
func putJob(rec engine.JobRecord) {
	_ = engine.UpdateState(func(st *engine.State) error {
		if old, ok := st.Job(rec.ID); ok {
			rec.CreatedAt, rec.Unit = old.CreatedAt, old.Unit
		}
		st.PutJob(rec)
		return nil
	})
}

// notRun undoes what the RPC of a skipped job prepared: a restore is
// recorded failed, so abort can clean it up, and a fork's or follower's
// cluster.toml entry is removed, as its cluster was never made. It holds no
// job lock, so the follower cleanup takes the exclusive node lock, reloads the
// desired state and removes only the entry its operation prepared.
func notRun(ctx context.Context, env *jobEnv, cause error) {
	switch env.Job {
	case "restore", "recover":
		failRestore(env, cause)
	case "db-fork":
		var p dbForkParams
		if json.Unmarshal(env.Params, &p) == nil && p.Name != "" {
			_ = forgetDatabase(p.Name)
		}
	case "db-follow":
		// A follower's entry whose cluster was never made; one awaiting
		// recovery stays awaiting.
		var p dbFollowParams
		if json.Unmarshal(env.Params, &p) == nil && p.Name != "" && !p.Recover && p.PreparedNew {
			if err := rollbackPreparedFollow(ctx, p); err != nil {
				logLine(env.Report, env.Job, "the skipped follow's cluster.toml entry was left for a retry: "+err.Error())
			}
		}
	}
}

// errSkipped marks a job that decided not to run, such as a backup while
// another one holds pgBackRest's lock.
var errSkipped = errors.New("skipped")

func runJobFunc(ctx context.Context, env *jobEnv) (any, error) {
	switch env.Job {
	case "backup-full", "backup-diff", "backup-incr":
		return jobBackup(ctx, env, strings.TrimPrefix(env.Job, "backup-"))
	case "backup-verify":
		return jobVerify(ctx, env)
	case "restore-drill":
		return jobDrill(ctx, env)
	case "db-dump":
		return jobDump(ctx, env)
	case "stanza-expiry":
		return jobStanzaExpiry(ctx, env)
	case "restore":
		return jobRestore(ctx, env)
	case "db-fork":
		return jobFork(ctx, env)
	case "db-copy":
		return jobCopy(ctx, env)
	case "db-follow":
		return jobFollow(ctx, env)
	case "db-unfollow":
		return jobUnfollow(ctx, env)
	case "maint":
		return jobMaint(ctx, env)
	case "maint-daily":
		return jobMaintDaily(ctx, env)
	case "maint-weekly":
		return jobMaintWeekly(ctx, env)
	case "recover":
		return jobRecover(ctx, env)
	}
	return nil, fmt.Errorf("job %s has no implementation", env.Job)
}

// runQuietJob runs a quiet job under the node lock.
func runQuietJob(ctx context.Context, job string, cfg config.Cluster, report engine.Reporter) (any, error) {
	switch job {
	case "metrics-refresh":
		if err := refreshMetrics(ctx, cfg); err != nil {
			return nil, err
		}
		return map[string]any{"job": job, "refreshed": true}, nil
	case "doctor":
		return runDoctorJob(ctx, cfg, report)
	}
	return nil, fmt.Errorf("job %s has no implementation", job)
}

// finishJob records a job's outcome.
func finishJob(rec engine.JobRecord, state, detail string, res any, cause error) {
	putFinishedJob(finishedJobRecord(rec, state, detail, res, cause))
}

// finishJobWithActivity publishes a terminal state and, while the job's
// activity scope is still open, marks its completion evidence pending. A
// follower then waits for the final activity write instead of concluding on a
// journal replay that ended before the evidence warning.
func finishJobWithActivity(rec engine.JobRecord, ja *jobActivity, state, detail string, res any, cause error) {
	if ja != nil {
		ja.recorded = true
	}
	rec.ActivityPending = ja != nil && !ja.finished
	finishJob(rec, state, detail, res, cause)
}

// JobOutcomeUnknown is the error detail of a job whose process or unit ended
// without recording a result: it may have run any part of its work, so a
// caller that handed secrets to the job has to treat them as possibly used.
const JobOutcomeUnknown = "job_outcome_unknown"

// JobObservationFailed is the error detail of a job.follow whose job state
// could not be read. The job may still be running; a copy that knows its job
// ID must retain its one-time credential handoff.
const JobObservationFailed = "job_observation_failed"

// jobObservationFailure is the sanitized RPC error for a job.follow whose
// state could not be observed. It names the known job ID but carries no
// underlying diagnostics.
func jobObservationFailure(id string) error {
	return output.NewError(output.CodeError, "the job's state could not be read", output.ExitGeneric,
		"The job may still be running; check vaire job follow "+id+" and vaire job list.").
		WithDetails(map[string]any{
			"reason":             JobObservationFailed,
			"job_id":             id,
			"observation_failed": true,
		})
}

// crashedJobRecord is the terminal record of a job whose unit ended without
// recording a result. It builds the JobOutcomeUnknown body first and finishes
// from a structured error carrying that body: a plain error would make
// finishedJobRecord rebuild the body and drop the detail, and with it the
// evidence that an unfollow may have installed its passwords.
func crashedJobRecord(rec engine.JobRecord) engine.JobRecord {
	body := output.ErrorBody{Code: output.CodeError, Message: "job " + rec.ID + " ended without recording a result",
		Hint: "Read journalctl -u " + rec.Unit + " on the node.", Details: map[string]any{"reason": JobOutcomeUnknown}}
	return finishedJobRecord(rec, engine.JobFailed, "ended without a result", nil, &output.CLIError{
		Code: body.Code, Message: body.Message, Hint: body.Hint, Details: body.Details,
	})
}

// putCrashedJobIfUnfinished writes a synthetic crash record only when the
// current record is still the same non-terminal attempt. A worker's terminal
// record always wins.
var putCrashedJobState = engine.UpdateState

func putCrashedJobIfUnfinished(rec engine.JobRecord) (engine.JobRecord, bool) {
	current, updated := rec, false
	err := putCrashedJobState(func(st *engine.State) error {
		cur, ok := st.Job(rec.ID)
		if !ok {
			return nil
		}
		current = cur
		if cur.Final() || !sameJobAttempt(cur, rec) {
			return nil
		}
		crashed := crashedJobRecord(cur)
		st.PutJob(crashed)
		current, updated = crashed, true
		return nil
	})
	if err != nil {
		return rec, false
	}
	return current, updated
}

func sameJobAttempt(current, expected engine.JobRecord) bool {
	if current.ID != expected.ID || current.Job != expected.Job || current.Unit != expected.Unit || current.State != expected.State {
		return false
	}
	if expected.StartedAt != "" && current.StartedAt != expected.StartedAt {
		return false
	}
	if expected.PID != 0 && current.PID != expected.PID {
		return false
	}
	return true
}

// finishedJobRecord is finishJob's pure part: it builds the terminal record
// without writing state, so callers can test refusal bookkeeping safely.
func finishedJobRecord(rec engine.JobRecord, state, detail string, res any, cause error) engine.JobRecord {
	rec.State, rec.Detail, rec.FinishedAt = state, detail, stamp()
	if res != nil {
		if raw, err := json.Marshal(secrets.RedactValue(res)); err == nil {
			rec.Result = raw
		}
	}
	if cause != nil {
		failure := output.Fail(nil, cause)
		body := failure.Error
		body.Message = secrets.Redact(body.Message)
		rec.Error = &body
	}
	return rec
}

// activityRefusedJobRecord is the terminal record of an already-created RPC
// job whose checked activity start failed. A timer job has no queued record
// yet, so existing is false and nothing is written.
func activityRefusedJobRecord(existing bool, id, job, db string, cause error) (engine.JobRecord, bool) {
	if !existing || id == "" {
		return engine.JobRecord{}, false
	}
	rec := finishedJobRecord(engine.JobRecord{ID: id, Job: job, DB: db},
		engine.JobFailed, "activity history unavailable", nil, cause)
	return rec, true
}

// putFinishedJob writes a terminal record, keeping the timestamps and unit
// the starting RPC recorded.
func putFinishedJob(rec engine.JobRecord) {
	_ = engine.UpdateState(func(st *engine.State) error {
		if old, ok := st.Job(rec.ID); ok {
			rec.CreatedAt, rec.Unit = old.CreatedAt, old.Unit
		}
		st.PutJob(rec)
		return nil
	})
}

func stamp() string { return time.Now().UTC().Format(time.RFC3339) }

// readJobSpec reads and deletes JobDir/<id>.json, registering its secrets.
func readJobSpec(id string) (jobSpec, error) {
	return readJobSpecAt(JobDir, id, false)
}

// readCopyJobSpec consumes a copy job's private spec with a fail-closed
// unlink. Decoded secrets are registered before any unlink diagnostic is
// built, so a retained spec's values remain redacted.
func readCopyJobSpec(id string) (jobSpec, error) {
	return readJobSpecAt(copyJobDir, id, true)
}

func readJobSpecAt(dir, id string, requireUnlink bool) (jobSpec, error) {
	if !validJobID(id) {
		return jobSpec{}, output.NewError(output.CodeUsage, "job id "+id+" is not a ULID", output.ExitUsage, "")
	}
	path := filepath.Join(dir, id+".json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a validated id under the fixed job directory.
	if err != nil {
		return jobSpec{}, output.NewError(output.CodePrecondition, "job "+id+" has no spec", output.ExitPrecondition,
			"Start jobs from their operator command.").WithDetails(map[string]any{"reason": "job_not_found", "job_id": id})
	}
	var spec jobSpec
	if !requireUnlink {
		_ = os.Remove(path)
		if err := json.Unmarshal(raw, &spec); err != nil {
			return jobSpec{}, err
		}
		registerJobSpecSecrets(spec)
		return spec, nil
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		// Even a partially decoded spec may already have exposed a secret in
		// the prior process; register what is available before diagnostics.
		registerJobSpecSecrets(spec)
		removeErr := os.Remove(path)
		return spec, copySpecInvalidError(id, removeErr)
	}
	registerJobSpecSecrets(spec)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return spec, copySpecUnlinkError(id)
	}
	return spec, nil
}

func registerJobSpecSecrets(spec jobSpec) {
	for _, v := range spec.Secrets {
		secrets.Add(v)
	}
}

func copySpecUnlinkError(id string) error {
	return output.NewError(output.CodeSecrets,
		"the copy job's private spec could not be removed", output.ExitSecrets,
		"Check /var/lib/vaire/jobs permissions as root; no copy work was started. The retained spec may contain credentials.").
		WithDetails(map[string]any{"reason": "copy_job_spec_unlink_failed", "job_id": id})
}

func jobSpecErrorReason(err error) string {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return ""
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

func copySpecInvalidError(id string, cleanup error) error {
	details := map[string]any{"reason": "copy_job_spec_invalid", "job_id": id}
	if cleanup != nil && !errors.Is(cleanup, fs.ErrNotExist) {
		details["spec_cleanup_failed"] = true
	}
	return output.NewError(output.CodeSecrets,
		"the copy job's private spec is invalid", output.ExitSecrets,
		"No copy work was started; inspect the private spec as root if it remains.").
		WithDetails(details)
}

func validJobID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789ABCDEFGHJKMNPQRSTVWXYZ", r) {
			return false
		}
	}
	return true
}

// jobUnit is the transient unit of a detachable job.
func jobUnit(id string) string { return "vaire-job-" + id + ".service" }

// startJob writes the job's spec and starts it as a transient unit
// (systemd-run), so it outlives the RPC: Ctrl-C or a dropped tunnel detaches
// without stopping the work. The job ID is the request ID.
func startJob(ctx context.Context, env *Env, job string, params any, jobSecrets map[string]string) (string, error) {
	id := env.Req.RequestID
	operationID := env.Req.OperationID
	if operationID == "" {
		operationID = id
	}
	spec := jobSpec{Job: job, Secrets: jobSecrets, OperationID: operationID, ParentRequestID: env.Req.RequestID}
	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return "", err
		}
		spec.Params = raw
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(JobDir, 0o700); err != nil {
		return "", err
	}
	if err := engine.WriteFileAtomic(filepath.Join(JobDir, id+".json"), raw, 0o600); err != nil {
		return "", err
	}
	unit := jobUnit(id)
	if err := engine.UpdateState(func(st *engine.State) error {
		st.PutJob(engine.JobRecord{ID: id, Job: job, DB: rpcJobDatabase(job, spec.Params), Unit: unit, State: engine.JobQueued, CreatedAt: stamp()})
		return nil
	}); err != nil {
		return "", err
	}
	if err := launchJobUnit(ctx, env, unit, job, id); err != nil {
		_ = os.Remove(filepath.Join(JobDir, id+".json"))
		finishJob(engine.JobRecord{ID: id, Job: job, Unit: unit}, engine.JobFailed, "systemd-run failed", nil, err)
		return "", output.WrapError(output.CodeExternalCommand, fmt.Errorf("starting the job unit: %w", err), output.ExitExternal, "")
	}
	env.Report(output.Event{Type: "progress", Phase: "job", Detail: id, Message: "job " + id + " started as " + unit})
	return id, nil
}

// launchJobUnit invokes systemd-run for a transient vaire job unit. It is
// shared by ordinary jobs and the copy-specific ambiguous-start path; callers
// decide what a failure means.
func launchJobUnit(ctx context.Context, env *Env, unit, job, id string) error {
	sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
	return sh.Exec(ctx, "job", engine.Command{Name: "systemd-run", Args: []string{
		"--unit=" + strings.TrimSuffix(unit, ".service"), "--description=vaire " + job, "--collect",
		"--property=After=postgresql.service", "--property=Nice=10",
		BinaryPath, "node", "run", job, "--job-id", id, "--output", "json",
	}}, env.Report)
}

// loadJobState reads the node's job records. It is a seam so a test can
// follow a job without a node state file.
var loadJobState = func() (engine.State, error) { return engine.LoadState(engine.StatePath) }

// jobFollowPoll is how often a follower re-reads an unfinished job's record.
// It is a seam so a test can exercise a state-observation failure without
// waiting for the production interval.
var jobFollowPoll = time.Second

// jobObservationError marks a failure to read a started job's state, as
// opposed to a terminal job outcome. A caller that has launched a job must
// treat this as an unknown outcome, not as proof that the job failed.
type jobObservationError struct{ err error }

func (e *jobObservationError) Error() string { return e.err.Error() }
func (e *jobObservationError) Unwrap() error { return e.err }

func jobObservation(err error) error {
	if err == nil {
		return nil
	}
	var observed *jobObservationError
	if errors.As(err, &observed) {
		return err
	}
	return &jobObservationError{err: err}
}

// finalJobRecord re-reads id only to see whether the job has already recorded
// a terminal outcome. The observer cancellation boundary uses it so a real
// terminal cancellation is not rewritten as an unobserved running job.
func finalJobRecord(id string) (engine.JobRecord, bool) {
	st, err := loadJobState()
	if err != nil {
		return engine.JobRecord{}, false
	}
	rec, ok := st.Job(id)
	if !ok || !rec.Final() {
		return engine.JobRecord{}, false
	}
	return rec, true
}

// jobEvidenceWait bounds how long a follower waits for the final activity
// record after the terminal state is published. It exceeds one bounded
// activity write, so only a job that died before settling stays pending.
var jobEvidenceWait = 5 * time.Second

// jobEvidencePoll is how often a follower re-reads a pending terminal record.
const jobEvidencePoll = 50 * time.Millisecond

// waitJobEvidence waits until the job's completion evidence is settled. It
// reports unsettled when the record still waits after the bound: that is
// missing evidence, not a clean result.
func waitJobEvidence(ctx context.Context, rec engine.JobRecord) (engine.JobRecord, bool) {
	if !rec.ActivityPending || rec.ID == "" {
		return rec, false
	}
	deadline := time.NewTimer(jobEvidenceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(jobEvidencePoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return rec, rec.ActivityPending
		case <-deadline.C:
			return rec, rec.ActivityPending
		case <-ticker.C:
		}
		st, err := loadJobState()
		if err != nil {
			continue
		}
		if next, ok := st.Job(rec.ID); ok {
			rec = next
		}
		if !rec.ActivityPending {
			return rec, false
		}
	}
}

// followJob streams a job's events from the journal until the job is
// recorded as finished, then returns its outcome as the RPC result.
func followJob(ctx context.Context, env *Env, id string) (Result, error) {
	st, err := loadJobState()
	if err != nil {
		return Result{}, jobObservation(err)
	}
	rec, ok := st.Job(id)
	if !ok {
		return Result{}, output.NewError(output.CodePrecondition, "there is no job "+id, output.ExitPrecondition,
			"List the jobs with vaire job list.").WithDetails(map[string]any{"reason": "job_not_found", "job_id": id})
	}
	args := []string{"-o", "cat", "--no-pager", "-n", "all"}
	switch {
	case rec.Unit != "":
		args = append(args, "-u", rec.Unit)
	case rec.InvocationID != "":
		args = append(args, "_SYSTEMD_INVOCATION_ID="+rec.InvocationID)
	default:
		args = nil
	}
	if !rec.Final() && args != nil {
		args = append(args, "-f")
	}
	jctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	var warnings jobWarningCollector
	if args != nil {
		go func() {
			defer close(done)
			streamJournal(jctx, args, warnings.reporter(env.Report))
		}()
	} else {
		close(done)
	}
	inactiveSince := time.Time{}
	for !rec.Final() {
		select {
		case <-ctx.Done():
			// The observer was interrupted while the job may still be
			// running. A terminal record that already exists is the real
			// outcome and must stay definite; otherwise this is an
			// observation failure, not a job failure.
			if final, ok := finalJobRecord(id); ok {
				rec = final
				continue
			}
			return Result{}, jobObservation(ctx.Err())
		case <-time.After(jobFollowPoll):
		}
		st, err := loadJobState()
		if err != nil {
			return Result{}, jobObservation(err)
		}
		if r, ok := st.Job(id); ok {
			rec = r
		}
		if rec.Final() {
			continue
		}
		if rec.Job == "db-copy" {
			switch copyJobLiveness(ctx, rec) {
			case copyLivenessActive, copyLivenessUnknown:
				inactiveSince = time.Time{}
			case copyLivenessInactive:
				if inactiveSince.IsZero() {
					inactiveSince = time.Now()
				} else {
					grace := 15 * time.Second
					if rec.State == engine.JobQueued {
						grace *= 4
					}
					if time.Since(inactiveSince) > grace {
						next, updated := putCrashedJobIfUnfinished(rec)
						rec = next
						if !updated {
							inactiveSince = time.Time{}
						}
					}
				}
			}
			continue
		}
		if rec.Unit == "" {
			// A timer run or a run by hand: its PID or invocation tells.
			if reapJobs(ctx, id) == nil {
				if st, err := loadJobState(); err == nil {
					if r, ok := st.Job(id); ok {
						rec = r
					}
				}
			}
			continue
		}
		// A unit that ended without recording a result crashed.
		out, _ := exec.CommandContext(ctx, "systemctl", "is-active", rec.Unit).Output() //nolint:gosec // G204: the unit name is vaire-job-<validated id>.
		if state := strings.TrimSpace(string(out)); state != "active" && state != "activating" {
			if inactiveSince.IsZero() {
				inactiveSince = time.Now()
			} else if grace := 15 * time.Second; rec.State == engine.JobQueued && time.Since(inactiveSince) > 4*grace || rec.State != engine.JobQueued && time.Since(inactiveSince) > grace {
				rec = crashedJobRecord(rec)
				putFinishedJob(rec)
			}
		}
	}
	// Wait for the final activity write before reading the result: the job
	// publishes its terminal state first, and a follower that stopped on that
	// state alone could miss the evidence warning carried with it.
	rec, unsettled := waitJobEvidence(ctx, rec)
	if unsettled {
		rec.Warnings = mergeJobWarnings(rec.Warnings, []output.Warning{activityWarning()})
	}
	// Let journalctl catch up: with -f it never ends, so give it a moment;
	// without -f (the job had finished already) it ends once it has
	// replayed the log, so give it longer before cutting it off.
	catchUp := 30 * time.Second
	if slices.Contains(args, "-f") {
		catchUp = 2 * time.Second
	}
	if args != nil {
		select {
		case <-done:
		case <-ctx.Done():
		case <-time.After(catchUp):
		}
	}
	cancel()
	// The job is over: never let the journal reader hold the reply.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
	for _, warning := range cleanupCopyJobSpecs(cleanupCtx, engine.State{Jobs: []engine.JobRecord{rec}}) {
		env.Report(output.Event{Type: "warning", Code: warning.Code, Message: warning.Message})
	}
	cleanupCancel()
	res, err := jobOutcome(rec)
	all := mergeJobWarnings(rec.Warnings, warnings.list())
	if err != nil {
		return res, attachJobWarnings(err, all)
	}
	res.Warnings = append(res.Warnings, all...)
	return res, nil
}

// attachJobWarnings carries evidence warnings on a failed job result too, so
// a structured failure does not hide that its evidence is incomplete. The
// warnings stay typed first-class notices; error details are left untouched.
func attachJobWarnings(err error, warnings []output.Warning) error {
	if len(warnings) == 0 {
		return err
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return err
	}
	copied := *cliErr
	copied.Warnings = mergeJobWarnings(cliErr.Warnings, warnings)
	return &copied
}

// jobOutcome turns a finished job record into the RPC result.
func jobOutcome(rec engine.JobRecord) (Result, error) {
	switch rec.State {
	case engine.JobSucceeded:
		return Result{Data: map[string]any{"job_id": rec.ID, "job": rec.Job, "state": rec.State, "result": rec.Result}}, nil
	case engine.JobSkipped:
		return Result{}, output.NewError(output.CodePrecondition, "job "+rec.ID+" was skipped: "+rec.Detail, output.ExitPrecondition,
			"Another vaire operation or backup holds the lock; retry when it has finished.").
			WithDetails(map[string]any{"reason": "lock_held", "job_id": rec.ID, "result": rec.Result})
	}
	body := output.ErrorBody{Code: output.CodeError, Message: "job " + rec.ID + " failed"}
	if rec.Error != nil {
		body = *rec.Error
	}
	return Result{}, &output.CLIError{
		Code:     body.Code,
		Message:  body.Message,
		Hint:     body.Hint,
		ExitCode: output.ExitFor(body.Code),
		Details:  jobErrorDetails(rec, body.Details),
	}
}

// jobErrorDetails are a failed job's error details with its id and name, at
// the top level as the job's own command shows them (details.output_tail,
// details.step); details that aren't an object stay under error_details.
func jobErrorDetails(rec engine.JobRecord, details any) map[string]any {
	out := map[string]any{}
	switch d := details.(type) {
	case map[string]any:
		for k, v := range d {
			out[k] = v
		}
	case nil:
	default:
		out["error_details"] = d
	}
	out["job_id"], out["job"] = rec.ID, rec.Job
	return out
}

// streamJournal runs journalctl and re-emits the job's events: lines that
// are vaire events pass through, other lines become log events.
func streamJournal(ctx context.Context, args []string, report engine.Reporter) {
	cmd := exec.CommandContext(ctx, "journalctl", args...) //nolint:gosec // G204: fixed journalctl with a validated unit or invocation id.
	cmd.WaitDelay = time.Second
	out, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err := cmd.Start(); err != nil {
		return
	}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	var f journalFilter
	for sc.Scan() {
		if ev, ok := f.event(sc.Text()); ok {
			report(ev)
		}
	}
	_ = cmd.Wait()
}

// journalFilter turns a job's journal lines into the events a follower
// sees. vaire node run --output json writes one event per line and, at the
// end, its result envelope indented over many lines: the envelope is
// dropped (the job record carries the result, and the follower prints its
// result line from that), as are result events and journalNoise.
type journalFilter struct {
	inEnvelope bool
}

// event is the event for one journal line, or false when the line is
// dropped.
func (f *journalFilter) event(line string) (output.Event, bool) {
	if f.inEnvelope {
		// The envelope's closing brace is the only unindented line in it.
		if line == "}" {
			f.inEnvelope = false
		}
		return output.Event{}, false
	}
	if line == "{" {
		f.inEnvelope = true
		return output.Event{}, false
	}
	var ev output.Event
	if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &ev) == nil && ev.Schema == output.EventSchema {
		if ev.Type == "result" || ev.Type == "" {
			return output.Event{}, false
		}
		// Keep the job's own timestamp: replay time must not masquerade as
		// occurrence time in activity or the follower's output.
		return ev, true
	}
	if strings.HasPrefix(line, "{") && strings.Contains(line, `"schema":"vaire/v1"`) {
		// A compact final envelope.
		return output.Event{}, false
	}
	if journalNoise(line) {
		return output.Event{}, false
	}
	return output.Event{Type: "log", Step: "job", Line: line}, true
}

// journalNoise reports whether a journal line is not the job's own: the
// pam_unix session lines runuser writes each time a job runs a command as
// postgres, and the SysV sync lines systemctl enable writes for a unit
// with an init script (pgbouncer, nginx) when run without --quiet.
func journalNoise(line string) bool {
	return strings.Contains(line, "pam_unix(runuser:session): session opened") ||
		strings.Contains(line, "pam_unix(runuser:session): session closed") ||
		systemdSysvNoise(line)
}

// systemdSysvNoise reports whether line is one of systemctl enable's SysV
// compatibility messages.
func systemdSysvNoise(line string) bool {
	return strings.Contains(line, "with SysV service script with") ||
		strings.Contains(line, "Executing: /usr/lib/systemd/systemd-sysv-install") ||
		strings.Contains(line, "Executing: /lib/systemd/systemd-sysv-install")
}

type jobFollowParams struct {
	ID string `json:"id"`
}

func handleJobFollow(ctx context.Context, env *Env) (Result, error) {
	var p jobFollowParams
	if err := decodeParams(env.Req.Params, &p); err != nil {
		return Result{}, err
	}
	id := strings.ToUpper(p.ID)
	if !validJobID(id) {
		return Result{}, output.NewError(output.CodeUsage, "job id "+p.ID+" is not a job id", output.ExitUsage, "List the jobs with vaire job list.")
	}
	res, err := followJob(ctx, env, id)
	var observed *jobObservationError
	if errors.As(err, &observed) {
		return Result{}, jobObservationFailure(id)
	}
	return res, err
}

func handleJobList(ctx context.Context, _ *Env) (Result, error) {
	_ = reapJobs(ctx, "")
	warnings := pruneCopyWorkspaces(ctx)
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	jobs := make([]engine.JobRecord, 0, len(st.Jobs))
	for i := len(st.Jobs) - 1; i >= 0; i-- {
		jobs = append(jobs, st.Jobs[i])
	}
	return Result{Data: map[string]any{"jobs": jobs}, Warnings: warnings}, nil
}

// queuedGrace is how long a queued job's unit may be missing: the RPC
// records the job before systemd-run starts its unit.
const queuedGrace = time.Minute

// reapJobs records as failed each unfinished job (only id, when given) whose
// process or unit is gone, such as one stopped with systemctl stop: it would
// otherwise show as running forever. The check runs under the state lock, and
// a job records its outcome before its process exits, so a job that is
// finishing is never reaped.
func reapJobs(ctx context.Context, id string) error {
	return engine.UpdateState(func(st *engine.State) error {
		for i, rec := range st.Jobs {
			if rec.Final() || id != "" && rec.ID != id {
				continue
			}
			gone := false
			if rec.Job == "db-copy" {
				gone = copyWorkspaceInactive(ctx, rec)
			} else {
				gone = jobGone(ctx, rec)
			}
			if !gone {
				continue
			}
			rec.State, rec.Detail, rec.FinishedAt = engine.JobFailed, "ended without a result", stamp()
			rec.Error = &output.ErrorBody{Code: output.CodeError, Message: "job " + rec.ID + " ended without recording a result", Hint: jobLogHint(rec),
				Details: map[string]any{"reason": JobOutcomeUnknown}}
			st.Jobs[i] = rec
		}
		return nil
	})
}

// jobGone reports whether an unfinished job's process or unit has ended: its
// recorded PID no longer runs vaire node run <job>, or its unit (the RPC's
// transient unit, or the timer's service for this invocation) isn't active.
func jobGone(ctx context.Context, rec engine.JobRecord) bool {
	if rec.PID > 0 {
		return !jobProcessAlive(rec.PID, rec.Job)
	}
	unit, invocation := rec.Unit, ""
	if unit == "" && rec.InvocationID != "" {
		unit, invocation = timerUnit(rec), rec.InvocationID
	}
	if unit == "" {
		return false
	}
	if rec.State == engine.JobQueued {
		created, err := time.Parse(time.RFC3339, rec.CreatedAt)
		if err != nil || time.Since(created) < queuedGrace {
			return false
		}
	}
	active, current, ok := unitState(ctx, unit)
	if !ok {
		return false
	}
	return !active || invocation != "" && current != invocation
}

// jobProcessAlive reports whether pid runs vaire node run job. Where /proc
// can't be read it trusts the PID alone.
func jobProcessAlive(pid int, job string) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	cmdline, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return !errors.Is(err, os.ErrNotExist)
	}
	return strings.Contains(string(cmdline), "\x00node\x00run\x00"+job+"\x00") || strings.HasSuffix(string(cmdline), "\x00node\x00run\x00"+job)
}

// timerUnit is the service a timer run of rec's job runs in.
func timerUnit(rec engine.JobRecord) string {
	switch {
	case databaseJobs[rec.Job] && rec.DB != "":
		return "vaire-" + rec.Job + "@" + rec.DB + ".service"
	case rec.Job == "stanza-expiry":
		return "vaire-stanza-expiry.service"
	}
	return ""
}

// unitState is whether unit is active (or activating) and its current
// invocation id. ok is false when systemctl can't be asked.
func unitState(ctx context.Context, unit string) (active bool, invocation string, ok bool) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState,InvocationID", unit).Output() //nolint:gosec // G204: an vaire unit name built from a validated id or a configured database.
	if err != nil {
		return false, "", false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "ActiveState":
			active = value == "active" || value == "activating" || value == "reloading"
		case "InvocationID":
			invocation = value
		}
	}
	return active, invocation, true
}

// jobLogHint says where an unfinished job's log is.
func jobLogHint(rec engine.JobRecord) string {
	switch {
	case rec.Unit != "":
		return "Read journalctl -u " + rec.Unit + " on the node."
	case rec.InvocationID != "":
		return "Read journalctl _SYSTEMD_INVOCATION_ID=" + rec.InvocationID + " on the node."
	}
	return ""
}

// stepSecrets are the request secrets a job's steps read (steps.StepSecrets)
// out of req, for the spec of a job that runs steps. Only the request that
// starts a job carries secrets: without them in its spec, a step that needs
// one the node lacks fails with input_required (postgres-roles-v1 on a
// rebuilt master, which has no service password files yet).
func stepSecrets(req map[string]string) map[string]string {
	out := map[string]string{}
	for _, key := range steps.StepSecrets {
		if v := req[key]; v != "" {
			out[key] = v
		}
	}
	return out
}

// runJobRPC starts job as a detachable unit and follows it.
func runJobRPC(ctx context.Context, env *Env, job string, params any, jobSecrets map[string]string) (Result, error) {
	id, err := startJob(ctx, env, job, params, jobSecrets)
	if err != nil {
		return Result{}, err
	}
	return followJob(ctx, env, id)
}

// jobDatabase is the database a template-unit job runs for: --db, else the
// RPC's params.database.
func jobDatabase(env *jobEnv) string {
	if env.DB != "" {
		return env.DB
	}
	var p struct {
		Database string `json:"database"`
	}
	if len(env.Params) > 0 {
		_ = json.Unmarshal(env.Params, &p)
	}
	return p.Database
}

// rpcJobDatabase is the database job list shows for an RPC-only job: a
// restore's database, or the new database of a fork (its source is in the
// params).
func rpcJobDatabase(job string, params json.RawMessage) string {
	var p struct {
		Database string `json:"database"`
		Name     string `json:"name"`
		DB       string `json:"db"`
	}
	if len(params) > 0 {
		_ = json.Unmarshal(params, &p)
	}
	switch job {
	case "restore", "recover":
		return p.Database
	case "db-fork", "db-follow", "db-unfollow", "db-copy":
		return p.Name
	case "maint":
		return p.DB
	}
	return ""
}

// jobClusters are the clusters a per-database job works in, in name order,
// which is the order their locks are taken.
func jobClusters(env *jobEnv) []string {
	var p struct {
		Database string `json:"database"`
		Source   string `json:"source"`
		Name     string `json:"name"`
		DB       string `json:"db"`
	}
	if len(env.Params) > 0 {
		_ = json.Unmarshal(env.Params, &p)
	}
	var names []string
	// A copy's target is not configured yet, but its cluster lock is taken
	// anyway: the name must be serialized against another copy and against
	// every later configuration writer before the job can create it.
	includeUnconfigured := false
	switch {
	case databaseJobs[env.Job]:
		names = []string{env.DB}
	case env.Job == "maint":
		names = []string{p.DB}
	case env.Job == "restore", env.Job == "recover":
		names = []string{p.Database}
	case env.Job == "db-fork", env.Job == "db-follow":
		names = []string{p.Source, p.Name}
	case env.Job == "db-unfollow":
		names = []string{p.Name}
	case env.Job == "db-copy":
		names = []string{p.Name}
		includeUnconfigured = true
	}
	var out []string
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, ok := env.Cluster.ClusterByName(name); ok || includeUnconfigured {
			if !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	slices.Sort(out)
	return out
}
