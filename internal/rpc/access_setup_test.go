package rpc

import (
	"testing"
	"time"
)

func TestRecoverySetupHasDistinctNonExpiringMode(t *testing.T) {
	for _, tc := range []struct {
		mode   string
		expiry time.Time
		valid  bool
	}{
		{"", time.Time{}, false},
		{"", time.Now(), true},
		{AccessSetupRecovery, time.Time{}, true},
		{AccessSetupRecovery, time.Now(), false},
		{"unknown", time.Now(), false},
	} {
		code, err := (AccessSetup{Version: 1, ID: "dev-1", Mode: tc.mode, ExpiresAt: tc.expiry}).Encode()
		if err != nil {
			t.Fatal(err)
		}

		_, err = DecodeAccessSetup(code)
		if (err == nil) != tc.valid {
			t.Fatalf("mode %q expiry %v: %v", tc.mode, tc.expiry, err)
		}
	}
}
