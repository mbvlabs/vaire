package node

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/transport"
)

func TestBootstrapOwnerPinsFirstDevice(t *testing.T) {
	cfg := config.Cluster{
		Transport: config.Transport{Subnet: "10.42.0.0/24"},
		Nodes:     []config.NodeSpec{{WGAddress: "10.42.0.1"}},
	}
	legacy := config.Operators{Operators: []config.Operator{{Name: "alice", WGAddress: "10.42.0.100"}}}

	var registry config.AccessRegistry

	_, sshKey, err := transport.GenerateSSHKey("")
	if err != nil {
		t.Fatal(err)
	}

	_, wgKey, err := transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	p := rpc.AccessBootstrapOwnerParams{User: "alice", SSHPublicKey: sshKey, WGPublicKey: wgKey}

	index, err := bootstrapOwnerDevice(cfg, &registry, legacy, p)
	if err != nil {
		t.Fatal(err)
	}

	device := registry.Devices[index]
	if device.Label != setupDeviceLabel || device.State != config.DevicePending || device.TemporaryAddress != "" ||
		device.Address == legacy.Operators[0].WGAddress || registry.Operators[0].Role != config.AccessRoleOwner {
		t.Fatalf("unexpected bootstrap state: %+v", device)
	}

	if again, err := bootstrapOwnerDevice(cfg, &registry, legacy, p); err != nil || again != index {
		t.Fatalf("resume result: index %d, error %v", again, err)
	}

	_, p.WGPublicKey, err = transport.GenerateWGKey()
	if err != nil {
		t.Fatal(err)
	}

	if _, err := bootstrapOwnerDevice(cfg, &registry, legacy, p); err == nil {
		t.Fatal("accepted different owner device keys")
	}

	if len(registry.Operators) != 1 || len(registry.Devices) != 1 {
		t.Fatal("retry created additional identities")
	}
}

func TestBootstrapOwnerRequiresOriginalOperation(t *testing.T) {
	cfg := bootstrapOwnershipFixture(t)
	if err := prepareBootstrapOwnership(cfg, "db1", "original-setup"); err != nil {
		t.Fatal(err)
	}

	for _, operation := range []string{"", "another-setup"} {
		if err := authorizeBootstrapOperation(cfg.Name(), "db1", operation); err == nil {
			t.Fatal("non-bootstrap operation was accepted")
		}
	}

	if err := authorizeBootstrapOperation(cfg.Name(), "db1", "original-setup"); err != nil {
		t.Fatal(err)
	}
}
