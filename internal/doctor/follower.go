package doctor

import (
	"fmt"
	"strconv"
	"time"
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
// none. A follower awaiting recovery or not running is skipped (pg.running
// reports it); one whose replication couldn't be read warns.
func perFollower(f Facts, fn func(c ClusterFacts) (Status, string, string)) []Result {
	var out []Result
	for _, c := range f.Clusters {
		if c.Follows == "" {
			continue
		}
		r := Result{DB: c.Name}
		switch {
		case c.AwaitingRecovery:
			r.Status, r.Detail = Skip, "awaiting recovery (vaire db recover "+c.Name+")"
		case c.State != "running":
			r.Status, r.Detail = Skip, "the cluster is "+c.State
		case c.Follower == nil || !c.Follower.Known:
			r.Status, r.Detail = Warn, "cannot read its replication"
			if c.Follower != nil && c.Follower.Error != "" {
				r.Detail += ": " + c.Follower.Error
			}
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

func followerStreaming(f Facts) []Result {
	return perFollower(f, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		logs := "vaire logs postgres --db " + c.Name
		switch {
		case !st.InRecovery:
			return Fail, "it is out of recovery: no longer a standby of " + c.Follows, "vaire db unfollow " + c.Name + " --confirm " + c.Name + "/unfollow, or drop it and follow " + c.Follows + " again"
		case st.ReplayPaused:
			return Fail, "its replay is paused (a hot-standby floor below " + c.Follows + "'s pauses it)", "vaire apply --db " + c.Name + " --confirm " + c.Name + "/restart; then " + logs
		case !st.Streaming:
			detail := "it is not streaming from " + c.Follows
			if st.SenderState != "" {
				detail += " (walsender " + st.SenderState + ")"
			}
			return Fail, detail, logs
		}
		return Pass, "streaming from " + c.Follows, ""
	})
}

func followerLag(f Facts) []Result {
	return perFollower(f, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		lag := time.Duration(st.LagSeconds * float64(time.Second))
		detail := fmt.Sprintf("%s behind %s, %s of WAL", lag.Round(time.Second), c.Follows, bytesText(st.LagBytes))
		switch {
		case st.LagBytes >= FollowerLagFailBytes || lag >= FollowerLagFail:
			return Fail, detail, "vaire status --db " + c.Name + "; a follower this far behind may lose its slot"
		case st.LagBytes >= FollowerLagWarnBytes || lag >= FollowerLagWarn:
			return Warn, detail, "vaire status --db " + c.Name
		}
		return Pass, detail, ""
	})
}

func followerSlot(f Facts) []Result {
	return perFollower(f, func(c ClusterFacts) (Status, string, string) {
		st := c.Follower
		refollow := "drop it and follow again: vaire db drop " + c.Name + " --confirm " + c.Name + ", then vaire db follow " + c.Follows + " --name " + c.Name
		switch {
		case !st.SlotExists:
			return Fail, "the slot " + st.Slot + " is missing on " + c.Follows, refollow
		case st.SlotWALStatus == "lost":
			return Fail, "the slot " + st.Slot + " on " + c.Follows + " is lost: the WAL it needed is gone", refollow
		case st.SlotHalfUsed():
			return Warn, fmt.Sprintf("the slot %s holds %s on %s, more than half of what max_slot_wal_keep_size lets it", st.Slot, bytesText(st.SlotRetainedBytes), c.Follows),
				"start the follower, or drop it if it isn't needed"
		}
		return Pass, fmt.Sprintf("the slot %s holds %s on %s", st.Slot, bytesText(st.SlotRetainedBytes), c.Follows), ""
	})
}

func followerOrphanSlots(f Facts) []Result {
	if len(f.OrphanSlots) == 0 {
		return one(Skip, "no slot without its follower", "")
	}
	out := make([]Result, 0, len(f.OrphanSlots))
	for _, s := range f.OrphanSlots {
		out = append(out, Result{DB: s.Source, Status: Warn,
			Detail: "the replication slot " + s.Slot + " belongs to no follower and keeps WAL on " + s.Source,
			Fix: "on the node: sudo -u postgres psql -h /var/run/postgresql -p " + strconv.Itoa(s.Port) +
				" -d postgres -c \"SELECT pg_drop_replication_slot('" + s.Slot + "')\""})
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
