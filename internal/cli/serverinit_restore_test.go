package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secretsdoc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

func TestRestoreWithoutWorkspaceUsesKitNotEnrollment(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv(workspaces.EnvWorkspace, "")

	kit := writeOfflineRestoreKit(t)
	root := NewRoot()

	cmd, _, err := root.Find([]string{"server", "init"})
	if err != nil {
		t.Fatal(err)
	}

	for flag, value := range map[string]string{
		"restore": "true", "host": "root@192.0.2.2", "recovery-kit": kit,
		"passphrase-stdin": "true", "old-master-unreachable": "true",
	} {
		if err := cmd.Flags().Set(flag, value); err != nil {
			t.Fatal(err)
		}
	}

	cmd.SetIn(strings.NewReader("example-passphrase\n"))

	r, rr, backup, err := newRestoreRun(cmd)
	if err != nil {
		t.Fatal(err)
	}

	if r.name != "prod" || rr.ctx.Operator.Name != "owner" || backup.Bucket != "test-bucket" ||
		rr.identity != backup.Encryption.Identity || workspaces.Exists("prod") {
		t.Fatal("restore did not derive its context from the kit alone")
	}

	if err := cmd.Flags().Set("old-master-unreachable", "false"); err != nil {
		t.Fatal(err)
	}

	cmd.SetIn(strings.NewReader("example-passphrase\n"))

	if _, _, _, err := newRestoreRun(cmd); err == nil || workspaces.Exists("prod") {
		t.Fatal("restore from kit did not require acknowledgement that the old node is offline")
	}
}

func writeOfflineRestoreKit(t *testing.T) string {
	t.Helper()

	identity, recipient, err := backup.NewIdentity()
	if err != nil {
		t.Fatal(err)
	}

	_, host, err := transport.GenerateSSHKey("test-server")
	if err != nil {
		t.Fatal(err)
	}

	_, wg, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	doc := secretsdoc.Document{
		Schema: secretsdoc.Schema, Server: secretsdoc.Server{Name: "prod", TLSMode: "self-signed"},
		Nodes: []secretsdoc.Node{{Name: "db1", Role: workspaces.RoleMaster, PublicAddress: "192.0.2.1",
			WGAddress: "10.42.0.1", WGPublicKey: wg, ListenPort: 51820, SSHHostKeys: []string{host}}},
		Operator: secretsdoc.Operator{Name: "owner"}, RecoveryCode: "irrelevant-spent-code",
		Backup: &workspaces.BackupCreds{Provider: "r2", Bucket: "test-bucket", Prefix: "prod",
			Endpoint: "https://example.test", Region: "auto", URIStyle: "path",
			AccessKeyID: "test-access-id", SecretAccessKey: "test-access-secret"},
	}
	doc.Backup.Encryption.Identity, doc.Backup.Encryption.Recipient = identity, recipient
	doc.Backup.PgBackRest.CipherPass = "test-backup-cipher"

	raw, err := secretsdoc.EncryptArmored(doc, "example-passphrase")
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "recovery.age")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func TestInstallOfflineRestoreWorkspace(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	_, host, err := transport.GenerateSSHKey("test-node")
	if err != nil {
		t.Fatal(err)
	}

	r := &initRun{name: "prod", restore: &restoreRun{ctx: &workspaces.Workspace{
		Name: "prod", Server: "prod",
		Nodes: []workspaces.Node{{Name: "db1", Role: workspaces.RoleMaster}},
	}}}
	setup := rpc.AccessSetup{Server: "prod", Node: "db1", User: "owner", Label: "new-device",
		UnixLogin: "vdev-test", Address: "10.42.0.2", NodeAddress: "10.42.0.1",
		NodeWGPublicKey: "node-key", NodePublicAddress: "192.0.2.2", HostPublicKey: host, WGPort: 51820}
	keys := enrollmentKeys{SSH: "new-ssh", WG: "new-wg"}

	if err := r.installKitRestoreWorkspace(setup, keys); err != nil {
		t.Fatal(err)
	}

	if name, err := workspaces.Current(); err != nil || name != "prod" {
		t.Fatalf("restored workspace is not current: %q, %v", name, err)
	}

	if err := r.installKitRestoreWorkspace(setup, keys); err != nil {
		t.Fatalf("cannot resume after installing workspace: %v", err)
	}

	if err := r.installKitRestoreWorkspace(setup, enrollmentKeys{SSH: "different", WG: keys.WG}); err == nil {
		t.Fatal("resume accepted different device keys")
	}
}

func TestOfflineRestorePinsBackupHostIdentity(t *testing.T) {
	sshPrivate, host, err := transport.GenerateSSHKey("test-node")
	if err != nil {
		t.Fatal(err)
	}

	wgPrivate, wgPublic, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	rr := restoreRun{ctx: &workspaces.Workspace{Nodes: []workspaces.Node{
		{Name: "db1", Role: workspaces.RoleMaster, WGPublicKey: wgPublic},
	}}, kitHostKeys: []string{host}}
	b := masterbackup.Bundle{Manifest: masterbackup.Manifest{Node: "db1"}, Files: []masterbackup.File{
		masterbackup.NewFile(masterbackup.WGKeyPath, "root", "root", 0o600, []byte(wgPrivate+"\n")),
		masterbackup.NewFile("/etc/ssh/ssh_host_ed25519_key", "root", "root", 0o600, sshPrivate),
	}}

	if err := rr.validateKitBundle(b); err != nil {
		t.Fatal(err)
	}

	rr.ctx.Nodes[0].WGPublicKey = "different-key"

	if err := rr.validateKitBundle(b); err == nil {
		t.Fatal("accepted a bundle with a different WireGuard identity")
	}
}

func TestRestoreTemporaryTransportAndWorkspaceExchange(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	ws := &workspaces.Workspace{Name: "prod", Server: "prod", Managed: true,
		Operator:  workspaces.Operator{Name: "owner", SSHUser: "old-device", WGAddress: "10.42.0.9"},
		Nodes:     []workspaces.Node{{Name: "db", Role: workspaces.RoleMaster, PublicAddress: "192.0.2.1", WGAddress: "10.42.0.1"}},
		Databases: map[string]string{"app": "db"}, TunnelPersist: "3m"}
	if err := workspaces.Save(ws); err != nil {
		t.Fatal(err)
	}

	if err := workspaces.SaveCredentials("prod", workspaces.Credentials{}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{workspaces.SSHKeyFile, workspaces.WGKeyFile} {
		if err := transport.WriteSecretFile(workspaces.Path("prod", name), []byte("old-device-key")); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"

	cfg.Nodes = []config.NodeSpec{{Name: "db", Role: config.RoleMaster, WGAddress: "10.42.0.1"}}

	cmd := &cobra.Command{}

	r := &initRun{name: "prod", cmd: cmd, cfg: cfg, prog: &workspaces.Progress{}, restore: &restoreRun{ctx: ws}}
	if err := r.restoreSecrets(); err != nil {
		t.Fatal(err)
	}

	if r.sec.SSHPrivateKey == "old-device-key" || r.sec.WGPrivateKey == "old-device-key" ||
		ws.Operator.WGAddress != "10.42.0.2" {
		t.Fatal("temporary transport was not independently generated")
	}

	first := r.sec
	if err := r.restoreSecrets(); err != nil {
		t.Fatal(err)
	}

	if r.sec.SSHPrivateKey != first.SSHPrivateKey || r.sec.WGPrivateKey != first.WGPrivateKey {
		t.Fatal("resume regenerated temporary transport")
	}

	ownerSSH, hostPub, err := transport.GenerateSSHKey("test-owner")
	if err != nil {
		t.Fatal(err)
	}

	ownerWG, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	keys := enrollmentKeys{SSH: string(ownerSSH), WG: ownerWG}

	setup := rpc.AccessSetup{Server: "prod", Node: "db", User: "owner", UnixLogin: "new-device",
		Address: "10.42.0.3", NodeAddress: "10.42.0.1", NodePublicAddress: "192.0.2.2",
		NodeWGPublicKey: "node-key", HostPublicKey: hostPub, WGPort: 51820}
	if err := r.replaceRestoreWorkspace(setup, keys); err != nil {
		t.Fatal(err)
	}

	saved, err := workspaces.Load("prod")
	if err != nil {
		t.Fatal(err)
	}

	if saved.Operator.SSHUser != setup.UnixLogin || saved.Operator.WGAddress != setup.Address ||
		saved.Nodes[0].PublicAddress != setup.NodePublicAddress || saved.Databases["app"] != "db" || saved.TunnelPersist != "3m" {
		t.Fatalf("workspace not replaced correctly: %+v", saved)
	}

	if err := r.replaceRestoreWorkspace(setup, keys); err != nil {
		t.Fatalf("workspace replacement is not resumable: %v", err)
	}

	if _, err := os.Stat(filepath.Join(workspaces.PendingDir("prod"), "restore-workspace")); !os.IsNotExist(err) {
		t.Fatalf("old workspace remains in pending: %v", err)
	}
}

func TestRestoreOldMasterMissingKeyRequiresOverride(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cmd := &cobra.Command{}
	cmd.Flags().Bool("old-master-unreachable", false, "")

	r := &initRun{cmd: cmd, restore: &restoreRun{
		ctx:   &workspaces.Workspace{Name: "prod", Nodes: []workspaces.Node{{Role: workspaces.RoleMaster, PublicAddress: "192.0.2.1"}}},
		state: &workspaces.RestoreProgress{OldAddress: "192.0.2.1", NewAddress: "192.0.2.2"},
	}}
	if err := r.checkOldMaster(context.Background()); err == nil || !strings.Contains(err.Error(), "old master") {
		t.Fatalf("missing key did not fail closed: %v", err)
	}

	if err := cmd.Flags().Set("old-master-unreachable", "true"); err != nil {
		t.Fatal(err)
	}

	if err := r.checkOldMaster(context.Background()); err != nil {
		t.Fatalf("explicit override failed: %v", err)
	}
}

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

func TestRestoreResumeLoadsPendingOwnerKeys(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cfg := config.DefaultCluster()
	cfg.ClusterName = "prod"

	r := &initRun{name: "prod", cfg: cfg, prog: &workspaces.Progress{
		Phases:  map[string]string{phaseSecrets: "done"},
		Restore: &workspaces.RestoreProgress{},
	}}
	if err := r.savePending(); err != nil {
		t.Fatal(err)
	}

	want := workspaces.PendingSecrets{OwnerSSHPrivateKey: "test-owner-ssh", OwnerWGPrivateKey: "test-owner-wg"}
	if err := workspaces.SaveSecrets("prod", want); err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.Flags().Bool("resume", true, "")
	cmd.Flags().String("identity", "", "")

	resumed := &initRun{name: "prod", cmd: cmd, restore: &restoreRun{}}
	if err := resumed.restoreLoadPending(); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(resumed.sec, want) {
		t.Fatal("resume did not retain the pending owner identity")
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
			CopySafety: new("safe"), StandbySafe: new(true),
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
