package steps

import (
	"os"
	"os/exec"
	"path/filepath"
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
ufw allow in on wg0 to any port 6418 proto tcp
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
	cfg.Network.ClientAllowlist = []string{"203.0.113.7/32", "203.0.113.7/32", "198.51.100.9/24", "2001:db8::1/128"}
	want := configuredRules(cfg)

	have := parseShowAdded(showAdded)
	if len(have) != 13 {
		t.Fatalf("parsed %d rules: %v", len(have), have)
	}

	if d := firewallDrift(want, have, publicSSHAbsent); d != "" {
		t.Errorf("drift on a converged firewall: %s\nwant %v\nhave %v", d, want, have)
	}

	if n := countOf(want, "allow from 203.0.113.7 to any port 6432 proto tcp"); n != 1 {
		t.Errorf("203.0.113.7 rendered %d times", n)
	}
}

func TestDefaultRoutesPreserveAddressFamily(t *testing.T) {
	for _, tc := range []struct {
		cidr   string
		halves []string
	}{
		{"0.0.0.0/0", []string{"0.0.0.0/1", "128.0.0.0/1"}},
		{"::/0", []string{"::/1", "8000::/1"}},
	} {
		t.Run(tc.cidr, func(t *testing.T) {
			cfg := config.DefaultCluster()
			cfg.Transport.Subnet = ""
			cfg.Network.ClientAllowlist = []string{tc.cidr, tc.cidr}
			rules := configuredRules(cfg)

			for _, half := range tc.halves {
				rule := "allow from " + half + " to any port 6432 proto tcp"
				if countOf(rules, rule) != 1 {
					t.Errorf("expected one %q in %v", rule, rules)
				}
			}

			if slices.Contains(rules, "allow 6432/tcp") || len(rules) != 11 {
				t.Errorf("unexpected rules: %v", rules)
			}

			var show strings.Builder
			for _, rule := range rules {
				show.WriteString("ufw " + rule + "\n")
			}

			if drift := firewallDrift(rules, parseShowAdded(show.String()), publicSSHAbsent); drift != "" {
				t.Errorf("drift: %s", drift)
			}
		})
	}
}

func TestPublicPoolerRulesExcludeWireGuardSubnet(t *testing.T) {
	cfg := config.DefaultCluster()
	cfg.Network.RoleAllowlist = &config.RoleAllowlist{App: []string{"10.42.0.0/15"}, Migrate: []string{"192.0.2.7/32"}}
	rules := configuredRules(cfg)

	for _, rule := range []string{
		"allow from 10.43.0.0/16 to any port 6432 proto tcp",
		"allow from 10.43.0.0/16 to any port 5432 proto tcp",
		"allow from 192.0.2.7 to any port 5432 proto tcp",
	} {
		if !slices.Contains(rules, rule) {
			t.Errorf("missing %q in %v", rule, rules)
		}
	}

	if slices.Contains(rules, "allow from 192.0.2.7 to any port 6432 proto tcp") ||
		slices.Contains(rules, "allow from 10.42.0.0/15 to any port 5432 proto tcp") {
		t.Fatalf("overbroad pooler rules: %v", rules)
	}
}

func TestFirewallScriptStaleRuleRemoval(t *testing.T) { //nolint:gocognit // Each case checks the script result and the recorded UFW calls.
	for _, tc := range []struct {
		name       string
		have       string
		failDelete bool
		failShow   bool
		wantError  bool
	}{
		{name: "already converged", have: "allow 80/tcp"},
		{name: "remove stale", have: "allow 9999/tcp"},
		{name: "failed removal", have: "allow 9999/tcp", failDelete: true, wantError: true},
		{name: "failed read", failShow: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "calls")

			stub := `#!/bin/bash
printf '%s\n' "$*" >> "$CALLS"
if [ "$*" = "show added" ]; then
  [ "$FAIL_SHOW" != "true" ] || exit 1
  printf 'ufw %s\n' "$HAVE"
elif [ "$1" = "delete" ] && [ "$FAIL_DELETE" = "true" ]; then
  exit 1
fi
`
			if err := os.WriteFile(filepath.Join(dir, "ufw"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}

			cmd := exec.Command("/bin/bash", "-s")
			cmd.Stdin = strings.NewReader(Script("firewall"))

			cmd.Env = []string{
				"PATH=" + dir + ":/usr/bin:/bin", "UFW_RULES=allow 80/tcp", "CALLS=" + log,
				"HAVE=" + tc.have,
			}
			if tc.failDelete {
				cmd.Env = append(cmd.Env, "FAIL_DELETE=true")
			}

			if tc.failShow {
				cmd.Env = append(cmd.Env, "FAIL_SHOW=true")
			}

			out, err := cmd.CombinedOutput()
			if (err != nil) != tc.wantError {
				t.Fatalf("error %v, output %s", err, out)
			}

			calls, err := os.ReadFile(log)
			if err != nil {
				t.Fatal(err)
			}

			if tc.have == "allow 80/tcp" && strings.Contains(string(calls), "delete ") {
				t.Errorf("deleted a wanted rule: %s", calls)
			}

			if tc.have == "allow 9999/tcp" && !strings.Contains(string(calls), "delete allow 9999/tcp\n") {
				t.Errorf("did not delete stale rule: %s", calls)
			}

			if tc.wantError && strings.Contains(string(calls), "--force enable") {
				t.Errorf("enabled firewall after failure: %s", calls)
			}
		})
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
