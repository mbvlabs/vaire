package cli

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

func TestRestoreResumePendingReadsSavedAnswers(t *testing.T) {
	for _, identity := range []string{"", "/test/replacement-key"} {
		t.Run("identity="+identity, func(t *testing.T) {
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			cfg := config.DefaultCluster()
			cfg.ClusterName, cfg.Domain = "prod", "example.com"
			cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
			cfg.Clusters = []config.ClusterSpec{{Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small", Protected: true}}
			boot := workspaces.Bootstrap{
				Host: "203.0.113.10", User: "root", Port: 22,
				Identity: "/test/original-key", HostKeyFingerprint: "SHA256:test",
			}
			prog := &workspaces.Progress{
				SetupOperationID: "original-operation",
				Phases:           map[string]string{phaseHost: "done", phaseFenced: "done"},
				Restore: &workspaces.RestoreProgress{
					Bundle: "original-bundle", From: "bucket", Epoch: 4,
					InstanceID: "original-instance", EpochWrittenAt: "2026-11-18T13:46:00Z",
					OldAddress: boot.Host, NewAddress: boot.Host,
				},
			}
			original := &initRun{name: "prod", cfg: cfg, boot: boot, prog: prog}
			if err := original.savePending(); err != nil {
				t.Fatal(err)
			}
			cmd := &cobra.Command{}
			cmd.Flags().Bool("resume", true, "")
			cmd.Flags().String("identity", identity, "")
			resumed := &initRun{name: "prod", cmd: cmd, restore: &restoreRun{}}
			if err := resumed.restoreLoadPending(); err != nil {
				t.Fatal(err)
			}
			if resumed.cfg.ClusterName != cfg.ClusterName ||
				!reflect.DeepEqual(resumed.cfg.Nodes, cfg.Nodes) ||
				len(resumed.cfg.Clusters) != 1 || !resumed.cfg.Clusters[0].Protected ||
				resumed.cfg.Clusters[0].Stanza != "app-abc123" {
				t.Fatalf("pending cluster configuration changed: %+v", resumed.cfg)
			}
			if identity != "" {
				boot.Identity = identity
			}
			if resumed.boot != boot || !reflect.DeepEqual(resumed.prog, prog) ||
				!reflect.DeepEqual(resumed.restore.state, prog.Restore) {
				t.Fatalf("resume changed bootstrap or recovery identity: %+v %+v", resumed.boot, resumed.prog)
			}
		})
	}
}

func TestRestoreResumePendingStillRejectsUnknownClusterKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n[unexpected]\nvalue = true\n")...)
	if err := workspaces.SaveAnswers("prod", raw, workspaces.Bootstrap{Host: "203.0.113.10", User: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := workspaces.SaveProgress("prod", &workspaces.Progress{Restore: &workspaces.RestoreProgress{Epoch: 4}}); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.Flags().Bool("resume", true, "")
	cmd.Flags().String("identity", "", "")
	r := &initRun{name: "prod", cmd: cmd, restore: &restoreRun{}}
	if err := r.restoreLoadPending(); err == nil || !strings.Contains(err.Error(), "unexpected") {
		t.Fatalf("unknown cluster keys were not rejected: %v", err)
	}
}

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
			testRestoreConfigKeepsExtensionIntent(t, tc.recovery)
		})
	}
}

func testRestoreConfigKeepsExtensionIntent(t *testing.T, recovery *[]string) {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"
	cfg.Domain = "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	cfg.Clusters = []config.ClusterSpec{{
		Name: "app", Node: "db1", Port: 5433, Stanza: "app-abc123", Size: "small",
		StatementTimeout: "60s", Extensions: []string{"custom", "pg_stat_statements"},
		RecoveryExtensions: recovery,
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
			ctx:    &workspaces.Workspace{Server: "prod"},
			state:  &workspaces.RestoreProgress{NewAddress: "203.0.113.20"},
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
	case recovery == nil:
		if r.cfg.Clusters[0].RecoveryExtensions != nil {
			t.Fatalf("absent declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
		}
	case len(*recovery) == 0:
		if r.cfg.Clusters[0].RecoveryExtensions == nil || len(*r.cfg.Clusters[0].RecoveryExtensions) != 0 {
			t.Fatalf("explicit empty declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
		}
	default:
		if r.cfg.Clusters[0].RecoveryExtensions == nil || !slices.Equal(*r.cfg.Clusters[0].RecoveryExtensions, *recovery) {
			t.Fatalf("populated declaration became %v", r.cfg.Clusters[0].RecoveryExtensions)
		}
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
		r := &initRun{cfg: cfg, restore: &restoreRun{state: &workspaces.RestoreProgress{}}}
		if got := r.restoreResultData().Next; !slices.Equal(got, want) {
			t.Fatalf("configuration %v:\n got %v\nwant %v", clusters, got, want)
		}
	}
}
