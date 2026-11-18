package cli

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Dumps are not part of the CLI: no command, subcommand or flag may bring
// the removed surface back.
func TestDumpSurfaceRemoved(t *testing.T) {
	root := NewRoot()
	for _, path := range []string{"dump", "dump list", "dump get", "node dump"} {
		if cmd, _, err := root.Find(strings.Fields(path)); err == nil && cmd.CommandPath() == "vaire "+path {
			t.Errorf("%s still resolves", path)
		}
	}

	var walk func(*cobra.Command)

	walk = func(cmd *cobra.Command) {
		if strings.EqualFold(cmd.Name(), "dump") {
			t.Errorf("%q is still a command", cmd.CommandPath())
		}

		cmd.Flags().VisitAll(func(flag *pflag.Flag) {
			if strings.Contains(strings.ToLower(flag.Name), "dump") {
				t.Errorf("%s still has --%s", cmd.CommandPath(), flag.Name)
			}
		})

		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
}
