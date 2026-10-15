package doctor

import (
	"time"
)

// MasterFacts is the master backup's state (Phase 5), from last.json, the
// local bundles and the fence file.
type MasterFacts struct {
	// Known is false on a node without the master backup's directory (a
	// host before master-backup-v1).
	Known      bool
	Bucket     bool
	Fenced     bool
	FencedSeen string
	// PendingSince is when a change was first left unbundled or unuploaded.
	PendingSince string
	LastFailure  string
	// NewestLocal is when the newest local bundle was made.
	NewestLocal    string
	LastVerifiedAt string
	LastExportedAt string
	// Dirty is the dirty flag (a change the job hasn't picked up yet).
	Dirty bool
}

// Thresholds of the master backup checks.
const (
	MasterPendingWarn = time.Hour
	MasterPendingFail = 24 * time.Hour
	MasterVerifyWarn  = 48 * time.Hour
)

// MasterChecks are the master backup's checks, in report order.
func MasterChecks() []Check {
	return []Check{
		{ID: "master.fenced", Category: CatBackups, AtInit: true, Run: masterFenced},
		{ID: "master.backup", Category: CatBackups, Run: masterBackup},
		{ID: "master.verified", Category: CatBackups, Run: masterVerified},
		{ID: "master.offsite", Category: CatBackups, Run: masterOffsite},
	}
}

func parseTime(ts string) (time.Time, bool) {
	t, err := time.Parse(time.RFC3339, ts)
	return t, err == nil
}

func masterFenced(f Facts) []Result {
	if !f.Master.Known {
		return one(Skip, "the master backup isn't set up yet", "eru apply")
	}
	if f.Master.Fenced {
		return one(Fail, "a newer master took over this server's bucket (fenced "+f.Master.FencedSeen+"): its timers and poolers are stopped and archiving refuses",
			"Destroy this server: it was replaced by eru server init --restore")
	}
	return one(Pass, "not fenced", "")
}

func masterBackup(f Facts) []Result {
	if !f.Master.Known {
		return one(Skip, "the master backup isn't set up yet", "eru apply")
	}
	since, pending := parseTime(f.Master.PendingSince)
	if !pending {
		if f.Master.NewestLocal == "" {
			return one(Warn, "no master bundle yet", "eru server backup now")
		}
		return one(Pass, "the newest bundle is from "+f.Master.NewestLocal, "")
	}
	age := f.Now.Sub(since)
	detail := "a change from " + f.Master.PendingSince + " isn't in a bundle that reached its destination"
	if f.Master.LastFailure != "" {
		detail += ": " + f.Master.LastFailure
	}
	switch {
	case age >= MasterPendingFail:
		return one(Fail, detail, "eru server backup now")
	case age >= MasterPendingWarn:
		return one(Warn, detail, "eru server backup now")
	}
	return one(Pass, "a bundle is being made", "")
}

func masterVerified(f Facts) []Result {
	if !f.Master.Known || !f.Master.Bucket {
		return one(Skip, "no bucket", "")
	}
	at, ok := parseTime(f.Master.LastVerifiedAt)
	if !ok {
		if f.Master.NewestLocal == "" {
			return one(Skip, "no bundle yet", "")
		}
		return one(Warn, "the bucket's newest bundle was never confirmed", "eru server backup verify")
	}
	if f.Now.Sub(at) > MasterVerifyWarn {
		return one(Warn, "the bucket's newest bundle was last confirmed "+f.Master.LastVerifiedAt, "eru server backup verify")
	}
	return one(Pass, "the bucket's newest bundle is this server's (confirmed "+f.Master.LastVerifiedAt+")", "")
}

func masterOffsite(f Facts) []Result {
	if !f.Master.Known {
		return one(Skip, "the master backup isn't set up yet", "eru apply")
	}
	if f.Master.Bucket {
		return one(Pass, "bundles go to the bucket", "")
	}
	newest, haveBundle := parseTime(f.Master.NewestLocal)
	if !haveBundle {
		return one(Skip, "no bundle yet", "")
	}
	exported, ok := parseTime(f.Master.LastExportedAt)
	if !ok || exported.Before(newest) {
		return one(Warn, "no bucket, and the newest master bundle ("+f.Master.NewestLocal+") was never copied off the server", "eru server backup export")
	}
	return one(Pass, "the newest bundle was exported "+f.Master.LastExportedAt, "")
}
