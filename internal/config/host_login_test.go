package config

import "testing"

func TestHostLogin(t *testing.T) {
	for input, want := range map[string]string{
		"vdev-1": "vshl-1", "vdev-abc_1": "vshl-abc_1",
		"root": "", "vshl-1": "", "vdev-a\n": "", "vdev-": "",
	} {
		if got := HostLogin(input); got != want {
			t.Errorf("HostLogin(%q) = %q, want %q", input, got, want)
		}
	}
}
