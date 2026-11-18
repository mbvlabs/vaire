package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

// vaire db recover <db>…|--all (Phase 5, D8): bring back the databases a
// rebuilt master hasn't recovered, each from its stanza, one at a time.

func dbRecoverCommand() cmdSpec {
	return cmdSpec{
		use:  "recover [db...]",
		args: cobra.ArbitraryArgs,
		run:  runDBRecover,
		flags: []fl{
			flagBool("all", "Every database awaiting recovery: protected first, then smallest first"),
			flagString("time", "", "Recover to this time instead of the latest (RFC 3339 or a relative time); needs --confirm <db>/recover"),
			flagBool("no-promote", "Stop at a ready copy to inspect; vaire restore promote finishes it"),
			flagBool("no-full", "Don't take a full backup of each recovered database"),
		},
		meta: CommandMeta{ //nolint:gosec // G101: help text, not a credential.
			Summary: "Recover databases from their stanzas after server init --restore",
			WhenToUse: []string{
				"A rebuilt master lists its databases as awaiting_recovery: this materialises each from its pgBackRest stanza, promotes it on a new timeline and opens it with the same URL and passwords.",
			},
			WhenNotToUse: []string{"To go back in time on a running database: vaire restore."},
			Examples: []string{
				"vaire db recover --all",
				"vaire db recover app",
				"vaire db recover app --time 2026-10-15T09:30:00Z --confirm app/recover",
			},
			Mutating:       true,
			SupportsDryRun: true,
			Remote:         true,
			ConfirmToken:   "{db}/recover",
			ErrorCodes:     []string{"precondition_failed", "database_protected", "node_fenced", "unsafe_action_requires_confirmation"},
			Next:           []string{"vaire db list", "vaire doctor"},
		},
	}
}

// recoverOrder is --all's order (D8): followers after every other database,
// as they follow their recovered source (Phase 6, D14); then the named
// databases first, then the protected ones, then smallest first.
func recoverOrder(dbs []databaseInfo, named []string) []string {
	var awaiting []databaseInfo

	for _, d := range dbs {
		if d.State == stateAwaitingRecovery {
			awaiting = append(awaiting, d)
		}
	}

	sort.SliceStable(awaiting, func(i, j int) bool {
		a, b := awaiting[i], awaiting[j]

		ai, bi := slices.Index(named, a.Name), slices.Index(named, b.Name)
		switch {
		case (a.Follows != "") != (b.Follows != ""):
			return a.Follows == ""
		case ai >= 0 && bi >= 0:
			return ai < bi
		case ai >= 0 || bi >= 0:
			return ai >= 0
		case a.Protected != b.Protected:
			return a.Protected
		case a.SizeBytes != b.SizeBytes:
			return a.SizeBytes < b.SizeBytes
		}

		return a.Name < b.Name
	})

	out := make([]string, 0, len(awaiting))
	for _, d := range awaiting {
		out = append(out, d.Name)
	}

	return out
}

func runDBRecover(cmd *cobra.Command, args []string) error {
	all := flagTrue(cmd, "all")
	if !all && len(args) == 0 {
		return output.NewError(output.CodeUsage, "name the databases to recover, or pass --all", output.ExitUsage, "For example vaire db recover --all.")
	}

	at, err := recoverTarget(cmd, args, all)
	if err != nil {
		return err
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	dbs := args

	if all {
		var warnings []output.Warning

		dbs, warnings, err = recoverAwaiting(cmd, p, s, args)
		if err != nil {
			return err
		}

		if len(dbs) == 0 {
			return output.Success(cmd, output.Result{Data: map[string]any{"recovered": []string{}}, Message: "no database awaits recovery", Warnings: warnings})
		}
	}

	secretValues, generated, err := recoverSecrets(s.ctx.Name)
	if err != nil {
		return err
	}

	opts := remote.CallOptions{DryRun: dryRun(cmd), Secrets: secretValues}
	if at != "" && !dryRun(cmd) {
		token := dbs[0] + "/recover"
		if err := Confirm(cmd, token); err != nil {
			return err
		}

		opts.Confirm = token
	}
	// With several databases the fulls wait until every one is back, so the
	// next recovery doesn't wait for the heavy-I/O lock.
	noFull := flagTrue(cmd, "no-full") || len(dbs) > 1
	start := time.Now()

	warnings := append(append([]output.Warning(nil), s.warnings...), staleWarning(generated)...)

	got := recoverEvery(cmd, s, p, dbs, at, noFull, opts, warnings)
	if got.detached {
		return detachedResult(cmd, s, got.detachID, "the recovery of "+got.detachDB)
	}

	outcomes, lines, warnings := got.outcomes, got.lines, got.warnings
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"databases": outcomes, "dry_run": true}, Message: strings.Join(lines, "\n"), Warnings: warnings})
	}

	recovered := recoveredNames(outcomes)
	toBackUp := withStanza(outcomes)

	lines, warnings, detachID, detached := recoverFulls(cmd, s, p, dbs, toBackUp, lines, warnings)
	if detached {
		return detachedResult(cmd, s, detachID, "the full backups")
	}

	if len(recovered) == len(dbs) {
		return recoverSuccess(cmd, outcomes, lines, warnings, start)
	}

	return failedOutcomes("recover", "databases", outcomes)
}

// recoverOutcomes are the results of one run's per-database recovery jobs.
type recoverOutcomes struct {
	outcomes []dbJobOutcome
	lines    []string
	warnings []output.Warning
	detachID string
	detachDB string
	detached bool
}

// recoverEvery recovers every database in order.
func recoverEvery(cmd *cobra.Command, s *session, p *progress, dbs []string, at string, noFull bool, opts remote.CallOptions, warnings []output.Warning) recoverOutcomes {
	var got recoverOutcomes

	got.warnings = warnings

	for _, db := range dbs {
		if len(dbs) > 1 {
			p.note(db + ":")
		}

		o, moreLines, moreWarnings, detachID, detached := recoverOne(cmd, s, p, db, at, noFull, opts)
		if detached {
			got.detachID, got.detachDB, got.detached = detachID, db, true
			return got
		}

		got.outcomes = append(got.outcomes, o)
		got.lines = append(got.lines, moreLines...)
		got.warnings = append(got.warnings, moreWarnings...)
	}

	return got
}

// recoverFulls takes a full backup of every recovered database with a stanza,
// reporting the lines, warnings and a detached job id.
func recoverFulls(cmd *cobra.Command, s *session, p *progress, dbs, toBackUp, lines []string, warnings []output.Warning) ([]string, []output.Warning, string, bool) {
	if len(dbs) <= 1 || flagTrue(cmd, "no-full") || flagTrue(cmd, "no-promote") || len(toBackUp) == 0 {
		return lines, warnings, "", false
	}

	p.note("taking a full backup of each recovered database")

	fulls, fullLines, fullWarnings, detached := eachDatabase(cmd, s, p, toBackUp, "backup.now",
		func(db string) any { return recoverFullParams(db) },
		func(raw []byte) (any, string, error) { return json.RawMessage(raw), "full backup done", nil })
	if detached != nil {
		return lines, warnings, *detached, true
	}

	moreLines, moreWarnings := fullBackupResults(fulls, fullLines)
	lines = append(lines, moreLines...)
	warnings = append(warnings, moreWarnings...)
	warnings = append(warnings, fullWarnings...)

	return lines, warnings, "", false
}

// recoverSuccess is the run's result once every database is back.
func recoverSuccess(cmd *cobra.Command, outcomes []dbJobOutcome, lines []string, warnings []output.Warning, start time.Time) error {
	data := map[string]any{"databases": outcomes, "duration_s": int64(time.Since(start).Seconds())}
	message := strings.Join(append(lines, fmt.Sprintf("took %s", time.Since(start).Round(time.Second))), "\n")

	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: []string{"vaire db list", "vaire doctor"}})
}

// recoverTarget is --time as the UTC RFC 3339Nano the node takes, or "".
func recoverTarget(cmd *cobra.Command, args []string, all bool) (string, error) {
	_, at, err := targetFlag(cmd, "time")
	if err != nil {
		return "", err
	}

	if at == "" {
		return "", nil
	}

	if all || len(args) != 1 {
		return "", output.NewError(output.CodeUsage, "--time recovers one database", output.ExitUsage, "Name one database with --time.")
	}

	return at, nil
}

// recoverAwaiting lists the databases that await recovery, in order. Its
// warnings are the run's own when there is nothing to do.
func recoverAwaiting(cmd *cobra.Command, p *progress, s *session, args []string) ([]string, []output.Warning, error) {
	reply, err := p.call(commandContext(cmd), s.node, "db.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return nil, nil, err
	}

	var data struct {
		Databases []databaseInfo `json:"databases"`
	}
	if err := reply.Decode(&data); err != nil {
		return nil, nil, err
	}

	dbs := recoverOrder(data.Databases, args)
	if len(dbs) == 0 {
		return nil, append(s.warnings, reply.Warnings...), nil
	}

	p.note("recovering, in order: " + strings.Join(dbs, ", "))

	return dbs, nil, nil
}

// recoverOne recovers one database, returning its outcome, its line and the
// warnings of its job. detached reports the Ctrl-C case.
func recoverOne(cmd *cobra.Command, s *session, p *progress, db, at string, noFull bool, opts remote.CallOptions) (dbJobOutcome, []string, []output.Warning, string, bool) {
	params := map[string]any{"database": db, "time": at, "no_promote": flagTrue(cmd, "no-promote"), "no_full": noFull}

	reply, jobID, detached, err := callJob(commandContext(cmd), s, p, "db.recover", params, opts)
	if detached {
		return dbJobOutcome{}, nil, nil, jobID, true
	}

	o := dbJobOutcome{Database: db, JobID: jobID}

	var (
		lines    []string
		warnings []output.Warning
	)

	if err == nil && dryRun(cmd) {
		var data map[string]any

		_ = reply.Decode(&data)
		o.Result = data

		return o, []string{fmt.Sprintf("%s: would recover from %v to %v", db, data["stanza"], data["target"])}, nil, "", false
	}

	if err == nil {
		var (
			res map[string]any
			jr  jobReply
		)
		if jr, err = decodeJob(reply, &res); err == nil {
			o.JobID, o.Result = jr.JobID, res
			lines = append(lines, recoverLine(db, res))
			warnings = append(warnings, reply.Warnings...)
			warnings = append(warnings, recoverWarnings(db, res)...)
		}
	}

	if err != nil {
		o.Error = errorBody(err)
		lines = append(lines, db+": "+o.Error.Message)
	}

	return o, lines, warnings, "", false
}

// fullBackupResults are the lines and warnings of the full backups taken
// after a multi-database recovery.
func fullBackupResults(fulls []dbJobOutcome, fullLines []string) ([]string, []output.Warning) {
	var (
		lines    []string
		warnings []output.Warning
	)

	for i, f := range fulls {
		switch {
		case f.Error != nil:
			warnings = append(warnings, output.Warning{Code: "full_backup_failed", Message: f.Database + ": " + f.Error.Message + "; run vaire backup now --db " + f.Database + " --type full"})
		case i < len(fullLines):
			lines = append(lines, f.Database+": "+fullLines[i])
		}
	}

	return lines, warnings
}

// recoverSecrets are the secrets db.recover sends: what an apply sends,
// because the recovered database's steps run inside the job. A rebuilt
// master has no service password files until postgres-roles-v1 writes them
// from these, and metrics-v1 and pgbackrest-v1 take the /metrics hash and
// the recovery kit where the node lacks them. It also returns what the
// top-up generated, for the secrets_document_stale warning.
func recoverSecrets(workspaceName string) (map[string]string, []string, error) {
	values, creds, generated, err := serviceSecrets(workspaceName)
	if err != nil {
		return nil, nil, err
	}

	addRecoveryKit(values, creds.Backup)

	return values, generated, nil
}

// recoverFullParams is the backup.now request of the full backup that
// follows the recovery of db with --all.
func recoverFullParams(db string) node.BackupNowParams {
	return node.BackupNowParams{Database: db, Type: backup.TypeFull}
}

// errorBody is err as a per-database outcome's error.
func errorBody(err error) *output.ErrorBody {
	body := output.ErrorBody{Code: output.CodeError, Message: err.Error()}
	if ce, ok := errors.AsType[*output.CLIError](err); ok {
		body = output.ErrorBody{Code: ce.Code, Message: ce.Error(), Hint: ce.Hint, Details: ce.Details}
	}

	return &body
}

// withStanza are the recovered databases that get a full backup: not the
// followers, whose result names the source they follow again (Phase 6).
func withStanza(outcomes []dbJobOutcome) []string {
	var out []string

	for _, o := range outcomes {
		res, _ := o.Result.(map[string]any)
		if f, _ := res["follows"].(string); o.Error == nil && f == "" {
			out = append(out, o.Database)
		}
	}

	return out
}

func recoveredNames(outcomes []dbJobOutcome) []string {
	var out []string

	for _, o := range outcomes {
		if o.Error == nil {
			out = append(out, o.Database)
		}
	}

	return out
}

func recoverLine(db string, res map[string]any) string {
	if f, _ := res["follows"].(string); f != "" {
		line := db + ": following " + f + " again (" + fmt.Sprint(res["from"]) + ")"
		if n := len(recoverWarnings(db, res)); n > 0 {
			line += fmt.Sprintf(", but %d part(s) of finishing it failed (see the warnings)", n)
		}

		return line
	}

	line := db + ": recovered"
	if res["phase"] == "ready" {
		line = db + ": ready to inspect as " + fmt.Sprint(res["restoring"]) + "; vaire restore promote " + db + " finishes it"
		if notice, _ := res["access_notice"].(string); notice != "" {
			line += "; " + notice
		}
	} else {
		if t, _ := res["recovered_to"].(string); t != "" {
			line += " to " + t
		}

		if d, ok := res["duration_s"].(float64); ok {
			line += fmt.Sprintf(" in %ds", int64(d))
		}
	}
	// The data is back, but something after it failed: say so on the
	// database's own line, not only among the warnings.
	if n := len(recoverWarnings(db, res)); n > 0 {
		line += fmt.Sprintf(", but %d part(s) of finishing it failed (see the warnings)", n)
	}

	return line
}

// recoverWarnings are what failed after db's data was recovered: the job's
// own *_warning results, the structured warning of the promote (the poolers or
// the full backup after the cut-over), nested in its promoted result, and a
// follower recovery's warnings, which keep their codes and messages.
func recoverWarnings(db string, res map[string]any) []output.Warning {
	out := promotedRecoverWarnings(res)
	for _, key := range []string{"timers_warning", "pooler_warning", "scaffold_warning", "steps_warning", "metrics_warning"} {
		if v, _ := res[key].(string); v != "" {
			out = append(out, output.Warning{Code: "recover_" + strings.TrimSuffix(key, "_warning"), Message: db + ": " + v})
		}
	}

	out = append(out, followerRecoverWarnings(res)...)

	return out
}

// promotedRecoverWarnings is the structured warning of a recovery's promote,
// nested in the job's promoted result.
func promotedRecoverWarnings(res map[string]any) []output.Warning {
	promoted, ok := res["promoted"].(map[string]any)
	if !ok {
		return nil
	}

	w, ok := promoted["warning"].(map[string]any)
	if !ok {
		return nil
	}

	message, _ := w["message"].(string)
	if message == "" {
		return nil
	}

	code, _ := w["code"].(string)
	if code == "" {
		code = "after_cutover_failed"
	}

	return []output.Warning{{Code: code, Message: message}}
}

// followerRecoverWarnings flattens the warnings of a recovered follower's job
// result, which are structured like the node's other warnings, so they reach
// the standard warning list with their own codes instead of staying nested in
// the job result (Phase 6).
func followerRecoverWarnings(res map[string]any) []output.Warning {
	raw, ok := res["warnings"].([]any)
	if !ok {
		return nil
	}

	var out []output.Warning

	for _, item := range raw {
		w, ok := item.(map[string]any)
		if !ok {
			continue
		}

		code, _ := w["code"].(string)

		message, _ := w["message"].(string)
		if message == "" {
			continue
		}

		out = append(out, output.Warning{Code: code, Message: message})
	}

	return out
}
