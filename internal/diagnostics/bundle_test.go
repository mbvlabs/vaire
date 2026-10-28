package diagnostics

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
)

func validBundle() Bundle {
	events := []activity.Event{
		{
			Schema:         activity.Schema,
			EventID:        "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
			OccurredAt:     "2026-10-26T10:00:00Z",
			Sequence:       1,
			Server:         "prod",
			Node:           "db1",
			NodeInstanceID: "3f1c2b4a-0000-4000-8000-000000000001",
			BootID:         "3f1c2b4a-0000-4000-8000-000000000002",
			OperationID:    "op-01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
			RequestID:      "req-01J9ZK3Q6V3W8Y2XH5N7T1C4BE",
			JobID:          "01J9ZK3Q6V3W8Y2XH5N7T1C4BF",
			Source:         activity.SourceRPC,
			Action:         "backup.full",
			Step:           "backup",
			Database:       "app",
			Outcome:        activity.OutcomeSucceeded,
			DurationMS:     1200,
			ActorKind:      activity.ActorOperator,
			ActorLabel:     "alice",
			NodeVersion:    "1.0.0",
		},
		{
			Schema:      activity.Schema,
			EventID:     "01J9ZK3Q6V3W8Y2XH5N7T1C4BG",
			OccurredAt:  "2026-10-26T10:00:01Z",
			Sequence:    2,
			Server:      "prod",
			Node:        "db1",
			OperationID: "op-01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
			Source:      activity.SourceStep,
			Action:      "backup.full",
			Step:        "pgbackrest",
			Database:    "app",
			Outcome:     activity.OutcomeSucceeded,
			ActorKind:   activity.ActorSystem,
			NodeVersion: "1.0.0",
		},
	}
	return Bundle{
		Schema:    Schema,
		CreatedAt: "2026-10-26T11:00:01Z",
		Query: &activity.Query{
			Since:       "24h",
			OperationID: "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
			Database:    "app",
			Action:      "backup.full",
			Outcome:     activity.OutcomeSucceeded,
			Limit:       100,
		},
		Metadata: Metadata{
			Server:        "prod",
			Node:          "db1",
			NodeVersion:   "1.0.0",
			ClientVersion: "1.0.0",
			CollectedAt:   "2026-10-26T11:00:00Z",
		},
		Activity: activity.Page{
			Schema: activity.Schema,
			Events: events,
			Coverage: activity.Coverage{
				Node:            "db1",
				Available:       true,
				FirstRetainedAt: "2026-10-26T09:00:00Z",
				LastRetainedAt:  "2026-10-26T11:00:00Z",
				FirstReturnedAt: "2026-10-26T10:00:00Z",
				LastReturnedAt:  "2026-10-26T10:00:01Z",
				EventsReturned:  2,
				EventsScanned:   2,
				SegmentsScanned: 1,
				BytesScanned:    512,
			},
		},
	}
}

func TestBundleRoundTripUnderscoreDatabase(t *testing.T) {
	b := validBundle()
	b.Query.Database = "_app"
	for i := range b.Activity.Events {
		b.Activity.Events[i].Database = "_app"
	}
	path := filepath.Join(t.TempDir(), "incident.json")
	if err := Write(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, b) {
		t.Fatal("database correlation changed during bundle round trip")
	}
}

func missingHistoryBundle() Bundle {
	b := validBundle()
	b.Activity.Events = []activity.Event{}
	b.Activity.Coverage = activity.Coverage{
		Node:      "db1",
		Available: false,
		Partial:   true,
	}
	return b
}

func TestWriteReadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	want := validBundle()
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a test bundle path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte("\n")) || !bytes.HasPrefix(raw, []byte("{\n  \"schema\": \"vaire-incident/v1\",")) {
		t.Fatalf("bundle is not canonical typed JSON:\n%s", raw)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the bundle:\n got %+v\nwant %+v", got, want)
	}
}

func TestCanonicalWriteDropsNothingTyped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	b := validBundle()
	b.Activity.NextCursor = "c1.abc-def_ghi"
	b.Activity.Coverage.Truncated = true
	if err := Write(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Activity.NextCursor != "c1.abc-def_ghi" {
		t.Fatalf("next cursor = %q", got.Activity.NextCursor)
	}
	if !got.Activity.Coverage.Truncated || got.Activity.Coverage.Partial {
		t.Fatal("coverage flags were not preserved")
	}
}

func TestBundleQueryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	want := validBundle()
	if err := Write(path, want); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a test bundle path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"query"`)) {
		t.Fatal("written bundle lost the query provenance")
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Query == nil || !reflect.DeepEqual(*got.Query, *want.Query) {
		t.Fatalf("query changed: got %+v, want %+v", got.Query, want.Query)
	}
}

func TestOldBundleWithoutQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	b := validBundle()
	b.Query = nil
	if err := Write(path, b); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: a test bundle path.
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(`"query"`)) {
		t.Fatal("a metadata-only bundle gained a query field")
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Query != nil {
		t.Fatalf("old bundle query = %+v, want nil", got.Query)
	}
}

func TestEmptyQueryIsAllowed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	b := validBundle()
	b.Query = &activity.Query{}
	if err := Write(path, b); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Query == nil || *got.Query != (activity.Query{}) {
		t.Fatalf("empty query = %+v", got.Query)
	}
}

func TestReadRejectsUnknownQueryField(t *testing.T) {
	raw := marshalMap(t, validBundle())
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["query"].(map[string]any)["raw_sql"] = "select * from users"
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Read(writeRaw(t, data))
	if codeOf(t, err) != output.CodeBundleCorrupt || reasonOf(t, err) != "unknown_field" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "select *") {
		t.Fatalf("error echoed the query body: %v", err)
	}
}

func TestBundleRejectsInvalidQuery(t *testing.T) {
	cases := map[string]func(*activity.Query){
		"limit too large": func(q *activity.Query) { q.Limit = activity.MaxLimit + 1 },
		"negative limit":  func(q *activity.Query) { q.Limit = -1 },
		"long cursor":     func(q *activity.Query) { q.Cursor = strings.Repeat("A", maxQueryCursorBytes+1) },
		"bad cursor":      func(q *activity.Query) { q.Cursor = "abcd" },
		"sql database":    func(q *activity.Query) { q.Database = "app;drop table users" },
		"url operation":   func(q *activity.Query) { q.OperationID = "https://example.invalid/op" },
		"bad since":       func(q *activity.Query) { q.Since = "yesterday" },
		"bad outcome":     func(q *activity.Query) { q.Outcome = "ok" },
		"inverted window": func(q *activity.Query) {
			q.Since = "2026-10-26T12:00:00Z"
			q.Until = "2026-10-26T10:00:00Z"
		},
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			edit(b.Query)
			path := filepath.Join(t.TempDir(), "incident.json")
			err := Write(path, b)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "drop table") || strings.Contains(err.Error(), "example.invalid") {
				t.Fatalf("error echoed the rejected query value: %v", err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("an invalid query bundle was written")
			}
		})
	}
}

func TestReadNormalizesMissingEvents(t *testing.T) {
	for name, b := range map[string]Bundle{
		"empty page": func() Bundle {
			b := validBundle()
			b.Activity.Events = nil
			b.Activity.Coverage.EventsReturned = 0
			b.Activity.Coverage.EventsScanned = 0
			b.Activity.Coverage.FirstReturnedAt = ""
			b.Activity.Coverage.LastReturnedAt = ""
			return b
		}(),
		"missing history": func() Bundle {
			b := missingHistoryBundle()
			b.Activity.Events = nil
			return b
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(b)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(raw, []byte(`"events":null`)) {
				t.Fatalf("fixture is not a null events array: %s", raw)
			}
			got, err := Read(writeRaw(t, raw))
			if err != nil {
				t.Fatal(err)
			}
			if got.Activity.Events == nil || len(got.Activity.Events) != 0 {
				t.Fatalf("events were not normalized: %#v", got.Activity.Events)
			}
			canonical, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(canonical, []byte(`"events":[]`)) {
				t.Fatalf("canonical output kept a null array: %s", canonical)
			}
		})
	}
}

func TestValidatePage(t *testing.T) {
	if err := ValidatePage(validBundle().Activity); err != nil {
		t.Fatalf("valid page: %v", err)
	}
	cases := map[string]func(*activity.Page){
		"missing page":  func(p *activity.Page) { *p = activity.Page{} },
		"sql action":    func(p *activity.Page) { p.Events[0].Action = "drop table users" },
		"too long work": func(p *activity.Page) { p.Events[0].DurationMS = 10*365*24*60*60*1000 + 1 },
		"bad coverage":  func(p *activity.Page) { p.Coverage.Available = false },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			page := validBundle().Activity
			edit(&page)
			err := ValidatePage(page)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "drop table") || strings.Contains(err.Error(), "DROP TABLE") {
				t.Fatalf("error echoed the value: %v", err)
			}
		})
	}
}

func TestUTCTimestampLayoutsAreAccepted(t *testing.T) {
	for _, stamp := range []string{
		"2026-10-26T10:00:00Z",
		"2026-10-26T10:00:00.120Z",
		"2026-10-26T10:00:00.000000000Z",
	} {
		b := validBundle()
		b.Activity.Events[0].OccurredAt = stamp
		b.Activity.Coverage.FirstReturnedAt = stamp
		if err := Write(filepath.Join(t.TempDir(), "incident.json"), b); err != nil {
			t.Fatalf("UTC timestamp %s was rejected: %v", stamp, err)
		}
	}
}

func TestMissingHistoryIsAValidPartialBundle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	if err := Write(path, missingHistoryBundle()); err != nil {
		t.Fatal(err)
	}
	got, err := Read(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Activity.Coverage.Available || !got.Activity.Coverage.Partial || len(got.Activity.Events) != 0 {
		t.Fatalf("missing history changed: %+v", got.Activity.Coverage)
	}
}

func TestWriteRefusesToOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	if err := Write(path, validBundle()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path) //nolint:gosec // G304: a test bundle path.
	if err != nil {
		t.Fatal(err)
	}
	changed := validBundle()
	changed.Metadata.Node = "db2"
	err = Write(path, changed)
	if codeOf(t, err) != output.CodePrecondition || reasonOf(t, err) != "output_exists" {
		t.Fatalf("err = %v", err)
	}
	after, err := os.ReadFile(path) //nolint:gosec // G304: a test bundle path.
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("an existing bundle was modified")
	}
}

func TestWriteRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := Write(target, validBundle()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(target) //nolint:gosec // G304: a test bundle path.
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	err := Write(link, validBundle())
	if codeOf(t, err) != output.CodeValidation || reasonOf(t, err) != "symlink" {
		t.Fatalf("err = %v", err)
	}
	after, _ := os.ReadFile(target) //nolint:gosec // G304: a test bundle path.
	if !bytes.Equal(before, after) {
		t.Fatal("writing through a symlink changed its target")
	}
}

func TestWriteDoesNotCreateParentDirectories(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "missing")
	path := filepath.Join(parent, "incident.json")
	err := Write(path, validBundle())
	if codeOf(t, err) != output.CodeValidation || reasonOf(t, err) != "parent_missing" {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(parent); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("Write created the parent directory")
	}
}

func TestInvalidBundleIsNotWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "incident.json")
	b := validBundle()
	b.Schema = "vaire-incident/v2"
	err := Write(path, b)
	if codeOf(t, err) != output.CodeValidation || reasonOf(t, err) != "unsupported" {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("an invalid bundle was written")
	}
}

func TestReadRejectsUnknownFieldsAtEveryLevel(t *testing.T) {
	for name, edit := range map[string]func(map[string]any){
		"top": func(m map[string]any) { m["raw_config"] = "secret" },
		"metadata": func(m map[string]any) {
			m["metadata"].(map[string]any)["client_commit"] = "abc"
		},
		"activity": func(m map[string]any) {
			m["activity"].(map[string]any)["raw_logs"] = []any{"from users"}
		},
		"coverage": func(m map[string]any) {
			m["activity"].(map[string]any)["coverage"].(map[string]any)["error"] = "cannot read /etc/vaire"
		},
		"event": func(m map[string]any) {
			event := m["activity"].(map[string]any)["events"].([]any)[0].(map[string]any)
			event["details"] = map[string]any{"password": "hunter2"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw := marshalMap(t, validBundle())
			var m map[string]any
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			edit(m)
			data, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			path := writeRaw(t, data)
			_, err = Read(path)
			if codeOf(t, err) != output.CodeBundleCorrupt || reasonOf(t, err) != "unknown_field" {
				t.Fatalf("err = %v", err)
			}
			if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "raw_logs") {
				t.Fatalf("error echoed the rejected content: %v", err)
			}
		})
	}
}

func TestReadRejectsCanariesInKnownFields(t *testing.T) {
	cases := map[string]func(*Bundle){
		"sql in action": func(b *Bundle) { b.Activity.Events[0].Action = "select * from users" },
		"key in actor label": func(b *Bundle) {
			b.Activity.Events[0].ActorLabel = "https://example.invalid/?token=sk_live_abcdef"
		},
		"sql in actor label": func(b *Bundle) {
			b.Activity.Events[0].ActorLabel = "drop table users"
		},
		"control chars in database": func(b *Bundle) { b.Activity.Events[0].Database = "app\nDROP TABLE users" },
		"url in operation id":       func(b *Bundle) { b.Activity.Events[0].OperationID = "postgres://u:p@host/db" },
		"multiline reason":          func(b *Bundle) { b.Activity.Events[0].ReasonCode = "line1\nline2" },
		"sql in metadata server":    func(b *Bundle) { b.Metadata.Server = "prod; rm -rf /" },
		"url in version":            func(b *Bundle) { b.Metadata.NodeVersion = "1.0.0 https://example.invalid" },
		"control in coverage node":  func(b *Bundle) { b.Activity.Coverage.Node = "db1\x00" },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			edit(&b)
			path := filepath.Join(t.TempDir(), "incident.json")
			err := Write(path, b)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("a bundle with a rejected field was written")
			}
		})
	}
}

func TestUnknownEnumsAreRejected(t *testing.T) {
	for name, edit := range map[string]func(*activity.Event){
		"source":     func(ev *activity.Event) { ev.Source = "shell" },
		"outcome":    func(ev *activity.Event) { ev.Outcome = "ok" },
		"actor kind": func(ev *activity.Event) { ev.ActorKind = "root" },
	} {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			edit(&b.Activity.Events[0])
			err := Write(filepath.Join(t.TempDir(), "incident.json"), b)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestCoverageInvariants(t *testing.T) {
	cases := map[string]func(*Bundle){
		"returned count": func(b *Bundle) { b.Activity.Coverage.EventsReturned = 1 },
		"scanned less than returned": func(b *Bundle) {
			b.Activity.Coverage.EventsScanned = 1
		},
		"unavailable with events": func(b *Bundle) {
			b.Activity.Coverage.Available = false
			b.Activity.Coverage.Partial = true
		},
		"partial required for cursor expiry": func(b *Bundle) {
			b.Activity.Coverage.CursorExpired = true
		},
		"partial required for loss": func(b *Bundle) {
			b.Activity.Coverage.MalformedEvents = 1
			b.Activity.Coverage.LossDetected = true
		},
		"loss flag required": func(b *Bundle) {
			b.Activity.Coverage.MalformedEvents = 1
			b.Activity.Coverage.Partial = true
		},
		"negative bytes": func(b *Bundle) { b.Activity.Coverage.BytesScanned = -1 },
		"limit without events": func(b *Bundle) {
			b.Activity.Events = []activity.Event{}
			b.Activity.Coverage.EventsReturned = 0
			b.Activity.Coverage.EventsScanned = 0
			b.Activity.Coverage.FirstReturnedAt = ""
			b.Activity.Coverage.LastReturnedAt = ""
			b.Activity.Coverage.LimitReached = true
		},
		"first returned mismatch": func(b *Bundle) {
			b.Activity.Coverage.FirstReturnedAt = "2026-10-26T10:00:02Z"
		},
		"last returned missing": func(b *Bundle) { b.Activity.Coverage.LastReturnedAt = "" },
		"returned without events": func(b *Bundle) {
			b.Activity.Events = []activity.Event{}
			b.Activity.Coverage.EventsReturned = 0
			b.Activity.Coverage.EventsScanned = 0
		},
		"retained inverted": func(b *Bundle) {
			b.Activity.Coverage.FirstRetainedAt = "2026-10-26T12:00:00Z"
		},
		"bad cursor": func(b *Bundle) {
			b.Activity.Coverage.Truncated = true
			b.Activity.Coverage.Partial = true
			b.Activity.NextCursor = "has space"
		},
		"non-utc timestamp": func(b *Bundle) {
			b.Activity.Events[0].OccurredAt = "2026-10-26T12:00:00+02:00"
		},
		"bad event schema":   func(b *Bundle) { b.Activity.Events[0].Schema = "activity/v2" },
		"missing sequence":   func(b *Bundle) { b.Activity.Events[0].Sequence = 0 },
		"label without kind": func(b *Bundle) { b.Activity.Events[0].ActorKind = "" },
		"verified operator":  func(b *Bundle) { b.Activity.Events[0].ActorVerified = true },
		"negative duration":  func(b *Bundle) { b.Activity.Events[0].DurationMS = -1 },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			b := validBundle()
			edit(&b)
			path := filepath.Join(t.TempDir(), "incident.json")
			err := Write(path, b)
			if codeOf(t, err) != output.CodeValidation {
				t.Fatalf("err = %v", err)
			}
			if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatal("an invalid bundle was written")
			}
		})
	}
}

func TestReadRejectsTrailingData(t *testing.T) {
	raw := marshalMap(t, validBundle())
	for name, data := range map[string][]byte{
		"second object": append(append([]byte(nil), raw...), []byte(`{}`)...),
		"garbage":       append(append([]byte(nil), raw...), []byte("not json")...),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Read(writeRaw(t, data))
			if codeOf(t, err) != output.CodeBundleCorrupt || reasonOf(t, err) != "trailing_data" {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestReadRejectsTruncatedJSON(t *testing.T) {
	raw := marshalMap(t, validBundle())
	_, err := Read(writeRaw(t, raw[:len(raw)/2]))
	if codeOf(t, err) != output.CodeBundleCorrupt || reasonOf(t, err) != "decode_failed" {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsOversizeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.json")
	data := bytes.Repeat([]byte(" "), int(MaxBundleBytes)+1)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Read(path)
	if codeOf(t, err) != output.CodeValidation || reasonOf(t, err) != "bundle_too_large" {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsSymlinkAndNonOwnerOnly(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	if err := Write(target, validBundle()); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link); codeOf(t, err) != output.CodeValidation || reasonOf(t, err) != "symlink" {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Chmod(target, 0o644); err != nil { //nolint:gosec // G302: deliberately tests a non-owner-only bundle.
		t.Fatal(err)
	}
	if _, err := Read(target); codeOf(t, err) != output.CodePrecondition || reasonOf(t, err) != "not_owner_only" {
		t.Fatalf("permissions: %v", err)
	}
}

func TestReadMissingFile(t *testing.T) {
	_, err := Read(filepath.Join(t.TempDir(), "absent.json"))
	if codeOf(t, err) != output.CodeBundleNotFound {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRejectsDirectory(t *testing.T) {
	_, err := Read(t.TempDir())
	if codeOf(t, err) != output.CodeBundleCorrupt || reasonOf(t, err) != "not_regular_file" {
		t.Fatalf("err = %v", err)
	}
}

func writeRaw(t *testing.T, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func marshalMap(t *testing.T, b Bundle) []byte {
	t.Helper()
	data, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	return cliErr.Code
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}
