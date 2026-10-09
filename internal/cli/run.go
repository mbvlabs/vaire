package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/buildinfo"
	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/guide"
	"github.com/spf13/cobra"
)

func runRoot(cmd *cobra.Command, _ []string) error {
	opts, err := output.ParseOptions(cmd)
	if err != nil {
		return err
	}
	if output.UsesStructuredOutput(opts) {
		return output.NewError(
			output.CodeUsage,
			"a command is required",
			output.ExitUsage,
			"Run eru commands --json to list commands.",
		)
	}
	return cmd.Help()
}

func parentRun(cmd *cobra.Command, _ []string) error {
	opts, err := output.ParseOptions(cmd)
	if err != nil {
		return err
	}
	if output.UsesStructuredOutput(opts) {
		names := make([]string, 0)
		for _, child := range childCommands(cmd) {
			names = append(names, child.Name())
		}
		return output.NewError(
			output.CodeUsage,
			cmd.Name()+" needs a subcommand",
			output.ExitUsage,
			"available: "+strings.Join(names, ", "),
		)
	}
	return cmd.Help()
}

func notImplemented(cmd *cobra.Command, _ []string) error {
	return output.NewError(
		output.CodeError,
		cmd.CommandPath()+" is not implemented yet",
		output.ExitGeneric,
		"This command lands in a later Phase 1 milestone.",
	)
}

func confirmArgThen(cmd *cobra.Command, args []string) error {
	if !dryRun(cmd) {
		if err := Confirm(cmd, args[0]); err != nil {
			return err
		}
	}
	return notImplemented(cmd, args)
}

func runDBCredentials(cmd *cobra.Command, args []string) error {
	rotate := false
	if flag := cmd.Flags().Lookup("rotate"); flag != nil && flag.Value.String() == "true" {
		rotate = true
	}
	if rotate && !dryRun(cmd) {
		if err := Confirm(cmd, args[0]); err != nil {
			return err
		}
	}
	return notImplemented(cmd, args)
}

func runVersion(cmd *cobra.Command, _ []string) error {
	info := map[string]string{
		"version":          buildinfo.Version,
		"commit":           buildinfo.Commit,
		"channel":          buildinfo.Channel,
		"release_base_url": buildinfo.ReleaseBaseURL,
	}
	message := fmt.Sprintf("eru %s (%s, %s)", buildinfo.Version, buildinfo.Commit, buildinfo.Channel)
	return output.OK(cmd, info, message)
}

func runGuideAgents(cmd *cobra.Command, _ []string) error {
	body := guide.Agents
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	opts, err := output.ParseOptions(cmd)
	if err != nil {
		return err
	}
	if opts.Mode == output.ModeHuman || opts.Mode == output.ModeMarkdown {
		if opts.Quiet {
			return nil
		}
		_, err := fmt.Fprint(cmd.OutOrStdout(), body)
		return err
	}
	return output.OK(cmd, map[string]string{"markdown": body}, "eru agent guide")
}
