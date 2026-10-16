package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/password"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/totp"
	"github.com/spf13/cobra"
)

// The secrets export gate. eru secrets export rebuilds the whole secrets
// document, and commands that show a new handoff (eru backup setup) reveal the
// same. They need a human factor: the master password (an argon2id hash in the
// context, set with eru secrets password set or at server init), or a current
// code from the authenticator app the node verifies. There is no flag or
// environment variable for either, so an agent cannot export: structured
// output, --no-input and a missing terminal all refuse with
// secret_gate_required.

// secretGateRequired refuses when the gate cannot ask a human: structured
// output, --yes, --no-input or no terminal.
func secretGateRequired(masterPasswordSet bool) *output.CLIError {
	hint := "Set a master password with eru secrets password set, then run this at a terminal and type it. Two-factor protection (eru protect init) also opens the gate, with a code from the authenticator app."
	if masterPasswordSet {
		hint = "Run this at a terminal and type the master password, or a current code from the authenticator app. There is no flag or environment variable for either, so scripts and agents cannot export."
	}
	return output.NewError(output.CodeSecretGateRequired,
		"revealing the whole secrets document needs the master password or a code from the authenticator app",
		output.ExitUnsafe, hint).
		WithDetails(map[string]any{"reason": "secret_gate_required", "master_password_set": masterPasswordSet})
}

// masterPasswordHash returns the context's master password hash, "" when none
// is set.
func masterPasswordHash(name string) (string, error) {
	creds, err := contexts.LoadCredentials(name)
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
// context's master password hash ("" when none); s and p, when set, let the
// node verify a code. It refuses with secret_gate_required when no factor can
// be checked.
func secretGateUnlock(cmd *cobra.Command, c *contexts.Context, hash string, s *session, p *progress, action string) error {
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return secretGateRequired(hash != "")
	}
	configured := false
	if s != nil && p != nil {
		if st, err := fetchProtectStatus(cmd, s, p); err == nil {
			configured = st.Configured
		}
	}
	switch {
	case configured:
		return secretGateCode(cmd, s, p, hash, action)
	case hash != "":
		return secretGatePassword(cmd, hash, action)
	default:
		return secretGateRequired(false)
	}
}

// secretGateCode asks for a code from the authenticator app and has the node
// verify it. When a master password is set too, an empty answer uses it, so
// the password still works if the app is lost.
func secretGateCode(cmd *cobra.Command, s *session, p *progress, hash, action string) error {
	if hash != "" {
		p.note("Two-factor protection is set up: " + action + " needs a current code from the authenticator app, or the master password.")
		code, err := readSecretTTY(cmd, "Current code from the authenticator app (Enter to use the master password)")
		if err != nil {
			return err
		}
		if strings.TrimSpace(code) == "" {
			return secretGatePassword(cmd, hash, action)
		}
		return secretGateVerifyCode(cmd, s, p, totp.NormalizeCode(code))
	}
	secrets, err := readAuthenticatorCode(cmd, action+" was refused", nil)
	if err != nil {
		return err
	}
	return secretGateVerifyCode(cmd, s, p, secrets[node.ProtectCodeSecret])
}

// secretGateVerifyCode has the node check code, reusing the write path's guard
// so the code is one use and failures lock codes out as they do there.
func secretGateVerifyCode(cmd *cobra.Command, s *session, p *progress, code string) error {
	if s == nil || p == nil {
		return secretGateRequired(false)
	}
	if _, err := p.call(commandContext(cmd), s.node, "protect.verify", nil,
		remote.CallOptions{Secrets: map[string]string{node.ProtectCodeSecret: code}, OnEvent: p.event}); err != nil {
		return err
	}
	return nil
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

// runSecretsPasswordSet sets or changes the context's master password. It
// never takes the password from a flag: replacing it needs the current
// password, or a code when two-factor protection is set up.
func runSecretsPasswordSet(cmd *cobra.Command, _ []string) error {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return err
	}
	if MustNotPrompt(cmd) {
		return output.NewError(output.CodeInputRequired,
			"setting the master password needs a terminal", output.ExitInput,
			"Run eru secrets password set yourself at a terminal. There is no flag or environment variable for the password.").
			WithDetails(map[string]any{"reason": "password_requires_human"})
	}
	hash, err := masterPasswordHash(c.Name)
	if err != nil {
		return err
	}
	if hash != "" {
		// Changing the password needs the current one, or a code when
		// protection is set up.
		if !stdinIsTTY() {
			return secretGateRequired(true)
		}
		s, p, closeFn := openSecretGateSession(cmd)
		defer closeFn()
		if err := secretGateUnlock(cmd, c, hash, s, p, "changing the master password"); err != nil {
			return err
		}
	}
	newPw, err := readSecretTTY(cmd, fmt.Sprintf("New master password (at least %d characters)", password.MinLength))
	if err != nil {
		return err
	}
	again, err := readSecretTTY(cmd, "Repeat the new master password")
	if err != nil {
		return err
	}
	if newPw != again {
		return output.NewError(output.CodeInputRequired, "the two master passwords do not match", output.ExitInput, "Run the command again.")
	}
	encoded, err := password.Hash(newPw)
	if err != nil {
		return output.NewError(output.CodeInputRequired, err.Error(), output.ExitInput,
			fmt.Sprintf("Use at least %d characters.", password.MinLength)).
			WithDetails(map[string]any{"reason": "password_too_short", "min_length": password.MinLength})
	}
	if err := contexts.SetMasterPasswordHash(c.Name, encoded); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:    map[string]any{"context": c.Name, "master_password_set": true},
		Message: "master password set for context " + c.Name + "; eru secrets export and a new handoff now need it, or a code from the authenticator app",
	})
}
