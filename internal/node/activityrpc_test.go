package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/buildinfo"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/rpc"
)

func withActivityStore(t *testing.T, store activityStore) {
	t.Helper()
	old := activityStoreFactory
	activityStoreFactory = func() (activityStore, error) { return store, nil }
	t.Cleanup(func() { activityStoreFactory = old })
}

func activityEnv(t *testing.T, params string) *Env {
	t.Helper()
	return &Env{Req: rpc.Request{Schema: rpc.Schema, Method: "activity.list", Params: json.RawMessage(params)}}
}

func TestActivityListQuery(t *testing.T) {
	store := &fakeActivityStore{page: activity.Page{Schema: activity.Schema, Events: []activity.Event{}}}
	withActivityStore(t, store)
	env := activityEnv(t, `{"since":"24h","operation_id":"01m4x0me0nbq95hnndt213bgh9","outcome":"failed","action":"backup.now","limit":9999}`)
	res, err := handleActivityList(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	page, ok := res.Data.(activity.Page)
	if !ok || page.Schema != activity.Schema {
		t.Fatalf("result = %#v", res.Data)
	}
	q := store.query
	if q.OperationID != testOperationID {
		t.Fatalf("operation_id = %q, want normalized %q", q.OperationID, testOperationID)
	}
	if q.Limit != activity.MaxLimit {
		t.Fatalf("limit = %d, want cap %d", q.Limit, activity.MaxLimit)
	}
	if q.Since != "24h" || q.Outcome != "failed" || q.Action != "backup.now" {
		t.Fatalf("query changed = %+v", q)
	}
	if len(store.recorded()) != 0 {
		t.Fatal("listing activity wrote a history event")
	}
}

func TestActivityListRejectsUnknownAndUnsafeParams(t *testing.T) {
	store := &fakeActivityStore{}
	withActivityStore(t, store)
	cases := []string{
		`{"bogus":true}`,
		`{"outcome":"maybe"}`,
		`{"limit":-1}`,
		`{"since":"yesterday"}`,
		`{"since":"-24h"}`,
		`{"since":"999999h"}`,
		`{"since":"2026-10-26T12:00:00Z","until":"2026-10-26T11:00:00Z"}`, `{"operation_id":"not-a-ulid"}`,
		`{"cursor":"` + strings.Repeat("x", 1001) + `"}`,
		`{"database":"bad name"}`,
		`{"action":"SQL TEXT"}`,
	}
	for _, params := range cases {
		_, err := handleActivityList(t.Context(), activityEnv(t, params))
		if err == nil {
			t.Fatalf("params %s were accepted", params)
		}
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || cliErr.Code != output.CodeUsage {
			t.Fatalf("params %s error = %v, want usage_error", params, err)
		}
	}
	if len(store.recorded()) != 0 {
		t.Fatal("a rejected query wrote a history event")
	}
}

func TestActivityShowRequiresExactlyOneID(t *testing.T) {
	store := &fakeActivityStore{page: activity.Page{Schema: activity.Schema, Events: []activity.Event{}}}
	withActivityStore(t, store)

	for _, params := range []string{`{}`, `{"operation_id":"` + testOperationID + `","request_id":"` + testRequestID + `"}`} {
		if _, err := handleActivityShow(t.Context(), activityEnv(t, params)); err == nil {
			t.Fatalf("show accepted %s", params)
		}
	}

	res, err := handleActivityShow(t.Context(), activityEnv(t, `{"job_id":"`+testJobID+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.Data.(activity.Page); !ok {
		t.Fatalf("result = %#v", res.Data)
	}
	if store.query.JobID != testJobID || store.query.OperationID != "" || store.query.RequestID != "" {
		t.Fatalf("query = %+v", store.query)
	}
	if store.query.Limit != activityShowDefaultLimit {
		t.Fatalf("limit = %d, want bounded default %d", store.query.Limit, activityShowDefaultLimit)
	}

	if _, err := handleActivityShow(t.Context(), activityEnv(t, `{"operation_id":"bad"}`)); err == nil {
		t.Fatal("show accepted a malformed id")
	}
	if len(store.recorded()) != 0 {
		t.Fatal("show wrote a history event")
	}
}

func TestActivityHealthAndReadErrors(t *testing.T) {
	store := &fakeActivityStore{health: activity.Health{Dir: "/var/log/vaire/activity", Exists: true, Readable: true}}
	withActivityStore(t, store)
	res, err := handleActivityHealth(t.Context(), activityEnv(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	health, ok := res.Data.(activity.Health)
	if !ok || !health.Exists || !health.Readable {
		t.Fatalf("health = %#v", res.Data)
	}
	if len(store.recorded()) != 0 {
		t.Fatal("health wrote a history event")
	}

	if _, err := handleActivityHealth(t.Context(), activityEnv(t, `{"bogus":1}`)); err == nil {
		t.Fatal("health accepted an unknown param")
	}

	const storeText = "secret path /var/log/vaire/activity said no"
	store.healthErr = errors.New(storeText)
	_, err = handleActivityHealth(t.Context(), activityEnv(t, `{}`))
	if err == nil {
		t.Fatal("health hid a store error")
	}
	raw, _ := json.Marshal(output.Fail(nil, err))
	if strings.Contains(string(raw), storeText) {
		t.Fatalf("health error leaked store text: %s", raw)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodePrecondition {
		t.Fatalf("health error = %v", err)
	}
	if details, ok := cliErr.Details.(map[string]any); !ok || details["reason"] != "activity_unavailable" {
		t.Fatalf("health error details = %#v", cliErr.Details)
	}

	store.queryErr = errors.New(storeText)
	if _, err := handleActivityList(t.Context(), activityEnv(t, `{}`)); err == nil {
		t.Fatal("list hid a store error")
	}
}

func TestActivityQueryErrorsAreSafeUsageErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason string
	}{
		{activity.ValidateQuery(activity.Query{Action: "bad action"}), activity.QueryReasonInvalid},
		{activity.ValidateQuery(activity.Query{Limit: -1}), activity.QueryReasonOutOfRange},
	} {
		err := activityQueryError(tc.err)
		var cliErr *output.CLIError
		if !errors.As(err, &cliErr) || cliErr.Code != output.CodeUsage || cliErr.ExitCode != output.ExitUsage {
			t.Fatalf("query error = %v, want usage_error", err)
		}
		details, ok := cliErr.Details.(map[string]any)
		if !ok || details["reason"] != tc.reason {
			t.Fatalf("query reason = %#v, want %q", cliErr.Details, tc.reason)
		}
		raw, _ := json.Marshal(output.Fail(nil, err))
		if strings.Contains(string(raw), "bad action") || strings.Contains(string(raw), "negative") {
			t.Fatalf("query error echoed a rejected filter: %s", raw)
		}
	}
}

func TestActivityCursorMismatchIsNotReportedAsUnreadableStore(t *testing.T) {
	store := &activity.Store{
		Dir:          filepath.Join(t.TempDir(), "activity"),
		MaxBytes:     64 << 10,
		SegmentBytes: 8 << 10,
		Retention:    time.Hour,
	}
	for i := 0; i < 2; i++ {
		if _, err := store.Append(t.Context(), activity.Event{
			Source: activity.SourceRPC, Action: "plan", Outcome: activity.OutcomeStarted,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := store.Query(t.Context(), activity.Query{Limit: 1})
	if err != nil || first.NextCursor == "" {
		t.Fatalf("first page: %v cursor=%q", err, first.NextCursor)
	}
	_, mismatch := store.Query(t.Context(), activity.Query{
		Limit: 1, Action: "backup.now", Cursor: first.NextCursor,
	})
	if mismatch == nil {
		t.Fatal("a cursor with different filters was accepted")
	}
	err = activityQueryError(mismatch)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || cliErr.Code != output.CodeUsage {
		t.Fatalf("cursor mismatch = %v, want usage_error", err)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != activity.QueryReasonCursorMismatch {
		t.Fatalf("cursor mismatch reason = %#v", cliErr.Details)
	}

	// Exercise the production handler path: queryActivity must not flatten a
	// caller-caused cursor mismatch into activity_unavailable.
	withActivityStore(t, store)
	_, err = handleActivityList(t.Context(), activityEnv(t,
		`{"limit":1,"action":"backup.now","cursor":"`+first.NextCursor+`"}`))
	if err == nil {
		t.Fatal("handler accepted a cursor with different filters")
	}
	var handlerErr *output.CLIError
	if !errors.As(err, &handlerErr) || handlerErr.Code != output.CodeUsage {
		t.Fatalf("handler cursor mismatch = %v, want usage_error", err)
	}
}

func TestActivityQueriesDoNotCreateStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "activity")
	withActivityStore(t, &activity.Store{
		Dir:          dir,
		MaxBytes:     1 << 20,
		SegmentBytes: 64 << 10,
		Retention:    time.Hour,
	})
	res, err := handleActivityList(t.Context(), activityEnv(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	page, ok := res.Data.(activity.Page)
	if !ok || page.Coverage.Available {
		t.Fatalf("absent store page = %#v, want unavailable coverage", res.Data)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("activity list created the store: %v", err)
	}
	res, err = handleActivityHealth(t.Context(), activityEnv(t, `{}`))
	if err != nil {
		t.Fatal(err)
	}
	if health, ok := res.Data.(activity.Health); !ok || health.Exists {
		t.Fatalf("absent store health = %#v", res.Data)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("activity health created the store: %v", err)
	}
}

func withServeActivity(t *testing.T, store *fakeActivityStore) {
	t.Helper()
	oldStore := activityStoreFactory
	oldRecorder := activityRecorderFactory
	oldRoot := serveRunsAsRoot
	activityStoreFactory = func() (activityStore, error) { return store, nil }
	activityRecorderFactory = func() *activityRecorder {
		rec, _ := testRecorder(t, store)
		return rec
	}
	serveRunsAsRoot = func() bool { return true }
	t.Cleanup(func() {
		activityStoreFactory = oldStore
		activityRecorderFactory = oldRecorder
		serveRunsAsRoot = oldRoot
	})
}

func serveRequest(t *testing.T, req rpc.Request) (string, int) {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := Serve(bytes.NewReader(append(raw, '\n')), &out)
	return out.String(), code
}

func matchingBuildRequest(method string) rpc.Request {
	return rpc.Request{
		Schema:          rpc.Schema,
		RequestID:       testRequestID,
		OperationID:     testOperationID,
		ParentRequestID: testParentID,
		Method:          method,
		Operator:        "alice",
		ClientVersion:   buildinfo.Version,
		ClientCommit:    buildinfo.Commit,
		ClientChannel:   buildinfo.Channel,
	}
}

func TestServeRecordsEarlyRefusals(t *testing.T) {
	t.Run("unknown method", func(t *testing.T) {
		store := &fakeActivityStore{}
		withServeActivity(t, store)
		out, code := serveRequest(t, matchingBuildRequest("nope.nope"))
		if code == 0 || !strings.Contains(out, "unknown method") {
			t.Fatalf("output = %s, code = %d", out, code)
		}
		events := store.recorded()
		if len(events) != 1 {
			t.Fatalf("recorded %d events, want one refusal: %+v", len(events), events)
		}
		ev := events[0]
		if ev.Outcome != activity.OutcomeRefused || ev.Action != "node.rpc" ||
			ev.ReasonCode != "unknown_method" || ev.ErrorCode != output.CodeUsage {
			t.Fatalf("refusal = %+v", ev)
		}
	})

	t.Run("version mismatch", func(t *testing.T) {
		store := &fakeActivityStore{}
		withServeActivity(t, store)
		req := matchingBuildRequest("status")
		req.ClientVersion = "old-build"
		out, code := serveRequest(t, req)
		if code == 0 || !strings.Contains(out, "does not match node build") {
			t.Fatalf("output = %s, code = %d", out, code)
		}
		events := store.recorded()
		if len(events) != 1 || events[0].ReasonCode != "version_mismatch" ||
			events[0].ErrorCode != output.CodeNodeVersionMismatch {
			t.Fatalf("refusal = %+v", events)
		}
	})

	t.Run("unknown activity method is still audited", func(t *testing.T) {
		store := &fakeActivityStore{}
		withServeActivity(t, store)
		out, code := serveRequest(t, matchingBuildRequest("activity.foo"))
		if code == 0 || !strings.Contains(out, "unknown method") {
			t.Fatalf("output = %s, code = %d", out, code)
		}
		events := store.recorded()
		if len(events) != 1 {
			t.Fatalf("recorded %d events, want one refusal: %+v", len(events), events)
		}
		ev := events[0]
		if ev.Action != "node.rpc" || ev.ReasonCode != "unknown_method" || ev.Outcome != activity.OutcomeRefused {
			t.Fatalf("unknown activity refusal = %+v, want fixed node.rpc action", ev)
		}
		if strings.Contains(ev.Action, "activity.foo") {
			t.Fatalf("unknown method name escaped into action: %+v", ev)
		}
	})

	t.Run("decode failure", func(t *testing.T) {
		store := &fakeActivityStore{}
		withServeActivity(t, store)
		var out bytes.Buffer
		code := Serve(strings.NewReader("{not json"), &out)
		if code == 0 {
			t.Fatalf("bad request exited 0: %s", out.String())
		}
		events := store.recorded()
		if len(events) != 1 || events[0].Action != "node.rpc" ||
			events[0].ReasonCode != "request_rejected" {
			t.Fatalf("refusal = %+v", events)
		}
	})
}

func TestServeDoesNotRecordActivityQueries(t *testing.T) {
	store := &fakeActivityStore{page: activity.Page{Schema: activity.Schema, Events: []activity.Event{}}}
	withServeActivity(t, store)
	out, code := serveRequest(t, matchingBuildRequest("activity.list"))
	if code != 0 {
		t.Fatalf("activity.list failed: %s", out)
	}
	if len(store.recorded()) != 0 {
		t.Fatalf("activity.list recorded itself: %+v", store.recorded())
	}
	out, code = serveRequest(t, matchingBuildRequest("activity.health"))
	if code != 0 {
		t.Fatalf("activity.health failed: %s", out)
	}
	if len(store.recorded()) != 0 {
		t.Fatalf("activity.health recorded itself: %+v", store.recorded())
	}
}
