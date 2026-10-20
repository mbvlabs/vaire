// Package node is the node side of vaire: the RPC dispatcher, its method table,
// selfcheck and upgrade.install.
package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"sort"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// Env is what a method handler gets for one request.
type Env struct {
	Req     rpc.Request
	Cluster config.Cluster
	Log     *engine.RunLog
	Report  engine.Reporter
}

// Result is a successful method outcome.
type Result struct {
	Data     any
	Warnings []output.Warning
}

// method is one entry in the method table (§4.3).
type method struct {
	name string
	// lock returns the lock mode for the request. Nil means no lock.
	lock func(req rpc.Request) engine.LockMode
	// clusterLock names the cluster whose lock the request takes after the
	// global one, and the mode. An empty name takes none.
	clusterLock func(req rpc.Request) (string, engine.LockMode)
	// cluster loads cluster.toml before the confirm check.
	cluster bool
	// confirm returns the token the request must carry. Nil means none.
	confirm func(cfg config.Cluster, params json.RawMessage) string
	// confirmAny returns the tokens a method that takes several accepts;
	// every token the request carries must be one of them.
	confirmAny func(cfg config.Cluster, params json.RawMessage) []string
	// runLog reports whether the request writes a run log.
	runLog func(req rpc.Request) bool
	// versionExempt skips the client/node build check.
	versionExempt bool
	handler       func(ctx context.Context, env *Env) (Result, error)
}

func always(rpc.Request) bool { return true }

// fencedReads are methods with a run log that still answer on a fenced
// node: they change nothing a newer master owns.
var fencedReads = map[string]bool{"ssh.confirm": true, "upgrade.install": true}

// fencedRefuses reports whether a fenced node refuses req: every method
// that takes the node lock exclusive or writes a run log, apart from
// fencedReads.
func fencedRefuses(m method, req rpc.Request) bool {
	if !masterbackup.IsFenced() || fencedReads[m.name] {
		return false
	}
	if m.lock != nil && m.lock(req) == engine.LockExclusive {
		return true
	}
	return m.runLog != nil && m.runLog(req)
}

func fixedLock(mode engine.LockMode) func(rpc.Request) engine.LockMode {
	return func(rpc.Request) engine.LockMode { return mode }
}

// Serve runs `vaire node rpc`: read one request from stdin, run it, and stream
// redacted NDJSON events ending in exactly one result. It returns the exit
// status, which equals the result envelope's exit code.
func Serve(stdin io.Reader, stdout io.Writer) (code int) {
	out := &eventWriter{w: stdout}
	command := "node rpc"
	defer func() {
		if r := recover(); r != nil {
			out.log.Line("panic", fmt.Sprintf("%v\n%s", r, debug.Stack()))
			code = out.fail(command, panicError())
		}
		_ = out.log.Close()
	}()

	// 1. Root only.
	if os.Geteuid() != 0 {
		return out.fail(command, output.NewError(
			output.CodePrecondition,
			"vaire node rpc must run as root",
			output.ExitPrecondition,
			"Run it through sudo -n /usr/local/bin/vaire node rpc.",
		).WithDetails(map[string]any{"reason": "root_required"}))
	}

	// 2. Decode.
	req, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return out.fail(command, err)
	}
	command = req.Method

	// 3. Register secrets before anything else can print.
	for _, value := range req.Secrets {
		secrets.Add(value)
	}

	// 4. Version check. upgrade.install is the only exempt method.
	table := methods()
	m, known := table[req.Method]
	if !known || !m.versionExempt {
		if err := checkVersion(req); err != nil {
			return out.fail(command, err)
		}
	}

	// 5. Method lookup.
	if !known {
		names := make([]string, 0, len(table))
		for name := range table {
			names = append(names, name)
		}
		sort.Strings(names)
		return out.fail(command, output.NewError(
			output.CodeUsage,
			"unknown method "+req.Method,
			output.ExitUsage,
			"Use one of details.methods.",
		).WithDetails(map[string]any{"methods": names}))
	}

	// A fenced node (a newer master took over its bucket) refuses every
	// method that changes something; reads still answer (Phase 5, D5).
	if fencedRefuses(m, req) {
		return out.fail(command, fencedError())
	}
	if err := refuseAwaiting(req); err != nil {
		return out.fail(command, err)
	}

	env := &Env{Req: req, Report: out.emit}
	if m.cluster {
		cfg, err := config.LoadCluster(config.ClusterPath)
		if err != nil {
			return out.fail(command, err)
		}
		env.Cluster = cfg
		if raw, err := os.ReadFile(config.ClusterPath); err == nil && config.HasLegacyVPS(raw) {
			out.emit(output.Event{Type: "warning", Code: "legacy_config_ignored",
				Message: "cluster.toml still has [backup.vps] from before Phase 5 (restic is gone); it is ignored, and the next config change drops it"})
		}
	}

	// 6. Confirm tokens, recomputed on the node. A wrong token is refused
	// before any step runs.
	if err := checkConfirm(m, env.Cluster, req); err != nil {
		return out.fail(command, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	// 7. Lock.
	if m.lock != nil {
		release, err := engine.Acquire(ctx, m.lock(req), req.Method, req.RequestID, req.LockTimeout())
		if err != nil {
			return out.fail(command, err)
		}
		defer release()
	}
	if m.clusterLock != nil {
		if cluster, mode := m.clusterLock(req); cluster != "" {
			if _, ok := env.Cluster.ClusterByName(cluster); !ok {
				return out.fail(command, databaseNotFound(cluster))
			}
			release, err := engine.AcquireCluster(ctx, cluster, mode, req.Method, req.RequestID, req.LockTimeout())
			if err != nil {
				return out.fail(command, err)
			}
			defer release()
		}
	}

	// 8. Run log, handler, result.
	if m.runLog != nil && m.runLog(req) {
		log, err := engine.OpenRunLog(req.Method, req.RequestID)
		if err != nil {
			return out.fail(command, output.WrapError(output.CodePrecondition, err, output.ExitPrecondition,
				"Check that "+engine.LogDir+" is writable by root.").WithDetails(map[string]any{"reason": "run_log_unavailable"}))
		}
		out.setLog(log)
		env.Log = log
		log.JSON("request", req.Loggable())
	}
	res, err := runHandler(ctx, m, env, out)
	if err != nil {
		return out.fail(command, err)
	}
	return out.ok(command, res)
}

// checkConfirm refuses a request whose confirm tokens the method does not
// accept. A method without tokens ignores them.
func checkConfirm(m method, cfg config.Cluster, req rpc.Request) error {
	tokens := req.ConfirmTokens()
	if len(tokens) == 0 {
		return nil
	}
	var accepted []string
	switch {
	case m.confirmAny != nil:
		accepted = m.confirmAny(cfg, req.Params)
	case m.confirm != nil:
		if len(tokens) > 1 {
			return output.NewError(output.CodeUnsafeAction, req.Method+" takes one confirm token", output.ExitUnsafe,
				"Re-run with the token in details.confirm_token after a human approves it.").
				WithDetails(map[string]any{"confirm_token": m.confirm(cfg, req.Params)})
		}
		accepted = []string{m.confirm(cfg, req.Params)}
	default:
		return nil
	}
	ok := map[string]bool{}
	for _, token := range accepted {
		ok[token] = true
	}
	for _, token := range tokens {
		if ok[token] {
			continue
		}
		details := map[string]any{"confirm_token": accepted[0]}
		if len(accepted) > 1 {
			details["confirm_tokens"] = accepted
		}
		return output.NewError(
			output.CodeUnsafeAction,
			"the confirm token "+token+" does not match",
			output.ExitUnsafe,
			"Re-run with a token from details after a human approves it.",
		).WithDetails(details)
	}
	return nil
}

func runHandler(ctx context.Context, m method, env *Env, out *eventWriter) (res Result, err error) {
	defer func() {
		if r := recover(); r != nil {
			out.log.Line("panic", fmt.Sprintf("%v\n%s", r, debug.Stack()))
			err = panicError()
		}
	}()
	return m.handler(ctx, env)
}

func panicError() *output.CLIError {
	return output.NewError(
		output.CodeError,
		"vaire node rpc hit an internal error",
		output.ExitGeneric,
		"This is an vaire bug. The stack trace is in the run log.",
	)
}

func checkVersion(req rpc.Request) error {
	same := req.ClientVersion == buildinfo.Version && req.ClientChannel == buildinfo.Channel
	// Every dev build reports version "dev", so dev builds must also match on
	// commit.
	if same && buildinfo.Channel == "dev" {
		same = req.ClientCommit == buildinfo.Commit
	}
	if same {
		return nil
	}
	return output.NewError(
		output.CodeNodeVersionMismatch,
		fmt.Sprintf("operator build %s (%s, %s) does not match node build %s (%s, %s)",
			req.ClientVersion, req.ClientCommit, req.ClientChannel,
			buildinfo.Version, buildinfo.Commit, buildinfo.Channel),
		output.ExitVersion,
		"Install the operator's build on the node, or run a matching operator.",
	).WithDetails(map[string]any{
		"client": map[string]string{"version": req.ClientVersion, "commit": req.ClientCommit, "channel": req.ClientChannel},
		"node":   map[string]string{"version": buildinfo.Version, "commit": buildinfo.Commit, "channel": buildinfo.Channel},
	})
}

// streamQueueMax is how many lines may wait for stdout before log events are
// dropped from the stream. The run log still gets every line.
const streamQueueMax = 4096

// streamDrainTimeout is how long ok and fail wait for queued lines to reach
// stdout before Serve returns.
var streamDrainTimeout = 5 * time.Second

// eventWriter writes redacted NDJSON events, flushing after each line, and
// copies them to the run log. It writes the result at most once.
//
// Each line goes to the run log first, under mu, and is then queued for a
// writer goroutine that owns stdout. A stalled SSH channel therefore never
// blocks a handler's Report, and a broken one never loses the run log or
// stops the handler: after the first write error stdout is abandoned.
type eventWriter struct {
	mu   sync.Mutex
	w    io.Writer
	log  *engine.RunLog
	done bool
	// deadLogged records that the run log noted the stream loss.
	deadLogged bool

	start   sync.Once
	drained chan struct{}

	// qmu guards the queue fields; the writer goroutine waits on cond.
	qmu     sync.Mutex
	cond    *sync.Cond
	pending [][]byte
	closed  bool
	dropped int
	dead    bool
	deadErr error
}

func (e *eventWriter) setLog(log *engine.RunLog) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = log
}

func (e *eventWriter) emit(ev output.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return
	}
	e.writeLocked(ev)
}

func (e *eventWriter) writeLocked(ev output.Event) {
	ev.Schema = output.EventSchema
	if ev.TS == "" {
		ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
	}
	line, err := marshalLine(secrets.RedactValue(ev))
	if err != nil {
		line, _ = marshalLine(map[string]string{"schema": output.EventSchema, "type": "warning", "code": "event_encode_failed", "ts": ev.TS})
	}
	e.log.Line("event", string(line[:len(line)-1]))
	e.enqueueLocked(ev.Type, line)
}

// enqueueLocked hands line to the stdout writer without blocking. When the
// queue is full, log and progress lines are dropped; once a slot frees, or at
// the latest before the result, one events_dropped warning reports how many.
func (e *eventWriter) enqueueLocked(kind string, line []byte) {
	e.start.Do(e.startWriter)
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if e.dead {
		if !e.deadLogged {
			e.deadLogged = true
			e.log.Line("stream", "stdout failed, events continue in this log only: "+errString(e.deadErr))
		}
		return
	}
	if len(e.pending) >= streamQueueMax && (kind == "log" || kind == "progress") {
		e.dropped++
		return
	}
	if e.dropped > 0 && (len(e.pending) < streamQueueMax || kind == "result") {
		warning, _ := marshalLine(secrets.RedactValue(output.Event{
			Schema:  output.EventSchema,
			Type:    "warning",
			Code:    "events_dropped",
			Message: fmt.Sprintf("dropped %d log events from the stream because the client was not reading; the run log has them", e.dropped),
			TS:      time.Now().UTC().Format(time.RFC3339Nano),
		}))
		e.log.Line("event", string(warning[:len(warning)-1]))
		e.pending = append(e.pending, warning)
		e.dropped = 0
	}
	e.pending = append(e.pending, line)
	e.cond.Signal()
}

func (e *eventWriter) startWriter() {
	e.cond = sync.NewCond(&e.qmu)
	e.drained = make(chan struct{})
	go e.writeStdout()
}

// writeStdout copies queued lines to stdout until the queue is closed and
// empty. After the first write error it discards lines.
func (e *eventWriter) writeStdout() {
	defer close(e.drained)
	for {
		e.qmu.Lock()
		for len(e.pending) == 0 && !e.closed {
			e.cond.Wait()
		}
		batch := e.pending
		e.pending = nil
		dead := e.dead
		e.qmu.Unlock()
		if len(batch) == 0 {
			return
		}
		if dead {
			continue
		}
		for _, line := range batch {
			if err := e.writeLine(line); err != nil {
				e.qmu.Lock()
				e.dead, e.deadErr = true, err
				e.pending = nil
				e.qmu.Unlock()
				break
			}
		}
	}
}

func (e *eventWriter) writeLine(line []byte) error {
	if _, err := e.w.Write(line); err != nil {
		return err
	}
	if f, ok := e.w.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return err
		}
	}
	// Sync fails on pipes and sockets; only a real file cares.
	if f, ok := e.w.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
	return nil
}

// drainLocked closes the queue and waits up to streamDrainTimeout for the
// writer to finish. The result is already in the run log either way.
func (e *eventWriter) drainLocked() {
	e.start.Do(e.startWriter)
	e.qmu.Lock()
	e.closed = true
	e.cond.Signal()
	e.qmu.Unlock()
	timer := time.NewTimer(streamDrainTimeout)
	defer timer.Stop()
	select {
	case <-e.drained:
	case <-timer.C:
		e.log.Line("stream", "stdout did not drain in "+streamDrainTimeout.String()+"; the result is in this log")
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func marshalLine(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func (e *eventWriter) ok(command string, res Result) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return output.ExitGeneric
	}
	for _, w := range res.Warnings {
		e.writeLocked(output.Event{Type: "warning", Code: w.Code, Message: w.Message})
	}
	e.writeLocked(output.Event{Type: "result", Envelope: output.Envelope{
		Schema:   output.Schema,
		OK:       true,
		Command:  command,
		Data:     res.Data,
		Warnings: res.Warnings,
	}})
	e.done = true
	e.drainLocked()
	return output.ExitOK
}

func (e *eventWriter) fail(command string, err error) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.done {
		return output.ExitCode(err)
	}
	failure := output.Fail(nil, err)
	failure.Command = command
	e.writeLocked(output.Event{Type: "result", Envelope: failure})
	e.done = true
	e.drainLocked()
	code := output.ExitCode(err)
	if code == output.ExitOK {
		code = output.ExitGeneric
	}
	return code
}
