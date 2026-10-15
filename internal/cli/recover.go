package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/spf13/cobra"
)

// eru db recover <db>…|--all (Phase 5, D8): bring back the databases a
// rebuilt master hasn't recovered, each from its stanza, one at a time.

func dbRecoverCommand() cmdSpec {
	return cmdSpec{
		use:  "recover [db...]",
		args: cobra.ArbitraryArgs,
		run:  runDBRecover,
		flags: []fl{
			flagBool("all", "Every database awaiting recovery: protected first, then smallest first"),
			flagString("time", "", "Recover to this time instead of the latest (RFC 3339 or a relative time); needs --confirm <db>/recover"),
			flagBool("no-promote", "Stop at a ready copy to inspect; eru restore promote finishes it"),
			flagBool("no-full", "Don't take a full backup of each recovered database"),
		},
		meta: CommandMeta{ //nolint:gosec // G101: help text, not a credential.
			Summary: "Recover databases from their stanzas after server init --restore",
			WhenToUse: []string{
				"A rebuilt master lists its databases as awaiting_recovery: this materialises each from its pgBackRest stanza, promotes it on a new timeline and opens it with the same URL and passwords.",
			},
			WhenNotToUse: []string{"To go back in time on a running database: eru restore."},
			Examples: []string{
				"eru db recover --all",
				"eru db recover app",
				"eru db recover app --time 2026-10-15T09:30:00Z --confirm app/recover",
			},
			Mutating:       true,
			SupportsDryRun: true,
			Remote:         true,
			ConfirmToken:   "{db}/recover",
			ErrorCodes:     []string{"precondition_failed", "database_protected", "node_fenced", "unsafe_action_requires_confirmation"},
			Next:           []string{"eru db list", "eru doctor"},
		},
	}
}

// recoverOrder is --all's order (D8): the named databases first, then the
// protected ones, then smallest first.
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
		return output.NewError(output.CodeUsage, "name the databases to recover, or pass --all", output.ExitUsage, "For example eru db recover --all.")
	}
	var at string
	if v := stringFlag(cmd, "time"); v != "" {
		t, err := parseTarget(v, time.Now())
		if err != nil {
			return err
		}
		at = t.UTC().Format(time.RFC3339)
		if all || len(args) != 1 {
			return output.NewError(output.CodeUsage, "--time recovers one database", output.ExitUsage, "Name one database with --time.")
		}
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dbs := args
	if all {
		reply, err := p.call(commandContext(cmd), s.node, "db.list", nil, remote.CallOptions{OnEvent: p.event})
		if err != nil {
			return err
		}
		var data struct {
			Databases []databaseInfo `json:"databases"`
		}
		if err := reply.Decode(&data); err != nil {
			return err
		}
		dbs = recoverOrder(data.Databases, args)
		if len(dbs) == 0 {
			return output.Success(cmd, output.Result{Data: map[string]any{"recovered": []string{}}, Message: "no database awaits recovery", Warnings: s.warnings})
		}
		p.note("recovering, in order: " + strings.Join(dbs, ", "))
	}
	opts := remote.CallOptions{DryRun: dryRun(cmd)}
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
	var outcomes []dbJobOutcome
	var lines []string
	warnings := append([]output.Warning(nil), s.warnings...)
	for _, db := range dbs {
		if len(dbs) > 1 {
			p.note(db + ":")
		}
		params := map[string]any{"database": db, "time": at, "no_promote": flagTrue(cmd, "no-promote"), "no_full": noFull}
		reply, jobID, detached, err := callJob(cmd, s, p, "db.recover", params, opts)
		if detached {
			return detachedResult(cmd, s, jobID, "the recovery of "+db)
		}
		o := dbJobOutcome{Database: db, JobID: jobID}
		if err == nil && dryRun(cmd) {
			var data map[string]any
			_ = reply.Decode(&data)
			o.Result = data
			lines = append(lines, fmt.Sprintf("%s: would recover from %v to %v", db, data["stanza"], data["target"]))
			outcomes = append(outcomes, o)
			continue
		}
		if err == nil {
			var res map[string]any
			var jr jobReply
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
		outcomes = append(outcomes, o)
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: map[string]any{"databases": outcomes, "dry_run": true}, Message: strings.Join(lines, "\n"), Warnings: warnings})
	}
	recovered := recoveredNames(outcomes)
	if len(dbs) > 1 && !flagTrue(cmd, "no-full") && !flagTrue(cmd, "no-promote") && len(recovered) > 0 {
		p.note("taking a full backup of each recovered database")
		fulls, fullLines, fullWarnings, detached := eachDatabase(cmd, s, p, recovered, "backup.now",
			func(db string) any { return map[string]any{"db": db, "type": "full"} },
			func(raw []byte) (any, string, error) { return json.RawMessage(raw), "full backup done", nil })
		if detached != nil {
			return detachedResult(cmd, s, *detached, "the full backups")
		}
		for i, f := range fulls {
			if f.Error != nil {
				warnings = append(warnings, output.Warning{Code: "full_backup_failed", Message: f.Database + ": " + f.Error.Message + "; run eru backup now --db " + f.Database + " --type full"})
			} else if i < len(fullLines) {
				lines = append(lines, f.Database+": "+fullLines[i])
			}
		}
		warnings = append(warnings, fullWarnings...)
	}
	data := map[string]any{"databases": outcomes, "duration_s": int64(time.Since(start).Seconds())}
	message := strings.Join(append(lines, fmt.Sprintf("took %s", time.Since(start).Round(time.Second))), "\n")
	if failed := len(dbs) - len(recovered); failed > 0 {
		return failedOutcomes("recover", "databases", outcomes)
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: []string{"eru db list", "eru doctor"}})
}

// errorBody is err as a per-database outcome's error.
func errorBody(err error) *output.ErrorBody {
	body := output.ErrorBody{Code: output.CodeError, Message: err.Error()}
	var ce *output.CLIError
	if errors.As(err, &ce) {
		body = output.ErrorBody{Code: ce.Code, Message: ce.Error(), Hint: ce.Hint, Details: ce.Details}
	}
	return &body
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
	if res["phase"] == "ready" {
		return db + ": ready to inspect as " + fmt.Sprint(res["restoring"]) + "; eru restore promote " + db + " finishes it"
	}
	line := db + ": recovered"
	if t, _ := res["recovered_to"].(string); t != "" {
		line += " to " + t
	}
	if d, ok := res["duration_s"].(float64); ok {
		line += fmt.Sprintf(" in %ds", int64(d))
	}
	return line
}

func recoverWarnings(db string, res map[string]any) []output.Warning {
	var out []output.Warning
	for _, key := range []string{"timers_warning", "pooler_warning", "scaffold_warning", "steps_warning", "metrics_warning"} {
		if v, _ := res[key].(string); v != "" {
			out = append(out, output.Warning{Code: "recover_" + strings.TrimSuffix(key, "_warning"), Message: db + ": " + v})
		}
	}
	return out
}
