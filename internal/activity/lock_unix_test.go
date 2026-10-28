//go:build unix

package activity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestAppendLockContention(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	oldWait, oldPoll := lockWait, lockPoll
	lockWait, lockPoll = 100*time.Millisecond, 2*time.Millisecond
	t.Cleanup(func() { lockWait, lockPoll = oldWait, oldPoll })
	lockPath := filepath.Join(s.Dir, lockName)
	f, err := os.OpenFile(lockPath, os.O_RDWR, fileMode) //nolint:gosec // G304: a lock file in the test store.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("hold lock: %v", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := s.Append(ctx, baseEvent()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("context-bound append error = %v", err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errLockTimeout) {
		t.Fatalf("timeout append error = %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); err != nil {
		t.Fatalf("append after release: %v", err)
	}
}

func TestAppendLockFileSafety(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	lockPath := filepath.Join(s.Dir, lockName)
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, nil, 0o644); err != nil { //nolint:gosec // G306: the test needs an unsafe lock mode.
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); !errors.Is(err, errUnsafeLock) {
		t.Fatalf("unsafe lock mode error = %v, want errUnsafeLock", err)
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, nil, fileMode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(context.Background(), baseEvent()); err == nil {
		t.Fatal("symlinked lock file was accepted")
	}
}

func TestAppendWriteFailureIsReportedAndCounted(t *testing.T) {
	s := testStore(t)
	mustAppend(t, s, baseEvent())
	old := writeSegmentLine
	writeSegmentLine = func(string, []byte, bool) error { return fmt.Errorf("injected write failure") }
	t.Cleanup(func() { writeSegmentLine = old })
	if _, err := s.Append(context.Background(), baseEvent()); err == nil {
		t.Fatal("append swallowed the write failure")
	}
	health, err := s.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.DroppedEvents != 1 {
		t.Fatalf("dropped events = %d, want 1", health.DroppedEvents)
	}
	if !health.LossDetected {
		t.Fatal("dropped append not reported as loss")
	}
	page, err := s.Query(context.Background(), Query{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if page.Coverage.DroppedEvents != 1 || !page.Coverage.LossDetected {
		t.Fatalf("query coverage = %+v", page.Coverage)
	}
	if len(page.Events) != 1 {
		t.Fatalf("events = %d, want the previously written event", len(page.Events))
	}
}

func TestCrossProcessAppends(t *testing.T) {
	s := testStore(t)
	const processes = 4
	const each = 10
	var wg sync.WaitGroup
	outputs := make([]string, processes)
	errs := make([]error, processes)
	for p := 0; p < processes; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=TestHelperProcessAppend") //nolint:gosec // G204: the test binary's own path.
			cmd.Env = append(os.Environ(),
				"VAIRE_ACTIVITY_HELPER=1",
				"VAIRE_ACTIVITY_DIR="+s.Dir,
				"VAIRE_ACTIVITY_COUNT="+strconv.Itoa(each),
			)
			raw, err := cmd.CombinedOutput()
			outputs[p], errs[p] = string(raw), err
		}(p)
	}
	wg.Wait()
	for p := 0; p < processes; p++ {
		if errs[p] != nil {
			t.Fatalf("helper %d: %v\n%s", p, errs[p], outputs[p])
		}
	}
	page, err := s.Query(context.Background(), Query{Limit: MaxLimit})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got, want := len(page.Events), processes*each; got != want {
		t.Fatalf("events = %d, want %d", got, want)
	}
	for i, ev := range page.Events {
		if ev.Sequence != uint64(i+1) {
			t.Fatalf("event %d sequence = %d", i, ev.Sequence)
		}
	}
}

func TestHelperProcessAppend(t *testing.T) {
	if os.Getenv("VAIRE_ACTIVITY_HELPER") != "1" {
		return
	}
	dir := os.Getenv("VAIRE_ACTIVITY_DIR")
	count, err := strconv.Atoi(os.Getenv("VAIRE_ACTIVITY_COUNT"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	s := &Store{Dir: dir, MaxBytes: 256 << 10, SegmentBytes: 16 << 10, Retention: time.Hour}
	for i := 0; i < count; i++ {
		if _, err := s.Append(context.Background(), Event{
			Source:      SourceJob,
			Action:      "backup.full",
			Outcome:     OutcomeSucceeded,
			JobID:       "job-helper",
			OperationID: "op-helper",
		}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
	}
	os.Exit(0)
}
