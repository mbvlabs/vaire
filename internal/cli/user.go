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

func userCommands() cmdSpec {
	return cmdSpec{
		use: "user", run: parentRun,
		meta: CommandMeta{Summary: "Manage workspace users", WhenToUse: []string{"Create and list users; creating a user does not enroll a device."}, Examples: []string{"vaire user add alice", "vaire user list"}, Remote: true},
		children: []cmdSpec{
			{use: "add <user>", args: cobra.ExactArgs(1), run: runUserAdd,
				flags: []fl{flagString("role", "member", "Workspace role: owner or member")},
				meta:  CommandMeta{Summary: "Create a workspace user without enrolling a device", WhenToUse: []string{"Grant a role to a named user."}, Examples: []string{"vaire user add alice", "vaire user add alice --role owner"}, Mutating: true, SupportsDryRun: true, Remote: true}},
			{use: "list", args: cobra.NoArgs, run: runUserList,
				meta: CommandMeta{Summary: "List workspace users", WhenToUse: []string{"See users and their roles."}, Examples: []string{"vaire user list"}, Remote: true}},
			{use: "revoke <user>", args: cobra.ExactArgs(1), run: runUserRevoke,
				meta: CommandMeta{Summary: "Revoke a user and all their devices", WhenToUse: []string{"Remove a user's access and cancel outstanding setup codes."}, Examples: []string{"vaire user revoke alice --confirm alice"}, Mutating: true, Destructive: true, Remote: true, ConfirmToken: "{user}", SkipDryRunCheck: true, SkipDryRunReason: "revocation requires confirmation"}},
		},
	}
}

func runUserRevoke(cmd *cobra.Command, args []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	if err := Confirm(cmd, args[0]); err != nil {
		return err
	}

	reply, err := p.call(commandContext(cmd), s.node, "user.revoke", rpc.UserRevokeParams{User: args[0]}, remote.CallOptions{Confirm: args[0], OnEvent: p.event})
	if err != nil {
		return err
	}

	var user rpc.User
	if err := reply.Decode(&user); err != nil {
		return err
	}

	return output.Success(cmd, output.Result{Data: user, Message: fmt.Sprintf("Revoked user %s.", user.Name), Warnings: append(s.warnings, reply.Warnings...)})
}

func runUserAdd(cmd *cobra.Command, args []string) error {
	role := flagStringIf(cmd, "role")
	if role != "owner" && role != "member" {
		return output.NewError(output.CodeValidation, "role must be owner or member", output.ExitConfig, "")
	}

	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	reply, err := p.call(commandContext(cmd), s.node, "user.add", rpc.UserAddParams{Name: args[0], Role: role}, remote.CallOptions{DryRun: dryRun(cmd), OnEvent: p.event})
	if err != nil {
		return err
	}

	var user rpc.User
	if err := reply.Decode(&user); err != nil {
		return err
	}

	message := fmt.Sprintf("Created user %s.", user.Name)
	if dryRun(cmd) {
		message = fmt.Sprintf("Would create user %s.", user.Name)
	}

	return output.Success(cmd, output.Result{Data: user, Message: message, Warnings: append(s.warnings, reply.Warnings...)})
}

func runUserList(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)

	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()

	reply, err := p.call(commandContext(cmd), s.node, "user.list", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}

	var users rpc.UsersReply
	if err := reply.Decode(&users); err != nil {
		return err
	}

	var b strings.Builder

	w := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)

	_, _ = fmt.Fprintln(w, "USER\tROLE\tSTATUS")
	for _, user := range users.Users {
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\n", user.Name, user.Role, user.Status)
	}

	_ = w.Flush()

	return output.Success(cmd, output.Result{Data: users, Message: strings.TrimSpace(b.String()), Warnings: append(s.warnings, reply.Warnings...)})
}
