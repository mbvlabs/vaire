package node

import (
	"os"

	"github.com/mbvlabs/vaire/internal/cli/output"
)

// The exact managed sudo rule uses sudo's environment reset; SUDO_UID is set
// by sudo, not supplied by the RPC payload. Without sudo, only root can run RPC.
func trustedHostAdmin() bool {
	uid := os.Getenv("SUDO_UID")

	return os.Geteuid() == 0 && (uid == "" || uid == "0")
}

func trustedHostAdminRequired() error {
	return output.NewError(output.CodePrecondition, "this operation requires a trusted root session", output.ExitPrecondition,
		"Use the provider console or SSH directly as the trusted root account.").
		WithDetails(map[string]any{"reason": "trusted_host_admin_required"})
}
