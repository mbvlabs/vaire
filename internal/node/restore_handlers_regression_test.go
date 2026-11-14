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
			if pair[0] == "recover" || pair[1] == "recover" {
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abcdef", Since: "today"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
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

			var mu sync.Mutex
			var node, db, target sync.RWMutex
			held := map[string][]string{}
			entered, proceed, secondWaiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
			acquire := func(name string, rw *sync.RWMutex, mode engine.LockMode, id string) engine.Release {
				if mode == engine.LockExclusive {
					rw.Lock()
				} else {
					rw.RLock()
				}
				mu.Lock()
				held[id] = append(held[id], name)
				mu.Unlock()
				return func() {
					mu.Lock()
					list := held[id]
					held[id] = list[:len(list)-1]
					mu.Unlock()
					if mode == engine.LockExclusive {
						rw.Unlock()
					} else {
						rw.RUnlock()
					}
				}
			}
			nodeLockers.node = func(_ context.Context, mode engine.LockMode, _, id string, _ time.Duration) (engine.Release, error) {
				return acquire("node", &node, mode, id), nil
			}
			nodeLockers.cluster = func(_ context.Context, name string, mode engine.LockMode, _, id string, _ time.Duration) (engine.Release, error) {
				if name == "app" {
					if mode != engine.LockShared {
						t.Errorf("database lock is not shared")
					}
					return acquire(name, &db, mode, id), nil
				}
				if name != "app__restoring" || mode != engine.LockExclusive {
					t.Errorf("unexpected target lock %s %s", name, mode)
				}
				if id == "job2" {
					close(secondWaiting)
				}
				return acquire(name, &target, mode, id), nil
			}
			var publishCount, launchCount int
			restorePublish = func(fn func(*backup.Records) error) error {
				mu.Lock()
				publishCount++
				first := publishCount == 1
				locks := append([]string(nil), held["job1"]...)
				mu.Unlock()
				if first {
					if len(locks) != 3 || locks[0] != "node" || locks[1] != "app" || locks[2] != "app__restoring" {
						t.Errorf("publication locks: %v", locks)
					}
					close(entered)
					<-proceed
				}
				return backup.UpdateRecords(fn)
			}
			restoreLaunch = func(_ context.Context, env *Env, name string, _ any, _ map[string]string) (Result, error) {
				mu.Lock()
				defer mu.Unlock()
				launchCount++
				if len(held[env.Req.RequestID]) != 0 {
					t.Errorf("launch under preparation locks")
				}
				r, err := backup.LoadRecords()
				if err != nil || r.Restores["app"].ID != env.Req.RequestID || r.Restores["app"].InstanceID != "install" || r.Restores["app"].OperationID != "operation" {
					t.Errorf("launch without published intent: %+v %v", r.Restores["app"], err)
				}
				return Result{}, nil
			}
			run := func(id string) error {
				job := pair[0]
				if id == "job2" {
					job = pair[1]
				}
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
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { first <- run("job1") }()
			<-entered
			go func() { second <- run("job2") }()
			<-secondWaiting
			close(proceed)
			if err := <-first; err != nil {
				t.Fatal(err)
			}
			if err := <-second; err == nil {
				t.Fatal("second preparation overwrote an open intent")
			}
			r, err := backup.LoadRecords()
			if err != nil || r.Restores["app"].ID != "job1" || publishCount != 1 || launchCount != 1 {
				t.Fatalf("record=%+v publications=%d launches=%d err=%v", r.Restores["app"], publishCount, launchCount, err)
			}
		})
	}
}

func TestRestoreRecoverRunnerInvocationFenceAndLease(t *testing.T) {
	for _, kind := range []string{"restore", "dump", "recover"} {
		for _, scenario := range []string{"matching", "missing", "replaced", "install", "install-changed", "operation", "legacy", "type", "kind", "phase", "prelock-operation"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				env, dir := attemptFixture(t)
				cfg := env.Cluster
				cfg.Backup.Provider = "r2"
				job := kind
				params := json.RawMessage(`{"database":"app","kind":"latest"}`)
				restoreKind := backup.TargetLatest
				switch kind {
				case "dump":
					job, restoreKind = "restore", targetDump
					params = json.RawMessage(`{"database":"app","kind":"dump","dump":"dump1"}`)
				case "recover":
					params = json.RawMessage(`{"database":"app"}`)
				}
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
				if scenario != "missing" {
					if err := backup.UpdateRecords(func(s *backup.Records) error { s.Restores["app"] = record; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				oldStat, oldUID, oldRead, oldLoad, oldUpdate, oldFence := jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster, jobUpdateState, jobFenceCheck
				oldLocks, oldActivity, oldSettle := nodeLockers, activityRecorderFactory, settleJobRecord
				oldScaffold, oldHost, oldConstructor, oldOwned := recoverScaffoldApply, jobRuntimeHostRead, callerNewMaterialised, callerNewRestoreOwned
				t.Cleanup(func() {
					jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster, jobUpdateState, jobFenceCheck = oldStat, oldUID, oldRead, oldLoad, oldUpdate, oldFence
					nodeLockers, activityRecorderFactory, settleJobRecord = oldLocks, oldActivity, oldSettle
					recoverScaffoldApply, jobRuntimeHostRead, callerNewMaterialised, callerNewRestoreOwned = oldScaffold, oldHost, oldConstructor, oldOwned
				})
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
				var state engine.State
				jobUpdateState = func(fn func(*engine.State) error) error { return fn(&state) }
				jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
				activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
				settleJobRecord = func(string, bool) error { return nil }
				var acquired, released, scaffold, constructors int
				take := func() engine.Release { acquired++; return func() { released++ } }
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
				stop := errors.New("test stops before host mutation")
				jobRuntimeHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{}, nil }
				recoverScaffoldApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
					scaffold++
					return engine.ApplyData{}, nil, stop
				}
				callerNewMaterialised = func(string, string, string, int, engine.Shell, string, engine.Reporter) (*backup.Materialised, error) {
					constructors++
					return nil, stop
				}
				callerNewRestoreOwned = callerNewMaterialised
				lockedJobs.Lock()
				initialLeases := len(lockedJobs.active)
				lockedJobs.Unlock()
				_, skipped, err := RunJob(context.Background(), job, JobOptions{ID: env.ID}, nil)
				if err == nil || skipped != "" {
					t.Fatalf("runner result err=%v skipped=%q", err, skipped)
				}
				if scenario == "matching" {
					if !errors.Is(err, stop) {
						t.Fatalf("matching invocation did not reach job body: %v", err)
					}
					if job == "recover" && (scaffold != 1 || constructors != 0) {
						t.Fatalf("scaffold=%d constructors=%d", scaffold, constructors)
					}
					if job == "restore" && (constructors != 1 || scaffold != 0) {
						t.Fatalf("scaffold=%d constructors=%d", scaffold, constructors)
					}
				} else if scaffold != 0 || constructors != 0 {
					t.Fatalf("refused invocation reached host effects: scaffold=%d constructors=%d", scaffold, constructors)
				}
				if acquired != released || (scenario != "prelock-operation" && acquired < 3) {
					t.Fatalf("locks acquired=%d released=%d", acquired, released)
				}
				lockedJobs.Lock()
				leases := len(lockedJobs.active)
				lockedJobs.Unlock()
				if leases != initialLeases {
					t.Fatalf("lease not finalized: before=%d after=%d", initialLeases, leases)
				}
				stored, err := backup.LoadRecords()
				if err != nil {
					t.Fatal(err)
				}
				got, found := stored.Restores["app"]
				if scenario == "missing" && found {
					t.Fatal("missing record recreated")
				}
				if scenario != "missing" && scenario != "matching" && !reflect.DeepEqual(got, record) {
					t.Fatalf("foreign record modified: before=%+v after=%+v", record, got)
				}
				terminal, found := state.Job(env.ID)
				if !found || terminal.State != engine.JobFailed {
					t.Fatalf("terminal job: %+v", terminal)
				}
			})
		}
	}
}

func TestRestoreRecoverPreparationCompareCheckedPublication(t *testing.T) {
	for _, job := range []string{"restore", "recover"} {
		t.Run(job, func(t *testing.T) {
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
			if job == "recover" {
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abcdef", Since: "today"}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
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
			if err == nil || launches != 0 {
				t.Fatalf("err=%v launches=%d", err, launches)
			}
			got, err := backup.LoadRecords()
			if err != nil || !reflect.DeepEqual(got.Restores["app"], foreign) {
				t.Fatalf("foreign record changed: %+v %v", got.Restores["app"], err)
			}
		})
	}
}
