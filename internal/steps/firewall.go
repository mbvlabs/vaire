package steps

import (
	"context"
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/mbvlabs/eru/internal/config"
	"github.com/mbvlabs/eru/internal/engine"
)

const firewallID = "firewall-v1"

// ufw rule specs in "ufw show added" form.
const (
	RulePublicSSH = "allow 22/tcp"
	RuleWGSSH     = "allow in on wg0 to any port 22 proto tcp"
)

// FirewallRules is the rule set step 5 converges to. Public 22/tcp stays until
// the commit-confirm lockdown is confirmed (§8.3).
func FirewallRules(cfg config.Cluster, st engine.State) []string {
	rules := []string{
		fmt.Sprintf("allow %d/udp", cfg.Transport.ListenPort),
		"allow 80/tcp",
		"allow 443/tcp",
		RuleWGSSH,
	}
	// WireGuard peers always reach both database ports; the expose flags gate
	// only the public allowlist (§11 step 5).
	dbPort := func(port int, exposed bool) {
		rules = append(rules, fmt.Sprintf("allow in on wg0 to any port %d proto tcp", port))
		if !exposed {
			return
		}
		for _, cidr := range cfg.Network.ClientAllowlist {
			if p, err := netip.ParsePrefix(cidr); err == nil {
				rules = append(rules, fmt.Sprintf("allow from %s to any port %d proto tcp", p.Masked(), port))
			}
		}
	}
	dbPort(6432, cfg.Network.ExposePgbouncer)
	dbPort(5432, cfg.Network.ExposePostgres)
	if st.PublicSSHOpen() {
		rules = append(rules, RulePublicSSH)
	}
	return rules
}

// UFWRules returns the configured ufw rules in "ufw show added" form.
func UFWRules(ctx context.Context) ([]string, error) {
	out, err := Probe(ctx, "ufw", "show", "added")
	if err != nil {
		return nil, err
	}
	var rules []string
	for _, line := range strings.Split(out, "\n") {
		if rule, ok := strings.CutPrefix(strings.TrimSpace(line), "ufw "); ok {
			rules = append(rules, strings.Join(strings.Fields(rule), " "))
		}
	}
	return rules, nil
}

type firewall struct{}

func (firewall) ID() string { return firewallID }

func (firewall) Describe(config.Cluster) string {
	return "converge ufw: deny incoming, allow WireGuard, 80, 443 and SSH on wg0"
}

func firewallInputs(cfg config.Cluster) ([]string, error) {
	st, err := engine.LoadState(engine.StatePath)
	if err != nil {
		return nil, err
	}
	return FirewallRules(cfg, st), nil
}

func (firewall) Inputs(cfg config.Cluster, _ engine.Runtime) ([]byte, error) {
	rules, err := firewallInputs(cfg)
	if err != nil {
		return nil, err
	}
	return canonical(rules)
}

func (firewall) Check(ctx context.Context, cfg config.Cluster, _ engine.Runtime) (engine.CheckResult, error) {
	want, err := firewallInputs(cfg)
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
	if !sameSet(want, have) {
		return incomplete("ufw rules differ"), nil
	}
	return complete(), nil
}

func (s firewall) Apply(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	rules, err := firewallInputs(cfg)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	err = rt.Shell.Run(ctx, s.ID(), Script("firewall"), map[string]string{"UFW_RULES": strings.Join(rules, "\n")}, report)
	return engine.ApplyResult{Restart: engine.RestartNone}, err
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x := append([]string(nil), a...)
	y := append([]string(nil), b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}
