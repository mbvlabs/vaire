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
		st.SlotSafeBytes != nil || st.SlotHalfUsed() {
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

// FG-m2/A-5: a NULL safe_wal_size is not unlimited. The allowance is omitted
// unless it was measured; a slot is uncapped only when the source proves
// max_slot_wal_keep_size is -1 for a valid, non-lost slot. Known zero and a
// negative (already past the cap) stay measured values.
func TestParseFollowerSlotSafetyUnknown(t *testing.T) {
	int64p := func(v int64) *int64 { return &v }
	base := map[string]string{"slot_active": "true", "slot_wal_status": "reserved", "slot_retained": "4096"}
	row := func(extra map[string]string) map[string]string {
		out := make(map[string]string, len(base)+len(extra))
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	cases := map[string]struct {
		src      map[string]string
		want     *int64
		uncapped bool
	}{
		"missing slot": {src: map[string]string{"slot_active": ""}},
		"lost slot": {src: map[string]string{"slot_active": "false", "slot_wal_status": "lost",
			"slot_retained": "0", "slot_safe": "", "slot_uncapped": "true"}},
		"null safe size without uncapped evidence": {src: row(map[string]string{"slot_safe": ""})},
		"null safe size with a cap":                {src: row(map[string]string{"slot_safe": "", "slot_uncapped": "false"})},
		"malformed safe size":                      {src: row(map[string]string{"slot_safe": "12x", "slot_uncapped": "true"})},
		"measured zero":                            {src: row(map[string]string{"slot_safe": "0"}), want: int64p(0)},
		"measured cap":                             {src: row(map[string]string{"slot_safe": "1024"}), want: int64p(1024)},
		"measured past the cap":                    {src: row(map[string]string{"slot_safe": "-2048"}), want: int64p(-2048)},
		"measured one byte past":                   {src: row(map[string]string{"slot_safe": "-1"}), want: int64p(-1)},
		"proven uncapped": {src: row(map[string]string{"slot_safe": "", "slot_uncapped": "true"}),
			want: int64p(-1), uncapped: true},
	}
	for name, tc := range cases {
		st := ParseFollower("app", "vaire_f", tc.src, nil)
		switch {
		case tc.want == nil && st.SlotSafeBytes != nil:
			t.Errorf("%s: allowance %d, want unknown", name, *st.SlotSafeBytes)
		case tc.want != nil && (st.SlotSafeBytes == nil || *st.SlotSafeBytes != *tc.want):
			t.Errorf("%s: allowance %v, want %d", name, st.SlotSafeBytes, *tc.want)
		}
		if st.SlotUncapped != tc.uncapped {
			t.Errorf("%s: uncapped %v, want %v", name, st.SlotUncapped, tc.uncapped)
		}
		raw, err := json.Marshal(st)
		if err != nil {
			t.Fatal(err)
		}
		doc := jsonObject(t, raw)
		if _, present := doc["slot_safe_bytes"]; present != (tc.want != nil) {
			t.Errorf("%s: slot_safe_bytes present %v, want %v: %s", name, present, tc.want != nil, raw)
		}
		if _, present := doc["slot_uncapped"]; present != tc.uncapped {
			t.Errorf("%s: slot_uncapped present %v, want %v: %s", name, present, tc.uncapped, raw)
		}
	}

	// An unknown allowance cannot prove a slot is past half of a cap; a
	// measured zero or negative value with retained WAL does.
	unknown := ParseFollower("app", "vaire_f", row(map[string]string{"slot_safe": ""}), nil)
	if unknown.SlotHalfUsed() {
		t.Errorf("an unknown allowance was treated as measured: %+v", unknown)
	}
	uncapped := ParseFollower("app", "vaire_f", row(map[string]string{"slot_safe": "", "slot_uncapped": "true"}), nil)
	if uncapped.SlotHalfUsed() {
		t.Errorf("an uncapped slot was treated as measured: %+v", uncapped)
	}
	zero := ParseFollower("app", "vaire_f", row(map[string]string{"slot_safe": "0"}), nil)
	if !zero.SlotHalfUsed() {
		t.Errorf("a measured zero allowance lost the slot pressure: %+v", zero)
	}
	negative := ParseFollower("app", "vaire_f", row(map[string]string{"slot_safe": "-2048"}), nil)
	if !negative.SlotHalfUsed() {
		t.Errorf("a measured negative allowance lost the slot pressure: %+v", negative)
	}
	// A measured -1 is a real over-cap allowance, not the no-cap sentinel:
	// SlotUncapped is what distinguishes the two.
	minusOne := ParseFollower("app", "vaire_f", row(map[string]string{"slot_safe": "-1"}), nil)
	if minusOne.SlotUncapped || !minusOne.SlotHalfUsed() {
		t.Errorf("a measured -1 was taken as uncapped: %+v", minusOne)
	}
}

// jsonObject is raw decoded as a JSON object.
func jsonObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
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
