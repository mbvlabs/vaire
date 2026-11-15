package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/render"
	"github.com/mbvlabs/vaire/internal/secrets"
)

// Plan and skip reasons (§5.3).
const (
	ReasonNew               = "new"
	ReasonInputsChanged     = "inputs_changed"
	ReasonCheckIncomplete   = "check_incomplete"
	ReasonPreviousFailure   = "previous_failure"
	ReasonJournal           = "journal"
	ReasonCheck             = "check"
	ReasonCheckInconclusive = "check_inconclusive"
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
	// AccessConfirm contains exact database names authorized by access tokens.
	// InternalAccess is populated only by trusted lifecycle callers that own
	// and provision that exact new target. Neither authorizes restarts.
	AccessConfirm  map[string]bool
	InternalAccess map[string]bool
	// Restarter restarts one cluster for its pending restarts.
	Restarter Restarter
	// LeaveRestartsPending reports pending restarts without checking,
	// settling, or performing them, regardless of confirmations.
	LeaveRestartsPending bool
	// Report receives step and log events.
	Report Reporter
	// FailureHint replaces the step_failed hint, for callers that resume
	// differently from apply (server init uses --resume).
	FailureHint string
	// Recovering is a database ClusterView keeps although it awaits recovery
	// or is an unfinished ordinary follow: db recover and the owned follow job
	// build their cluster under it (Phase 5/6).
	Recovering string
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
	// AccessConfirmTokens are the exact tokens for selected access transitions.
	AccessConfirmTokens []string `json:"access_confirm_tokens,omitempty"`
}

// ApplyStep is one step's outcome in an apply.
type ApplyStep struct {
	ID      string `json:"id"`
	Cluster string `json:"cluster,omitempty"`
	Index   int    `json:"index"`
	State   string `json:"state"`
	Reason  string `json:"reason,omitempty"`
	Detail  string `json:"detail,omitempty"`
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
	// RestartsSettled are the clusters whose pending restart was cleared
	// without a restart: PostgreSQL already ran every setting.
	RestartsSettled []string `json:"restarts_settled,omitempty"`
}

// Restarted reports whether the apply restarted any cluster.
func (d ApplyData) Restarted() bool { return len(d.Restarts) > 0 }

type decision struct {
	apply  bool
	reason string
	hash   string
	detail string
}

// ClusterView, when set, narrows the configuration every Plan and Apply
// sees. The node sets it (Phase 5): a database awaiting recovery after a
// rebuild, or one whose ordinary follow has not completed, has no cluster to
// converge, so no step may create an empty one, render its pooler entry or
// start its timers. What's saved is never narrowed.
var ClusterView func(cfg config.Cluster, keep string) config.Cluster

// GuardView is an optional fail-closed view applied before step expansion.
// It receives the selection so a private lifecycle operation can retain its
// target only for an exact per-database run, never for shared rendering.
var GuardView func(context.Context, config.Cluster, Options) (config.Cluster, error)

func view(ctx context.Context, cfg config.Cluster, opts Options) (config.Cluster, error) {
	if ClusterView != nil {
		cfg = ClusterView(cfg, opts.Recovering)
	}
	if GuardView != nil {
		return GuardView(ctx, cfg, opts)
	}
	return cfg, nil
}

// Plan runs the runner's decision for every selected step and writes nothing.
func Plan(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (PlanData, error) {
	return planWithInvalidatedCompletions(ctx, steps, cfg, rt, opts, nil)
}

// PlanAfterInvalidatingCompletions previews invalidation of completed entries
// without changing the journal or its pending restarts.
func PlanAfterInvalidatingCompletions(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options, ids []string) (PlanData, error) {
	return planWithInvalidatedCompletions(ctx, steps, cfg, rt, opts, ids)
}

func planWithInvalidatedCompletions(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options, ids []string) (PlanData, error) {
	cfg, err := view(ctx, cfg, opts)
	if err != nil {
		return PlanData{}, err
	}
	all, selected, err := prepare(steps, cfg, opts)
	if err != nil {
		return PlanData{}, err
	}
	j, err := LoadJournal(opts.journalPath())
	if err != nil {
		return PlanData{}, err
	}
	dropCompleted(j, ids)
	data := PlanData{Steps: make([]PlanStep, 0, len(selected)), Total: len(selected)}
	for i, step := range selected {
		ps, d, err := planStep(ctx, step, i+1, cfg, rt, j)
		if err != nil {
			return data, stepFailed(step.ID(), i+1, err)
		}
		if d.apply {
			data.WouldApply++
			if access, ok := step.(AccessTransition); ok {
				data.AccessConfirmTokens = append(data.AccessConfirmTokens, access.AccessTarget()+"/access")
			}
		}
		data.Steps = append(data.Steps, ps)
	}
	data.RestartPending = pendingIn(j, all, opts.DB)
	data.ConfirmTokens = RestartTokens(cfg.Name(), data.RestartPending)
	return data, nil
}

// dropCompleted removes the journal's completed entries for ids, so the plan
// previews running them again. It changes only the loaded copy.
func dropCompleted(j *Journal, ids []string) {
	for _, id := range ids {
		if entry, ok := j.Steps[id]; ok && entry.Status == StatusCompleted {
			delete(j.Steps, id)
		}
	}
}

// planStep decides one selected step and, when it would run, renders the
// managed files it would rewrite.
func planStep(ctx context.Context, step Step, index int, cfg config.Cluster, rt Runtime, j *Journal) (PlanStep, decision, error) {
	d, err := decide(ctx, step, cfg, rt, j)
	if err != nil {
		return PlanStep{}, decision{}, err
	}
	action := "skip"
	if d.apply {
		action = "apply"
	}
	_, cluster := SplitID(step.ID())
	ps := PlanStep{ID: step.ID(), Cluster: cluster, Index: index, Action: action, Reason: d.reason, Detail: d.detail}
	if d.apply {
		ps.Diffs = fileDiffs(step, cfg, rt)
	}
	return ps, d, nil
}

// fileDiffs returns the redacted diffs of the managed files step would write.
func fileDiffs(step Step, cfg config.Cluster, rt Runtime) []FileDiff {
	r, ok := step.(FileRenderer)
	if !ok {
		return nil
	}
	var out []FileDiff
	for _, f := range r.RenderedFiles(cfg, rt) {
		if diff := render.Diff(f.Path, f.Content); diff != "" {
			out = append(out, FileDiff{Path: f.Path, Diff: secrets.Redact(diff)})
		}
	}
	return out
}

// Apply converges the selected steps in catalogue order (§5.2). The first
// failure stops the run; the next apply resumes there because earlier steps
// skip on the journal.
func Apply(ctx context.Context, steps []Step, cfg config.Cluster, rt Runtime, opts Options) (ApplyData, []output.Warning, error) {
	if rt.DryRun {
		return ApplyData{}, nil, errors.New("engine.Apply called with DryRun; use Plan")
	}
	cfg, err := view(ctx, cfg, opts)
	if err != nil {
		return ApplyData{}, nil, err
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
	r := &applyRun{ctx: ctx, cfg: cfg, rt: rt, all: all, j: j, path: path, opts: opts, report: opts.Report}
	r.data = ApplyData{Steps: make([]ApplyStep, 0, len(selected)), Total: len(selected)}
	// The events carry the request's numbering when it runs several applies
	// (see Numbering); the result keeps this run's own.
	r.offset, r.shown = 0, len(selected)
	if n := numberingFrom(ctx); n != nil {
		r.offset, r.shown = n.span(len(selected))
	}
	warnings, err := r.run(selected)
	return r.data, warnings, err
}

// applyRun is one Apply: what it works on and what it has reported so far.
type applyRun struct {
	ctx    context.Context
	cfg    config.Cluster
	rt     Runtime
	all    []Step
	j      *Journal
	path   string
	opts   Options
	report Reporter
	offset int
	shown  int
	data   ApplyData
	// warnings are the run's non-fatal notices, in order.
	warnings []output.Warning
}

// run applies every selected step and then settles the pending restarts. It
// returns the warnings that travel with a failure, and on success all of the
// run's warnings.
func (r *applyRun) run(selected []Step) ([]output.Warning, error) {
	// Inspect all selected transitions before any of them writes. A missing
	// token or unknown inspection blocks every access write, but not unrelated
	// selected steps (including protection changes).
	accessErr := r.preflight(selected)
	for i, step := range selected {
		warnings, err := r.step(i+1, step, accessErr)
		if err != nil {
			return warnings, err
		}
	}
	if accessErr != nil {
		return r.warnings, accessErr
	}
	if err := r.restarts(); err != nil {
		return nil, err
	}
	return r.warnings, nil
}

// preflight inspects every selected access transition before any of them
// writes.
func (r *applyRun) preflight(selected []Step) error {
	var accessErr error
	var missing []string
	for i, step := range selected {
		access, ok := step.(AccessTransition)
		if !ok {
			continue
		}
		d, err := decide(r.ctx, step, r.cfg, r.rt, r.j)
		if err != nil {
			if accessErr == nil {
				accessErr = stepFailed(step.ID(), i+1, err)
			}
			continue
		}
		target := access.AccessTarget()
		if d.apply && !r.confirmed(target) {
			missing = append(missing, target+"/access")
		}
	}
	if accessErr == nil && len(missing) > 0 {
		accessErr = output.NewError(output.CodeUnsafeAction, "access changes require confirmation", output.ExitUnsafe,
			"Re-run apply with the exact access confirmation tokens.").
			WithDetails(map[string]any{"confirm_tokens": missing})
	}
	return accessErr
}

// confirmed reports whether opts carries target's access token.
func (r *applyRun) confirmed(target string) bool {
	return r.opts.AccessConfirm[target] || r.opts.InternalAccess[target]
}

// step applies or skips one selected step. index is its place in this run;
// the run's warnings are returned when the error is an access refusal, and
// are dropped for every other failure, as Apply has always reported them.
func (r *applyRun) step(index int, step Step, accessErr error) ([]output.Warning, error) {
	id := step.ID()
	_, cluster := SplitID(id)
	at := r.offset + index
	if _, ok := step.(AccessTransition); ok && accessErr != nil {
		r.data.Steps = append(r.data.Steps, ApplyStep{ID: id, Cluster: cluster, Index: index, State: "skipped", Reason: "access_preflight"})
		r.data.Skipped++
		return nil, nil
	}
	d, err := decide(r.ctx, step, r.cfg, r.rt, r.j)
	if err != nil {
		return nil, failStep(r.j, r.path, r.opts, id, index, at, r.shown, decision{}, err)
	}
	if !d.apply {
		return nil, r.skip(id, cluster, index, d)
	}
	if access, ok := step.(AccessTransition); ok && !r.confirmed(access.AccessTarget()) {
		return r.warnings, output.NewError(output.CodeUnsafeAction, "access changes require confirmation", output.ExitUnsafe,
			"Re-run apply with the exact access confirmation token.").
			WithDetails(map[string]any{"confirm_tokens": []string{access.AccessTarget() + "/access"}})
	}
	return nil, r.execute(index, step, id, cluster, at, d)
}

// skip records a step the decision says can be skipped.
func (r *applyRun) skip(id, cluster string, index int, d decision) error {
	if d.reason == ReasonCheck {
		entry := r.j.Steps[id]
		entry.Status = StatusCompleted
		entry.InputsHash = d.hash
		entry.CompletedAt = now()
		entry.Error = ""
		if err := r.j.setEntry(r.path, id, entry); err != nil {
			return err
		}
	}
	detail := d.reason
	if d.reason == ReasonCheckInconclusive {
		detail = d.detail
		r.warnings = append(r.warnings, output.Warning{Code: "step_check_inconclusive", Message: id + ": " + d.detail})
	}
	r.report.emit(output.Event{Type: "step", Step: id, State: "skipped", Index: intPtr(r.offset + index), Total: intPtr(r.shown), Detail: detail})
	r.data.Steps = append(r.data.Steps, ApplyStep{ID: id, Cluster: cluster, Index: index, State: "skipped", Reason: d.reason, Detail: d.detail})
	r.data.Skipped++
	return nil
}

// execute applies a step whose decision says it must run.
func (r *applyRun) execute(index int, step Step, id, cluster string, at int, d decision) error {
	r.report.emit(output.Event{Type: "step", Step: id, State: "started", Index: intPtr(at), Total: intPtr(r.shown), Detail: step.Describe(r.cfg)})
	if err := r.j.setEntry(r.path, id, JournalEntry{Status: StatusRunning, InputsHash: d.hash, StartedAt: now()}); err != nil {
		return err
	}
	res, err := step.Apply(r.ctx, r.cfg, r.rt, r.report)
	if err == nil && r.ctx.Err() != nil {
		err = ErrInterrupted
	}
	if err != nil {
		if r.ctx.Err() != nil {
			err = ErrInterrupted
		}
		return failStep(r.j, r.path, r.opts, id, index, at, r.shown, d, err)
	}
	restart, err := r.complete(id, res)
	if err != nil {
		return err
	}
	state := "completed"
	if res.Incomplete {
		state = "incomplete"
	}
	r.report.emit(output.Event{Type: "step", Step: id, State: state, Index: intPtr(at), Total: intPtr(r.shown), Detail: strings.Join(res.Notes, "; ")})
	r.warnings = append(r.warnings, res.Warnings...)
	r.data.Steps = append(r.data.Steps, ApplyStep{ID: id, Cluster: cluster, Index: index, State: state, Restart: string(restart)})
	r.data.Applied++
	return nil
}

// complete records id completed and marks its restart pending, returning the
// restart need.
func (r *applyRun) complete(id string, res ApplyResult) (RestartNeed, error) {
	restart := res.Restart
	if restart == "" {
		restart = RestartNone
	}
	entry := r.j.Steps[id]
	entry.Status = StatusCompleted
	entry.CompletedAt = now()
	entry.Restart = string(restart)
	entry.Error = ""
	if res.Incomplete {
		entry.Status = StatusIncomplete
		entry.CompletedAt = ""
	}
	err := r.j.update(r.path, func(j *Journal) {
		j.Steps[id] = entry
		if restart == RestartRestart {
			j.addPending(id)
		}
	})
	return restart, err
}

// restarts settles and performs the pending restarts, one cluster at a time,
// reporting each step in data.
func (r *applyRun) restarts() error {
	pending := pendingIn(r.j, r.all, r.opts.DB)
	if len(pending) == 0 {
		return nil
	}
	if r.opts.LeaveRestartsPending {
		r.setPending(pending)
		r.warnings = append(r.warnings, pendingRestartWarning(pending, r.data.ConfirmTokens))
		return nil
	}
	settled, err := settlePending(r.ctx, r.j, r.path, r.opts, pending)
	r.data.RestartsSettled = settled
	if err != nil {
		return err
	}
	if pending = pendingIn(r.j, r.all, r.opts.DB); len(pending) == 0 {
		return nil
	}
	if r.opts.Restarter == nil {
		return output.NewError(
			output.CodeError,
			"a step needs a restart but this build has no restarter",
			output.ExitGeneric,
			"This is an vaire bug. Report it with the run log.",
		).WithDetails(map[string]any{"restart_pending": pending})
	}
	restarts, err := restartConfirmed(r.ctx, r.j, r.path, r.opts, r.cfg, pending)
	r.data.Restarts = append(r.data.Restarts, restarts...)
	if err != nil {
		return err
	}
	if left := pendingIn(r.j, r.all, r.opts.DB); len(left) > 0 {
		r.setPending(left)
		r.warnings = append(r.warnings, pendingRestartWarning(left, r.data.ConfirmTokens))
	}
	return nil
}

// setPending records the restarts still pending and the tokens that clear
// them.
func (r *applyRun) setPending(pending []string) {
	r.data.RestartPending = pending
	r.data.ConfirmTokens = RestartTokens(r.cfg.Name(), pending)
}

func pendingRestartWarning(pending, tokens []string) output.Warning {
	return output.Warning{Code: "restart_pending",
		Message: "a restart is pending for " + strings.Join(pending, ", ") + "; re-run apply with --confirm " +
			strings.Join(tokens, " --confirm ")}
}

// settlePending clears the pending restarts of every cluster that no longer
// needs one: PostgreSQL runs with every setting the restart was for. It asks
// only when the restarter can tell (RestartChecker), and leaves a cluster it
// can't ask pending. Server-level pending restarts are left as they are. It
// returns the clusters it settled.
func settlePending(ctx context.Context, j *Journal, path string, opts Options, pending []string) ([]string, error) {
	checker, ok := opts.Restarter.(RestartChecker)
	if !ok {
		return nil, nil
	}
	byCluster := map[string][]string{}
	var order []string
	for _, id := range pending {
		if _, cluster := SplitID(id); cluster != "" {
			if _, seen := byCluster[cluster]; !seen {
				order = append(order, cluster)
			}
			byCluster[cluster] = append(byCluster[cluster], id)
		}
	}
	report := opts.Report
	var settled []string
	for _, cluster := range order {
		needed, err := checker.NeedsRestart(ctx, cluster)
		if err != nil || needed {
			continue
		}
		if err := j.clearPending(path, byCluster[cluster]); err != nil {
			return settled, err
		}
		settled = append(settled, cluster)
		if report != nil {
			report(output.Event{Type: "log", Step: "restart", Line: cluster + " no longer needs its pending restart: PostgreSQL already runs every setting; cleared without restarting"})
		}
	}
	return settled, nil
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
	if _, ok := step.(AccessTransition); ok {
		return decideAccess(ctx, step, cfg, rt, hash)
	}
	if f, ok := step.(Forcer); ok {
		if reason := f.Force(cfg, rt); reason != "" {
			return decision{apply: true, reason: reason, hash: hash}, nil
		}
	}
	entry, ok := j.Steps[id]
	if ok && entry.Status == StatusCompleted && entry.InputsHash == hash {
		return decideCompleted(ctx, step, cfg, rt, hash)
	}
	if ok && entry.InputsHash == hash && entry.Status != StatusFailed && entry.Status != StatusRunning {
		check, err := step.Check(ctx, cfg, rt)
		if err != nil {
			return decision{}, fmt.Errorf("check: %w", err)
		}
		if check.Complete && !check.Inconclusive {
			return decision{reason: ReasonCheck, hash: hash, detail: check.Detail}, nil
		}
	}
	return decision{apply: true, reason: reasonFor(entry, ok, hash), hash: hash}, nil
}

// decideAccess decides an access transition. Its access is checked on every
// run, independently of the journal.
func decideAccess(ctx context.Context, step Step, cfg config.Cluster, rt Runtime, hash string) (decision, error) {
	check, err := step.Check(ctx, cfg, rt)
	if err != nil {
		return decision{}, fmt.Errorf("check: %w", err)
	}
	if check.Complete && !check.Inconclusive {
		return decision{reason: "access_installed", hash: hash, detail: check.Detail}, nil
	}
	return decision{apply: true, reason: ReasonCheckIncomplete, hash: hash, detail: check.Detail}, nil
}

// decideCompleted decides a step whose journal entry records the same inputs:
// the journal is proof unless the step rechecks a result that can drift, and
// an inconclusive check leaves the completed entry untouched.
func decideCompleted(ctx context.Context, step Step, cfg config.Cluster, rt Runtime, hash string) (decision, error) {
	rc, recheck := step.(Rechecker)
	if !recheck || !rc.Recheck() {
		return decision{reason: ReasonJournal, hash: hash}, nil
	}
	check, err := step.Check(ctx, cfg, rt)
	if err != nil {
		return decision{}, fmt.Errorf("check: %w", err)
	}
	if check.Inconclusive {
		detail := "effective state is unknown"
		if check.Detail != "" {
			detail += ": " + secrets.Redact(check.Detail)
		}
		return decision{reason: ReasonCheckInconclusive, hash: hash, detail: detail}, nil
	}
	if check.Complete {
		return decision{reason: ReasonJournal, hash: hash}, nil
	}
	return decision{apply: true, reason: ReasonCheckIncomplete, hash: hash, detail: check.Detail}, nil
}

// reasonFor names why a step with no completed matching journal entry must
// run.
func reasonFor(entry JournalEntry, journaled bool, hash string) string {
	switch {
	case !journaled:
		return ReasonNew
	case entry.Status == StatusFailed || entry.Status == StatusRunning:
		return ReasonPreviousFailure
	case entry.InputsHash != hash:
		return ReasonInputsChanged
	default:
		return ReasonCheckIncomplete
	}
}

// InputsHash is sha256(ID || 0x00 || inputs) in hex.
func InputsHash(id string, inputs []byte) string {
	h := sha256.New()
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write(inputs)
	return hex.EncodeToString(h.Sum(nil))
}

// failStep records id failed. index is its place in this run (the error's
// details); at and shown are the numbers its event prints.
func failStep(j *Journal, path string, opts Options, id string, index, at, shown int, d decision, cause error) error {
	msg := secrets.Redact(cause.Error())
	entry := j.Steps[id]
	entry.Status = StatusFailed
	if d.hash != "" {
		entry.InputsHash = d.hash
	}
	entry.Error = msg
	saveErr := j.setEntry(path, id, entry)
	opts.Report.emit(output.Event{Type: "step", Step: id, State: "failed", Index: intPtr(at), Total: intPtr(shown), Detail: msg})
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
	if errors.As(cause, &cliErr) && (cliErr.Code == output.CodePrecondition || cliErr.Code == output.CodeInputRequired || cliErr.Code == output.CodeStepFailed) {
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
		return nil, nil, output.WrapError(output.CodeError, err, output.ExitGeneric, "This is an vaire bug in the step catalogue.")
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
