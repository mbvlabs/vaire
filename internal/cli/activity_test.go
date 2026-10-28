package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/diagnostics"
	"github.com/mbvlabs/vaire/internal/remote"
)

func TestActivityCommandsInTree(t *testing.T) {
	if report := checkCommandMetadata(NewRoot()); !report.OK {
		t.Fatalf("metadata: %v", report.Failures)
	}
	if cmd := commandAt(t, "activity list"); cmd == nil {
		t.Fatal("activity list is missing")
	} else {
		for _, name := range []string{"since", "until", "db", "action", "outcome", "limit", "cursor", "operation", "request", "job"} {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("activity list lacks --%s", name)
			}
		}
	}
	if cmd := commandAt(t, "activity show"); cmd == nil {
		t.Fatal("activity show is missing")
	} else {
		for _, name := range []string{"operation", "request", "job"} {
			if cmd.Flags().Lookup(name) == nil {
				t.Errorf("activity show lacks --%s", name)
			}
		}
	}
	if cmd := commandAt(t, "activity health"); cmd == nil {
		t.Fatal("activity health is missing")
	}
}

func TestActivityFilterFlagsBuildAQuery(t *testing.T) {
	cmd := commandAt(t, "activity list",
		"--since", "2026-10-26T00:00:00Z",
		"--until", "2026-10-26T12:00:00Z",
		"--operation", "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
		"--request", "01J9ZK3Q6V3W8Y2XH5N7T1C4BE",
		"--job", "01J9ZK3Q6V3W8Y2XH5N7T1C4BF",
		"--db", "app",
		"--action", "backup.full",
		"--outcome", "failed",
		"--limit", "250")
	q, err := activityQueryFromFlags(cmd)
	if err != nil {
		t.Fatal(err)
	}
	want := activity.Query{
		Since:       "2026-10-26T00:00:00Z",
		Until:       "2026-10-26T12:00:00Z",
		OperationID: "01J9ZK3Q6V3W8Y2XH5N7T1C4BD",
		RequestID:   "01J9ZK3Q6V3W8Y2XH5N7T1C4BE",
		JobID:       "01J9ZK3Q6V3W8Y2XH5N7T1C4BF",
		Database:    "app",
		Action:      "backup.full",
		Outcome:     "failed",
		Limit:       250,
	}
	if q != want {
		t.Fatalf("query = %+v, want %+v", q, want)
	}
}

func TestActivityFilterAllowsUnderscoreDatabase(t *testing.T) {
	for _, path := range []string{"activity list", "diagnostics collect"} {
		q, err := activityQueryFromFlags(commandAt(t, path, "--db", "_app"))
		if err != nil || q.Database != "_app" {
			t.Fatalf("%s query = %+v, err = %v", path, q, err)
		}
	}
}

func TestActivityListValidatesBeforeRemote(t *testing.T) {
	for name, tc := range map[string]struct {
		args   []string
		reason string
	}{
		"limit too large":   {[]string{"--limit", "501"}, "out_of_range"},
		"limit zero":        {[]string{"--limit", "0"}, "out_of_range"},
		"unknown outcome":   {[]string{"--outcome", "ok"}, "not_allowed"},
		"bad since":         {[]string{"--since", "yesterday"}, "not_a_time"},
		"bad until":         {[]string{"--until", "soon"}, "not_a_time"},
		"inverted window":   {[]string{"--since", "2026-10-26T12:00:00Z", "--until", "2026-10-26T12:00:00Z"}, "not_after_since"},
		"inverted lookback": {[]string{"--since", "1h", "--until", "2h"}, "not_after_since"},
		"mixed inverted":    {[]string{"--since", "9999-01-01T00:00:00Z", "--until", "1h"}, "invalid"},
		"bad operation id":  {[]string{"--operation", "bad id"}, "not_allowed"},
		"bad database":      {[]string{"--db", "app;drop"}, "not_allowed"},
		"bad action":        {[]string{"--action", "select * from x"}, "not_allowed"},
		"bad cursor":        {[]string{"--cursor", "has space"}, "not_allowed"},
		"invalid cursor":    {[]string{"--cursor", "abcd"}, "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			err := runActivityList(commandAt(t, "activity list", tc.args...), nil)
			if cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != tc.reason {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestActivityShowRequiresOneBoundedSelector(t *testing.T) {
	if _, err := activityShowQuery(commandAt(t, "activity show"), nil); cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "required" {
		t.Fatalf("no selector: %v", err)
	}
	if _, err := activityShowQuery(commandAt(t, "activity show"), []string{"one", "two"}); cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "too_many" {
		t.Fatalf("two arguments: %v", err)
	}
	if _, err := activityShowQuery(commandAt(t, "activity show"), []string{"not an id"}); cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "not_allowed" {
		t.Fatalf("bad positional: %v", err)
	}
	if _, err := activityShowQuery(commandAt(t, "activity show", "--request", "not an id"), nil); cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "not_allowed" {
		t.Fatalf("bad request: %v", err)
	}
	if _, err := activityShowQuery(commandAt(t, "activity show", "--operation", "01J9ZK3Q6V3W8Y2XH5N7T1C4BD"), []string{"01J9ZK3Q6V3W8Y2XH5N7T1C4BE"}); cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "too_many" {
		t.Fatalf("conflicting selectors: %v", err)
	}
	q, err := activityShowQuery(commandAt(t, "activity show", "--job", "01j9zk3q6v3w8y2xh5n7t1c4bf"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if q.JobID != "01J9ZK3Q6V3W8Y2XH5N7T1C4BF" || q.Limit != activity.MaxLimit {
		t.Fatalf("job query = %+v", q)
	}
	q, err = activityShowQuery(commandAt(t, "activity show"), []string{"01j9zk3q6v3w8y2xh5n7t1c4bd"})
	if err != nil || q.OperationID != "01J9ZK3Q6V3W8Y2XH5N7T1C4BD" {
		t.Fatalf("operation query = %+v (%v)", q, err)
	}
}

func TestActivityNextKeepsCursorFilters(t *testing.T) {
	q := activity.Query{Since: "24h", Database: "app", Limit: 250, Outcome: "failed"}
	cursor := "c1." + strings.Repeat("a", 300)
	page := activity.Page{NextCursor: cursor}
	next := activityNext(page, q, "prod")
	if len(next) != 1 {
		t.Fatalf("next = %v", next)
	}
	for _, want := range []string{"--context prod", "--since 24h", "--db app", "--limit 250", "--outcome failed", "--cursor " + cursor} {
		if !strings.Contains(next[0], want) {
			t.Errorf("next command %q lacks %q", next[0], want)
		}
	}
	if activityNext(activity.Page{}, q, "prod") != nil {
		t.Fatal("a page without a cursor offered a next step")
	}
}

func TestDecodeNodeDataRejectsUnknownFieldsAndTrailingJSON(t *testing.T) {
	page := activity.Page{
		Schema:   activity.Schema,
		Events:   []activity.Event{},
		Coverage: activity.Coverage{Available: true},
	}
	raw, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeNodeData[activity.Page](remote.Reply{Data: raw}, "activity page"); err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	generic["raw_logs"] = []any{"line from a hostile node"}
	withUnknown, _ := json.Marshal(generic)
	_, err = decodeNodeData[activity.Page](remote.Reply{Data: withUnknown}, "activity page")
	if cliCodeOf(t, err) != output.CodeValidation || cliReasonOf(t, err) != "decode_failed" {
		t.Fatalf("unknown field: %v", err)
	}
	trailing := append(append([]byte(nil), raw...), []byte(`{}`)...)
	_, err = decodeNodeData[activity.Page](remote.Reply{Data: trailing}, "activity page")
	if cliCodeOf(t, err) != output.CodeValidation || cliReasonOf(t, err) != "trailing_data" {
		t.Fatalf("trailing data: %v", err)
	}
	_, err = decodeNodeData[activity.Page](remote.Reply{}, "activity page")
	if cliCodeOf(t, err) != output.CodeValidation || cliReasonOf(t, err) != "empty_reply" {
		t.Fatalf("empty reply: %v", err)
	}
}

func TestActivityTimelineDoesNotInjectControlText(t *testing.T) {
	page := activity.Page{
		Schema: activity.Schema,
		Events: []activity.Event{{
			Schema:      activity.Schema,
			EventID:     "01J9ZK",
			OccurredAt:  "2026-10-26T10:00:00Z",
			Source:      activity.SourceRPC,
			Action:      "backup.full",
			Outcome:     activity.OutcomeFailed,
			Database:    "app\nDROP TABLE users",
			ActorKind:   activity.ActorOperator,
			ActorLabel:  "alice\x1b[31mred",
			NodeVersion: "1.0.0",
		}},
		Coverage: activity.Coverage{
			Node:            "db1",
			Available:       true,
			FirstReturnedAt: "2026-10-26T10:00:00Z",
			LastReturnedAt:  "2026-10-26T10:00:00Z",
			EventsReturned:  1,
			EventsScanned:   1,
		},
	}
	message := activityPageMessage(page)
	if strings.Contains(message, "\x1b") || strings.Contains(message, "\nDROP") || strings.Contains(message, "app\nDROP") {
		t.Fatalf("human timeline kept control text: %q", message)
	}
	if !strings.Contains(message, "backup.full") || !strings.Contains(message, "claimed") {
		t.Fatalf("timeline lost typed fields: %q", message)
	}
}

func TestMissingActivityEvidenceIsNotACleanResult(t *testing.T) {
	page := activity.Page{
		Schema:   activity.Schema,
		Events:   []activity.Event{},
		Coverage: activity.Coverage{Node: "db1", Available: false, Partial: true},
	}
	message := activityPageMessage(page)
	if !strings.Contains(message, "unavailable") || strings.Contains(message, "no matching activity events") {
		t.Fatalf("message = %q", message)
	}
	warnings := activityCoverageWarnings(page.Coverage)
	if len(warnings) == 0 || warnings[0].Code != "activity_unavailable" {
		t.Fatalf("warnings = %+v", warnings)
	}
}

func TestActivityCoverageWarningsAreTyped(t *testing.T) {
	warnings := activityCoverageWarnings(activity.Coverage{
		Node:              "db1",
		Available:         true,
		Partial:           true,
		Truncated:         true,
		CursorExpired:     true,
		LossDetected:      true,
		MalformedSegments: 1,
	})
	codes := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		codes = append(codes, warning.Code)
	}
	for _, want := range []string{"activity_partial", "activity_truncated", "activity_cursor_expired", "activity_loss_detected"} {
		if !containsString(codes, want) {
			t.Errorf("missing warning %s in %v", want, codes)
		}
	}
}

func cliCodeOf(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	return cliErr.Code
}

func cliReasonOf(t *testing.T, err error) string {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("got %v, want a CLIError", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	reason, _ := details["reason"].(string)
	return reason
}

func TestActivityCursorRoundTripThroughStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &activity.Store{Dir: dir}
	now := time.Now().UTC().Truncate(time.Second)
	since := now.Add(-time.Hour).Format(time.RFC3339)
	until := now.Add(time.Hour).Format(time.RFC3339)
	operation := "01J9ZK3Q6V3W8Y2XH5N7T1C4BD"
	request := "01J9ZK3Q6V3W8Y2XH5N7T1C4BE"
	job := "01J9ZK3Q6V3W8Y2XH5N7T1C4BF"
	database := strings.Repeat("d", 64)
	action := strings.Repeat("a", 64)
	event := activity.Event{
		OccurredAt:  now.Format(time.RFC3339),
		OperationID: operation,
		RequestID:   request,
		JobID:       job,
		Source:      activity.SourceRPC,
		Action:      action,
		Database:    database,
		Outcome:     activity.OutcomeSucceeded,
		ActorKind:   activity.ActorSystem,
		NodeVersion: "dev",
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Append(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.Query(context.Background(), activity.Query{
		Since:       since,
		Until:       until,
		OperationID: operation,
		RequestID:   request,
		JobID:       job,
		Database:    database,
		Action:      action,
		Outcome:     activity.OutcomeSucceeded,
		Limit:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor == "" {
		t.Fatal("the store did not generate a cursor")
	}
	cmd := commandAt(t, "activity list",
		"--since", since,
		"--until", until,
		"--operation", operation,
		"--request", request,
		"--job", job,
		"--db", database,
		"--action", action,
		"--outcome", activity.OutcomeSucceeded,
		"--limit", "1",
		"--cursor", page.NextCursor)
	q, err := activityQueryFromFlags(cmd)
	if err != nil {
		t.Fatalf("CLI rejected a store cursor: %v", err)
	}
	if q.Cursor != page.NextCursor || q.Limit != 1 {
		t.Fatalf("continuation query = %+v", q)
	}
}

func TestActivityCursorBoundIsTheStoreBound(t *testing.T) {
	// A syntactically valid cursor beyond the old 1000-byte cap must reach the
	// store validator (which rejects this fake payload as invalid) rather than
	// being refused by the CLI's old, too-small bound.
	fake := "c1." + strings.Repeat("A", 1500)
	err := runActivityList(commandAt(t, "activity list", "--cursor", fake), nil)
	if cliCodeOf(t, err) != output.CodeUsage || cliReasonOf(t, err) != "invalid" {
		t.Fatalf("a long cursor was refused as %v", err)
	}
}

func TestDecodeNodeDataRejectsOversizeReply(t *testing.T) {
	data := bytes.Repeat([]byte(" "), int(diagnostics.MaxBundleBytes)+1)
	_, err := decodeNodeData[activity.Page](remote.Reply{Data: data}, "activity page")
	if cliCodeOf(t, err) != output.CodeValidation || cliReasonOf(t, err) != "reply_too_large" {
		t.Fatalf("err = %v", err)
	}
}
