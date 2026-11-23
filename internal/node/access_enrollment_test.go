package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func TestCloseAccessSSHChecksOnlyRevokedWireGuardEndpoint(t *testing.T) {
	original := temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessCommand = original })

	ipv4 := []string{"-H", "-4", "-t", "state", "established", "src", "10.42.0.1:22", "dst", "10.42.0.2"}
	ipv6 := []string{"-H", "-6", "-t", "state", "established", "src", "[::ffff:10.42.0.1]:22", "dst", "[::ffff:10.42.0.2]"}
	want := [][]string{append([]string{"-K"}, ipv4...), ipv4, append([]string{"-K"}, ipv6...), ipv6}
	calls := 0
	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		if name != "ss" || calls >= len(want) || !slices.Equal(args, want[calls]) {
			t.Fatalf("wrong socket selection: %s %v", name, args)
		}

		calls++

		return "", nil
	}

	if err := closeAccessSSH(t.Context(), "10.42.0.1", "10.42.0.2"); err != nil || calls != len(want) {
		t.Fatalf("closed and checked sockets: calls %d, err %v", calls, err)
	}

	if err := closeAccessSSH(t.Context(), "10.42.0.1", "10.42.0.1"); err == nil || calls != len(want) {
		t.Fatalf("accepted same endpoints: calls %d, err %v", calls, err)
	}
}

func TestAccessRedemptionBindsSourceTokenAndKeys(t *testing.T) {
	now := time.Now()
	p := rpc.AccessRedeemParams{ID: "dev-1", Token: "test-token", SSHPublicKey: "ssh-key", WGPublicKey: "wg-key"}
	digest := sha256.Sum256([]byte(p.Token))

	d := config.AccessDevice{
		ID: p.ID, TemporaryAddress: "10.42.0.4", State: config.DevicePending,
		RequestDigest: hex.EncodeToString(digest[:]), ExpiresAt: now.Add(time.Minute),
	}
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, now); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, source  string
		changeRequest func(*rpc.AccessRedeemParams)
		changeDevice  func(*config.AccessDevice)
	}{
		{name: "wrong peer", source: "10.42.0.9"},
		{name: "wrong token", changeRequest: func(p *rpc.AccessRedeemParams) { p.Token = "wrong" }},
		{name: "expired", changeDevice: func(d *config.AccessDevice) { d.ExpiresAt = now }},
		{name: "revoked", changeDevice: func(d *config.AccessDevice) { d.State = config.DeviceRevoked }},
		{name: "different keys on repeat", changeDevice: func(d *config.AccessDevice) {
			d.State, d.SSHPublicKey, d.WGPublicKey = config.DeviceActive, "different-ssh", "different-wg"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, dev, src := p, d, d.TemporaryAddress
			if tc.source != "" {
				src = tc.source
			}

			if tc.changeRequest != nil {
				tc.changeRequest(&req)
			}

			if tc.changeDevice != nil {
				tc.changeDevice(&dev)
			}

			if err := validateAccessRedemption(dev, src, req, now); err == nil {
				t.Fatal("unexpected redemption")
			}
		})
	}

	d.SSHPublicKey, d.WGPublicKey, d.State, d.Cleanup = p.SSHPublicKey, p.WGPublicKey, config.DeviceActive, true
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, now); err != nil {
		t.Fatalf("winning activation could not resume: %v", err)
	}

	d.Cleanup = false
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, now); err == nil {
		t.Fatal("redeemed lease accepted after peer cleanup")
	}
}

func TestAccessAddressesKeepsLegacyAndManagedPeersDistinct(t *testing.T) {
	cfg := config.Cluster{
		Transport: config.Transport{Subnet: "10.42.0.0/24"},
		Nodes:     []config.NodeSpec{{WGAddress: "10.42.0.1"}},
	}
	legacy := config.Operators{Operators: []config.Operator{{WGAddress: "10.42.0.2"}}}
	registry := config.AccessRegistry{Devices: []config.AccessDevice{{State: config.DevicePending, Address: "10.42.0.3", TemporaryAddress: "10.42.0.4"}}}

	permanent, temporary, err := accessAddresses(cfg, registry, legacy)
	if err != nil || permanent != "10.42.0.5" || temporary != "10.42.0.6" {
		t.Fatalf("permanent=%q temporary=%q err=%v", permanent, temporary, err)
	}
}
