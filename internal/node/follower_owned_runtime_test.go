package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

type followerJobStore struct{ cfg config.Cluster }

func (s *followerJobStore) Load() (config.Cluster, []byte, error) { return s.cfg, nil, nil }
func (*followerJobStore) Save([]byte) error                       { return nil }
func (*followerJobStore) Revision() (string, error)               { return "", nil }

type followerDropStore struct {
	cfg  config.Cluster
	fail bool
}

func (s *followerDropStore) Load() (config.Cluster, []byte, error) { return s.cfg, nil, nil }
func (s *followerDropStore) Save(raw []byte) error {
	if s.fail {
		return errors.New("injected save failure")
	}
	cfg, err := config.ParseCluster(raw)
	if err == nil {
		s.cfg = cfg
	}
	return err
}
func (*followerDropStore) Revision() (string, error) { return "", nil }

type ownedFollowerHost struct {
	dir, data, ports, name string
	uid                    uint32
	after                  func() error
}

func (h *ownedFollowerHost) RegistrationDirectory(string) string       { return h.dir }
func (h *ownedFollowerHost) ReservationStore() (string, uint32, error) { return h.ports, h.uid, nil }
func (*ownedFollowerHost) Stop(context.Context) error                  { return nil }
func (h *ownedFollowerHost) AdoptRegistration(port int) error {
	if err := os.WriteFile(filepath.Join(h.dir, "postgresql.conf"), []byte(fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", h.data, h.name, port)), 0o600); err != nil {
		return err
	}
	if h.after != nil {
		return h.after()
	}
	return nil
}

func TestFollowerOwnedMaterialisationAndResume(t *testing.T) {
	for _, mode := range []string{"success", "retry", "retry-lost-slot", "recovery_requirements_unmet", "recovery_parameter_mismatch", "reboot", "foreign-reservation", "replaced-reservation", "replace-data", "stale-lease", "postguard", "historical-mapping", "legacy-temp", "legacy-reboot", "legacy-final-absent", "legacy-completed-attempt"} {
		t.Run(mode, func(t *testing.T) { runFollowerOwnedScenario(t, mode) })
	}
}

func runFollowerOwnedScenario(t *testing.T, mode string) {
	t.Helper()
	recovering := strings.HasPrefix(mode, "cleanup-")
	runJob := strings.HasPrefix(mode, "job-")
	forceRecoveryFailure := recovering
	owner, key := followRetentionFixture(t, recovering)
	base := filepath.Dir(backup.RecordsPath)
	oldBase, oldConf, oldStateDir, oldUID, oldPG := materialisationAncestorBase, materialisationConfigRoot, materialisationStateDir, materialisationRecordUID, materialisationPostgresUID
	materialisationAncestorBase, materialisationConfigRoot, materialisationStateDir = base, filepath.Join(base, "config"), base
	materialisationRecordUID = uint32(os.Geteuid()) //nolint:gosec // local test uid
	materialisationPostgresUID = func() (uint32, error) { return materialisationRecordUID, nil }
	t.Cleanup(func() {
		materialisationAncestorBase, materialisationConfigRoot, materialisationStateDir, materialisationRecordUID, materialisationPostgresUID = oldBase, oldConf, oldStateDir, oldUID, oldPG
	})
	dataRoot, ports := filepath.Join(base, "data"), filepath.Join(base, "ports")
	for _, dir := range []string{dataRoot, ports, materialisationConfigRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spec := owner.Spec
	spec.Port, spec.Size = 5434, "small"
	if runJob {
		spec.CPUMode, spec.CPUQuota = "spare", "175%"
	}
	owner.Spec = spec
	src := config.ClusterSpec{Name: "source", Port: 5433, Stanza: key.SourceStanza, Size: "small"}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot, cfg.Clusters = dataRoot, []config.ClusterSpec{src, spec}
	if recovering {
		if err := backup.UpdateRecords(func(r *backup.Records) error { r.AwaitingRecovery[spec.Name] = owner.Awaiting; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	params, _ := json.Marshal(dbFollowParams{Name: spec.Name, Source: src.Name, Generation: owner.Generation, Recover: recovering, RecoverSince: owner.Awaiting.Since, From: followFromStanza})
	env := &jobEnv{ID: key.Job, OperationID: key.Operation, installID: "install", Job: "db-follow", Cluster: cfg, Params: params}
	acquired, released := 0, 0
	take := func() engine.Release { acquired++; return func() { released++ } }
	lockers := jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
		cluster: func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
	}
	var lease *lockedJobLease
	var err error
	if !runJob {
		lease, _, err = takeJobLocks(context.Background(), env, lockers)
		if err != nil {
			t.Fatal(err)
		}
		env.lease = lease
		t.Cleanup(lease.finish)
	}
	oldNew, oldResume, oldStandby, oldRestore := newFollowerCopy, resumeFollowerCopy, followerStandbyCopy, followerRestoreCopy
	oldSlots, oldRegistered, oldData, oldConfDir := followerSlotOps, followConfRegistered, followRegisteredData, followConfDirectory
	oldHost := materialisationHostInfo
	t.Cleanup(func() {
		newFollowerCopy, resumeFollowerCopy, followerStandbyCopy, followerRestoreCopy = oldNew, oldResume, oldStandby, oldRestore
		followerSlotOps, followConfRegistered, followRegisteredData, followConfDirectory = oldSlots, oldRegistered, oldData, oldConfDir
		materialisationHostInfo = oldHost
	})
	materialisationHostInfo = func(string) (hostinfo.Info, error) { return hostinfo.Info{RAMMiB: 8192, VCPU: 4}, nil }
	slotState := followerSlotState{Exists: true, Usable: true}
	checkFreshSlot := false
	followerSlotOps.inspect = func(context.Context, config.ClusterSpec, string) (followerSlotState, error) { return slotState, nil }
	followerSlotOps.create = func(context.Context, config.ClusterSpec, string) error {
		if checkFreshSlot {
			if _, err := os.Stat(filepath.Join(dataRoot, spec.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("fresh slot created before owned cleanup")
			}
		}
		return nil
	}
	followerSlotOps.reset = func(context.Context, config.ClusterSpec, string) error { return nil }
	followerSlotOps.drop = func(context.Context, config.ClusterSpec, string) error { t.Fatal("unexpected slot drop"); return nil }
	path := filepath.Join(materialisationConfigRoot, spec.Name)
	data := filepath.Join(dataRoot, spec.Name)
	followConfRegistered = func(string) bool { _, err := os.Stat(filepath.Join(path, "postgresql.conf")); return err == nil }
	followConfDirectory = func(string) (bool, error) {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	followRegisteredData = func(string) (string, error) {
		if followConfRegistered(spec.Name) {
			return data, nil
		}
		return "", nil
	}
	uid := materialisationRecordUID
	newFollowerCopy = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (followerCopy, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: ports, ConfigRoot: materialisationConfigRoot, UID: uid,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
		})
	}
	host := &ownedFollowerHost{dir: path, data: data, ports: ports, name: spec.Name, uid: uid}
	resumeCalls := 0
	resumeFollowerCopy = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) followerCopy {
		resumeCalls++
		switch mode {
		case "legacy-temp", "legacy-reboot", "legacy-final-absent", "job-historical":
			if err := writeFollowOwnership(owner, followPhaseCopied); err != nil {
				t.Fatal(err)
			}
			if mode == "legacy-final-absent" {
				if err := host.AdoptRegistration(spec.Port); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "legacy-temp" && mode != "job-historical" {
				if err := os.Remove(filepath.Join(ports, name)); err != nil {
					t.Fatal(err)
				}
			}
		case "legacy-completed-attempt":
			own, owned, err := readFollowOwnership(owner)
			if err != nil || !owned {
				t.Fatalf("completed record: %v", err)
			}
			own.Attempt.ArtifactStage = ""
			own.Attempt.DataIdentity = materialisationFileID{}
			own.Attempt.ConfigIdentity = materialisationFileID{}
			own.Attempt.ReservationIdentity = backup.ReservationIdentity{}
			if err := writeFollowRetention(own); err != nil {
				t.Fatal(err)
			}
		case "retry", "retry-lost-slot":
			if resumeCalls == 1 {
				host.after = func() error { return errors.New("interrupted adoption") }
			} else {
				host.after = nil
			}
		case "historical-mapping":
			own, owned, err := readFollowOwnership(owner)
			if err != nil || !owned {
				t.Fatalf("completed record: %v", err)
			}
			own.Attempt.TemporaryPort = 0
			own.Attempt.ReservationIdentity = backup.ReservationIdentity{}
			if err := writeFollowRetention(own); err != nil {
				t.Fatal(err)
			}
			if err := host.AdoptRegistration(spec.Port); err != nil {
				t.Fatal(err)
			}
		case "reboot":
			if err := os.Remove(filepath.Join(ports, name)); err != nil {
				t.Fatal(err)
			}
		case "foreign-reservation":
			if err := os.WriteFile(filepath.Join(ports, name), []byte(fmt.Sprintf("%d\n", key.TemporaryPort+1)), 0o600); err != nil {
				t.Fatal(err)
			}
		case "replaced-reservation":
			if err := os.Remove(filepath.Join(ports, name)); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(ports, name), []byte(fmt.Sprintf("%d\n", key.TemporaryPort)), 0o600); err != nil {
				t.Fatal(err)
			}
		case "replace-data":
			if err := os.Rename(data, data+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(data, 0o700); err != nil {
				t.Fatal(err)
			}
		case "stale-lease":
			lease.finish()
		case "postguard":
			host.after = func() error {
				p, err := followOwnershipPath(name)
				if err != nil {
					return err
				}
				raw, err := os.ReadFile(p) //nolint:gosec // test marker
				if err != nil {
					return err
				}
				if err := os.Rename(p, p+"-old"); err != nil {
					return err
				}
				return os.WriteFile(p, raw, 0o600) //nolint:gosec // test-owned marker
			}
		}
		return backup.ResumeMaterialisedWithHost(root, name, source, port, sh, step, report, materialisationConfigRoot, host)
	}
	populate := func(ctx context.Context, m *backup.Materialised) error {
		p := m.OwnershipPaths()
		if m.DestinationConfig == nil {
			t.Fatal("destination config missing")
		}
		if err := m.Ownership.CreateData(ctx, p); err != nil {
			return err
		}
		if err := m.GuardOwnership(ctx, backup.MaterialisationBaseBackup, func() error { return nil }); err != nil {
			return err
		}
		if err := m.Ownership.CreateConfig(ctx, p); err != nil {
			return err
		}
		if err := host.AdoptRegistration(p.Port); err != nil {
			return err
		}
		if err := m.GuardOwnership(ctx, backup.MaterialisationRegister, func() error { return nil }); err != nil {
			return err
		}
		if forceRecoveryFailure {
			return attemptFailure("recovery_parameter_mismatch")
		}
		if mode == "job-failure" {
			return attemptFailure("recovery_parameter_mismatch")
		}
		if mode == "recovery_requirements_unmet" || mode == "recovery_parameter_mismatch" {
			return attemptFailure(mode)
		}
		if err := m.GuardOwnership(ctx, backup.MaterialisationStart, func() error { return nil }); err != nil {
			return err
		}
		return nil
	}
	followerStandbyCopy = func(m *backup.Materialised, ctx context.Context, _ string) error { return populate(ctx, m) }
	followerRestoreCopy = func(m *backup.Materialised, ctx context.Context, _ backup.Driver, _ backup.RestoreOptions) error {
		return populate(ctx, m)
	}
	started := 0
	if runJob {
		oldLocks, oldStat, oldEffective, oldRead, oldLoad := nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster
		oldUpdate, oldFence, oldActivity, oldSettle := jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord
		oldState, oldRunning, oldFloors, oldServer, oldJobHost := followerState, clusterRunning, followerEffectiveFloors, followerServerOps, jobRuntimeHostRead
		t.Cleanup(func() {
			nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster = oldLocks, oldStat, oldEffective, oldRead, oldLoad
			jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord = oldUpdate, oldFence, oldActivity, oldSettle
			followerState, clusterRunning, followerEffectiveFloors, followerServerOps, jobRuntimeHostRead = oldState, oldRunning, oldFloors, oldServer, oldJobHost
		})
		nodeLockers = lockers
		jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(base) }
		jobEffectiveUID = func() int { return 0 }
		jobReadSpec = func(string) (jobSpec, error) {
			return jobSpec{Job: "db-follow", Params: params, OperationID: key.Operation}, nil
		}
		jobLoadCluster = func(string) (config.Cluster, error) { return cfg, nil }
		var jobState engine.State
		jobUpdateState = func(fn func(*engine.State) error) error { return fn(&jobState) }
		jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
		activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
		settleJobRecord = func(string, bool) error { return nil }
		followerState = func() state.Store { return &followerJobStore{cfg: cfg} }
		clusterRunning = func(s config.ClusterSpec) bool { return !s.IsFollower() }
		followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) {
			values := map[string]int{}
			for _, name := range steps.FloorSettings {
				values[name] = 0
			}
			return values, nil
		}
		jobRuntimeHostRead = materialisationHostInfo
		followerServerOps.build = materialiseFollower
		followerServerOps.start = func(_ context.Context, _ *jobEnv, _ config.Cluster, _ config.ClusterSpec, got config.ClusterSpec, _ string) ([]output.Warning, followerReading, error) {
			started++
			if got.CPUMode != spec.CPUMode || got.CPUQuota != spec.CPUQuota {
				t.Fatal("follower CPU policy changed")
			}
			return nil, followerReading{}, nil
		}
		result, skipped, runErr := RunJob(context.Background(), "db-follow", JobOptions{ID: key.Job}, nil)
		if skipped != "" {
			t.Fatalf("job skipped: %s", skipped)
		}
		err = runErr
		rec, found := jobState.Job(key.Job)
		if !found || mode != "job-failure" && rec.State != engine.JobSucceeded || mode == "job-failure" && rec.State != engine.JobFailed {
			t.Fatalf("terminal state %+v", rec)
		}
		if mode == "job-failure" && started != 0 || mode != "job-failure" && started != 1 {
			t.Fatalf("start/exposure count %d", started)
		}
		if mode == "job-historical" {
			data, ok := result.(map[string]any)
			if !ok {
				t.Fatalf("unexpected result: %v", result)
			}
			warnings, ok := data["warnings"].([]output.Warning)
			if !ok || len(warnings) != 1 || warnings[0].Code != "follower_reservation_retained" {
				t.Fatalf("missing retained warning: %v", data)
			}
			if _, owned, err := readFollowOwnership(owner); err != nil || !owned {
				t.Fatalf("historical record lost: %v", err)
			}
		}
	} else {
		err = materialiseFollower(context.Background(), env, cfg, src, spec, followFromStanza, recovering)
	}
	if recovering {
		if recoveryFailureReason(err) != "recovery_parameter_mismatch" {
			t.Fatalf("typed failure: %v", err)
		}
		oldReserve, oldAbsent, oldRegister, oldDrop := followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop
		t.Cleanup(func() {
			followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop = oldReserve, oldAbsent, oldRegister, oldDrop
		})
		followCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
			return backup.CleanupFollowerReservationWithHost(ctx, ports, uid, name, port, id, effect)
		}
		followCleanupAbsent = func(ctx context.Context, name string, port int, present bool) (func(), error) {
			return backup.LockForkReservationWithHost(ctx, ports, uid, name, port, present)
		}
		followCleanupRegistration = func(name, source, root string, sourcePort, port int) error {
			return backup.ResumeMaterialisedWithHost(root, name, source, sourcePort, engine.Shell{}, "", nil, materialisationConfigRoot, host).VerifyFollowerRegistration(port)
		}
		followCleanupDrop = func(context.Context, *Env, string) error { return nil }
		if mode == "cleanup-dropfail" {
			followCleanupDrop = func(context.Context, *Env, string) error { return errors.New("injected cleanup failure") }
		}
		if mode == "cleanup-partial-effect" {
			followCleanupDrop = func(context.Context, *Env, string) error {
				if err := os.RemoveAll(data); err != nil {
					return err
				}
				return errors.New("injected partial cleanup failure")
			}
		}
		if mode == "cleanup-replaced-data" {
			if err := os.Rename(data, data+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(data, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "cleanup-replaced-port" {
			p := filepath.Join(ports, spec.Name)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", key.TemporaryPort)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "cleanup-handler-foreign" {
			if err := os.Rename(data, data+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(data, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "cleanup-handler-reservation" {
			p := filepath.Join(ports, spec.Name)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", key.TemporaryPort)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if mode == "cleanup-handler-record" {
			locked := followCleanupReservation
			followCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
				return locked(ctx, name, port, id, func() error {
					p, err := followOwnershipPath(name)
					if err != nil {
						return err
					}
					raw, err := os.ReadFile(p) //nolint:gosec // temporary test record
					if err != nil {
						return err
					}
					if err := os.Rename(p, p+"-old"); err != nil {
						return err
					}
					if err := os.WriteFile(p, raw, 0o600); err != nil { //nolint:gosec // temporary test record
						return err
					}
					return effect()
				})
			}
		}
		if strings.HasPrefix(mode, "cleanup-handler-") {
			// A real db.drop cannot enter while the follower job holds the
			// exclusive node lock. Finalize the test job before invoking it.
			lease.finish()
			oldStore, oldSlot, oldLeft, oldJournal := dbDropStore, dbDropSlot, dbDropLeftovers, dbDropForgetJournal
			oldOther, oldApply, oldRouting, oldMetrics, oldClear := dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred
			t.Cleanup(func() {
				dbDropStore, dbDropSlot, dbDropLeftovers, dbDropForgetJournal = oldStore, oldSlot, oldLeft, oldJournal
				dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred = oldOther, oldApply, oldRouting, oldMetrics, oldClear
			})
			store := &followerDropStore{cfg: cfg, fail: mode == "cleanup-handler-savefail"}
			dbDropStore = func() state.Store { return store }
			slot, routing := 0, 0
			dbDropSlot = func(context.Context, config.Cluster, config.ClusterSpec) *output.Warning { slot++; return nil }
			dbDropLeftovers = func(*Env, string) error { return nil }
			dbDropForgetJournal = func(string, string) error { return nil }
			dbDropOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, string, string) []string {
				return nil
			}
			dbDropApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
				return nil
			}
			dbDropRouting = func(context.Context, *Env, config.Cluster) error { routing++; return nil }
			dbDropMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
				return nil
			}
			dbDropClearDeferred = func(...string) {}
			req := rpc.Request{Params: json.RawMessage(`{"name":"follower"}`)}
			if _, err := handleDBDrop(context.Background(), &Env{Cluster: cfg, Req: req}); err == nil {
				t.Fatal("missing confirmation accepted")
			}
			req.Confirm = "wrong"
			if _, err := handleDBDrop(context.Background(), &Env{Cluster: cfg, Req: req}); err == nil {
				t.Fatal("wrong confirmation accepted")
			}
			req.Confirm = spec.Name
			_, dropErr := handleDBDrop(context.Background(), &Env{Cluster: cfg, Req: req})
			switch mode {
			case "cleanup-handler-success":
				if dropErr != nil || slot != 1 || routing != 1 {
					t.Fatalf("drop: slot=%d routing=%d err=%v", slot, routing, dropErr)
				}
				if _, found, _ := readFollowOwnership(owner); found {
					t.Fatal("cleaned record remains")
				}
				if _, present := store.cfg.ClusterByName(spec.Name); present {
					t.Fatal("config remains")
				}
			case "cleanup-handler-foreign", "cleanup-handler-reservation", "cleanup-handler-record":
				if dropErr == nil || slot != 0 || routing != 0 {
					t.Fatalf("foreign path admitted: %v", dropErr)
				}
				if _, err := os.Stat(data); err != nil {
					t.Fatal("foreign data removed")
				}
				if _, found, err := readFollowOwnership(owner); err != nil || !found {
					t.Fatalf("record lost: %v", err)
				}
			case "cleanup-handler-savefail":
				if dropErr == nil || routing != 0 {
					t.Fatalf("save failure exposed target: %v", dropErr)
				}
				if _, found, err := readFollowOwnership(owner); err != nil || !found {
					t.Fatalf("marker lost after failed save: %v", err)
				}
			}
			lease.finish()
			if acquired != released {
				t.Fatal("lease leaked")
			}
			return
		}
		if mode == "cleanup-foreign-registration" {
			if err := host.AdoptRegistration(key.TemporaryPort + 1); err != nil {
				t.Fatal(err)
			}
		}
		cleanup, openErr := openFollowCleanup(cfg, spec)
		if mode == "cleanup-replaced-data" || mode == "cleanup-foreign-registration" {
			if openErr == nil {
				cleanup.close()
				t.Fatal("foreign artifact accepted")
			}
			if _, err := os.Stat(data); err != nil {
				t.Fatal("foreign data removed")
			}
			lease.finish()
			if acquired != released {
				t.Fatal("lease leaked")
			}
			return
		}
		if openErr != nil || cleanup == nil {
			t.Fatalf("open cleanup: %v", openErr)
		}
		if mode == "cleanup-replaced-record" {
			p, err := followOwnershipPath(spec.Name)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(p) //nolint:gosec // temporary test record
			if err != nil {
				t.Fatal(err)
			} //nolint:gosec // temp test record
			if err := os.Rename(p, p+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, raw, 0o600); err != nil { //nolint:gosec // temporary test record
				t.Fatal(err)
			}
		}
		removeErr := cleanup.remove(context.Background(), &Env{}, cfg)
		if mode == "cleanup-dropfail" || mode == "cleanup-partial-effect" {
			cleanup.close()
			if removeErr == nil {
				t.Fatal("failed cleanup returned success")
			}
			if _, found, err := readFollowOwnership(owner); err != nil || !found {
				t.Fatalf("failed cleanup lost record: %v", err)
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("failed cleanup lost configuration: %v", err)
			}
			lease.finish()
			if acquired != released {
				t.Fatal("lease leaked")
			}
			return
		}
		if mode == "cleanup-replaced-port" || mode == "cleanup-replaced-record" {
			cleanup.close()
			if removeErr == nil {
				t.Fatal("replaced reservation accepted")
			}
			if _, err := os.Stat(data); err != nil {
				t.Fatal("data removed after reservation refusal")
			}
			lease.finish()
			if acquired != released {
				t.Fatal("lease leaked")
			}
			return
		}
		if removeErr != nil {
			cleanup.close()
			t.Fatal(removeErr)
		}
		if err := cleanup.retire(context.Background()); err != nil {
			cleanup.close()
			t.Fatal(err)
		}
		cleanup.close()
		forceRecoveryFailure = false
		mode = "success"
		err = materialiseFollower(context.Background(), env, cfg, src, spec, followFromStanza, true)
	}
	if mode == "retry" || mode == "retry-lost-slot" {
		if err == nil {
			t.Fatal("first adoption did not fail")
		}
		lease.finish()
		env.ID, env.OperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAX", "01ARZ3NDEKTSV4RRFFQ69G5FAY"
		lease, _, err = takeJobLocks(context.Background(), env, lockers)
		if err != nil {
			t.Fatal(err)
		}
		env.lease = lease
		if mode == "retry-lost-slot" {
			slotState = followerSlotState{}
			checkFreshSlot = true
			host.after = nil
			oldReserve, oldAbsent, oldRegister, oldDrop := followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop
			t.Cleanup(func() {
				followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop = oldReserve, oldAbsent, oldRegister, oldDrop
			})
			followCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
				return backup.CleanupFollowerReservationWithHost(ctx, ports, uid, name, port, id, effect)
			}
			followCleanupAbsent = func(ctx context.Context, name string, port int, present bool) (func(), error) {
				return backup.LockForkReservationWithHost(ctx, ports, uid, name, port, present)
			}
			followCleanupRegistration = func(name, source, root string, sourcePort, port int) error {
				return backup.ResumeMaterialisedWithHost(root, name, source, sourcePort, engine.Shell{}, "", nil, materialisationConfigRoot, host).VerifyFollowerRegistration(port)
			}
			followCleanupDrop = func(context.Context, *Env, string) error { return nil }
			preview, err := followerFollowResume(context.Background(), cfg, src, spec)
			if err != nil || preview.Resume || preview.RecopyReason == "" {
				t.Fatalf("lost-slot preview: %+v %v", preview, err)
			}
			if _, err := os.Stat(data); err != nil {
				t.Fatal("dry-run removed data")
			}
		}
		err = materialiseFollower(context.Background(), env, cfg, src, spec, followFromStanza, false)
		if resumeCalls != 2 {
			t.Fatalf("resume calls %d, error: %v", resumeCalls, err)
		}
	}
	switch mode {
	case "success", "reboot", "retry", "retry-lost-slot", "job-success", "legacy-reboot", "legacy-final-absent":
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(ports, spec.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reservation retained: %v", err)
		}
	case "historical-mapping", "legacy-temp", "legacy-completed-attempt", "job-historical":
		if err != nil || mode != "job-historical" && !env.retainedFollowerReservation {
			t.Fatalf("historical mapping: %v", err)
		}
		if _, err := os.Stat(filepath.Join(ports, spec.Name)); err != nil {
			t.Fatalf("historical mapping lost: %v", err)
		}
	case "recovery_requirements_unmet", "recovery_parameter_mismatch", "job-failure":
		reason := mode
		if mode == "job-failure" {
			reason = "recovery_parameter_mismatch"
		}
		if recoveryFailureReason(err) != reason {
			t.Fatalf("typed failure: %v", err)
		}
		own, found, e := readFollowOwnership(owner)
		if e != nil || !found || own.Attempt == nil || own.Attempt.State != followAttemptRetained || own.Attempt.ArtifactStage != materialisationRegistered {
			t.Fatalf("retention: %+v %v", own, e)
		}
	default:
		if err == nil {
			t.Fatalf("%s accepted", mode)
		}
		if _, err := os.Stat(data); err != nil {
			t.Fatalf("data not retained: %v", err)
		}
	}
	if lease != nil {
		lease.finish()
	}
	if acquired != released {
		t.Fatalf("lease release %d/%d", released, acquired)
	}
}

func TestFollowerExactCleanupRefusesReplacement(t *testing.T) {
	for _, mode := range []string{"cleanup-replaced-data", "cleanup-replaced-port", "cleanup-replaced-record", "cleanup-foreign-registration"} {
		t.Run(mode, func(t *testing.T) { runFollowerOwnedScenario(t, mode) })
	}
}

func TestFollowerRunJobOwnedLifecycle(t *testing.T) {
	for _, mode := range []string{"job-success", "job-failure", "job-historical"} {
		t.Run(mode, func(t *testing.T) { runFollowerOwnedScenario(t, mode) })
	}
}

func TestFollowerAdoptedStartOnlyRequiresFinalRegistration(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	node.copyReady(cfg, "app_follower")
	markAdoptedForTest(t, "app_follower")
	installRetainingSlot(t)
	src, _ := cfg.ClusterByName("app")
	spec, _ := cfg.ClusterByName("app_follower")
	decision, err := decideFollowerBuild(context.Background(), cfg, src, spec, true, false)
	if err != nil || !decision.StartOnly {
		t.Fatalf("adopted follower: %+v %v", decision, err)
	}
	path := filepath.Join(node.confDir(spec.Name), "postgresql.conf")
	if err := os.WriteFile(path, []byte(fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", cfg.Server.DataDirectory(spec.Name), spec.Name, spec.Port+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := decideFollowerBuild(context.Background(), cfg, src, spec, true, false); err == nil {
		t.Fatal("foreign final port accepted")
	}
}

func TestHandleDBDropOwnedFollower(t *testing.T) {
	for _, mode := range []string{"cleanup-handler-success", "cleanup-handler-foreign", "cleanup-handler-reservation", "cleanup-handler-record", "cleanup-handler-savefail"} {
		t.Run(mode, func(t *testing.T) { runFollowerOwnedScenario(t, mode) })
	}
}
