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
	"github.com/mbvlabs/eru/internal/render"
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
	// Only limits the run to these step IDs, kept in catalogue order. A
	// template's ID selects every instance; <id>@<cluster> selects one.
	Only []string
	// DB limits the run to one cluster's instances; server-level steps are
	// left out, and only that cluster's pending restarts are considered.
	DB string
	// JournalPath defaults to JournalPath.
	JournalPath string
	// Confirm is what the request's restart tokens allow.
	Confirm Confirmation
	// Restarter restarts one cluster for its pending restarts.
	Restarter Restarter
	// Report receives step and log events.
	Report Reporter
	// FailureHint replaces the step_failed hint, for callers that resume
	// differently from apply (server init uses --resume).
	FailureHint string
}

func (o Options) journalPath() string {
	if o.JournalPath == "" {
		return JournalPath
	}
	return o.JournalPath
}

// Confirmation is what the restart confirm tokens of a request allow:
// <server>/restart restarts every cluster with a pending restart, one at a
// time, and <cluster>/restart that cluster only.
type Confirmation struct {
	Server   bool
	Clusters []string
}

// Covers reports whether cluster's restart is confirmed.
func (c Confirmation) Covers(cluster string) bool {
	if c.Server {
		return true
	}
	for _, name := range c.Clusters {
		if name == cluster {
			return true
		}
	}
	return false
}

// RestartTokens are the confirm tokens that clear pending, in order: one
// <cluster>/restart per cluster, and <server>/restart for a server-level
// step's restart.
func RestartTokens(server string, pending []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, id := range pending {
		_, cluster := SplitID(id)
		token := server + "/restart"
		if cluster != "" {
			token = cluster + "/restart"
		}
		if !seen[token] {
			seen[token] = true
			out = append(out, token)
		}
	}
	return out
}

// PlanStep is one step in a plan. Cluster names the cluster of an instance.
type PlanStep struct {
	ID      string `json:"id"`
	Cluster string `json:"cluster,omitempty"`
	Index   int    `json:"index"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
	Detail  string `json:"detail,omitempty"`
	// Diffs are unified diffs of the managed files the step would rewrite.
	Diffs []FileDiff `json:"diffs,omitempty"`
}

// FileDiff is one managed file's change, with secrets redacted.
type FileDiff struct {
	Path string `json:"path"`
	Diff string `json:"diff"`
}

// PlanData is the plan result, also returned by apply with dry_run.
type PlanData struct {
	Steps          []PlanStep `json:"steps"`
	WouldApply     int        `json:"would_apply"`
	Total          int        `json:"total"`
	RestartPending []string   `json:"restart_pending,omitempty"`
	// ConfirmTokens are the tokens that perform the pending restarts.
	ConfirmTokens []string `json:"confirm_tokens,omitempty"`
}

// ApplyStep is one step's outcome in an apply.
type ApplyStep struct {
	ID      string `json:"id"`
	Cluster string `json:"cluster,omitempty"`
	Index   int    `json:"index"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Restart string `json:"restart,omitempty"`
}

// ClusterRestart is one cluster restarted by an apply, with how long it was
// unavailable.
type ClusterRestart struct {
	Cluster    string `json:"cluster"`
	DowntimeMS int64  `json:"downtime_ms"`
}

// ApplyData is the apply result.
type ApplyData struct {
	Steps          []ApplyStep `json:"steps"`
	Applied        int         `json:"applied"`
	Skipped        int         `json:"skipped"`
	Total          int         `json:"total"`
	RestartPending []string    `json:"restart_pending,omitempty"`
	// ConfirmTokens are the tokens that perform the restarts still pending.
	ConfirmTokens []string `json:"confirm_tokens,omitempty"`
	// Restarts are the clusters restarted, in order.
	Restarts []ClusterRestart `json:"restarts,omitempty"`
}

// Restarted reports whether the apply restarted any cluster.
func (d ApplyData) Restarted() bool { return len(d.Restarts) > 0 }

type decision struct {
	apply  bool
	reason string
	hash   string
	detail string
}

// Plan runs the runner's decision for every selected step and writes nothing.
func Plan(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (PlanData, error) {
	all, selected, err := prepare(steps, cfg, opts)
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
		_, cluster := SplitID(step.ID())
		ps := PlanStep{ID: step.ID(), Cluster: cluster, Index: i + 1, Action: action, Reason: d.reason, Detail: d.detail}
		if r, ok := step.(FileRenderer); ok && d.apply {
			for _, f := range r.RenderedFiles(cfg, rt) {
				if diff := render.Diff(f.Path, f.Content); diff != "" {
					ps.Diffs = append(ps.Diffs, FileDiff{Path: f.Path, Diff: secrets.Redact(diff)})
				}
			}
		}
		data.Steps = append(data.Steps, ps)
	}
	data.RestartPending = pendingIn(j, all, opts.DB)
	data.ConfirmTokens = RestartTokens(cfg.Name(), data.RestartPending)
	return data, nil
}

// Apply converges the selected steps in catalogue order (§5.2). The first
// failure stops the run; the next apply resumes there because earlier steps
// skip on the journal.
func Apply(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (ApplyData, []output.Warning, error) {
	if rt.DryRun {
		return ApplyData{}, nil, errors.New("engine.Apply called with DryRun; use Plan")
	}
	all, selected, err := prepare(steps, cfg, opts)
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
	var warnings []output.Warning
	for i, step := range selected {
		id := step.ID()
		_, cluster := SplitID(id)
		index := i + 1
		d, err := decide(ctx, step, cfg, rt, j)
		if err != nil {
			return data, nil, failStep(j, path, opts, id, index, total, decision{}, err)
		}
		if !d.apply {
			if d.reason == ReasonCheck {
				entry := j.Steps[id]
				entry.Status = StatusCompleted
				entry.InputsHash = d.hash
				entry.CompletedAt = now()
				entry.Error = ""
				if err := j.setEntry(path, id, entry); err != nil {
					return data, nil, err
				}
			}
			report.emit(output.Event{Type: "step", Step: id, State: "skipped", Index: intPtr(index), Total: intPtr(total), Detail: d.reason})
			data.Steps = append(data.Steps, ApplyStep{ID: id, Cluster: cluster, Index: index, State: "skipped", Reason: d.reason})
			data.Skipped++
			continue
		}
		report.emit(output.Event{Type: "step", Step: id, State: "started", Index: intPtr(index), Total: intPtr(total), Detail: step.Describe(cfg)})
		if err := j.setEntry(path, id, JournalEntry{Status: StatusRunning, InputsHash: d.hash, StartedAt: now()}); err != nil {
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
			return data, nil, failStep(j, path, opts, id, index, total, d, err)
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
		if err := j.update(path, func(j *Journal) {
			j.Steps[id] = entry
			if restart == RestartRestart {
				j.addPending(id)
			}
		}); err != nil {
			return data, nil, err
		}
		report.emit(output.Event{Type: "step", Step: id, State: "completed", Index: intPtr(index), Total: intPtr(total), Detail: strings.Join(res.Notes, "; ")})
		warnings = append(warnings, res.Warnings...)
		data.Steps = append(data.Steps, ApplyStep{ID: id, Cluster: cluster, Index: index, State: "completed", Restart: string(restart)})
		data.Applied++
	}

	pending := pendingIn(j, all, opts.DB)
	if len(pending) == 0 {
		return data, warnings, nil
	}
	if opts.Restarter == nil {
		return data, nil, output.NewError(
			output.CodeError,
			"a step needs a restart but this build has no restarter",
			output.ExitGeneric,
			"This is an eru bug. Report it with the run log.",
		).WithDetails(map[string]any{"restart_pending": pending})
	}
	restarts, err := restartConfirmed(ctx, j, path, opts, cfg, pending)
	data.Restarts = append(data.Restarts, restarts...)
	if err != nil {
		return data, nil, err
	}
	if left := pendingIn(j, all, opts.DB); len(left) > 0 {
		data.RestartPending = left
		data.ConfirmTokens = RestartTokens(cfg.Name(), left)
		warnings = append(warnings, output.Warning{
			Code: "restart_pending",
			Message: "a restart is pending for " + strings.Join(left, ", ") + "; re-run apply with --confirm " +
				strings.Join(data.ConfirmTokens, " --confirm "),
		})
	}
	return data, warnings, nil
}

// restartConfirmed restarts, one at a time and in [[clusters]] order, every
// cluster with a pending restart that opts.Confirm covers, and clears what
// each restart settled. A server-level step's restart needs <server>/restart
// and restarts every cluster. The first failure stops, leaving the rest
// pending.
func restartConfirmed(ctx context.Context, j *Journal, path string, opts Options, cfg config.Cluster, pending []string) ([]ClusterRestart, error) {
	byCluster := map[string][]string{}
	var serverIDs []string
	for _, id := range pending {
		if _, cluster := SplitID(id); cluster != "" {
			byCluster[cluster] = append(byCluster[cluster], id)
		} else {
			serverIDs = append(serverIDs, id)
		}
	}
	all := len(serverIDs) > 0 && opts.Confirm.Server
	var out []ClusterRestart
	for _, c := range cfg.Clusters {
		ids, needed := byCluster[c.Name]
		if (!needed || !opts.Confirm.Covers(c.Name)) && !all {
			continue
		}
		if opts.DB != "" && c.Name != opts.DB {
			continue
		}
		downtime, err := opts.Restarter.Restart(ctx, c.Name)
		if err != nil {
			msg := secrets.Redact(err.Error())
			return out, output.WrapError(output.CodeStepFailed, fmt.Errorf("restart of %s failed: %s", c.Name, msg), output.ExitStep,
				"Inspect the run log, then re-run apply with the confirm token.").
				WithDetails(map[string]any{"step": "restart", "cluster": c.Name, "error": msg, "restart_pending": pending})
		}
		out = append(out, ClusterRestart{Cluster: c.Name, DowntimeMS: downtime.Milliseconds()})
		if err := j.clearPending(path, ids); err != nil {
			return out, err
		}
	}
	if all {
		if err := j.clearPending(path, serverIDs); err != nil {
			return out, err
		}
	}
	return out, nil
}

func decide(ctx context.Context, step Step, cfg config.Cluster, rt Runtime, j *Journal) (decision, error) {
	id := step.ID()
	inputs, err := step.Inputs(cfg, rt)
	if err != nil {
		return decision{}, fmt.Errorf("inputs: %w", err)
	}
	hash := InputsHash(id, inputs)
	if sk, ok := step.(Skipper); ok {
		if reason := sk.SkipReason(cfg); reason != "" {
			return decision{reason: reason, hash: hash}, nil
		}
	}
	if f, ok := step.(Forcer); ok {
		if reason := f.Force(cfg, rt); reason != "" {
			return decision{apply: true, reason: reason, hash: hash}, nil
		}
	}
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

func failStep(j *Journal, path string, opts Options, id string, index, total int, d decision, cause error) error {
	msg := secrets.Redact(cause.Error())
	entry := j.Steps[id]
	entry.Status = StatusFailed
	if d.hash != "" {
		entry.InputsHash = d.hash
	}
	entry.Error = msg
	saveErr := j.setEntry(path, id, entry)
	opts.Report.emit(output.Event{Type: "step", Step: id, State: "failed", Index: intPtr(index), Total: intPtr(total), Detail: msg})
	err := stepFailed(id, index, cause)
	if opts.FailureHint != "" {
		err.Hint = opts.FailureHint
	}
	if saveErr != nil {
		err.Cause = errors.Join(cause, saveErr)
	}
	return err
}

func stepFailed(id string, index int, cause error) *output.CLIError {
	msg := secrets.Redact(cause.Error())
	details := map[string]any{"step": id, "error": msg, "index": index}
	var scriptErr *ScriptError
	if errors.As(cause, &scriptErr) && len(scriptErr.Tail) > 0 {
		details["output_tail"] = scriptErr.Tail
	}
	out := &output.CLIError{
		Code:     output.CodeStepFailed,
		Message:  fmt.Sprintf("step %s failed: %s", id, msg),
		Hint:     "Fix the cause and re-run apply. It resumes at this step.",
		ExitCode: output.ExitStep,
		Details:  details,
		Cause:    cause,
	}
	// A step that refuses for a reason the operator has to act on keeps its
	// code, hint and reason, so agents can tell it from a failed script.
	var cliErr *output.CLIError
	if errors.As(cause, &cliErr) && (cliErr.Code == output.CodePrecondition || cliErr.Code == output.CodeInputRequired) {
		out.Code, out.ExitCode = cliErr.Code, cliErr.ExitCode
		if cliErr.Hint != "" {
			out.Hint = cliErr.Hint
		}
		if extra, ok := cliErr.Details.(map[string]any); ok {
			for k, v := range extra {
				if _, taken := details[k]; !taken {
					details[k] = v
				}
			}
		}
	}
	return out
}

// prepare validates the catalogue, expands its templates for cfg and picks
// the steps opts selects. all is the expanded catalogue.
func prepare(steps []Step, cfg config.Cluster, opts Options) (all, selected []Step, err error) {
	if err := ValidateIDs(steps); err != nil {
		return nil, nil, output.WrapError(output.CodeError, err, output.ExitGeneric, "This is an eru bug in the step catalogue.")
	}
	all = Expand(steps, cfg)
	if opts.DB != "" {
		if _, ok := cfg.ClusterByName(opts.DB); !ok {
			names := make([]string, 0, len(cfg.Clusters))
			for _, c := range cfg.Clusters {
				names = append(names, c.Name)
			}
			return nil, nil, output.NewError(
				output.CodeValidation,
				"database "+opts.DB+" is not in cluster.toml",
				output.ExitConfig,
				"Pick a database from details.databases.",
			).WithDetails(map[string]any{"database": opts.DB, "databases": names})
		}
	}
	selected, err = selectSteps(steps, all, opts.Only)
	if err != nil {
		return nil, nil, err
	}
	if opts.DB != "" {
		kept := selected[:0:0]
		for _, step := range selected {
			if _, cluster := SplitID(step.ID()); cluster == opts.DB {
				kept = append(kept, step)
			}
		}
		selected = kept
	}
	return all, selected, nil
}

// selectSteps picks the steps of all that only names. A template's ID
// selects every instance.
func selectSteps(catalog, all []Step, only []string) ([]Step, error) {
	if len(only) == 0 {
		return all, nil
	}
	var valid []string
	known := map[string]bool{}
	for _, step := range catalog {
		if _, ok := step.(Template); ok {
			valid = append(valid, step.ID())
			known[step.ID()] = true
		}
	}
	for _, step := range all {
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
	out := make([]Step, 0, len(all))
	for _, step := range all {
		template, _ := SplitID(step.ID())
		if want[step.ID()] || want[template] {
			out = append(out, step)
		}
	}
	return out, nil
}

// pendingIn returns the journal's pending restarts that still belong to the
// expanded catalogue, limited to db's instances when db is set.
func pendingIn(j *Journal, steps []Step, db string) []string {
	known := map[string]bool{}
	for _, step := range steps {
		known[step.ID()] = true
	}
	var out []string
	for _, id := range j.RestartPending {
		if !known[id] {
			continue
		}
		if _, cluster := SplitID(id); db != "" && cluster != db {
			continue
		}
		out = append(out, id)
	}
	return out
}

func now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
