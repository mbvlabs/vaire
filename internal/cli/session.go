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

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/secrets"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// session is one operator command's connection to the primary: a dialer
// into the WireGuard tunnel (the workspace's shared tunnel daemon, or an
// in-process tunnel with VAIRE_TUNNEL=inprocess), SSH over it, and a checked
// node version (§3).
type session struct {
	ctx *workspaces.Workspace
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

// openSession resolves the workspace, brings up the tunnel, connects SSH over
// it, runs the version handshake, and confirms a pending SSH lockdown the
// first time this workspace reaches the node (§8.5).
func openSession(cmd *cobra.Command, p *progress) (*session, error) {
	return openNodeSession(cmd, p, true)
}

// A node upgrade must reach the old binary before the normal version check.
func openNodeSession(cmd *cobra.Command, p *progress, checkVersion bool) (*session, error) {
	c, err := workspaces.Resolve(stringFlag(cmd, "workspace"))
	if err != nil {
		return nil, err
	}

	s := &session{ctx: c, warnings: workspaces.FixModes(c.Name)}

	nodeSpec, ok := c.Master()
	if !ok {
		return nil, output.NewError(output.CodeConfig, "workspace "+c.Name+" lists no node", output.ExitConfig, "Recreate it with vaire login.")
	}

	wgCfg, err := workspaceTunnelConfig(c, nodeSpec)
	if err != nil {
		return nil, err
	}

	sshKey, err := workspaces.ReadFile(c.Name, workspaces.SSHKeyFile)
	if err != nil {
		return nil, workspaceFileError(c.Name, workspaces.SSHKeyFile, err)
	}

	auth, err := transport.KeyAuth(sshKey)
	if err != nil {
		return nil, workspaceFileError(c.Name, workspaces.SSHKeyFile, err)
	}

	known, err := transport.LoadKnownHosts(workspaces.Path(c.Name, workspaces.KnownHostsFile))
	if err != nil {
		return nil, err
	}
	// The tunnel, SSH dial and version handshake are silent; the waiting
	// line covers them until the first RPC.
	defer p.wait("connecting to " + nodeLabel(c, nodeSpec))()

	ctx := commandContext(cmd)
	// Bound the initial connection, including shared-daemon attachment and
	// its WireGuard dial. The shared dialer's own budget can be longer than
	// the command's connection timeout.
	connectCtx, cancelConnect := context.WithTimeout(ctx, timeoutFlag(cmd))
	defer cancelConnect()

	if err := s.openDialer(connectCtx, cmd, wgCfg, p); err != nil {
		return nil, err
	}

	target := transport.SSHTarget{User: c.Operator.SSHUser, Addr: net.JoinHostPort(nodeSpec.WGAddress, "22")}
	sshOpts := transport.SSHOptions{Auth: []ssh.AuthMethod{auth}, HostKeys: known, Timeout: timeoutFlag(cmd)}
	s.dial = func(ctx context.Context) (*transport.SSHClient, error) {
		return transport.DialSSH(ctx, s.dialer, target, sshOpts)
	}

	client, err := s.dial(connectCtx)

	cancelConnect()

	if err != nil {
		s.Close()

		again, healed, retryErr := retryHealedSession(cmd, p, c, nodeSpec, err, checkVersion)
		if healed {
			return again, retryErr
		}

		return nil, err
	}

	s.node = &remote.Node{SSH: client, Operator: c.Operator.Name}
	if p.opts.Verbose {
		s.node.Stderr = os.Stderr
	}

	if !checkVersion {
		return s, nil
	}

	if err := s.handshake(ctx, cmd, c, nodeSpec, p); err != nil {
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

// retryHealedSession heals a workspace whose master moved to a new address and
// opens the session again, once. healed is false when no retry happened.
func retryHealedSession(cmd *cobra.Command, p *progress, c *workspaces.Workspace, nodeSpec workspaces.Node, dialErr error, checkVersion bool) (*session, bool, error) {
	// A rebuilt master on a new address (Phase 5): heal the workspace from
	// DNS once and try again.
	if endpointHealed(cmd) {
		return nil, false, nil
	}

	err := tunnelHint(dialErr, nodeSpec.PublicAddress, nodeSpec.ListenPort)

	healed, w := healEndpoint(commandContext(cmd), c, err)
	if !healed {
		return nil, false, nil
	}

	markEndpointHealed(cmd)

	again, err := openNodeSession(cmd, p, checkVersion)
	if err != nil {
		return nil, true, err
	}

	again.warnings = append(again.warnings, w)

	return again, true, nil
}

// handshake runs the node's version handshake, upgrading the node's binary
// when the command or the workspace asks for it.
func (s *session) handshake(ctx context.Context, cmd *cobra.Command, c *workspaces.Workspace, nodeSpec workspaces.Node, p *progress) error {
	auto := flagTrue(cmd, "auto-upgrade-nodes") || c.AutoUpgradeNodes
	upgrading := func(line string) {
		p.note(line)
		p.setPhase("upgrading vaire on " + nodeLabel(c, nodeSpec))
	}

	return s.node.Handshake(ctx, remote.HandshakeOptions{AutoUpgrade: auto, Progress: upgrading})
}

// nodeLabel names a node for the waiting line: its name, else its hostname,
// else the workspace's name.
func nodeLabel(c *workspaces.Workspace, n workspaces.Node) string {
	for _, name := range []string{n.Name, n.Hostname} {
		if name != "" {
			return name
		}
	}

	return c.Name
}

func workspaceFileError(name, file string, err error) error {
	return output.WrapError(output.CodeConfig, fmt.Errorf("read %s: %w", workspaces.Path(name, file), err), output.ExitConfig,
		"Enroll again with vaire login or recover owner access with vaire recover.")
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

	return workspaces.Save(s.ctx)
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
		Endpoint:      netip.AddrPortFrom(pub, uint16(port)),
		AllowedIPs:    []netip.Prefix{netip.PrefixFrom(wg, 24).Masked()},
	}, nil
}

// workspaceTunnelConfig is the tunnel to a workspace's node, with the operator
// key read from the workspace directory.
func workspaceTunnelConfig(c *workspaces.Workspace, n workspaces.Node) (transport.WireGuardConfig, error) {
	wgKey, err := workspaces.ReadFile(c.Name, workspaces.WGKeyFile)
	if err != nil {
		return transport.WireGuardConfig{}, workspaceFileError(c.Name, workspaces.WGKeyFile, err)
	}

	secrets.Add(strings.TrimSpace(string(wgKey)))

	return tunnelConfig(string(wgKey), c.Operator.WGAddress, n.WGPublicKey, n.PublicAddress, n.ListenPort, n.WGAddress)
}

// upTunnel starts an in-process userspace tunnel to a node, for vaire server
// init and VAIRE_TUNNEL=inprocess. It first takes the workspace's tunnel lock
// (see tunnellock.go), waiting while another vaire process or the workspace's
// tunnel daemon holds it, and holds it until the tunnel closes: two devices
// with one operator key would steal the node's peer from each other.
func upTunnel(ctx context.Context, workspaceName, privateKey, operatorAddr, nodePub, publicAddr string, port int, nodeWG string, p *progress) (*transport.Tunnel, error) {
	return upTunnelWithLock(ctx, workspaceName, workspaces.Path(workspaceName, TunnelLockFile), privateKey, operatorAddr, nodePub, publicAddr, port, nodeWG, p)
}

func upTunnelWithLock(ctx context.Context, workspaceName, lockPath, privateKey, operatorAddr, nodePub, publicAddr string, port int, nodeWG string, p *progress) (*transport.Tunnel, error) {
	cfg, err := tunnelConfig(privateKey, operatorAddr, nodePub, publicAddr, port, nodeWG)
	if err != nil {
		return nil, err
	}

	if p != nil && p.opts.Verbose {
		cfg.Logf = func(format string, args ...any) { p.note("wireguard: " + fmt.Sprintf(format, args...)) }
	}

	release, err := lockTunnelPath(ctx, workspaceName, lockPath, p)
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

// tunnelHint replaces a bare dial failure inside the tunnel with what to
// check, matching the cause: the daemon not running, the daemon not
// answering, or a dial that timed out after a real WireGuard handshake
// window. A transient timeout is not the place for break-glass.
func tunnelHint(err error, publicAddr string, port int) error {
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodeRemoteUnreachable {
		return err
	}

	cliErr.Code = output.CodeTransportFailed
	target := net.JoinHostPort(publicAddr, strconv.Itoa(port))

	var dialErr *transport.TunnelDialError
	switch {
	case transport.IsNoDaemon(err):
		cliErr.Hint = "The tunnel daemon isn't running. It starts with the next command; retry, or run vaire tunnel status."
	case errors.As(err, &dialErr):
		cliErr.Hint = "WireGuard to " + target + " did not answer within the tunnel's dial timeout. Check that UDP " +
			strconv.Itoa(port) + " reaches the server and that wg0 runs there, then retry. If it keeps failing, use break-glass from the provider console: vaire node unlock-ssh."
	case errors.Is(err, transport.ErrDaemonNoAnswer):
		cliErr.Hint = "The tunnel daemon did not reply. Run vaire tunnel status, and see tunnel.log in the workspace directory; " +
			"vaire tunnel stop --force restarts it. A WireGuard re-handshake after idling can take a few seconds, so retry once first."
	default:
		cliErr.Hint = "WireGuard to " + target + " did not answer. Check that UDP " +
			strconv.Itoa(port) + " leaves this network and reaches the server, then retry."
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
	stepIDs, err := applyStepIDs(cmd)
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

	secretValues, creds, generated, err := serviceSecrets(s.ctx.Name)
	if err != nil {
		return err
	}

	if err := applyRecoveryKit(cmd, s, p, secretValues, creds); err != nil {
		return err
	}

	params := applyParams(cmd, stepIDs)
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

	var data applyReplyData
	if err := reply.Decode(&data); err != nil {
		return err
	}

	warnings = append(warnings, syncWorkspaceFromNode(commandContext(cmd), s, p)...)

	message := fmt.Sprintf("applied %d, skipped %d of %d steps", data.Applied, data.Skipped, data.Total)
	if data.Restarted() {
		message += "; restarted " + restartSummary(data.Restarts)
	}

	if len(data.RestartsSettled) > 0 {
		message += "; no restart needed for " + strings.Join(data.RestartsSettled, ", ") + " (already running the new settings)"
	}

	if data.Doctor != nil {
		message += "\n" + healthLine(data.Doctor.Summary)
	}

	var next []string

	if len(data.RestartPending) > 0 {
		message += "; restart pending for " + strings.Join(data.RestartPending, ", ")
		next = []string{"vaire apply " + confirmFlags(data.ConfirmTokens)}

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

// applyStepIDs are the --step values, which config set/edit --apply doesn't
// define.
func applyStepIDs(cmd *cobra.Command) ([]string, error) {
	if cmd.Flags().Lookup("step") == nil {
		return nil, nil
	}

	stepIDs, err := cmd.Flags().GetStringSlice("step")
	if err != nil {
		return nil, err
	}

	return stepIDs, nil
}

// applyRecoveryKit adds this machine's recovery kit to the secrets the node
// receives, and refuses when backups are on without one. The node writes the
// kit only when its copy is missing (D7); vaire never makes a new cipher for
// an existing repository. The age recipient rides along even without a
// bucket, so master-backup-v1 can repair a node that lost its copy.
func applyRecoveryKit(cmd *cobra.Command, s *session, p *progress, secretValues map[string]string, creds workspaces.Credentials) error {
	if creds.Backup != nil && creds.Backup.HasBucket() {
		addRecoveryKit(secretValues, creds.Backup)
		return nil
	}

	cfg, err := remoteConfig(cmd, s, p)
	if err != nil {
		// The apply itself reports an unreachable node; still carry what the
		// kit has.
		addRecoveryKit(secretValues, creds.Backup)
		return nil
	}

	return addRecoveryKitUnlessIncomplete(secretValues, creds.Backup, cfg.BackupConfigured())
}

// addRecoveryKitUnlessIncomplete adds the kit to a request's secrets and
// refuses when the node has backups configured but this machine's kit has no
// bucket to restore its keys and cipher. An identity-only kit still sends its
// age recipient when backups are off.
func addRecoveryKitUnlessIncomplete(secretValues map[string]string, kit *workspaces.BackupCreds, backupConfigured bool) error {
	if backupConfigured && (kit == nil || !kit.HasBucket()) {
		return output.NewError(output.CodeInputRequired, "backups are on, but this machine's workspace has no recovery kit", output.ExitInput,
			"Use an owner workspace that holds the backup credentials, then retry.").
			WithDetails(map[string]any{"reason": "recovery_kit_missing"})
	}

	addRecoveryKit(secretValues, kit)

	return nil
}

// applyParams are the RPC parameters of vaire apply.
func applyParams(cmd *cobra.Command, stepIDs []string) map[string]any {
	params := map[string]any{}
	if len(stepIDs) > 0 {
		params["steps"] = stepIDs
	}

	if db := stringFlag(cmd, "db"); db != "" {
		params["db"] = db
	}

	if flagTrue(cmd, "skip-doctor") {
		params["skip_doctor"] = true
	}

	return params
}

// planMessage renders a plan for humans: one line per step, the file diffs of
// the steps that would apply, and any pending restart.
func planMessage(header string, data engine.PlanData) string {
	lines := []string{header}
	for _, step := range data.Steps {
		lines = append(lines, fmt.Sprintf("  %-5s %s (%s)", step.Action, step.ID, step.Reason))
		if step.Detail != "" {
			lines = append(lines, "        "+step.Detail)
		}
	}

	if len(data.AccessConfirmTokens) > 0 {
		lines = append(lines, "", "access expansion requires: "+confirmFlags(data.AccessConfirmTokens))
	}

	for _, step := range data.Steps {
		for _, d := range step.Diffs {
			lines = append(lines, "", strings.TrimRight(d.Diff, "\n"))
		}
	}

	if len(data.RestartPending) > 0 {
		lines = append(lines, "", "restart pending for: "+strings.Join(data.RestartPending, ", "),
			"run vaire apply "+confirmFlags(data.ConfirmTokens)+" to restart those clusters")
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
