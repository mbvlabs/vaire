package doctor

import (
	"testing"
	"time"
)

func masterHealthy() Facts {
	f := healthy()
	f.Master = MasterFacts{Known: true, Bucket: true, NewestLocal: now.Add(-time.Hour).Format(time.RFC3339),
		LastVerifiedAt: now.Add(-2 * time.Hour).Format(time.RFC3339)}
	return f
}

func TestMasterChecksHealthy(t *testing.T) {
	for _, r := range Run(MasterChecks(), masterHealthy(), "") {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Detail)
		}
	}
}

func TestMasterBackupPending(t *testing.T) {
	f := masterHealthy()
	for _, c := range []struct {
		age  time.Duration
		want Status
	}{{10 * time.Minute, Pass}, {2 * time.Hour, Warn}, {25 * time.Hour, Fail}} {
		f.Master.PendingSince = now.Add(-c.age).Format(time.RFC3339)
		f.Master.LastFailure = "the bucket refused the keys"
		if got := byID(masterBackup(f), "", "").Status; got != c.want {
			t.Errorf("pending %s: %s, want %s", c.age, got, c.want)
		}
	}
}

func TestMasterFencedAndVerified(t *testing.T) {
	f := masterHealthy()
	f.Master.Fenced, f.Master.FencedSeen = true, "2026-10-14T11:00:00Z"
	if masterFenced(f)[0].Status != Fail {
		t.Error("a fenced node passes")
	}
	f = masterHealthy()
	f.Master.LastVerifiedAt = now.Add(-72 * time.Hour).Format(time.RFC3339)
	if masterVerified(f)[0].Status != Warn {
		t.Error("an old confirmation passes")
	}
	f.Master.Bucket = false
	if masterVerified(f)[0].Status != Skip {
		t.Error("verify without a bucket doesn't skip")
	}
}

func TestMasterOffsite(t *testing.T) {
	f := masterHealthy()
	f.Master.Bucket = false
	if masterOffsite(f)[0].Status != Warn {
		t.Error("no bucket, never exported: not a warning")
	}
	f.Master.LastExportedAt = now.Format(time.RFC3339)
	if masterOffsite(f)[0].Status != Pass {
		t.Error("exported after the newest bundle: not a pass")
	}
	f.Master.LastExportedAt = now.Add(-2 * time.Hour).Format(time.RFC3339)
	if masterOffsite(f)[0].Status != Warn {
		t.Error("exported before the newest bundle: not a warning")
	}
}

func TestAwaitingRecoverySkipsPostgresChecks(t *testing.T) {
	f := healthy()
	f.Clusters[0].AwaitingRecovery = true
	f.Clusters[0].State = "missing"
	results := Run(NodeChecks(), f, "")
	for _, id := range []string{"pg.running", "backup.archiving", "pg.checksums"} {
		if r := byID(results, id, "app"); r.Status != Skip {
			t.Errorf("%s for a database awaiting recovery: %s %s", id, r.Status, r.Detail)
		}
	}
}
