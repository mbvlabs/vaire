package cli

import (
	"errors"
	"testing"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
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

	if err := secretGateUnlock(cmd, c, "", nil, nil, "vaire secrets export"); !isSecretGateErr(err) {
		t.Fatalf("no factor: %v", err)
	}
	err := secretGateUnlock(cmd, c, "$argon2id$v=19$m=1,t=1,p=1$c2FsdA$aGFzaA", nil, nil, "vaire secrets export")
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

func TestSecretGateFactorSelection(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		human, password, node, totp bool
		want                        string
	}{
		{"both factors prefer code", true, true, true, true, "code"},
		{"TOTP only", true, false, true, true, "code"},
		{"password only", true, true, true, false, "password"},
		{"unreachable node uses password", true, true, false, false, "password"},
		{"unreachable node without password refuses", true, false, false, false, "refused"},
		{"neither factor refuses", true, false, true, false, "refused"},
		{"noninteractive never opens node", false, true, true, true, "refused"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ""
			opened, closed := false, false
			err := unlockSecretFactors(tc.human, tc.password, func() (func() error, func()) {
				opened = true
				closeFn := func() { closed = true }
				if !tc.node || !tc.totp {
					return nil, closeFn
				}
				return func() error { got = "code"; return nil }, closeFn
			}, func() error { got = "password"; return nil })
			if isSecretGateErr(err) {
				got = "refused"
			} else if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || opened != tc.human || closed != tc.human {
				t.Fatalf("factor=%s opened=%v closed=%v, want %s human=%v", got, opened, closed, tc.want, tc.human)
			}
		})
	}
}

func TestSecretGateDoesNotHideCodeRejection(t *testing.T) {
	rejected := errors.New("replayed code")
	err := unlockSecretFactors(true, true, func() (func() error, func()) {
		return func() error { return rejected }, func() {}
	}, func() error { t.Fatal("must not silently fall back after a rejected code"); return nil })
	if !errors.Is(err, rejected) {
		t.Fatalf("got %v, want code rejection", err)
	}
}
