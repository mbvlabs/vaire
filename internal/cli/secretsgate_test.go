package cli

import (
	"errors"
	"testing"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/spf13/cobra"
)

// isSecretGateErr reports whether err is secret_gate_required.
func isSecretGateErr(err error) bool {
	var cliErr *output.CLIError
	return errors.As(err, &cliErr) && cliErr.Code == output.CodeSecretGateRequired
}

// Without a terminal (or with --json / --no-input) the gate refuses: no flag
// or environment variable stands in for the password or the code, so an agent
// cannot export.
func TestSecretGateRefusesWithoutTerminal(t *testing.T) {
	cmd := &cobra.Command{}
	cmd.Flags().Bool("no-input", false, "")
	_ = cmd.Flags().Set("no-input", "true")
	c := &contexts.Context{Name: "prod"}

	if err := secretGateUnlock(cmd, c, "", nil, nil, "eru secrets export"); !isSecretGateErr(err) {
		t.Fatalf("no factor: %v", err)
	}
	err := secretGateUnlock(cmd, c, "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", nil, nil, "eru secrets export")
	if !isSecretGateErr(err) {
		t.Fatalf("password set: %v", err)
	}
	var cliErr *output.CLIError
	_ = errors.As(err, &cliErr)
	details, _ := cliErr.Details.(map[string]any)
	if details["master_password_set"] != true {
		t.Fatalf("details = %+v, want master_password_set true", details)
	}
}
