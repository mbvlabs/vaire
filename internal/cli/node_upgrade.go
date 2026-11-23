package cli

import (
	"fmt"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/node"
	"github.com/mbvlabs/vaire/internal/release"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/spf13/cobra"
)

func runNodeUpgrade(cmd *cobra.Command, _ []string) error {
	target := remote.LocalBuild()
	if target.Channel != release.ChannelRC && target.Channel != release.ChannelStable {
		return output.NewError(output.CodePrecondition, "node upgrades require a tagged RC or stable local build",
			output.ExitPrecondition, "Install a supported signed Vaire release on this workstation first.")
	}

	p := newProgress(cmd)

	s, err := openNodeSession(cmd, p, false)
	if err != nil {
		return err
	}
	defer s.Close()

	return upgradeConnectedNode(cmd, p, s, target)
}

func upgradeConnectedNode(cmd *cobra.Command, p *progress, s *session, target node.BuildID) error {
	ctx := commandContext(cmd)

	current, installed, err := s.node.Version(ctx)
	if err != nil {
		return err
	}

	if !installed {
		return output.NewError(output.CodePrecondition, "Vaire is not installed on this node", output.ExitPrecondition,
			"Use vaire server init to bootstrap a fresh server.")
	}

	if current.Version == target.Version && !remote.SameBuild(current, target) {
		return output.NewError(output.CodePrecondition, "the node reports the target version on a different channel",
			output.ExitPrecondition, "")
	}

	if remote.SameBuild(current, target) {
		return output.Success(cmd, output.Result{
			Data: node.InstallResult{Installed: current}, Message: "Node is already on " + current.Version, Warnings: s.warnings,
		})
	}

	if err := confirmNodeUpgrade(cmd, current, target); err != nil {
		return err
	}

	return installConnectedNode(cmd, p, s, target)
}

func confirmNodeUpgrade(cmd *cobra.Command, current, target node.BuildID) error {
	if flagTrue(cmd, "yes") {
		return nil
	}

	if MustNotPrompt(cmd) {
		return output.NewError(output.CodeInputRequired, "node upgrade needs confirmation", output.ExitInput,
			"Re-run vaire node upgrade --yes to approve the change.")
	}

	plan := fmt.Sprintf("Upgrade node from %s to %s", current.Version, target.Version)

	ok, err := PromptYes(cmd, plan+"?")
	if err != nil {
		return err
	}

	if !ok {
		return output.NewError(output.CodeUnsafeAction, "node upgrade not confirmed", output.ExitUnsafe, "")
	}

	return nil
}

func installConnectedNode(cmd *cobra.Command, p *progress, s *session, target node.BuildID) error {
	ctx := commandContext(cmd)

	params := map[string]string{"version": target.Version, "channel": target.Channel}

	reply, err := p.call(ctx, s.node, "upgrade.install", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var result node.InstallResult
	if err := reply.Decode(&result); err != nil {
		return err
	}

	if err := s.node.Handshake(ctx, remote.HandshakeOptions{}); err != nil {
		return err
	}

	return output.Success(cmd, output.Result{Data: result, Message: "Node upgraded to " + target.Version,
		Warnings: append(s.warnings, reply.Warnings...), Next: []string{"vaire status"}})
}
