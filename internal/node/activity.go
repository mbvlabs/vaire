package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// This file is the node's emission side of the activity contract: it turns
// typed RPC, job and step lifecycle facts into activity.Event values. It never
// records params, results, SQL, command output, request or result bodies, or
// free-form error text. Those stay in the existing run logs.
//
// Emission is deliberately best effort for reads and safety-critical
// background work. Important mutations take a checked local start instead: if
// that write fails, the mutation is refused before it has side effects.

// activityStoreTimeout bounds how long an operation waits for its own local
// history write. The store's own limits still apply.
const activityStoreTimeout = 2 * time.Second

// activityWriteWait is how long a caller waits for one queued write. Tests
// shorten it. It bounds the caller, not the write: a store call that ignores
// its context must never hold an operation, a lock or a safety job.
var activityWriteWait = activityStoreTimeout

// activityWriterCap bounds the writer goroutines alive in one process. A store
// call that ignores cancellation pins its goroutine for as long as the
// filesystem stalls it; without a process-wide bound, repeated in-process runs
// (vaire server init --local) would add one more on every request.
var (
	activityWriterCap   int64 = 8
	activityWritersLive atomic.Int64
)

// acquireActivityWriter reserves one writer slot, reporting false when the cap
// is already held by goroutines waiting on the store.
func acquireActivityWriter() bool {
	limit := atomic.LoadInt64(&activityWriterCap)
	for {
		live := activityWritersLive.Load()
		if live >= limit {
			return false
		}
		if activityWritersLive.CompareAndSwap(live, live+1) {
			return true
		}
	}
}

// releaseActivityWriter frees the slot a writer goroutine held.
func releaseActivityWriter() { activityWritersLive.Add(-1) }

// activityQueryTimeout bounds a read-only activity RPC.
const activityQueryTimeout = 5 * time.Second

var (
	errActivityStoreUnavailable = errors.New("activity store unavailable")
	errActivityStorePanic       = errors.New("activity: the store failed while recording this event")
	errActivityWriterBusy       = errors.New("activity: an earlier history write is still in progress")
	errActivityWriteTimeout     = errors.New("activity: timed out recording this event")
)

// activityWarningCode and activityWarningMessage report lost history without
// failing an operation that has already done its work.
const (
	activityWarningCode    = "activity_not_recorded"
	activityWarningMessage = "the local activity history could not record this operation; its outcome is unchanged and it was not retried automatically"
)

// activityStore is the part of activity.Store the node uses. Keeping it an
// interface lets tests inject a store without touching /var/log.
type activityStore interface {
	Append(context.Context, activity.Event) (activity.Event, error)
	Query(context.Context, activity.Query) (activity.Page, error)
	Health(context.Context) (activity.Health, error)
}

// activityStoreFactory is the production store. Tests replace it.
var activityStoreFactory = func() (activityStore, error) {
	return activity.DefaultStore(), nil
}

// activityPaths are the fixed files the node reads for event metadata. They
// are injected in tests so no test reads or writes the real system paths.
type activityPaths struct {
	Node     string
	Cluster  string
	Instance string
	BootID   string
}

var defaultActivityPaths = activityPaths{
	Node:     config.NodePath,
	Cluster:  config.ClusterPath,
	Instance: masterbackup.InstancePath,
	BootID:   "/proc/sys/kernel/random/boot_id",
}

// activityRecorderFactory builds the recorder for one Serve or job run.
// Tests replace it with a recorder over a fake store.
var activityRecorderFactory = func() *activityRecorder {
	return newActivityRecorder(defaultActivityPaths)
}

// activityNodeInfo is the stable identity attached to every event this
// process emits. Empty fields are omitted by the store's JSON encoding.
type activityNodeInfo struct {
	Server         string
	Node           string
	NodeInstanceID string
	BootID         string
	NodeVersion    string
}

// activityRecorder appends typed events with this node's metadata filled in.
// A nil recorder is disabled and every method is a no-op.
type activityRecorder struct {
	store   activityStore
	storeOK bool
	node    activityNodeInfo
	paths   activityPaths
	now     func() time.Time

	writer activityWriter

	// degraded stops further writes after one of them timed out. A recorder
	// lives for one request or job run, so this only stops that operation from
	// paying the same stall again.
	degraded atomic.Bool

	// nodeMu guards the lazily refreshed identity. A recorder can be built
	// before bootstrap has written node.toml or cluster.toml; later appends
	// may fill the missing fixed-path identity without reading a caller path.
	nodeMu sync.Mutex
}

// newActivityRecorder opens the store without reading the node's metadata: the
// writer loads it inside the caller's bounded wait, so diagnostic metadata I/O
// cannot hold an operation either. A store that cannot be opened yields a
// recorder that reports the failure on append rather than panicking.
func newActivityRecorder(paths activityPaths) *activityRecorder {
	rec := &activityRecorder{paths: paths, now: time.Now}
	store, err := activityStoreFactory()
	if err != nil || store == nil {
		rec.storeOK = false
		return rec
	}
	rec.store, rec.storeOK = store, true
	return rec
}

// activityWrite is one queued event. Exactly one of the caller and the writer
// moves it out of activityWritePending, so a write that finishes after the
// caller stopped waiting is either reported to that caller or handled as
// abandoned, never mistaken for one that succeeded in time.
type activityWrite struct {
	event activity.Event
	done  chan error
	state atomic.Int32

	// closeOnAbandon marks the start event of an operation whose caller refused
	// to run it because this write did not land in time. If it lands later, it
	// is closed as an incomplete outcome. Every other abandoned write is left
	// alone: that work went ahead, so rewriting its evidence as a refusal would
	// be a lie, and a late terminal is only a lost completion record.
	closeOnAbandon bool
}

const (
	activityWritePending int32 = iota
	activityWriteDone
	activityWriteAbandoned
)

// activityWriter serializes one recorder's writes on one goroutine. The queue
// has room for one event, so a store call that ignores its context can hold
// this single goroutine and nothing else: later callers fail fast instead of
// piling up behind it. The goroutine exits when the queue drains.
type activityWriter struct {
	mu      sync.Mutex
	queue   chan *activityWrite
	running bool
}

// submit queues wr, starting the writer if it is idle. It reports false when
// an earlier write still occupies the writer or the process-wide writer cap is
// already pinned on the store.
func (w *activityWriter) submit(rec *activityRecorder, wr *activityWrite) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.queue == nil {
		w.queue = make(chan *activityWrite, 1)
	}
	if !w.running {
		if !acquireActivityWriter() {
			// Enough goroutines are already pinned on the store. Adding one more
			// would let a stalled filesystem grow without bound.
			return false
		}
		w.running = true
		go w.run(rec)
	}
	select {
	case w.queue <- wr:
		return true
	default:
		return false
	}
}

// run writes queued events in order until the queue drains.
func (w *activityWriter) run(rec *activityRecorder) {
	defer releaseActivityWriter()
	for {
		wr, ok := w.next()
		if !ok {
			return
		}
		err := w.write(rec, wr)
		if wr.state.CompareAndSwap(activityWritePending, activityWriteDone) {
			wr.done <- err
			continue
		}
		if err == nil && wr.closeOnAbandon {
			w.closeAbandoned(rec, wr.event)
		}
	}
}

// write fills the event's metadata and appends it. Metadata is filled here
// rather than in the caller, so its file reads are covered by the same bounded
// wait as the store write. A panicking store is reported as a failed write
// instead of ending the process: this goroutine runs outside the request's
// recover.
func (w *activityWriter) write(rec *activityRecorder, wr *activityWrite) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errActivityStorePanic
		}
	}()
	rec.fill(&wr.event)
	_, err = rec.store.Append(context.Background(), wr.event)
	return err
}

// next takes the next queued event, or reports that the queue drained and
// this goroutine may exit.
func (w *activityWriter) next() (*activityWrite, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	select {
	case wr := <-w.queue:
		return wr, true
	default:
		w.running = false
		return nil, false
	}
}

// closeAbandoned records that a start which landed too late belongs to an
// operation that was refused, so the pair reads as an incomplete outcome
// rather than as work that ran. The handler never ran, so unknown is honest.
// It only applies while the process is still alive: vaire node rpc may exit
// first, leaving the start without a terminal event, which also reads as
// incomplete.
func (w *activityWriter) closeAbandoned(rec *activityRecorder, start activity.Event) {
	defer func() { _ = recover() }()
	terminal := start
	terminal.EventID = ""
	terminal.OccurredAt = ""
	terminal.Sequence = 0
	terminal.Outcome = activity.OutcomeUnknown
	terminal.ErrorCode = output.CodePrecondition
	terminal.ReasonCode = "activity_unavailable"
	terminal.DurationMS = 0
	_, _ = rec.store.Append(context.Background(), terminal)
}

// append hands one best-effort event to this recorder's writer. The write is
// deliberately not tied to the caller's context: a final outcome is recorded
// even after a signal, and a stalled filesystem must never hold the caller.
// The caller waits at most activityWriteWait and treats a timeout as a lost
// event, which the checked-start policy turns into a refusal.
func (r *activityRecorder) append(_ context.Context, ev activity.Event) error {
	return r.submitEvent(ev, false)
}

// appendChecked hands over the start event of an operation that must not run
// without it, so an abandoned start that still lands is closed as incomplete.
func (r *activityRecorder) appendChecked(ev activity.Event) error {
	return r.submitEvent(ev, true)
}

// submitEvent queues ev and waits at most activityWriteWait for the writer.
func (r *activityRecorder) submitEvent(ev activity.Event, closeOnAbandon bool) error {
	if r == nil {
		return nil
	}
	if !r.storeOK || r.store == nil {
		return errActivityStoreUnavailable
	}
	if r.degraded.Load() {
		return errActivityWriterBusy
	}
	wr := &activityWrite{event: ev, done: make(chan error, 1), closeOnAbandon: closeOnAbandon}
	if !r.writer.submit(r, wr) {
		// A write is still in flight and the queue is full. That is bounded
		// already: report the event as lost without adding another waiter.
		return errActivityWriterBusy
	}
	timer := time.NewTimer(activityWriteWait)
	defer timer.Stop()
	select {
	case err := <-wr.done:
		return err
	case <-timer.C:
	}
	if !wr.state.CompareAndSwap(activityWritePending, activityWriteAbandoned) {
		// The write won the race; its result is already on its way.
		return <-wr.done
	}
	r.degraded.Store(true)
	return errActivityWriteTimeout
}

// fill adds this node's identity to ev without overwriting values a caller
// set deliberately (a job scope sets its own correlation ids first).
func (r *activityRecorder) fill(ev *activity.Event) {
	if ev.Schema == "" {
		ev.Schema = activity.Schema
	}
	r.refreshIdentity()
	r.nodeMu.Lock()
	node := r.node
	r.nodeMu.Unlock()
	if ev.Server == "" {
		ev.Server = node.Server
	}
	if ev.Node == "" {
		ev.Node = node.Node
	}
	if ev.NodeInstanceID == "" {
		ev.NodeInstanceID = node.NodeInstanceID
	}
	if ev.BootID == "" {
		ev.BootID = node.BootID
	}
	if ev.NodeVersion == "" {
		ev.NodeVersion = node.NodeVersion
	}
	// The RPC operator field is claimed by the client. It is never proof of
	// identity, so this stays false even when a label is present.
	ev.ActorVerified = false
}

// refreshIdentity fills identity fields that were absent when the recorder
// was built (during bootstrap, the files may not exist yet). It reads only the
// recorder's fixed paths and stops once every field is known.
func (r *activityRecorder) refreshIdentity() {
	if r == nil {
		return
	}
	if r.paths.Node == "" && r.paths.Cluster == "" && r.paths.Instance == "" && r.paths.BootID == "" {
		return
	}
	r.nodeMu.Lock()
	defer r.nodeMu.Unlock()
	if r.node.Server != "" && r.node.Node != "" && r.node.NodeInstanceID != "" && r.node.BootID != "" {
		return
	}
	fresh := activityNodeInfoLoader(r.paths, config.Cluster{})
	if r.node.Server == "" {
		r.node.Server = fresh.Server
	}
	if r.node.Node == "" {
		r.node.Node = fresh.Node
	}
	if r.node.NodeInstanceID == "" {
		r.node.NodeInstanceID = fresh.NodeInstanceID
	}
	if r.node.BootID == "" {
		r.node.BootID = fresh.BootID
	}
	if r.node.NodeVersion == "" {
		r.node.NodeVersion = fresh.NodeVersion
	}
}

// activityScope is one RPC or job life: its correlation ids, action and actor,
// plus step timings. It is not safe for concurrent step reporters; the engine
// reports steps from one goroutine.
type activityScope struct {
	rec             *activityRecorder
	action          string
	source          string
	operationID     string
	requestID       string
	parentRequestID string
	jobID           string
	database        string
	actorKind       string
	actorLabel      string

	started  time.Time
	finished bool
	warned   sync.Once

	lostMu       sync.Mutex
	lost         bool
	resultWarned bool

	stepMu      sync.Mutex
	stepStarted map[string]time.Time
}

// newRPCScope builds the scope of one RPC attempt.
func newRPCScope(rec *activityRecorder, method string, req rpc.Request) *activityScope {
	op := req.OperationID
	if op == "" {
		op = req.RequestID
	}
	return &activityScope{
		rec:             rec,
		action:          safeActivityAction(method),
		source:          activity.SourceRPC,
		operationID:     safeActivityID(op),
		requestID:       safeActivityID(req.RequestID),
		parentRequestID: safeActivityID(req.ParentRequestID),
		database:        activityDatabaseFor(method, req.Params),
		actorKind:       activity.ActorOperator,
		actorLabel:      activityActorLabel(req.Operator),
		stepStarted:     map[string]time.Time{},
	}
}

// newJobScope builds the scope of one job run. OperationID defaults to the
// job id for a timer run that had no earlier RPC; the job id stays the same as
// the starting request for an RPC job.
func newJobScope(rec *activityRecorder, job, id, operationID, parentRequestID, database string, timer bool) *activityScope {
	if operationID == "" {
		operationID = id
	}
	actor := activity.ActorOperator
	if timer {
		actor = activity.ActorTimer
	}
	return &activityScope{
		rec:             rec,
		action:          jobActivityAction(job),
		source:          activity.SourceJob,
		operationID:     safeActivityID(operationID),
		requestID:       safeActivityID(id),
		parentRequestID: safeActivityID(parentRequestID),
		jobID:           safeActivityID(id),
		database:        safeActivityResource(database),
		actorKind:       actor,
		stepStarted:     map[string]time.Time{},
	}
}

// begin records the started fact of read-only or safety work, which continues
// even when the event cannot be written.
func (s *activityScope) begin(ctx context.Context) error {
	return s.beginEvent(ctx, false)
}

// beginChecked records the start of an operation that must not run without it.
// A start that lands only after the caller refused is closed as incomplete.
func (s *activityScope) beginChecked(ctx context.Context) error {
	return s.beginEvent(ctx, true)
}

// beginEvent records the started fact. The caller decides whether a failure
// must refuse the work (important mutations) or only warn (reads and
// background safety jobs).
func (s *activityScope) beginEvent(ctx context.Context, checked bool) error {
	if s == nil || s.rec == nil {
		return nil
	}
	s.started = s.now()
	ev := s.event(activity.OutcomeStarted, "", "", 0)
	var err error
	if checked {
		err = s.rec.appendChecked(ev)
	} else {
		err = s.rec.append(ctx, ev)
	}
	if err != nil {
		s.markLost()
	}
	return err
}

// finish records a terminal fact. A panic leaves outcome unknown; a normal
// error is failed unless the caller maps it to refused or skipped first.
func (s *activityScope) finish(ctx context.Context, outcome, code, reason string) error {
	if s == nil || s.rec == nil || s.finished {
		return nil
	}
	s.finished = true
	ms := int64(0)
	if !s.started.IsZero() {
		ms = s.now().Sub(s.started).Milliseconds()
		if ms < 0 {
			ms = 0
		}
	}
	err := s.rec.append(ctx, s.event(outcome, code, reason, ms))
	if err != nil {
		s.markLost()
	}
	return err
}

// finishWarn records a terminal fact and reports a bounded warning through
// emit when the write fails. The operation has already run; it is never
// replayed or rolled back to make history look complete.
func (s *activityScope) finishWarn(ctx context.Context, outcome, code, reason string, emit func(output.Event)) error {
	err := s.finish(ctx, outcome, code, reason)
	if err != nil && s != nil {
		s.warn(emit)
	}
	return err
}

// warn emits one warning per scope, so a failing store does not flood output.
func (s *activityScope) warn(emit func(output.Event)) {
	if s == nil || emit == nil {
		return
	}
	s.warned.Do(func() {
		emit(activityWarningEvent())
	})
}

// markLost records that some evidence for this scope did not reach the store.
// The scope keeps the fact so a successful result can still carry a warning
// even when the terminal append happens to succeed.
func (s *activityScope) markLost() {
	if s == nil {
		return
	}
	s.lostMu.Lock()
	s.lost = true
	s.lostMu.Unlock()
}

// lostEvidence reports whether any append for this scope failed.
func (s *activityScope) lostEvidence() bool {
	if s == nil {
		return false
	}
	s.lostMu.Lock()
	defer s.lostMu.Unlock()
	return s.lost
}

// resultWarning returns the one result-envelope warning for lost evidence.
func (s *activityScope) resultWarning() (output.Warning, bool) {
	if s == nil || !s.lostEvidence() {
		return output.Warning{}, false
	}
	s.lostMu.Lock()
	defer s.lostMu.Unlock()
	if s.resultWarned {
		return output.Warning{}, false
	}
	s.resultWarned = true
	return activityWarning(), true
}

// finishActivitySuccess records a successful RPC outcome and makes any lost
// evidence visible in the success envelope, even when this terminal append
// itself succeeds.
func finishActivitySuccess(ctx context.Context, scope *activityScope, res *Result, emit func(output.Event)) {
	if err := scope.finish(ctx, activity.OutcomeSucceeded, "", ""); err != nil {
		scope.warn(emit)
	}
	if warning, ok := scope.resultWarning(); ok {
		res.Warnings = append(res.Warnings, warning)
	}
}

// recorder wraps next so only typed step events are added to history. Raw
// log, progress and warning payloads are ignored: they stay in the run log
// and stream, not in the agent-shareable history.
func (s *activityScope) recorder(next engine.Reporter) engine.Reporter {
	if s == nil {
		return next
	}
	return func(ev output.Event) {
		if ev.Type == "step" {
			s.recordStep(ev, next)
		}
		if next != nil {
			next(ev)
		}
	}
}

// recordStep adds one step occurrence. Detail is free text, so it is only used
// as a reason when it is one of the engine's stable tokens. A step replayed
// from a job's journal carries its original timestamp; it was recorded where
// it actually ran, so the follower must not duplicate it.
func (s *activityScope) recordStep(ev output.Event, next engine.Reporter) {
	if ev.TS != "" {
		return
	}
	if s == nil || s.rec == nil {
		return
	}
	step := safeActivityStep(ev.Step)
	if step == "" {
		return
	}
	outcome, code, reason := activityStepOutcome(ev.State, ev.Detail)
	now := s.now()
	var ms int64
	s.stepMu.Lock()
	if outcome == activity.OutcomeStarted {
		s.stepStarted[step] = now
	} else if started, ok := s.stepStarted[step]; ok {
		ms = now.Sub(started).Milliseconds()
		delete(s.stepStarted, step)
	}
	s.stepMu.Unlock()
	if ms < 0 {
		ms = 0
	}
	e := s.event(outcome, code, reason, ms)
	e.Source = activity.SourceStep
	e.Step = step
	if cluster := stepCluster(step); cluster != "" {
		e.Database = cluster
	}
	if err := s.rec.append(context.Background(), e); err != nil {
		s.markLost()
		s.warn(next)
	}
}

func (s *activityScope) event(outcome, code, reason string, ms int64) activity.Event {
	return activity.Event{
		OperationID:     s.operationID,
		RequestID:       s.requestID,
		ParentRequestID: s.parentRequestID,
		JobID:           s.jobID,
		Source:          s.source,
		Action:          s.action,
		Database:        s.database,
		Outcome:         outcome,
		ErrorCode:       code,
		ReasonCode:      reason,
		DurationMS:      ms,
		ActorKind:       s.actorKind,
		ActorLabel:      s.actorLabel,
		ActorVerified:   false,
	}
}

func (s *activityScope) now() time.Time {
	if s.rec != nil && s.rec.now != nil {
		return s.rec.now()
	}
	return time.Now()
}

// beginActivityScope applies the checked-start policy. Important mutations
// refuse when their start cannot be written; reads and background work warn
// and stay available.
func beginActivityScope(ctx context.Context, scope *activityScope, important bool, emit func(output.Event)) error {
	if scope == nil {
		return nil
	}
	var err error
	if important {
		err = scope.beginChecked(ctx)
	} else {
		err = scope.begin(ctx)
	}
	if err != nil {
		if important {
			return activityUnavailableError()
		}
		scope.warn(emit)
	}
	return nil
}

// recordActivityRefusal records a request the node refused before its
// handler ran. It is best effort: the refusal itself is the answer even when
// history cannot be written.
func recordActivityRefusal(rec *activityRecorder, req rpc.Request, action string, err error, defaultReason string) {
	if rec == nil {
		return
	}
	code, reason := activityErrorTokens(err)
	if reason == "" {
		reason = safeActivityToken(defaultReason)
	}
	op := safeActivityID(req.OperationID)
	if op == "" {
		op = safeActivityID(req.RequestID)
	}
	ev := activity.Event{
		OperationID:     op,
		RequestID:       safeActivityID(req.RequestID),
		ParentRequestID: safeActivityID(req.ParentRequestID),
		Source:          activity.SourceRPC,
		Action:          safeActivityAction(action),
		Database:        activityDatabaseFor(action, req.Params),
		Outcome:         activity.OutcomeRefused,
		ErrorCode:       code,
		ReasonCode:      reason,
		ActorKind:       activity.ActorOperator,
		ActorLabel:      activityActorLabel(req.Operator),
		ActorVerified:   false,
	}
	_ = rec.append(context.Background(), ev)
}

// recordJobRefusal records a job invocation refused before its runner began.
// timer says the invocation came from a systemd timer, so history does not
// attribute it to an operator who was not there.
func recordJobRefusal(rec *activityRecorder, job, id string, err error, defaultReason string, timer bool) {
	if rec == nil {
		return
	}
	code, reason := activityErrorTokens(err)
	if reason == "" {
		reason = safeActivityToken(defaultReason)
	}
	actor := activity.ActorOperator
	if timer {
		actor = activity.ActorTimer
	}
	ev := activity.Event{
		OperationID:   safeActivityID(id),
		JobID:         safeActivityID(id),
		Source:        activity.SourceJob,
		Action:        jobActivityAction(job),
		Outcome:       activity.OutcomeRefused,
		ErrorCode:     code,
		ReasonCode:    reason,
		ActorKind:     actor,
		ActorVerified: false,
	}
	_ = rec.append(context.Background(), ev)
}

// activityDryRunAware lists the methods whose handler honours req.DryRun and
// changes nothing when it is set, but whose other rules still look like a
// mutation (an exclusive lock or an unconditional run log). Only these skip
// the checked start because of the flag; methods that accept but ignore it
// (db.create, db.drop, protect.init, cluster bootstrap) stay checked. Other
// dry-run-aware handlers already classify as non-important through their own
// lock or run-log rules.
var activityDryRunAware = map[string]bool{
	"tune.apply":     true,
	"db.fork":        true,
	"queries.reset":  true,
	"metrics.rotate": true,
}

// activityImportant says whether a request must have a checked local start
// before it runs. Reads, dry runs of methods that honour the flag, and the
// explicitly sensitive no-run-log methods are classified statically; no
// request field can upgrade a read.
func activityImportant(m method, req rpc.Request) bool {
	if req.DryRun && activityDryRunAware[m.name] {
		return false
	}
	switch m.name {
	case "protect.init", "db.write-access":
		return true
	case "sql":
		// sql takes only the shared lock and has no run log, but a write
		// through a temporary write role is a mutation.
		return sqlWrites(req)
	case "plan":
		// plan has a run log but changes nothing.
		return false
	case "db.credentials":
		// Only rotating a password mutates; plain credentials are refused on
		// the node and stay available like any other refusal.
		return rotateRequested(req.Params)
	case "backup.now", "backup.verify", "restore.start", "restore.drill",
		"restore.abort", "restore.cleanup", "restore.promote", "restore.undo",
		"dump.create", "db.recover", "master-backup.now", "maint.run":
		// These start or clean up background work, but a diagnostic history
		// problem must not stop backups, drills, dumps, recovery copies or
		// maintenance. The job runner records the outcome in state.json and
		// reports any lost activity evidence as a warning.
		return false
	case "ssh.confirm", "ssh.lockdown", "upgrade.install", "protect.verify",
		"cluster.confirm-secrets":
		// These are automatic or recovery prerequisites: confirming SSH,
		// upgrading the binary, verifying the authenticator gate and
		// confirming the stored handoff must still work when the diagnostic
		// store is unavailable. Refusing them would lock the operator out.
		return false
	}
	if m.runLog != nil && m.runLog(req) {
		return true
	}
	return m.lock != nil && m.lock(req) == engine.LockExclusive
}

// isActivityMethod reports the registered read-only history methods. They are
// never emitted into history themselves and never create the store. An unknown
// activity.* name is not exempt: it is refused and recorded as node.rpc rather
// than escaping the audit trail by prefix.
func isActivityMethod(name string) bool {
	switch name {
	case "activity.list", "activity.show", "activity.health":
		return true
	}
	return false
}

// activityUnavailableError is the checked-start refusal. It carries a stable
// reason and no store error text.
func activityUnavailableError() *output.CLIError {
	return output.NewError(
		output.CodePrecondition,
		"the node's activity history is unavailable, so this operation was not started",
		output.ExitPrecondition,
		"A human should inspect the node's activity evidence and storage with vaire activity health and vaire doctor --only host before retrying. Do not move or reset the directory while the evidence may still matter.",
	).WithDetails(map[string]any{"reason": "activity_unavailable"})
}

// activityWarning is the success-result warning for a lost event.
func activityWarning() output.Warning {
	return output.Warning{Code: activityWarningCode, Message: activityWarningMessage}
}

func activityWarningEvent() output.Event {
	return output.Event{Type: "warning", Code: activityWarningCode, Message: activityWarningMessage}
}

// activityErrorTokens extracts only a known stable error code and a bounded
// reason token from a typed error. Message, hint, params and arbitrary details
// are never copied.
func activityErrorTokens(err error) (string, string) {
	code, reason := output.CodeError, ""
	if err == nil {
		return code, reason
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		if output.IsKnownCode(cliErr.Code) {
			code = cliErr.Code
		}
		reason = activityReasonOf(cliErr.Details)
	}
	switch {
	case errors.Is(err, context.Canceled):
		reason = firstActivityValue(reason, "cancelled")
	case errors.Is(err, context.DeadlineExceeded):
		reason = firstActivityValue(reason, "deadline_exceeded")
	}
	return code, reason
}

func activityReasonOf(details any) string {
	switch d := details.(type) {
	case map[string]any:
		if s, ok := d["reason"].(string); ok {
			return safeActivityToken(s)
		}
	case map[string]string:
		return safeActivityToken(d["reason"])
	}
	return ""
}

// activityStepOutcome maps engine step states without copying step detail.
func activityStepOutcome(state, detail string) (string, string, string) {
	switch state {
	case "started":
		return activity.OutcomeStarted, "", ""
	case "completed", "ok":
		return activity.OutcomeSucceeded, "", ""
	case "failed":
		return activity.OutcomeFailed, output.CodeStepFailed, ""
	case "skipped":
		return activity.OutcomeSkipped, "", safeActivityToken(detail)
	}
	return activity.OutcomeUnknown, "", ""
}

// stepCluster returns the database of a per-cluster step instance such as
// postgres-config-v1@app. A full apply has no request db filter, so the step
// id is the only source of that resource name.
func stepCluster(step string) string {
	_, cluster := engine.SplitID(step)
	return safeActivityResource(cluster)
}

// jobActivityAction is the typed action of a job. Only a known job may be
// copied into an exported event; anything else becomes the fixed job.unknown.
func jobActivityAction(job string) string {
	if job == "" || !knownJob(job) {
		return "job.unknown"
	}
	return "job." + job
}

// safeActivityAction keeps only a registered method name or the fixed
// node.rpc fallback. A syntactically valid but unknown request name could be
// attacker-chosen text, so it is never copied into an exported field.
func safeActivityAction(action string) string {
	if action == "node.rpc" {
		return action
	}
	if _, ok := methods()[action]; ok {
		return action
	}
	return "node.rpc"
}

// activityDatabaseFor reads only the well-known database params of methods
// where they mean a database. It never reads name or source generically:
// name is a backup name in master-backup.get, a database in db.drop, and
// something else elsewhere.
func activityDatabaseFor(method string, params json.RawMessage) string {
	if len(params) == 0 {
		return ""
	}
	var p struct {
		DB       string `json:"db"`
		Database string `json:"database"`
		Name     string `json:"name"`
	}
	if json.Unmarshal(params, &p) != nil {
		return ""
	}
	if p.DB != "" {
		return safeActivityResource(p.DB)
	}
	if p.Database != "" {
		return safeActivityResource(p.Database)
	}
	if activityNameIsDatabase[method] {
		return safeActivityResource(p.Name)
	}
	return ""
}

// activityNameIsDatabase lists the methods whose name param is a database.
var activityNameIsDatabase = map[string]bool{
	"db.create": true, "db.drop": true, "db.resize": true, "db.credentials": true, "db.fork": true,
}

// activityNodeInfoLoader reads the node's optional identity files. Tests
// replace it to stall metadata I/O without a fault filesystem.
var activityNodeInfoLoader = loadActivityNodeInfo

// loadActivityNodeInfo reads optional node identity files. A missing or
// malformed file omits the field; it never fails the request.
func loadActivityNodeInfo(paths activityPaths, cluster config.Cluster) activityNodeInfo {
	var info activityNodeInfo
	if node, err := config.LoadNode(paths.Node); err == nil {
		info.Server = safeActivityResource(node.Server)
		info.Node = safeActivityResource(node.NodeName)
	}
	if cluster.Name() == "" {
		if cfg, err := config.LoadCluster(paths.Cluster); err == nil {
			cluster = cfg
		}
	}
	if info.Server == "" {
		info.Server = safeActivityResource(cluster.Name())
	}
	if info.Node == "" {
		if master, ok := cluster.Master(); ok {
			info.Node = safeActivityResource(master.Name)
		}
	}
	if raw, err := os.ReadFile(paths.Instance); err == nil { //nolint:gosec // G304: a fixed node path supplied by the caller.
		info.NodeInstanceID = safeActivityResource(strings.TrimSpace(string(raw)))
	}
	if raw, err := os.ReadFile(paths.BootID); err == nil { //nolint:gosec // G304: a fixed node path supplied by the caller.
		info.BootID = safeActivityResource(strings.TrimSpace(string(raw)))
	}
	info.NodeVersion = activityNodeVersion()
	return info
}

func activityNodeVersion() string {
	version := buildinfo.Version
	if buildinfo.Commit != "" && buildinfo.Commit != "none" {
		version += "+" + buildinfo.Commit
	}
	if len(version) > 64 {
		version = version[:64]
	}
	if !activityVersionPattern.MatchString(version) {
		return ""
	}
	return version
}

// activityKnownReason is the explicit allowlist for ReasonCode. Error details
// and step detail are untrusted text; a lower-case token that merely matches
// the identifier regex must not become shareable evidence.
var activityKnownReason = map[string]bool{
	// Step engine decisions.
	"new": true, "inputs_changed": true, "check_incomplete": true,
	"previous_failure": true, "journal": true, "check": true,
	// Step skip reasons.
	"no_databases": true, "backups_not_configured": true,
	// Node refusal and outcome reasons.
	"activity_unavailable": true, "already_a_node": true, "already_placed": true,
	"already_undone": true, "awaiting_recovery": true, "backend_not_found": true,
	"backup_not_configured": true, "bundle_server_mismatch": true,
	"backup_credentials_missing": true, "backup_credentials_rotated": true,
	"cipher_mismatch": true, "cluster_mismatch": true,
	"config_changed": true, "confirm_mismatch": true, "database_exists": true,
	"database_not_found": true, "database_protected": true, "database_required": true,
	"dump_not_found": true, "epoch_conflict": true, "extension_missing": true,
	"final_dump_failed": true, "heavy_io_lock_held": true, "host_phase_incomplete": true,
	"immutable_key": true, "install_source_invalid": true, "job_not_found": true,
	"job_skipped": true, "job_spec_missing": true, "job_spec_mismatch": true,
	"job_spec_required": true, "cluster_lock_held": true, "lock_held": true,
	"journal_corrupt": true, "pgbackrest_too_old": true, "preflight_failed": true,
	"sha256_mismatch": true, "state_corrupt": true,
	"no_backup_in_stanza": true, "no_backups": true, "no_cluster_port": true,
	"no_dump_recipient": true, "no_leftover": true, "no_lockdown_pending": true,
	"no_offsite_backup": true, "no_open_restore": true, "no_ready_restore": true,
	"node_belongs_to_other_server": true, "node_fenced": true, "node_outdated": true,
	"not_a_node": true, "not_awaiting_recovery": true, "operator_only": true,
	"os_unsupported": true, "outside_window": true, "postgres_present": true,
	"protect_already_initialized": true, "protect_not_initialized": true,
	"protect_not_pending": true, "protected_backend": true, "ram_too_small": true,
	"repack_unavailable": true, "repo_in_use": true, "restore_in_progress": true,
	"restore_promoted": true, "restore_running": true, "restore_swapped_in": true,
	"restore_undone": true, "restoring_exists": true, "root_required": true,
	"run_log_unavailable": true, "runs_on_server": true, "selfcheck_failed": true,
	"selfcheck_failed_rolled_back": true, "set_not_found": true,
	"sshd_port_not_22": true, "stanza_still_written": true, "tls_pending": true,
	"undo_in_progress": true, "write_requires_human": true,
	// Protection code outcomes.
	"code_invalid": true, "code_required": true, "code_reused": true, "locked": true,
	// Process and correlation outcomes.
	"cancelled": true, "deadline_exceeded": true, "panic": true,
	"request_rejected": true, "unknown_method": true, "unknown_job": true,
	"version_mismatch": true, "config_error": true, "stopped": true, "unchanged": true,
}

// The patterns below are the only shapes that may enter metadata fields.
// Anything else is omitted rather than truncated into a misleading token.
var (
	activityNamePattern    = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	activityLabelPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,63}$`)
	activityTokenPattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	activityStepPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@-]{0,127}$`)
	activityIDPattern      = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	activityVersionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+/-]{0,63}$`)
)

// safeActivityResource accepts a configured server, node, instance or
// database name. It is redacted first in case a secret value was configured
// in the same field.
func safeActivityResource(value string) string {
	value = strings.TrimSpace(secrets.Redact(value))
	if !activityNamePattern.MatchString(value) {
		return ""
	}
	return value
}

// activityActorLabel keeps the client's claimed label, marked unverified. It
// is not proof of identity.
func activityActorLabel(value string) string {
	value = strings.TrimSpace(secrets.Redact(value))
	if !activityLabelPattern.MatchString(value) {
		return ""
	}
	return value
}

func safeActivityToken(value string) string {
	value = strings.TrimSpace(secrets.Redact(value))
	if !activityTokenPattern.MatchString(value) || !activityKnownReason[value] {
		return ""
	}
	return value
}

func safeActivityStep(value string) string {
	value = strings.TrimSpace(secrets.Redact(value))
	if !activityStepPattern.MatchString(value) {
		return ""
	}
	return value
}

func safeActivityID(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if !activityIDPattern.MatchString(value) {
		return ""
	}
	return value
}

func firstActivityValue(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
