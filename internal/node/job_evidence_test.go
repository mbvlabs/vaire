package node

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/engine"
)

// captureSettlement replaces the persisted-record seam and returns the calls.
func captureSettlement(t *testing.T) func() (id string, lost bool) {
	t.Helper()
	var (
		mu   sync.Mutex
		last struct {
			id   string
			lost bool
			set  bool
		}
	)
	old := settleJobRecord
	settleJobRecord = func(id string, lost bool) error {
		mu.Lock()
		defer mu.Unlock()
		last.id, last.lost, last.set = id, lost, true
		return nil
	}
	t.Cleanup(func() { settleJobRecord = old })
	return func() (string, bool) {
		mu.Lock()
		defer mu.Unlock()
		if !last.set {
			t.Fatal("the finalizer did not settle the job record")
		}
		return last.id, last.lost
	}
}

// stubJobState replaces the state read and the evidence bound for one test.
func stubJobState(t *testing.T, load func() (engine.State, error), wait time.Duration) {
	t.Helper()
	oldLoad, oldWait := loadJobState, jobEvidenceWait
	loadJobState, jobEvidenceWait = load, wait
	t.Cleanup(func() { loadJobState, jobEvidenceWait = oldLoad, oldWait })
}

// shortJobSettle shortens the settlement bound and cap for one test.
func shortJobSettle(t *testing.T, wait time.Duration, cap int64) {
	t.Helper()
	oldWait, oldCap := jobSettleWait, jobSettleCap
	jobSettleWait, jobSettleCap = wait, cap
	t.Cleanup(func() {
		jobSettleWait, jobSettleCap = oldWait, oldCap
		waitJobSettlers(t)
	})
}

// waitJobSettlers waits until no settlement goroutine is alive.
func waitJobSettlers(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for jobSettlesLive.Load() > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%d job settlements still alive", jobSettlesLive.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestJobFinalizerSettlesActivityEvidence proves the terminal record's pending
// marker is cleared and a lost final write is carried into it, after the
// activity scope finishes rather than when the state was first published.
func TestJobFinalizerSettlesActivityEvidence(t *testing.T) {
	settled := captureSettlement(t)

	store := &fakeActivityStore{}
	rec, _ := testRecorder(t, store)
	ja := newJobActivity(rec, "backup-full", testJobID, testOperationID, "", "app", true, false, false)
	ja.recorded = true
	if _, err := ja.begin(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	finishRunJobActivity(ja, nil, "", nil)
	if id, lost := settled(); id != testJobID || lost {
		t.Fatalf("settled %q lost=%v, want %q lost=false", id, lost, testJobID)
	}

	failing := &fakeActivityStore{appendErr: errors.New("disk full")}
	rec, _ = testRecorder(t, failing)
	ja = newJobActivity(rec, "backup-full", testJobID, testOperationID, "", "app", true, false, false)
	ja.recorded = true
	finishRunJobActivity(ja, nil, "", nil)
	if id, lost := settled(); id != testJobID || !lost {
		t.Fatalf("settled %q lost=%v, want %q lost=true", id, lost, testJobID)
	}
}

// TestFollowJobWaitsForCompletionEvidence covers a follower that attaches
// while the terminal state is still pending: it must not conclude from that
// record, and it must see the warning the job carried once settled.
func TestFollowJobWaitsForCompletionEvidence(t *testing.T) {
	final := engine.JobRecord{ID: testJobID, Job: "backup-full", State: engine.JobSucceeded, ActivityPending: true}
	settled := final
	settled.ActivityPending = false
	settled.Warnings = []output.Warning{activityWarning()}

	var (
		mu    sync.Mutex
		polls int
	)
	stubJobState(t, func() (engine.State, error) {
		mu.Lock()
		defer mu.Unlock()
		polls++
		rec := final
		if polls > 2 {
			rec = settled
		}
		return engine.State{Jobs: []engine.JobRecord{rec}}, nil
	}, 5*time.Second)

	res, err := followJob(context.Background(), &Env{}, testJobID)
	if err != nil {
		t.Fatalf("followJob: %v", err)
	}
	mu.Lock()
	gotPolls := polls
	mu.Unlock()
	if gotPolls < 3 {
		t.Fatalf("follower read %d times, want it to wait for settlement", gotPolls)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != activityWarningCode {
		t.Fatalf("warnings = %+v, want the settled evidence warning", res.Warnings)
	}
}

// TestFollowJobFlagsUnsettledEvidence keeps a job that died before settling
// from looking complete: the follower reports missing evidence within the
// bound instead of waiting forever.
func TestFollowJobFlagsUnsettledEvidence(t *testing.T) {
	rec := engine.JobRecord{ID: testJobID, Job: "backup-full", State: engine.JobSucceeded, ActivityPending: true}
	stubJobState(t, func() (engine.State, error) {
		return engine.State{Jobs: []engine.JobRecord{rec}}, nil
	}, 30*time.Millisecond)

	started := time.Now()
	res, err := followJob(context.Background(), &Env{}, testJobID)
	if err != nil {
		t.Fatalf("followJob: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("unsettled evidence held the follower for %s", elapsed)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != activityWarningCode {
		t.Fatalf("warnings = %+v, want one missing-evidence warning", res.Warnings)
	}
}

// TestFollowJobFailureCarriesEvidenceWarning keeps a warning visible on a
// failed job result, where a success envelope is not rendered.
func TestFollowJobFailureCarriesEvidenceWarning(t *testing.T) {
	rec := engine.JobRecord{
		ID: testJobID, Job: "backup-full", State: engine.JobFailed,
		Warnings: []output.Warning{activityWarning()},
	}
	stubJobState(t, func() (engine.State, error) {
		return engine.State{Jobs: []engine.JobRecord{rec}}, nil
	}, time.Second)

	_, err := followJob(context.Background(), &Env{}, testJobID)
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("followJob error = %v, want a typed failure", err)
	}
	if len(cliErr.Warnings) != 1 || cliErr.Warnings[0].Code != activityWarningCode {
		t.Fatalf("failure warnings = %#v, want the evidence warning", cliErr.Warnings)
	}
	if details, ok := cliErr.Details.(map[string]any); ok {
		if _, exists := details["warnings"]; exists {
			t.Fatalf("warnings stayed an untyped detail: %#v", details)
		}
	}
}

// TestJobSettlementIsBoundedWhenStateStalls proves diagnostic bookkeeping
// cannot hold a finished safety job: a stalled state write returns within the
// settlement bound, warns, and leaves the record for the follower to report.
func TestJobSettlementIsBoundedWhenStateStalls(t *testing.T) {
	shortJobSettle(t, 25*time.Millisecond, 1)
	entered := make(chan struct{}, 1)
	blocked := make(chan struct{})
	var closeOnce sync.Once
	old := settleJobRecord
	settleJobRecord = func(string, bool) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return nil
	}
	t.Cleanup(func() {
		settleJobRecord = old
		closeOnce.Do(func() { close(blocked) })
	})

	var warnings []output.Event
	rec, _ := testRecorder(t, &fakeActivityStore{})
	ja := newJobActivity(rec, "backup-full", testJobID, testOperationID, "", "app", true, false, false)
	ja.recorded = true
	ja.base = func(ev output.Event) {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}

	started := time.Now()
	finishRunJobActivity(ja, nil, "", nil)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stalled settlement held the job for %s", elapsed)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("settlement never reached the state update")
	}
	if len(warnings) != 1 || warnings[0].Code != activityWarningCode {
		t.Fatalf("warnings = %+v, want one missing-evidence warning", warnings)
	}
	if jobSettlesLive.Load() != 1 {
		t.Fatalf("live settlements = %d, want one pinned writer", jobSettlesLive.Load())
	}
	closeOnce.Do(func() { close(blocked) })
	waitJobSettlers(t)
}

// TestJobSettlementCapBoundsAbandonedSettlers proves repeated in-process job
// runs cannot each pin another goroutine on a stalled state file.
func TestJobSettlementCapBoundsAbandonedSettlers(t *testing.T) {
	shortJobSettle(t, 25*time.Millisecond, 1)
	entered := make(chan struct{}, 1)
	blocked := make(chan struct{})
	var closeOnce sync.Once
	var calls atomic.Int64
	old := settleJobRecord
	settleJobRecord = func(string, bool) error {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-blocked
		return nil
	}
	t.Cleanup(func() {
		settleJobRecord = old
		closeOnce.Do(func() { close(blocked) })
	})

	first := newJobActivity(recorderForSettlementTest(t), "backup-full", testJobID, testOperationID, "", "app", true, false, false)
	first.recorded = true
	finishRunJobActivity(first, nil, "", nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("the pinned settlement never started")
	}

	var warnings []output.Event
	second := newJobActivity(recorderForSettlementTest(t), "backup-full", testOperationID, testOperationID, "", "app", true, false, false)
	second.recorded = true
	second.base = func(ev output.Event) {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}
	started := time.Now()
	finishRunJobActivity(second, nil, "", nil)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("capped settlement took %s", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("settlement calls = %d, want the one pinned writer", got)
	}
	if len(warnings) != 1 || warnings[0].Code != activityWarningCode {
		t.Fatalf("warnings = %+v, want one missing-evidence warning", warnings)
	}
	closeOnce.Do(func() { close(blocked) })
	waitJobSettlers(t)
}

// TestJobSettlementFailureLeavesEvidenceIncomplete keeps a failed state write
// visible: the finished job warns and the record stays pending for a follower.
func TestJobSettlementFailureLeavesEvidenceIncomplete(t *testing.T) {
	shortJobSettle(t, time.Second, 1)
	old := settleJobRecord
	settleJobRecord = func(string, bool) error { return errors.New("state write failed") }
	t.Cleanup(func() { settleJobRecord = old })

	var warnings []output.Event
	ja := newJobActivity(recorderForSettlementTest(t), "backup-full", testJobID, testOperationID, "", "app", true, false, false)
	ja.recorded = true
	ja.base = func(ev output.Event) {
		if ev.Type == "warning" {
			warnings = append(warnings, ev)
		}
	}
	finishRunJobActivity(ja, nil, "", nil)
	waitJobSettlers(t)
	if len(warnings) != 1 || warnings[0].Code != activityWarningCode {
		t.Fatalf("warnings = %+v, want one missing-evidence warning", warnings)
	}
}

// recorderForSettlementTest builds a recorder over a fast fake store.
func recorderForSettlementTest(t *testing.T) *activityRecorder {
	t.Helper()
	rec, _ := testRecorder(t, &fakeActivityStore{})
	return rec
}

func TestMergeJobWarningsDeduplicates(t *testing.T) {
	warning := activityWarning()
	merged := mergeJobWarnings([]output.Warning{warning, {Code: "other", Message: "x"}}, []output.Warning{warning, {}})
	if len(merged) != 2 || merged[0] != warning || merged[1].Code != "other" {
		t.Fatalf("merged = %+v", merged)
	}
	if merged := mergeJobWarnings(nil, nil); merged != nil {
		t.Fatalf("empty merge = %+v", merged)
	}
}
