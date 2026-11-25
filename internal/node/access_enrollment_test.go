package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"strings"
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

func TestSupersedeDeviceInvite(t *testing.T) {
	now := time.Now()
	req := rpc.AccessRedeemParams{Token: "old-test-token"}
	digest := sha256.Sum256([]byte(req.Token))
	unused := config.AccessDevice{
		ID: "dev-old", OperatorID: "operator", Label: "laptop", State: config.DevicePending,
		TemporaryAddress: "10.42.0.4", RequestDigest: hex.EncodeToString(digest[:]), ExpiresAt: now.Add(time.Minute),
	}

	for _, tc := range []struct {
		name   string
		change func(*config.AccessDevice)
	}{
		{"unused", nil},
		{"active", func(d *config.AccessDevice) { d.State = config.DeviceActive }},
		{"recovery", func(d *config.AccessDevice) { d.Recovery = true }},
		{"redeeming", func(d *config.AccessDevice) { d.WGPublicKey = "bound-key" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := unused
			if tc.change != nil {
				tc.change(&old)
			}

			r := config.AccessRegistry{Devices: []config.AccessDevice{old}}
			err := prepareDeviceInvite(&r, old.OperatorID, old.Label, true)

			if tc.change != nil {
				if err == nil || r.Devices[0] != old {
					t.Fatal("changed a protected device")
				}

				return
			}

			got := r.Devices[0]
			if err != nil || got.State != config.DeviceRevoked || !got.Cleanup || got.TemporaryAddress != old.TemporaryAddress {
				t.Fatalf("invite not invalidated with cleanup retained: %v", err)
			}

			if validateAccessRedemption(got, old.TemporaryAddress, req, now) == nil {
				t.Fatal("superseded code was still redeemable")
			}
		})
	}
}

func TestDeviceInviteRequiresExplicitFresh(t *testing.T) {
	d := config.AccessDevice{OperatorID: "operator", Label: "laptop", State: config.DevicePending, TemporaryAddress: "10.42.0.4"}
	r := config.AccessRegistry{Devices: []config.AccessDevice{d}}

	if err := prepareDeviceInvite(&r, d.OperatorID, d.Label, false); err == nil || r.Devices[0] != d {
		t.Fatal("plain add replaced an existing invite")
	}

	if err := prepareDeviceInvite(&r, d.OperatorID, "new-device", true); err == nil {
		t.Fatal("--fresh accepted an unknown label")
	}
}

func TestFreshDeviceInviteAfterExpiryCleanup(t *testing.T) {
	d := config.AccessDevice{OperatorID: "operator", Label: "laptop", State: config.DeviceRevoked}
	r := config.AccessRegistry{Devices: []config.AccessDevice{d}}

	if err := prepareDeviceInvite(&r, d.OperatorID, d.Label, false); err == nil {
		t.Fatal("plain add accepted an expired invite without --fresh")
	}

	if err := prepareDeviceInvite(&r, d.OperatorID, d.Label, true); err != nil {
		t.Fatalf("--fresh refused an expired invite after cleanup: %v", err)
	}
}

func TestRecoveryRedemptionDoesNotExpireAndPinsWinningKeys(t *testing.T) {
	p := rpc.AccessRedeemParams{Token: "recovery-token", SSHPublicKey: "ssh", WGPublicKey: "wg"}
	digest := sha256.Sum256([]byte(p.Token))

	d := config.AccessDevice{Recovery: true, State: config.DevicePending, TemporaryAddress: "10.0.0.4", RequestDigest: hex.EncodeToString(digest[:])}
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, time.Now().AddDate(20, 0, 0)); err != nil {
		t.Fatal(err)
	}

	d.SSHPublicKey, d.WGPublicKey = p.SSHPublicKey, p.WGPublicKey
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, time.Now()); err != nil {
		t.Fatal(err)
	}

	other := p
	other.WGPublicKey = "other"

	if err := validateAccessRedemption(d, d.TemporaryAddress, other, time.Now()); err == nil {
		t.Fatal("recovery accepted different keys after first attempt")
	}

	d.State, d.Cleanup = config.DeviceActive, false
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, time.Now()); err == nil {
		t.Fatal("recovery accepted after completed redemption")
	}

	d.State = config.DeviceRevoked
	if err := validateAccessRedemption(d, d.TemporaryAddress, p, time.Now()); err == nil {
		t.Fatal("revoked recovery accepted")
	}
}

func TestRecoveryLeaseRestorationAfterRebootAndSync(t *testing.T) {
	now := time.Now()
	if !accessLeaseUsable(config.AccessDevice{Recovery: true}, now.AddDate(10, 0, 0)) {
		t.Fatal("unused recovery peer would not be restored")
	}

	if accessLeaseUsable(config.AccessDevice{ExpiresAt: now.Add(-time.Second)}, now) {
		t.Fatal("expired invite would be restored")
	}

	old := temporaryAccessCommand

	t.Cleanup(func() { temporaryAccessCommand = old })

	var calls []string

	temporaryAccessCommand = func(_ context.Context, name string, args ...string) (string, error) {
		call := name + " " + strings.Join(args, " ")
		calls = append(calls, call)

		if strings.Contains(call, " -C ") {
			return "", errors.New("guard missing")
		}

		return "", nil
	}
	r := config.AccessRegistry{Devices: []config.AccessDevice{{
		Recovery: true, State: config.DevicePending, TemporaryAddress: "10.0.0.2", TemporaryPublicKey: temporaryTestKey,
	}}}
	// Both a reboot and a syncconf can leave wg0 without the temporary peer.
	for _, event := range []string{"reboot", "wireguard sync"} {
		calls = nil

		if err := expireAccessDevices(t.Context(), config.Cluster{}, r, config.NodeSpec{WGAddress: "10.0.0.1"}); err != nil {
			t.Fatal(err)
		}

		if len(calls) != 10 || !strings.HasPrefix(calls[9], "wg set wg0 peer ") {
			t.Fatalf("%s did not restore guarded peer: %v", event, calls)
		}
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
