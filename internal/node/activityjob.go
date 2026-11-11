package node

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

// jobActivity is one job's activity life. It is created before any branch of
// RunJob so the deferred final recorder runs after every lock release and
// never holds an operation lock across an append.
type jobActivity struct {
	scope     *activityScope
	base      engine.Reporter
	important bool
	warned    bool
	finished  bool
	// recorded says RunJob published a terminal record for this scope, so the
	// finalizer must settle it. Jobs that never reach that point have no
	// record to update (and tests must not touch the node state file).
	recorded bool

	pendingOutcome string
	pendingCode    string
	pendingReason  string
}

func newJobActivity(rec *activityRecorder, job, id, operationID, parentRequestID, database string, timer, rpcStarted, important bool) *jobActivity {
	scope := newJobScope(rec, job, id, operationID, parentRequestID, database, timer)
	if !rpcStarted {
		scope.requestID = ""
	}
	return &jobActivity{scope: scope, important: important}
}

// begin records the job's started fact and returns the reporter steps should
// use. Important jobs fail closed when the start cannot be recorded; safety
// background jobs warn and stay available.
func (a *jobActivity) begin(ctx context.Context, report engine.Reporter) (engine.Reporter, error) {
	if a == nil || a.scope == nil {
		return report, nil
	}
	a.base = report
	var err error
	if a.important {
		err = a.scope.beginChecked(ctx)
	} else {
		err = a.scope.begin(ctx)
	}
	if err != nil {
		a.warn()
		if a.important {
			return report, activityUnavailableError()
		}
	}
	return a.scope.recorder(report), nil
}

// pending asks the deferred finalizer to record outcome instead of inferring
// it from the function's return values.
func (a *jobActivity) pending(outcome, code, reason string) {
	if a == nil {
		return
	}
	a.pendingOutcome, a.pendingCode, a.pendingReason = outcome, code, reason
}

// finish records the terminal fact once, after the job's locks are released.
func (a *jobActivity) finish(ctx context.Context, outcome, code, reason string) {
	if a == nil || a.finished {
		return
	}
	a.finished = true
	if a.scope == nil {
		return
	}
	if outcome == "" {
		outcome, code, reason = a.pendingOutcome, a.pendingCode, a.pendingReason
	}
	if outcome == "" {
		outcome = activity.OutcomeUnknown
	}
	if err := a.scope.finish(ctx, outcome, code, reason); err != nil {
		a.warn()
	}
}

// warn reports a lost history write through the job's own output, exactly
// once. The job's outcome and return value are unchanged and it is not
// retried.
func (a *jobActivity) warn() {
	if a == nil || a.warned || a.base == nil {
		return
	}
	a.warned = true
	a.base(activityWarningEvent())
}

// jobSafetyJob reports the jobs that must keep running when history cannot be
// written. Every detachable job and timer run is background work whose outcome
// is already durable in state.json: losing the diagnostic copy must not stop a
// backup, restore, recovery, drill, dump, certificate renewal or maintenance
// run. The interactive RPC that starts a live mutation still uses a checked
// start when activityImportant says so.
func jobSafetyJob(string) bool { return true }

// jobWarningCollector captures bounded warning events replayed from a job's
// journal so a structured follower can return them in the job result even
// though it intentionally ignores the job process's final envelope. It never
// captures log lines or free-form diagnostic text.
type jobWarningCollector struct {
	mu       sync.Mutex
	warnings []output.Warning
}

func (c *jobWarningCollector) reporter(next engine.Reporter) engine.Reporter {
	return func(ev output.Event) {
		if ev.Type == "warning" && ev.Code != "" && ev.Message != "" {
			c.mu.Lock()
			c.warnings = append(c.warnings, output.Warning{Code: ev.Code, Message: ev.Message})
			c.mu.Unlock()
		}
		if next != nil {
			next(ev)
		}
	}
}

func (c *jobWarningCollector) list() []output.Warning {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]output.Warning(nil), c.warnings...)
}

// finishRunJobActivity is RunJob's finalizer. A recovered panic always records
// unknown/panic; it never falls through to the success inference. The caller
// re-raises the panic after this returns. The final activity write runs after
// every lock is released, then the persisted record is settled within a bound
// so a follower knows the evidence is complete.
func finishRunJobActivity(ja *jobActivity, err error, skipped string, recovered any) {
	if ja == nil {
		return
	}
	if !ja.finished {
		if recovered != nil {
			ja.finish(context.Background(), activity.OutcomeUnknown, output.CodeError, "panic")
		} else {
			outcome, code, reason := ja.pendingOutcome, ja.pendingCode, ja.pendingReason
			if outcome == "" {
				switch {
				case err != nil && errors.Is(err, errSkipped):
					outcome, code, reason = activity.OutcomeSkipped, "", "job_skipped"
				case err != nil:
					code, reason = activityErrorTokens(err)
					outcome = activity.OutcomeFailed
				case skipped != "":
					outcome, code, reason = activity.OutcomeSkipped, "", "job_skipped"
				default:
					outcome = activity.OutcomeSucceeded
				}
			}
			ja.finish(context.Background(), outcome, code, reason)
		}
	}
	if !ja.settle() {
		ja.warn()
	}
}

// jobSettleWait bounds how long a finished job waits for its completion
// evidence to be persisted. Tests shorten it. A state lock or filesystem that
// outlasts it leaves the record pending, which a follower reports as
// incomplete evidence.
var jobSettleWait = 2 * time.Second

// jobSettleCap bounds the settlement goroutines pinned on a stalled state
// file. Settlement is diagnostic bookkeeping; it must not grow with every
// in-process job run.
var (
	jobSettleCap   int64 = 2
	jobSettlesLive atomic.Int64
)

// acquireJobSettler reserves one settlement slot, reporting false when the cap
// is already held by goroutines waiting on the state file.
func acquireJobSettler() bool {
	limit := atomic.LoadInt64(&jobSettleCap)
	for {
		live := jobSettlesLive.Load()
		if live >= limit {
			return false
		}
		if jobSettlesLive.CompareAndSwap(live, live+1) {
			return true
		}
	}
}

// releaseJobSettler frees the slot a settlement goroutine held.
func releaseJobSettler() { jobSettlesLive.Add(-1) }

// settle clears the job record's pending marker after the final activity
// write and carries a lost-evidence warning into it. The terminal state is
// already durable; this only tells a follower whether the evidence is
// complete. It reports false when the state lock or write did not finish in
// time, leaving the record pending instead of holding the finished job.
func (a *jobActivity) settle() bool {
	if a == nil || !a.recorded || a.scope == nil || a.scope.jobID == "" {
		return true
	}
	if !acquireJobSettler() {
		return false
	}
	done := make(chan error, 1)
	go func() {
		defer releaseJobSettler()
		done <- settleJobRecord(a.scope.jobID, a.scope.lostEvidence())
	}()
	timer := time.NewTimer(jobSettleWait)
	defer timer.Stop()
	select {
	case err := <-done:
		return err == nil
	case <-timer.C:
		return false
	}
}

// settleJobRecord updates the persisted terminal record. Its context bounds
// the wait for another process's state write; the save itself still runs to
// completion in this goroutine. It is a seam so a test can observe settlement
// without a node state file.
var settleJobRecord = func(id string, lost bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), jobSettleWait)
	defer cancel()
	return engine.UpdateStateWithin(ctx, func(st *engine.State) error {
		rec, ok := st.Job(id)
		if !ok {
			return nil
		}
		rec.ActivityPending = false
		if lost {
			rec.Warnings = mergeJobWarnings(rec.Warnings, []output.Warning{activityWarning()})
		}
		st.PutJob(rec)
		return nil
	})
}

// mergeJobWarnings joins two bounded warning lists without duplicates.
func mergeJobWarnings(a, b []output.Warning) []output.Warning {
	out := make([]output.Warning, 0, len(a)+len(b))
	seen := make(map[output.Warning]bool, len(a)+len(b))
	for _, group := range [][]output.Warning{a, b} {
		for _, warning := range group {
			if warning.Code == "" || warning.Message == "" || seen[warning] {
				continue
			}
			seen[warning] = true
			out = append(out, warning)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// jobSkipReason maps the lock a job found busy to a stable token.
func jobSkipReason(busy *jobLockBusy, err error) string {
	if err != nil {
		return "stopped"
	}
	if busy == nil {
		return "job_skipped"
	}
	if busy.fields["lock"] == "heavy_io" {
		return "heavy_io_lock_held"
	}
	if _, ok := busy.fields["cluster"]; ok {
		return "cluster_lock_held"
	}
	return "lock_held"
}
