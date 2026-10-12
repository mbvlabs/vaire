package cli

import (
	"fmt"
	"os"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/spf13/cobra"
)

// runNodeRPC serves one request. The dispatcher writes the result event
// itself, so the exit status propagates without a second envelope.
func runNodeRPC(cmd *cobra.Command, _ []string) error {
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
// systemd, whose output lands in the journal.
func nodeReporter(cmd *cobra.Command) engine.Reporter {
	opts, _ := output.ParseOptions(cmd)
	if output.UsesStructuredOutput(opts) || opts.Quiet {
		return func(output.Event) {}
	}
	w := cmd.ErrOrStderr()
	return func(ev output.Event) {
		if ev.Type == "log" {
			_, _ = fmt.Fprintln(w, ev.Line)
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
	data, err := node.RunJob(commandContext(cmd), job, nodeReporter(cmd))
	if err != nil {
		return err
	}
	return output.OK(cmd, data, "ran "+job)
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
