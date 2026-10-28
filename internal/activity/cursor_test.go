package activity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func sampleCursor() cursorState {
	return cursorState{
		Version:     cursorVersion,
		StoreID:     "st-0123456789abcdef0123456789abcdef",
		After:       7,
		Snapshot:    20,
		SinceRaw:    "24h",
		UntilRaw:    "2026-10-26T00:00:00Z",
		SinceAt:     formatTime(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)),
		UntilAt:     formatTime(time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)),
		OperationID: "op-1",
		RequestID:   "req-1",
		JobID:       "job-1",
		Database:    "app_db",
		Action:      "backup.full",
		Outcome:     OutcomeFailed,
		Limit:       50,
	}
}

func TestCursorRoundTrip(t *testing.T) {
	raw := encodeCursor(sampleCursor())
	if raw == "" {
		t.Fatal("encodeCursor returned an empty cursor")
	}
	decoded, err := decodeCursor(raw)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	if decoded != sampleCursor() {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", decoded, sampleCursor())
	}
}

func TestCursorRejectsTampering(t *testing.T) {
	raw := encodeCursor(sampleCursor())
	tampered := raw[:len(raw)-2] + "AA"
	if _, err := decodeCursor(tampered); err == nil {
		t.Fatal("tampered cursor was accepted")
	}
	oversized := cursorPrefix + strings.Repeat("A", maxCursorBytes)
	if _, err := decodeCursor(oversized); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("oversized cursor error = %v", err)
	}
	if _, err := decodeCursor(""); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("empty cursor error = %v", err)
	}
	if _, err := decodeCursor("c1."); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("empty payload error = %v", err)
	}
}

func TestCursorRejectsUnknownOrInvalidFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"unknown field", `{"v":1,"a":1,"s":2,"l":10,"extra":"x"}`},
		{"zero after", `{"v":1,"a":0,"s":2,"l":10}`},
		{"snapshot below after", `{"v":1,"a":5,"s":4,"l":10}`},
		{"bad limit", `{"v":1,"a":1,"s":2,"l":0}`},
		{"bad version", `{"v":2,"a":1,"s":2,"l":10}`},
		{"bad action", `{"v":1,"a":1,"s":2,"l":10,"ac":"two words"}`},
		{"bad outcome", `{"v":1,"a":1,"s":2,"l":10,"oc":"done"}`},
		{"bad since", `{"v":1,"a":1,"s":2,"l":10,"sa":"yesterday"}`},
		{"inverted window", `{"v":1,"a":1,"s":2,"l":10,"sa":"2026-10-21T00:00:00.000000000Z","ua":"2026-10-20T00:00:00.000000000Z"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := cursorPrefix + base64.RawURLEncoding.EncodeToString([]byte(tc.body))
			if _, err := decodeCursor(raw); !errors.Is(err, errInvalidCursor) {
				t.Fatalf("decodeCursor error = %v, want errInvalidCursor", err)
			}
		})
	}
}

func TestCursorRejectsTrailingJSON(t *testing.T) {
	body, err := json.Marshal(sampleCursor())
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, []byte(`{"x":1}`)...)
	raw := cursorPrefix + base64.RawURLEncoding.EncodeToString(body)
	if _, err := decodeCursor(raw); !errors.Is(err, errInvalidCursor) {
		t.Fatalf("decodeCursor error = %v, want errInvalidCursor", err)
	}
}

func TestCursorKeepsMixedWindowAfterClockAdvances(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	q := Query{Since: "1h", Until: now.Add(-30 * time.Minute).Format(time.RFC3339), Limit: 1}
	first, err := normalizeQuery(q, now)
	if err != nil {
		t.Fatal(err)
	}
	q.Cursor = cursorFor(first, 1, 2, sampleCursor().StoreID)
	next, err := normalizeQuery(q, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("frozen cursor stopped working: %v", err)
	}
	if !next.since.Equal(first.since) || !next.until.Equal(first.until) {
		t.Fatalf("cursor window drifted: got %v to %v, want %v to %v", next.since, next.until, first.since, first.until)
	}
	q.Cursor = ""
	if _, err := normalizeQuery(q, now.Add(2*time.Hour)); !errors.Is(err, errInvalidQuery) {
		t.Fatalf("a new inverted query must still fail: %v", err)
	}
}
