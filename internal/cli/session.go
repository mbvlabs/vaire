package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eru/internal/cli/output"
	"github.com/mbvlabs/eru/internal/contexts"
	"github.com/mbvlabs/eru/internal/engine"
	"github.com/mbvlabs/eru/internal/remote"
	"github.com/mbvlabs/eru/internal/secrets"
	"github.com/mbvlabs/eru/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// session is one operator command's connection to the primary: the
// userspace WireGuard tunnel, SSH over it, and a checked node version (§3).
type session struct {
	ctx      *contexts.Context
	tunnel   *transport.Tunnel
	node     *remote.Node
	warnings []output.Warning
}

func (s *session) Close() {
	if s == nil {
		return
	}
	if s.node != nil {
		s.node.SSH.Close()
	}
	s.tunnel.Close()
}

func commandContext(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil {
		return ctx
	}
	return context.Background()
}

func timeoutFlag(cmd *cobra.Command) time.Duration {
	if d, err := cmd.Flags().GetDuration("timeout"); err == nil && d > 0 {
		return d
	}
	return 30 * time.Second
}

// openSession resolves the context, brings up the tunnel, connects SSH over
// it, runs the version handshake, and confirms a pending SSH lockdown the
// first time this context reaches the node (§8.5).
func openSession(cmd *cobra.Command, p *progress) (*session, error) {
	c, err := contexts.Resolve(stringFlag(cmd, "context"))
	if err != nil {
		return nil, err
	}
	s := &session{ctx: c, warnings: contexts.FixModes(c.Name)}
	nodeSpec, ok := c.Primary()
	if !ok {
		return nil, output.NewError(output.CodeConfig, "context "+c.Name+" lists no node", output.ExitConfig, "Recreate it with eru login.")
	}
	wgKey, err := contexts.ReadFile(c.Name, contexts.WGKeyFile)
	if err != nil {
		return nil, contextFileError(c.Name, contexts.WGKeyFile, err)
	}
	sshKey, err := contexts.ReadFile(c.Name, contexts.SSHKeyFile)
	if err != nil {
		return nil, contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	secrets.Add(strings.TrimSpace(string(wgKey)))
	auth, err := transport.KeyAuth(sshKey)
	if err != nil {
		return nil, contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	known, err := transport.LoadKnownHosts(contexts.Path(c.Name, contexts.KnownHostsFile))
	if err != nil {
		return nil, err
	}
	tun, err := upTunnel(string(wgKey), c.Operator.WGAddress, nodeSpec.WGPublicKey, nodeSpec.PublicAddress, nodeSpec.ListenPort, nodeSpec.WGAddress, p)
	if err != nil {
		return nil, err
	}
	s.tunnel = tun
	ctx := commandContext(cmd)
	target := transport.SSHTarget{User: c.Operator.SSHUser, Addr: net.JoinHostPort(nodeSpec.WGAddress, "22")}
	client, err := transport.DialSSH(ctx, tun, target, transport.SSHOptions{Auth: []ssh.AuthMethod{auth}, HostKeys: known, Timeout: timeoutFlag(cmd)})
	if err != nil {
		s.Close()
		return nil, tunnelHint(err, nodeSpec.PublicAddress, nodeSpec.ListenPort)
	}
	s.node = &remote.Node{SSH: client, Operator: c.Operator.Name}
	if p.opts.Verbose {
		s.node.Stderr = os.Stderr
	}
	auto := flagTrue(cmd, "auto-upgrade-nodes") || c.AutoUpgradeNodes
	if err := s.node.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: auto, Progress: p.note}); err != nil {
		s.Close()
		return nil, err
	}
	if c.LockdownConfirmedAt == "" {
		if err := s.confirmLockdown(ctx, p); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

func contextFileError(name, file string, err error) error {
	return output.WrapError(output.CodeConfig, fmt.Errorf("read %s: %w", contexts.Path(name, file), err), output.ExitConfig,
		"Recreate the context with eru login <secrets.age>.")
}

func (s *session) confirmLockdown(ctx context.Context, p *progress) error {
	reply, err := s.node.Call(ctx, "ssh.confirm", map[string]bool{"auto": true}, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var res struct {
		Confirmed    bool   `json:"confirmed"`
		LockedDownAt string `json:"locked_down_at"`
	}
	if err := reply.Decode(&res); err != nil {
		return err
	}
	if res.Confirmed {
		p.note("confirmed the SSH lockdown over WireGuard; public SSH is closed")
		s.warnings = append(s.warnings, output.Warning{Code: "lockdown_confirmed", Message: "public SSH on the node is now closed; SSH works only over WireGuard"})
	}
	s.ctx.LockdownConfirmedAt = res.LockedDownAt
	if s.ctx.LockdownConfirmedAt == "" {
		s.ctx.LockdownConfirmedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return contexts.Save(s.ctx)
}

// upTunnel starts the userspace tunnel to a node. Only the node /24 routes
// through it.
func upTunnel(privateKey, operatorAddr, nodePub, publicAddr string, port int, nodeWG string, p *progress) (*transport.Tunnel, error) {
	opAddr, err := netip.ParseAddr(operatorAddr)
	if err != nil {
		return nil, output.NewError(output.CodeConfig, "operator wg_address "+operatorAddr+" is invalid", output.ExitConfig, "")
	}
	pub, err := netip.ParseAddr(publicAddr)
	if err != nil {
		return nil, output.NewError(output.CodeConfig, "node public_address "+publicAddr+" is invalid", output.ExitConfig, "")
	}
	wg, err := netip.ParseAddr(nodeWG)
	if err != nil {
		return nil, output.NewError(output.CodeConfig, "node wg_address "+nodeWG+" is invalid", output.ExitConfig, "")
	}
	if port == 0 {
		port = 51820
	}
	nodes := netip.PrefixFrom(wg, 24).Masked()
	var logf func(string, ...any)
	if p != nil && p.opts.Verbose {
		logf = func(format string, args ...any) { p.note("wireguard: " + fmt.Sprintf(format, args...)) }
	}
	return transport.UpWireGuard(transport.WireGuardConfig{
		PrivateKey:    strings.TrimSpace(privateKey),
		Address:       opAddr,
		PeerPublicKey: nodePub,
		Endpoint:      netip.AddrPortFrom(pub, uint16(port)), //nolint:gosec // G115: listen_port is validated as 1-65535.
		AllowedIPs:    []netip.Prefix{nodes},
		Logf:          logf,
	})
}

// tunnelHint replaces a bare dial failure inside the tunnel with what to check.
func tunnelHint(err error, publicAddr string, port int) error {
	var cliErr *output.CLIError
	if errors.As(err, &cliErr) && cliErr.Code == output.CodeRemoteUnreachable {
		cliErr.Code = output.CodeTransportFailed
		cliErr.Hint = "WireGuard to " + net.JoinHostPort(publicAddr, strconv.Itoa(port)) + " did not answer. Check that UDP " +
			strconv.Itoa(port) + " leaves this network and reaches the server, and that wg0 runs there. Break-glass: eru node unlock-ssh from the provider console."
	}
	return err
}

// dialRetryReport dials SSH until it works or the deadline passes. Host key
// and authentication failures stop at once. onAttempt, when set, hears about
// each attempt and the deadline, for a live status line.
func dialRetryReport(ctx context.Context, d transport.Dialer, target transport.SSHTarget, opts transport.SSHOptions, within time.Duration,
	onAttempt func(attempt int, deadline time.Time),
) (*transport.SSHClient, error) {
	deadline := time.Now().Add(within)
	for n := 1; ; n++ {
		if onAttempt != nil {
			onAttempt(n, deadline)
		}
		attempt := opts
		if left := time.Until(deadline); left < attempt.Timeout || attempt.Timeout == 0 {
			attempt.Timeout = left
		}
		if attempt.Timeout < time.Second {
			attempt.Timeout = time.Second
		}
		client, err := transport.DialSSH(ctx, d, target, attempt)
		if err == nil {
			return client, nil
		}
		var cliErr *output.CLIError
		if errors.As(err, &cliErr) && (cliErr.Code == output.CodeHostKeyMismatch || isAuthFailure(cliErr)) {
			return nil, err
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func isAuthFailure(e *output.CLIError) bool {
	details, ok := e.Details.(map[string]any)
	return ok && details["reason"] == "auth_failed"
}

func runPlan(cmd *cobra.Command, _ []string) error {
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	reply, err := s.node.Call(commandContext(cmd), "plan", nil, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data engine.PlanData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  planMessage(fmt.Sprintf("%d of %d steps would apply on %s", data.WouldApply, data.Total, s.ctx.Name), data, s.ctx.Name),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runApply(cmd *cobra.Command, _ []string) error {
	stepIDs, err := cmd.Flags().GetStringSlice("step")
	if err != nil {
		return err
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := dryRun(cmd)
	// D7: top up any missing eru_admin and service secrets before apply.
	// Application database passwords are never generated here.
	creds, err := contexts.LoadCredentials(s.ctx.Name)
	if err != nil {
		return err
	}
	if creds.Database == nil {
		creds.Database = &contexts.DatabaseCreds{}
	}
	secretValues, generated, err := ensureClusterSecrets(creds.Database)
	if err != nil {
		return err
	}
	if generated {
		if err := contexts.SaveCredentials(s.ctx.Name, creds); err != nil {
			return err
		}
	}
	var params any
	if len(stepIDs) > 0 {
		params = map[string][]string{"steps": stepIDs}
	}
	reply, err := s.node.Call(commandContext(cmd), "apply", params, remote.CallOptions{
		Secrets: secretValues,
		DryRun:  dry,
		Confirm: stringFlag(cmd, "confirm"),
		OnEvent: p.event,
	})
	if err != nil {
		return err
	}
	warnings := append(append(s.warnings, reply.Warnings...), staleWarning(generated)...)
	if dry {
		var data engine.PlanData
		if err := reply.Decode(&data); err != nil {
			return err
		}
		return output.Success(cmd, output.Result{
			Data:     data,
			Message:  planMessage(fmt.Sprintf("dry run: %d of %d steps would apply", data.WouldApply, data.Total), data, s.ctx.Name),
			Warnings: warnings,
		})
	}
	var data engine.ApplyData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	warnings = append(warnings, syncContextFromNode(cmd, s, p)...)
	message := fmt.Sprintf("applied %d, skipped %d of %d steps", data.Applied, data.Skipped, data.Total)
	if data.Restarted {
		message += fmt.Sprintf("; PostgreSQL restarted (%d ms unavailable)", data.RestartDowntimeMS)
	}
	var next []string
	if len(data.RestartPending) > 0 {
		message += "\nrestart pending for: " + strings.Join(data.RestartPending, ", ")
		next = []string{"eru apply --confirm " + s.ctx.Name + "/restart"}
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  message,
		Warnings: warnings,
		Next:     next,
	})
}

// planMessage renders a plan for humans: one line per step, the file diffs of
// the steps that would apply, and any pending restart.
func planMessage(header string, data engine.PlanData, cluster string) string {
	lines := []string{header}
	for _, step := range data.Steps {
		lines = append(lines, fmt.Sprintf("  %-5s %s (%s)", step.Action, step.ID, step.Reason))
	}
	for _, step := range data.Steps {
		for _, d := range step.Diffs {
			lines = append(lines, "", strings.TrimRight(d.Diff, "\n"))
		}
	}
	if len(data.RestartPending) > 0 {
		lines = append(lines, "", "restart pending for: "+strings.Join(data.RestartPending, ", "),
			"run eru apply --confirm "+cluster+"/restart to restart PostgreSQL")
	}
	return strings.Join(lines, "\n")
}
