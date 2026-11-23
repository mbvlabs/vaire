package cli

import (
	"fmt"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/spf13/cobra"
)

func runWhoami(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	reply, err := p.call(commandContext(cmd), s.node, "whoami", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var identity rpc.WhoamiReply
	if err := reply.Decode(&identity); err != nil {
		return err
	}

	return output.Success(cmd, output.Result{
		Data: struct {
			Workspace string `json:"workspace"`
			rpc.WhoamiReply
		}{Workspace: s.ctx.Name, WhoamiReply: identity},
		Message:  whoamiMessage(s.ctx.Name, identity),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func whoamiMessage(workspace string, identity rpc.WhoamiReply) string {
	selection := fmt.Sprintf("Workspace   %s\nServer      %s\nNode        %s\n", workspace, identity.Server, identity.Node)
	switch identity.IdentityKind {
	case rpc.IdentityVerifiedDevice:
		return selection + fmt.Sprintf("Operator    %s (%s)\nDevice      %s\nIdentity    verified by %s",
			identity.Operator, identity.Role, identity.Device, identity.Node)
	case rpc.IdentityLegacyShared:
		return selection + fmt.Sprintf("SSH account %s\nIdentity    shared login (operator and device not individually verified)", identity.SSHAccount)
	default:
		return selection + "Identity    not verified"
	}
}
