package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/secrets"
)

// Plan and skip reasons (§5.3).
const (
	ReasonNew             = "new"
	ReasonInputsChanged   = "inputs_changed"
	ReasonCheckIncomplete = "check_incomplete"
	ReasonPreviousFailure = "previous_failure"
	ReasonJournal         = "journal"
	ReasonCheck           = "check"
)

// Options steer one Plan or Apply.
type Options struct {
	// Only limits the run to these step IDs, kept in catalogue order.
	Only []string
	// JournalPath defaults to JournalPath.
	JournalPath string
	// Confirmed is true when the request carried the correct restart token.
	Confirmed bool
	// Restarter performs the aggregated restart. Nil in production M2.
	Restarter Restarter
	// Report receives step and log events.
	Report Reporter
}

func (o Options) journalPath() string {
	if o.JournalPath == "" {
		return JournalPath
	}
	return o.JournalPath
}

// PlanStep is one step in a plan.
type PlanStep struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Action string `json:"action"`
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// PlanData is the plan result, also returned by apply with dry_run.
type PlanData struct {
	Steps          []PlanStep `json:"steps"`
	WouldApply     int        `json:"would_apply"`
	Total          int        `json:"total"`
	RestartPending []string   `json:"restart_pending,omitempty"`
}

// ApplyStep is one step's outcome in an apply.
type ApplyStep struct {
	ID      string `json:"id"`
	Index   int    `json:"index"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Restart string `json:"restart,omitempty"`
}

// ApplyData is the apply result.
type ApplyData struct {
	Steps          []ApplyStep `json:"steps"`
	Applied        int         `json:"applied"`
	Skipped        int         `json:"skipped"`
	Total          int         `json:"total"`
	RestartPending []string    `json:"restart_pending,omitempty"`
	Restarted      bool        `json:"restarted"`
}

type decision struct {
	apply  bool
	reason string
	hash   string
	detail string
}

// Plan runs the runner's decision for every selected step and writes nothing.
func Plan(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (PlanData, error) {
	selected, err := selectSteps(steps, opts.Only)
	if err != nil {
		return PlanData{}, err
	}
	j, err := LoadJournal(opts.journalPath())
	if err != nil {
		return PlanData{}, err
	}
	data := PlanData{Steps: make([]PlanStep, 0, len(selected)), Total: len(selected)}
	for i, step := range selected {
		d, err := decide(ctx, step, cfg, rt, j)
		if err != nil {
			return data, stepFailed(step.ID(), i+1, err)
		}
		action := "skip"
		if d.apply {
			action = "apply"
			data.WouldApply++
		}
		data.Steps = append(data.Steps, PlanStep{ID: step.ID(), Index: i + 1, Action: action, Reason: d.reason, Detail: d.detail})
	}
	data.RestartPending = pendingIn(j, steps)
	return data, nil
}

// Apply converges the selected steps in catalogue order (§5.2). The first
// failure stops the run; the next apply resumes there because earlier steps
// skip on the journal.
func Apply(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (ApplyData, []output.Warning, error) {
	if rt.DryRun {
		return ApplyData{}, nil, errors.New("engine.Apply called with DryRun; use Plan")
	}
	selected, err := selectSteps(steps, opts.Only)
	if err != nil {
		return ApplyData{}, nil, err
	}
	path := opts.journalPath()
	j, err := LoadJournal(path)
	if err != nil {
		return ApplyData{}, nil, err
	}
	report := opts.Report
	total := len(selected)
	data := ApplyData{Steps: make([]ApplyStep, 0, total), Total: total}
	for i, step := range selected {
		id := step.ID()
		index := i + 1
		d, err := decide(ctx, step, cfg, rt, j)
		if err != nil {
			return data, nil, failStep(j, path, report, id, index, total, decision{}, err)
		}
		if !d.apply {
			if d.reason == ReasonCheck {
				entry := j.Steps[id]
				entry.Status = StatusCompleted
				entry.InputsHash = d.hash
				entry.CompletedAt = now()
				entry.Error = ""
				j.Steps[id] = entry
				if err := j.Save(path); err != nil {
					return data, nil, err
				}
			}
			report.emit(output.Event{Type: "step", Step: id, State: "skipped", Index: intPtr(index), Total: intPtr(total), Detail: d.reason})
			data.Steps = append(data.Steps, ApplyStep{ID: id, Index: index, State: "skipped", Reason: d.reason})
			data.Skipped++
			continue
		}
		report.emit(output.Event{Type: "step", Step: id, State: "started", Index: intPtr(index), Total: intPtr(total), Detail: step.Describe(cfg)})
		j.Steps[id] = JournalEntry{Status: StatusRunning, InputsHash: d.hash, StartedAt: now()}
		if err := j.Save(path); err != nil {
			return data, nil, err
		}
		res, err := step.Apply(ctx, cfg, rt, report)
		if err == nil && ctx.Err() != nil {
			err = ErrInterrupted
		}
		if err != nil {
			if ctx.Err() != nil {
				err = ErrInterrupted
			}
			return data, nil, failStep(j, path, report, id, index, total, d, err)
		}
		restart := res.Restart
		if restart == "" {
			restart = RestartNone
		}
		entry := j.Steps[id]
		entry.Status = StatusCompleted
		entry.CompletedAt = now()
		entry.Restart = string(restart)
		entry.Error = ""
		j.Steps[id] = entry
		if restart == RestartRestart {
			j.addPending(id)
		}
		if err := j.Save(path); err != nil {
			return data, nil, err
		}
		report.emit(output.Event{Type: "step", Step: id, State: "completed", Index: intPtr(index), Total: intPtr(total), Detail: strings.Join(res.Notes, "; ")})
		data.Steps = append(data.Steps, ApplyStep{ID: id, Index: index, State: "completed", Restart: string(restart)})
		data.Applied++
	}

	var warnings []output.Warning
	pending := pendingIn(j, steps)
	if len(pending) == 0 {
		return data, nil, nil
	}
	if opts.Restarter == nil {
		return data, nil, output.NewError(
			output.CodeError,
			"a step needs a restart but this build has no restarter",
			output.ExitGeneric,
			"This is an eru bug. Report it with the run log.",
		).WithDetails(map[string]any{"restart_pending": pending})
	}
	if !opts.Confirmed {
		data.RestartPending = pending
		warnings = append(warnings, output.Warning{
			Code:    "restart_pending",
			Message: "a restart is pending for " + strings.Join(pending, ", ") + "; re-run apply with the restart confirm token",
		})
		return data, warnings, nil
	}
	if err := opts.Restarter.Restart(ctx); err != nil {
		return data, nil, output.WrapError(output.CodeStepFailed, fmt.Errorf("restart failed: %s", secrets.Redact(err.Error())), output.ExitStep,
			"Inspect the run log, then re-run apply with the confirm token.").
			WithDetails(map[string]any{"step": "restart", "error": secrets.Redact(err.Error()), "restart_pending": pending})
	}
	j.RestartPending = nil
	if err := j.Save(path); err != nil {
		return data, nil, err
	}
	data.Restarted = true
	return data, warnings, nil
}

func decide(ctx context.Context, step Step, cfg config.Cluster, rt Runtime, j *Journal) (decision, error) {
	id := step.ID()
	inputs, err := step.Inputs(cfg, rt)
	if err != nil {
		return decision{}, fmt.Errorf("inputs: %w", err)
	}
	hash := InputsHash(id, inputs)
	entry, ok := j.Steps[id]
	if ok && entry.Status == StatusCompleted && entry.InputsHash == hash {
		return decision{reason: ReasonJournal, hash: hash}, nil
	}
	if ok && entry.InputsHash == hash {
		check, err := step.Check(ctx, cfg, rt)
		if err != nil {
			return decision{}, fmt.Errorf("check: %w", err)
		}
		if check.Complete {
			return decision{reason: ReasonCheck, hash: hash, detail: check.Detail}, nil
		}
	}
	d := decision{apply: true, hash: hash}
	switch {
	case !ok:
		d.reason = ReasonNew
	case entry.Status == StatusFailed || entry.Status == StatusRunning:
		d.reason = ReasonPreviousFailure
	case entry.InputsHash != hash:
		d.reason = ReasonInputsChanged
	default:
		d.reason = ReasonCheckIncomplete
	}
	return d, nil
}

// InputsHash is sha256(ID || 0x00 || inputs) in hex.
func InputsHash(id string, inputs []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(inputs)
	return hex.EncodeToString(h.Sum(nil))
}

func failStep(j *Journal, path string, report Reporter, id string, index, total int, d decision, cause error) error {
	msg := secrets.Redact(cause.Error())
	entry := j.Steps[id]
	entry.Status = StatusFailed
	if d.hash != "" {
		entry.InputsHash = d.hash
	}
	entry.Error = msg
	j.Steps[id] = entry
	saveErr := j.Save(path)
	report.emit(output.Event{Type: "step", Step: id, State: "failed", Index: intPtr(index), Total: intPtr(total), Detail: msg})
	err := stepFailed(id, index, cause)
	if saveErr != nil {
		err.Cause = errors.Join(cause, saveErr)
	}
	return err
}

func stepFailed(id string, index int, cause error) *output.CLIError {
	msg := secrets.Redact(cause.Error())
	return &output.CLIError{
		Code:     output.CodeStepFailed,
		Message:  fmt.Sprintf("step %s failed: %s", id, msg),
		Hint:     "Fix the cause and re-run apply. It resumes at this step.",
		ExitCode: output.ExitStep,
		Details:  map[string]any{"step": id, "error": msg, "index": index},
		Cause:    cause,
	}
}

func selectSteps(steps []Step, only []string) ([]Step, error) {
	if err := ValidateIDs(steps); err != nil {
		return nil, output.WrapError(output.CodeError, err, output.ExitGeneric, "This is an eru bug in the step catalogue.")
	}
	if len(only) == 0 {
		return steps, nil
	}
	valid := make([]string, 0, len(steps))
	known := map[string]bool{}
	for _, step := range steps {
		valid = append(valid, step.ID())
		known[step.ID()] = true
	}
	want := map[string]bool{}
	var unknown []string
	for _, id := range only {
		if !known[id] {
			unknown = append(unknown, id)
		}
		want[id] = true
	}
	if len(unknown) > 0 {
		return nil, output.NewError(
			output.CodeValidation,
			"unknown step ID "+strings.Join(unknown, ", "),
			output.ExitConfig,
			"Pick step IDs from details.valid.",
		).WithDetails(map[string]any{"unknown": unknown, "valid": valid})
	}
	out := make([]Step, 0, len(want))
	for _, step := range steps {
		if want[step.ID()] {
			out = append(out, step)
		}
	}
	return out, nil
}

// pendingIn returns the journal's pending restarts that still belong to the
// catalogue.
func pendingIn(j *Journal, steps []Step) []string {
	known := map[string]bool{}
	for _, step := range steps {
		known[step.ID()] = true
	}
	var out []string
	for _, id := range j.RestartPending {
		if known[id] {
			out = append(out, id)
		}
	}
	return out
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
