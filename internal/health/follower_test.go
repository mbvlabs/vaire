package health

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseFollowerStreaming(t *testing.T) {
	src := map[string]string{"sender_state": "streaming", "lag_bytes": "4096", "replay_lag": "0.25",
		"slot_active": "true", "slot_wal_status": "reserved", "slot_retained": "8192", "slot_safe": "1000000"}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused"}
	st := ParseFollower("app", "vaire_f", src, self)
	if !st.SourceKnown || !st.SelfKnown || !st.Streaming || !st.InRecovery || st.ReplayPaused {
		t.Fatalf("%+v", st)
	}
	if st.LagBytes == nil || *st.LagBytes != 4096 || st.LagSeconds == nil || *st.LagSeconds != 0.25 ||
		!st.SlotExists || !st.SlotActive || st.SlotRetainedBytes == nil || *st.SlotRetainedBytes != 8192 || st.SlotHalfUsed() {
		t.Fatalf("%+v", st)
	}
}

func TestParseFollowerCaughtUpAndIdle(t *testing.T) {
	// replay_lag is NULL once a standby has caught up and the source is idle.
	src := map[string]string{"sender_state": "streaming", "lag_bytes": "0", "slot_active": "true", "slot_retained": "0", "slot_safe": ""}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused"}
	st := ParseFollower("app", "vaire_f", src, self)
	if st.LagBytes == nil || *st.LagBytes != 0 || st.LagSeconds == nil || *st.LagSeconds != 0 ||
		st.SlotSafeBytes != -1 || st.SlotHalfUsed() {
		t.Fatalf("an idle, caught-up follower has no lag: %+v", st)
	}
}

// R4-4: a NULL replay_lag beside a positive byte backlog is an unknown
// delay, not a caught-up zero. An old replay timestamp is no substitute.
func TestParseFollowerNullReplayLagWithBacklog(t *testing.T) {
	src := map[string]string{"sender_state": "streaming", "lag_bytes": "4096", "slot_active": "true", "slot_retained": "0", "slot_safe": ""}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused", "replay_age": "3600"}
	st := ParseFollower("app", "vaire_f", src, self)
	if st.LagBytes == nil || *st.LagBytes != 4096 || st.LagSeconds != nil {
		t.Fatalf("positive bytes with no replay_lag has an unknown delay: %+v", st)
	}
}

// R4-1/Q3: no walsender means no byte-lag measurement; the slot's retained
// WAL is a separate measurement.
func TestParseFollowerStopped(t *testing.T) {
	src := map[string]string{"sender_state": "", "lag_bytes": "", "slot_active": "false", "slot_wal_status": "extended",
		"slot_retained": "3000000", "slot_safe": "1000000"}
	st := ParseFollower("app", "vaire_f", src, nil)
	if !st.SourceKnown || st.SelfKnown || st.Streaming || st.LagBytes != nil || st.LagSeconds != nil ||
		!st.SlotExists || st.SlotActive || st.SlotRetainedBytes == nil || *st.SlotRetainedBytes != 3000000 || !st.SlotHalfUsed() {
		t.Fatalf("%+v", st)
	}
	self := map[string]string{"in_recovery": "true", "pause_state": "paused"}
	st = ParseFollower("app", "vaire_f", src, self)
	if !st.ReplayPaused || st.LagSeconds != nil {
		t.Fatalf("%+v", st)
	}
}

func TestParseFollowerLostAndMissingSlot(t *testing.T) {
	lost := ParseFollower("app", "vaire_f", map[string]string{"slot_active": "false", "slot_wal_status": "lost", "slot_retained": ""}, nil)
	if !lost.SlotExists || lost.SlotWALStatus != "lost" || lost.SlotRetainedBytes != nil || lost.LagBytes != nil {
		t.Fatalf("%+v", lost)
	}
	missing := ParseFollower("app", "vaire_f", map[string]string{"slot_active": ""}, nil)
	if missing.SlotExists || missing.SlotRetainedBytes != nil {
		t.Fatalf("%+v", missing)
	}
}

// R4-1: each side's availability is independent, and unknown measurements
// are left out of JSON rather than written as zero.
func TestParseFollowerSidesAndJSON(t *testing.T) {
	src := map[string]string{"sender_state": "", "lag_bytes": "", "slot_active": "false", "slot_wal_status": "extended",
		"slot_retained": "3000000", "slot_safe": "1000000"}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused"}
	for name, tc := range map[string]struct {
		src, self     map[string]string
		source, selfK bool
		slotRetained  bool
	}{
		"source only": {src: src, source: true, slotRetained: true},
		"self only":   {self: self, selfK: true},
		"neither":     {},
		"both":        {src: src, self: self, source: true, selfK: true, slotRetained: true},
	} {
		st := ParseFollower("app", "vaire_f", tc.src, tc.self)
		if st.SourceKnown != tc.source || st.SelfKnown != tc.selfK {
			t.Errorf("%s: source known %v, self known %v: %+v", name, st.SourceKnown, st.SelfKnown, st)
		}
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatal(err)
		}
		_, hasBytes := doc["lag_bytes"]
		_, hasSeconds := doc["lag_seconds"]
		_, hasRetained := doc["slot_retained_bytes"]
		if hasBytes || hasSeconds || hasRetained != tc.slotRetained {
			t.Errorf("%s: unknown measurements in JSON: %s", name, raw)
		}
	}
	unknown := ParseFollower("app", "vaire_f", nil, nil)
	raw, _ := json.Marshal(unknown)
	if !strings.Contains(string(raw), `"source_known":false`) || !strings.Contains(string(raw), `"self_known":false`) ||
		strings.Contains(string(raw), `"lag_seconds"`) || strings.Contains(string(raw), `"slot_exists"`) ||
		strings.Contains(string(raw), `"in_recovery"`) || strings.Contains(string(raw), `"slot_safe_bytes"`) {
		t.Fatalf("an unread follower claims measurements: %s", raw)
	}
	known := ParseFollower("app", "vaire_f", map[string]string{"slot_active": "true", "slot_safe": "1000"},
		map[string]string{"in_recovery": "true"})
	raw, _ = json.Marshal(known)
	if !strings.Contains(string(raw), `"slot_exists":true`) || !strings.Contains(string(raw), `"in_recovery":true`) {
		t.Fatalf("a read follower lost its facts: %s", raw)
	}
}

func TestFollowerSourceSQLQuotes(t *testing.T) {
	sql := FollowerSourceSQL("app_follower", "vaire_app_follower")
	if !strings.Contains(sql, "application_name = 'app_follower'") || !strings.Contains(sql, "slot_name = 'vaire_app_follower'") {
		t.Fatal(sql)
	}
}

func TestSlotsSQLMatchesTheSlotPrefix(t *testing.T) {
	// The vaire_ prefix is six bytes; a query that compares four matches
	// nothing.
	if !strings.Contains(SlotsSQL, "left(slot_name, 6) = 'vaire_'") {
		t.Fatal(SlotsSQL)
	}
}
