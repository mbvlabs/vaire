package config

import (
	"net/netip"
	"slices"
)

// ExposureIncreases lists surfaces needing approval for a proposed config
// change. Call it only with validated configs. It does not inspect live
// firewall state or authorize a change.
func ExposureIncreases(old, next Cluster) []string {
	var surfaces []string

	if next.Network.RoleAllowlist == nil && old.Network.RoleAllowlist == nil &&
		prefixesWiden(old.Network.ClientAllowlist, next.Network.ClientAllowlist) {
		surfaces = append(surfaces, "session_pooler", "transaction_pooler")
	}

	if next.Network.RoleAllowlist != nil || old.Network.RoleAllowlist != nil {
		for _, role := range []string{AllowlistApp, AllowlistMigrate, AllowlistRO} {
			if prefixesWiden(old.Network.ClientsForRole(role), next.Network.ClientsForRole(role)) {
				surfaces = append(surfaces, "database_role_"+role)
			}
		}

		// Shared mode also admits non-application logins through its
		// public network gate; per-role mode does not.
		if next.Network.RoleAllowlist == nil && len(next.Network.ClientAllowlist) > 0 {
			surfaces = append(surfaces, "database_other_roles")
		}
	}

	if next.Transport.Subnet != "" && prefixesWiden(
		subnetPrefixes(old.Transport.Subnet), []string{next.Transport.Subnet}) {
		surfaces = append(surfaces, "wireguard_subnet")
	}

	if prefixesWiden(metricsClients(old), metricsClients(next)) {
		surfaces = append(surfaces, "metrics")
	}

	return surfaces
}

func subnetPrefixes(subnet string) []string {
	if subnet == "" {
		return nil
	}

	return []string{subnet}
}

func metricsClients(c Cluster) []string {
	if !c.Metrics.Enabled {
		return nil
	}

	if len(c.Metrics.Allowlist) == 0 {
		return []string{"0.0.0.0/0", "::/0"}
	}

	// nginx also admits loopback when it renders a restricted allowlist.
	return append(slices.Clone(c.Metrics.Allowlist), "127.0.0.1/32", "::1/128")
}

func prefixesWiden(old, next []string) bool {
	previous := make([]netip.Prefix, 0, len(old))
	for _, value := range old {
		if p, err := netip.ParsePrefix(value); err == nil {
			previous = append(previous, p.Masked())
		}
	}

	for _, value := range next {
		p, err := netip.ParsePrefix(value)
		if err != nil || !prefixCovered(p.Masked(), previous) {
			return true
		}
	}

	return false
}

// prefixCovered considers the union of the old prefixes, so joining two
// already-covered halves is not treated as an increase.
func prefixCovered(want netip.Prefix, previous []netip.Prefix) bool {
	var hasSubset bool

	for _, p := range previous {
		if !p.Overlaps(want) {
			continue
		}

		if p.Bits() <= want.Bits() {
			return true
		}

		hasSubset = true
	}

	if !hasSubset {
		return false
	}

	bit := want.Bits()
	addr := want.Addr().AsSlice()
	addr[bit/8] |= 1 << (7 - bit%8)
	right, _ := netip.AddrFromSlice(addr)

	return prefixCovered(netip.PrefixFrom(want.Addr(), bit+1), previous) &&
		prefixCovered(netip.PrefixFrom(right, bit+1), previous)
}
