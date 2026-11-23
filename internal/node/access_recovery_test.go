package node

import (
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
)

func TestRecoveryIssuanceRetryAndBootstrapCannotReplace(t *testing.T) {
	r := config.AccessRegistry{Devices: []config.AccessDevice{{
		Recovery: true, OperatorID: "owner", RequestDigest: "digest",
		TemporaryPublicKey: "temporary", State: config.DevicePending,
	}}}
	if i, err := recoveryIssuanceMatch(r, "owner", "digest", "temporary", true); err != nil || i != 0 {
		t.Fatalf("bootstrap retry: index %d, %v", i, err)
	}

	for _, tc := range []struct {
		operator, digest, key string
		bootstrap             bool
	}{
		{"owner", "different", "temporary", true},
		{"owner", "digest", "different", true},
		{"other", "digest", "temporary", false},
	} {
		if _, err := recoveryIssuanceMatch(r, tc.operator, tc.digest, tc.key, tc.bootstrap); err == nil {
			t.Fatalf("unexpected issuance: %+v", tc)
		}
	}

	if i, err := recoveryIssuanceMatch(r, "owner", "different", "new", false); err != nil || i != -1 {
		t.Fatalf("owner replacement: index %d, %v", i, err)
	}

	r.Devices[0].WGPublicKey = "winning-key"
	if _, err := recoveryIssuanceMatch(r, "owner", "digest", "temporary", false); err == nil {
		t.Fatal("issued code replayed after redemption began")
	}
}

func TestRecoveryReplacementRevokesOnlyUnusedCodes(t *testing.T) {
	r := config.AccessRegistry{Devices: []config.AccessDevice{
		{Recovery: true, State: config.DevicePending},
		{Recovery: true, State: config.DevicePending, WGPublicKey: "pinned"},
		{Recovery: true, State: config.DeviceActive},
		{State: config.DevicePending},
	}}
	revokeUnusedRecovery(&r)

	if r.Devices[0].State != config.DeviceRevoked || !r.Devices[0].Cleanup {
		t.Fatal("unused recovery was not revoked and scheduled for cleanup")
	}

	if r.Devices[1].State != config.DevicePending || r.Devices[2].State != config.DeviceActive || r.Devices[3].State != config.DevicePending {
		t.Fatal("replacement changed used recovery or unrelated device")
	}
}
