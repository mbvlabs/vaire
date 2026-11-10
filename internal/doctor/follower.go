package doctor

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/vaire/internal/health"
)

// Follower thresholds (Phase 6, D13).
const (
	FollowerLagWarnBytes = 1 << 30
	FollowerLagFailBytes = 16 << 30
	FollowerLagWarn      = 5 * time.Minute
	FollowerLagFail      = time.Hour
)

// FollowerChecks are the follower checks (Phase 6): whether each follower
// streams, how far behind it is, its slot on the source, and slots no
// follower owns.
func FollowerChecks() []Check {
	return []Check{
		{ID: "follower.streaming", Category: CatPostgres, Run: followerStreaming},
		{ID: "follower.lag", Category: CatPostgres, Run: followerLag},
		{ID: "follower.slot", Category: CatPostgres, Run: followerSlot},
		{ID: "follower.orphan_slots", Category: CatPostgres, Run: followerOrphanSlots},
	}
}

// perFollower runs fn for every follower, with one skip row when there is
// none. A follower awaiting recovery is skipped (pg.running reports it);
// needsRunning checks skip a stopped follower too, while the source-side slot
// check still runs when the source answered. A follower whose needed side
// couldn't be read warns.
func perFollower(f Facts, needsRunning bool, fn func(c ClusterFacts) (Status, string, string)) []Result {
	var out []Result
	for _, c := range f.Clusters {
		if c.Follows == "" {
			continue
		}
		r := Result{DB: c.Name}
		switch {
		case c.AwaitingRecovery:
			r.Status, r.Detail = Skip, "awaiting recovery (vaire db recover "+c.Name+")"
		case needsRunning && c.State != "running":
			r.Status, r.Detail = Skip, "the cluster is "+c.State
		case c.Follower == nil || (needsRunning && !c.Follower.SourceKnown && !c.Follower.SelfKnown):
			r.Status, r.Detail = Warn, "cannot read its replication"
			if c.Follower != nil {
				r.Detail += sideErrors(c.Follower.SourceError, c.Follower.SelfError)
			}
		case !needsRunning && !c.Follower.SourceKnown:
			r.Status, r.Detail = Warn, "cannot read its slot on "+c.Follows+sideErrors(c.Follower.SourceError)
		default:
			r.Status, r.Detail, r.Fix = fn(c)
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return one(Skip, "no followers", "")
	}
	return out
}

// sideErrors joins the errors of the sides a check needed.
func sideErrors(errs ...string) string {
	var kept []string
	for _, err := range errs {
		if err != "" {
			kept = append(kept, err)
		}
	}
	if len(kept) == 0 {
		return ""
	}
	return ": " + strings.Join(kept, "; ")
}

func followerStreaming(f Facts) []Result {
	return perFollower(f, true, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		logs := "vaire logs postgres --db " + c.Name
		// A failure one side proves stands even when the other side cannot
		// be read; an unread side only warns when nothing known fails.
		switch {
		case st.SelfKnown && !st.InRecovery:
			return Fail, "it is out of recovery: no longer a standby of " + c.Follows, "vaire db unfollow " + c.Name + " --confirm " + c.Name + "/unfollow, or drop it and follow " + c.Follows + " again"
		case st.SelfKnown && st.ReplayPaused:
			return Fail, "its replay is paused (a hot-standby floor below " + c.Follows + "'s pauses it)", "vaire apply --db " + c.Name + " --confirm " + c.Name + "/restart; then " + logs
		case st.SourceKnown && !st.Streaming:
			detail := "it is not streaming from " + c.Follows
			if st.SenderState != "" {
				detail += " (walsender " + st.SenderState + ")"
			}
			return Fail, detail, logs
		case !st.SelfKnown:
			return Warn, "cannot read the follower's own state" + sideErrors(st.SelfError), logs
		case !st.SourceKnown:
			return Warn, "cannot read its replication on " + c.Follows + sideErrors(st.SourceError), logs
		}
		return Pass, "streaming from " + c.Follows, ""
	})
}

func followerLag(f Facts) []Result {
	return perFollower(f, true, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		if !st.SourceKnown {
			return Warn, "cannot read its replication on " + c.Follows + sideErrors(st.SourceError), "vaire status --db " + c.Name
		}
		if st.LagBytes == nil && st.LagSeconds == nil {
			return Warn, "its replay lag is unknown", "vaire status --db " + c.Name + "; vaire logs postgres --db " + c.Name
		}
		detail := followerLagDetail(st, c.Follows)
		switch {
		case (st.LagBytes != nil && *st.LagBytes >= FollowerLagFailBytes) || (st.LagSeconds != nil && *st.LagSeconds >= FollowerLagFail.Seconds()):
			return Fail, detail, "vaire status --db " + c.Name + "; a follower this far behind may lose its slot"
		case (st.LagBytes != nil && *st.LagBytes >= FollowerLagWarnBytes) || (st.LagSeconds != nil && *st.LagSeconds >= FollowerLagWarn.Seconds()):
			return Warn, detail, "vaire status --db " + c.Name
		case st.LagSeconds == nil || st.LagBytes == nil:
			return Warn, detail, "vaire status --db " + c.Name
		}
		return Pass, detail, ""
	})
}

// followerLagDetail names the measured parts of a follower's lag and calls
// the unknown parts unknown.
func followerLagDetail(st *health.FollowerState, source string) string {
	seconds := "replay delay unknown"
	if st.LagSeconds != nil {
		lag := time.Duration(*st.LagSeconds * float64(time.Second))
		seconds = lag.Round(time.Second).String() + " behind " + source
	}
	bytes := "WAL backlog unknown"
	if st.LagBytes != nil {
		bytes = bytesText(*st.LagBytes) + " of WAL"
	}
	return seconds + ", " + bytes
}

func followerSlot(f Facts) []Result {
	return perFollower(f, false, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		if !st.SourceKnown {
			return Warn, "cannot read its slot on " + c.Follows + sideErrors(st.SourceError), "vaire status --db " + c.Name
		}
		refollow := "drop it and follow again: vaire db drop " + c.Name + " --confirm " + c.Name + ", then vaire db follow " + c.Follows + " --name " + c.Name
		switch {
		case !st.SlotExists:
			return Fail, "the slot " + st.Slot + " is missing on " + c.Follows, refollow
		case st.SlotWALStatus == "lost":
			return Fail, "the slot " + st.Slot + " on " + c.Follows + " is lost: the WAL it needed is gone", refollow
		case st.SlotHalfUsed():
			return Warn, fmt.Sprintf("the slot %s holds %s on %s, more than half of what max_slot_wal_keep_size lets it", st.Slot, slotHeld(st), c.Follows),
				"start the follower, or drop it if it isn't needed"
		}
		return Pass, fmt.Sprintf("the slot %s holds %s on %s", st.Slot, slotHeld(st), c.Follows), ""
	})
}

// slotHeld is the WAL a slot keeps, or that it couldn't be measured.
func slotHeld(st *health.FollowerState) string {
	if st.SlotRetainedBytes == nil {
		return "an unknown amount of WAL"
	}
	return bytesText(*st.SlotRetainedBytes)
}

func followerOrphanSlots(f Facts) []Result {
	var out []Result
	for _, e := range f.OrphanSlotErrors {
		out = append(out, Result{DB: e.Source, Status: Warn,
			Detail: "cannot list the replication slots on " + e.Source + ": " + e.Error,
			Fix:    "vaire status --db " + e.Source + " and vaire logs postgres --db " + e.Source})
	}
	for _, s := range f.OrphanSlots {
		out = append(out, Result{DB: s.Source, Status: Warn,
			Detail: "the replication slot " + s.Slot + " belongs to no follower and keeps WAL on " + s.Source,
			Fix: "on the node: sudo -u postgres psql -h /var/run/postgresql -p " + strconv.Itoa(s.Port) +
				" -d postgres -c \"SELECT pg_drop_replication_slot('" + s.Slot + "')\""})
	}
	if len(out) == 0 {
		return one(Skip, "no slot without its follower", "")
	}
	return out
}

// bytesText is n in binary units.
func bytesText(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
