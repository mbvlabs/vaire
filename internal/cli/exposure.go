package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/cli/ui"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/password"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/totp"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

func hasApprovalFactor(status *node.ExposureFactorStatus) bool {
	return status != nil && (status.Password || status.TOTP)
}

func exposureChoices(status *node.ExposureFactorStatus) [][2]string {
	var options [][2]string

	if status == nil || status.Password {
		options = append(options, [2]string{"Master password", "password"})
	}

	if status == nil || status.TOTP {
		options = append(options, [2]string{"Your authenticator code (TOTP)", "totp"})
	}

	return options
}

func chooseExposureFactor(cmd *cobra.Command, title string, options [][2]string) (string, error) {
	if richOutput(cmd) {
		value, err := ui.Choose(title, "", options)

		return value, promptErr(err)
	}

	var choices []string
	for i, option := range options {
		choices = append(choices, fmt.Sprintf("%d. %s", i+1, option[0]))
	}

	value, err := promptLine(cmd, title+" ("+strings.Join(choices, "; ")+")", "")
	if err != nil {
		return "", err
	}

	n, err := strconv.Atoi(value)
	if err != nil || n < 1 || n > len(options) {
		return "", output.NewError(output.CodeInputRequired, "choose a numbered exposure factor", output.ExitInput, "")
	}

	return options[n-1][1], nil
}

// exposureApproval sends credentials only in the RPC secret map, and binds
// them to the exact proposal returned by the node's dry run.
func exposureApproval(cmd *cobra.Command, p *progress, preview configSetPreview, description string) (map[string]string, error) {
	if len(preview.ExposureIncreases) == 0 {
		return nil, nil
	}

	if preview.ExposureProposal == "" {
		return nil, output.NewError(output.CodePrecondition, "the node did not return an exposure proposal", output.ExitPrecondition, "Retry after updating the node.")
	}

	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return nil, output.NewError(output.CodeInputRequired, "widening network exposure needs a human at a terminal", output.ExitInput, "Run without --yes or structured output and approve with an enrolled exposure factor.")
	}

	if description != "" {
		p.note(description)
	}

	p.note("Exposure increases: " + strings.Join(preview.ExposureIncreases, ", "))

	return promptApprovalFactor(cmd, preview.ExposureFactors, "Approve with")
}

func promptApprovalFactor(cmd *cobra.Command, status *node.ExposureFactorStatus, title string) (map[string]string, error) {
	options := exposureChoices(status)
	if len(options) == 0 {
		return nil, output.NewError(output.CodePrecondition, "no exposure factor is enrolled", output.ExitPrecondition, node.ExposureEnrollmentHint)
	}

	factor, err := chooseExposureFactor(cmd, title, options)
	if err != nil {
		return nil, err
	}

	switch factor {
	case "password":
		value, err := readSecretTTY(cmd, "Master password")
		if err != nil {
			return nil, err
		}

		secrets.Add(value)

		return map[string]string{"exposure_password": value}, nil
	case "totp":
		value, err := readProtectCode(cmd, "Current authenticator code")
		if err != nil {
			return nil, err
		}

		secrets.Add(value)

		return map[string]string{"exposure_totp": value}, nil
	default:
		return nil, output.NewError(output.CodeInputRequired, "choose password or totp", output.ExitInput, "Retry and choose an enrolled exposure factor.")
	}
}

func runAuthSetup(cmd *cobra.Command, _ []string) error {
	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return output.NewError(output.CodeInputRequired, "setting up an approval factor needs an interactive terminal", output.ExitInput, node.ExposureEnrollmentHint)
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	ctx := commandContext(cmd)

	reply, err := p.call(ctx, s.node, "auth.status", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var status node.ExposureFactorStatus
	if err := reply.Decode(&status); err != nil {
		return err
	}

	mode, err := chooseExposureFactor(cmd, "Set up an approval factor", exposureChoices(nil))
	if err != nil {
		return err
	}

	values, err := authSetupProof(cmd, status, mode)
	if err != nil {
		return err
	}

	label := s.ctx.Name
	if status.Operator != "" {
		label += "/" + status.Operator
	}

	localHash, err := prepareAuthFactor(cmd, label, mode, values)
	if err != nil {
		return err
	}

	if _, err := p.call(ctx, s.node, "auth.setup", map[string]string{"mode": mode},
		remote.CallOptions{Secrets: values, OnEvent: p.event}); err != nil {
		return err
	}

	if localHash != "" {
		if err := workspaces.SetMasterPasswordHash(s.ctx.Name, localHash); err != nil {
			return output.WrapError(output.CodeSecrets, err, output.ExitSecrets, "The node accepted the password but the local export password could not be saved.")
		}
	}

	return output.Success(cmd, output.Result{Data: map[string]string{"factor": mode}, Message: "Approval factor set up"})
}

func authSetupProof(cmd *cobra.Command, status node.ExposureFactorStatus, mode string) (map[string]string, error) {
	if mode == "totp" && !status.TOTP {
		return map[string]string{}, nil
	}

	if mode == "password" && status.Password {
		status.TOTP = false
	}

	options := exposureChoices(&status)
	if len(options) == 0 {
		return map[string]string{}, nil
	}

	factor, err := chooseExposureFactor(cmd, "Verify an existing factor", options)
	if err != nil {
		return nil, err
	}

	if factor == "password" {
		pw, err := readSecretTTY(cmd, "Current master password for this node")
		secrets.Add(pw)

		return map[string]string{"exposure_password": pw}, err
	}

	code, err := readProtectCode(cmd, "Current authenticator code")
	secrets.Add(code)

	return map[string]string{"exposure_totp": code}, err
}

func prepareAuthFactor(cmd *cobra.Command, workspace, mode string, values map[string]string) (string, error) {
	if mode == "password" {
		return prepareAuthPassword(cmd, values)
	}

	key, err := totp.NewSecret()
	if err != nil {
		return "", err
	}

	secrets.Add(key)

	if err := showApprovalSecret(cmd, workspace, key); err != nil {
		return "", err
	}

	code, err := readProtectCode(cmd, "Code from the new authenticator entry")
	secrets.Add(code)
	values["new_totp_secret"], values["new_totp_code"] = key, code

	return "", err
}

func showApprovalSecret(cmd *cobra.Command, workspace, key string) error {
	qr, err := ui.QR(totp.URI("vaire", workspace+" approval", key))
	if err != nil {
		return err
	}

	resume := ui.Suspend()
	defer resume()

	_, err = fmt.Fprintf(cmd.ErrOrStderr(), "Scan this QR code for Vaire approvals:\n\n%s\nOr add this key by hand: %s (time-based, SHA1, 6 digits, 30 seconds)\n\n", qr, groupSecret(key))

	return err
}

func prepareAuthPassword(cmd *cobra.Command, values map[string]string) (string, error) {
	pw, err := readSecretTTY(cmd, "New master password")
	if err != nil {
		return "", err
	}

	secrets.Add(pw)

	again, err := readSecretTTY(cmd, "Repeat the master password")
	if err != nil {
		return "", err
	}

	secrets.Add(again)

	if pw != again {
		return "", output.NewError(output.CodeInputRequired, "the master passwords do not match", output.ExitInput, "")
	}

	values["new_password"] = pw

	return password.Hash(pw)
}
