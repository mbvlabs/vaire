package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

func withWorkspaceIdentity(run func(*cobra.Command, []string) error) func(*cobra.Command, []string) error {
	if run == nil {
		return nil
	}

	return func(cmd *cobra.Command, args []string) error {
		if _, pinned := os.LookupEnv(workspaces.EnvIdentityPin); pinned {
			return run(cmd, args)
		}

		name := strings.TrimSpace(flagStringIf(cmd, "workspace"))
		if name == "" {
			name = strings.TrimSpace(os.Getenv(workspaces.EnvWorkspace))
		}

		if name == "" {
			name, _ = workspaces.Current()
		}

		if name == "" || workspaces.ValidateName(name) != nil || !workspaces.Exists(name) {
			return run(cmd, args)
		}

		return runWithIdentityPin(cmd, args, run, name)
	}
}

func runWithIdentityPin(cmd *cobra.Command, args []string, run func(*cobra.Command, []string) error, name string) error {
	id, err := workspaces.ActiveIdentity(name)
	if err != nil {
		return err
	}

	if err := workspaces.PinIdentity(name, id); err != nil {
		return err
	}

	defer func() { _ = os.Unsetenv(workspaces.EnvIdentityPin) }()

	return run(cmd, args)
}

func workspaceIdentityCommands() cmdSpec {
	return cmdSpec{
		use: "operator", run: parentRun,
		meta: CommandMeta{Summary: "List and switch signed-in operators in one workspace",
			WhenToUse: []string{"Access one workspace as different signed-in operators."}, Examples: []string{"vaire -w thelab workspace operator list"}},
		children: []cmdSpec{
			{use: "list", args: cobra.NoArgs, run: runWorkspaceIdentityList,
				meta: CommandMeta{Summary: "List signed-in operators", WhenToUse: []string{"See which operators and devices are available and which is active."},
					Examples: []string{"vaire -w thelab workspace operator list"}}},
			{use: "use <operator>", args: cobra.ExactArgs(1), run: runWorkspaceIdentityUse,
				flags: []fl{flagString("device", "", "Device label (required when the operator has more than one enrolled device here)")},
				meta: CommandMeta{Summary: "Select a signed-in operator", WhenToUse: []string{"Switch operators without changing workspaces."},
					Examples: []string{"vaire -w thelab workspace operator use alice", "vaire -w thelab workspace operator use alice --device laptop"},
					Mutating: true, SkipDryRunCheck: true, SkipDryRunReason: "switches a local identity selection"}},
		},
	}
}

type workspaceIdentityRow struct {
	id       string
	Operator string `json:"operator"`
	Device   string `json:"device,omitempty"`
	Active   bool   `json:"active"`
}

func workspaceIdentityRows(name string) ([]workspaceIdentityRow, error) {
	ids, err := workspaces.ListIdentities(name)
	if err != nil {
		return nil, err
	}

	active, err := workspaces.ActiveIdentity(name)
	if err != nil {
		return nil, err
	}

	rows := make([]workspaceIdentityRow, 0, len(ids))
	for _, id := range ids {
		w, err := workspaces.LoadIdentity(name, id)
		if err != nil {
			return nil, err
		}

		device := w.Operator.Device
		if device == "" {
			device = w.Operator.SSHUser
		}

		rows = append(rows, workspaceIdentityRow{id: id, Operator: w.Operator.Name, Device: device, Active: id == active})
	}

	return rows, nil
}

func runWorkspaceIdentityList(cmd *cobra.Command, _ []string) error {
	w, err := workspaces.Resolve(stringFlag(cmd, "workspace"))
	if err != nil {
		return err
	}

	rows, err := workspaceIdentityRows(w.Name)
	if err != nil {
		return err
	}

	var text strings.Builder
	text.WriteString("ACTIVE\tOPERATOR\tDEVICE\n")

	for _, row := range rows {
		marker := " "
		if row.Active {
			marker = "*"
		}

		_, _ = fmt.Fprintf(&text, "%s\t%s\t%s\n", marker, row.Operator, row.Device)
	}

	return output.OK(cmd, rows, strings.TrimRight(text.String(), "\n"))
}

func runWorkspaceIdentityUse(cmd *cobra.Command, args []string) error {
	w, err := workspaces.Resolve(stringFlag(cmd, "workspace"))
	if err != nil {
		return err
	}

	rows, err := workspaceIdentityRows(w.Name)
	if err != nil {
		return err
	}

	row, err := selectWorkspaceIdentity(rows, args[0], stringFlag(cmd, "device"))
	if err != nil {
		return err
	}

	if err := workspaces.SetActiveIdentity(w.Name, row.id); err != nil {
		return err
	}

	row.Active = true

	return output.OK(cmd, row, fmt.Sprintf("Using operator %s in workspace %s.", row.Operator, w.Name))
}

func selectWorkspaceIdentity(rows []workspaceIdentityRow, operator, device string) (workspaceIdentityRow, error) {
	var matches []workspaceIdentityRow

	for _, row := range rows {
		if row.Operator == operator && (device == "" || row.Device == device) {
			matches = append(matches, row)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return workspaceIdentityRow{}, fmt.Errorf("no signed-in operator matches %q; run vaire workspace operator list", operator)
	default:
		return workspaceIdentityRow{}, fmt.Errorf("operator %q has multiple devices here; select one with --device", operator)
	}
}
