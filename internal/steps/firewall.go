package steps

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

const firewallID = "firewall-v1"

// ufw rule specs in "ufw show added" form.
const (
	RulePublicSSH = "allow 22/tcp"
	RuleWGSSH     = "allow in on wg0 to any port 22 proto tcp"
)

// SSHUnlockUnit is the break-glass unlock's timer and service (vaire node
// unlock-ssh, §8.4): while its timer is active, public 22/tcp is open on
// purpose.
const SSHUnlockUnit = "vaire-ssh-unlock"

// FirewallRules is the rule set step 5 converges to. Public 22/tcp stays until
// the commit-confirm lockdown is confirmed (§8.3), and while a break-glass
// unlock is open.
func FirewallRules(cfg config.Cluster, st engine.State, unlockOpen bool) []string {
	rules := configuredRules(cfg)
	if st.PublicSSHOpen() || unlockOpen {
		rules = appendRule(rules, RulePublicSSH)
	}
	return rules
}

// configuredRules is the rule set the cluster config alone decides: every
// rule except public 22/tcp, which follows the lockdown flags. Each rule is
// in the form "ufw show added" prints it, so the check compares like with
// like and firewall.sh never deletes a rule it would add again.
func configuredRules(cfg config.Cluster) []string {
	rules := []string{
		fmt.Sprintf("allow %d/udp", cfg.Transport.ListenPort),
		"allow 80/tcp",
		"allow 443/tcp",
		RuleWGSSH,
	}
	// WireGuard peers always reach both poolers; the expose flags gate only
	// the public allowlist (§11 step 5). 5432 is the session pooler, and no
	// cluster has a TCP port to open: they listen on sockets only (RFC 8.2).
	dbPort := func(port int, exposed bool) {
		rules = appendRule(rules, fmt.Sprintf("allow in on wg0 to any port %d proto tcp", port))
		if !exposed {
			return
		}
		for _, cidr := range cfg.Network.ClientAllowlist {
			if p, err := netip.ParsePrefix(cidr); err == nil {
				rules = appendRule(rules, allowFromRule(p, port))
			}
		}
	}
	dbPort(PgbouncerPort, cfg.Network.ExposePgbouncer)
	dbPort(SessionPoolerPort, cfg.Network.ExposePostgres)
	return rules
}

// allowFromRule is the rule that opens port to the clients in p, as ufw
// prints it: ufw keeps a single address without its /32 or /128, and a rule
// from any address (/0) in its short form.
func allowFromRule(p netip.Prefix, port int) string {
	p = p.Masked()
	switch {
	case p.Bits() == 0:
		return fmt.Sprintf("allow %d/tcp", port)
	case p.IsSingleIP():
		return fmt.Sprintf("allow from %s to any port %d proto tcp", p.Addr().Unmap(), port)
	}
	return fmt.Sprintf("allow from %s to any port %d proto tcp", p, port)
}

// appendRule appends rule unless rules has it: ufw keeps one copy of a rule.
func appendRule(rules []string, rule string) []string {
	for _, r := range rules {
		if r == rule {
			return rules
		}
	}
	return append(rules, rule)
}

// UFWRules returns the configured ufw rules in "ufw show added" form.
func UFWRules(ctx context.Context) ([]string, error) {
	out, err := Probe(ctx, "ufw", "show", "added")
	if err != nil {
		return nil, err
	}
	return parseShowAdded(out), nil
}

// parseShowAdded reads the rules of "ufw show added", one "ufw <rule>" line
// each after its header.
func parseShowAdded(out string) []string {
	var rules []string
	for _, line := range strings.Split(out, "\n") {
		if rule, ok := strings.CutPrefix(strings.TrimSpace(line), "ufw "); ok {
			rules = append(rules, strings.Join(strings.Fields(rule), " "))
		}
	}
	return rules
}

type firewall struct{}

func (firewall) ID() string { return firewallID }

func (firewall) Describe(config.Cluster) string {
	return "converge ufw: deny incoming, allow WireGuard, 80, 443 and SSH on wg0"
}

func firewallInputs(ctx context.Context, cfg config.Cluster) ([]string, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return nil, err
	}
	return FirewallRules(cfg, st, sshUnlockOpen(ctx)), nil
}

// sshUnlockOpen reports whether a break-glass unlock holds public 22 open.
func sshUnlockOpen(ctx context.Context) bool {
	out, _ := Probe(ctx, "systemctl", "is-active", SSHUnlockUnit+".timer")
	return out == "active"
}

// Inputs hashes the rule set without public 22/tcp. That rule follows the
// lockdown flags in state.json, and the ssh.lockdown, ssh.confirm, ssh-revert
// and unlock-ssh handlers add or delete it in ufw themselves when they change
// those flags. Hashing it would make the journal entry init records before
// lockdown stale the moment lockdown is confirmed, so the first apply after
// init would re-run this step for a firewall that is already converged.
func (firewall) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	return canonical(configuredRules(cfg))
}

// Recheck makes the runner check ufw even when the journal says the step is
// current: a rule added or deleted by hand drifts without any input
// changing, and doctor's fw.rules fix is vaire apply.
func (firewall) Recheck() bool { return true }

func (firewall) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return engine.CheckResult{}, err
	}
	status, _ := Probe(ctx, "ufw", "status")
	if !strings.HasPrefix(status, "Status: active") {
		return incomplete("ufw is inactive"), nil
	}
	have, err := UFWRules(ctx)
	if err != nil {
		return incomplete("cannot read ufw rules"), nil
	}
	if detail := firewallDrift(configuredRules(cfg), have, publicSSHExpectation(st, sshUnlockOpen(ctx))); detail != "" {
		return incomplete(detail), nil
	}
	return complete(), nil
}

// What the check expects of public 22/tcp.
const (
	publicSSHAbsent = iota
	publicSSHPresent
	// publicSSHEither: a lockdown waits for its confirmation (it closes 22
	// itself, or keeps it until ssh.confirm with --local), or a break-glass
	// unlock has it open. The lockdown and unlock handlers own the rule
	// then, and doctor's ssh.public_closed reports it.
	publicSSHEither
)

func publicSSHExpectation(st engine.State, unlockOpen bool) int {
	switch {
	case st.LockdownPending || unlockOpen:
		return publicSSHEither
	case st.PublicSSHOpen():
		return publicSSHPresent
	}
	return publicSSHAbsent
}

// firewallDrift names the rules missing from ufw and the ones ufw has that
// vaire doesn't manage, or "" when they match.
func firewallDrift(want, have []string, publicSSH int) string {
	switch publicSSH {
	case publicSSHPresent:
		want = appendRule(want, RulePublicSSH)
	case publicSSHEither:
		want = without(want, RulePublicSSH)
		have = without(have, RulePublicSSH)
	}
	missing, extra := setDiff(want, have), setDiff(have, want)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, "ufw is missing "+quoteRules(missing))
	}
	if len(extra) > 0 {
		parts = append(parts, "ufw has "+quoteRules(extra)+", which vaire doesn't manage")
	}
	return strings.Join(parts, "; ")
}

func without(rules []string, rule string) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if r != rule {
			out = append(out, r)
		}
	}
	return out
}

// setDiff is the rules of a that b doesn't have, sorted.
func setDiff(a, b []string) []string {
	in := make(map[string]bool, len(b))
	for _, r := range b {
		in[r] = true
	}
	var out []string
	for _, r := range a {
		if !in[r] {
			out = append(out, r)
			in[r] = true
		}
	}
	sort.Strings(out)
	return out
}

func quoteRules(rules []string) string {
	quoted := make([]string, len(rules))
	for i, r := range rules {
		quoted[i] = "\"" + r + "\""
	}
	return strings.Join(quoted, ", ")
}

func (s firewall) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	rules, err := firewallInputs(ctx, cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("firewall"), map[string]string{"UFW_RULES": strings.Join(rules, "\n")}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}
