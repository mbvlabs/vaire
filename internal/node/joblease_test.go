package node

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
)

func leaseTestEnv(job, params, db string) *jobEnv {
	return &jobEnv{ID: "job", Job: job, OperationID: "operation", installID: "install", DB: db,
		Params: json.RawMessage(params), Cluster: config.Cluster{Clusters: []config.ClusterSpec{{Name: "app"}}}}
}

func TestJobLockRequestsModes(t *testing.T) {
	tests := []struct {
		env      *jobEnv
		mode     engine.LockMode
		requests []jobLockRequest
	}{
		{leaseTestEnv("db-fork", `{"source":"app","name":"new"}`, ""), engine.LockExclusive, []jobLockRequest{{"app", engine.LockShared}, {"new", engine.LockExclusive}}},
		{leaseTestEnv("db-follow", `{"source":"app","name":"new"}`, ""), engine.LockExclusive, []jobLockRequest{{"app", engine.LockExclusive}, {"new", engine.LockExclusive}}},
		{leaseTestEnv("db-fork", `{"source":"app","name":"app"}`, ""), engine.LockExclusive, []jobLockRequest{{"app", engine.LockExclusive}}},
		{leaseTestEnv("restore", `{"database":"app"}`, ""), engine.LockShared, []jobLockRequest{{"app", engine.LockShared}, {"app__restoring", engine.LockExclusive}}},
		{leaseTestEnv("recover", `{"database":"app"}`, ""), engine.LockShared, []jobLockRequest{{"app", engine.LockShared}, {"app__restoring", engine.LockExclusive}}},
		{leaseTestEnv("restore-drill", "", "app"), engine.LockShared, []jobLockRequest{{"app", engine.LockShared}, {"app__drill", engine.LockExclusive}}},
	}
	for _, tt := range tests {
		t.Run(tt.env.Job+string(tt.env.Params), func(t *testing.T) {
			if err := validateJobLockParams(tt.env); err != nil {
				t.Fatal(err)
			}
			if got := jobLockMode(tt.env); got != tt.mode {
				t.Fatalf("node mode = %s", got)
			}
			if got := jobLockRequests(tt.env); !reflect.DeepEqual(got, tt.requests) {
				t.Fatalf("requests = %#v, want %#v", got, tt.requests)
			}
		})
	}
	for _, raw := range []string{`{"source":"missing","name":"new"}`, `{"source":"app","name":"../new"}`, `{`} {
		if validateJobLockParams(leaseTestEnv("db-fork", raw, "")) == nil {
			t.Fatalf("accepted invalid params %q", raw)
		}
	}
}

func TestJobLeaseAcquisitionAndRevocation(t *testing.T) {
	env := leaseTestEnv("recover", `{"database":"app"}`, "")
	var events []string
	var lease *lockedJobLease
	locks := jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			events = append(events, "node")
			return func() { events = append(events, "-node") }, nil
		},
		heavyWait: func(context.Context, string, string, time.Duration) (engine.Release, error) {
			events = append(events, "heavy")
			return func() { events = append(events, "-heavy") }, nil
		},
		cluster: func(_ context.Context, name string, _ engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			events = append(events, name)
			if env.lease != nil || lease != nil {
				t.Fatal("authority issued before all locks")
			}
			return func() { events = append(events, "-"+name) }, nil
		},
	}
	if withLockedJobTarget(env, "app__restoring", func() error { return nil }) == nil {
		t.Fatal("authority before acquisition")
	}
	var busy *jobLockBusy
	var err error
	lease, busy, err = takeJobLocks(context.Background(), env, locks)
	if err != nil || busy != nil {
		t.Fatalf("acquire: %v %v", busy, err)
	}
	env.lease = lease
	ok := func() error { return nil }
	if err := withLockedJobTarget(env, "app__restoring", ok); err != nil {
		t.Fatal(err)
	}
	if withLockedJobTarget(env, "app", ok) == nil {
		t.Fatal("source accepted as target")
	}
	original := env.Params
	env.Params = json.RawMessage(`{"database":"other"}`)
	if withLockedJobTarget(env, "app__restoring", ok) == nil {
		t.Fatal("changed source parameters accepted")
	}
	env.Params = original
	copiedEnv := *env
	if withLockedJobTarget(&copiedEnv, "app__restoring", ok) == nil {
		t.Fatal("copied environment accepted")
	}
	forged := &lockedJobLease{id: lease.id, kind: lease.kind, operation: lease.operation, install: lease.install,
		env: env, params: lease.params, db: lease.db, target: lease.target, targets: lease.targets}
	env.lease = forged
	if withLockedJobTarget(env, "app__restoring", ok) == nil {
		t.Fatal("forged lease accepted")
	}
	env.lease = lease
	lease.releaseHeavyIO()
	lease.releaseHeavyIO()
	if err := withLockedJobTarget(env, "app__restoring", ok); err != nil {
		t.Fatalf("early heavy release revoked target: %v", err)
	}
	lease.finish()
	lease.finish()
	if withLockedJobTarget(env, "app__restoring", ok) == nil {
		t.Fatal("stale lease accepted")
	}
	want := []string{"node", "heavy", "app", "app__restoring", "-heavy", "-app__restoring", "-app", "-node"}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

func TestJobLeaseFailureAndGuardDrain(t *testing.T) {
	env := leaseTestEnv("restore", `{"database":"app"}`, "")
	var mu sync.Mutex
	var events []string
	add := func(s string) { mu.Lock(); defer mu.Unlock(); events = append(events, s) }
	fail := true
	locks := jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			add("node")
			return func() { add("-node") }, nil
		},
		cluster: func(_ context.Context, name string, _ engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			if fail && name == "app__restoring" {
				return nil, errors.New("failure")
			}
			add(name)
			return func() { add("-" + name) }, nil
		},
	}
	l, _, err := takeJobLocks(context.Background(), env, locks)
	if err == nil || l != nil || env.lease != nil {
		t.Fatal("failed acquisition issued authority")
	}
	fail = false
	l, _, err = takeJobLocks(context.Background(), env, locks)
	if err != nil {
		t.Fatal(err)
	}
	env.lease = l
	entered, leave, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		_ = withLockedJobTarget(env, "app__restoring", func() error { close(entered); <-leave; return nil })
	}()
	<-entered
	go func() { l.finish(); close(done) }()
	select {
	case <-done:
		t.Fatal("released while action active")
	case <-time.After(25 * time.Millisecond):
	}
	mu.Lock()
	before := append([]string(nil), events...)
	mu.Unlock()
	if !reflect.DeepEqual(before, []string{"node", "app", "-app", "-node", "node", "app", "app__restoring"}) {
		t.Fatalf("locks released early: %v", before)
	}
	close(leave)
	<-done
	if withLockedJobTarget(env, "app__restoring", func() error { return nil }) == nil {
		t.Fatal("stale authority")
	}
}

func TestJobLeasePanicFinalizes(t *testing.T) {
	env := leaseTestEnv("restore", `{"database":"app"}`, "")
	released := false
	var l *lockedJobLease
	l = newLockedJobLease(env, []jobLockRequest{{"app__restoring", engine.LockExclusive}}, nil, func() {
		lockedJobs.Lock()
		_, active := lockedJobs.active[l]
		lockedJobs.Unlock()
		if active {
			t.Error("OS release preceded revocation")
		}
		released = true
	})
	env.lease = l
	func() {
		defer func() { _ = recover() }()
		defer l.finish()
		panic("test")
	}()
	if !released || withLockedJobTarget(env, "app__restoring", func() error { return nil }) == nil {
		t.Fatal("panic did not revoke before releasing")
	}
}

func TestRecoverHeavyWaitFailureReleasesNode(t *testing.T) {
	env := leaseTestEnv("recover", `{"database":"app"}`, "")
	wantErr := errors.New("heavy wait failed")
	released := false
	locks := jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return func() { released = true }, nil
		},
		heavyWait: func(_ context.Context, _, _ string, wait time.Duration) (engine.Release, error) {
			if wait != recoverHeavyIOWait {
				t.Errorf("recover wait = %s", wait)
			}
			return nil, wantErr
		},
		cluster: func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			t.Fatal("cluster acquired after heavy wait failure")
			return nil, nil
		},
	}
	l, busy, err := takeJobLocks(context.Background(), env, locks)
	if l != nil || busy != nil || !errors.Is(err, wantErr) || !released {
		t.Fatalf("lease=%v busy=%v err=%v released=%v", l, busy, err, released)
	}
}
