package steps

import (
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/hostinfo"
)

func TestAwaitingTargetRetainsHugePageBudgetWithoutExposure(t *testing.T) {
	cfg := followerConfig()
	cfg.Backup.Provider, cfg.Backup.Bucket = "r2", "b"
	target := config.ClusterSpec{Name: "unfinished", Node: "db1", Port: 5441, Stanza: "unfinished-abcdef", Size: "large"}
	target.Backup.FullSchedule = "*-*-* 03:00:00"
	host := hostinfo.Info{VCPU: 8, RAMMiB: 16384, DataDisk: hostinfo.Disk{SizeGiB: 100}}
	cfg.Clusters = append(cfg.Clusters, target)
	before := hugePagesFor(cfg, host)
	found := false
	for _, db := range poolerDatabases(cfg, backup.Records{}, false) {
		found = found || db.Name == target.Name
	}
	if !found {
		t.Fatal("positive-control pooler target missing")
	}
	found = false
	for name := range timerFiles(cfg) {
		found = found || strings.Contains(name, target.Name)
	}
	if !found {
		t.Fatal("positive-control timer missing")
	}
	cfg.Clusters = cfg.Clusters[:len(cfg.Clusters)-1]
	cfg.Awaiting = append(cfg.Awaiting, target)
	targetPages := tuningFor(cfg, target, host).HugePages
	if targetPages == 0 {
		t.Fatal("target fixture does not require huge pages")
	}
	want := targetPages
	for _, c := range cfg.Clusters {
		want += tuningFor(cfg, c, host).HugePages
	}
	if got := hugePagesFor(cfg, host); got != want || got < targetPages || before == 0 {
		t.Fatalf("huge pages: got %d want %d target %d before %d", got, want, targetPages, before)
	}
	for _, session := range []bool{false, true} {
		for _, db := range poolerDatabases(cfg, backup.Records{}, session) {
			if db.Name == target.Name {
				t.Fatal("awaiting target routed")
			}
		}
	}
	for name := range timerFiles(cfg) {
		if strings.Contains(name, target.Name) {
			t.Fatalf("awaiting target scheduled: %s", name)
		}
	}
}
