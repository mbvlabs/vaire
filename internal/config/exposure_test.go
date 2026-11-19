package config

import (
	"slices"
	"strings"
	"testing"
)

func TestClientAllowlistConfig(t *testing.T) {
	cfg := DefaultCluster()
	if len(cfg.Network.ClientAllowlist) != 0 {
		t.Fatal("both poolers must be private by default")
	}

	if err := cfg.Set("network.client_allowlist", `["203.0.113.7/32"]`); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(cfg.Network.ClientAllowlist, []string{"203.0.113.7/32"}) {
		t.Fatalf("config set produced %v", cfg.Network.ClientAllowlist)
	}

	for key := range strings.FieldsSeq("expose_postgres expose_session_pooler expose_pgbouncer") {
		if _, err := ParseCluster([]byte("[network]\n" + key + " = true\n")); err == nil {
			t.Fatalf("retired key %s was silently accepted", key)
		}
	}
}

func TestRoleAllowlistModes(t *testing.T) {
	cfg, err := ParseCluster([]byte("[network.role_allowlist]\napp = [\"192.0.2.7/32\"]\n"))
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(cfg.Network.ClientsForRole(AllowlistApp), []string{"192.0.2.7/32"}) ||
		len(cfg.Network.ClientsForRole(AllowlistMigrate)) != 0 {
		t.Fatal("app rule leaked to another role")
	}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	roundTrip, err := ParseCluster(raw)
	if err != nil || roundTrip.Network.RoleAllowlist == nil {
		t.Fatalf("round trip lost per-role mode: %v", err)
	}

	cfg.Network.ClientAllowlist = []string{"192.0.2.7/32"}
	if err := cfg.ValidateFor(ScopeHost); err == nil || !strings.Contains(err.Error(), "client_allowlist must be empty") {
		t.Fatalf("mixed mode was not rejected: %v", err)
	}
}

func TestRoleAllowlistWideningWithUnchangedCIDRUnion(t *testing.T) {
	old := DefaultCluster()
	old.Network.RoleAllowlist = &RoleAllowlist{App: []string{"192.0.2.7/32"}}
	next := old
	next.Network.RoleAllowlist = &RoleAllowlist{Migrate: []string{"192.0.2.7/32"}}

	if got := ExposureIncreases(old, next); !slices.Equal(got, []string{"database_role_migrate"}) {
		t.Fatalf("role widening = %v", got)
	}

	shared := old

	shared.Network = Network{ClientAllowlist: []string{"192.0.2.7/32"}}
	if got := ExposureIncreases(shared, old); len(got) != 0 {
		t.Fatalf("switch to narrower role policy widens %v", got)
	}

	if got := ExposureIncreases(old, shared); !slices.Contains(got, "database_role_migrate") {
		t.Fatalf("switch to shared policy did not flag migration access: %v", got)
	}
}

func TestExposureIncreases(t *testing.T) {
	for _, tc := range []struct {
		name string
		old  Cluster
		next Cluster
		want []string
	}{
		{
			name: "one client widens both poolers",
			next: Cluster{Network: Network{ClientAllowlist: []string{"192.0.2.1/32"}}},
			want: []string{"session_pooler", "transaction_pooler"},
		},
		{
			name: "closing database ports and restricting metrics",
			old: Cluster{
				Network: Network{ClientAllowlist: []string{"192.0.2.1/32"}},
				Metrics: Metrics{Enabled: true},
			},
			next: Cluster{Metrics: Metrics{Enabled: true, Allowlist: []string{"192.0.2.1/32"}}},
		},
		{
			name: "clearing metrics allowlist opens both address families",
			old:  Cluster{Metrics: Metrics{Enabled: true, Allowlist: []string{"0.0.0.0/0"}}},
			next: Cluster{Metrics: Metrics{Enabled: true}},
			want: []string{"metrics"},
		},
		{
			name: "enabling metrics with a fixed client",
			next: Cluster{Metrics: Metrics{Enabled: true, Allowlist: []string{"2001:db8::1/128"}}},
			want: []string{"metrics"},
		},
		{
			name: "metrics already admit loopback",
			old:  Cluster{Metrics: Metrics{Enabled: true, Allowlist: []string{"192.0.2.1/32"}}},
			next: Cluster{Metrics: Metrics{Enabled: true, Allowlist: []string{"127.0.0.1/32", "::1/128"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExposureIncreases(tc.old, tc.next); !slices.Equal(got, tc.want) {
				t.Fatalf("ExposureIncreases = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPrefixesWiden(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old, next []string
		want      bool
	}{
		{"merged v4 union", []string{"192.0.2.5/25", "192.0.2.128/25"}, []string{"192.0.2.0/24"}, false},
		{"one uncovered address", []string{"192.0.2.0/31", "192.0.2.2/32"}, []string{"192.0.2.0/30"}, true},
		{"merged v6 union", []string{"2001:db8::/65", "2001:db8:0:0:8000::/65"}, []string{"2001:db8::/64"}, false},
		{"family change", []string{"0.0.0.0/0"}, []string{"::/0"}, true},
		{"empty list closes access", []string{"0.0.0.0/0"}, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := prefixesWiden(tc.old, tc.next); got != tc.want {
				t.Fatalf("prefixesWiden = %v, want %v", got, tc.want)
			}
		})
	}
}
