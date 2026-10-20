package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

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

// jobNames lists every job vaire node run accepts. restore, db-fork and
// dump-create run only as detachable jobs an RPC starts (--job-id).
var jobNames = []string{
	"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill", "db-dump",
	"stanza-expiry", "restore", "db-fork", "maint", "recover",
	"maint-daily", "maint-weekly", "certs-renew", "metrics-refresh", "doctor",
	"master-backup", "fence-check",
}

// bucketWriteJobs write to or delete from the bucket, so they check the
// epoch fence first (Phase 5, D5).
var bucketWriteJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-incr": true, "db-dump": true,
	"stanza-expiry": true, "db-fork": true, "restore": true, "recover": true,
}

// locklessJobs take no node lock (Phase 5): the master backup must never
// wait for a long apply, and the fence check must run at boot whatever
// holds the lock. The master backup has its own lock.
var locklessJobs = map[string]bool{"master-backup": true, "fence-check": true}

// rpcOnlyJobs need a spec written by the RPC that starts them.
var rpcOnlyJobs = map[string]bool{"restore": true, "db-fork": true, "maint": true, "recover": true}

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
}

// jobEnv is what a job function gets.
type jobEnv struct {
	ID      string
	Job     string
	Cluster config.Cluster
	Params  json.RawMessage
	Secrets map[string]string
	Report  engine.Reporter
	DB      string
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
	if !knownJob(job) {
		return nil, "", output.NewError(output.CodeUsage, "unknown job "+job, output.ExitUsage, "Use one of details.jobs.").
			WithDetails(map[string]any{"jobs": jobNames})
	}
	if _, err := os.Stat(config.ClusterPath); errors.Is(err, os.ErrNotExist) {
		return nil, "", output.NewError(
			output.CodePrecondition,
			"this machine is not an vaire node: "+config.ClusterPath+" does not exist",
			output.ExitPrecondition,
			"vaire node run runs on the server. From your machine: vaire ssh --command 'sudo vaire node run "+job+"'",
		).WithDetails(map[string]any{"reason": "not_a_node"})
	}
	if os.Geteuid() != 0 {
		return nil, "", rootRequired("vaire node run")
	}
	if job == "certs-renew" {
		release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), certsLockWait)
		if engine.IsLockHeld(err) {
			logLine(report, job, "the node lock is busy; skipping "+job)
			return map[string]any{"job": job, "skipped": "lock_busy"}, "the node lock was busy", nil
		}
		if err != nil {
			return nil, "", err
		}
		defer release()
		res, err := certsRenew(ctx, report)
		return res, "", err
	}
	if locklessJobs[job] {
		cfg, err := config.LoadCluster(config.ClusterPath)
		if err != nil {
			return nil, "", err
		}
		if job == "fence-check" {
			res, err := runFenceCheck(ctx, cfg, report)
			return res, "", err
		}
		res, err := runMasterBackup(ctx, cfg, report, masterBackupOptions{debounce: true})
		return res, "", err
	}
	if quietJobs[job] {
		release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), 0)
		if engine.IsLockHeld(err) {
			logLine(report, job, "the node lock is busy; skipping "+job)
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
		res, err := runQuietJob(ctx, job, cfg, report)
		return res, "", err
	}
	if rpcOnlyJobs[job] && opts.ID == "" {
		return nil, "", output.NewError(output.CodeUsage, "job "+job+" is started by its operator command, not by hand", output.ExitUsage,
			"Use vaire restore or vaire db fork from your machine.")
	}

	env := &jobEnv{ID: opts.ID, Job: job, Report: report, DB: opts.DB}
	if env.ID == "" {
		env.ID = rpc.NewRequestID()
	} else {
		spec, err := readJobSpec(env.ID)
		if err != nil {
			return nil, "", err
		}
		if spec.Job != job {
			return nil, "", output.NewError(output.CodeUsage, "job "+env.ID+" is a "+spec.Job+" job, not "+job, output.ExitUsage, "")
		}
		env.Params, env.Secrets = spec.Params, spec.Secrets
	}
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return nil, "", err
	}
	env.Cluster = cfg
	if databaseJobs[job] {
		env.DB = jobDatabase(env)
		if env.DB == "" {
			return nil, "", output.NewError(output.CodeUsage, "job "+job+" runs for one database", output.ExitUsage,
				"Pass --db <database>, as the vaire-"+job+"@<database> timer does.")
		}
		if _, ok := cfg.ClusterByName(env.DB); !ok {
			return nil, "", databaseNotFound(env.DB)
		}
	}

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
		detail := busy.what + " was busy"
		if err != nil {
			// systemctl stop while the run waited for the lock.
			detail = "stopped while waiting for " + busy.what
			res["skipped"] = "stopped"
		} else {
			logLine(report, job, busy.log+"; skipping "+job)
		}
		finishJob(rec, engine.JobSkipped, detail, res, nil)
		notRun(env, errors.New(detail))
		return res, detail, nil
	}
	if err != nil {
		finishJob(rec, engine.JobFailed, "", nil, err)
		return nil, "", err
	}
	defer release()

	if bucketWriteJobs[job] {
		if err := requireNotFenced(ctx, cfg, report); err != nil {
			finishJob(rec, engine.JobFailed, "", nil, err)
			return nil, "", err
		}
	}
	res, err = runJobFunc(ctx, env)
	if err != nil {
		if errors.Is(err, errSkipped) {
			finishJob(rec, engine.JobSkipped, err.Error(), res, nil)
			return res, strings.TrimPrefix(err.Error(), errSkipped.Error()+": "), nil
		}
		finishJob(rec, engine.JobFailed, "", nil, err)
		return nil, "", err
	}
	finishJob(rec, engine.JobSucceeded, "", res, nil)
	return res, "", nil
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

// tryJobLocks takes a job's locks once each, in order: the node lock
// (shared), the heavy-I/O lock for heavy jobs, then its clusters' locks. On a
// busy lock it releases what it took and returns which one was busy.
func tryJobLocks(ctx context.Context, env *jobEnv) (engine.Release, *jobLockBusy, error) {
	var held []engine.Release
	releaseAll := func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i]()
		}
	}
	release, err := engine.Acquire(ctx, engine.LockShared, env.Job, env.ID, 0)
	if engine.IsLockHeld(err) {
		return nil, &jobLockBusy{what: "the node lock", log: "the node lock is busy"}, nil
	}
	if err != nil {
		return nil, nil, err
	}
	held = append(held, release)
	if heavyIOJobs[env.Job] || maintHeavy(env) {
		release, err := engine.AcquireHeavyIO(ctx, env.Job, env.ID)
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
		release, err := engine.AcquireCluster(ctx, cluster, engine.LockShared, env.Job, env.ID, 0)
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
// recorded failed, so abort can clean it up, and a fork's cluster.toml entry
// is removed, as its cluster was never made.
func notRun(env *jobEnv, cause error) {
	switch env.Job {
	case "restore", "recover":
		failRestore(env, cause)
	case "db-fork":
		var p dbForkParams
		if json.Unmarshal(env.Params, &p) == nil && p.Name != "" {
			_ = forgetDatabase(p.Name)
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
	if !validJobID(id) {
		return jobSpec{}, output.NewError(output.CodeUsage, "job id "+id+" is not a ULID", output.ExitUsage, "")
	}
	path := filepath.Join(JobDir, id+".json")
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a validated id under the fixed job directory.
	if err != nil {
		return jobSpec{}, output.NewError(output.CodePrecondition, "job "+id+" has no spec", output.ExitPrecondition,
			"Start jobs from their operator command.").WithDetails(map[string]any{"reason": "job_not_found", "job_id": id})
	}
	_ = os.Remove(path)
	var spec jobSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return jobSpec{}, err
	}
	for _, v := range spec.Secrets {
		secrets.Add(v)
	}
	return spec, nil
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
	spec := jobSpec{Job: job, Secrets: jobSecrets}
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
	sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
	if err := sh.Exec(ctx, "job", engine.Command{Name: "systemd-run", Args: []string{
		"--unit=" + strings.TrimSuffix(unit, ".service"), "--description=vaire " + job, "--collect",
		"--property=After=postgresql.service", "--property=Nice=10",
		BinaryPath, "node", "run", job, "--job-id", id, "--output", "json",
	}}, env.Report); err != nil {
		_ = os.Remove(filepath.Join(JobDir, id+".json"))
		finishJob(engine.JobRecord{ID: id, Job: job, Unit: unit}, engine.JobFailed, "systemd-run failed", nil, err)
		return "", output.WrapError(output.CodeExternalCommand, fmt.Errorf("starting the job unit: %w", err), output.ExitExternal, "")
	}
	env.Report(output.Event{Type: "progress", Phase: "job", Detail: id, Message: "job " + id + " started as " + unit})
	return id, nil
}

// followJob streams a job's events from the journal until the job is
// recorded as finished, then returns its outcome as the RPC result.
func followJob(ctx context.Context, env *Env, id string) (Result, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
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
	if args != nil {
		go func() {
			defer close(done)
			streamJournal(jctx, args, env.Report)
		}()
	} else {
		close(done)
	}
	inactiveSince := time.Time{}
	for !rec.Final() {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(time.Second):
		}
		st, err := engine.LoadState(engine.StatePath)
		if err != nil {
			return Result{}, err
		}
		if r, ok := st.Job(id); ok {
			rec = r
		}
		if rec.Final() {
			continue
		}
		if rec.Unit == "" {
			// A timer run or a run by hand: its PID or invocation tells.
			if reapJobs(ctx, id) == nil {
				if st, err := engine.LoadState(engine.StatePath); err == nil {
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
				rec.State = engine.JobFailed
				rec.Error = &output.ErrorBody{Code: output.CodeError, Message: "job " + id + " ended without recording a result", Hint: "Read journalctl -u " + rec.Unit + " on the node."}
				finishJob(rec, engine.JobFailed, "ended without a result", nil, errors.New(rec.Error.Message))
			}
		}
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
	return jobOutcome(rec)
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
		ev.TS = ""
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
	if !validJobID(strings.ToUpper(p.ID)) {
		return Result{}, output.NewError(output.CodeUsage, "job id "+p.ID+" is not a job id", output.ExitUsage, "List the jobs with vaire job list.")
	}
	return followJob(ctx, env, strings.ToUpper(p.ID))
}

func handleJobList(ctx context.Context, _ *Env) (Result, error) {
	_ = reapJobs(ctx, "")
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return Result{}, err
	}
	jobs := make([]engine.JobRecord, 0, len(st.Jobs))
	for i := len(st.Jobs) - 1; i >= 0; i-- {
		jobs = append(jobs, st.Jobs[i])
	}
	return Result{Data: map[string]any{"jobs": jobs}}, nil
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
			if rec.Final() || id != "" && rec.ID != id || !jobGone(ctx, rec) {
				continue
			}
			rec.State, rec.Detail, rec.FinishedAt = engine.JobFailed, "ended without a result", stamp()
			rec.Error = &output.ErrorBody{Code: output.CodeError, Message: "job " + rec.ID + " ended without recording a result", Hint: jobLogHint(rec)}
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
	case "db-fork":
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
	switch {
	case databaseJobs[env.Job]:
		names = []string{env.DB}
	case env.Job == "maint":
		names = []string{p.DB}
	case env.Job == "restore", env.Job == "recover":
		names = []string{p.Database}
	case env.Job == "db-fork":
		names = []string{p.Source, p.Name}
	}
	var out []string
	for _, name := range names {
		if _, ok := env.Cluster.ClusterByName(name); ok && !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}
