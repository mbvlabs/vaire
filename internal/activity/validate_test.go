package activity

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func validEvent() Event {
	return Event{
		Server:          "db1.example.com",
		Node:            "db1",
		NodeInstanceID:  "8b1d4c1e-0c5a-4a53-9a3e-2b0f1f0b6f11",
		BootID:          "b1d4c1e0c5a44a539a3e2b0f1f0b6f11",
		OperationID:     "op-7f3a",
		RequestID:       "req-1",
		ParentRequestID: "req-0",
		JobID:           "job-9",
		Source:          SourceStep,
		Action:          "postgres.start",
		Step:            "postgres.start",
		Database:        "app_db",
		Outcome:         OutcomeSucceeded,
		ErrorCode:       "PG_LOCK_BUSY",
		ReasonCode:      "HEAVY_IO",
		DurationMS:      1250,
		ActorKind:       ActorOperator,
		ActorLabel:      "mbv@example.com",
		NodeVersion:     "1.2.3-abc+df1",
	}
}

func TestValidateEventAcceptsAllowlistedFields(t *testing.T) {
	if err := ValidateEvent(Event{Source: SourceRPC, Action: "backup.full", Outcome: OutcomeStarted}); err != nil {
		t.Fatalf("minimal event: %v", err)
	}
	if err := ValidateEvent(validEvent()); err != nil {
		t.Fatalf("full event: %v", err)
	}
	timer := validEvent()
	timer.ActorKind = ActorTimer
	timer.ActorLabel = ""
	timer.ActorVerified = true
	if err := ValidateEvent(timer); err != nil {
		t.Fatalf("verified timer: %v", err)
	}
}

func TestValidateEventRejectsOutsideContract(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Event)
		wantErr error
	}{
		{"schema", func(ev *Event) { ev.Schema = "vaire-activity/v2" }, errInvalidEvent},
		{"missing source", func(ev *Event) { ev.Source = "" }, errInvalidEvent},
		{"unknown source", func(ev *Event) { ev.Source = "sql" }, errInvalidEvent},
		{"missing action", func(ev *Event) { ev.Action = "" }, errInvalidEvent},
		{"sql action", func(ev *Event) { ev.Action = "SELECT * FROM pg_shadow" }, errInvalidEvent},
		{"url action", func(ev *Event) { ev.Action = "https://example.com/backup" }, errInvalidEvent},
		{"query string action", func(ev *Event) { ev.Action = "backup?full=1" }, errInvalidEvent},
		{"multiline action", func(ev *Event) { ev.Action = "backup.full\nsucceeded" }, errInvalidEvent},
		{"control action", func(ev *Event) { ev.Action = "backup\x00full" }, errInvalidEvent},
		{"key value action", func(ev *Event) { ev.Action = "password=hunter2" }, errInvalidEvent},
		{"long action", func(ev *Event) { ev.Action = strings.Repeat("a", maxActionLen+1) }, errInvalidEvent},
		{"unicode action", func(ev *Event) { ev.Action = "sauvegarde.é" }, errInvalidEvent},
		{"missing outcome", func(ev *Event) { ev.Outcome = "" }, errInvalidEvent},
		{"unknown outcome", func(ev *Event) { ev.Outcome = "done" }, errInvalidEvent},
		{"sql database", func(ev *Event) { ev.Database = "SELECT * FROM pg_shadow" }, errInvalidEvent},
		{"space in operation", func(ev *Event) { ev.OperationID = "two words" }, errInvalidEvent},
		{"shell in node", func(ev *Event) { ev.Node = "db1;rm -rf /" }, errInvalidEvent},
		{"event id", func(ev *Event) { ev.EventID = "not a token" }, errInvalidEvent},
		{"timestamp", func(ev *Event) { ev.OccurredAt = "yesterday" }, errInvalidEvent},
		{"old timestamp", func(ev *Event) { ev.OccurredAt = "1960-01-01T00:00:00Z" }, errInvalidEvent},
		{"negative duration", func(ev *Event) { ev.DurationMS = -1 }, errInvalidEvent},
		{"huge duration", func(ev *Event) { ev.DurationMS = maxDurationMS + 1 }, errInvalidEvent},
		{"unknown actor", func(ev *Event) { ev.ActorKind = "human" }, errInvalidEvent},
		{"label without kind", func(ev *Event) { ev.ActorKind = "" }, errInvalidEvent},
		{"verified operator", func(ev *Event) { ev.ActorVerified = true }, errInvalidEvent},
		{"error detail", func(ev *Event) { ev.ErrorCode = "PG ERROR: deadlock" }, errInvalidEvent},
		{"newline version", func(ev *Event) { ev.NodeVersion = "1.2.3\nforged" }, errInvalidEvent},
		{"url boot id", func(ev *Event) { ev.BootID = "https://example.com/boot" }, errInvalidEvent},
		{"long request id", func(ev *Event) { ev.RequestID = strings.Repeat("r", maxTokenLen+1) }, errInvalidEvent},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ev := validEvent()
			tc.mutate(&ev)
			err := ValidateEvent(ev)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("ValidateEvent error = %v, want %v", err, tc.wantErr)
			}
			for _, canary := range []string{"pg_shadow", "example.com", "hunter2", "forged"} {
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("error echoed user data %q: %v", canary, err)
				}
			}
		})
	}
}

func TestEncodeEventRejectsOversizedRecord(t *testing.T) {
	ev := validEvent()
	ev.Server = strings.Repeat("a", maxEventBytes+1)
	if _, err := encodeEvent(ev); !errors.Is(err, errInvalidEvent) {
		t.Fatalf("encodeEvent error = %v, want errInvalidEvent", err)
	}
}

func TestValidateQueryNormalizes(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	nq, err := normalizeQuery(Query{Since: "24h", Limit: 0}, now)
	if err != nil {
		t.Fatalf("normalizeQuery: %v", err)
	}
	if got, want := nq.limit, DefaultLimit; got != want {
		t.Fatalf("limit = %d, want %d", got, want)
	}
	if got, want := nq.since, now.Add(-24*time.Hour); !got.Equal(want) {
		t.Fatalf("since = %v, want %v", got, want)
	}
	nq, err = normalizeQuery(Query{Limit: MaxLimit + 100}, now)
	if err != nil {
		t.Fatalf("normalizeQuery cap: %v", err)
	}
	if nq.limit != MaxLimit {
		t.Fatalf("limit = %d, want cap %d", nq.limit, MaxLimit)
	}
	nq, err = normalizeQuery(Query{Since: "2026-10-20T00:00:00Z", Until: "2026-10-21T00:00:00+02:00"}, now)
	if err != nil {
		t.Fatalf("normalizeQuery timestamps: %v", err)
	}
	if nq.since.Location() != time.UTC || nq.until.Location() != time.UTC {
		t.Fatalf("normalized times are not UTC")
	}
}

func TestValidateQueryRejectsInvalid(t *testing.T) {
	tests := []struct {
		name string
		q    Query
	}{
		{"negative limit", Query{Limit: -1}},
		{"bad since", Query{Since: "yesterday"}},
		{"zero duration", Query{Since: "0s"}},
		{"negative duration", Query{Since: "-1h"}},
		{"future window", Query{Since: "2026-10-21T00:00:00Z", Until: "2026-10-20T00:00:00Z"}},
		{"empty window", Query{Since: "2026-10-20T00:00:00Z", Until: "2026-10-20T00:00:00Z"}},
		{"space action", Query{Action: "backup full"}},
		{"unknown outcome", Query{Outcome: "done"}},
		{"sql database", Query{Database: "SELECT 1"}},
		{"garbage cursor", Query{Cursor: "c1.not-base64"}},
		{"foreign cursor", Query{Cursor: "other.abc"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateQuery(tc.q)
			if err == nil {
				t.Fatal("ValidateQuery accepted an invalid query")
			}
			if strings.Contains(err.Error(), tc.q.Cursor) && tc.q.Cursor != "" {
				t.Fatalf("error echoed cursor: %v", err)
			}
		})
	}
}
