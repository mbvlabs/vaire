package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/spf13/cobra"
)

// runNodeRPC serves one request. The dispatcher writes the result event
// itself, so the exit status propagates without a second envelope.
//
// SIGPIPE is ignored so a closed SSH channel makes stdout writes fail with
// EPIPE instead of killing the process mid-step; the handler then finishes
// and the run log keeps the result.
func runNodeRPC(cmd *cobra.Command, _ []string) error {
	signal.Ignore(syscall.SIGPIPE)
	code := node.Serve(os.Stdin, cmd.OutOrStdout())
	if code != output.ExitOK {
		return &localenv.SilentExit{Code: code}
	}
	return nil
}

func runNodeSelfcheck(cmd *cobra.Command, _ []string) error {
	data, warnings, err := node.SelfCheck()
	if err != nil {
		return err
	}
	message := "node selfcheck passed"
	if len(warnings) > 0 {
		message = "node selfcheck passed with warnings"
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings})
}

// nodeReporter prints script output for node commands run by hand or by
// systemd, whose output lands in the journal, and progress every tenth of
// the way (pgBackRest's per-file detail lines become progress events and
// are not printed).
func nodeReporter(cmd *cobra.Command) engine.Reporter {
	opts, _ := output.ParseOptions(cmd)
	if output.UsesStructuredOutput(opts) || opts.Quiet {
		return func(output.Event) {}
	}
	w := cmd.ErrOrStderr()
	shown := map[string]int{}
	return func(ev output.Event) {
		switch {
		case ev.Type == "log":
			_, _ = fmt.Fprintln(w, ev.Line)
		case ev.Type == "progress" && ev.Pct != nil:
			last, seen := shown[ev.Phase]
			if seen && *ev.Pct/10 <= last/10 {
				return
			}
			shown[ev.Phase] = *ev.Pct
			_, _ = fmt.Fprintf(w, "%s: %d%%\n", ev.Phase, *ev.Pct)
		}
	}
}

func runNodeSSHRevert(cmd *cobra.Command, _ []string) error {
	res, err := node.RevertSSH(commandContext(cmd), flagTrue(cmd, "lockdown-only"), nodeReporter(cmd))
	if err != nil {
		return err
	}
	message := "restored SSH and firewall rules from the commit-confirm snapshot"
	switch {
	case res.Skipped != "":
		message = "nothing to revert: " + res.Skipped
	case res.Mode == "lockdown-only":
		message = "break-glass closed: password login is off and public SSH is closed again"
	}
	return output.OK(cmd, res, message)
}

func runNodeRun(cmd *cobra.Command, args []string) error {
	job := args[0]
	if dryRun(cmd) {
		return output.OK(cmd, map[string]any{"job": job, "dry_run": true}, "would run "+job)
	}
	opts := node.JobOptions{ID: strings.ToUpper(stringFlag(cmd, "job-id")), DB: stringFlag(cmd, "db")}
	report := nodeReporter(cmd)
	if opts.ID != "" || os.Getenv("INVOCATION_ID") != "" {
		// Under systemd the events go to the journal as NDJSON, which
		// eru job follow replays.
		report = jobReporter(cmd)
	}
	data, skipped, err := node.RunJob(commandContext(cmd), job, opts, report)
	if err != nil {
		return err
	}
	if skipped != "" {
		return output.OK(cmd, data, "skipped "+job+": "+skipped)
	}
	return output.OK(cmd, data, "ran "+job)
}

// jobReporter writes each event as one redacted NDJSON line on stdout.
func jobReporter(cmd *cobra.Command) engine.Reporter {
	w := cmd.OutOrStdout()
	return func(ev output.Event) {
		ev.Schema = output.EventSchema
		if ev.TS == "" {
			ev.TS = time.Now().UTC().Format(time.RFC3339Nano)
		}
		if raw, err := json.Marshal(secrets.RedactValue(ev)); err == nil {
			_, _ = fmt.Fprintln(w, string(raw))
		}
	}
}

// runNodeDump writes pg_dump -Fc of one database to stdout, for
// eru dump --to local.
func runNodeDump(cmd *cobra.Command, args []string) error {
	if dryRun(cmd) {
		return output.OK(cmd, map[string]any{"database": args[0], "dry_run": true}, "would dump "+args[0])
	}
	if err := node.DumpStream(commandContext(cmd), args[0], cmd.OutOrStdout()); err != nil {
		return err
	}
	return nil
}

// runNodeArchivePush is the archive_command wrapper (Phase 5). It replaces
// the process with pgBackRest, so nothing after it runs on success.
func runNodeArchivePush(cmd *cobra.Command, args []string) error {
	wal := ""
	if len(args) == 1 {
		wal = args[0]
	}
	// Argument errors are the wrapper's own refusals, so they reach the
	// PostgreSQL log with a reason.
	return node.ArchivePush(stringFlag(cmd, "pgbackrest"), stringFlag(cmd, "stanza"), wal)
}

// runNodeUnfence lifts the fence on this node (Phase 5), on the node itself:
// a fenced node refuses mutating RPCs.
func runNodeUnfence(cmd *cobra.Command, _ []string) error {
	res, err := node.Unfence(commandContext(cmd), stringFlag(cmd, "confirm"), flagTrue(cmd, "reclaim"), dryRun(cmd))
	if err != nil {
		return err
	}
	var message string
	switch {
	case !res.WasFenced:
		message = "this node is not fenced"
	case res.DryRun && res.Reclaimed:
		message = fmt.Sprintf("dry run: would write epoch %d naming this node and lift the fence", res.Epoch)
	case res.DryRun:
		message = "dry run: would lift the fence"
	case res.Reclaimed:
		message = fmt.Sprintf("wrote epoch %d naming this node and lifted the fence; run eru apply to bring back the timers and poolers", res.Epoch)
	default:
		message = "lifted the fence; run eru apply to bring back the timers and poolers"
	}
	return output.Success(cmd, output.Result{Data: res, Message: message, Next: []string{"eru apply"}})
}

func runNodeUnlockSSH(cmd *cobra.Command, _ []string) error {
	minutes, err := cmd.Flags().GetInt("minutes")
	if err != nil {
		return err
	}
	res, err := node.UnlockSSH(commandContext(cmd), minutes, dryRun(cmd), nodeReporter(cmd))
	if err != nil {
		return err
	}
	verb := "Opened"
	if res.DryRun {
		verb = "Would open"
	}
	message := fmt.Sprintf("%s public SSH (22/tcp) until %s. Only %s may log in, with its password from the secrets document.\nClose it early with: sudo eru node ssh-revert --lockdown-only",
		verb, res.OpenUntil, res.AdminUser)
	return output.OK(cmd, res, message)
}
