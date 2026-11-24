package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/remote"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/transport"
	"github.com/mbvlabs/vaire/internal/workspaces"
	"github.com/spf13/cobra"
)

type failingOwnerCaller struct {
	requests   []rpc.AccessBootstrapOwnerParams
	operations []string
}

func (c *failingOwnerCaller) Call(_ context.Context, method string, params any, opts remote.CallOptions) (remote.Reply, error) {
	if method != "access.bootstrap-owner" {
		panic("unexpected method " + method)
	}

	p, ok := params.(rpc.AccessBootstrapOwnerParams)
	if !ok {
		return remote.Reply{}, errors.New("unexpected bootstrap parameters")
	}

	c.requests = append(c.requests, p)
	c.operations = append(c.operations, opts.OperationID)

	return remote.Reply{}, errors.New("lost response")
}

func TestBootstrapOwnerPersistsKeysBeforeRPCAndResumes(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	cmd := &cobra.Command{}
	r := &initRun{name: "prod", sec: workspaces.PendingSecrets{OperatorName: "owner"},
		cmd: cmd, p: newProgress(cmd), prog: &workspaces.Progress{SetupOperationID: "setup-id"}}
	caller := &failingOwnerCaller{}

	_, keys, err := r.bootstrapOwner(context.Background(), caller)
	if err == nil {
		t.Fatal("expected lost response")
	}

	saved, err := workspaces.LoadSecrets(r.name)
	if err != nil {
		t.Fatal(err)
	}

	if keys.SSH == "" || keys.WG == "" || !machineOperatorNamePattern.MatchString(saved.SetupDeviceLabel) ||
		saved.OwnerSSHPrivateKey != keys.SSH || saved.OwnerWGPrivateKey != keys.WG || caller.requests[0].Label != saved.SetupDeviceLabel {
		t.Fatal("owner keys not persisted before RPC")
	}

	r.sec = saved

	_, again, err := r.bootstrapOwner(context.Background(), caller)
	if err == nil || again != keys || len(caller.requests) != 2 || caller.requests[0] != caller.requests[1] {
		t.Fatal("resume did not retry the same owner keys")
	}

	if caller.operations[0] != "setup-id" || caller.operations[1] != "setup-id" {
		t.Fatal("bootstrap did not use the persisted setup operation")
	}
}

func TestOwnerSetupChecksNodePins(t *testing.T) {
	_, host, err := transport.GenerateSSHKey("host")
	if err != nil {
		t.Fatal(err)
	}

	r := &initRun{name: "prod", sec: workspaces.PendingSecrets{OperatorName: "owner", SetupDeviceLabel: "setup-device"},
		cfg:  config.Cluster{Nodes: []config.NodeSpec{{Name: "db", Role: "master", PublicAddress: "192.0.2.1", WGAddress: "10.0.0.1"}}, Transport: config.Transport{ListenPort: 51820}},
		prog: &workspaces.Progress{WGPublicKey: "node-key", SSHHostKeys: []string{host}}}

	s := rpc.AccessSetup{ID: "device", Label: "setup-device", UnixLogin: "device-user", Address: "10.0.0.3", Server: "prod", Node: "db", User: "owner", NodeAddress: "10.0.0.1",
		NodePublicAddress: "192.0.2.1", NodeWGPublicKey: "node-key", WGPort: 51820, HostPublicKey: host}
	if err := r.validateOwnerSetup(s); err != nil {
		t.Fatal(err)
	}

	s.Label = "another-device"
	if err := r.validateOwnerSetup(s); err == nil {
		t.Fatal("accepted changed device label")
	}

	s.Label = "setup-device"
	s.NodePublicAddress = "192.0.2.2"

	if err := r.validateOwnerSetup(s); err == nil {
		t.Fatal("accepted changed endpoint")
	}

	s.NodePublicAddress = "192.0.2.1"

	_, s.HostPublicKey, err = transport.GenerateSSHKey("other")
	if err != nil {
		t.Fatal(err)
	}

	if err := r.validateOwnerSetup(s); err == nil || !strings.Contains(err.Error(), "unpinned") {
		t.Fatalf("accepted changed host key: %v", err)
	}
}

func TestOwnerInstallResumeAndRecoverySeparation(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	legacySSH, host, err := transport.GenerateSSHKey("legacy")
	if err != nil {
		t.Fatal(err)
	}

	ownerSSH, _, err := transport.GenerateSSHKey("owner")
	if err != nil {
		t.Fatal(err)
	}

	ownerWG, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	keys := enrollmentKeys{SSH: string(ownerSSH), WG: ownerWG}
	r := &initRun{name: "prod", sec: workspaces.PendingSecrets{OperatorName: "owner", SSHPrivateKey: string(legacySSH),
		WGPrivateKey: "legacy-wg", OwnerSSHPrivateKey: keys.SSH, OwnerWGPrivateKey: keys.WG, RecoveryCode: "recovery-code"},
		cfg:  config.Cluster{Nodes: []config.NodeSpec{{Name: "db", Role: "master", PublicAddress: "192.0.2.1", WGAddress: "10.0.0.1"}}},
		prog: &workspaces.Progress{WGPublicKey: "node-key", SSHHostKeys: []string{host}}}

	doc, err := r.document()
	if err != nil {
		t.Fatal(err)
	}

	if doc.RecoveryCode != "recovery-code" || doc.Operator.SSHPrivateKey != "" || doc.Operator.WGPrivateKey != "" || doc.Operator.SudoPassword != "" {
		t.Fatal("handoff must contain the recovery code but no reusable login")
	}

	s := rpc.AccessSetup{ID: "device", Server: "prod", Node: "db", User: "owner", UnixLogin: "device-user",
		Address: "10.0.0.3", NodeAddress: "10.0.0.1", NodePublicAddress: "192.0.2.1",
		NodeWGPublicKey: "node-key", WGPort: 51820, HostPublicKey: host}

	creds := workspaces.Credentials{SudoPassword: "recovery-sudo"}
	if err := r.installOrResumeOwner(s, keys, creds, "locked"); err != nil {
		t.Fatal(err)
	}

	if err := r.installOrResumeOwner(s, keys, creds, "locked"); err != nil {
		t.Fatalf("matching installation could not resume: %v", err)
	}

	keys.WG = "different"
	if err := r.installOrResumeOwner(s, keys, creds, "locked"); err == nil {
		t.Fatal("accepted a different pending identity")
	}
}

func TestInitInstallsServiceCredentialsWithoutRecoverySudo(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	sshKey, host, err := transport.GenerateSSHKey("owner")
	if err != nil {
		t.Fatal(err)
	}

	wgKey, _, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	database := &workspaces.DatabaseCreds{Admin: workspaces.Role{User: "vaire_admin", Password: "db-secret"}}
	r := &initRun{name: "prod", cmd: cmd, p: newProgress(cmd),
		sec: workspaces.PendingSecrets{SudoPassword: "recovery-only", Database: database},
		cfg: config.Cluster{Nodes: []config.NodeSpec{{Name: "db", Role: "master", PublicAddress: "192.0.2.1", WGAddress: "10.0.0.1"}}}}

	s := rpc.AccessSetup{Server: "prod", Node: "db", User: "owner", UnixLogin: "device-user",
		Address: "10.0.0.3", NodeAddress: "10.0.0.1", NodePublicAddress: "192.0.2.1",
		NodeWGPublicKey: "node-key", WGPort: 51820, HostPublicKey: host}
	if err := r.initInstallWorkspace(s, enrollmentKeys{SSH: string(sshKey), WG: wgKey}, "locked", &initResult{}, r.master()); err != nil {
		t.Fatal(err)
	}

	creds, err := workspaces.LoadCredentials(r.name)
	if err != nil {
		t.Fatal(err)
	}

	if creds.SudoPassword != "" || creds.Database == nil || creds.Database.Admin.Password != "db-secret" {
		t.Fatalf("incorrect managed credentials: %+v", creds)
	}
}
