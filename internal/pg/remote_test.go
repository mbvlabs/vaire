package pg

import (
	"crypto/tls"
	"testing"
)

func TestTLSConfigModes(t *testing.T) {
	for _, tc := range []struct {
		mode       string
		skipVerify bool
	}{
		{mode: "self-signed", skipVerify: true},
		{mode: "require", skipVerify: true},
		{mode: "acme"},
		{mode: "verify-full"},
		{mode: ""},
		{mode: "unknown"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			cfg := TLSConfig(Params{TLSMode: tc.mode, ServerName: "db.example.test"})
			if cfg.InsecureSkipVerify != tc.skipVerify {
				t.Errorf("certificate verification disabled = %v, want %v", cfg.InsecureSkipVerify, tc.skipVerify)
			}
			if cfg.MinVersion < tls.VersionTLS12 {
				t.Error("the minimum TLS version is below TLS 1.2")
			}
			if !tc.skipVerify && cfg.ServerName != "db.example.test" {
				t.Errorf("server name = %q, want db.example.test", cfg.ServerName)
			}
		})
	}
}
