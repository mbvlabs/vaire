package cli

import (
	"encoding/json"
	"os"

	"github.com/mbvlabs/vaire/internal/node"
	"github.com/spf13/cobra"
)

func nodeAccessEnrollCommand() cmdSpec {
	return cmdSpec{
		use: "access-enroll", hidden: true, run: runNodeAccessEnroll,
		meta: CommandMeta{
			Summary: "Redeem a setup code from the node enrollment socket", LinuxOnly: true,
			Examples: []string{"vaire node access-enroll"}, WhenToUse: []string{"Only the systemd enrollment socket invokes this service."},
		},
	}
}

func nodeAccessExpireCommand() cmdSpec {
	return cmdSpec{
		use: "access-expire", hidden: true, run: runNodeAccessExpire,
		meta: CommandMeta{
			Summary: "Reconcile and expire temporary enrollment peers", LinuxOnly: true,
			Examples: []string{"vaire node access-expire"}, WhenToUse: []string{"The node enrollment timer invokes this service."},
		},
	}
}

func runNodeAccessEnroll(cmd *cobra.Command, _ []string) error {
	err := node.AccessRedeem(commandContext(cmd), os.Getenv("REMOTE_ADDR"), cmd.InOrStdin())

	encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
		OK bool `json:"ok"`
	}{OK: err == nil})
	if err != nil {
		// Errors contain no setup codes or request bytes. Keep the detail in
		// the service journal, not on the temporary peer's socket.
		_, _ = cmd.ErrOrStderr().Write([]byte(err.Error() + "\n"))
	}

	return encodeErr
}

func runNodeAccessExpire(cmd *cobra.Command, _ []string) error {
	return node.ExpireAccess(commandContext(cmd))
}
