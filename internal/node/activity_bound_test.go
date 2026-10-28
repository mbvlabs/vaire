package node

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/activity"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
)

// boundTestEvent is one valid event for the bounded-write tests.
func boundTestEvent() activity.Event {
	return activity.Event{Source: activity.SourceRPC, Action: "plan", Outcome: activity.OutcomeStarted}
}

// stalledActivityStore models a filesystem call that never returns: Append
// ignores its context until the test releases it. after is how many appends
// complete before the store stalls, so a test can let a start land and stall
// the terminal event instead.
type stalledActivityStore struct {
	entered chan struct{}
	release chan struct{}
	after   int

	mu     sync.Mutex
	events []activity.Event
	calls  int
	seq    uint64
}

func newStalledActivityStore() *stalledActivityStore {
	return &stalledActivityStore{entered: make(chan struct{}, 1), release: make(chan struct{})}
}

func (s *stalledActivityStore) Append(_ context.Context, ev activity.Event) (activity.Event, error) {
	s.mu.Lock()
	s.calls++
	stall := s.after == 0 || s.calls > s.after
	s.mu.Unlock()
	if stall {
		select {
		case s.entered <- struct{}{}:
		default:
		}
		<-s.release
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Schema == "" {
		ev.Schema = activity.Schema
	}
	if err := activity.ValidateEvent(ev); err != nil {
		return ev, err
	}
	s.seq++
	ev.EventID = "evt-01M4X0ME0NBQ95HNNDT213BGH9"
	ev.Sequence = s.seq
	ev.OccurredAt = time.Now().UTC().Format(time.RFC3339Nano)
	s.events = append(s.events, ev)
	return ev, nil
}

func (s *stalledActivityStore) Query(context.Context, activity.Query) (activity.Page, error) {
	return activity.Page{}, nil
}

func (s *stalledActivityStore) Health(context.Context) (activity.Health, error) {
	return activity.Health{}, nil
}

func (s *stalledActivityStore) recorded() []activity.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]activity.Event(nil), s.events...)
}

func shortActivityWait(t *testing.T) {
	t.Helper()
	old := activityWriteWait
	activityWriteWait = 25 * time.Millisecond
	t.Cleanup(func() { activityWriteWait = old })
}

// waitActivityWriters waits until no activity writer goroutine is alive, so a
// test cannot leak a pinned writer into the next one.
func waitActivityWriters(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for activityWritersLive.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d activity writers still alive", activityWritersLive.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// releaseAndDrain releases a stalled store and waits for its writer to exit.
func releaseAndDrain(t *testing.T, store *stalledActivityStore) {
	t.Helper()
	close(store.release)
	waitActivityWriters(t)
}

// TestActivityStartIsBoundedWhenTheStoreStalls proves a stalled store cannot
// hold an RPC: the append returns without waiting for the filesystem, and the
// checked-start policy turns that into a refusal before the handler runs.
func TestActivityStartIsBoundedWhenTheStoreStalls(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	t.Cleanup(func() { releaseAndDrain(t, store) })
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "db.create", testRPCRequest())

	started := time.Now()
	err := scope.begin(context.Background())
	if err == nil {
		t.Fatal("begin succeeded although the store never returned")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("begin took %s with a stalled store", elapsed)
	}

	important := newRPCScope(rec, "db.create", testRPCRequest())
	refused := beginActivityScope(context.Background(), important, true, nil)
	var cliErr *output.CLIError
	if !errors.As(refused, &cliErr) {
		t.Fatalf("checked start error = %v, want a typed refusal", refused)
	}
	details, _ := cliErr.Details.(map[string]any)
	if details["reason"] != "activity_unavailable" {
		t.Fatalf("refusal details = %v", cliErr.Details)
	}
}

// TestActivityAbandonedStartIsClosedWhenItArrivesLate keeps history honest: a
// start that becomes durable after the caller refused the operation must not
// look like work that ran.
func TestActivityAbandonedStartIsClosedWhenItArrivesLate(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "db.create", testRPCRequest())

	if err := scope.beginChecked(context.Background()); !errors.Is(err, errActivityWriteTimeout) {
		t.Fatalf("begin = %v, want errActivityWriteTimeout", err)
	}
	close(store.release)
	waitActivityWriters(t)
	deadline := time.Now().Add(5 * time.Second)
	for len(store.recorded()) < 2 {
		if time.Now().After(deadline) {
			t.Fatalf("abandoned start was not closed: %+v", store.recorded())
		}
		time.Sleep(5 * time.Millisecond)
	}
	events := store.recorded()
	if events[0].Outcome != activity.OutcomeStarted {
		t.Fatalf("first event = %+v, want started", events[0])
	}
	last := events[1]
	if last.Outcome != activity.OutcomeUnknown || last.ReasonCode != "activity_unavailable" {
		t.Fatalf("abandoned start was not closed as unknown: %+v", last)
	}
}

// TestActivityStalledStoreKeepsOneWriter bounds the cost of a broken store: one
// goroutine may wait on it, later appends fail fast, and no other operation
// pays the timeout again.
func TestActivityStalledStoreKeepsOneWriter(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	t.Cleanup(func() { releaseAndDrain(t, store) })
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "db.create", testRPCRequest())

	baseline := runtime.NumGoroutine()
	if err := scope.begin(context.Background()); err == nil {
		t.Fatal("begin succeeded with a stalled store")
	}
	if !rec.degraded.Load() {
		t.Fatal("recorder did not stop writing after a timeout")
	}

	started := time.Now()
	for i := 0; i < 50; i++ {
		if err := rec.append(context.Background(), scope.event(activity.OutcomeSucceeded, "", "", 0)); err == nil {
			t.Fatal("append succeeded with a stalled store")
		}
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("50 degraded appends took %s", elapsed)
	}
	if grown := runtime.NumGoroutine() - baseline; grown > 2 {
		t.Fatalf("stalled store added %d goroutines", grown)
	}
}

// TestActivityLateBestEffortStartIsLeftAlone keeps history truthful when the
// work continued: a start that lands after the caller stopped waiting is
// recorded as started, never rewritten as a refusal, and its lost terminal
// event leaves an incomplete outcome.
func TestActivityLateBestEffortStartIsLeftAlone(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "backup.now", testRPCRequest())

	if err := scope.begin(context.Background()); !errors.Is(err, errActivityWriteTimeout) {
		t.Fatalf("begin = %v, want errActivityWriteTimeout", err)
	}
	releaseAndDrain(t, store)
	events := store.recorded()
	if len(events) != 1 {
		t.Fatalf("events = %+v, want only the started record", events)
	}
	if events[0].Outcome != activity.OutcomeStarted {
		t.Fatalf("late best-effort start was rewritten: %+v", events[0])
	}
}

// TestActivityLateJobStartIsLeftAlone covers a safety job, which keeps running
// when its start cannot be recorded in time.
func TestActivityLateJobStartIsLeftAlone(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	rec, _ := testRecorder(t, store)
	ja := newJobActivity(rec, "backup", testRequestID, testRequestID, "", "app", false, true, false)

	if _, err := ja.begin(context.Background(), nil); err != nil {
		t.Fatalf("job begin = %v, want the run to continue", err)
	}
	releaseAndDrain(t, store)
	events := store.recorded()
	if len(events) != 1 || events[0].Outcome != activity.OutcomeStarted {
		t.Fatalf("events = %+v, want only the started record", events)
	}
	if events[0].Source != activity.SourceJob {
		t.Fatalf("job event source = %q", events[0].Source)
	}
}

// TestActivityLateTerminalIsRecordedAsItself proves a slow completion is never
// rewritten as a refusal: it lands as the outcome it is, or not at all.
func TestActivityLateTerminalIsRecordedAsItself(t *testing.T) {
	shortActivityWait(t)
	store := newStalledActivityStore()
	store.after = 1
	rec, _ := testRecorder(t, store)
	scope := newRPCScope(rec, "db.create", testRPCRequest())

	if err := scope.beginChecked(context.Background()); err != nil {
		t.Fatalf("begin = %v", err)
	}
	if err := scope.finish(context.Background(), activity.OutcomeSucceeded, "", ""); !errors.Is(err, errActivityWriteTimeout) {
		t.Fatalf("finish = %v, want errActivityWriteTimeout", err)
	}
	releaseAndDrain(t, store)
	events := store.recorded()
	if len(events) != 2 {
		t.Fatalf("events = %+v, want the start and its real outcome", events)
	}
	if last := events[1]; last.Outcome != activity.OutcomeSucceeded || last.ReasonCode != "" {
		t.Fatalf("late terminal was rewritten: %+v", last)
	}
}

// TestActivityMetadataStallIsBounded proves the wait also covers the node's
// identity reads, which used to happen in the caller before the timer ran.
func TestActivityMetadataStallIsBounded(t *testing.T) {
	shortActivityWait(t)
	entered := make(chan struct{}, 1)
	blocked := make(chan struct{})
	old := activityNodeInfoLoader
	activityNodeInfoLoader = func(activityPaths, config.Cluster) activityNodeInfo {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return activityNodeInfo{}
	}
	t.Cleanup(func() { activityNodeInfoLoader = old })
	fake := &fakeActivityStore{}
	oldStore := activityStoreFactory
	activityStoreFactory = func() (activityStore, error) { return fake, nil }
	t.Cleanup(func() { activityStoreFactory = oldStore })

	rec := newActivityRecorder(activityPaths{Node: "/nonexistent/node.toml"})
	started := time.Now()
	err := rec.append(context.Background(), boundTestEvent())
	if !errors.Is(err, errActivityWriteTimeout) {
		t.Fatalf("append = %v, want errActivityWriteTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("metadata stall held the caller for %s", elapsed)
	}
	select {
	case <-entered:
	default:
		t.Fatal("the writer never read the node metadata")
	}
	close(blocked)
	waitActivityWriters(t)
}

// TestActivityWriterSlotsAreProcessWide proves repeated in-process calls cannot
// each pin another goroutine on a stalled store.
func TestActivityWriterSlotsAreProcessWide(t *testing.T) {
	shortActivityWait(t)
	oldCap := activityWriterCap
	activityWriterCap = 1
	t.Cleanup(func() {
		activityWriterCap = oldCap
		waitActivityWriters(t)
	})

	stalled := newStalledActivityStore()
	stalledRec, _ := testRecorder(t, stalled)
	if err := stalledRec.append(context.Background(), boundTestEvent()); !errors.Is(err, errActivityWriteTimeout) {
		t.Fatalf("stalled append = %v, want errActivityWriteTimeout", err)
	}

	fast := &fakeActivityStore{}
	fastRec, _ := testRecorder(t, fast)
	started := time.Now()
	if err := fastRec.append(context.Background(), boundTestEvent()); !errors.Is(err, errActivityWriterBusy) {
		t.Fatalf("append with the cap held = %v, want errActivityWriterBusy", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("capped append took %s", elapsed)
	}
	if len(fast.recorded()) != 0 {
		t.Fatal("a capped append still reached the store")
	}

	releaseAndDrain(t, stalled)
	if err := fastRec.append(context.Background(), boundTestEvent()); err != nil {
		t.Fatalf("append after the slot was freed = %v", err)
	}
	waitActivityWriters(t)
}
