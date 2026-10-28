package health

import (
	"strings"
	"testing"
)

func TestParseFollowerStreaming(t *testing.T) {
	src := map[string]string{"sender_state": "streaming", "lag_bytes": "4096", "replay_lag": "0.25",
		"slot_active": "true", "slot_wal_status": "reserved", "slot_retained": "8192", "slot_safe": "1000000"}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused", "replay_age": "3600"}
	st := ParseFollower("app", "vaire_f", src, self)
	if !st.Known || !st.Streaming || !st.InRecovery || st.ReplayPaused {
		t.Fatalf("%+v", st)
	}
	if st.LagBytes != 4096 || st.LagSeconds != 0.25 || !st.SlotExists || !st.SlotActive || st.SlotHalfUsed() {
		t.Fatalf("%+v", st)
	}
}

func TestParseFollowerCaughtUpAndIdle(t *testing.T) {
	// replay_lag is NULL once idle; the last commit is an hour old.
	src := map[string]string{"sender_state": "streaming", "lag_bytes": "0", "slot_active": "true", "slot_retained": "0", "slot_safe": ""}
	self := map[string]string{"in_recovery": "true", "pause_state": "not paused", "replay_age": "3600"}
	st := ParseFollower("app", "vaire_f", src, self)
	if st.LagSeconds != 0 || st.LagBytes != 0 || st.SlotSafeBytes != -1 || st.SlotHalfUsed() {
		t.Fatalf("an idle, caught-up follower has no lag: %+v", st)
	}
}

func TestParseFollowerStopped(t *testing.T) {
	// No walsender: the slot holds what the follower hasn't confirmed.
	src := map[string]string{"sender_state": "", "lag_bytes": "", "slot_active": "false", "slot_wal_status": "extended",
		"slot_retained": "3000000", "slot_safe": "1000000"}
	st := ParseFollower("app", "vaire_f", src, nil)
	if st.Streaming || st.LagBytes != 3000000 || !st.SlotExists || st.SlotActive || !st.SlotHalfUsed() {
		t.Fatalf("%+v", st)
	}
	self := map[string]string{"in_recovery": "true", "pause_state": "paused", "replay_age": "120"}
	st = ParseFollower("app", "vaire_f", src, self)
	if !st.ReplayPaused || st.LagSeconds != 120 {
		t.Fatalf("%+v", st)
	}
}

func TestParseFollowerLostAndMissingSlot(t *testing.T) {
	lost := ParseFollower("app", "vaire_f", map[string]string{"slot_active": "false", "slot_wal_status": "lost", "slot_retained": ""}, nil)
	if !lost.SlotExists || lost.SlotWALStatus != "lost" || lost.LagBytes != 0 {
		t.Fatalf("%+v", lost)
	}
	missing := ParseFollower("app", "vaire_f", map[string]string{"slot_active": ""}, nil)
	if missing.SlotExists {
		t.Fatalf("%+v", missing)
	}
	unknown := ParseFollower("app", "vaire_f", nil, nil)
	if unknown.Known {
		t.Fatal("neither side read is not known")
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
