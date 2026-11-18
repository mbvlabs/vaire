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

// followerOwnedRun is one owned-follower scenario's state across its setup,
// execution and assertion phases.
type followerOwnedRun struct {
	t                    *testing.T
	mode                 string
	recovering           bool
	runJob               bool
	forceRecoveryFailure bool
	owner                followOwner
	key                  followAttemptKey
	base                 string
	dataRoot             string
	ports                string
	uid                  uint32
	path                 string
	data                 string
	spec                 config.ClusterSpec
	src                  config.ClusterSpec
	cfg                  config.Cluster
	env                  *jobEnv
	lockers              jobLockers
	acquired             int
	released             int
	lease                *lockedJobLease
	err                  error
	host                 *ownedFollowerHost
	resumeCalls          int
	slotState            followerSlotState
	checkFreshSlot       bool
	jobState             engine.State
	started              int
	// terminal records that the scenario asserted its own terminal state and
	// returned before the shared result switch.
	terminal bool
}

func runFollowerOwnedScenario(t *testing.T, mode string) {
	t.Helper()
	run := &followerOwnedRun{t: t, mode: mode}
	run.setup()
	run.installCopy()
	run.execute()
	run.verify()
}

func (r *followerOwnedRun) setup() {
	t := r.t
	t.Helper()
	useExtensionFileFixture(t)

	r.recovering = strings.HasPrefix(r.mode, "cleanup-")
	r.runJob = strings.HasPrefix(r.mode, "job-")
	r.forceRecoveryFailure = r.recovering
	r.owner, r.key = followRetentionFixture(t, r.recovering)
	r.base = filepath.Dir(backup.RecordsPath)
	oldBase, oldConf, oldConfBase, oldStateDir, oldUID, oldPG := materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase, materialisationStateDir, materialisationRecordUID, materialisationPostgresUID
	materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase, materialisationStateDir = r.base, filepath.Join(r.base, "config"), r.base, r.base
	materialisationRecordUID = uint32(os.Geteuid())
	materialisationPostgresUID = func() (uint32, error) { return materialisationRecordUID, nil }

	t.Cleanup(func() {
		materialisationAncestorBase, materialisationConfigRoot, materialisationConfigBase, materialisationStateDir, materialisationRecordUID, materialisationPostgresUID = oldBase, oldConf, oldConfBase, oldStateDir, oldUID, oldPG
	})

	r.dataRoot, r.ports = filepath.Join(r.base, "data"), filepath.Join(r.base, "ports")
	for _, dir := range []string{r.dataRoot, r.ports, materialisationConfigRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	r.spec = r.owner.Spec

	r.spec.Port, r.spec.Size = 5434, "small"
	if r.runJob {
		r.spec.CPUMode, r.spec.CPUQuota = "spare", "175%"
	}

	r.owner.Spec = r.spec
	r.src = config.ClusterSpec{Name: "source", Port: 5433, Stanza: r.key.SourceStanza, Size: "small"}
	noExtensions := []string{}
	r.src.RecoveryExtensions = &noExtensions
	r.cfg = config.DefaultCluster()

	r.cfg.Server.DataRoot, r.cfg.Clusters = r.dataRoot, []config.ClusterSpec{r.src, r.spec}
	if r.recovering {
		if err := backup.UpdateRecords(func(recs *backup.Records) error { recs.AwaitingRecovery[r.spec.Name] = r.owner.Awaiting; return nil }); err != nil {
			t.Fatal(err)
		}
	}

	params, _ := json.Marshal(dbFollowParams{Name: r.spec.Name, Source: r.src.Name, Generation: r.owner.Generation, Recover: r.recovering, RecoverSince: r.owner.Awaiting.Since, From: followFromStanza})
	r.env = &jobEnv{ID: r.key.Job, OperationID: r.key.Operation, installID: "install", Job: "db-follow", Cluster: r.cfg, Params: params}
	take := func() engine.Release {
		r.acquired++
		return func() { r.released++ }
	}

	r.lockers = jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
		cluster: func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
	}
	if !r.runJob {
		r.lease, _, r.err = takeJobLocks(context.Background(), r.env, r.lockers)
		reject(t, r.err != nil, r.err)
		r.env.lease = r.lease
		t.Cleanup(r.lease.finish)
	}

	r.uid = materialisationRecordUID
	r.path = filepath.Join(materialisationConfigRoot, r.spec.Name)
	r.data = filepath.Join(r.dataRoot, r.spec.Name)
	r.host = &ownedFollowerHost{dir: r.path, data: r.data, ports: r.ports, name: r.spec.Name, uid: r.uid}
	r.slotState = followerSlotState{Exists: true, Usable: true}
}

// installCopy replaces the physical copy at its narrowest seam, keeping the
// production ownership, admission and retry decisions.
func (r *followerOwnedRun) installCopy() {
	t := r.t
	t.Helper()

	oldNew, oldResume, oldStandby, oldRestore := newFollowerCopy, resumeFollowerCopy, followerStandbyCopy, followerRestoreCopy
	oldSlots, oldRegistered, oldData, oldConfDir := followerSlotOps, followConfRegistered, followRegisteredData, followConfDirectory
	oldHost := materialisationHostInfo

	t.Cleanup(func() {
		newFollowerCopy, resumeFollowerCopy, followerStandbyCopy, followerRestoreCopy = oldNew, oldResume, oldStandby, oldRestore
		followerSlotOps, followConfRegistered, followRegisteredData, followConfDirectory = oldSlots, oldRegistered, oldData, oldConfDir
		materialisationHostInfo = oldHost
	})

	materialisationHostInfo = func(string) (hostinfo.Info, error) { return hostinfo.Info{RAMMiB: 8192, VCPU: 4}, nil }

	r.installConstructors()
	r.installSlotOps()
	r.installRegistrationProbes()
	r.installPopulate()
}

func (r *followerOwnedRun) installConstructors() {
	newFollowerCopy = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (followerCopy, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: r.ports, ConfigRoot: materialisationConfigRoot, UID: r.uid,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
		})
	}
	resumeFollowerCopy = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) followerCopy {
		r.resumeCalls++
		r.arrangeResume(name)

		return backup.ResumeMaterialisedWithHost(root, name, source, port, sh, step, report, materialisationConfigRoot, r.host)
	}
}

// arrangeResume prepares the durable state one interrupted adoption leaves
// before the copy is resumed.
func (r *followerOwnedRun) arrangeResume(name string) {
	switch r.mode {
	case "legacy-temp", "legacy-reboot", "legacy-final-absent", "job-historical":
		r.arrangeLegacyAdoption(name)
	case "legacy-completed-attempt":
		r.arrangeCompletedAttempt()
	case "retry", "retry-lost-slot":
		r.arrangeRetryAdoption()
	case "historical-mapping":
		r.arrangeHistoricalMapping()
	case "reboot":
		r.removeReservation(name)
	case "foreign-reservation":
		if err := os.WriteFile(filepath.Join(r.ports, name), []byte(fmt.Sprintf("%d\n", r.key.TemporaryPort+1)), 0o600); err != nil {
			r.t.Fatal(err)
		}
	case "replaced-reservation":
		r.removeReservation(name)

		if err := os.WriteFile(filepath.Join(r.ports, name), []byte(fmt.Sprintf("%d\n", r.key.TemporaryPort)), 0o600); err != nil {
			r.t.Fatal(err)
		}
	case "replace-data":
		r.replaceData()
	case "stale-lease":
		r.lease.finish()
	case "postguard":
		r.arrangePostguard(name)
	}
}

func (r *followerOwnedRun) arrangeLegacyAdoption(name string) {
	t := r.t
	if err := writeFollowOwnership(r.owner, followPhaseCopied); err != nil {
		t.Fatal(err)
	}

	if r.mode == "legacy-final-absent" {
		if err := r.host.AdoptRegistration(r.spec.Port); err != nil {
			t.Fatal(err)
		}
	}

	if r.mode != "legacy-temp" && r.mode != "job-historical" {
		r.removeReservation(name)
	}
}

func (r *followerOwnedRun) arrangeCompletedAttempt() {
	r.mutateOwnership(func(own *followOwnership) {
		own.Attempt.ArtifactStage = ""
		own.Attempt.DataIdentity = materialisationFileID{}
		own.Attempt.ConfigIdentity = materialisationFileID{}
		own.Attempt.ReservationIdentity = backup.ReservationIdentity{}
	})
}

func (r *followerOwnedRun) arrangeRetryAdoption() {
	if r.resumeCalls == 1 {
		r.host.after = func() error { return errors.New("interrupted adoption") }
	} else {
		r.host.after = nil
	}
}

func (r *followerOwnedRun) arrangeHistoricalMapping() {
	t := r.t
	r.mutateOwnership(func(own *followOwnership) {
		own.Attempt.TemporaryPort = 0
		own.Attempt.ReservationIdentity = backup.ReservationIdentity{}
	})

	if err := r.host.AdoptRegistration(r.spec.Port); err != nil {
		t.Fatal(err)
	}
}

func (r *followerOwnedRun) removeReservation(name string) {
	if err := os.Remove(filepath.Join(r.ports, name)); err != nil {
		r.t.Fatal(err)
	}
}

func (r *followerOwnedRun) arrangePostguard(name string) {
	r.host.after = func() error {
		p, err := followOwnershipPath(name)
		if err != nil {
			return err
		}

		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}

		if err := os.Rename(p, p+"-old"); err != nil {
			return err
		}

		return os.WriteFile(p, raw, 0o600)
	}
}

func (r *followerOwnedRun) mutateOwnership(mutate func(*followOwnership)) {
	t := r.t
	own, owned, err := readFollowOwnership(r.owner)
	rejectf(t, err != nil || !owned, "completed record: %v", err)
	mutate(&own)

	if err := writeFollowRetention(own); err != nil {
		t.Fatal(err)
	}
}

func (r *followerOwnedRun) installSlotOps() {
	followerSlotOps.inspect = func(context.Context, config.ClusterSpec, string) (followerSlotState, error) {
		return r.slotState, nil
	}
	followerSlotOps.create = func(context.Context, config.ClusterSpec, string) error {
		if r.checkFreshSlot {
			if _, err := os.Stat(filepath.Join(r.dataRoot, r.spec.Name)); !errors.Is(err, os.ErrNotExist) {
				r.t.Fatal("fresh slot created before owned cleanup")
			}
		}

		return nil
	}
	followerSlotOps.reset = func(context.Context, config.ClusterSpec, string) error { return nil }
	followerSlotOps.drop = func(context.Context, config.ClusterSpec, string) error {
		r.t.Fatal("unexpected slot drop")
		return nil
	}
}

func (r *followerOwnedRun) installRegistrationProbes() {
	followConfRegistered = func(string) bool {
		_, err := os.Stat(filepath.Join(r.path, "postgresql.conf"))
		return err == nil
	}
	followConfDirectory = func(string) (bool, error) {
		_, err := os.Stat(r.path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return err == nil, err
	}
	followRegisteredData = func(string) (string, error) {
		if followConfRegistered(r.spec.Name) {
			return r.data, nil
		}

		return "", nil
	}
}

func (r *followerOwnedRun) installPopulate() {
	populate := func(ctx context.Context, m *backup.Materialised) error {
		p := m.OwnershipPaths()
		reject(r.t, m.DestinationConfig == nil, "destination config missing")

		if err := m.Ownership.CreateData(ctx, p); err != nil {
			return err
		}

		if err := m.GuardOwnership(ctx, backup.MaterialisationBaseBackup, func() error { return nil }); err != nil {
			return err
		}

		if err := m.Ownership.CreateConfig(ctx, p); err != nil {
			return err
		}

		if err := r.host.AdoptRegistration(p.Port); err != nil {
			return err
		}

		if err := m.GuardOwnership(ctx, backup.MaterialisationRegister, func() error { return nil }); err != nil {
			return err
		}

		if r.forceRecoveryFailure {
			return attemptFailure("recovery_parameter_mismatch")
		}

		if r.mode == "job-failure" {
			return attemptFailure("recovery_parameter_mismatch")
		}

		if r.mode == "recovery_requirements_unmet" || r.mode == "recovery_parameter_mismatch" {
			return attemptFailure(r.mode)
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
}

func (r *followerOwnedRun) execute() {
	if r.runJob {
		r.runOwnedJob()
	} else {
		r.err = materialiseFollower(context.Background(), r.env, r.cfg, r.src, r.spec, followFromStanza, r.recovering)
	}

	if r.recovering {
		r.runRecoveryCleanup()
		return
	}

	if r.mode == "retry" || r.mode == "retry-lost-slot" {
		r.runRetry()
	}
}

func (r *followerOwnedRun) runOwnedJob() {
	t := r.t
	r.installJobRuntime()
	result, skipped, runErr := RunJob(context.Background(), "db-follow", JobOptions{ID: r.key.Job}, nil)
	rejectf(t, skipped != "", "job skipped: %s", skipped)

	r.err = runErr
	rec, found := r.jobState.Job(r.key.Job)
	rejectf(t, !found || r.mode != "job-failure" && rec.State != engine.JobSucceeded || r.mode == "job-failure" && rec.State != engine.JobFailed, "terminal state %+v", rec)
	rejectf(t, r.mode == "job-failure" && r.started != 0 || r.mode != "job-failure" && r.started != 1, "start/exposure count %d", r.started)

	if r.mode == "job-historical" {
		r.checkJobHistorical(result)
	}
}

func (r *followerOwnedRun) installJobRuntime() {
	oldLocks, oldStat, oldEffective, oldRead, oldLoad := nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster
	oldUpdate, oldFence, oldActivity, oldSettle := jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord
	oldState, oldRunning, oldFloors, oldServer, oldJobHost := followerState, clusterRunning, followerEffectiveFloors, followerServerOps, jobRuntimeHostRead

	r.t.Cleanup(func() {
		nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster = oldLocks, oldStat, oldEffective, oldRead, oldLoad
		jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord = oldUpdate, oldFence, oldActivity, oldSettle
		followerState, clusterRunning, followerEffectiveFloors, followerServerOps, jobRuntimeHostRead = oldState, oldRunning, oldFloors, oldServer, oldJobHost
	})
	nodeLockers = r.lockers
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(r.base) }
	jobEffectiveUID = func() int { return 0 }
	jobReadSpec = func(string) (jobSpec, error) {
		return jobSpec{Job: "db-follow", Params: r.env.Params, OperationID: r.key.Operation}, nil
	}
	jobLoadCluster = func(string) (config.Cluster, error) { return r.cfg, nil }
	jobUpdateState = func(fn func(*engine.State) error) error { return fn(&r.jobState) }
	jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(r.t) }
	settleJobRecord = func(string, bool) error { return nil }
	followerState = func() state.Store { return &followerJobStore{cfg: r.cfg} }
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
		r.started++
		reject(r.t, got.CPUMode != r.spec.CPUMode || got.CPUQuota != r.spec.CPUQuota, "follower CPU policy changed")

		return nil, followerReading{}, nil
	}
}

func (r *followerOwnedRun) checkJobHistorical(result any) {
	t := r.t

	data, ok := result.(map[string]any)
	if !ok {
		t.Fatalf("unexpected result: %v", result)
	}

	warnings, ok := data["warnings"].([]output.Warning)
	rejectf(t, !ok || len(warnings) != 1 || warnings[0].Code != "follower_reservation_retained", "missing retained warning: %v", data)

	if _, owned, err := readFollowOwnership(r.owner); err != nil || !owned {
		t.Fatalf("historical record lost: %v", err)
	}
}

func (r *followerOwnedRun) runRecoveryCleanup() {
	t := r.t
	t.Helper()

	if recoveryFailureReason(r.err) != "recovery_parameter_mismatch" {
		t.Fatalf("typed failure: %v", r.err)
	}

	r.installCleanupStubs()
	r.arrangeCleanup()

	if strings.HasPrefix(r.mode, "cleanup-handler-") {
		r.runDropHandler()
		return
	}

	if r.mode == "cleanup-foreign-registration" {
		if err := r.host.AdoptRegistration(r.key.TemporaryPort + 1); err != nil {
			t.Fatal(err)
		}
	}

	r.runCleanup()
}

func (r *followerOwnedRun) installCleanupStubs() {
	oldReserve, oldAbsent, oldRegister, oldDrop := followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop

	r.t.Cleanup(func() {
		followCleanupReservation, followCleanupAbsent, followCleanupRegistration, followCleanupDrop = oldReserve, oldAbsent, oldRegister, oldDrop
	})

	followCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
		return backup.CleanupFollowerReservationWithHost(ctx, r.ports, r.uid, name, port, id, effect)
	}
	followCleanupAbsent = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		return backup.LockForkReservationWithHost(ctx, r.ports, r.uid, name, port, present)
	}
	followCleanupRegistration = func(name, source, root string, sourcePort, port int) error {
		return backup.ResumeMaterialisedWithHost(root, name, source, sourcePort, engine.Shell{}, "", nil, materialisationConfigRoot, r.host).VerifyFollowerRegistration(port)
	}
	followCleanupDrop = func(context.Context, *Env, string) error { return nil }
}

// arrangeCleanup prepares the durable artefacts one cleanup interruption
// leaves.
func (r *followerOwnedRun) arrangeCleanup() {
	switch r.mode {
	case "cleanup-dropfail":
		followCleanupDrop = func(context.Context, *Env, string) error { return errors.New("injected cleanup failure") }
	case "cleanup-partial-effect":
		followCleanupDrop = func(context.Context, *Env, string) error {
			if err := os.RemoveAll(r.data); err != nil {
				return err
			}

			return errors.New("injected partial cleanup failure")
		}
	case "cleanup-replaced-data", "cleanup-handler-foreign":
		r.replaceData()
	case "cleanup-replaced-port", "cleanup-handler-reservation":
		r.replacePortReservation()
	case "cleanup-handler-record":
		r.wrapCleanupReservation()
	}
}

func (r *followerOwnedRun) replaceData() {
	t := r.t
	if err := os.Rename(r.data, r.data+"-old"); err != nil {
		t.Fatal(err)
	}

	if err := os.Mkdir(r.data, 0o700); err != nil {
		t.Fatal(err)
	}
}

func (r *followerOwnedRun) replacePortReservation() {
	t := r.t

	p := filepath.Join(r.ports, r.spec.Name)
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, []byte(fmt.Sprintf("%d\n", r.key.TemporaryPort)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *followerOwnedRun) wrapCleanupReservation() {
	locked := followCleanupReservation
	followCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
		return locked(ctx, name, port, id, func() error {
			p, err := followOwnershipPath(name)
			if err != nil {
				return err
			}

			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}

			if err := os.Rename(p, p+"-old"); err != nil {
				return err
			}

			if err := os.WriteFile(p, raw, 0o600); err != nil {
				return err
			}

			return effect()
		})
	}
}

func (r *followerOwnedRun) runDropHandler() {
	t := r.t
	// A real db.drop cannot enter while the follower job holds the
	// exclusive node lock. Finalize the test job before invoking it.
	r.lease.finish()

	oldStore, oldSlot, oldLeft, oldJournal := dbDropStore, dbDropSlot, dbDropLeftovers, dbDropForgetJournal
	oldOther, oldApply, oldRouting, oldMetrics, oldClear := dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred

	t.Cleanup(func() {
		dbDropStore, dbDropSlot, dbDropLeftovers, dbDropForgetJournal = oldStore, oldSlot, oldLeft, oldJournal
		dbDropOtherConfigs, dbDropApplyOtherConfigs, dbDropRouting, dbDropMetrics, dbDropClearDeferred = oldOther, oldApply, oldRouting, oldMetrics, oldClear
	})

	store := &followerDropStore{cfg: r.cfg, fail: r.mode == "cleanup-handler-savefail"}
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
	if _, err := handleDBDrop(context.Background(), &Env{Cluster: r.cfg, Req: req}); err == nil {
		t.Fatal("missing confirmation accepted")
	}

	req.Confirm = "wrong"
	if _, err := handleDBDrop(context.Background(), &Env{Cluster: r.cfg, Req: req}); err == nil {
		t.Fatal("wrong confirmation accepted")
	}

	req.Confirm = r.spec.Name
	_, dropErr := handleDBDrop(context.Background(), &Env{Cluster: r.cfg, Req: req})
	r.checkDropResult(dropErr, slot, routing, store)
	r.lease.finish()
	reject(t, r.acquired != r.released, "lease leaked")
	r.terminal = true
}

func (r *followerOwnedRun) checkDropResult(dropErr error, slot, routing int, store *followerDropStore) {
	t := r.t
	switch r.mode {
	case "cleanup-handler-success":
		rejectf(t, dropErr != nil || slot != 1 || routing != 1, "drop: slot=%d routing=%d err=%v", slot, routing, dropErr)

		if _, found, _ := readFollowOwnership(r.owner); found {
			t.Fatal("cleaned record remains")
		}

		if _, present := store.cfg.ClusterByName(r.spec.Name); present {
			t.Fatal("config remains")
		}
	case "cleanup-handler-foreign", "cleanup-handler-reservation", "cleanup-handler-record":
		rejectf(t, dropErr == nil || slot != 0 || routing != 0, "foreign path admitted: %v", dropErr)

		if _, err := os.Stat(r.data); err != nil {
			t.Fatal("foreign data removed")
		}

		if _, found, err := readFollowOwnership(r.owner); err != nil || !found {
			t.Fatalf("record lost: %v", err)
		}
	case "cleanup-handler-savefail":
		rejectf(t, dropErr == nil || routing != 0, "save failure exposed target: %v", dropErr)

		if _, found, err := readFollowOwnership(r.owner); err != nil || !found {
			t.Fatalf("marker lost after failed save: %v", err)
		}
	}
}

func (r *followerOwnedRun) runCleanup() {
	t := r.t

	cleanup, openErr := openFollowCleanup(r.cfg, r.spec)
	if r.mode == "cleanup-replaced-data" || r.mode == "cleanup-foreign-registration" {
		if openErr == nil {
			cleanup.close()
			t.Fatal("foreign artifact accepted")
		}

		if _, err := os.Stat(r.data); err != nil {
			t.Fatal("foreign data removed")
		}

		r.lease.finish()
		reject(t, r.acquired != r.released, "lease leaked")
		r.terminal = true

		return
	}

	rejectf(t, openErr != nil || cleanup == nil, "open cleanup: %v", openErr)

	if r.mode == "cleanup-replaced-record" {
		r.replaceOwnershipRecord()
	}

	removeErr := cleanup.remove(context.Background(), &Env{}, r.cfg)
	if r.mode == "cleanup-dropfail" || r.mode == "cleanup-partial-effect" {
		r.expectFailedCleanup(cleanup, removeErr)
		return
	}

	if r.mode == "cleanup-replaced-port" || r.mode == "cleanup-replaced-record" {
		r.expectReplacedReservation(cleanup, removeErr)
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

	r.forceRecoveryFailure = false
	r.mode = "success"
	r.err = materialiseFollower(context.Background(), r.env, r.cfg, r.src, r.spec, followFromStanza, true)
}

func (r *followerOwnedRun) replaceOwnershipRecord() {
	t := r.t

	p, err := followOwnershipPath(r.spec.Name)
	if err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(p)
	reject(t, err != nil, err)

	if err := os.Rename(p, p+"-old"); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (r *followerOwnedRun) expectFailedCleanup(cleanup *followCleanup, removeErr error) {
	t := r.t

	cleanup.close()

	if removeErr == nil {
		t.Fatal("failed cleanup returned success")
	}

	if _, found, err := readFollowOwnership(r.owner); err != nil || !found {
		t.Fatalf("failed cleanup lost record: %v", err)
	}

	if _, err := os.Stat(r.path); err != nil {
		t.Fatalf("failed cleanup lost configuration: %v", err)
	}

	r.lease.finish()
	reject(t, r.acquired != r.released, "lease leaked")
	r.terminal = true
}

func (r *followerOwnedRun) expectReplacedReservation(cleanup *followCleanup, removeErr error) {
	t := r.t

	cleanup.close()

	if removeErr == nil {
		t.Fatal("replaced reservation accepted")
	}

	if _, err := os.Stat(r.data); err != nil {
		t.Fatal("data removed after reservation refusal")
	}

	r.lease.finish()
	reject(t, r.acquired != r.released, "lease leaked")
	r.terminal = true
}

func (r *followerOwnedRun) runRetry() {
	t := r.t
	if r.err == nil {
		t.Fatal("first adoption did not fail")
	}

	r.lease.finish()
	r.env.ID, r.env.OperationID = "01ARZ3NDEKTSV4RRFFQ69G5FAX", "01ARZ3NDEKTSV4RRFFQ69G5FAY"
	r.lease, _, r.err = takeJobLocks(context.Background(), r.env, r.lockers)
	reject(t, r.err != nil, r.err)

	r.env.lease = r.lease
	if r.mode == "retry-lost-slot" {
		r.prepareLostSlot()
	}

	r.err = materialiseFollower(context.Background(), r.env, r.cfg, r.src, r.spec, followFromStanza, false)
	rejectf(t, r.resumeCalls != 2, "resume calls %d, error: %v", r.resumeCalls, r.err)
}

func (r *followerOwnedRun) prepareLostSlot() {
	t := r.t
	r.slotState = followerSlotState{}
	r.checkFreshSlot = true
	r.host.after = nil
	r.installCleanupStubs()
	preview, err := followerFollowResume(context.Background(), r.cfg, r.src, r.spec)
	rejectf(t, err != nil || preview.Resume || preview.RecopyReason == "", "lost-slot preview: %+v %v", preview, err)

	if _, err := os.Stat(r.data); err != nil {
		t.Fatal("dry-run removed data")
	}
}

func (r *followerOwnedRun) verify() {
	t := r.t
	t.Helper()

	if r.terminal {
		return
	}

	switch r.mode {
	case "success", "reboot", "retry", "retry-lost-slot", "job-success", "legacy-reboot", "legacy-final-absent":
		reject(t, r.err != nil, r.err)

		if _, err := os.Stat(filepath.Join(r.ports, r.spec.Name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("reservation retained: %v", err)
		}
	case "historical-mapping", "legacy-temp", "legacy-completed-attempt", "job-historical":
		rejectf(t, r.err != nil || r.mode != "job-historical" && !r.env.retainedFollowerReservation, "historical mapping: %v", r.err)

		if _, err := os.Stat(filepath.Join(r.ports, r.spec.Name)); err != nil {
			t.Fatalf("historical mapping lost: %v", err)
		}
	case "recovery_requirements_unmet", "recovery_parameter_mismatch", "job-failure":
		reason := r.mode
		if r.mode == "job-failure" {
			reason = "recovery_parameter_mismatch"
		}

		rejectf(t, recoveryFailureReason(r.err) != reason, "typed failure: %v", r.err)
		own, found, e := readFollowOwnership(r.owner)
		rejectf(t, e != nil || !found || own.Attempt == nil || own.Attempt.State != followAttemptRetained || own.Attempt.ArtifactStage != materialisationRegistered, "retention: %+v %v", own, e)
	default:
		rejectf(t, r.err == nil, "%s accepted", r.mode)

		if _, err := os.Stat(r.data); err != nil {
			t.Fatalf("data not retained: %v", err)
		}
	}

	if r.lease != nil {
		r.lease.finish()
	}

	rejectf(t, r.acquired != r.released, "lease release %d/%d", r.released, r.acquired)
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
	node.awaitRecovery()
	node.copyReady(cfg)
	markAdoptedForTest(t, "app_follower")
	installRetainingSlot(t)

	src, _ := cfg.ClusterByName("app")
	spec, _ := cfg.ClusterByName("app_follower")
	decision, err := decideFollowerBuild(context.Background(), cfg, src, spec, true, false)
	rejectf(t, err != nil || !decision.StartOnly, "adopted follower: %+v %v", decision, err)

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
