package steps

import (
	"context"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

const sshdHardeningID = "sshd-hardening-v1"

// SSHDropInPath is vaire's sshd drop-in. sshd keeps the first value it reads for
// most keywords and reads sshd_config.d in lexical order, so the drop-in sorts
// first to win over provider drop-ins such as 50-cloud-init.conf.
const SSHDropInPath = "/etc/ssh/sshd_config.d/00-vaire.conf"

// BreakGlassPath is the temporary drop-in vaire node unlock-ssh writes.
const BreakGlassPath = "/etc/ssh/sshd_config.d/01-vaire-break-glass.conf"

// sshdExpect are effective sshd -T lines the hardening must produce.
const sshdExpect = "permitrootlogin no\npasswordauthentication no\n"

// RenderSSHDropIn returns the fixed drop-in content (§11.1).
func RenderSSHDropIn(cfg config.Cluster) string {
	return strings.Join([]string{
		"# managed by vaire: SSH is reachable only over WireGuard once locked down (ufw)",
		"PermitRootLogin no",
		"PubkeyAuthentication yes",
		"PasswordAuthentication no",
		"KbdInteractiveAuthentication no",
		"PermitEmptyPasswords no",
		"X11Forwarding no",
		"AllowTcpForwarding no",
		"AllowAgentForwarding no",
		"AllowUsers " + cfg.Server.AdminUser,
		"MaxAuthTries 3",
		"MaxSessions 10",
		"LoginGraceTime 30",
		"ClientAliveInterval 300",
		"ClientAliveCountMax 2",
		"",
	}, "\n")
}

// RenderBreakGlass returns the drop-in that allows password login for the
// admin user only, for vaire node unlock-ssh (§8.4).
func RenderBreakGlass(adminUser string) string {
	return strings.Join([]string{
		"# vaire break-glass: removed by vaire node ssh-revert --lockdown-only",
		"Match User " + adminUser,
		"    PasswordAuthentication yes",
		"Match all",
		"",
	}, "\n")
}

// SSHDropInEnv is the sshd-dropin script environment that writes content to
// path, or removes path when content is empty.
func SSHDropInEnv(path, content string, hardening bool) map[string]string {
	env := map[string]string{"SSHD_DROPIN_PATH": path, "SSHD_DROPIN_ACTION": "remove"}
	if content != "" {
		env["SSHD_DROPIN_ACTION"] = "write"
		env["SSHD_DROPIN"] = content
	}
	if hardening {
		env["SSHD_EXPECT"] = sshdExpect
	}
	return env
}

type sshdHardening struct{}

func (sshdHardening) ID() string { return sshdHardeningID }

func (sshdHardening) Describe(config.Cluster) string {
	return "harden sshd (no root, no passwords, AllowUsers); public 22 stays open until lockdown"
}

func (sshdHardening) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return canonical(map[string]string{"path": SSHDropInPath, "dropin": RenderSSHDropIn(cfg)})
}

func (sshdHardening) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	if !fileIs(SSHDropInPath, RenderSSHDropIn(cfg), 0o644) {
		return incomplete(SSHDropInPath + " differs"), nil
	}
	if out, _ := Probe(ctx, "systemctl", "is-enabled", "ssh.socket"); out == "enabled" {
		return incomplete("ssh.socket activation is still enabled"), nil
	}
	return complete(), nil
}

func (s sshdHardening) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	err := rt.Shell.Run(ctx, s.ID(), Script("sshd-dropin"), SSHDropInEnv(SSHDropInPath, RenderSSHDropIn(cfg), true), report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
