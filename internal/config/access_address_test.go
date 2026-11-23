package config

import (
	"net/netip"
	"testing"
)

func TestNextAccessAddress(t *testing.T) {
	subnet := netip.MustParsePrefix("10.42.0.2/30")
	node := netip.MustParseAddr("10.42.0.1")
	device := netip.MustParseAddr("10.42.0.2")

	got, err := NextAccessAddress(subnet, []netip.Addr{node})
	if err != nil || got != device {
		t.Fatalf("address = %v, %v; want %v", got, err, device)
	}

	if _, err := NextAccessAddress(subnet, []netip.Addr{node, device}); err == nil {
		t.Fatal("allocated a broadcast or reserved address")
	}

	for _, invalid := range []netip.Prefix{netip.MustParsePrefix("10.42.0.0/31"), netip.MustParsePrefix("fd00::/64"), {}} {
		if _, err := NextAccessAddress(invalid, nil); err == nil {
			t.Errorf("accepted invalid subnet %v", invalid)
		}
	}
}
