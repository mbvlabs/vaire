package doctor

import (
	"fmt"

	"github.com/mbvlabs/vaire/internal/activity"
)

// ActivityChecks are the node's local activity-history checks. The history is
// bounded, owner-only evidence for operator and agent diagnosis; a missing
// store is normal because the node creates it lazily with the first event.
func ActivityChecks() []Check {
	return []Check{
		{ID: "activity.storage", Category: CatHost, Run: activityStorage},
		{ID: "activity.permissions", Category: CatHost, Security: true, AtInit: true, Run: activityPermissions},
		{ID: "activity.coverage", Category: CatHost, Run: activityCoverage},
	}
}

// activityStorage checks that the history exists, can be read, stays within
// its byte budget and shows no evidence loss. Retention removes old segments by
// design and is not loss; the loss counters cover malformed data, failed
// appends and sequence gaps.
func activityStorage(f Facts) []Result {
	if !f.ActivityHealthChecked {
		return one(Skip, "the activity history was not measured", "")
	}
	if !f.ActivityHealthKnown {
		// The store refused or failed the read: that is missing evidence, not a
		// clean skip. The fixed message keeps any raw error out of the report.
		return one(Warn, "activity history health could not be read", "")
	}
	h := f.ActivityHealth
	switch {
	case !h.Exists:
		// The node's emitter creates the store with the first write. Do not
		// warn about a repair or create it from a read-only check.
		return one(Warn, "no activity history recorded yet", "")
	case !h.IsDir:
		return one(Fail, "the activity history is not a directory", "")
	case !h.Readable:
		return one(Fail, "the activity history is not readable", "")
	}
	if h.OverBudget {
		return one(Warn, activityUsage(h)+": over its byte budget", "")
	}
	if h.LossDetected || h.MalformedSegments > 0 || h.MalformedEvents > 0 || h.DroppedEvents > 0 || h.SequenceGaps > 0 {
		return one(Warn, "the activity history shows lost or unreadable evidence", "")
	}
	return one(Pass, activityUsage(h), "")
}

// activityPermissions checks the owner-only rules the store promises. It fails
// an existing store that other users can read or that its owner cannot write;
// an absent store is left to the storage check.
func activityPermissions(f Facts) []Result {
	if !f.ActivityHealthChecked {
		return one(Skip, "the activity history was not measured", "")
	}
	if !f.ActivityHealthKnown {
		return one(Skip, "activity history health could not be read", "")
	}
	h := f.ActivityHealth
	if !h.Exists {
		return one(Skip, "no activity history recorded yet", "")
	}
	switch {
	case !h.IsDir:
		return one(Fail, "the activity history is not a directory", "")
	case !h.Readable:
		return one(Fail, "the activity history is not readable by its owner", "")
	case !h.Writable:
		return one(Fail, "the activity history is not writable by its owner", "")
	case !h.OwnerOnly:
		if h.NonOwnerOnlySegments > 0 {
			return one(Fail, fmt.Sprintf("the activity history is not owner-only (%d segments)", h.NonOwnerOnlySegments), "")
		}
		return one(Fail, "the activity history is not owner-only", "")
	}
	return one(Pass, "the activity history is owner-only and writable", "")
}

// activityCoverage checks that the store reports a plausible retained window.
// It deliberately has no age threshold: retention pruning old evidence is
// expected, while loss and budget overruns are the storage check's concern.
func activityCoverage(f Facts) []Result {
	if !f.ActivityHealthChecked {
		return one(Skip, "the activity history was not measured", "")
	}
	if !f.ActivityHealthKnown {
		return one(Skip, "activity history health could not be read", "")
	}
	h := f.ActivityHealth
	switch {
	case !h.Exists:
		return one(Skip, "no activity history recorded yet", "")
	case !h.IsDir || !h.Readable:
		return one(Skip, "the activity history is not readable", "")
	case h.FirstRetainedAt == "" && h.LastRetainedAt == "":
		return one(Warn, "no activity events are retained yet", "")
	case h.FirstRetainedAt == "" || h.LastRetainedAt == "":
		return one(Warn, "the activity history's retained window is incomplete", "")
	}
	first, firstOK := parseTime(h.FirstRetainedAt)
	last, lastOK := parseTime(h.LastRetainedAt)
	switch {
	case (h.FirstRetainedAt != "" && !firstOK) || (h.LastRetainedAt != "" && !lastOK):
		return one(Warn, "the activity history's retained window does not parse", "")
	case first.After(last):
		return one(Warn, "the activity history's retained window is inconsistent", "")
	}
	return one(Pass, "activity history covers "+h.FirstRetainedAt+" to "+h.LastRetainedAt, "")
}

// activityUsage is a path-free description of the store's bounded size.
func activityUsage(h activity.Health) string {
	if h.MaxBytes > 0 {
		return fmt.Sprintf("%d activity segments use %d of %d bytes", h.Segments, h.UsedBytes, h.MaxBytes)
	}
	return fmt.Sprintf("%d activity segments", h.Segments)
}
