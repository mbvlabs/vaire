package release

import (
	"context"
	"errors"

	"github.com/mbvlabs/eru/internal/cli/output"
)

// Verifier checks a sigstore bundle over raw bytes for an expected identity.
type Verifier interface {
	VerifyBlob(ctx context.Context, content, bundle []byte, identity string) error
}

func verificationFailed(message string) error {
	return output.NewError(
		output.CodeReleaseVerification,
		message,
		output.ExitReleaseVerification,
		"Do not retry. Report this to a human.",
	)
}

func verificationWrap(err error) error {
	if err == nil {
		return nil
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		return err
	}
	return output.WrapError(
		output.CodeReleaseVerification,
		err,
		output.ExitReleaseVerification,
		"Do not retry. Report this to a human.",
	)
}

func unreachable(url string, err error) error {
	if err == nil {
		return output.NewError(
			output.CodeRemoteUnreachable,
			"release URL is unreachable",
			output.ExitTransport,
			"",
		).WithDetails(map[string]any{"url": url})
	}
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) {
		return err
	}
	return output.WrapError(
		output.CodeRemoteUnreachable,
		err,
		output.ExitTransport,
		"",
	).WithDetails(map[string]any{"url": url})
}

func invalidChannel(channel string) error {
	return output.NewError(
		output.CodeValidation,
		"release channel "+channel+" is invalid",
		output.ExitConfig,
		"Use stable, stable-<major>.x (for example stable-1.x) or edge.",
	).WithDetails(map[string]any{"channel": channel})
}
