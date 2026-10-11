package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/localenv"
	"github.com/spf13/cobra"
)

// Main executes the eru CLI and returns the process exit status.
func Main() int {
	root := NewRoot()
	cmd, err := root.ExecuteC()
	if err == nil {
		return 0
	}
	var silent *localenv.SilentExit
	if errors.As(err, &silent) && silent != nil {
		return silent.Code
	}
	if cmd == nil {
		cmd = root
	}
	if rerr := output.RenderError(cmd, err); rerr != nil {
		if _, werr := fmt.Fprintln(os.Stderr, err.Error()); werr != nil {
			return output.ExitGeneric
		}
	}
	return output.ExitCode(err)
}

// NewRoot builds the Phase 1 command tree.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "eru",
		Short:         "Operate a production PostgreSQL cluster",
		Long:          "Operate a production PostgreSQL cluster. Agents should start with eru commands --json.",
		RunE:          runRoot,
		SilenceErrors: true,
		SilenceUsage:  true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		PersistentPreRunE: chainPersistentPreRun(
			func(cmd *cobra.Command, _ []string) error {
				_, err := output.ParseOptions(cmd)
				return err
			},
			platformPreRun,
		),
	}
	flags := root.PersistentFlags()
	flags.StringP("context", "c", "", "Cluster context name")
	output.RegisterPersistentFlags(root)
	flags.Bool("no-input", false, "Never prompt. Implied when stdin is not a terminal")
	flags.Bool("sudo", false, "Pre-approve local sudo escalation")
	flags.Bool("install-missing", false, "Pre-approve local package installs")
	flags.Bool("auto-upgrade-nodes", false, "Install this CLI's build on a node that runs a different one")
	flags.Duration("timeout", 30*time.Second, "How long to wait for a lock or connection")
	flags.Bool("yes", false, "Accept non-destructive prompts. Never satisfies --confirm")
	flags.Bool("escalated", false, "Internal marker for a sudo re-exec")
	if err := flags.MarkHidden("escalated"); err != nil {
		panic(err)
	}
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return output.NewError(
			output.CodeUsage,
			err.Error(),
			output.ExitUsage,
			"see "+cmd.CommandPath()+" --help",
		)
	})
	registerMeta(root, rootMeta())
	addSpecs(root, phase1Commands())
	finalizeMeta(root)
	return root
}

func rootMeta() CommandMeta {
	return CommandMeta{
		Summary: "Operate a production PostgreSQL cluster",
		WhenToUse: []string{
			"Run an eru command. Agents should start with eru commands --json when the next step is unknown.",
		},
		Examples: []string{
			"eru version",
			"eru commands --json",
			"eru guide agents",
		},
		Next: []string{"eru commands --json", "eru guide agents"},
	}
}
