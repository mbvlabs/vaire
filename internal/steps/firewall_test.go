package steps

import (
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

// showAdded is "ufw show added" on a node with public 22 closed and an
// allowlist, as ufw prints it: single addresses without /32 or /128, each
// rule once for IPv4 and IPv6.
const showAdded = `Added user rules (see 'ufw status' for running firewall):
ufw allow 51820/udp
ufw allow 80/tcp
ufw allow 443/tcp
ufw allow in on wg0 to any port 22 proto tcp
ufw allow in on wg0 to any port 6432 proto tcp
ufw allow from 203.0.113.7 to any port 6432 proto tcp
ufw allow from 198.51.100.0/24 to any port 6432 proto tcp
ufw allow from 2001:db8::1 to any port 6432 proto tcp
ufw allow in on wg0 to any port 5432 proto tcp
ufw allow from 203.0.113.7 to any port 5432 proto tcp
ufw allow from 198.51.100.0/24 to any port 5432 proto tcp
ufw allow from 2001:db8::1 to any port 5432 proto tcp`

// The rules vaire renders are the ones ufw prints back, so a converged
// firewall shows no drift. Before, the /32 allowlist entries never matched
// and doctor's fw.rules failed on a converged firewall.
func TestConfiguredRulesMatchUFWShowAdded(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Network.ExposePgbouncer = true
	cfg.Network.ExposePostgres = true
	cfg.Network.ClientAllowlist = []string{"203.0.113.7/32", "203.0.113.7/32", "198.51.100.9/24", "2001:db8::1/128"}
	want := configuredRules(cfg)
	have := parseShowAdded(showAdded)
	if len(have) != 12 {
		t.Fatalf("parsed %d rules: %v", len(have), have)
	}
	if d := firewallDrift(want, have, publicSSHAbsent); d != "" {
		t.Errorf("drift on a converged firewall: %s\nwant %v\nhave %v", d, want, have)
	}
	if n := countOf(want, "allow from 203.0.113.7 to any port 6432 proto tcp"); n != 1 {
		t.Errorf("203.0.113.7 rendered %d times", n)
	}
	// A source of any address is ufw's short form.
	cfg.Network.ClientAllowlist = []string{"0.0.0.0/0", "::/0"}
	if open := configuredRules(cfg); countOf(open, "allow 6432/tcp") != 1 || countOf(open, "allow 5432/tcp") != 1 {
		t.Errorf("a /0 allowlist renders %v", open)
	}
}

func countOf(list []string, s string) int {
	n := 0
	for _, x := range list {
		if x == s {
			n++
		}
	}
	return n
}

func TestFirewallDriftNamesTheRules(t *testing.T) {
	want := []string{"allow 80/tcp", RuleWGSSH}
	if d := firewallDrift(want, []string{RuleWGSSH, "allow 80/tcp"}, publicSSHAbsent); d != "" {
		t.Errorf("order counts: %s", d)
	}
	d := firewallDrift(want, []string{RuleWGSSH, "allow 8080/tcp"}, publicSSHAbsent)
	if !strings.Contains(d, `ufw is missing "allow 80/tcp"`) || !strings.Contains(d, `ufw has "allow 8080/tcp", which vaire doesn't manage`) {
		t.Errorf("detail %q", d)
	}
	// Public 22: required before lockdown, refused after it, either way while
	// a lockdown waits for its confirmation or a break-glass unlock is open.
	if d := firewallDrift(want, want, publicSSHPresent); !strings.Contains(d, `ufw is missing "allow 22/tcp"`) {
		t.Errorf("before lockdown: %q", d)
	}
	withSSH := append(slices.Clone(want), RulePublicSSH)
	if d := firewallDrift(want, withSSH, publicSSHAbsent); !strings.Contains(d, `ufw has "allow 22/tcp"`) {
		t.Errorf("after lockdown: %q", d)
	}
	for _, have := range [][]string{want, withSSH} {
		if d := firewallDrift(want, have, publicSSHEither); d != "" {
			t.Errorf("unlock open: %q", d)
		}
	}
}

func TestPublicSSHExpectation(t *testing.T) {
	cases := []struct {
		st     engine.State
		unlock bool
		want   int
	}{
		{engine.State{}, false, publicSSHPresent},
		{engine.State{LockdownPending: true}, false, publicSSHEither},
		{engine.State{LockedDownAt: "2026-10-12T10:00:00Z"}, false, publicSSHAbsent},
		{engine.State{LockedDownAt: "2026-10-12T10:00:00Z"}, true, publicSSHEither},
	}
	for _, c := range cases {
		if got := publicSSHExpectation(c.st, c.unlock); got != c.want {
			t.Errorf("%+v unlock %v: %d, want %d", c.st, c.unlock, got, c.want)
		}
	}
	// Apply keeps public 22 while an unlock holds it open.
	locked := engine.State{LockedDownAt: "2026-10-12T10:00:00Z"}
	if slices.Contains(FirewallRules(config.DefaultCluster(), locked, false), RulePublicSSH) {
		t.Error("locked down, no unlock: 22 rendered")
	}
	if !slices.Contains(FirewallRules(config.DefaultCluster(), locked, true), RulePublicSSH) {
		t.Error("unlock open: 22 not rendered, apply would close the break-glass")
	}
}
