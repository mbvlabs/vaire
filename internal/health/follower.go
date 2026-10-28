package health

import (
	"strconv"
	"strings"
)

// Followers (Phase 6): a follower's state is read from both sides, its
// source (pg_stat_replication and the follower's slot) and the follower
// itself (recovery, replay pause, the last replayed commit). The node runs
// the queries; this file builds them and turns their rows into a
// FollowerState.

// FollowerState is one follower's replication as vaire reads it.
type FollowerState struct {
	Source string `json:"source"`
	Slot   string `json:"slot"`
	// Known is false when neither side could be read.
	Known bool   `json:"known"`
	Error string `json:"error,omitempty"`
	// Streaming is pg_stat_replication's state streaming on the source.
	Streaming bool `json:"streaming"`
	// SenderState is pg_stat_replication's state, empty without a sender.
	SenderState string `json:"sender_state,omitempty"`
	// InRecovery and ReplayPaused are the follower's own view; a follower
	// out of recovery is no longer a standby.
	InRecovery   bool `json:"in_recovery"`
	ReplayPaused bool `json:"replay_paused"`
	// LagBytes is how much WAL the source wrote that the follower hasn't
	// replayed; LagSeconds how far behind its replay is (zero when caught
	// up).
	LagBytes   int64   `json:"lag_bytes"`
	LagSeconds float64 `json:"lag_seconds"`
	// The slot on the source: whether it exists and is active, its
	// wal_status (reserved, extended, unreserved, lost), the WAL it holds,
	// and how much more may be written before it is lost (-1: no cap).
	SlotExists        bool   `json:"slot_exists"`
	SlotActive        bool   `json:"slot_active"`
	SlotWALStatus     string `json:"slot_wal_status,omitempty"`
	SlotRetainedBytes int64  `json:"slot_retained_bytes"`
	SlotSafeBytes     int64  `json:"slot_safe_bytes"`
}

// quote single-quotes a validated name for a SQL literal.
func quote(name string) string { return "'" + strings.ReplaceAll(name, "'", "''") + "'" }

// FollowerSourceSQL reads follower's replication on its source: the
// newest walsender with its application_name, and its slot.
func FollowerSourceSQL(follower, slot string) string {
	app, s := quote(follower), quote(slot)
	rep := "FROM pg_stat_replication WHERE application_name = " + app + " ORDER BY backend_start DESC LIMIT 1"
	sl := "FROM pg_replication_slots WHERE slot_name = " + s
	return `SELECT
  coalesce((SELECT state ` + rep + `), '') AS sender_state,
  coalesce((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint ` + rep + `)::text, '') AS lag_bytes,
  coalesce((SELECT extract(epoch FROM replay_lag)::float8 ` + rep + `)::text, '') AS replay_lag,
  coalesce((SELECT active::text ` + sl + `), '') AS slot_active,
  coalesce((SELECT wal_status ` + sl + `), '') AS slot_wal_status,
  coalesce((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint ` + sl + `)::text, '') AS slot_retained,
  coalesce((SELECT safe_wal_size ` + sl + `)::text, '') AS slot_safe`
}

// FollowerSelfSQL reads a follower's own side: recovery, a paused replay
// and the age of its last replayed commit.
const FollowerSelfSQL = `SELECT
  pg_is_in_recovery()::text AS in_recovery,
  CASE WHEN pg_is_in_recovery() THEN pg_get_wal_replay_pause_state() ELSE '' END AS pause_state,
  coalesce(extract(epoch FROM now() - pg_last_xact_replay_timestamp())::float8::text, '') AS replay_age`

// SlotsSQL lists a cluster's vaire follower slots (vaire_*).
const SlotsSQL = `SELECT slot_name FROM pg_replication_slots WHERE left(slot_name, 6) = 'vaire_' ORDER BY slot_name`

// ParseFollower combines both sides' rows into a FollowerState. A nil row
// is a side that couldn't be read.
func ParseFollower(source, slot string, src, self map[string]string) FollowerState {
	st := FollowerState{Source: source, Slot: slot, SlotSafeBytes: -1, Known: src != nil || self != nil}
	replayAge := -1.0
	if self != nil {
		st.InRecovery = self["in_recovery"] == "true"
		st.ReplayPaused = self["pause_state"] == "paused" || self["pause_state"] == "pause requested"
		replayAge = parseFloat(self["replay_age"], -1)
	}
	if src == nil {
		return st
	}
	st.SenderState = src["sender_state"]
	st.Streaming = st.SenderState == "streaming"
	st.SlotExists = src["slot_active"] != ""
	st.SlotActive = src["slot_active"] == "true"
	st.SlotWALStatus = src["slot_wal_status"]
	st.SlotRetainedBytes = parseInt(src["slot_retained"], 0)
	st.SlotSafeBytes = parseInt(src["slot_safe"], -1)
	switch {
	case src["lag_bytes"] != "":
		st.LagBytes = parseInt(src["lag_bytes"], 0)
	case st.SlotExists:
		// No walsender: what the slot still holds is what the follower
		// hasn't confirmed.
		st.LagBytes = st.SlotRetainedBytes
	}
	if st.LagBytes < 0 {
		st.LagBytes = 0
	}
	switch {
	case st.Streaming && src["replay_lag"] != "":
		st.LagSeconds = parseFloat(src["replay_lag"], 0)
	case st.Streaming:
		// PostgreSQL shows NULL once a standby has caught up and the source
		// is idle: that is no lag, however old the last commit is.
		st.LagSeconds = 0
	case st.LagBytes > 0 && replayAge >= 0:
		st.LagSeconds = replayAge
	}
	return st
}

// SlotHalfUsed reports whether the slot holds more WAL than it may still
// take before it is lost: past half of max_slot_wal_keep_size.
func (s FollowerState) SlotHalfUsed() bool {
	return s.SlotSafeBytes >= 0 && s.SlotRetainedBytes > s.SlotSafeBytes
}

func parseInt(v string, def int64) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	if err != nil {
		return def
	}
	return n
}

func parseFloat(v string, def float64) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}
