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

// session is one operator command's connection to the primary: a dialer
// into the WireGuard tunnel (the context's shared tunnel daemon, or an
// in-process tunnel with ERU_TUNNEL=inprocess), SSH over it, and a checked
// node version (§3).
type session struct {
	ctx *contexts.Context
	// dialer reaches addresses inside the tunnel; closeDialer releases it
	// (the daemon lease, or the in-process tunnel).
	dialer      transport.Dialer
	closeDialer func()
	node        *remote.Node
	warnings    []output.Warning
	// dial connects SSH again over the tunnel, for reconnect.
	dial func(context.Context) (*transport.SSHClient, error)
}

// reconnect replaces the session's SSH connection with a new one, after the
// old one was lost. With the shared tunnel a dead daemon is respawned on the
// way. The node version was already checked on this session, so it skips
// the handshake.
func (s *session) reconnect(ctx context.Context) error {
	if s == nil || s.node == nil || s.dial == nil {
		return errors.New("this session can't reconnect")
	}
	s.node.SSH.Close()
	client, err := s.dial(ctx)
	if err != nil {
		return err
	}
	s.node.SSH = client
	return nil
}

// Close closes SSH and releases the tunnel. With the shared tunnel that
// only drops this process's lease; the daemon stays up for others.
func (s *session) Close() {
	if s == nil {
		return
	}
	if s.node != nil {
		s.node.SSH.Close()
	}
	if s.closeDialer != nil {
		s.closeDialer()
	}
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
	nodeSpec, ok := c.Master()
	if !ok {
		return nil, output.NewError(output.CodeConfig, "context "+c.Name+" lists no node", output.ExitConfig, "Recreate it with eru login.")
	}
	wgCfg, err := contextTunnelConfig(c, nodeSpec)
	if err != nil {
		return nil, err
	}
	sshKey, err := contexts.ReadFile(c.Name, contexts.SSHKeyFile)
	if err != nil {
		return nil, contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	auth, err := transport.KeyAuth(sshKey)
	if err != nil {
		return nil, contextFileError(c.Name, contexts.SSHKeyFile, err)
	}
	known, err := transport.LoadKnownHosts(contexts.Path(c.Name, contexts.KnownHostsFile))
	if err != nil {
		return nil, err
	}
	// The tunnel, SSH dial and version handshake are silent; the waiting
	// line covers them until the first RPC.
	defer p.wait("connecting to " + nodeLabel(c, nodeSpec))()
	ctx := commandContext(cmd)
	if err := s.openDialer(ctx, cmd, wgCfg, p); err != nil {
		return nil, err
	}
	target := transport.SSHTarget{User: c.Operator.SSHUser, Addr: net.JoinHostPort(nodeSpec.WGAddress, "22")}
	sshOpts := transport.SSHOptions{Auth: []ssh.AuthMethod{auth}, HostKeys: known, Timeout: timeoutFlag(cmd)}
	s.dial = func(ctx context.Context) (*transport.SSHClient, error) {
		return transport.DialSSH(ctx, s.dialer, target, sshOpts)
	}
	client, err := s.dial(ctx)
	if err != nil {
		s.Close()
		return nil, tunnelHint(err, nodeSpec.PublicAddress, nodeSpec.ListenPort)
	}
	s.node = &remote.Node{SSH: client, Operator: c.Operator.Name}
	if p.opts.Verbose {
		s.node.Stderr = os.Stderr
	}
	auto := flagTrue(cmd, "auto-upgrade-nodes") || c.AutoUpgradeNodes
	upgrading := func(line string) {
		p.note(line)
		p.setPhase("upgrading eru on " + nodeLabel(c, nodeSpec))
	}
	if err := s.node.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: auto, Progress: upgrading}); err != nil {
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

// nodeLabel names a node for the waiting line: its name, else its hostname,
// else the context's name.
func nodeLabel(c *contexts.Context, n contexts.Node) string {
	for _, name := range []string{n.Name, n.Hostname} {
		if name != "" {
			return name
		}
	}
	return c.Name
}

func contextFileError(name, file string, err error) error {
	return output.WrapError(output.CodeConfig, fmt.Errorf("read %s: %w", contexts.Path(name, file), err), output.ExitConfig,
		"Recreate the context with eru login <secrets.age>.")
}

func (s *session) confirmLockdown(ctx context.Context, p *progress) error {
	reply, err := p.call(ctx, s.node, "ssh.confirm", map[string]bool{"auto": true}, remote.CallOptions{OnEvent: p.event})
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

// tunnelConfig is the userspace tunnel to a node. Only the node /24 routes
// through it.
func tunnelConfig(privateKey, operatorAddr, nodePub, publicAddr string, port int, nodeWG string) (transport.WireGuardConfig, error) {
	opAddr, err := netip.ParseAddr(operatorAddr)
	if err != nil {
		return transport.WireGuardConfig{}, output.NewError(output.CodeConfig, "operator wg_address "+operatorAddr+" is invalid", output.ExitConfig, "")
	}
	pub, err := netip.ParseAddr(publicAddr)
	if err != nil {
		return transport.WireGuardConfig{}, output.NewError(output.CodeConfig, "node public_address "+publicAddr+" is invalid", output.ExitConfig, "")
	}
	wg, err := netip.ParseAddr(nodeWG)
	if err != nil {
		return transport.WireGuardConfig{}, output.NewError(output.CodeConfig, "node wg_address "+nodeWG+" is invalid", output.ExitConfig, "")
	}
	if port == 0 {
		port = 51820
	}
	return transport.WireGuardConfig{
		PrivateKey:    strings.TrimSpace(privateKey),
		Address:       opAddr,
		PeerPublicKey: nodePub,
		Endpoint:      netip.AddrPortFrom(pub, uint16(port)), //nolint:gosec // G115: listen_port is validated as 1-65535.
		AllowedIPs:    []netip.Prefix{netip.PrefixFrom(wg, 24).Masked()},
	}, nil
}

// contextTunnelConfig is the tunnel to a context's node, with the operator
// key read from the context directory.
func contextTunnelConfig(c *contexts.Context, n contexts.Node) (transport.WireGuardConfig, error) {
	wgKey, err := contexts.ReadFile(c.Name, contexts.WGKeyFile)
	if err != nil {
		return transport.WireGuardConfig{}, contextFileError(c.Name, contexts.WGKeyFile, err)
	}
	secrets.Add(strings.TrimSpace(string(wgKey)))
	return tunnelConfig(string(wgKey), c.Operator.WGAddress, n.WGPublicKey, n.PublicAddress, n.ListenPort, n.WGAddress)
}

// upTunnel starts an in-process userspace tunnel to a node, for eru server
// init and ERU_TUNNEL=inprocess. It first takes the context's tunnel lock
// (see tunnellock.go), waiting while another eru process or the context's
// tunnel daemon holds it, and holds it until the tunnel closes: two devices
// with one operator key would steal the node's peer from each other.
func upTunnel(ctx context.Context, contextName, privateKey, operatorAddr, nodePub, publicAddr string, port int, nodeWG string, p *progress) (*transport.Tunnel, error) {
	cfg, err := tunnelConfig(privateKey, operatorAddr, nodePub, publicAddr, port, nodeWG)
	if err != nil {
		return nil, err
	}
	if p != nil && p.opts.Verbose {
		cfg.Logf = func(format string, args ...any) { p.note("wireguard: " + fmt.Sprintf(format, args...)) }
	}
	release, err := lockTunnel(ctx, contextName, p)
	if err != nil {
		return nil, err
	}
	tun, err := transport.UpWireGuard(cfg)
	if err != nil {
		release()
		return nil, err
	}
	tun.OnClose(release)
	return tun, nil
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
	var params any
	if db := stringFlag(cmd, "db"); db != "" {
		params = map[string]string{"db": db}
	}
	reply, err := p.call(commandContext(cmd), s.node, "plan", params, remote.CallOptions{OnEvent: p.event})
	if err != nil {
		return err
	}
	var data engine.PlanData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	return output.Success(cmd, output.Result{
		Data:     data,
		Message:  planMessage(fmt.Sprintf("%d of %d steps would apply on %s", data.WouldApply, data.Total, s.ctx.Name), data),
		Warnings: append(s.warnings, reply.Warnings...),
	})
}

func runApply(cmd *cobra.Command, _ []string) error {
	// config set/edit --apply run this too, without a --step flag.
	var stepIDs []string
	if cmd.Flags().Lookup("step") != nil {
		var err error
		if stepIDs, err = cmd.Flags().GetStringSlice("step"); err != nil {
			return err
		}
	}
	p := newProgress(cmd)
	s, err := openSession(cmd, p)
	if err != nil {
		return err
	}
	defer s.Close()
	dry := dryRun(cmd)
	secretValues, creds, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}
	// The recovery kit rides along so a node that lost its copy gets it
	// back; the node writes it only when missing (D7). Backups that are on
	// without a kit here is input_required: eru never makes a new cipher for
	// an existing repository.
	if creds.Backup != nil && creds.Backup.HasBucket() {
		addRecoveryKit(secretValues, creds.Backup)
	} else if cfg, _, err := remoteConfig(cmd, s, p); err == nil && cfg.BackupConfigured() {
		return output.NewError(output.CodeInputRequired, "backups are on, but this machine's context has no recovery kit", output.ExitInput,
			"Log in from a current transfer bundle (eru login), which carries the recovery kit, then retry.").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}
	params := map[string]any{}
	if len(stepIDs) > 0 {
		params["steps"] = stepIDs
	}
	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}
	confirm, confirms := confirmOptions(cmd)
	// The summary names the pending restarts and next steps carry the
	// command, so the human output leaves out the node's restart_pending
	// warning (JSON keeps it).
	p.hideWarning("restart_pending")
	reply, err := p.call(commandContext(cmd), s.node, "apply", params, remote.CallOptions{
		Secrets:  secretValues,
		DryRun:   dry,
		Confirm:  confirm,
		Confirms: confirms,
		OnEvent:  p.event,
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
			Message:  planMessage(fmt.Sprintf("dry run: %d of %d steps would apply", data.WouldApply, data.Total), data),
			Warnings: warnings,
		})
	}
	var data engine.ApplyData
	if err := reply.Decode(&data); err != nil {
		return err
	}
	warnings = append(warnings, syncContextFromNode(cmd, s, p)...)
	message := fmt.Sprintf("applied %d, skipped %d of %d steps", data.Applied, data.Skipped, data.Total)
	if data.Restarted() {
		message += "; restarted " + restartSummary(data.Restarts)
	}
	var next []string
	if len(data.RestartPending) > 0 {
		message += "; restart pending for " + strings.Join(data.RestartPending, ", ")
		next = []string{"eru apply " + confirmFlags(data.ConfirmTokens)}
		for _, w := range warnings {
			if w.Code == "restart_pending" {
				p.shown(w.Message)
			}
		}
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
func planMessage(header string, data engine.PlanData) string {
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
			"run eru apply "+confirmFlags(data.ConfirmTokens)+" to restart those clusters")
	}
	return strings.Join(lines, "\n")
}

// restartSummary lists the restarted clusters with how long each was
// unavailable.
func restartSummary(restarts []engine.ClusterRestart) string {
	parts := make([]string, len(restarts))
	for i, r := range restarts {
		parts[i] = fmt.Sprintf("%s (%d ms unavailable)", r.Cluster, r.DowntimeMS)
	}
	return strings.Join(parts, ", ")
}
