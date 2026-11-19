package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/totp"
	"github.com/spf13/cobra"
)

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

	factor, err := promptLine(cmd, "Exposure factor (password or totp)", "")
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(factor) {
	case "password":
		value, err := readSecretTTY(cmd, "Exposure password")
		if err != nil {
			return nil, err
		}

		secrets.Add(value)

		return map[string]string{"exposure_password": value}, nil
	case "totp":
		value, err := readProtectCode(cmd, "Current exposure authenticator code")
		if err != nil {
			return nil, err
		}

		secrets.Add(value)

		return map[string]string{"exposure_totp": value}, nil
	default:
		return nil, output.NewError(output.CodeInputRequired, "choose password or totp", output.ExitInput, "Retry and choose an enrolled exposure factor.")
	}
}

func runNodeExposureEnroll(cmd *cobra.Command, args []string) error {
	mode := args[0]
	if mode != "password" && mode != "totp" {
		return output.NewError(output.CodeUsage, "choose password or totp", output.ExitUsage, "")
	}

	if MustNotPrompt(cmd) || flagTrue(cmd, "yes") || os.Getuid() != 0 || os.Geteuid() != 0 {
		return output.NewError(output.CodeInputRequired, "exposure enrollment needs a root console and an interactive terminal", output.ExitInput, "Run sudo vaire node exposure-enroll password or totp at the node console, without --yes.")
	}

	factor, err := promptLine(cmd, "Existing exposure factor (password, totp, or none for first enrollment)", "")
	if err != nil {
		return err
	}

	var existingPassword, existingCode, newPassword string

	switch strings.ToLower(factor) {
	case "none":
	case "password":
		existingPassword, err = readSecretTTY(cmd, "Existing exposure password")
		secrets.Add(existingPassword)
	case "totp":
		existingCode, err = readProtectCode(cmd, "Current exposure authenticator code")
		secrets.Add(existingCode)
	default:
		return output.NewError(output.CodeInputRequired, "choose password, totp, or none", output.ExitInput, "")
	}

	if err != nil {
		return err
	}

	if mode == "password" {
		newPassword, err = readSecretTTY(cmd, "New exposure password")
		if err != nil {
			return err
		}

		secrets.Add(newPassword)

		again, err := readSecretTTY(cmd, "Repeat the new exposure password")
		if err != nil {
			return err
		}

		secrets.Add(again)

		if newPassword != again {
			return output.NewError(output.CodeInputRequired, "the passwords do not match", output.ExitInput, "Retry enrollment.")
		}
	}

	err = node.EnrollExposureFactor(commandContext(cmd), mode, existingPassword, existingCode, newPassword, func(secret string) (string, error) {
		if err := showProtectSecret(cmd, totp.URI("vaire", "exposure", secret), secret); err != nil {
			return "", err
		}

		code, err := readProtectCode(cmd, "Code from the new exposure authenticator entry")
		secrets.Add(code)

		return code, err
	})
	if err != nil {
		return err
	}

	return output.Success(cmd, output.Result{Data: map[string]string{"factor": mode}, Message: fmt.Sprintf("exposure %s enrolled", mode)})
}
