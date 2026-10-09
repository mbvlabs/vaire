package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/spf13/cobra"
)

// MustNotPrompt reports whether this invocation has to receive values from flags.
func MustNotPrompt(cmd *cobra.Command) bool {
	if cmd != nil && cmd.Flags() != nil {
		if flag := cmd.Flags().Lookup("no-input"); flag != nil && flag.Value.String() == "true" {
			return true
		}
	}
	opts, err := output.ParseOptions(cmd)
	if err == nil && output.UsesStructuredOutput(opts) {
		return true
	}
	return !stdinIsTTY()
}

// RequireFlag returns input_required when a prompt would be needed and this
// invocation cannot prompt. provided means the flag already has a value.
func RequireFlag(cmd *cobra.Command, provided bool, flag, action string) error {
	if provided {
		return nil
	}
	if !MustNotPrompt(cmd) {
		return nil
	}
	return output.NewError(
		output.CodeInputRequired,
		action+" needs "+flag,
		output.ExitInput,
		"Pass "+flag+" to continue without a prompt.",
	).WithDetails(map[string]any{"flag": flag})
}

// Confirm requires the operator to pass or type token.
// --yes never satisfies this check.
func Confirm(cmd *cobra.Command, token string) error {
	if strings.TrimSpace(token) == "" {
		return output.NewError(
			output.CodeUnsafeAction,
			"confirmation token is empty",
			output.ExitUnsafe,
			"Refusing to confirm an empty token.",
		)
	}
	got, err := cmd.Flags().GetString("confirm")
	if err != nil {
		return output.NewError(output.CodeUsage, err.Error(), output.ExitUsage, "")
	}
	details := map[string]any{"confirm_token": token}
	if got == token {
		return nil
	}
	if got != "" {
		return output.NewError(
			output.CodeUnsafeAction,
			"confirmation token does not match",
			output.ExitUnsafe,
			"re-run with --confirm "+token,
		).WithDetails(details)
	}
	if MustNotPrompt(cmd) {
		return output.NewError(
			output.CodeUnsafeAction,
			"this action requires confirmation",
			output.ExitUnsafe,
			"re-run with --confirm "+token,
		).WithDetails(details)
	}
	if _, err := fmt.Fprintf(cmd.OutOrStdout(), "Type %s to confirm: ", token); err != nil {
		return err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return output.WrapError(output.CodeError, err, output.ExitGeneric, "")
	}
	if strings.TrimSpace(line) != token {
		return output.NewError(
			output.CodeUnsafeAction,
			"confirmation was not accepted",
			output.ExitUnsafe,
			"re-run with --confirm "+token,
		).WithDetails(details)
	}
	return nil
}

func dryRun(cmd *cobra.Command) bool {
	if cmd == nil || cmd.Flags() == nil {
		return false
	}
	flag := cmd.Flags().Lookup("dry-run")
	if flag == nil {
		return false
	}
	value, err := strconv.ParseBool(flag.Value.String())
	return err == nil && value
}

func stdinIsTTY() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
