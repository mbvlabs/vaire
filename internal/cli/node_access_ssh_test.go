package cli

import (
	"slices"
	"testing"
)

func TestAccessSSHArgsOnlyAcceptsFixedCommands(t *testing.T) {
	for _, tc := range []struct {
		original string
		want     []string
	}{
		{"/usr/local/bin/vaire node version --output json", []string{"/usr/local/bin/vaire", "node", "version", "--output", "json"}},
		{"sudo -n /usr/local/bin/vaire node rpc", []string{"/usr/bin/sudo", "-n", "/usr/local/bin/vaire", "node", "rpc"}},
		{"", nil},
		{"sudo -n /usr/local/bin/vaire node rpc --help", nil},
		{"/usr/local/bin/vaire node version --output json\ntrue", nil},
		{"sh", nil},
	} {
		if got := accessSSHArgs(tc.original); !slices.Equal(got, tc.want) {
			t.Errorf("args for %q = %q, want %q", tc.original, got, tc.want)
		}
	}
}
