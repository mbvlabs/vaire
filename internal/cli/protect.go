package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/cli/ui"
	"github.com/mbvlabs/eru/internal/node"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/totp"
	"github.com/spf13/cobra"
)

// protectStatusData is the protect.status result.
type protectStatusData struct {
	Configured  bool     `json:"configured"`
	Pending     bool     `json:"pending"`
	ConfirmedAt string   `json:"confirmed_at,omitempty"`
	Failures    int      `json:"failures"`
	LockedUntil string   `json:"locked_until,omitempty"`
	Protected   []string `json:"protected_databases"`
}

func fetchProtectStatus(cmd *cobra.Command, s *session, p *progress) (protectStatusData, error) {
	reply, err := p.call(commandContext(cmd), s.node, "protect.status", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return protectStatusData{}, err
	}
	var data protectStatusData
	if err := reply.Decode(&data); err != nil {
		return protectStatusData{}, err
	}
	return data, nil
}

// protectCodeAttempts is how many codes protect init asks for before it
// gives up on confirming the new secret.
const protectCodeAttempts = 3

// runProtectInit sets up the TOTP secret that lifting a database's
// protection asks a code from. The node makes and keeps the secret; this
// shows it once as a QR code and confirms it with a code before the node
// uses it. Only a person at a terminal can do this.
func runProtectInit(cmd *cobra.Command, _ []string) error {
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return output.NewError(output.CodeInputRequired, "eru protect init needs a human at an interactive terminal", output.ExitInput,
			"Run it yourself in a terminal, without --yes, --no-input or structured output: it shows a QR code to scan with an authenticator app and asks for a code from it.").
			WithDetails(map[string]any{"reason": "protect_requires_human"})
	}
	rotate := flagTrue(cmd, "rotate")
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	var initSecrets map[string]string
	if rotate {
		code, err := readProtectCode(cmd, "Current code from the existing eru entry in the authenticator app")
		if err != nil {
			return err
		}
		initSecrets = map[string]string{node.ProtectCodeSecret: code}
	}
	reply, err := p.call(commandContext(cmd), s.node, "protect.init", map[string]any{"rotate": rotate, "label": s.ctx.Name},
		remote.CallOptions{Secrets: initSecrets, OnEvent: p.event})
	if err != nil {
		return err
	}
	var setup struct {
		Secret string `json:"secret"`
		URI    string `json:"uri"`
	}
	if err := reply.Decode(&setup); err != nil {
		return err
	}
	if err := showProtectSecret(cmd, setup.URI, setup.Secret); err != nil {
		return err
	}
	for attempt := 1; ; attempt++ {
		code, err := readProtectCode(cmd, "Code the authenticator app now shows for eru:"+s.ctx.Name)
		if err != nil {
			return err
		}
		reply, err = p.call(commandContext(cmd), s.node, "protect.confirm", nil,
			remote.CallOptions{Secrets: map[string]string{node.ProtectCodeSecret: code}, OnEvent: p.event})
		if err == nil {
			break
		}
		if attempt >= protectCodeAttempts || !retryableCode(err) {
			return err
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "That code was not accepted ("+err.Error()+"). Check the device's clock and try the next code.")
	}
	var done struct {
		Rotated bool `json:"rotated"`
	}
	_ = reply.Decode(&done)
	message := "two-factor protection is set up: lifting a database's protection now asks for a code from eru:" + s.ctx.Name + " in the authenticator app"
	if done.Rotated {
		message = "the two-factor secret was rotated: remove the old eru:" + s.ctx.Name + " entry from the authenticator app and keep the new one"
	}
	return output.Success(cmd, output.Result{
		Data:     map[string]any{"configured": true, "rotated": done.Rotated},
		Message:  message,
		Warnings: s.warnings,
		Next:     []string{"eru db create <db> --protected", "eru config set clusters.<db>.protected true"},
	})
}

// retryableCode reports whether err is a wrong or reused code, which a
// fresh code may fix; a lockout is not.
func retryableCode(err error) bool {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodeProtectCodeRejected {
		return false
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason == "code_invalid" || reason == "code_reused"
}

// showProtectSecret prints the otpauth URI as a QR code and the secret for
// typing in by hand, on stderr, outside the live area.
func showProtectSecret(cmd *cobra.Command, uri, secret string) error {
	code, err := ui.QR(uri)
	if err != nil {
		return err
	}
	resume := ui.Suspend()
	defer resume()
	_, err = fmt.Fprint(cmd.ErrOrStderr(),
		"Scan this QR code with an authenticator app (1Password, Aegis, Google Authenticator, ...):\n\n"+code+"\n"+
			"Or add it by hand: "+groupSecret(secret)+" (time-based, SHA1, 6 digits, 30 seconds)\n"+
			"The node keeps the secret root-only and does not show it again. Keep a backup of the authenticator entry:\n"+
			"without it, only root on the node can lift a database's protection.\n\n")
	return err
}

// groupSecret splits a base32 secret into groups of four for reading.
func groupSecret(secret string) string {
	var parts []string
	for len(secret) > 4 {
		parts = append(parts, secret[:4])
		secret = secret[4:]
	}
	return strings.Join(append(parts, secret), " ")
}

// readProtectCode asks for a 6-digit code without echo until one is well
// formed.
func readProtectCode(cmd *cobra.Command, label string) (string, error) {
	for attempt := 1; ; attempt++ {
		code, err := readSecretTTY(cmd, label)
		if err != nil {
			return "", err
		}
		if code = totp.NormalizeCode(code); totp.WellFormed(code) {
			return code, nil
		}
		if attempt >= protectCodeAttempts {
			return "", output.NewError(output.CodeProtectCodeRejected, "the code must be 6 digits", output.ExitUnsafe,
				"Type the 6-digit code the authenticator app shows for eru.").WithDetails(map[string]any{"reason": "code_invalid"})
		}
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), "The code is 6 digits.")
	}
}

func runProtectStatus(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	data, err := fetchProtectStatus(cmd, s, p)
	if err != nil {
		return err
	}
	var message string
	switch {
	case data.Configured && data.ConfirmedAt != "":
		message = "two-factor protection is set up (since " + data.ConfirmedAt + ")"
	case data.Configured:
		message = "two-factor protection is set up"
	default:
		message = "two-factor protection is not set up"
	}
	if data.Pending {
		message += "; a new secret from eru protect init is waiting for its first code"
	}
	if data.LockedUntil != "" {
		message += "; codes are locked out until " + data.LockedUntil + " after too many wrong codes"
	} else if data.Failures > 0 {
		message += fmt.Sprintf("; %d wrong code(s) since the last accepted one", data.Failures)
	}
	if len(data.Protected) > 0 {
		message += "; protected: " + strings.Join(data.Protected, ", ")
	} else {
		message += "; no database is protected"
	}
	warnings := s.warnings
	var next []string
	if !data.Configured {
		next = []string{"eru protect init"}
		if len(data.Protected) > 0 {
			warnings = append(warnings, output.Warning{Code: "protect_not_initialized",
				Message: strings.Join(data.Protected, ", ") + " is protected but two-factor protection is not set up, so the protection cannot be lifted until a human runs eru protect init"})
		}
	}
	return output.Success(cmd, output.Result{Data: data, Message: message, Warnings: warnings, Next: next})
}
