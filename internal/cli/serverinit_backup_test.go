package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/node"
)

// With a bucket, init warns when the first master bundle didn't reach it;
// without one, only when the local bundle itself failed. Success is silent.
func TestFirstBundleWarning(t *testing.T) {
	ok := node.MasterBackupResult{Name: "m-prod-20261016T120000Z-e1-abc.tar.age", Uploaded: true, Verified: true}
	if w := firstBundleWarning(true, "vaire-backups-test", ok, nil); w != nil {
		t.Fatalf("uploaded: %+v", w)
	}
	if w := firstBundleWarning(false, "", ok, nil); w != nil {
		t.Fatalf("no bucket, local only: %+v", w)
	}

	w := firstBundleWarning(true, "vaire-backups-test", ok, errors.New("upload timed out"))
	if w == nil || w.Code != "master_backup_failed" || !strings.Contains(w.Message, "vaire-backups-test") || !strings.Contains(w.Message, "backup now") {
		t.Fatalf("bucket failure: %+v", w)
	}
	w = firstBundleWarning(false, "", node.MasterBackupResult{}, errors.New("no recipient"))
	if w == nil || w.Code != "master_backup_failed" || !strings.Contains(w.Message, "run vaire server backup now") {
		t.Fatalf("local failure: %+v", w)
	}
	w = firstBundleWarning(true, "vaire-backups-test", node.MasterBackupResult{Name: "m-1"}, nil)
	if w == nil || !strings.Contains(w.Message, "isn't confirmed in the bucket") {
		t.Fatalf("unconfirmed upload: %+v", w)
	}
}
