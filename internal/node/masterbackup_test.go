package node

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/masterbackup"
	"github.com/mbvlabs/vaire/internal/promfile"
)

func TestPruneBundles(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	at := func(days int, epoch int64) string {
		return masterbackup.Name(now.Add(-time.Duration(days)*24*time.Hour), epoch, "abc")
	}
	list := []backup.MasterBundle{
		{Name: at(0, 2), Complete: true},
		{Name: at(1, 2), Complete: true},
		{Name: at(100, 1), Complete: true},
		{Name: at(101, 1), Complete: true}, // same ISO week as day 100 or the next; one of them goes
		{Name: at(500, 1), Complete: true}, // past 52 weeks
		{Name: at(0, 2) + "x", Complete: false},
		{Name: at(3, 2), Complete: false}, // an incomplete upload older than a day
	}
	list[5].Name = masterbackup.Name(now.Add(-time.Hour), 2, "fff") // a fresh incomplete upload stays
	var deleted []string
	n, err := pruneBundles(context.Background(), list, now, config.MasterBackup{RetentionDays: 30, RetentionWeeks: 52, LocalKeep: 10},
		func(name string) error { deleted = append(deleted, name); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if n != len(deleted) {
		t.Fatalf("n %d, deleted %v", n, deleted)
	}
	for _, keep := range []string{at(0, 2), at(1, 2), list[5].Name} {
		if slices.Contains(deleted, keep) {
			t.Errorf("deleted %s", keep)
		}
	}
	for _, gone := range []string{at(500, 1), at(3, 2)} {
		if !slices.Contains(deleted, gone) {
			t.Errorf("kept %s", gone)
		}
	}
}

func TestPruneKeepsTheNewestThreeWhateverTheirAge(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	var list []backup.MasterBundle
	for d := 0; d < 3; d++ {
		list = append(list, backup.MasterBundle{Name: masterbackup.Name(now.Add(-time.Duration(800+d)*24*time.Hour), 1, "abc"), Complete: true})
	}
	n, err := pruneBundles(context.Background(), list, now, config.MasterBackup{RetentionDays: 30, RetentionWeeks: 52},
		func(string) error { return nil })
	if err != nil || n != 0 {
		t.Fatalf("pruned %d of three old bundles (%v)", n, err)
	}
}

// A run builds a new bundle when the content changed, there is none, the last
// one is gone, or the epoch moved (so a bundle's name carries its epoch).
func TestNeedsBuild(t *testing.T) {
	base := MasterLast{Name: "m-prod-20261015T120000Z-e1-abc.tar.age", ContentHash: "h1", Epoch: 1}
	if needsBuild(base, "h1", 1, true) {
		t.Error("unchanged, present: should not build")
	}
	for _, tc := range []struct {
		name string
		last MasterLast
		hash string
		ep   int64
		ex   bool
	}{
		{"content changed", base, "h2", 1, true},
		{"none yet", MasterLast{}, "h1", 1, true},
		{"gone from the node", base, "h1", 1, false},
		{"new epoch", base, "h1", 2, true},
	} {
		if !needsBuild(tc.last, tc.hash, tc.ep, tc.ex) {
			t.Errorf("%s: should build", tc.name)
		}
	}
}

func TestMasterFamilies(t *testing.T) {
	cfg := config.DefaultCluster()
	last := MasterLast{CreatedAt: "2026-10-15T10:00:00Z", UploadedAt: "2026-10-15T10:00:05Z", Size: 4096, LastVerifiedAt: "2026-10-15T10:00:05Z"}
	out := string(promfile.Render(masterFamilies(cfg, last, time.Now())))
	for _, want := range []string{
		"vaire_master_backup_last_success_timestamp_seconds 1792058400",
		"vaire_master_backup_size_bytes 4096",
		"vaire_master_backup_dirty 0",
		"vaire_master_fenced 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	cfg.Backup.Provider = "r2"
	out = string(promfile.Render(masterFamilies(cfg, last, time.Now())))
	if !strings.Contains(out, "vaire_master_backup_last_success_timestamp_seconds 1792058405") {
		t.Errorf("with a bucket, success is the upload:\n%s", out)
	}
	last.PendingSince = "2026-10-15T10:01:00Z"
	if !strings.Contains(string(promfile.Render(masterFamilies(cfg, last, time.Now()))), "vaire_master_backup_dirty 1") {
		t.Error("a pending change isn't dirty")
	}
}

func TestBucketCheckDue(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour).Format(time.RFC3339)
	stale := now.Add(-verifyEvery - time.Minute).Format(time.RFC3339)
	for _, c := range []struct {
		name   string
		last   MasterLast
		verify bool
		want   bool
	}{
		{"timer, checked an hour ago", MasterLast{Name: "a", UploadedName: "a", LastVerifiedAt: fresh}, false, false},
		{"timer, checked a day ago", MasterLast{Name: "a", UploadedName: "a", LastVerifiedAt: stale}, false, true},
		{"timer, never checked", MasterLast{Name: "a", UploadedName: "a"}, false, true},
		{"now, checked an hour ago", MasterLast{Name: "a", UploadedName: "a", LastVerifiedAt: fresh}, true, true},
		{"not uploaded yet", MasterLast{Name: "b", UploadedName: "a", LastVerifiedAt: fresh}, true, false},
	} {
		if got := bucketCheckDue(c.last, now, c.verify); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}
