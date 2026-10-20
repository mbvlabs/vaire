package cli

import (
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/spf13/cobra"
)

// discardResult is the result of vaire server init --discard.
type discardResult struct {
	Server           string `json:"server"`
	PendingPath      string `json:"pending_path"`
	HostBootstrapped bool   `json:"host_bootstrapped"`
	Host             string `json:"host,omitempty"`
}

// runInitDiscard deletes an unfinished init's local state. It never touches
// the server: a host that was already bootstrapped stays as it is.
func runInitDiscard(cmd *cobra.Command) error {
	name := stringFlag(cmd, "name")
	if name == "" {
		pending := contexts.PendingNames()
		if len(pending) != 1 {
			return output.NewError(output.CodeInputRequired, "--discard needs --name", output.ExitInput,
				"Pass --name with the server whose unfinished init to discard.").
				WithDetails(map[string]any{"flag": "--name", "pending": pending})
		}
		name = pending[0]
	}
	if !contexts.PendingExists(name) {
		return output.NewError(output.CodePrecondition, "there is no unfinished init for "+name, output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": "nothing_to_discard", "pending": contexts.PendingNames()})
	}
	res := discardResult{Server: name, PendingPath: contexts.PendingDir(name)}
	prog, err := contexts.LoadProgress(name)
	if err != nil {
		return err
	}
	_, boot, _ := contexts.LoadAnswers(name)
	res.Host = boot.Host
	res.HostBootstrapped = prog.Done(phaseHost)
	var warnings []output.Warning
	if res.HostBootstrapped {
		target := "the server"
		if boot.Host != "" {
			target = boot.Host
		}
		warnings = append(warnings, output.Warning{
			Code: "host_already_bootstrapped",
			Message: target + " was already bootstrapped by this init. Discarding drops the only credentials for it " +
				"(operator SSH key, WireGuard key, sudo password); rebuild the server before a new init.",
		})
	}
	if dryRun(cmd) {
		return output.Success(cmd, output.Result{Data: res, Message: "would delete " + res.PendingPath, Warnings: warnings})
	}
	p := newProgress(cmd)
	for _, w := range warnings {
		p.warn(w.Message)
	}
	if err := Confirm(cmd, "discard"); err != nil {
		return err
	}
	if err := contexts.RemovePending(name); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{Data: res, Message: "Discarded the unfinished init of " + name + " (" + res.PendingPath + ").", Warnings: warnings})
}
