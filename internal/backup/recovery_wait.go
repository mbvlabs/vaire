package backup

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

const recoveryPollBudget = 2 * time.Second
const recoveryPollInterval = time.Second
const recoverySubmissionGrace = 5 * time.Second

type recoveryObservation int

const (
	recoveryUnknown recoveryObservation = iota
	recoveryStarting
	recoveryRecovering
	recoveryPaused
	recoveryPromoted
	recoveryStopped
)

type recoveryServiceState int

const (
	serviceUnknown recoveryServiceState = iota
	serviceStarting
	serviceActive
	serviceInactive
	serviceFailed
)

type recoveryWaitProbes struct {
	service func(context.Context, string) recoveryServiceState
	sql     func(context.Context, int) recoveryObservation
	process func(context.Context, string) recoveryObservation
}

var recoveryProbes = recoveryWaitProbes{service: recoveryService, sql: recoverySQL, process: recoveryProcess}

var submitRecoveryService = func(ctx context.Context, name string) error {
	ctx, cancel := context.WithTimeout(ctx, recoveryPollBudget)
	defer cancel()
	// systemctl submits a job and returns; pg_ctlcluster's own readiness wait
	// runs inside the unit, not in this process.
	return recoveryStartCommand(ctx, name).Run()
}

func recoveryStartCommand(ctx context.Context, name string) *exec.Cmd {
	return exec.CommandContext(ctx, "systemctl", "start", "--no-block", "postgresql@18-"+name) //nolint:gosec // G204: validated managed cluster name.
}

func recoveryService(ctx context.Context, name string) recoveryServiceState {
	cmd := exec.CommandContext(ctx, "systemctl", "show", "postgresql@18-"+name, //nolint:gosec // G204: managed service name.
		"--property=ActiveState", "--property=Result", "--property=LoadState")
	var out limitedProbeBuffer
	cmd.Stdout = &out
	cmd.WaitDelay = 100 * time.Millisecond
	if cmd.Run() != nil {
		return serviceUnknown
	}
	var active, result, loaded string
	for _, line := range strings.Split(out.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok && key == "ActiveState" {
			active = value
		}
		if ok && key == "Result" {
			result = value
		}
		if ok && key == "LoadState" {
			loaded = value
		}
	}
	if loaded == "not-found" || loaded == "error" || loaded == "masked" || loaded == "bad-setting" {
		return serviceFailed
	}
	if loaded != "loaded" {
		return serviceUnknown
	}
	switch active {
	case "active":
		return serviceActive
	case "activating", "reloading":
		return serviceStarting
	case "failed":
		return serviceFailed
	case "inactive":
		if result != "" && result != "success" {
			return serviceFailed
		}
		return serviceInactive
	}
	return serviceUnknown
}

func recoverySQL(ctx context.Context, port int) recoveryObservation {
	cmd := exec.CommandContext(ctx, "runuser", "-u", "postgres", "--", "psql", "-X", "-q", "-t", "-A", //nolint:gosec // G204: fixed SQL and managed socket.
		"-v", "ON_ERROR_STOP=1", "-h", pgSocketDir, "-p", strconv.Itoa(port), "-d", "postgres",
		"-c", `SELECT pg_is_in_recovery()::text || '|' || CASE WHEN pg_is_in_recovery() THEN pg_get_wal_replay_pause_state() ELSE '' END`)
	var out limitedProbeBuffer
	cmd.Stdout = &out
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return recoveryUnknown
	}
	recovery, pause, ok := strings.Cut(strings.TrimSpace(out.String()), "|")
	if !ok {
		return recoveryUnknown
	}
	switch recovery {
	case "false":
		return recoveryPromoted
	case "true":
		switch pause {
		case "paused":
			return recoveryPaused
		case "not paused", "pause requested":
			return recoveryRecovering
		}
	}
	return recoveryUnknown
}

func recoveryProcess(ctx context.Context, data string) recoveryObservation {
	cmd := exec.CommandContext(ctx, "runuser", "-u", "postgres", "--", pgBin+"/pg_ctl", "-D", data, "status")
	err := cmd.Run()
	if err == nil {
		return recoveryRecovering // alive; SQL establishes actual recovery state.
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 3 {
		return recoveryStopped
	}
	return recoveryUnknown
}

func recoveryDiagnosticFailure(name string) error {
	return output.NewError(output.CodeExternalCommand, "recovery diagnostics are unavailable for "+name, output.ExitExternal,
		"Inspect the temporary cluster and its log.").
		WithDetails(map[string]any{"reason": "recovery_diagnostics_unavailable", "target": name, "stage": "recovery"})
}

func recoveryStoppedFailure(name string) error {
	return output.NewError(output.CodeExternalCommand, "the cluster "+name+" stopped during recovery", output.ExitExternal,
		"Inspect the temporary cluster and its log.").
		WithDetails(map[string]any{"reason": "recovery_stopped", "target": name, "stage": "recovery"})
}

func (m *Materialised) startRecovery(ctx context.Context, until, kind string) error {
	cursor, err := newRecoveryLogCursor("/var/log/postgresql/postgresql-18-" + m.Name + ".log")
	if err != nil {
		return recoveryDiagnosticFailure(m.Name)
	}
	if err := submitRecoveryService(ctx, m.Name); err != nil {
		return output.NewError(output.CodeExternalCommand, "could not submit temporary cluster start", output.ExitExternal,
			"Inspect the temporary cluster service.").WithDetails(map[string]any{"reason": "recovery_start_failed", "target": m.Name, "stage": "start"})
	}
	return waitRecovery(ctx, m.Name, m.Data, m.Port, until, kind, cursor, recoveryProbes, m.log)
}

type recoveryEvidence struct {
	stopSeen  bool
	arrived   bool
	seenTo    int64
	stopPID   string
	uncertain bool
}

func (e *recoveryEvidence) observe(raw, kind string, base int64) (RecoveryRequirement, bool) {
	raw = completeLogLines(raw)
	if mismatch, ok := recoveryMismatch(raw); ok {
		return mismatch, true
	}
	position := base
	for _, chunk := range strings.SplitAfter(raw, "\n") {
		end := position + int64(len(chunk))
		if end <= e.seenTo || !strings.HasSuffix(chunk, "\n") {
			position = end
			continue
		}
		e.seenTo = end
		position = end
		e.observeLine(strings.TrimSuffix(chunk, "\n"), kind)
	}
	return RecoveryRequirement{}, false
}

// completeLogLines drops an incomplete last line, so evidence is only taken
// from whole log lines.
func completeLogLines(raw string) string {
	if strings.HasSuffix(raw, "\n") {
		return raw
	}
	if i := strings.LastIndexByte(raw, '\n'); i >= 0 {
		return raw[:i+1]
	}
	return ""
}

// observeLine records the evidence of one complete log line.
func (e *recoveryEvidence) observeLine(line, kind string) {
	if strings.HasSuffix(line, "WARNING:  hot standby is not possible because of insufficient parameter settings") ||
		strings.HasSuffix(line, "FATAL:  recovery aborted because of insufficient parameter settings") {
		e.uncertain = true
	}
	prefix, body, ok := strings.Cut(line, "LOG:  ")
	if !ok {
		return
	}
	pid, ok := strings.CutSuffix(prefix, ":")
	if !ok {
		return
	}
	if n, err := strconv.ParseUint(pid, 10, 32); err != nil || n == 0 {
		return
	}
	if recoveryStoppedAtTarget(kind, body) {
		e.stopSeen = true
		e.stopPID = pid
	}
	if e.stopSeen && pid == e.stopPID && body == "pausing at the end of recovery" {
		e.arrived = true
	}
}

// recoveryStoppedAtTarget reports whether a LOG body is recovery's stop at the
// requested target kind.
func recoveryStoppedAtTarget(kind, body string) bool {
	switch kind {
	case TargetSet:
		return body == "recovery stopping after reaching consistency"
	case TargetTime:
		return strings.HasPrefix(body, "recovery stopping before commit of transaction ") ||
			strings.HasPrefix(body, "recovery stopping after commit of transaction ") ||
			strings.HasPrefix(body, "recovery stopping before abort of transaction ") ||
			strings.HasPrefix(body, "recovery stopping after abort of transaction ")
	}
	return false
}

// recoveryPoll is one round of recovery probes.
type recoveryPoll struct {
	service     recoveryServiceState
	observation recoveryObservation
	process     recoveryObservation
}

// recoveryWaitState accumulates what repeated polls of one cluster show: the
// evidence its log supplied and how far the service and its process have come.
type recoveryWaitState struct {
	name         string
	data         string
	port         int
	until        string
	kind         string
	cursor       *recoveryLogCursor
	probes       recoveryWaitProbes
	submittedAt  time.Time
	evidence     recoveryEvidence
	everReady    bool
	seenStarting bool
	seenProcess  bool
}

func waitRecovery(ctx context.Context, name, data string, port int, until, kind string, cursor *recoveryLogCursor, probes recoveryWaitProbes, log func(string)) error {
	state := &recoveryWaitState{
		name: name, data: data, port: port, until: until, kind: kind,
		cursor: cursor, probes: probes, submittedAt: time.Now(),
	}
	deadline := time.Now().Add(recoveryWait)
	lastNote := time.Now()
	for {
		poll, err := state.poll(ctx)
		if err != nil {
			return err
		}
		if state.stopped(poll) {
			return recoveryStoppedFailure(name)
		}
		if state.reached(poll) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("the cluster " + name + " did not reach its target within 24 hours")
		}
		if time.Since(lastNote) > time.Minute {
			lastNote = time.Now()
			log("recovery is still replaying WAL")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(recoveryPollInterval):
		}
	}
}

// poll runs one recovery diagnostics round: it reads the log before and after
// the probes, because the probes may write the evidence that decides the next
// round, and records what the observations add up to.
func (s *recoveryWaitState) poll(ctx context.Context) (recoveryPoll, error) {
	var poll recoveryPoll
	pollCtx, cancel := context.WithTimeout(ctx, recoveryPollBudget)
	defer cancel()
	if err := s.observeLog(); err != nil {
		return poll, err
	}
	poll.service = s.probes.service(pollCtx, s.name)
	poll.observation = s.probes.sql(pollCtx, s.port)
	poll.process = s.probes.process(pollCtx, s.data)
	if err := s.observeLog(); err != nil {
		return poll, err
	}
	s.record(&poll)
	return poll, nil
}

// observeLog takes the evidence of what was appended since the last read. A
// read that cannot establish what the log says is a diagnostics failure, and a
// mismatch is final: recovery cannot reach a different target.
func (s *recoveryWaitState) observeLog() error {
	raw, err := s.cursor.readLimit(recoveryDiagnosticPollLimit / 2)
	if err != nil {
		return recoveryDiagnosticFailure(s.name)
	}
	if mismatch, ok := s.evidence.observe(raw, s.kind, s.cursor.offset-int64(len(raw))); ok {
		return recoveryMismatchFailure(s.name, mismatch)
	}
	return nil
}

// record adds one poll's observations to the state the wait decisions read,
// and reports an unknown SQL observation as recovery starting while the
// service is still coming up.
func (s *recoveryWaitState) record(poll *recoveryPoll) {
	if poll.observation == recoveryRecovering || poll.observation == recoveryPaused || poll.observation == recoveryPromoted {
		s.everReady = true
	}
	if poll.service == serviceStarting || poll.service == serviceActive {
		s.seenStarting = true
	}
	if poll.process == recoveryRecovering {
		s.seenProcess = true
	}
	if poll.observation == recoveryUnknown && !s.everReady && poll.service == serviceStarting {
		poll.observation = recoveryStarting
	}
}

// stopped reports diagnostic evidence that the cluster stopped before it
// reached its target. A missing file, backlog or partial line makes negative
// diagnostic evidence unknown; it cannot authorize a stop.
func (s *recoveryWaitState) stopped(poll recoveryPoll) bool {
	if s.cursor.backlog || s.cursor.partial {
		return false
	}
	if poll.service == serviceFailed {
		return true
	}
	if poll.service == serviceInactive && (s.seenStarting || time.Since(s.submittedAt) >= recoverySubmissionGrace) {
		return true
	}
	return poll.process == recoveryStopped && (poll.service == serviceActive || s.everReady || s.seenProcess)
}

// reached reports a target that trustworthy evidence shows recovery has
// reached. A missing file, backlog or partial line makes negative diagnostic
// evidence unknown; it cannot authorize readiness.
func (s *recoveryWaitState) reached(poll recoveryPoll) bool {
	trustworthy := s.cursor.initial != nil && s.cursor.observedNew && !s.cursor.backlog && !s.cursor.partial && !s.cursor.skipOldLine && !s.evidence.uncertain
	if !trustworthy || (poll.service != serviceActive && poll.service != serviceStarting) {
		return false
	}
	switch s.until {
	case UntilOpen:
		return poll.observation == recoveryRecovering || poll.observation == recoveryPromoted ||
			poll.observation == recoveryPaused && s.evidence.arrived
	case UntilPaused:
		return poll.observation == recoveryPaused && s.evidence.arrived
	case UntilPromoted:
		return poll.observation == recoveryPromoted
	}
	return false
}
