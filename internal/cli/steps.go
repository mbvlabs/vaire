package cli

import (
	"fmt"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/steps"
	"github.com/mbvlabs/eru/internal/versions"
	"github.com/spf13/cobra"
)

// runCheckSteps backs the hidden eru commands --check-steps used by just lint:
// it validates step IDs and uniqueness and reports the catalogue size.
func runCheckSteps(cmd *cobra.Command) error {
	catalog := steps.Catalog()
	if err := engine.ValidateIDs(catalog); err != nil {
		return output.NewError(output.CodeValidation, err.Error(), output.ExitConfig, "Fix the step catalogue in internal/steps.")
	}
	if err := versions.CheckExporterPins(); err != nil {
		return output.NewError(output.CodeValidation, err.Error(), output.ExitConfig, "Pin a version and a sha256 per architecture in internal/versions.")
	}
	ids := make([]string, 0, len(catalog))
	for _, step := range catalog {
		ids = append(ids, step.ID())
	}
	return output.OK(cmd, map[string]any{"count": len(ids), "ids": ids}, fmt.Sprintf("%d steps", len(ids)))
}
