package config

import "strings"

// HostLogin derives the separate shell account for a managed device login.
// Invalid device logins never select a host account.
func HostLogin(device string) string {
	if !unixLoginPattern.MatchString(device) {
		return ""
	}

	return "vshl-" + strings.TrimPrefix(device, "vdev-")
}
