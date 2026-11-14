package node

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestMaterialisationConfigRendersDestinationAndFollowerFloors(t *testing.T) {
	cfg := config.DefaultCluster()
	source := config.ClusterSpec{Name: "app", Size: "small", Port: 5432}
	target := config.ClusterSpec{Name: "copy", Size: "medium", Port: 5433}
	cfg.Clusters = []config.ClusterSpec{source, target}
	host := hostinfo.Info{RAMMiB: 4096, VCPU: 4, DataDisk: hostinfo.Disk{SizeGiB: 50}}
	small, err := materialisationConfig(cfg, source, host)
	if err != nil {
		t.Fatal(err)
	}
	medium, err := materialisationConfig(cfg, target, host)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small.Tuning, "max_connections = 30") || !strings.Contains(medium.Tuning, "max_connections = 50") ||
		medium.Follower != nil || medium.Overrides != nil {
		t.Fatalf("destination layers: small=%s medium=%+v", small.Tuning, medium)
	}
	target.Follows = source.Name
	cfg.Clusters[1] = target
	follower, err := materialisationConfig(cfg, target, host)
	if err != nil || follower.Follower == nil {
		t.Fatalf("follower layers: %+v %v", follower, err)
	}
	if !strings.Contains(*follower.Follower, "max_connections = 50") {
		t.Fatalf("follower floors: %s", *follower.Follower)
	}
	for _, forbidden := range []string{"primary_conninfo", "primary_slot_name", "restore_command", "passfile", "standby.signal"} {
		if strings.Contains(*follower.Follower, forbidden) {
			t.Fatalf("unexpected follower transport %q in %s", forbidden, *follower.Follower)
		}
	}
}
