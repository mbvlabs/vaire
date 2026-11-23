package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/secrets"
)

const (
	testOperationID = "01M4X0ME0NBQ95HNNDT213BGH9"
	testRequestID   = "01M4X0ME0NBQ95HNNDT213BGHA"
	testParentID    = "01M4X0ME0NBQ95HNNDT213BGHB"
	testJobID       = "01M4X0ME0NBQ95HNNDT213BGHC"
)

func TestManagedActivityUsesAuthenticatedIdentity(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	rec.identity = &rpc.WhoamiReply{IdentityKind: rpc.IdentityVerifiedDevice,
		OperatorID: "op-1", Operator: "alice", DeviceID: "dev-1", Device: "laptop"}
	req := rpc.Request{Method: "db.create", Operator: "mallory"}

	scope := newRPCScope(rec, req.Method, req)
	if err := scope.begin(context.Background()); err != nil {
		t.Fatal(err)
	}

	if err := scope.finish(context.Background(), activity.OutcomeSucceeded, "", ""); err != nil {
		t.Fatal(err)
	}

	recordActivityRefusal(context.Background(), rec, req, req.Method, deviceAccessDenied(), "")

	for _, ev := range store.recorded() {
		if !ev.ActorVerified || ev.ActorLabel != "alice" || ev.OperatorID != "op-1" || ev.DeviceID != "dev-1" || ev.DeviceLabel != "laptop" {
			t.Fatalf("incorrect attribution: %+v", ev)
		}
	}

	if len(store.recorded()) != 3 {
		t.Fatalf("missing activity events: %+v", store.recorded())
	}
}

func TestActivityPreservesUnderscoreDatabase(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)

	scope := newRPCScope(rec, "db.create", rpc.Request{
		RequestID: testRequestID,
		Params:    json.RawMessage(`{"name":"_app"}`),
	})
	if err := scope.begin(context.Background()); err != nil {
		t.Fatal(err)
	}

	scope.recordStep(context.Background(), output.Event{Type: "step", Step: "postgres-cluster-v1@_app", State: "started"}, nil)

	for _, ev := range store.recorded() {
		rejectf(t, ev.Database != "_app", "database correlation was lost: %+v", ev)
	}

	q := activity.Query{Database: "_app"}
	if err := normalizeActivityQuery(&q); err != nil {
		t.Fatalf("valid database filter refused: %v", err)
	}
}

// fakeActivityStore records events after checking them against the real
// contract. It never touches /var/log.
type fakeActivityStore struct {
	mu sync.Mutex

	events    []activity.Event
	appendErr error
	queryErr  error
	healthErr error

	query  activity.Query
	page   activity.Page
	health activity.Health

	now func() time.Time
	seq uint64
}

func (f *fakeActivityStore) Append(_ context.Context, ev activity.Event) (activity.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.appendErr != nil {
		return ev, f.appendErr
	}

	if ev.Schema == "" {
		ev.Schema = activity.Schema
	}

	if err := activity.ValidateEvent(ev); err != nil {
		return ev, err
	}

	f.seq++
	ev.EventID = "01M4X0ME0NBQ95HNNDT213BGH9"
	ev.Sequence = f.seq

	now := time.Now
	if f.now != nil {
		now = f.now
	}

	ev.OccurredAt = now().UTC().Format(time.RFC3339Nano)
	f.events = append(f.events, ev)

	return ev, nil
}

func (f *fakeActivityStore) Query(_ context.Context, q activity.Query) (activity.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.query = q
	if f.queryErr != nil {
		return activity.Page{}, f.queryErr
	}

	return f.page, nil
}

func (f *fakeActivityStore) Health(context.Context) (activity.Health, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.healthErr != nil {
		return activity.Health{}, f.healthErr
	}

	return f.health, nil
}

func (f *fakeActivityStore) recorded() []activity.Event {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]activity.Event(nil), f.events...)
}

type activityTestClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *activityTestClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *activityTestClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

func testRecorder(t *testing.T, store activityStore) (*activityRecorder, *activityTestClock) {
	t.Helper()
	clock := &activityTestClock{t: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}

	return &activityRecorder{
		store:   store,
		storeOK: true,
		node: activityNodeInfo{
			Server: "prod", Node: "db1", NodeInstanceID: "abc123",
			BootID: "boot-1", NodeVersion: "dev+abc123",
		},
		now: clock.now,
	}, clock
}

func testRPCRequest() rpc.Request {
	return rpc.Request{
		Schema:          rpc.Schema,
		RequestID:       testRequestID,
		OperationID:     testOperationID,
		ParentRequestID: testParentID,
		Method:          "apply",
		Operator:        "alice",
		Params:          json.RawMessage(`{"db":"app","sql":"SELECT s3cr3t FROM users"}`),
		Secrets:         map[string]string{"secret_name": "s3cr3t"},
	}
}

func TestActivityScopeRecordsOnlyTypedMetadata(t *testing.T) {
	const canary = "CANARY-do-not-record-42"

	store := &fakeActivityStore{}
	rec, clock := testRecorder(t, store)
	req := testRPCRequest()
	req.Operator = "alice"
	req.Params = json.RawMessage(`{"db":"app","sql":"` + canary + `","password":"` + canary + `"}`)
	req.Secrets = map[string]string{"password": canary}

	scope := newRPCScope(rec, "apply", req)
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	next := func(output.Event) {}
	report := scope.recorder(context.Background(), next)
	report(output.Event{Type: "log", Line: canary})
	report(output.Event{Type: "progress", Phase: "x", Detail: canary})
	report(output.Event{Type: "warning", Code: "w", Message: canary})
	report(output.Event{Type: "step", Step: "postgres-roles-v1@app", State: "started", Detail: canary})
	clock.advance(1250 * time.Millisecond)
	report(output.Event{Type: "step", Step: "postgres-roles-v1@app", State: "completed", Detail: canary})

	handlerErr := output.NewError(output.CodeStepFailed, "failed: "+canary, output.ExitStep, canary).
		WithDetails(map[string]any{"reason": "lock_held", "detail": canary, "params": canary})
	code, reason := activityErrorTokens(handlerErr)
	_ = scope.finish(t.Context(), activity.OutcomeFailed, code, reason)

	events := store.recorded()
	rejectf(t, len(events) != 4, "recorded %d events, want 4 (rpc start, step start, step end, rpc end): %+v", len(events), events)
	rejectf(t, events[0].Outcome != activity.OutcomeStarted || events[0].Source != activity.SourceRPC, "rpc start = %+v", events[0])
	rejectf(t, events[1].Source != activity.SourceStep || events[1].Step != "postgres-roles-v1@app" ||
		events[1].Outcome != activity.OutcomeStarted, "step start = %+v", events[1])
	rejectf(t, events[2].Outcome != activity.OutcomeSucceeded || events[2].DurationMS != 1250, "step end = %+v, want succeeded after 1250ms", events[2])
	rejectf(t, events[3].Outcome != activity.OutcomeFailed || events[3].ErrorCode != output.CodeStepFailed ||
		events[3].ReasonCode != "lock_held", "rpc end = %+v", events[3])
	rejectf(t, events[0].Database != "app" || events[3].Database != "app", "database metadata = %q/%q", events[0].Database, events[3].Database)
	rejectf(t, events[0].ActorLabel != "alice" || events[0].ActorVerified, "actor = %q verified=%v, want claimed alice and never verified", events[0].ActorLabel, events[0].ActorVerified)
	assertActivityEventsRedacted(t, events, canary)
}

func assertActivityEventsRedacted(t *testing.T, events []activity.Event, canary string) {
	t.Helper()

	for _, ev := range events {
		raw, err := json.Marshal(ev)
		reject(t, err != nil, err)
		rejectf(t, strings.Contains(string(raw), canary) || strings.Contains(string(raw), "SELECT"), "event leaked request or result text: %s", raw)
		rejectf(t, ev.OperationID != testOperationID || ev.RequestID != testRequestID || ev.ParentRequestID != testParentID, "correlation lost: %+v", ev)
	}
}

func TestActivityStepStateMapping(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)

	scope := newRPCScope(rec, "apply", testRPCRequest())
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	rep := scope.recorder(context.Background(), func(output.Event) {})
	rep(output.Event{Type: "step", Step: "replayed", State: "completed", TS: "2026-10-26T11:00:00Z"})
	rep(output.Event{Type: "step", Step: "a", State: "started"})
	rep(output.Event{Type: "step", Step: "a", State: "skipped", Detail: "inputs_changed"})
	rep(output.Event{Type: "step", Step: "b", State: "failed", Detail: "script failed: password hunter2"})
	rep(output.Event{Type: "step", Step: "c", State: "mystery", Detail: "free text"})

	events := store.recorded()[1:]
	rejectf(t, len(events) != 4, "recorded %d step events, want 4", len(events))
	rejectf(t, events[1].Outcome != activity.OutcomeSkipped || events[1].ReasonCode != "inputs_changed", "skipped = %+v", events[1])
	rejectf(t, events[2].Outcome != activity.OutcomeFailed || events[2].ErrorCode != output.CodeStepFailed, "failed = %+v", events[2])
	rejectf(t, events[3].Outcome != activity.OutcomeUnknown || events[3].ReasonCode != "", "unknown state = %+v", events[3])

	for _, ev := range events {
		raw, _ := json.Marshal(ev)
		rejectf(t, strings.Contains(string(raw), "hunter2") || strings.Contains(string(raw), "free text"), "step detail leaked: %s", raw)
	}
}

func TestActivityErrorTokens(t *testing.T) {
	code, reason := activityErrorTokens(output.NewError(output.CodePrecondition, "secret text", output.ExitPrecondition, "hint").
		WithDetails(map[string]any{"reason": "lock_held", "holder": "secret"}))
	rejectf(t, code != output.CodePrecondition || reason != "lock_held", "tokens = %q/%q", code, reason)
	code, reason = activityErrorTokens(output.NewError("made_up_code", "x", 1, "").
		WithDetails(map[string]any{"reason": "Bad Reason!"}))
	rejectf(t, code != output.CodeError || reason != "", "untrusted tokens = %q/%q, want %q and empty", code, reason, output.CodeError)
	code, reason = activityErrorTokens(output.NewError(output.CodePrecondition, "x", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": "unregisteredlowercasetoken"}))
	rejectf(t, code != output.CodePrecondition || reason != "", "unknown lower-case reason = %q/%q, want no reason", code, reason)
	code, reason = activityErrorTokens(context.Canceled)
	rejectf(t, code != output.CodeError || reason != "cancelled", "canceled = %q/%q", code, reason)

	if code, reason := activityErrorTokens(nil); code != output.CodeError || reason != "" {
		t.Fatalf("nil = %q/%q", code, reason)
	}
}

func TestRecordActivityRefusalKeepsOnlySafeTokens(t *testing.T) {
	const canary = "REFUSAL-CANARY"

	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()
	req.Params = json.RawMessage(`{"database":"app","sql":"` + canary + `"}`)
	err := output.NewError(output.CodeUnsafeAction, "token "+canary+" rejected", output.ExitUnsafe, canary).
		WithDetails(map[string]any{"reason": "confirm_mismatch", "confirm_token": canary})
	recordActivityRefusal(context.Background(), rec, req, "db.drop", err, "")

	events := store.recorded()
	rejectf(t, len(events) != 1, "recorded %d events, want 1", len(events))

	ev := events[0]
	if ev.Outcome != activity.OutcomeRefused || ev.Action != "db.drop" ||
		ev.ErrorCode != output.CodeUnsafeAction || ev.ReasonCode != "confirm_mismatch" {
		t.Fatalf("refusal = %+v", ev)
	}

	raw, _ := json.Marshal(ev)
	rejectf(t, strings.Contains(string(raw), canary), "refusal leaked canary: %s", raw)
}

func TestActivityImportantClassifications(t *testing.T) {
	table := methods()

	cases := []struct {
		method string
		req    rpc.Request
		want   bool
	}{
		{"plan", rpc.Request{}, false},
		{"apply", rpc.Request{}, true},
		{"db.credentials", rpc.Request{}, false},
		{"db.credentials", rpc.Request{Params: json.RawMessage(`{"rotate":true}`)}, true},
		{"apply", rpc.Request{DryRun: true}, false},
		{"tune.apply", rpc.Request{DryRun: true}, false},
		{"db.fork", rpc.Request{DryRun: true}, false},
		{"db.follow", rpc.Request{DryRun: true}, false},
		{"db.unfollow", rpc.Request{DryRun: true}, false},
		{"db.follow", rpc.Request{}, true},
		{"db.unfollow", rpc.Request{}, true},
		{"queries.reset", rpc.Request{DryRun: true}, false},
		{"metrics.rotate", rpc.Request{DryRun: true}, false},
		{"user.add", rpc.Request{DryRun: true}, false},
		{"user.add", rpc.Request{}, true},
		{"device.revoke", rpc.Request{Params: json.RawMessage(`{"user":"alice","label":"laptop"}`)}, false},
		{"device.revoke", rpc.Request{Params: json.RawMessage(`{"user":"alice","label":"laptop","id":"dev-1"}`)}, true},
		{"device.revoke", rpc.Request{Params: json.RawMessage(`{`)}, true},
		// These handlers ignore dry_run and still mutate, so a direct RPC must
		// not use the flag to skip its checked start.
		{"db.create", rpc.Request{DryRun: true}, false},
		{"db.drop", rpc.Request{DryRun: true}, true},
		{"protect.init", rpc.Request{DryRun: true}, true},
		{"cluster.bootstrap", rpc.Request{DryRun: true}, true},
		{"selfcheck", rpc.Request{}, false},
		{"protect.init", rpc.Request{}, true},
		{"protect.verify", rpc.Request{}, false},
		{"db.write-access", rpc.Request{}, true},
		{"status", rpc.Request{}, false},
		{"sql", rpc.Request{Params: json.RawMessage(`{"write":true}`)}, true},
		{"sql", rpc.Request{Params: json.RawMessage(`{"write":false}`)}, false},
		{"backup.now", rpc.Request{}, false},
		{"backup.verify", rpc.Request{}, false},
		{"restore.start", rpc.Request{}, false},
		{"restore.promote", rpc.Request{}, false},
		{"restore.undo", rpc.Request{}, false},
		{"restore.drill", rpc.Request{}, false},
		{"restore.cleanup", rpc.Request{}, false},
		{"db.recover", rpc.Request{}, false},
		{"master-backup.now", rpc.Request{}, false},
		{"ssh.confirm", rpc.Request{}, false},
		{"ssh.lockdown", rpc.Request{}, false},
		{"upgrade.install", rpc.Request{}, false},
		{"protect.verify", rpc.Request{}, false},
		{"cluster.confirm-secrets", rpc.Request{}, false},
		{"activity.list", rpc.Request{}, false},
	}
	for _, tc := range cases {
		m, ok := table[tc.method]
		rejectf(t, !ok, "method %q missing", tc.method)

		tc.req.Method = tc.method
		if got := activityImportant(m, tc.req); got != tc.want {
			t.Errorf("activityImportant(%s, dryRun=%v) = %v, want %v", tc.method, tc.req.DryRun, got, tc.want)
		}
	}

	reject(t, !isActivityMethod("activity.list") || !isActivityMethod("activity.health") || isActivityMethod("status"), "activity method classification is wrong")
}

func TestActivityCheckedStartPolicy(t *testing.T) {
	fail := &fakeActivityStore{appendErr: errors.New("disk full: /var/log/vaire/activity")}
	rec, _ := testRecorder(t, fail)
	scope := newRPCScope(rec, "apply", testRPCRequest())

	if err := beginActivityScope(t.Context(), scope, true, func(output.Event) {}); err == nil {
		t.Fatal("an important mutation started although its history write failed")
	} else {
		if code, reason := activityErrorTokens(err); code != output.CodePrecondition || reason != "activity_unavailable" {
			t.Fatalf("checked-start error = %q/%q", code, reason)
		}

		raw, _ := json.Marshal(output.Fail(nil, err))
		rejectf(t, strings.Contains(string(raw), "/var/log") || strings.Contains(string(raw), "disk full"), "checked-start error leaked store text: %s", raw)
	}

	var warnings []output.Event

	readScope := newRPCScope(rec, "status", testRPCRequest())
	if err := beginActivityScope(t.Context(), readScope, false, func(ev output.Event) { warnings = append(warnings, ev) }); err != nil {
		t.Fatalf("a read was refused because history failed: %v", err)
	}

	rejectf(t, len(warnings) != 1 || warnings[0].Code != activityWarningCode, "read warning = %+v, want one activity warning", warnings)
}

func TestDeviceRevokePreviewWorksWithoutDurableActivity(t *testing.T) {
	store := &fakeActivityStore{appendErr: errors.New("activity unavailable")}
	rec, _ := testRecorder(t, store)
	m := methods()["device.revoke"]

	for _, tc := range []struct {
		params  string
		wantErr bool
	}{
		{`{"user":"alice","label":"laptop"}`, false},
		{`{"user":"alice","label":"laptop","id":"dev-1"}`, true},
	} {
		req := rpc.Request{Method: m.name, Params: json.RawMessage(tc.params)}
		scope := newRPCScope(rec, m.name, req)
		err := beginActivityScope(t.Context(), scope, activityImportant(m, req), func(output.Event) {})

		if (err != nil) != tc.wantErr {
			t.Errorf("params %s: error = %v, want error %v", tc.params, err, tc.wantErr)
		}
	}
}

func TestActivityFinishWarnNeverHidesOutcome(t *testing.T) {
	store := &fakeActivityStore{appendErr: errors.New("write failed")}
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "apply", testRPCRequest())

	var warnings []output.Event

	_ = scope.finishWarn(t.Context(), activity.OutcomeSucceeded, "", "", func(ev output.Event) { warnings = append(warnings, ev) })
	_ = scope.finishWarn(t.Context(), activity.OutcomeSucceeded, "", "", func(ev output.Event) { warnings = append(warnings, ev) })
	rejectf(t, len(warnings) != 1 || warnings[0].Code != activityWarningCode, "warnings = %+v, want one bounded warning", warnings)
}

func TestJobActivityCorrelatesTimerAndSteps(t *testing.T) {
	store := &fakeActivityStore{}
	rec, clock := testRecorder(t, store)
	ja := newJobActivity(rec, "backup-full", testJobID, testOperationID, testParentID, "app", true, true, true)
	rep, err := ja.begin(t.Context(), nil)
	reject(t, err != nil, err)
	rep(output.Event{Type: "step", Step: "pgbackrest-v1", State: "started"})
	clock.advance(2 * time.Second)
	rep(output.Event{Type: "step", Step: "pgbackrest-v1", State: "completed"})
	ja.pending(activity.OutcomeSkipped, output.CodePrecondition, "heavy_io_lock_held")
	ja.finish(t.Context(), "", "", "")

	events := store.recorded()
	rejectf(t, len(events) != 4, "recorded %d events, want 4: %+v", len(events), events)

	if events[0].Source != activity.SourceJob || events[0].JobID != testJobID ||
		events[0].OperationID != testOperationID || events[0].ParentRequestID != testParentID ||
		events[0].RequestID != testJobID {
		t.Fatalf("job start correlation = %+v", events[0])
	}

	rejectf(t, events[0].ActorKind != activity.ActorTimer || events[0].ActorVerified, "timer actor = %+v", events[0])

	if events[1].Source != activity.SourceStep || events[1].JobID != testJobID ||
		events[1].Action != "job.backup-full" {
		t.Fatalf("job step correlation = %+v", events[1])
	}

	rejectf(t, events[2].DurationMS != 2000 || events[2].Outcome != activity.OutcomeSucceeded, "job step end = %+v", events[2])

	if events[3].Outcome != activity.OutcomeSkipped || events[3].ReasonCode != "heavy_io_lock_held" ||
		events[3].JobID != testJobID {
		t.Fatalf("job end = %+v", events[3])
	}
}

func TestJobActivityTimerHasNoRequestID(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)

	ja := newJobActivity(rec, "metrics-refresh", testJobID, "", "", "", true, false, false)
	if _, err := ja.begin(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	ja.finish(t.Context(), activity.OutcomeSucceeded, "", "")

	events := store.recorded()
	rejectf(t, len(events) != 2, "recorded %d events, want start and end: %+v", len(events), events)

	if events[0].RequestID != "" || events[0].OperationID != testJobID ||
		events[0].ActorKind != activity.ActorTimer {
		t.Fatalf("timer job start = %+v", events[0])
	}
}

func TestConfirmRefusalRecordsSafeReason(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := rpc.Request{
		Method: "db.drop", RequestID: testRequestID, OperationID: testOperationID,
		Params: json.RawMessage(`{"name":"app"}`), Confirm: "wrong-token",
	}
	m := method{name: "db.drop", confirm: dbToken}
	err := checkConfirm(m, config.Cluster{}, req)
	reject(t, err == nil, "a wrong confirm token was accepted")
	recordActivityRefusal(context.Background(), rec, req, req.Method, err, "confirm_mismatch")

	events := store.recorded()
	rejectf(t, len(events) != 1, "recorded %d events, want one", len(events))

	ev := events[0]
	if ev.Action != "db.drop" || ev.Database != "app" || ev.Outcome != activity.OutcomeRefused ||
		ev.ReasonCode != "confirm_mismatch" || ev.ErrorCode != output.CodeUnsafeAction {
		t.Fatalf("confirm refusal = %+v", ev)
	}

	raw, _ := json.Marshal(ev)
	rejectf(t, strings.Contains(string(raw), "wrong-token"), "confirm token leaked: %s", raw)
}

func TestJobActivityStartFailurePolicy(t *testing.T) {
	fail := &fakeActivityStore{appendErr: errors.New("unavailable")}
	rec, _ := testRecorder(t, fail)

	important := newJobActivity(rec, "backup-full", testJobID, "", "", "", false, true, true)
	if _, err := important.begin(t.Context(), nil); err == nil {
		t.Fatal("an important job started although its history write failed")
	} else if _, reason := activityErrorTokens(err); reason != "activity_unavailable" {
		t.Fatalf("important job start error reason = %q", reason)
	}

	safety := newJobActivity(rec, "certs-renew", testJobID, "", "", "", true, false, false)

	var warnings []output.Event

	rep := func(ev output.Event) { warnings = append(warnings, ev) }
	if _, err := safety.begin(t.Context(), rep); err != nil {
		t.Fatalf("a safety job was refused because history failed: %v", err)
	}

	rejectf(t, len(warnings) != 1 || warnings[0].Code != activityWarningCode, "safety warning = %+v", warnings)
}

func TestLoadActivityNodeInfo(t *testing.T) {
	dir := t.TempDir()

	paths := activityPaths{
		Node:     filepath.Join(dir, "node.toml"),
		Cluster:  filepath.Join(dir, "cluster.toml"),
		Instance: filepath.Join(dir, "instance"),
		BootID:   filepath.Join(dir, "boot_id"),
	}
	if err := os.WriteFile(paths.Node, []byte("server = \"prod\"\nnode_name = \"db1\"\nrole = \"master\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(paths.Instance, []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(paths.BootID, []byte("boot-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	info := loadActivityNodeInfo(paths, config.Cluster{})
	rejectf(t, info.Server != "prod" || info.Node != "db1" || info.NodeInstanceID != "abc123" || info.BootID != "boot-1", "node info = %+v", info)
	reject(t, info.NodeVersion == "", "node version is empty")

	bad := activityPaths{
		Node:     filepath.Join(dir, "missing-node.toml"),
		Cluster:  filepath.Join(dir, "missing-cluster.toml"),
		Instance: filepath.Join(dir, "missing-instance"),
		BootID:   filepath.Join(dir, "missing-boot"),
	}
	if info := loadActivityNodeInfo(bad, config.Cluster{}); info.Server != "" || info.Node != "" || info.NodeInstanceID != "" {
		t.Fatalf("missing files produced metadata: %+v", info)
	}
}

func TestJobRefusalRecordsInvocationActor(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	cause := output.NewError(output.CodePrecondition, "database app is gone", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": "database_not_found"})
	recordJobRefusal(context.Background(), rec, "backup-full", testJobID, cause, "", true)
	recordJobRefusal(context.Background(), rec, "backup-full", testJobID, cause, "", false)

	events := store.recorded()
	rejectf(t, len(events) != 2, "refusals = %+v, want two", events)
	rejectf(t, events[0].ActorKind != activity.ActorTimer || events[1].ActorKind != activity.ActorOperator, "actors = %q/%q, want timer/operator", events[0].ActorKind, events[1].ActorKind)

	for _, ev := range events {
		rejectf(t, ev.ReasonCode != "database_not_found" || ev.Source != activity.SourceJob, "refusal = %+v", ev)
	}
}

func TestLockRefusalRecordsSafeReason(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()
	err := output.NewError(output.CodePrecondition, "another op holds the lock", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": "lock_held", "holder": map[string]any{"pid": 42, "request_id": testRequestID}})
	recordActivityRefusal(context.Background(), rec, req, "apply", err, "")

	events := store.recorded()
	rejectf(t, len(events) != 1 || events[0].ReasonCode != "lock_held" || events[0].ErrorCode != output.CodePrecondition, "lock refusal = %+v", events)
}

func TestJobWarningCollector(t *testing.T) {
	var (
		collector jobWarningCollector
		forwarded []output.Event
	)

	report := collector.reporter(func(ev output.Event) { forwarded = append(forwarded, ev) })
	report(output.Event{Type: "log", Line: "PRIVATE-LOG"})
	report(output.Event{Type: "warning", Code: "activity_not_recorded", Message: "PRIVATE-MESSAGE"})
	report(output.Event{Type: "warning", Code: "activity_not_recorded", Message: "PRIVATE-MESSAGE"})
	rejectf(t, len(forwarded) != 3, "forwarded %d events, want 3", len(forwarded))

	warnings := collector.list()
	rejectf(t, len(warnings) != 2 || warnings[0].Code != "activity_not_recorded", "warnings = %+v", warnings)
}

func TestActivityFieldsNeverCarrySecrets(t *testing.T) {
	const canary = "TOTP-CANARY-987654"
	secrets.Add(canary)

	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()
	req.Operator = canary + "-operator"
	req.Params = json.RawMessage(`{"db":"app","secret":"` + canary + `"}`)
	req.Secrets = map[string]string{"protect_code": canary}

	scope := newRPCScope(rec, "protect.verify", req)
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	_ = scope.finish(t.Context(), activity.OutcomeRefused, output.CodeProtectCodeRejected, "code_invalid")

	for _, ev := range store.recorded() {
		raw, _ := json.Marshal(ev)
		rejectf(t, strings.Contains(string(raw), canary), "secret canary reached activity: %s", raw)
	}
}

func TestActivityStepDatabaseFromInstanceID(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()
	req.Params = json.RawMessage(`{}`) // a full apply has no db filter

	scope := newRPCScope(rec, "apply", req)
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	report := scope.recorder(context.Background(), func(output.Event) {})
	report(output.Event{Type: "step", Step: "postgres-config-v1@app", State: "started"})
	report(output.Event{Type: "step", Step: "postgres-config-v1@app", State: "completed"})
	report(output.Event{Type: "step", Step: "pgbackrest-v1", State: "completed"})

	_ = scope.finish(t.Context(), activity.OutcomeSucceeded, "", "")

	events := store.recorded()
	rejectf(t, len(events) != 5, "recorded %d events, want 5: %+v", len(events), events)

	for _, i := range []int{1, 2} {
		rejectf(t, events[i].Database != "app", "step %d database = %q, want app", i, events[i].Database)

		if events[i].OperationID != testOperationID || events[i].RequestID != testRequestID ||
			events[i].ParentRequestID != testParentID {
			t.Fatalf("step %d lost parent correlation: %+v", i, events[i])
		}
	}

	rejectf(t, events[3].Database != "", "non-cluster step database = %q, want empty", events[3].Database)
}

func TestActivityRecorderRefreshesIdentityAfterBootstrap(t *testing.T) {
	dir := t.TempDir()
	paths := activityPaths{
		Node:     filepath.Join(dir, "node.toml"),
		Cluster:  filepath.Join(dir, "cluster.toml"),
		Instance: filepath.Join(dir, "instance"),
		BootID:   filepath.Join(dir, "boot_id"),
	}
	store := &fakeActivityStore{}
	rec := &activityRecorder{store: store, storeOK: true, paths: paths, now: time.Now}

	scope := newRPCScope(rec, "apply", testRPCRequest())
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	first := store.recorded()
	rejectf(t, first[0].Node != "" || first[0].Server != "", "pre-bootstrap event had identity: %+v", first[0])

	if err := os.WriteFile(paths.Node, []byte("server = \"prod\"\nnode_name = \"db1\"\nrole = \"master\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(paths.Instance, []byte("abc123\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(paths.BootID, []byte("boot-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_ = scope.finish(t.Context(), activity.OutcomeSucceeded, "", "")
	events := store.recorded()
	last := events[len(events)-1]
	rejectf(t, last.Server != "prod" || last.Node != "db1" || last.NodeInstanceID != "abc123" || last.BootID != "boot-1", "late identity not refreshed: %+v", last)
}

func TestLoadActivityNodeInfoUsesMasterName(t *testing.T) {
	dir := t.TempDir()
	paths := activityPaths{
		Node:     filepath.Join(dir, "missing-node.toml"),
		Cluster:  filepath.Join(dir, "missing-cluster.toml"),
		Instance: filepath.Join(dir, "missing-instance"),
		BootID:   filepath.Join(dir, "missing-boot"),
	}
	cfg := config.Cluster{
		ClusterName: "prod",
		Nodes:       []config.NodeSpec{{Name: "db1", Role: config.RoleMaster}},
	}
	info := loadActivityNodeInfo(paths, cfg)
	rejectf(t, info.Server != "prod", "server = %q, want setup name", info.Server)
	rejectf(t, info.Node != "db1", "node = %q, want master node name", info.Node)

	noMaster := loadActivityNodeInfo(paths, config.Cluster{ClusterName: "prod"})
	rejectf(t, noMaster.Node != "", "node without a master = %q, want empty", noMaster.Node)
}

func TestActivityActionAllowlist(t *testing.T) {
	for input, want := range map[string]string{
		"status": "status", "activity.list": "activity.list",
		"evil.method": "node.rpc", "activity.foo": "node.rpc", "": "node.rpc",
	} {
		if got := safeActivityAction(input); got != want {
			t.Errorf("safeActivityAction(%q) = %q, want %q", input, got, want)
		}
	}

	for input, want := range map[string]string{
		"backup-full": "job.backup-full", "certs-renew": "job.certs-renew",
		"db-follow": "job.db-follow", "db-unfollow": "job.db-unfollow",
		"credential-ish": "job.unknown", "evil.job": "job.unknown", "": "job.unknown",
	} {
		if got := jobActivityAction(input); got != want {
			t.Errorf("jobActivityAction(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestUnknownReasonTokensAreOmitted(t *testing.T) {
	const canary = "unregisteredlowercasetoken"

	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()

	scope := newRPCScope(rec, "apply", req)
	if err := scope.begin(t.Context()); err != nil {
		t.Fatal(err)
	}

	scope.recorder(context.Background(), func(output.Event) {})(output.Event{Type: "step", Step: "a", State: "skipped", Detail: canary})
	err := output.NewError(output.CodePrecondition, "failed", output.ExitPrecondition, "").
		WithDetails(map[string]any{"reason": canary})
	recordActivityRefusal(context.Background(), rec, req, "apply", err, "")

	for _, ev := range store.recorded() {
		rejectf(t, ev.ReasonCode != "", "unknown reason %q was exported: %+v", ev.ReasonCode, ev)
		raw, _ := json.Marshal(ev)
		rejectf(t, strings.Contains(string(raw), canary), "unknown reason leaked: %s", raw)
	}

	if got := safeActivityToken("inputs_changed"); got != "inputs_changed" {
		t.Fatalf("known step reason = %q", got)
	}

	if got := safeActivityToken("activity_unavailable"); got != "activity_unavailable" {
		t.Fatalf("known node reason = %q", got)
	}

	if got := safeActivityToken(canary); got != "" {
		t.Fatalf("unknown lower-case reason = %q, want empty", got)
	}
}

// A follower's refusals are recorded like any other refusal: the stable
// reason survives into history and the follower is the event's database.
func TestFollowerRefusalReasonsReachActivity(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	req := testRPCRequest()
	req.Params = json.RawMessage(`{"source":"app","name":"app_follower"}`)

	reasons := []string{"follower_not_running", "follower_recovery_floor_decrease", "follows_follower", "has_followers", "is_follower", "not_a_follower", "source_awaiting_recovery", "source_not_running"}
	for _, reason := range reasons {
		err := output.NewError(output.CodePrecondition, "refused", output.ExitPrecondition, "").
			WithDetails(map[string]any{"reason": reason, "database": "app_follower"})
		recordActivityRefusal(context.Background(), rec, req, "db.follow", err, "is_follower")
	}

	events := store.recorded()
	rejectf(t, len(events) != len(reasons), "recorded %d events, want %d", len(events), len(reasons))

	for i, ev := range events {
		if ev.Outcome != activity.OutcomeRefused || ev.ReasonCode != reasons[i] || ev.Database != "app_follower" {
			t.Errorf("event %d = %+v, want reason %s for app_follower", i, ev, reasons[i])
		}
	}
}

// The helpers that build a follower refusal carry the reason history keeps.
func TestFollowerRefusalHelpersAreRecordable(t *testing.T) {
	cfg := followerCfg()
	spec, _ := cfg.ClusterByName("app_follower")
	src, _ := cfg.ClusterByName("app")

	cases := []struct {
		name string
		err  error
	}{
		{"is_follower", followerError(spec, "a backup")},
		{"has_followers", refuseSourceDrop(cfg, "app")},
		{"follows_follower", checkFollowSource(spec, "db follow")},
		{"source_not_running", checkFollowSource(src, "db follow")},
	}
	for _, c := range cases {
		if got := reason(t, c.err); got != c.name {
			t.Errorf("%s: reason %q", c.name, got)
		}

		code, got := activityErrorTokens(c.err)
		if code != output.CodePrecondition || got != c.name {
			t.Errorf("%s: activity tokens %q/%q", c.name, code, got)
		}
	}
}

// db.follow and db.unfollow name a database in their name param, as db.fork
// does, so history can attribute them to the follower.
func TestFollowerMethodsCorrelateTheFollower(t *testing.T) {
	for _, method := range []string{"db.follow", "db.unfollow"} {
		if got := activityDatabaseFor(method, json.RawMessage(`{"source":"app","name":"app_follower"}`)); got != "app_follower" {
			t.Errorf("activityDatabaseFor(%s) = %q", method, got)
		}
	}

	if got := activityDatabaseFor("db.follow", json.RawMessage(`{"source":"app"}`)); got != "" {
		t.Errorf("db.follow without a name = %q, want empty", got)
	}
}

// The db-follow and db-unfollow jobs name the follower in their name param,
// so their activity events carry the follower too (Phase 6).
func TestFollowerJobsCorrelateTheFollower(t *testing.T) {
	for _, job := range []string{"db-follow", "db-unfollow"} {
		if got := rpcJobDatabase(job, json.RawMessage(`{"source":"app","name":"app_follower"}`)); got != "app_follower" {
			t.Errorf("rpcJobDatabase(%s) = %q", job, got)
		}
	}
}

// The follower's postgres-roles-v1 skip is a stable step reason.
func TestFollowerStepSkipReasonIsKept(t *testing.T) {
	if got := safeActivityToken("follower"); got != "follower" {
		t.Fatalf("safeActivityToken(follower) = %q", got)
	}
}

func TestActivityLostStartYieldsOneResultWarning(t *testing.T) {
	store := &fakeActivityStore{appendErr: errors.New("unavailable")}
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "status", testRPCRequest())

	var streamWarnings []output.Event

	if err := beginActivityScope(t.Context(), scope, false, func(ev output.Event) {
		streamWarnings = append(streamWarnings, ev)
	}); err != nil {
		t.Fatal(err)
	}

	reject(t, !scope.lostEvidence(), "failed start was not tracked as lost evidence")
	rejectf(t, len(streamWarnings) != 1 || streamWarnings[0].Code != activityWarningCode, "stream warnings = %+v", streamWarnings)

	store.appendErr = nil
	res := Result{}
	finishActivitySuccess(t.Context(), scope, &res, func(output.Event) {})
	rejectf(t, len(res.Warnings) != 1 || res.Warnings[0].Code != activityWarningCode, "result warnings = %+v, want one activity warning", res.Warnings)
	finishActivitySuccess(t.Context(), scope, &res, func(output.Event) {})
	rejectf(t, len(res.Warnings) != 1, "result warnings after a second finish = %+v, want one", res.Warnings)
}

func TestFinishRunJobActivityPanicNeverSucceeds(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)

	ja := newJobActivity(rec, "backup-full", testJobID, testOperationID, testParentID, "app", true, true, true)
	if _, err := ja.begin(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	finishRunJobActivity(context.Background(), ja, nil, "", "boom")

	events := store.recorded()
	last := events[len(events)-1]
	rejectf(t, last.Outcome != activity.OutcomeUnknown || last.ReasonCode != "panic", "panicking finalizer recorded %+v, want unknown/panic", last)

	for _, ev := range events {
		rejectf(t, ev.Outcome == activity.OutcomeSucceeded, "panic synthesized success: %+v", ev)
	}
}

func runPanickingJob(ja *jobActivity) (recovered any) {
	defer func() { recovered = recover() }()
	defer func() {
		r := recover()
		finishRunJobActivity(context.Background(), ja, nil, "", r)

		if r != nil {
			panic(r)
		}
	}()

	panic("job panic")
}

func TestRunJobPanicIsRecordedAndReRaised(t *testing.T) {
	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)

	ja := newJobActivity(rec, "restore", testJobID, testOperationID, "", "app", false, true, true)
	if _, err := ja.begin(t.Context(), nil); err != nil {
		t.Fatal(err)
	}

	if got := runPanickingJob(ja); got != "job panic" {
		t.Fatalf("recovered = %v, want the original panic", got)
	}

	events := store.recorded()
	last := events[len(events)-1]
	rejectf(t, last.Outcome != activity.OutcomeUnknown || last.ReasonCode != "panic", "panic event = %+v", last)
}

func TestActivityRefusedJobRecord(t *testing.T) {
	if _, ok := activityRefusedJobRecord(false, testJobID, "restore", "app", activityUnavailableError()); ok {
		t.Fatal("a timer job without a queued record was marked finished")
	}

	cause := activityUnavailableError()
	rec, ok := activityRefusedJobRecord(true, testJobID, "restore", "app", cause)
	reject(t, !ok, "an existing RPC job was not marked finished")
	rejectf(t, rec.State != engine.JobFailed || rec.Detail != "activity history unavailable" || rec.FinishedAt == "", "record = %+v", rec)
	rejectf(t, rec.Job != "restore" || rec.DB != "app" || rec.ID != testJobID, "record identity = %+v", rec)
	rejectf(t, rec.Error == nil || rec.Error.Code != output.CodePrecondition, "record error = %+v", rec.Error)
	details, ok := rec.Error.Details.(map[string]any)
	rejectf(t, !ok || details["reason"] != "activity_unavailable", "record error details = %#v", rec.Error.Details)
	raw, _ := json.Marshal(rec)
	rejectf(t, strings.Contains(string(raw), "/var/log") || strings.Contains(string(raw), "disk full"), "refused job record leaked store text: %s", raw)
}
