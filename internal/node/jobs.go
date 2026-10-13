package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/rpc"
	"github.com/mbvlabs/eru/internal/secrets"
)

// JobDir holds the specs of detachable jobs between the RPC that starts one
// and the unit that runs it. The secrets in a spec are read once and the file
// is deleted before the job does anything else.
const JobDir = "/var/lib/eru/jobs"

// jobNames lists every job eru node run accepts. restore, db-fork and
// dump-create run only as detachable jobs an RPC starts (--job-id).
var jobNames = []string{
	"backup-full", "backup-diff", "backup-incr", "backup-verify", "restore-drill", "db-dump",
	"stanza-expiry", "restore", "db-fork",
	"vps-backup", "maint-daily", "maint-weekly", "certs-renew", "metrics-refresh",
}

// laterJobs are the jobs of later phases.
var laterJobs = map[string]bool{"vps-backup": true, "maint-daily": true, "maint-weekly": true, "metrics-refresh": true}

// rpcOnlyJobs need a spec written by the RPC that starts them.
var rpcOnlyJobs = map[string]bool{"restore": true, "db-fork": true}

// databaseJobs are the template-unit jobs: each runs for one database, named
// by --db on a timer run (eru-<job>@<db>) or by the RPC's params.
var databaseJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-incr": true, "backup-verify": true, "restore-drill": true, "db-dump": true,
}

// heavyIOJobs take the server's heavy-I/O lock (RFC 9.3), so at most one of
// them reads the disk and pushes to the bucket at a time. Incrementals are
// small and frequent; they only take their cluster's lock.
var heavyIOJobs = map[string]bool{
	"backup-full": true, "backup-diff": true, "backup-verify": true, "restore-drill": true, "db-dump": true,
}

// JobOptions are eru node run's flags.
type JobOptions struct {
	// ID names a detachable job whose spec an RPC wrote.
	ID string
	// DB is the database of a template-unit timer run (eru-<job>@<db>).
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

// RunJob runs one job of eru node run: a timer run or a detachable job an RPC
// started. It records the job in state.json, takes the node lock (shared,
// timeout 0: a busy lock records skipped and exits 0; certs-renew waits up to
// 20 minutes), the heavy-I/O lock for heavy backup jobs and the clusters'
// locks (both timeout 0, skipped when busy), runs it and records the outcome.
func RunJob(ctx context.Context, job string, opts JobOptions, report engine.Reporter) (any, error) {
	if !knownJob(job) {
		return nil, output.NewError(output.CodeUsage, "unknown job "+job, output.ExitUsage, "Use one of details.jobs.").
			WithDetails(map[string]any{"jobs": jobNames})
	}
	if laterJobs[job] {
		return nil, output.NewError(output.CodeError, "job "+job+" is not implemented yet", output.ExitGeneric,
			"This job arrives in a later phase of the roadmap.")
	}
	if _, err := os.Stat(config.ClusterPath); errors.Is(err, os.ErrNotExist) {
		return nil, output.NewError(
			output.CodePrecondition,
			"this machine is not an eru node: "+config.ClusterPath+" does not exist",
			output.ExitPrecondition,
			"eru node run runs on the server. From your machine: eru ssh --command 'sudo eru node run "+job+"'",
		).WithDetails(map[string]any{"reason": "not_a_node"})
	}
	if os.Geteuid() != 0 {
		return nil, rootRequired("eru node run")
	}
	if job == "certs-renew" {
		release, err := engine.Acquire(ctx, engine.LockShared, job, rpc.NewRequestID(), certsLockWait)
		if engine.IsLockHeld(err) {
			logLine(report, job, "the node lock is busy; skipping "+job)
			return map[string]any{"job": job, "skipped": "lock_busy"}, nil
		}
		if err != nil {
			return nil, err
		}
		defer release()
		return certsRenew(ctx, report)
	}
	if rpcOnlyJobs[job] && opts.ID == "" {
		return nil, output.NewError(output.CodeUsage, "job "+job+" is started by its operator command, not by hand", output.ExitUsage,
			"Use eru restore or eru db fork from your machine.")
	}

	env := &jobEnv{ID: opts.ID, Job: job, Report: report, DB: opts.DB}
	if env.ID == "" {
		env.ID = rpc.NewRequestID()
	} else {
		spec, err := readJobSpec(env.ID)
		if err != nil {
			return nil, err
		}
		if spec.Job != job {
			return nil, output.NewError(output.CodeUsage, "job "+env.ID+" is a "+spec.Job+" job, not "+job, output.ExitUsage, "")
		}
		env.Params, env.Secrets = spec.Params, spec.Secrets
	}
	cfg, err := config.LoadCluster(config.ClusterPath)
	if err != nil {
		return nil, err
	}
	env.Cluster = cfg
	if databaseJobs[job] {
		env.DB = jobDatabase(env)
		if env.DB == "" {
			return nil, output.NewError(output.CodeUsage, "job "+job+" runs for one database", output.ExitUsage,
				"Pass --db <database>, as the eru-"+job+"@<database> timer does.")
		}
		if _, ok := cfg.ClusterByName(env.DB); !ok {
			return nil, databaseNotFound(env.DB)
		}
	}

	rec := engine.JobRecord{ID: env.ID, Job: job, DB: env.DB, State: engine.JobRunning, StartedAt: stamp(), InvocationID: os.Getenv("INVOCATION_ID")}
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

	release, err := engine.Acquire(ctx, engine.LockShared, job, env.ID, 0)
	if engine.IsLockHeld(err) {
		logLine(report, job, "the node lock is busy; skipping "+job)
		res := map[string]any{"job": job, "job_id": env.ID, "skipped": "lock_busy"}
		finishJob(rec, engine.JobSkipped, "the node lock was busy", res, nil)
		notRun(env, errors.New("the node lock was busy"))
		return res, nil
	}
	if err != nil {
		finishJob(rec, engine.JobFailed, "", nil, err)
		return nil, err
	}
	defer release()
	if heavyIOJobs[job] {
		release, err := engine.AcquireHeavyIO(ctx, job, env.ID)
		if engine.IsLockHeld(err) {
			logLine(report, job, "another backup job holds the heavy-I/O lock; skipping "+job)
			res := map[string]any{"job": job, "job_id": env.ID, "skipped": "lock_busy", "lock": "heavy_io", "db": env.DB}
			finishJob(rec, engine.JobSkipped, "the heavy-I/O lock was busy", res, nil)
			return res, nil
		}
		if err != nil {
			finishJob(rec, engine.JobFailed, "", nil, err)
			return nil, err
		}
		defer release()
	}
	for _, cluster := range jobClusters(env) {
		release, err := engine.AcquireCluster(ctx, cluster, engine.LockShared, job, env.ID, 0)
		if engine.IsLockHeld(err) {
			logLine(report, job, "the lock of "+cluster+" is busy; skipping "+job)
			res := map[string]any{"job": job, "job_id": env.ID, "skipped": "lock_busy", "cluster": cluster}
			finishJob(rec, engine.JobSkipped, "the lock of "+cluster+" was busy", res, nil)
			notRun(env, errors.New("the lock of "+cluster+" was busy"))
			return res, nil
		}
		if err != nil {
			finishJob(rec, engine.JobFailed, "", nil, err)
			return nil, err
		}
		defer release()
	}

	res, err := runJobFunc(ctx, env)
	if err != nil {
		if errors.Is(err, errSkipped) {
			finishJob(rec, engine.JobSkipped, err.Error(), res, nil)
			return res, nil
		}
		finishJob(rec, engine.JobFailed, "", nil, err)
		return nil, err
	}
	finishJob(rec, engine.JobSucceeded, "", res, nil)
	return res, nil
}

// notRun undoes what the RPC of a skipped job prepared: a restore is
// recorded failed, so abort can clean it up, and a fork's cluster.toml entry
// is removed, as its cluster was never made.
func notRun(env *jobEnv, cause error) {
	switch env.Job {
	case "restore":
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
	}
	return nil, fmt.Errorf("job %s has no implementation", env.Job)
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
func jobUnit(id string) string { return "eru-job-" + id + ".service" }

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
		st.PutJob(engine.JobRecord{ID: id, Job: job, Unit: unit, State: engine.JobQueued, CreatedAt: stamp()})
		return nil
	}); err != nil {
		return "", err
	}
	sh := engine.Shell{Secrets: secrets.Default, Log: env.Log}
	if err := sh.Exec(ctx, "job", engine.Command{Name: "systemd-run", Args: []string{
		"--unit=" + strings.TrimSuffix(unit, ".service"), "--description=eru " + job, "--collect",
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
			"List the jobs with eru job list.").WithDetails(map[string]any{"reason": "job_not_found", "job_id": id})
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
		if rec.Final() || rec.Unit == "" {
			continue
		}
		// A unit that ended without recording a result crashed.
		out, _ := exec.CommandContext(ctx, "systemctl", "is-active", rec.Unit).Output() //nolint:gosec // G204: the unit name is eru-job-<validated id>.
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
	if args != nil && slices.Contains(args, "-f") {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
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
			"Another eru operation or backup holds the lock; retry when it has finished.").
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
		Details:  map[string]any{"job_id": rec.ID, "job": rec.Job, "error_details": body.Details},
	}
}

// streamJournal runs journalctl and re-emits the job's events: lines that
// are eru events pass through, other lines become log events.
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
	for sc.Scan() {
		line := sc.Text()
		var ev output.Event
		if strings.HasPrefix(line, "{") && json.Unmarshal([]byte(line), &ev) == nil && ev.Schema == output.EventSchema {
			if ev.Type == "result" || ev.Type == "" {
				continue
			}
			ev.TS = ""
			report(ev)
			continue
		}
		if strings.HasPrefix(line, "{") && strings.Contains(line, `"schema":"eru/v1"`) {
			// The job's final envelope; the record carries the result.
			continue
		}
		report(output.Event{Type: "log", Step: "job", Line: line})
	}
	_ = cmd.Wait()
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
		return Result{}, output.NewError(output.CodeUsage, "job id "+p.ID+" is not a job id", output.ExitUsage, "List the jobs with eru job list.")
	}
	return followJob(ctx, env, strings.ToUpper(p.ID))
}

func handleJobList(context.Context, *Env) (Result, error) {
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

// jobClusters are the clusters a per-database job works in, in name order,
// which is the order their locks are taken.
func jobClusters(env *jobEnv) []string {
	var p struct {
		Database string `json:"database"`
		Source   string `json:"source"`
		Name     string `json:"name"`
	}
	if len(env.Params) > 0 {
		_ = json.Unmarshal(env.Params, &p)
	}
	var names []string
	switch {
	case databaseJobs[env.Job]:
		names = []string{env.DB}
	case env.Job == "restore":
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
