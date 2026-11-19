package config

import (
	"slices"
	"strings"
	"testing"
)

func TestSessionPoolerExposureConfig(t *testing.T) {
	if DefaultCluster().Network.ExposeSessionPooler {
		t.Fatal("the session pooler must be private by default")
	}

	cfg, err := ParseCluster([]byte("[network]\nexpose_session_pooler = true\n"))
	if err != nil || !cfg.Network.ExposeSessionPooler {
		t.Fatalf("new boolean did not decode: %v", err)
	}

	raw, err := cfg.Encode()
	if err != nil || !strings.Contains(string(raw), "expose_session_pooler = true") {
		t.Fatalf("new boolean did not encode: %v", err)
	}

	if _, err := ParseCluster([]byte("[network]\nexpose_postgres = true\n")); err == nil {
		t.Fatal("the retired beta.1 key must not be silently accepted")
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
			name: "session activation requires approval even without clients",
			next: Cluster{Network: Network{ExposeSessionPooler: true}},
			want: []string{"session_pooler"},
		},
		{
			name: "shared list can widen both poolers",
			old:  Cluster{Network: Network{ExposeSessionPooler: true, ExposePgbouncer: true}},
			next: Cluster{Network: Network{ExposeSessionPooler: true, ExposePgbouncer: true, ClientAllowlist: []string{"192.0.2.1/32"}}},
			want: []string{"session_pooler", "transaction_pooler"},
		},
		{
			name: "closing database ports and restricting metrics",
			old: Cluster{
				Network: Network{ExposeSessionPooler: true, ExposePgbouncer: true, ClientAllowlist: []string{"192.0.2.1/32"}},
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
