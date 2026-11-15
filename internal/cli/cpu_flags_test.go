package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestCPUFlagPresence(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().String("cpu-mode", "", "")
	cmd.Flags().String("cpu-quota", "", "")
	params := map[string]any{}
	addCPUFlags(cmd, params)
	if len(params) != 0 {
		t.Fatal(params)
	}
	if err := cmd.Flags().Set("cpu-mode", "spare"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("cpu-quota", "none"); err != nil {
		t.Fatal(err)
	}
	addCPUFlags(cmd, params)
	if params["cpu_mode"] != "spare" || params["cpu_quota"] != "none" {
		t.Fatal(params)
	}
}
