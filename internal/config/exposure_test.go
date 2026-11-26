package config

import (
	"slices"
	"strings"
	"testing"
)

func TestRetiredNetworkConfigRejected(t *testing.T) {
	for _, input := range []string{
		"[network]\nclient_allowlist = [\"192.0.2.7/32\"]\n",
		"[network.role_allowlist]\napp = [\"192.0.2.7/32\"]\n",
		"[network.role_allowlist]\nmigrate = [\"192.0.2.7/32\"]\n",
		"[network.role_allowlist]\nro = [\"192.0.2.7/32\"]\n",
		"[network]\nexpose_postgres = true\n",
		"[network]\nexpose_session_pooler = true\n",
		"[network]\nexpose_pgbouncer = true\n",
		"[network]\n",
	} {
		if _, err := ParseCluster([]byte(input)); err == nil {
			t.Errorf("retired config silently accepted: %q", input)
		}
	}

	cfg := DefaultCluster()
	if err := cfg.Set("network.client_allowlist", `["203.0.113.7/32"]`); err == nil {
		t.Fatal("retired config key accepted by Set")
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
			name: "restricting metrics",
			old:  Cluster{Metrics: Metrics{Enabled: true}},
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

func TestDatabaseClientAllowlist(t *testing.T) {
	cfg := followerCluster()

	cfg.Clusters[0].ClientAllowlist = []string{"192.0.2.0/24", "2001:db8::/32"}
	if p := validationProblems(cfg); strings.Contains(p, "clusters[") {
		t.Fatalf("valid allowlist rejected: %s", p)
	}

	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}

	parsed, err := ParseCluster(raw)
	if err != nil || !slices.Equal(parsed.Clusters[0].ClientAllowlist, cfg.Clusters[0].ClientAllowlist) {
		t.Fatalf("allowlist round trip: %v, %v", parsed.Clusters, err)
	}

	clone := cfg.Clone()

	clone.Clusters[0].ClientAllowlist[0] = "198.51.100.0/24"
	if cfg.Clusters[0].ClientAllowlist[0] != "192.0.2.0/24" {
		t.Fatal("clone shares database allowlist")
	}

	cfg.Clusters[0].ClientAllowlist = []string{"192.0.2.1"}
	if p := validationProblems(cfg); !strings.Contains(p, "clusters[0].client_allowlist") {
		t.Fatalf("invalid CIDR accepted: %s", p)
	}

	cfg.Clusters[1].ClientAllowlist = []string{"192.0.2.1/32"}
	if p := validationProblems(cfg); !strings.Contains(p, "clusters[1].client_allowlist must be empty") {
		t.Fatalf("follower grant accepted: %s", p)
	}
}

func TestPublicDatabaseCIDRs(t *testing.T) {
	cfg := followerCluster()
	cfg.Clusters[0].ClientAllowlist = []string{"192.0.2.0/24", "2001:db8::/32"}
	cfg.Clusters[1].ClientAllowlist = []string{"203.0.113.0/24"}
	cfg.Clusters = append(cfg.Clusters, ClusterSpec{Name: "other", ClientAllowlist: []string{"192.0.2.0/24", "198.51.100.0/24"}})
	want := []string{"192.0.2.0/24", "2001:db8::/32", "198.51.100.0/24"}

	got := cfg.PublicDatabaseCIDRs()
	if !slices.Equal(got, want) {
		t.Fatalf("union = %v, want %v", got, want)
	}

	got[0] = "changed"

	if cfg.Clusters[0].ClientAllowlist[0] != want[0] {
		t.Fatal("union shares input")
	}
}

func TestDatabaseExposureIncreases(t *testing.T) {
	old := Cluster{Clusters: []ClusterSpec{
		{Name: "app", Stanza: "app-123456", ClientAllowlist: []string{"192.0.2.0/24"}},
		{Name: "other", Stanza: "other-123456"},
	}}
	next := old.Clone()
	// Moving a CIDR to a different database widens that database even when
	// the shared network union is unchanged.
	next.Clusters[0].ClientAllowlist = nil

	next.Clusters[1].ClientAllowlist = []string{"192.0.2.0/24"}
	if got := ExposureIncreases(old, next); !slices.Equal(got, []string{"database:other"}) {
		t.Fatalf("moved grant = %v", got)
	}

	next = old.Clone()
	next.Clusters[0].ClientAllowlist = []string{"192.0.2.0/25", "192.0.2.128/25"}
	slices.Reverse(next.Clusters)

	if got := ExposureIncreases(old, next); len(got) != 0 {
		t.Fatalf("reordered and equivalent grant = %v", got)
	}

	next = old.Clone()

	next.Clusters[0].Stanza = "app-abcdef"
	if got := ExposureIncreases(old, next); !slices.Equal(got, []string{"database:app"}) {
		t.Fatalf("recreated database = %v", got)
	}
}
