package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/spf13/cobra"
)

func deviceCommands() cmdSpec {
	return cmdSpec{
		use: "device", run: parentRun,
		meta: CommandMeta{Summary: "Manage workspace devices", WhenToUse: []string{"Issue setup codes and revoke individual devices."}, Examples: []string{"vaire device add alice laptop", "vaire device list"}, Remote: true},
		children: []cmdSpec{
			{use: "add <user> <device>", args: cobra.ExactArgs(2), run: runDeviceAdd,
				flags: []fl{flagBool("fresh", "Replace an unused or expired setup code")},
				meta:  CommandMeta{Summary: "Issue a one-time device setup code", WhenToUse: []string{"Share the code through a trusted channel. Use --fresh for an unused or expired code, not an enrolled device."}, Examples: []string{"vaire device add alice laptop", "vaire device add alice laptop --fresh"}, Mutating: true, Remote: true, SkipDryRunCheck: true, SkipDryRunReason: "issuing a setup code has no host step to preview"}},
			{use: "list", args: cobra.NoArgs, run: runDeviceList, flags: []fl{flagString("user", "", "Only devices belonging to this user")},
				meta: CommandMeta{Summary: "List workspace devices", WhenToUse: []string{"See pending, active and revoked devices."}, Examples: []string{"vaire device list", "vaire device list --user alice"}, Remote: true}},
			{use: "revoke <user> <device>", args: cobra.ExactArgs(2), run: runDeviceRevoke,
				meta: CommandMeta{Summary: "Revoke one device or pending setup", WhenToUse: []string{"Confirm the resolved device ID to remove its access."}, Examples: []string{"vaire device revoke alice laptop"}, Mutating: true, Destructive: true, Remote: true, ConfirmToken: "{device_id}", SkipDryRunCheck: true, SkipDryRunReason: "revocation requires confirmation"}},
		},
	}
}

func runDeviceAdd(cmd *cobra.Command, args []string) error {
	opts, err := output.ParseOptions(cmd)
	if err != nil {
		return err
	}

	if opts.Quiet && !output.UsesStructuredOutput(opts) {
		return output.NewError(output.CodeValidation, "quiet human output would hide the one-time setup code", output.ExitConfig, "Remove --quiet or use structured output.")
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	// The code belongs only in the final issuing response, never in progress.
	reply, err := p.call(commandContext(cmd), s.node, "device.add", rpc.DeviceAddParams{User: args[0], Label: args[1], Fresh: flagTrue(cmd, "fresh")}, remote.CallOptions{OnEvent: func(output.Event) {}})
	if err != nil {
		return err
	}

	var device rpc.Device
	if err := reply.Decode(&device); err != nil {
		return err
	}

	if device.SetupCode == "" {
		return output.NewError(output.CodeError, "node did not return a setup code", output.ExitGeneric, "")
	}

	return output.Success(cmd, output.Result{Data: device, Message: fmt.Sprintf("Share this one-time setup code with %s through a trusted channel: %s", device.User, device.SetupCode), Warnings: append(s.warnings, reply.Warnings...)})
}

func runDeviceList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	reply, err := p.call(commandContext(cmd), s.node, "device.list", rpc.DeviceListParams{User: flagStringIf(cmd, "user")}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var devices rpc.DevicesReply
	if err := reply.Decode(&devices); err != nil {
		return err
	}

	var b strings.Builder

	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "USER\tDEVICE\tID\tSTATUS")

	for i := range devices.Devices {
		device := &devices.Devices[i]
		device.SetupCode = ""
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", device.User, device.Label, device.ID, device.Status)
	}

	_ = w.Flush()

	return output.Success(cmd, output.Result{Data: devices, Message: strings.TrimSpace(b.String()), Warnings: append(s.warnings, reply.Warnings...)})
}

func runDeviceRevoke(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	return revokeDevice(cmd, p, s.node, args[0], args[1], s.warnings)
}

func revokeDevice(cmd *cobra.Command, p *progress, node remote.Caller, user, label string, warnings []output.Warning) error {
	params := rpc.DeviceRevokeParams{User: user, Label: label}

	reply, err := p.call(commandContext(cmd), node, "device.revoke", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var device rpc.Device
	if err := reply.Decode(&device); err != nil {
		return err
	}

	if device.ID == "" || device.User != user || device.Label != label {
		return output.NewError(output.CodeError, "node returned an invalid device confirmation target", output.ExitGeneric, "")
	}

	if err := confirmDeviceRevoke(cmd, device); err != nil {
		return err
	}

	params.ID = device.ID

	reply, err = p.call(commandContext(cmd), node, "device.revoke", params, remote.CallOptions{Confirm: device.ID, OnEvent: p.event})
	if err != nil {
		return err
	}

	if err := reply.Decode(&device); err != nil {
		return err
	}

	device.SetupCode = ""

	return output.Success(cmd, output.Result{Data: device, Message: fmt.Sprintf("Revoked %s (%s).", device.Label, device.User), Warnings: append(warnings, reply.Warnings...)})
}

func confirmDeviceRevoke(cmd *cobra.Command, device rpc.Device) error {
	if len(confirmTokens(cmd)) != 0 || MustNotPrompt(cmd) || flagTrue(cmd, "yes") {
		return Confirm(cmd, device.ID)
	}

	ok, err := PromptYes(cmd, fmt.Sprintf("Revoke device %s for %s?", device.Label, device.User))
	if err != nil {
		return err
	}

	if !ok {
		return output.NewError(output.CodeUnsafeAction, "device revocation cancelled", output.ExitUnsafe, "")
	}

	return nil
}
