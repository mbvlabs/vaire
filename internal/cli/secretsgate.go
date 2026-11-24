package cli

import (
	"fmt"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/password"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

// The secrets export gate. vaire secrets export rebuilds the whole secrets
// document, and commands that show a new handoff (vaire backup setup) reveal the
// same. They need a human factor: the master password (an argon2id hash in the
// workspace, set with vaire auth setup or at server init), or a current
// code from the authenticator app the node verifies. There is no flag or
// environment variable for either, so an agent cannot export: structured
// output, --no-input and a missing terminal all refuse with
// secret_gate_required.

// secretGateRequired refuses when the gate cannot ask a human: structured
// output, --yes, --no-input or no terminal.
func secretGateRequired(masterPasswordSet bool) *output.CLIError {
	hint := "Set up a master password or authenticator with vaire auth setup, then run this at a terminal."
	if masterPasswordSet {
		hint = "Run this at a terminal and type the master password, or a current code from the authenticator app. There is no flag or environment variable for either, so scripts and agents cannot export."
	}

	return output.NewError(output.CodeSecretGateRequired,
		"revealing the whole secrets document needs the master password or a code from the authenticator app",
		output.ExitUnsafe, hint).
		WithDetails(map[string]any{"reason": "secret_gate_required", "master_password_set": masterPasswordSet})
}

// masterPasswordHash returns the workspace's master password hash, "" when none
// is set.
func masterPasswordHash(name string) (string, error) {
	creds, err := workspaces.LoadCredentials(name)
	if err != nil {
		return "", err
	}

	return creds.MasterPasswordHash, nil
}

// openSecretGateSession opens a session only for the node's TOTP factor. It is
// best effort: a failure returns no session, and the gate falls back to the
// master password.
func openSecretGateSession(cmd *cobra.Command) (*session, *progress, func()) {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return nil, p, func() {}
	}

	return s, p, s.Close
}

// secretGateUnlock requires a human factor before action. hash is the
// workspace's master password hash ("" when none); s and p, when set, let the
// node verify a code. It refuses with secret_gate_required when no factor can
// be checked.
func secretGateUnlock(cmd *cobra.Command, hash string, s *session, p *progress, action string) error {
	return unlockSecretFactors(!MustNotPrompt(cmd) && !flagTrue(cmd, "yes"), hash != "", func() (func() error, func()) {
		closeFn := func() {}
		if s == nil || p == nil {
			s, p, closeFn = openSecretGateSession(cmd)
		}

		if s == nil || p == nil {
			return nil, closeFn
		}

		st, err := fetchProtectStatus(cmd, s, p)
		if err != nil {
			return nil, closeFn
		}

		if factors := st.ApprovalFactors; hasApprovalFactor(factors) {
			return func() error { return secretGateApproval(cmd, s, p, factors) }, closeFn
		}

		return nil, closeFn
	}, func() error { return secretGatePassword(cmd, hash, action) })
}

func secretGateApproval(cmd *cobra.Command, s *session, p *progress, factors *node.ExposureFactorStatus) error {
	values, err := promptApprovalFactor(cmd, factors, "Approve with")
	if err != nil {
		return err
	}

	_, err = p.call(commandContext(cmd), s.node, "protect.verify", nil, remote.CallOptions{Secrets: values, OnEvent: p.event})

	return err
}

// unlockSecretFactors always discovers the code factor, even when a password
// exists. A failed node connection leaves the local password available. The
// human check precedes discovery so non-interactive exports never dial a node.
func unlockSecretFactors(human, passwordSet bool, openCode func() (func() error, func()), password func() error) error {
	if !human {
		return secretGateRequired(passwordSet)
	}

	code, closeFn := openCode()
	defer closeFn()

	if code != nil {
		return code()
	}

	if passwordSet {
		return password()
	}

	return secretGateRequired(false)
}

// secretGatePassword asks for the master password and checks it against the
// stored hash. It never echoes, and gives up after three tries.
func secretGatePassword(cmd *cobra.Command, hash, action string) error {
	for range 3 {
		pw, err := readSecretTTY(cmd, "Master password")
		if err != nil {
			return err
		}

		if password.Verify(hash, pw) {
			return nil
		}

		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "That password does not match.")
	}

	return output.NewError(output.CodeSecretGateRequired, "the master password is wrong; "+action+" was refused", output.ExitUnsafe,
		"Try again at a terminal. If the password is lost, a code from the authenticator app still opens the gate while protection is set up; otherwise the secrets document cannot be exported.").
		WithDetails(map[string]any{"reason": "secret_gate_required"})
}
