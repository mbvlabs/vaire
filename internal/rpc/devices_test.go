package rpc

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDeviceWireContract(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  string
	}{
		{DeviceAddParams{User: "alice", Label: "laptop"}, `{"user":"alice","label":"laptop"}`},
		{DeviceAddParams{User: "alice", Label: "laptop", Fresh: true}, `{"user":"alice","label":"laptop","fresh":true}`},
		{DeviceListParams{User: "alice"}, `{"user":"alice"}`},
		{DeviceRevokeParams{User: "alice", Label: "laptop", ID: "dev-1"}, `{"user":"alice","label":"laptop","id":"dev-1"}`},
		{UserRevokeParams{User: "alice"}, `{"user":"alice"}`},
	} {
		got, err := json.Marshal(tc.value)
		if err != nil || string(got) != tc.want {
			t.Fatalf("%T: %s, %v; want %s", tc.value, got, err, tc.want)
		}
	}

	raw, err := json.Marshal(Device{ID: "dev-1"})
	if err != nil || strings.Contains(string(raw), "setup_code") || strings.Contains(string(raw), "expires_at") {
		t.Fatalf("optional fields not omitted: %s, %v", raw, err)
	}

	raw, err = json.Marshal(Device{SetupCode: "secret", ExpiresAt: "2026-11-23T00:00:00Z"})
	if err != nil || !strings.Contains(string(raw), `"setup_code":"secret"`) || !strings.Contains(string(raw), `"expires_at":`) {
		t.Fatalf("issue fields missing: %s, %v", raw, err)
	}
}
