package node

import (
	"strings"
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

	p := rpc.AccessBootstrapOwnerParams{User: "alice", Label: "machine-123", SSHPublicKey: sshKey, WGPublicKey: wgKey}

	index, err := bootstrapOwnerDevice(cfg, &registry, legacy, p)
	if err != nil {
		t.Fatal(err)
	}

	device := registry.Devices[index]
	if device.Label != p.Label || device.State != config.DevicePending || device.TemporaryAddress != "" ||
		device.Address == legacy.Operators[0].WGAddress || registry.Operators[0].Role != config.AccessRoleOwner {
		t.Fatalf("unexpected bootstrap state: %+v", device)
	}

	if again, err := bootstrapOwnerDevice(cfg, &registry, legacy, p); err != nil || again != index {
		t.Fatalf("resume result: index %d, error %v", again, err)
	}

	p.Label = "machine-other"
	if _, err := bootstrapOwnerDevice(cfg, &registry, legacy, p); err == nil {
		t.Fatal("accepted different setup device label")
	}

	p.Label = device.Label

	registry.Devices[index].State = config.DeviceActive
	if !isOriginalSetupDevice(registry, device.ID, device.OperatorID, device.Label) {
		t.Fatal("original setup device rejected")
	}

	other := device
	other.ID, other.Label, other.State = "dev-other", "another-device", config.DeviceActive
	other.RequestDigest = strings.Repeat("a", 64)

	registry.Devices = append(registry.Devices, other)
	if isOriginalSetupDevice(registry, other.ID, other.OperatorID, other.Label) ||
		isOriginalSetupDevice(registry, other.ID, other.OperatorID, device.Label) {
		t.Fatal("accepted another owner device")
	}

	registry.Devices = registry.Devices[:1]

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
