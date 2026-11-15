package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
)

// These mocks replace only host-facing operations. The actual handlers,
// record transactions, admission checks and runner remain in use.
func TestRestoreRecoverPreparationHandlersSerializeAndPublish(t *testing.T) {
	for _, pair := range [][2]string{{"restore", "restore"}, {"recover", "recover"}, {"restore", "recover"}, {"recover", "restore"}} {
		t.Run(pair[0]+"-"+pair[1], func(t *testing.T) {
			cfg := setupRestorePreparation(t)
			if pair[0] == "recover" || pair[1] == "recover" {
				requireAwaitingRecovery(t)
			}
			c := newPreparationConcurrency()
			c.installLocks(t)
			c.installPublication(t)
			c.installLaunch(t)
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { first <- runRestorePreparation(cfg, pair[0], "job1") }()
			<-c.entered
			go func() { second <- runRestorePreparation(cfg, pair[1], "job2") }()
			<-c.secondWaiting
			close(c.proceed)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := <-second; err == nil {
				t.Fatal("second preparation overwrote an open intent")
			}
			r, err := backup.LoadRecords()
			rejectf(t, err != nil || r.Restores["app"].ID != "job1" || c.publishCount != 1 || c.launchCount != 1, "record=%+v publications=%d launches=%d err=%v", r.Restores["app"], c.publishCount, c.launchCount, err)
		})
	}
}

// setupRestorePreparation installs the host-facing reads shared by the restore
// and recover preparation tests, keeping the handlers, record transactions and
// runner real. It returns the cluster loadGuardCluster serves.
func setupRestorePreparation(t *testing.T) config.Cluster {
	t.Helper()
	dir := t.TempDir()
	oldRecords, oldInstance, oldLocks, oldLoad := backup.RecordsPath, followHostInstance, nodeLockers, loadGuardCluster
	oldTarget, oldSpace, oldNamespace, oldMetrics, oldLaunch := restoreCheckTarget, restoreCheckSpace, restoreCheckNamespace, restoreWriteMetrics, restoreLaunch
	oldFence, oldBackup, oldWritten, oldPublish := recoverCheckFence, recoverCheckBackup, recoverCheckWritten, restorePublish
	t.Cleanup(func() {
		backup.RecordsPath, followHostInstance, nodeLockers, loadGuardCluster = oldRecords, oldInstance, oldLocks, oldLoad
		restoreCheckTarget, restoreCheckSpace, restoreCheckNamespace, restoreWriteMetrics, restoreLaunch = oldTarget, oldSpace, oldNamespace, oldMetrics, oldLaunch
		recoverCheckFence, recoverCheckBackup, recoverCheckWritten, restorePublish = oldFence, oldBackup, oldWritten, oldPublish
	})
	backup.RecordsPath = filepath.Join(dir, "records.json")
	followHostInstance = func() (string, error) { return "install", nil }
	cfg := config.Cluster{Server: config.Server{DataRoot: dir}, Clusters: []config.ClusterSpec{{Name: "app", Port: 5433, Stanza: "app-abcdef"}}}
	cfg.Backup.Provider = "r2"
	loadGuardCluster = func() (config.Cluster, error) { return cfg, nil }
	restoreCheckTarget = func(context.Context, config.Cluster, restoreParams, map[string]string) (string, error) {
		return "latest", nil
	}
	restoreCheckSpace = func(context.Context, config.Cluster, string) ([]backup.SpaceNeed, error) { return nil, nil }
	restoreCheckNamespace = func(context.Context, config.Cluster, string) error { return nil }
	restoreWriteMetrics = func(config.Cluster) {}
	recoverCheckFence = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	recoverCheckBackup = func(context.Context, config.ClusterSpec, backup.Records) error { return nil }
	recoverCheckWritten = func(context.Context, config.Cluster, config.ClusterSpec) error { return nil }
	return cfg
}

// requireAwaitingRecovery records app as awaiting recovery, which recover
// preparation requires.
func requireAwaitingRecovery(t *testing.T) {
	t.Helper()
	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abcdef", Since: "today"}
		return nil
	}))
}

// preparationConcurrency tracks the locks, publications and launches of two
// competing preparation handlers, so the test can prove the second waits.
type preparationConcurrency struct {
	mu            sync.Mutex
	node, db      sync.RWMutex
	target        sync.RWMutex
	held          map[string][]string
	entered       chan struct{}
	proceed       chan struct{}
	secondWaiting chan struct{}
	publishCount  int
	launchCount   int
}

func newPreparationConcurrency() *preparationConcurrency {
	return &preparationConcurrency{
		held:          map[string][]string{},
		entered:       make(chan struct{}),
		proceed:       make(chan struct{}),
		secondWaiting: make(chan struct{}),
	}
}

// acquire takes the named lock and records it under the request id.
func (c *preparationConcurrency) acquire(name string, rw *sync.RWMutex, mode engine.LockMode, id string) engine.Release {
	if mode == engine.LockExclusive {
		rw.Lock()
	} else {
		rw.RLock()
	}
	c.mu.Lock()
	c.held[id] = append(c.held[id], name)
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		list := c.held[id]
		c.held[id] = list[:len(list)-1]
		c.mu.Unlock()
		if mode == engine.LockExclusive {
			rw.Unlock()
		} else {
			rw.RUnlock()
		}
	}
}

// installLocks serves the node, database and target locks from acquire, and
// signals when the second request waits on the target.
func (c *preparationConcurrency) installLocks(t *testing.T) {
	nodeLockers.node = func(_ context.Context, mode engine.LockMode, _, id string, _ time.Duration) (engine.Release, error) {
		return c.acquire("node", &c.node, mode, id), nil
	}
	nodeLockers.cluster = func(_ context.Context, name string, mode engine.LockMode, _, id string, _ time.Duration) (engine.Release, error) {
		if name == "app" {
			if mode != engine.LockShared {
				t.Errorf("database lock is not shared")
			}
			return c.acquire(name, &c.db, mode, id), nil
		}
		if name != "app__restoring" || mode != engine.LockExclusive {
			t.Errorf("unexpected target lock %s %s", name, mode)
		}
		if id == "job2" {
			close(c.secondWaiting)
		}
		return c.acquire(name, &c.target, mode, id), nil
	}
}

// installPublication checks the first publication holds all three locks and
// keeps the intent open until the test releases it.
func (c *preparationConcurrency) installPublication(t *testing.T) {
	restorePublish = func(fn func(*backup.Records) error) error {
		c.mu.Lock()
		c.publishCount++
		first := c.publishCount == 1
		locks := append([]string(nil), c.held["job1"]...)
		c.mu.Unlock()
		if first {
			if len(locks) != 3 || locks[0] != "node" || locks[1] != "app" || locks[2] != "app__restoring" {
				t.Errorf("publication locks: %v", locks)
			}
			close(c.entered)
			<-c.proceed
		}
		return backup.UpdateRecords(fn)
	}
}

// installLaunch checks the runner launches only after publication released the
// preparation locks.
func (c *preparationConcurrency) installLaunch(t *testing.T) {
	restoreLaunch = func(_ context.Context, env *Env, name string, _ any, _ map[string]string) (Result, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.launchCount++
		if len(c.held[env.Req.RequestID]) != 0 {
			t.Errorf("launch under preparation locks")
		}
		r, err := backup.LoadRecords()
		if err != nil || r.Restores["app"].ID != env.Req.RequestID || r.Restores["app"].InstanceID != "install" || r.Restores["app"].OperationID != "operation" {
			t.Errorf("launch without published intent: %+v %v", r.Restores["app"], err)
		}
		return Result{}, nil
	}
}

// runRestorePreparation invokes the preparation handler for one request.
func runRestorePreparation(cfg config.Cluster, job, id string) error {
	params := json.RawMessage(`{"database":"app","kind":"latest"}`)
	if job == "recover" {
		params = json.RawMessage(`{"database":"app"}`)
	}
	env := &Env{Cluster: cfg, Req: rpc.Request{RequestID: id, OperationID: "operation", Confirm: "app/restore", Params: params}}
	if job == "recover" {
		_, err := handleDBRecover(context.Background(), env)
		return err
	}
	_, err := handleRestoreStart(context.Background(), env)
	return err
}

func TestRestoreRecoverRunnerInvocationFenceAndLease(t *testing.T) {
	for _, kind := range []string{"restore", "dump", "recover"} {
		for _, scenario := range []string{"matching", "missing", "replaced", "install", "install-changed", "operation", "legacy", "type", "kind", "phase", "prelock-operation"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				assertRunnerInvocationFence(t, kind, scenario)
			})
		}
	}
}

// runnerInvocation collects the counters, terminal job state and stop sentinel
// of one runner invocation.
type runnerInvocation struct {
	state         engine.State
	stop          error
	acquired      int
	released      int
	scaffold      int
	constructors  int
	initialLeases int
}

// invocationCase maps a runner kind to the job name, restore target and params
// the fixture publishes.
func invocationCase(kind string) (string, string, json.RawMessage) {
	job, restoreKind := kind, backup.TargetLatest
	params := json.RawMessage(`{"database":"app","kind":"latest"}`)
	switch kind {
	case "dump":
		job, restoreKind = "restore", targetDump
		params = json.RawMessage(`{"database":"app","kind":"dump","dump":"dump1"}`)
	case "recover":
		params = json.RawMessage(`{"database":"app"}`)
	}
	return job, restoreKind, params
}

// invocationRecord returns the record the scenario publishes and whether it
// publishes one at all.
func invocationRecord(env *jobEnv, job, restoreKind, scenario string) (backup.Restore, bool) {
	record := backup.Restore{ID: env.ID, Database: "app", Kind: restoreKind, Phase: backup.PhaseRestoring,
		InstanceID: env.installID, OperationID: env.OperationID, Recover: job == "recover", Promote: job == "recover"}
	switch scenario {
	case "replaced":
		record.ID = "new-job"
	case "install":
		record.InstanceID = "other-install"
	case "operation", "prelock-operation":
		record.OperationID = "other-operation"
	case "legacy":
		record.InstanceID, record.OperationID = "", ""
	case "type":
		record.Recover = !record.Recover
	case "kind":
		record.Kind = "different-kind"
	case "phase":
		record.Phase = backup.PhaseReady
	}
	return record, scenario != "missing"
}

// installRunnerInvocation replaces the runner's host-facing reads and returns
// the invocation's counters and state.
func installRunnerInvocation(t *testing.T, scenario, dir string, env *jobEnv, job string, params json.RawMessage, cfg config.Cluster) *runnerInvocation {
	t.Helper()
	oldStat, oldUID, oldRead, oldLoad, oldUpdate, oldFence := jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster, jobUpdateState, jobFenceCheck
	oldLocks, oldActivity, oldSettle := nodeLockers, activityRecorderFactory, settleJobRecord
	oldScaffold, oldHost, oldConstructor, oldOwned := recoverScaffoldApply, jobRuntimeHostRead, callerNewMaterialised, callerNewRestoreOwned
	t.Cleanup(func() {
		jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster, jobUpdateState, jobFenceCheck = oldStat, oldUID, oldRead, oldLoad, oldUpdate, oldFence
		nodeLockers, activityRecorderFactory, settleJobRecord = oldLocks, oldActivity, oldSettle
		recoverScaffoldApply, jobRuntimeHostRead, callerNewMaterialised, callerNewRestoreOwned = oldScaffold, oldHost, oldConstructor, oldOwned
	})
	inv := &runnerInvocation{stop: errors.New("test stops before host mutation")}
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(dir) }
	if scenario == "install-changed" {
		calls := 0
		followHostInstance = func() (string, error) {
			calls++
			if calls > 1 {
				return "new-install", nil
			}
			return env.installID, nil
		}
	}
	jobEffectiveUID = func() int { return 0 }
	jobReadSpec = func(string) (jobSpec, error) {
		return jobSpec{Job: job, Params: params, OperationID: env.OperationID}, nil
	}
	jobLoadCluster = func(string) (config.Cluster, error) { return cfg, nil }
	jobUpdateState = func(fn func(*engine.State) error) error { return fn(&inv.state) }
	jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
	settleJobRecord = func(string, bool) error { return nil }
	take := func() engine.Release { inv.acquired++; return func() { inv.released++ } }
	nodeLockers.node = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		if scenario == "prelock-operation" {
			return nil, errors.New("lock acquisition failed")
		}
		return take(), nil
	}
	nodeLockers.cluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return take(), nil
	}
	nodeLockers.heavyWait = func(context.Context, string, string, time.Duration) (engine.Release, error) { return take(), nil }
	jobRuntimeHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
	recoverScaffoldApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		inv.scaffold++
		return engine.ApplyData{}, nil, inv.stop
	}
	callerNewMaterialised = func(string, string, string, int, engine.Shell, string, engine.Reporter) (*backup.Materialised, error) {
		inv.constructors++
		return nil, inv.stop
	}
	callerNewRestoreOwned = callerNewMaterialised
	return inv
}

// assertRunnerInvocationFence drives one runner invocation and checks its
// effects, leases and records.
func assertRunnerInvocationFence(t *testing.T, kind, scenario string) {
	t.Helper()
	env, dir := attemptFixture(t)
	cfg := env.Cluster
	cfg.Backup.Provider = "r2"
	job, restoreKind, params := invocationCase(kind)
	record, publish := invocationRecord(env, job, restoreKind, scenario)
	if publish {
		mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = record; return nil }))
	}
	inv := installRunnerInvocation(t, scenario, dir, env, job, params, cfg)
	lockedJobs.Lock()
	inv.initialLeases = len(lockedJobs.active)
	lockedJobs.Unlock()
	_, skipped, err := RunJob(context.Background(), job, JobOptions{ID: env.ID}, nil)
	assertInvocationEffects(t, scenario, job, inv, skipped, err)
	assertInvocationLeases(t, inv.initialLeases)
	assertInvocationRecords(t, scenario, env, record, inv.state)
}

// assertInvocationEffects checks the runner's result, its host effects and its
// lock accounting.
func assertInvocationEffects(t *testing.T, scenario, job string, inv *runnerInvocation, skipped string, err error) {
	t.Helper()
	rejectf(t, err == nil || skipped != "", "runner result err=%v skipped=%q", err, skipped)
	if scenario == "matching" {
		if !errors.Is(err, inv.stop) {
			t.Fatalf("matching invocation did not reach job body: %v", err)
		}
		rejectf(t, job == "recover" && (inv.scaffold != 1 || inv.constructors != 0), "scaffold=%d constructors=%d", inv.scaffold, inv.constructors)
		rejectf(t, job == "restore" && (inv.constructors != 1 || inv.scaffold != 0), "scaffold=%d constructors=%d", inv.scaffold, inv.constructors)
	} else if inv.scaffold != 0 || inv.constructors != 0 {
		t.Fatalf("refused invocation reached host effects: scaffold=%d constructors=%d", inv.scaffold, inv.constructors)
	}
	rejectf(t, inv.acquired != inv.released || (scenario != "prelock-operation" && inv.acquired < 3), "locks acquired=%d released=%d", inv.acquired, inv.released)
}

// assertInvocationLeases checks the runner finalized the job lease.
func assertInvocationLeases(t *testing.T, initial int) {
	t.Helper()
	lockedJobs.Lock()
	leases := len(lockedJobs.active)
	lockedJobs.Unlock()
	rejectf(t, leases != initial, "lease not finalized: before=%d after=%d", initial, leases)
}

// assertInvocationRecords checks the published record and the terminal job.
func assertInvocationRecords(t *testing.T, scenario string, env *jobEnv, record backup.Restore, state engine.State) {
	t.Helper()
	stored, err := backup.LoadRecords()
	reject(t, err != nil, err)
	got, found := stored.Restores["app"]
	reject(t, scenario == "missing" && found, "missing record recreated")
	rejectf(t, scenario != "missing" && scenario != "matching" && !reflect.DeepEqual(got, record), "foreign record modified: before=%+v after=%+v", record, got)
	terminal, found := state.Job(env.ID)
	rejectf(t, !found || terminal.State != engine.JobFailed, "terminal job: %+v", terminal)
}

func TestRestoreRecoverPreparationCompareCheckedPublication(t *testing.T) {
	for _, job := range []string{"restore", "recover"} {
		t.Run(job, func(t *testing.T) {
			cfg := setupRestorePreparation(t)
			if job == "recover" {
				requireAwaitingRecovery(t)
			}
			nodeLockers.node = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
				return func() {}, nil
			}
			nodeLockers.cluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
				return func() {}, nil
			}
			foreign := backup.Restore{ID: "foreign", Database: "app", Phase: backup.PhaseRestoring}
			restorePublish = func(fn func(*backup.Records) error) error {
				if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = foreign; return nil }); err != nil {
					return err
				}
				return backup.UpdateRecords(fn)
			}
			launches := 0
			restoreLaunch = func(context.Context, *Env, string, any, map[string]string) (Result, error) {
				launches++
				return Result{}, nil
			}
			params := json.RawMessage(`{"database":"app","kind":"latest"}`)
			if job == "recover" {
				params = json.RawMessage(`{"database":"app"}`)
			}
			env := &Env{Cluster: cfg, Req: rpc.Request{RequestID: "job", Confirm: "app/restore", Params: params}}
			var err error
			if job == "restore" {
				_, err = handleRestoreStart(context.Background(), env)
			} else {
				_, err = handleDBRecover(context.Background(), env)
			}
			rejectf(t, err == nil || launches != 0, "err=%v launches=%d", err, launches)
			got, err := backup.LoadRecords()
			rejectf(t, err != nil || !reflect.DeepEqual(got.Restores["app"], foreign), "foreign record changed: %+v %v", got.Restores["app"], err)
		})
	}
}
