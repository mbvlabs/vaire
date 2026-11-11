package cli

import (
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/contexts"
	"github.com/mbvlabs/vaire/internal/masterbackup"
)

// The real master bundle path must keep global descriptors and all three
// recovery declaration states through Build, ReadTar and restoreConfig's
// public-address rewrite.
func TestRestoreConfigKeepsExtensionIntent(t *testing.T) {
	cases := []struct {
		name     string
		recovery *[]string
	}{
		{name: "absent", recovery: nil},
		{name: "explicit empty", recovery: &[]string{}},
		{name: "populated", recovery: &[]string{"custom", "pg_trgm"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultCluster()
			cfg.ClusterName = "prod"
			cfg.Domain = "example.com"
			cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
			cfg.Clusters = []config.ClusterSpec{{
				Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small",
				StatementTimeout: "60s", Extensions: []string{"custom", "pg_stat_statements"},
				RecoveryExtensions: tc.recovery,
			}}
			cfg.Extensions = map[string]config.ExtensionDefinition{
				"custom": {
					Source: catalog.SourcePreinstalled, Settings: map[string]string{"custom.mode": "on"},
					CopySafety: stringPtr("safe"), StandbySafe: boolPtr(true),
				},
			}
			raw, err := cfg.Encode()
			if err != nil {
				t.Fatal(err)
			}
			tarball, _, err := masterbackup.Build(masterbackup.Meta{
				Server: "prod", Node: "db1", Epoch: 1, CreatedAt: time.Unix(0, 0).UTC(), VaireVersion: "test",
			}, []masterbackup.File{
				masterbackup.NewFile(masterbackup.ClusterPath, "root", "root", 0o640, raw),
			})
			if err != nil {
				t.Fatal(err)
			}
			bundle, err := masterbackup.ReadTar(tarball)
			if err != nil {
				t.Fatal(err)
			}
			r := &initRun{
				name: "prod",
				restore: &restoreRun{
					bundle: bundle,
					ctx:    &contexts.Context{Server: "prod"},
					state:  &contexts.RestoreProgress{NewAddress: "203.0.113.20"},
				},
			}
			if err := r.restoreConfig(); err != nil {
				t.Fatal(err)
			}
			if r.cfg.Nodes[0].PublicAddress != "203.0.113.20" {
				t.Fatalf("public address was not rewritten: %+v", r.cfg.Nodes[0])
			}
			if got := r.cfg.Extensions["custom"]; got.Source != catalog.SourcePreinstalled || got.Settings["custom.mode"] != "on" {
				t.Fatalf("custom definition lost: %+v", got)
			}
			switch {
			case tc.recovery == nil:
				if r.cfg.Clusters[0].RecoveryExtensions != nil {
					t.Fatalf("absent declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
				}
			case len(*tc.recovery) == 0:
				if r.cfg.Clusters[0].RecoveryExtensions == nil || len(*r.cfg.Clusters[0].RecoveryExtensions) != 0 {
					t.Fatalf("explicit empty declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
				}
			default:
				if r.cfg.Clusters[0].RecoveryExtensions == nil || !slices.Equal(*r.cfg.Clusters[0].RecoveryExtensions, *tc.recovery) {
					t.Fatalf("populated declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
				}
			}
		})
	}
}

func stringPtr(s string) *string { return &s }
func boolPtr(b bool) *bool       { return &b }

// A no-bucket rebuild's next commands drop followers before their sources,
// create the sources and re-follow the followers: no db create of a follower,
// whatever order the configuration lists them in (D12, R3-4).
func TestRestoreResultDataNoBucketOrdersFollowersFirst(t *testing.T) {
	source := config.ClusterSpec{Name: "app", Node: "db1", Port: 5433}
	follower := config.ClusterSpec{Name: "app_follower", Node: "db1", Port: 5434, Follows: "app"}
	want := []string{
		"vaire db drop app_follower --confirm app_follower",
		"vaire db drop app --confirm app",
		"vaire db create app",
		"vaire db follow app --name app_follower",
		"vaire doctor",
		"vaire server backup list",
	}
	for _, clusters := range [][]config.ClusterSpec{{source, follower}, {follower, source}} {
		cfg := config.DefaultCluster()
		cfg.Clusters = clusters
		r := &initRun{cfg: cfg, restore: &restoreRun{state: &contexts.RestoreProgress{}}}
		if got := r.restoreResultData().Next; !slices.Equal(got, want) {
			t.Fatalf("configuration %v:\n got %v\nwant %v", clusters, got, want)
		}
	}
}
