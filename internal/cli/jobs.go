package cli

import (
	"encoding/json"
	"fmt"
	"os/signal"
	"slices"
	"strings"
	"syscall"

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
// Ctrl-C or a dropped tunnel detaches: the job keeps running on the node and
// eru job follow <id> re-attaches. detached reports that case, with the job
// ID when the node had already announced it.
func callJob(cmd *cobra.Command, s *session, p *progress, method string, params any, opts remote.CallOptions) (reply remote.Reply, jobID string, detached bool, err error) {
	ctx, stop := signal.NotifyContext(commandContext(cmd), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	opts.OnEvent = func(ev output.Event) {
		if ev.Type == "progress" && ev.Phase == "job" {
			jobID = ev.Detail
			p.note("job " + jobID + " started; Ctrl-C detaches and it keeps running (eru job follow " + jobID + ")")
			return
		}
		p.event(ev)
	}
	reply, err = s.node.Call(ctx, method, params, opts)
	if err != nil && ctx.Err() != nil && commandContext(cmd).Err() == nil {
		return reply, jobID, true, nil
	}
	return reply, jobID, false, err
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
	reply, err := s.node.Call(commandContext(cmd), "job.list", nil, remote.CallOptions{OnEvent: p.event})
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
	return output.Success(cmd, output.Result{
		Data:     jr,
		Message:  fmt.Sprintf("job %s (%s) %s", jr.JobID, jr.Job, jr.State),
		Warnings: append(s.warnings, reply.Warnings...),
	})
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
