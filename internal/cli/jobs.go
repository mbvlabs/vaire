package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/backup"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/spf13/cobra"
)

// jobReply is the result of a method that runs as a detachable job.
type jobReply struct {
	JobID  string          `json:"job_id"`
	Job    string          `json:"job"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result"`
}

// callJob calls a method that starts a detachable job and follows it.
// Ctrl-C detaches: the job keeps running on the node and eru job follow <id>
// re-attaches. A lost connection after the node announced the job
// reconnects and re-attaches by itself a few times before giving up.
// detached reports the Ctrl-C case, with the job ID when the node had
// already announced it.
func callJob(cmd *cobra.Command, s *session, p *progress, method string, params any, opts remote.CallOptions) (reply remote.Reply, jobID string, detached bool, err error) {
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	defer p.wait(waitLabel(method, params))()
	if m, ok := params.(map[string]string); ok && method == "job.follow" {
		jobID = m["id"]
	}
	// seen counts the job's journal events shown so far: everything after
	// the node announced the job (all of them for job.follow). job.follow
	// replays the journal from the start, so a re-attach skips that many.
	seen := 0
	opts.OnEvent = func(ev output.Event) {
		if ev.Type == "progress" && ev.Phase == "job" {
			jobID = ev.Detail
			p.note("job " + jobID + " started; Ctrl-C detaches and it keeps running (eru job follow " + jobID + ")")
			return
		}
		if jobID != "" {
			seen++
		}
		p.event(ev)
	}
	reply, err = s.node.Call(ctx, method, params, opts)
	if err != nil && jobID != "" && isStreamLost(err) {
		follow := func(ctx context.Context, onEvent func(output.Event)) (remote.Reply, error) {
			return s.node.Call(ctx, "job.follow", map[string]string{"id": jobID}, remote.CallOptions{OnEvent: onEvent})
		}
		reply, err = reattachJob(ctx, jobID, err, &seen, s.reconnect, follow, p.event, p.note, jobReattachBackoff)
	}
	if err != nil && ctx.Err() != nil && commandContext(cmd).Err() == nil {
		return reply, jobID, true, nil
	}
	return reply, jobID, false, err
}

// jobReattachBackoff is the wait before each re-attach attempt after a lost
// connection; its length is the number of attempts.
var jobReattachBackoff = []time.Duration{2 * time.Second, 5 * time.Second, 15 * time.Second}

// reattachJob reconnects and follows job id after lost, the stream_lost
// error of the call that started or followed it. seen is how many of the
// job's journal events were already shown; the replay skips them and adds
// the new ones. It returns the job's outcome, or lost when every attempt
// failed the same way, so its hint (the request's run log) stands.
func reattachJob(ctx context.Context, id string, lost error, seen *int, reconnect func(context.Context) error,
	follow func(context.Context, func(output.Event)) (remote.Reply, error), emit func(output.Event), note func(string), backoff []time.Duration,
) (remote.Reply, error) {
	for n, wait := range backoff {
		note(fmt.Sprintf("lost the connection to the node; job %s keeps running there; re-attaching (attempt %d of %d)", id, n+1, len(backoff)))
		select {
		case <-ctx.Done():
			return remote.Reply{}, ctx.Err()
		case <-time.After(wait):
		}
		if err := reconnect(ctx); err != nil {
			if ctx.Err() != nil {
				return remote.Reply{}, ctx.Err()
			}
			continue
		}
		skip := *seen
		reply, err := follow(ctx, func(ev output.Event) {
			if skip > 0 {
				skip--
				return
			}
			*seen++
			emit(ev)
		})
		if err == nil || !isStreamLost(err) {
			return reply, err
		}
	}
	return remote.Reply{}, lost
}

// isStreamLost reports whether err is a node call whose connection died
// before the result.
func isStreamLost(err error) bool {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		return false
	}
	details, ok := cliErr.Details.(map[string]any)
	return ok && details["reason"] == "stream_lost"
}

// detachedResult is the output after Ctrl-C detached from a job.
func detachedResult(cmd *cobra.Command, s *session, jobID, what string) error {
	data := map[string]any{"job_id": jobID, "detached": true}
	message := "Detached; " + what + " keeps running on the node."
	next := []string{"eru job list"}
	if jobID != "" {
		message += " Re-attach with eru job follow " + jobID + "."
		next = []string{"eru job follow " + jobID}
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: s.warnings, Next: next})
}

// decodeJob decodes a finished job's reply and its result into v.
func decodeJob(reply remote.Reply, v any) (jobReply, error) {
	var jr jobReply
	if err := reply.Decode(&jr); err != nil {
		return jr, err
	}
	if v != nil && len(jr.Result) > 0 {
		if err := json.Unmarshal(jr.Result, v); err != nil {
			return jr, err
		}
	}
	return jr, nil
}

func runJobList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := p.call(commandContext(cmd), s.node, "job.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data struct {
		Jobs []engine.JobRecord `json:"jobs"`
	}
	if err := reply.Decode(&data); err != nil {
		return err
	}
	lines := []string{fmt.Sprintf("  %-26s  %-14s  %-12s  %-9s  %s", "ID", "JOB", "DB", "STATE", "STARTED")}
	for _, j := range data.Jobs {
		started := j.StartedAt
		if started == "" {
			started = j.CreatedAt
		}
		db := jobListDB(j)
		line := fmt.Sprintf("  %-26s  %-14s  %-12s  %-9s  %s", j.ID, j.Job, db, j.State, started)
		if j.Error != nil {
			line += "  " + j.Error.Message
		} else if j.Detail != "" {
			line += "  " + j.Detail
		}
		lines = append(lines, line)
	}
	if len(data.Jobs) == 0 {
		lines = []string{"no jobs yet"}
	}
	return output.Success(cmd, output.Result{Data: data, Message: strings.Join(lines, "\n"), Warnings: s.warnings})
}

func runJobFollow(cmd *cobra.Command, args []string) error {
	id := strings.ToUpper(args[0])
	p := newProgress(cmd)
	// Following a job is for reading its log: show the log lines.
	p.opts.Verbose = true
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, _, detached, err := callJob(cmd, s, p, "job.follow", map[string]string{"id": id}, remote.CallOptions{})
	if err != nil {
		return err
	}
	if detached {
		return detachedResult(cmd, s, id, "the job")
	}
	jr, err := decodeJob(reply, nil)
	if err != nil {
		return err
	}
	message := fmt.Sprintf("job %s (%s) %s", jr.JobID, jr.Job, jr.State)
	if line := jobResultMessage(jr.Job, jr.Result); line != "" {
		message += "\n" + line
	}
	return output.Success(cmd, output.Result{
		Data:     jr,
		Message:  message,
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

// jobResultMessage is the result line the command that started a job prints
// for its result (eru backup now, backup verify, restore drill, dump, restore,
// db fork), so eru job follow on a finished job says the same. It is empty
// for a result it can't read.
func jobResultMessage(job string, raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	switch job {
	case "backup-full", "backup-diff", "backup-incr":
		var res struct {
			DB  string     `json:"db"`
			Set backup.Set `json:"set"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Set.Label == "" {
			return ""
		}
		return fmt.Sprintf("%s: backup %s (%s) finished: %s", res.DB, res.Set.Label, res.Set.Type, humanBytes(res.Set.Size))
	case "backup-verify":
		var res backup.VerifyResult
		if json.Unmarshal(raw, &res) != nil || res.FinishedAt == "" {
			return ""
		}
		return "pgbackrest verify " + verifyState(&res) + " at " + res.FinishedAt
	case "restore-drill":
		var drill backup.Drill
		if json.Unmarshal(raw, &drill) != nil || drill.Result == "" {
			return ""
		}
		out := []string{fmt.Sprintf("%s: restore drill %s: backup %s recovered to %s", drill.Database, drill.Result, drill.BackupLabel, drill.RecoveredTo)}
		for _, c := range drill.Checks {
			mark := "ok"
			if !c.OK {
				mark = "FAILED"
			}
			line := "  " + mark + "  " + c.Name
			if c.Detail != "" {
				line += ": " + c.Detail
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	case "db-dump":
		var res struct {
			Dump backup.Dump `json:"dump"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Dump.ID == "" {
			return ""
		}
		return fmt.Sprintf("dump %s of %s is in the bucket (%s, age-encrypted)", res.Dump.ID, res.Dump.Database, humanBytes(res.Dump.Size))
	case "restore":
		var res struct {
			Database  string `json:"database"`
			Restoring string `json:"restoring"`
			Phase     string `json:"phase"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Database == "" {
			return ""
		}
		if res.Phase == "promoted" {
			return res.Database + " is restored and promoted, on a new timeline; a full backup is running. The replaced data is kept as a stopped cluster until eru restore cleanup " + res.Database + "; eru restore undo " + res.Database + " swaps it back."
		}
		return "the temporary cluster " + res.Restoring + " is ready beside the live " + res.Database + "; inspect it with eru sql --db " + res.Restoring + ", then eru restore promote " + res.Database + " or eru restore abort " + res.Database + "."
	case "db-fork":
		var res struct {
			Source string `json:"source"`
			Name   string `json:"name"`
		}
		if json.Unmarshal(raw, &res) != nil || res.Name == "" {
			return ""
		}
		return "forked " + res.Source + " into " + res.Name + " with its own roles (the credentials were shown only by eru db fork)"
	}
	return ""
}

// jobListDB is a job's DB column: its database, else what its result names
// (a restore's database, a fork's new database, the dropped databases a
// stanza-expiry run expired), or -. Records from before the node recorded
// the database of restores and forks only have it in the result.
func jobListDB(j engine.JobRecord) string {
	if j.DB != "" {
		return j.DB
	}
	if len(j.Result) > 0 {
		var res struct {
			Database string `json:"database"`
			Name     string `json:"name"`
			Expired  []struct {
				Database string `json:"database"`
			} `json:"expired"`
		}
		_ = json.Unmarshal(j.Result, &res)
		switch {
		case j.Job == "restore" && res.Database != "":
			return res.Database
		case j.Job == "db-fork" && res.Name != "":
			return res.Name
		}
		var dbs []string
		for _, e := range res.Expired {
			if e.Database != "" && !slices.Contains(dbs, e.Database) {
				dbs = append(dbs, e.Database)
			}
		}
		if len(dbs) > 0 {
			return strings.Join(dbs, ",")
		}
	}
	return "-"
}
