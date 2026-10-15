package tune

import (
	"strconv"
	"testing"

	"github.com/mbvlabs/eru/internal/config"
)

// The pools leave config.ReservedConnections free, so both reserved sets
// together must be exactly that many, or full pools would hit "remaining
// connection slots are reserved" before they reach their size.
func TestReservedSlotsMatchThePools(t *testing.T) {
	res := Calculate(Input{RAMMiB: 1024, VCPU: 2, Clusters: 1, Profile: ProfileMixed, MaxConnections: 50, PGMajor: 18})
	sum := 0
	for _, s := range res.Settings {
		if s.Name == "superuser_reserved_connections" || s.Name == "reserved_connections" {
			n, err := strconv.Atoi(s.Value)
			if err != nil {
				t.Fatalf("%s = %q", s.Name, s.Value)
			}
			sum += n
		}
	}
	if sum != config.ReservedConnections {
		t.Errorf("superuser_reserved_connections + reserved_connections = %d, want config.ReservedConnections %d", sum, config.ReservedConnections)
	}
}
