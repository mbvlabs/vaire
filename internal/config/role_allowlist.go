package config

import (
	"net/netip"
	"slices"
)

// RoleAllowlist replaces the shared list. Missing roles allow no public IPs.
type RoleAllowlist struct {
	App     []string `toml:"app,omitempty"`
	Migrate []string `toml:"migrate,omitempty"`
	RO      []string `toml:"ro,omitempty"`
}

const (
	AllowlistApp     = "app"
	AllowlistMigrate = "migrate"
	AllowlistRO      = "ro"
)

// ClientsForRole returns the public sources for a supported role.
func (n Network) ClientsForRole(role string) []string {
	switch role {
	case AllowlistApp, AllowlistMigrate, AllowlistRO:
	default:
		return nil
	}

	if n.RoleAllowlist == nil {
		return n.ClientAllowlist
	}

	switch role {
	case AllowlistApp:
		return n.RoleAllowlist.App
	case AllowlistMigrate:
		return n.RoleAllowlist.Migrate
	default:
		return n.RoleAllowlist.RO
	}
}

// PoolerClients is the coarse firewall policy. PgBouncer enforces the
// login-specific policy in per-role mode after this network check.
func (n Network) PoolerClients(session bool) []string {
	if n.RoleAllowlist == nil {
		return n.ClientAllowlist
	}

	clients := slices.Concat(n.RoleAllowlist.App, n.RoleAllowlist.RO)
	if session {
		clients = append(clients, n.RoleAllowlist.Migrate...)
	}

	return clients
}

func (n Network) validateRoleAllowlist(add func(string, ...any)) {
	if n.RoleAllowlist == nil {
		return
	}

	if len(n.ClientAllowlist) > 0 {
		add("network.client_allowlist must be empty when network.role_allowlist is set")
	}

	for _, role := range []string{AllowlistApp, AllowlistMigrate, AllowlistRO} {
		for _, cidr := range n.ClientsForRole(role) {
			p, err := netip.ParsePrefix(cidr)
			if err != nil || p.Addr().Is4In6() {
				add("network.role_allowlist.%s entry %q must be an IPv4 or IPv6 CIDR", role, cidr)
			}
		}
	}
}
