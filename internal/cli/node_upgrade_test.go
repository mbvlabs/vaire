package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/spf13/cobra"
)

func TestNodeUpgradeNonInteractiveConfirmation(t *testing.T) {
	cmd := &cobra.Command{Use: "upgrade"}
	cmd.Flags().Bool("yes", false, "")
	cmd.Flags().Bool("no-input", true, "")

	from := node.BuildID{Version: "1.0.0-rc.2", Channel: "rc"}
	to := node.BuildID{Version: "1.0.0", Channel: "stable"}

	if err := confirmNodeUpgrade(cmd, from, to); cliCodeOf(t, err) != output.CodeInputRequired {
		t.Fatalf("without confirmation: %v", err)
	}

	if err := cmd.Flags().Set("yes", "true"); err != nil {
		t.Fatal(err)
	}

	if err := confirmNodeUpgrade(cmd, from, to); err != nil {
		t.Fatal(err)
	}
}

func TestNodeUpgradeDevBuildRefusesBeforeOpeningWorkspace(t *testing.T) {
	root := NewRoot()
	root.SetArgs([]string{"-w", "missing", "node", "upgrade", "--yes"})

	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "tagged RC or stable local build") {
		t.Fatalf("unexpected command result: %v", err)
	}
}
