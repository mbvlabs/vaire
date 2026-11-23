package config

import (
	"fmt"
	"net/netip"
)

// NextAccessAddress finds a usable peer address without reusing a reserved
// address. The caller holds the node lock and passes every node, legacy
// operator, and managed device address, including revoked device records.
func NextAccessAddress(subnet netip.Prefix, reserved []netip.Addr) (netip.Addr, error) {
	if !subnet.IsValid() || !subnet.Addr().Is4() || subnet.Bits() > 30 {
		return netip.Addr{}, fmt.Errorf("device enrollment needs an IPv4 subnet with usable host addresses")
	}

	used := make(map[netip.Addr]bool, len(reserved))
	for _, address := range reserved {
		if subnet.Contains(address) {
			used[address] = true
		}
	}

	for address := subnet.Masked().Addr().Next(); subnet.Contains(address.Next()); address = address.Next() {
		if !used[address] {
			return address, nil
		}
	}

	return netip.Addr{}, fmt.Errorf("no WireGuard peer addresses are available in %s", subnet.Masked())
}
