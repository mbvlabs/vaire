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
		"--property=ActiveState", "--property=Result", "--property=LoadState") //nolint:gosec // G204: managed service name.
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
		"-c", `SELECT pg_is_in_recovery()::text || '|' || CASE WHEN pg_is_in_recovery() THEN pg_get_wal_replay_pause_state() ELSE '' END`) //nolint:gosec // G204: fixed SQL and managed socket.
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
	cmd := exec.CommandContext(ctx, "runuser", "-u", "postgres", "--", pgBin+"/pg_ctl", "-D", data, "status") //nolint:gosec // G204: managed data directory.
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
	if !strings.HasSuffix(raw, "\n") {
		if i := strings.LastIndexByte(raw, '\n'); i >= 0 {
			raw = raw[:i+1]
		} else {
			raw = ""
		}
	}
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
		line := strings.TrimSuffix(chunk, "\n")
		position = end
		e.seenTo = end
		if strings.HasSuffix(line, "WARNING:  hot standby is not possible because of insufficient parameter settings") ||
			strings.HasSuffix(line, "FATAL:  recovery aborted because of insufficient parameter settings") {
			e.uncertain = true
		}
		prefix, body, ok := strings.Cut(line, "LOG:  ")
		if !ok {
			continue
		}
		pid, ok := strings.CutSuffix(prefix, ":")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(pid, 10, 32); err != nil || n == 0 {
			continue
		}
		if kind == TargetSet && body == "recovery stopping after reaching consistency" {
			e.stopSeen = true
			e.stopPID = pid
		}
		if kind == TargetTime && (strings.HasPrefix(body, "recovery stopping before commit of transaction ") ||
			strings.HasPrefix(body, "recovery stopping after commit of transaction ") ||
			strings.HasPrefix(body, "recovery stopping before abort of transaction ") ||
			strings.HasPrefix(body, "recovery stopping after abort of transaction ")) {
			e.stopSeen = true
			e.stopPID = pid
		}
		if e.stopSeen && pid == e.stopPID && body == "pausing at the end of recovery" {
			e.arrived = true
		}
	}
	return RecoveryRequirement{}, false
}

func waitRecovery(ctx context.Context, name, data string, port int, until, kind string, cursor *recoveryLogCursor, probes recoveryWaitProbes, log func(string)) error {
	deadline := time.Now().Add(recoveryWait)
	lastNote := time.Now()
	submittedAt := time.Now()
	var evidence recoveryEvidence
	everReady := false
	seenStarting := false
	seenProcess := false
	for {
		poll, cancel := context.WithTimeout(ctx, recoveryPollBudget)
		raw, err := cursor.readLimit(recoveryDiagnosticPollLimit / 2)
		if err != nil {
			cancel()
			return recoveryDiagnosticFailure(name)
		}
		if mismatch, ok := evidence.observe(raw, kind, cursor.offset-int64(len(raw))); ok {
			cancel()
			return recoveryMismatchFailure(name, mismatch)
		}
		service := probes.service(poll, name)
		observation := probes.sql(poll, port)
		process := probes.process(poll, data)
		raw, err = cursor.readLimit(recoveryDiagnosticPollLimit / 2)
		cancel()
		if err != nil {
			return recoveryDiagnosticFailure(name)
		}
		if mismatch, ok := evidence.observe(raw, kind, cursor.offset-int64(len(raw))); ok {
			return recoveryMismatchFailure(name, mismatch)
		}
		if observation == recoveryRecovering || observation == recoveryPaused || observation == recoveryPromoted {
			everReady = true
		}
		if service == serviceStarting || service == serviceActive {
			seenStarting = true
		}
		if process == recoveryRecovering {
			seenProcess = true
		}
		if observation == recoveryUnknown && !everReady && service == serviceStarting {
			observation = recoveryStarting
		}
		if !cursor.backlog && !cursor.partial &&
			(service == serviceFailed || (service == serviceInactive && (seenStarting || time.Since(submittedAt) >= recoverySubmissionGrace)) ||
				(process == recoveryStopped && (service == serviceActive || everReady || seenProcess))) {
			return recoveryStoppedFailure(name)
		}
		// A missing file, backlog or partial line makes negative diagnostic
		// evidence unknown. It cannot authorize readiness.
		trustworthy := cursor.initial != nil && cursor.observedNew && !cursor.backlog && !cursor.partial && !cursor.skipOldLine && !evidence.uncertain
		if trustworthy && (service == serviceActive || service == serviceStarting) {
			switch until {
			case UntilOpen:
				if observation == recoveryRecovering || observation == recoveryPromoted {
					return nil
				}
				if observation == recoveryPaused && evidence.arrived {
					return nil
				}
			case UntilPaused:
				if observation == recoveryPaused && evidence.arrived {
					return nil
				}
			case UntilPromoted:
				if observation == recoveryPromoted {
					return nil
				}
			}
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
