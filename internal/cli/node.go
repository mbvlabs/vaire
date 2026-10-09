package cli

import (
	"os"

	"github.com/mbvlabs/eru/internal/cli/output"
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
