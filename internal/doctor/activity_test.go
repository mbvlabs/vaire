package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
)

// activityHealthy is the activity store of a server that has recorded events
// and stays within its budget.
func activityHealthy() Facts {
	f := healthy()
	f.ActivityHealthChecked = true
	f.ActivityHealthKnown = true
	f.ActivityHealth = activity.Health{
		Dir:             "/var/log/vaire/activity",
		Exists:          true,
		IsDir:           true,
		DirMode:         "0700",
		Readable:        true,
		Writable:        true,
		SegmentMode:     "0600",
		OwnerOnly:       true,
		MaxBytes:        64 << 20,
		UsedBytes:       1 << 20,
		SegmentBytes:    8 << 20,
		Segments:        2,
		RetentionS:      int64((30 * 24 * time.Hour).Seconds()),
		FirstRetainedAt: now.Add(-20 * 24 * time.Hour).Format(time.RFC3339),
		LastRetainedAt:  now.Add(-time.Hour).Format(time.RFC3339),
		LastWriteAt:     now.Add(-time.Hour).Format(time.RFC3339),
	}
	return f
}

func TestActivityChecksHealthy(t *testing.T) {
	for _, r := range Run(ActivityChecks(), activityHealthy(), "") {
		if r.Status != Pass {
			t.Errorf("%s: %s %s", r.ID, r.Status, r.Detail)
		}
	}
}

func TestActivityMissingStoreWarnsWithoutFailing(t *testing.T) {
	f := activityHealthy()
	f.ActivityHealth = activity.Health{Exists: false}
	results := Run(ActivityChecks(), f, "")
	if got := byID(results, "activity.storage", ""); got.Status != Warn || got.Detail != "no activity history recorded yet" {
		t.Errorf("storage: %s %q, want a warning that no history was recorded", got.Status, got.Detail)
	}
	if got := byID(results, "activity.permissions", ""); got.Status != Skip {
		t.Errorf("permissions on a missing store: %s, want skip", got.Status)
	}
	if got := byID(results, "activity.coverage", ""); got.Status != Skip {
		t.Errorf("coverage on a missing store: %s, want skip", got.Status)
	}
	if failed := Failed(results); len(failed) != 0 {
		t.Errorf("a missing store failed: %+v", failed)
	}
}

// A pure fixture that never gathered facts leaves both flags false: the
// existing checks skip rather than claiming the history is healthy or broken.
func TestActivityUnmeasuredHealthSkips(t *testing.T) {
	f := activityHealthy()
	f.ActivityHealthChecked, f.ActivityHealthKnown = false, false
	for _, r := range Run(ActivityChecks(), f, "") {
		if r.Status != Skip {
			t.Errorf("%s without a measurement: %s %s", r.ID, r.Status, r.Detail)
		}
	}
}

// gatherFacts marks the read as attempted before calling Store.Health. A read
// that then fails is missing evidence and must be visible, while permissions
// and coverage cannot claim anything about the unread store.
func TestActivityCheckedUnknownHealthWarnsStorage(t *testing.T) {
	f := activityHealthy()
	f.ActivityHealthKnown = false
	results := Run(ActivityChecks(), f, "")
	if got := byID(results, "activity.storage", ""); got.Status != Warn || got.Detail != "activity history health could not be read" {
		t.Errorf("storage: %s %q, want a fixed read-failure warning", got.Status, got.Detail)
	}
	for _, id := range []string{"activity.permissions", "activity.coverage"} {
		if got := byID(results, id, ""); got.Status != Skip {
			t.Errorf("%s with unknown health: %s, want skip", id, got.Status)
		}
	}
	if failed := Failed(results); len(failed) != 0 {
		t.Errorf("an unknown read failed: %+v", failed)
	}
	if got := byID(results, "activity.storage", ""); strings.Contains(got.Detail, "/") || got.Fix != "" {
		t.Errorf("read failure echoes a path or asks for a repair: %+v", got)
	}
}

func TestActivityStorageBudgetAndLoss(t *testing.T) {
	for name, mutate := range map[string]func(*activity.Health){
		"over budget":       func(h *activity.Health) { h.OverBudget = true },
		"loss flag":         func(h *activity.Health) { h.LossDetected = true },
		"malformed segment": func(h *activity.Health) { h.MalformedSegments = 1 },
		"malformed event":   func(h *activity.Health) { h.MalformedEvents = 1 },
		"dropped append":    func(h *activity.Health) { h.DroppedEvents = 1 },
		"sequence gap":      func(h *activity.Health) { h.SequenceGaps = 1 },
	} {
		f := activityHealthy()
		mutate(&f.ActivityHealth)
		if got := activityStorage(f)[0].Status; got != Warn {
			t.Errorf("%s: %s, want warn (never fail for capacity or loss)", name, got)
		}
	}

	// Retention pruning old evidence is expected: an old first retained event
	// with no loss counters still passes.
	f := activityHealthy()
	f.ActivityHealth.FirstRetainedAt = now.Add(-365 * 24 * time.Hour).Format(time.RFC3339)
	f.ActivityHealth.RetentionS = int64((24 * time.Hour).Seconds())
	if got := activityStorage(f)[0].Status; got != Pass {
		t.Errorf("old retained evidence: %s, want pass", got)
	}

	f = activityHealthy()
	f.ActivityHealth.Readable = false
	f.ActivityHealth.Dir = "/secret/activity"
	r := activityStorage(f)[0]
	if r.Status != Fail {
		t.Errorf("unreadable store: %s, want fail", r.Status)
	}
	if r.Fix != "" {
		t.Errorf("unreadable store asks for a repair: %q", r.Fix)
	}
	if strings.Contains(r.Detail, f.ActivityHealth.Dir) {
		t.Errorf("detail echoes the store path: %q", r.Detail)
	}
}

func TestActivityPermissions(t *testing.T) {
	for name, mutate := range map[string]func(*activity.Health){
		"not a directory":      func(h *activity.Health) { h.IsDir = false },
		"unreadable":           func(h *activity.Health) { h.Readable = false },
		"not writable":         func(h *activity.Health) { h.Writable = false },
		"not owner-only":       func(h *activity.Health) { h.OwnerOnly = false },
		"foreign segment":      func(h *activity.Health) { h.OwnerOnly, h.NonOwnerOnlySegments = false, 2 },
		"unreadable and loose": func(h *activity.Health) { h.Readable, h.OwnerOnly = false, false },
	} {
		f := activityHealthy()
		mutate(&f.ActivityHealth)
		f.ActivityHealth.Dir = "/secret/activity"
		r := activityPermissions(f)[0]
		if r.Status != Fail {
			t.Errorf("%s: %s, want fail", name, r.Status)
		}
		if r.Fix != "" {
			t.Errorf("%s asks for a repair: %q", name, r.Fix)
		}
		if strings.Contains(r.Detail, f.ActivityHealth.Dir) {
			t.Errorf("%s: detail echoes the store path: %q", name, r.Detail)
		}
	}

	f := activityHealthy()
	f.ActivityHealth.Exists = false
	if got := activityPermissions(f)[0].Status; got != Skip {
		t.Errorf("missing store: %s, want skip", got)
	}
}

func TestActivityCoverage(t *testing.T) {
	f := activityHealthy()
	f.ActivityHealth.FirstRetainedAt, f.ActivityHealth.LastRetainedAt = "", ""
	if r := activityCoverage(f)[0]; r.Status != Warn || !strings.Contains(r.Detail, "retained") {
		t.Errorf("empty retained window: %s %q", r.Status, r.Detail)
	}

	f = activityHealthy()
	f.ActivityHealth.LastRetainedAt = ""
	if r := activityCoverage(f)[0]; r.Status != Warn || !strings.Contains(r.Detail, "incomplete") {
		t.Errorf("incomplete retained window: %s %q", r.Status, r.Detail)
	}

	f = activityHealthy()
	f.ActivityHealth.FirstRetainedAt = "yesterday"
	if got := activityCoverage(f)[0].Status; got != Warn {
		t.Errorf("unparseable retained window: %s, want warn", got)
	}

	f = activityHealthy()
	f.ActivityHealth.FirstRetainedAt = now.Add(time.Hour).Format(time.RFC3339)
	f.ActivityHealth.LastRetainedAt = now.Format(time.RFC3339)
	if got := activityCoverage(f)[0].Status; got != Warn {
		t.Errorf("inverted retained window: %s, want warn", got)
	}

	f = activityHealthy()
	f.ActivityHealth.Exists = false
	if got := activityCoverage(f)[0].Status; got != Skip {
		t.Errorf("missing store: %s, want skip", got)
	}
}

func TestActivityChecksAreRegistered(t *testing.T) {
	results := Run(NodeChecks(), activityHealthy(), "")
	for _, id := range []string{"activity.storage", "activity.permissions", "activity.coverage"} {
		if got := byID(results, id, ""); got.ID == "" || got.Status != Pass {
			t.Errorf("%s: %+v", id, got)
		}
	}
	security := Filter(ActivityChecks(), CatSecurity)
	if len(security) != 1 || security[0].ID != "activity.permissions" {
		t.Errorf("security filter: %+v", security)
	}
}

// A store that does not exist yet is the normal state before the first
// activity write. The storage check warns; it never demands a repair command.
func TestActivityMissingStoreHasNoRepair(t *testing.T) {
	f := activityHealthy()
	f.ActivityHealth = activity.Health{Exists: false}
	if fix := activityStorage(f)[0].Fix; fix != "" {
		t.Errorf("missing store asks for a repair: %q", fix)
	}
}
