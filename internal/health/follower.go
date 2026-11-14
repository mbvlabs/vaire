package health

import (
	"encoding/json"
	"math"
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
	// SourceKnown and SelfKnown say independently whether the source and the
	// follower answered. A measurement is meaningful only when its side is.
	SourceKnown bool   `json:"source_known"`
	SelfKnown   bool   `json:"self_known"`
	SourceError string `json:"source_error,omitempty"`
	SelfError   string `json:"self_error,omitempty"`
	// Streaming is pg_stat_replication's state streaming on the source.
	Streaming bool `json:"streaming"`
	// SenderState is pg_stat_replication's state, empty without a sender.
	SenderState string `json:"sender_state,omitempty"`
	// InRecovery and ReplayPaused are the follower's own view; a follower
	// out of recovery is no longer a standby.
	InRecovery   bool `json:"in_recovery"`
	ReplayPaused bool `json:"replay_paused"`
	// LagBytes is how much WAL the source wrote that the follower hasn't
	// replayed; LagSeconds is PostgreSQL's replay delay. Either is nil when
	// it couldn't be measured: an unknown lag is not zero.
	LagBytes            *int64   `json:"lag_bytes,omitempty"`
	LagSeconds          *float64 `json:"lag_seconds,omitempty"`
	ReceiveBacklogBytes *uint64  `json:"receive_backlog_bytes,omitempty"`
	ReplayBacklogBytes  *uint64  `json:"replay_backlog_bytes,omitempty"`
	// The slot on the source: whether it exists and is active, its
	// wal_status (reserved, extended, unreserved, lost), the WAL it holds,
	// and how much more may be written before it is lost. SlotSafeBytes is
	// a measured allowance, negative once the slot is already past its cap;
	// it is -1 only for a proven uncapped slot, and nil when unmeasured.
	// SlotUncapped is set only from the source's effective
	// max_slot_wal_keep_size = -1 evidence.
	SlotExists        bool   `json:"slot_exists"`
	SlotActive        bool   `json:"slot_active"`
	SlotWALStatus     string `json:"slot_wal_status,omitempty"`
	SlotRetainedBytes *int64 `json:"slot_retained_bytes,omitempty"`
	SlotSafeBytes     *int64 `json:"slot_safe_bytes,omitempty"`
	SlotUncapped      bool   `json:"slot_uncapped,omitempty"`
}

// MarshalJSON leaves out a fact whose side didn't answer, so an unknown
// state is never encoded as false or as the -1 no-cap sentinel.
func (s FollowerState) MarshalJSON() ([]byte, error) {
	type state FollowerState
	raw, err := json.Marshal(state(s))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if !s.SourceKnown {
		delete(fields, "streaming")
		delete(fields, "slot_exists")
		delete(fields, "slot_active")
		delete(fields, "slot_safe_bytes")
		delete(fields, "slot_uncapped")
	}
	if !s.SelfKnown {
		delete(fields, "in_recovery")
		delete(fields, "replay_paused")
	}
	return json.Marshal(fields)
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
  pg_current_wal_lsn()::text AS source_lsn,
  coalesce((SELECT state ` + rep + `), '') AS sender_state,
  coalesce((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), replay_lsn)::bigint ` + rep + `)::text, '') AS lag_bytes,
  coalesce((SELECT extract(epoch FROM replay_lag)::float8 ` + rep + `)::text, '') AS replay_lag,
  coalesce((SELECT active::text ` + sl + `), '') AS slot_active,
  coalesce((SELECT wal_status ` + sl + `), '') AS slot_wal_status,
  coalesce((SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)::bigint ` + sl + `)::text, '') AS slot_retained,
  coalesce((SELECT safe_wal_size ` + sl + `)::text, '') AS slot_safe,
  coalesce((SELECT (setting = '-1')::text FROM pg_settings WHERE name = 'max_slot_wal_keep_size'), '') AS slot_uncapped`
}

// FollowerSelfSQL reads a follower's own side: recovery and a paused replay.
const FollowerSelfSQL = `SELECT
  pg_is_in_recovery()::text AS in_recovery,
  CASE WHEN pg_is_in_recovery() THEN pg_get_wal_replay_pause_state() ELSE '' END AS pause_state,
  coalesce(pg_last_wal_receive_lsn()::text, '') AS receive_lsn,
  coalesce(pg_last_wal_replay_lsn()::text, '') AS replay_lsn`

// SlotsSQL lists a cluster's vaire follower slots (vaire_*).
const SlotsSQL = `SELECT slot_name FROM pg_replication_slots WHERE left(slot_name, 6) = 'vaire_' ORDER BY slot_name`

// ParseFollower combines both sides' rows into a FollowerState. A nil row
// is a side that couldn't be read; measurements from that side stay unknown.
func ParseFollower(source, slot string, src, self map[string]string) FollowerState {
	st := FollowerState{Source: source, Slot: slot, SourceKnown: src != nil, SelfKnown: self != nil}
	if self != nil {
		st.InRecovery = self["in_recovery"] == "true"
		st.ReplayPaused = self["pause_state"] == "paused" || self["pause_state"] == "pause requested"
	}
	if src == nil {
		return st
	}
	st.SenderState = src["sender_state"]
	st.Streaming = st.SenderState == "streaming"
	if st.SelfKnown && st.InRecovery {
		st.ReceiveBacklogBytes = lsnBacklog(src["source_lsn"], self["receive_lsn"])
		st.ReplayBacklogBytes = lsnBacklog(src["source_lsn"], self["replay_lsn"])
	}
	st.SlotExists = src["slot_active"] != ""
	if st.SlotExists {
		st.SlotActive = src["slot_active"] == "true"
		st.SlotWALStatus = src["slot_wal_status"]
		if retained, ok := parseKnownInt(src["slot_retained"]); ok {
			st.SlotRetainedBytes = &retained
		}
		st.SlotSafeBytes, st.SlotUncapped = slotSafety(src)
	}
	if lag, ok := parseKnownInt(src["lag_bytes"]); ok {
		// A replay LSN at or ahead of the source's is a caught-up follower.
		if lag < 0 {
			lag = 0
		}
		st.LagBytes = &lag
	}
	switch {
	case !st.Streaming:
		// PostgreSQL reports replay_lag on a walsender: no sender, no
		// seconds measurement.
	case src["replay_lag"] != "":
		if seconds, ok := parseKnownFloat(src["replay_lag"]); ok {
			st.LagSeconds = &seconds
		}
	case st.LagBytes != nil && *st.LagBytes == 0:
		// PostgreSQL shows NULL once a standby has caught up and the source
		// is idle: known zero bytes is no replay delay. Positive or unknown
		// bytes leave the seconds unknown.
		seconds := 0.0
		st.LagSeconds = &seconds
	}
	return st
}

func parseLSN(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(s, "/")
	if !ok || hi == "" || lo == "" || len(hi) > 8 || len(lo) > 8 {
		return 0, false
	}
	h, e1 := strconv.ParseUint(hi, 16, 32)
	l, e2 := strconv.ParseUint(lo, 16, 32)
	if e1 != nil || e2 != nil {
		return 0, false
	}
	return h<<32 | l, true
}

func lsnBacklog(source, follower string) *uint64 {
	s, ok := parseLSN(source)
	f, ok2 := parseLSN(follower)
	if !ok || !ok2 || f > s || s-f > math.MaxInt64 {
		return nil
	}
	n := s - f
	return &n
}

// SlotHalfUsed reports whether the slot holds more WAL than it may still
// take before it is lost: past half of max_slot_wal_keep_size. A measured
// negative allowance is already past the cap. An unknown allowance or a
// proven uncapped slot cannot be half used.
func (s FollowerState) SlotHalfUsed() bool {
	if s.SlotUncapped || s.SlotSafeBytes == nil || s.SlotRetainedBytes == nil {
		return false
	}
	return s.SlotExists && *s.SlotRetainedBytes > *s.SlotSafeBytes
}

// slotSafety reads the source's slot allowance. A measured safe_wal_size
// (zero and negative included; negative means already past the cap) is the
// allowance. NULL is not unlimited by itself: it is the -1 no-cap sentinel
// only when the source's effective max_slot_wal_keep_size is -1, the slot is
// not lost, and its restart position was readable. A malformed or NULL value
// without that evidence is unknown, and so is a lost or missing slot.
func slotSafety(src map[string]string) (*int64, bool) {
	if n, ok := parseKnownInt(src["slot_safe"]); ok {
		return &n, false
	}
	if strings.TrimSpace(src["slot_safe"]) != "" {
		return nil, false
	}
	if strings.TrimSpace(src["slot_uncapped"]) != "true" {
		return nil, false
	}
	if status := strings.TrimSpace(src["slot_wal_status"]); status == "" || status == "lost" {
		return nil, false
	}
	if strings.TrimSpace(src["slot_retained"]) == "" {
		return nil, false
	}
	unlimited := int64(-1)
	return &unlimited, true
}

func parseKnownInt(v string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n, err == nil
}

func parseKnownFloat(v string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return f, err == nil
}
