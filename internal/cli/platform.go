package cli

import (
	"runtime"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/spf13/cobra"
)

func chainPersistentPreRun(existing, next func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		if existing != nil {
			if err := existing(cmd, args); err != nil {
				return err
			}
		}
		if next != nil {
			return next(cmd, args)
		}
		return nil
	}
}

func platformPreRun(cmd *cobra.Command, _ []string) error {
	if runtime.GOOS == "linux" {
		return nil
	}
	meta, ok := metaFor(cmd)
	if !ok || !linuxOnlyApplies(cmd, meta) {
		return nil
	}
	return output.NewError(
		output.CodePrecondition,
		"node commands run on the Linux server",
		output.ExitPrecondition,
		"node commands run on the Linux server; use the operator commands from this machine",
	).WithDetails(map[string]any{
		"reason": "node_mode_requires_linux",
		"os":     runtime.GOOS,
	})
}

func linuxOnlyApplies(cmd *cobra.Command, meta CommandMeta) bool {
	if meta.LinuxOnly {
		return true
	}
	for _, name := range meta.LinuxOnlyFlags {
		if flagChanged(cmd, name) {
			return true
		}
	}
	return false
}

func flagChanged(cmd *cobra.Command, name string) bool {
	name = strings.TrimLeft(name, "-")
	if cmd == nil || cmd.Flags() == nil {
		return false
	}
	flag := cmd.Flags().Lookup(name)
	return flag != nil && flag.Changed
}
