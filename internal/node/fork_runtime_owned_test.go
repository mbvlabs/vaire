package node

import (
	"bytes"
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
	"github.com/mbvlabs/vaire/internal/steps"
)

// ownedForkState carries the fixture, the stubbed runtime observations and the
// target paths shared by the fork-owned runtime assertions.
type ownedForkState struct {
	mode         string
	owner        materialisationOwnership
	root         string
	registry     string
	cfg          config.Cluster
	foreignBytes []byte
	state        engine.State
	seenContext  context.Context
	acquired     int
	released     int
	provisioned  bool
	verified     bool
	exposed      bool
	copyEntered  bool
	dropCalls    int
	forgetCalls  int
}

func runOwnedForkFixture(t *testing.T, mode string) {
	t.Helper()
	s := newOwnedForkFixture(t, mode)
	s.stubJobRuntime(t)
	s.stubForkHooks(t)
	initialLeases := activeJobLeases()
	_, skipped, err := RunJob(context.Background(), "db-fork", JobOptions{ID: s.owner.Job}, nil)
	rejectf(t, skipped != "", "job skipped: %s", skipped)
	s.assertOutcome(t, err)
	reject(t, s.seenContext != nil && hasForkMaterialisationView(s.seenContext, "target"), "authority leaked")
	rejectf(t, s.acquired < 3 || s.acquired != s.released, "lease release %d/%d", s.released, s.acquired)
	reject(t, initialLeases != activeJobLeases(), "runner retained a job lease")
	rec, found := s.state.Job(s.owner.Job)
	rejectf(t, !found || mode == "success" && rec.State != engine.JobSucceeded || mode != "success" && rec.State != engine.JobFailed, "terminal job state: %+v", rec)
}

func activeJobLeases() int {
	lockedJobs.Lock()
	defer lockedJobs.Unlock()
	return len(lockedJobs.active)
}

func newOwnedForkFixture(t *testing.T, mode string) *ownedForkState {
	useTempRecords(t)
	useExtensionFileFixture(t)
	t.Helper()
	owner, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(owner, root); err != nil {
		t.Fatal(err)
	}
	s := &ownedForkState{mode: mode, owner: owner, root: root}
	if strings.HasPrefix(mode, "foreign-marker-") {
		foreign := owner
		switch mode {
		case "foreign-marker-install":
			foreign.Instance = "other"
		case "foreign-marker-job":
			foreign.Job = "other"
		case "foreign-marker-operation":
			foreign.Operation = "other"
		}
		raw, err := json.Marshal(foreign)
		reject(t, err != nil, err)
		path, err := materialisationPath(owner.Target)
		reject(t, err != nil, err)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		s.foreignBytes = raw
	}
	s.registry = filepath.Join(materialisationAncestorBase, "ports")
	if err := os.Mkdir(s.registry, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(materialisationConfigRoot, "source")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "postgresql.conf"), []byte("port = 5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.cfg = config.DefaultCluster()
	s.cfg.Server.DataRoot = root
	s.cfg.Clusters = []config.ClusterSpec{{Name: "source", Stanza: owner.Stanza, Port: 5432, Size: "small"}, {Name: "target", Stanza: "target-abcdef", Port: 5433, Size: "medium"}}
	noExtensions := []string{}
	s.cfg.Clusters[0].RecoveryExtensions = &noExtensions
	return s
}

func (s *ownedForkState) stubJobRuntime(t *testing.T) {
	t.Helper()
	p := dbForkParams{Source: "source", Name: "target"}
	if strings.HasPrefix(s.mode, "recovery_") {
		p.At = "2026-11-01T00:00:00Z"
	}
	params, err := json.Marshal(p)
	reject(t, err != nil, err)
	take := func() engine.Release { s.acquired++; return func() { s.released++ } }
	lockers := jobLockers{
		node: func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
		cluster: func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
			return take(), nil
		},
	}
	oldLocks, oldStat, oldUID, oldRead, oldLoad := nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster
	oldUpdate, oldFence, oldActivity, oldSettle := jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord
	t.Cleanup(func() {
		nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster = oldLocks, oldStat, oldUID, oldRead, oldLoad
		jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord = oldUpdate, oldFence, oldActivity, oldSettle
	})
	nodeLockers = lockers
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(s.root) }
	jobEffectiveUID = func() int { return 0 }
	jobReadSpec = func(string) (jobSpec, error) {
		return jobSpec{Job: "db-fork", Params: params, OperationID: s.owner.Operation}, nil
	}
	jobLoadCluster = func(string) (config.Cluster, error) { return s.cfg, nil }
	jobUpdateState = func(fn func(*engine.State) error) error { return fn(&s.state) }
	jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
	settleJobRecord = func(string, bool) error { return nil }
}

func (s *ownedForkState) stubForkHooks(t *testing.T) {
	t.Helper()
	oldRegistered, oldData := followConfRegistered, followRegisteredData
	followConfRegistered = func(name string) bool {
		_, err := os.Stat(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"))
		return err == nil
	}
	followRegisteredData = func(name string) (string, error) {
		data := s.cfg.Server.DataDirectory(name)
		raw, err := os.ReadFile(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"))
		if err != nil {
			return "", err
		}
		if !strings.Contains(string(raw), data) {
			return "", errors.New("registration data directory mismatch")
		}
		return data, nil
	}
	t.Cleanup(func() { followConfRegistered, followRegisteredData = oldRegistered, oldData })
	oldComplete := forkCompleteActivation
	forkCompleteActivation = func(context.Context, config.ClusterSpec, backup.ActivationFence, *extensionCopyPlan, engine.Runtime, engine.Reporter) (config.Cluster, []output.Warning, error) {
		return s.cfg, nil, nil
	}
	t.Cleanup(func() { forkCompleteActivation = oldComplete })
	oldNew, oldBase, oldRestore, oldPromote, oldRename := forkNewMaterialised, forkBaseBackup, forkRestore, forkPromote, forkRename
	oldDrop, oldForget := forkDrop, forkForgetDatabase
	oldKernel, oldProvision, oldVerify, oldPool := forkApplyKernel, forkProvisionOwned, forkVerifyProvision, forkApplyPoolers
	oldOther, oldApplyOther, oldMetrics, oldFirst, oldWrite := forkOtherConfigs, forkApplyOtherConfigs, forkApplyMetrics, forkFirstBackup, forkWriteMetrics
	oldReservation, oldHost, oldJobHost := forkReservationLock, materialisationHostInfo, jobRuntimeHostRead
	t.Cleanup(func() {
		forkNewMaterialised, forkBaseBackup, forkRestore, forkPromote, forkRename = oldNew, oldBase, oldRestore, oldPromote, oldRename
		forkDrop, forkForgetDatabase = oldDrop, oldForget
		forkApplyKernel, forkProvisionOwned, forkVerifyProvision, forkApplyPoolers = oldKernel, oldProvision, oldVerify, oldPool
		forkOtherConfigs, forkApplyOtherConfigs, forkApplyMetrics, forkFirstBackup, forkWriteMetrics = oldOther, oldApplyOther, oldMetrics, oldFirst, oldWrite
		forkReservationLock, materialisationHostInfo, jobRuntimeHostRead = oldReservation, oldHost, oldJobHost
	})
	host := func(string) (hostinfo.Info, error) { return hostinfo.Info{RAMMiB: 8192, VCPU: 4}, nil }
	materialisationHostInfo, jobRuntimeHostRead = host, host
	forkDrop = func(*backup.Materialised) error {
		s.dropCalls++
		return errors.New("test refuses host cleanup")
	}
	forkForgetDatabase = func(string) error {
		s.forgetCalls++
		return errors.New("test refuses config mutation")
	}
	uid := uint32(os.Geteuid())
	forkReservationLock = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		if s.mode == "retire" && s.verified && !present {
			return nil, errors.New("reservation check refused")
		}
		return backup.LockForkReservationWithHost(ctx, s.registry, uid, name, port, present)
	}
	forkNewMaterialised = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: s.registry, ConfigRoot: materialisationConfigRoot, UID: uid,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
			Running: func(context.Context, string) bool { return false },
			WriteFile: func(path string, data []byte, perm os.FileMode, _, _ string) error {
				return os.WriteFile(path, data, perm)
			},
		})
	}
	forkBaseBackup = func(m *backup.Materialised, ctx context.Context) error { return s.populate(t, ctx, m, false) }
	forkRestore = func(ctx context.Context, _ *jobEnv, _ dbForkParams, _ config.ClusterSpec, m *backup.Materialised) error {
		return s.populate(t, ctx, m, true)
	}
	forkPromote = func(m *backup.Materialised, ctx context.Context) ([]string, error) {
		return nil, m.GuardOwnership(ctx, backup.MaterialisationPromote, func() error { return nil })
	}
	forkRename = func(context.Context, sqlRunner, string, string, map[string]string) error { return nil }
	forkOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, string, string) []string {
		return nil
	}
	forkApplyKernel = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		return engine.ApplyData{}, nil, nil
	}
	forkProvisionOwned = func(ctx context.Context, cfg config.Cluster, _ engine.Runtime, name, _ string, _ engine.Reporter) error {
		o, found, err := readMaterialisation(name, s.root)
		assertForkAuthorityNotAmbient(t, ctx, cfg, name, o, found, err)
		s.seenContext, s.provisioned = ctx, true
		if s.mode == "access" {
			return errors.New("access failed")
		}
		return nil
	}
	forkVerifyProvision = func(context.Context, config.Cluster, engine.Runtime, string) error {
		reject(t, !s.provisioned, "verified before provisioning")
		if _, found, _ := readMaterialisation("target", s.root); !found {
			t.Fatal("marker retired before verification")
		}
		s.verified = true
		return nil
	}
	forkApplyPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		reject(t, !s.verified, "exposed before verification")
		if _, found, _ := readMaterialisation("target", s.root); found {
			t.Fatal("exposed before retirement")
		}
		s.exposed = true
		return nil
	}
	forkApplyMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
		return nil
	}
	forkApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
		return nil
	}
	forkFirstBackup = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string) *output.Warning {
		return nil
	}
	forkWriteMetrics = func(config.Cluster) {}
}

// assertForkAuthorityNotAmbient proves the fork capability is never ambient: a
// fresh context sees no owner view, public apply keeps refusing and a copied
// capability is rejected. Each probe deliberately starts from a new root
// instead of the caller's context, because inheriting the caller's values is
// exactly what the code under test must not do.
//
//nolint:contextcheck // the fresh root is the property under test
func assertForkAuthorityNotAmbient(t *testing.T, ctx context.Context, cfg config.Cluster, name string, o materialisationOwnership, found bool, err error) {
	t.Helper()
	rejectf(t, err != nil || !found || !materialisationOwner(ctx, o) || materialisationOwner(context.Background(), o), "owner view missing: %v", err)
	reject(t, guardPublicMaterialisation(context.Background(), cfg, name) == nil, "public apply inherited authority")
	copied := *mustAs[*forkView](t, ctx.Value(forkViewKey{})).capability
	forged := &forkView{capability: &copied, target: name}
	forged.active.Store(true)
	reject(t, hasForkMaterialisationView(context.WithValue(context.Background(), forkViewKey{}, forged), name), "copied capability accepted")
}

// populate writes the owned data and config artifacts, applying the
// replacement or foreign reservation the mode expects the guards to refuse.
func (s *ownedForkState) populate(t *testing.T, ctx context.Context, m *backup.Materialised, typed bool) error {
	t.Helper()
	s.copyEntered = true
	reject(t, m.DestinationConfig == nil || !strings.Contains(m.DestinationConfig.Tuning, "max_connections = 50"), "target settings missing")
	paths := m.OwnershipPaths()
	if s.mode == "foreign-reservation" {
		path := filepath.Join(s.registry, paths.Name)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", paths.Port)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Ownership.CreateData(ctx, paths); err != nil {
		return err
	}
	if s.mode == "replacement" {
		if err := os.Rename(paths.DataDir, paths.DataDir+"-old"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(paths.DataDir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.GuardOwnership(ctx, backup.MaterialisationBaseBackup, func() error {
		return os.WriteFile(filepath.Join(paths.DataDir, "sentinel"), []byte("data"), 0o600)
	}); err != nil {
		return err
	}
	if err := m.Ownership.CreateConfig(ctx, paths); err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(paths.ConfigDir, "conf.d"), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(paths.ConfigDir, "pg_ctl.conf"), nil, 0o600); err != nil {
		return err
	}
	if err := m.GuardOwnership(ctx, backup.MaterialisationRegister, func() error { return nil }); err != nil {
		return err
	}
	if typed {
		return attemptFailure(s.mode)
	}
	return m.GuardOwnership(ctx, backup.MaterialisationStart, func() error { return nil })
}

func (s *ownedForkState) assertOutcome(t *testing.T, err error) {
	t.Helper()
	if s.mode == "success" {
		if err != nil || !s.exposed || !s.verified {
			t.Fatalf("success: exposed=%v verified=%v err=%v", s.exposed, s.verified, err)
		}
		return
	}
	rejectf(t, err == nil || s.exposed, "failure exposed target: %v", err)
	if s.foreignBytes != nil {
		s.assertForeignMarker(t)
		return
	}
	s.assertFailureMarker(t)
}

func (s *ownedForkState) assertForeignMarker(t *testing.T) {
	t.Helper()
	path, _ := materialisationPath(s.owner.Target)
	raw, readErr := os.ReadFile(path)
	rejectf(t, readErr != nil || !bytes.Equal(raw, s.foreignBytes), "foreign marker changed: %v", readErr)
	reject(t, s.provisioned, "foreign marker reached provisioning")
	reject(t, s.copyEntered, "foreign marker reached materialisation")
}

func (s *ownedForkState) assertFailureMarker(t *testing.T) {
	t.Helper()
	o, found, readErr := readMaterialisation("target", s.root)
	rejectf(t, readErr != nil || !found, "marker lost: %v", readErr)
	rejectf(t, strings.HasPrefix(s.mode, "recovery_") && (o.Phase != materialisationFailed || o.Reason != s.mode), "typed marker: %+v", o)
	if strings.HasPrefix(s.mode, "recovery_") {
		s.assertRecoveryEvidence(t, o)
	}
	reject(t, s.mode == "access" && !s.provisioned, "access callback not reached")
	reject(t, s.mode == "retire" && (!s.provisioned || !s.verified), "retirement failure did not follow verification")
	if s.mode == "replacement" {
		s.assertReplacement(t)
	}
	if s.mode == "foreign-reservation" {
		s.assertForeignReservation(t)
	}
}

func (s *ownedForkState) assertRecoveryEvidence(t *testing.T, o materialisationOwnership) {
	t.Helper()
	if o.ArtifactStage != materialisationRegistered || o.TemporaryPort == 0 || o.DataIdentity.Inode == 0 || o.ConfigIdentity.Inode == 0 {
		t.Fatalf("missing immutable birth evidence: %+v", o)
	}
	if _, err := os.Stat(filepath.Join(o.DataPath, "sentinel")); err != nil {
		t.Fatalf("partial target lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.registry, o.Target)); err != nil {
		t.Fatalf("reservation lost: %v", err)
	}
	reject(t, s.dropCalls != 0 || s.forgetCalls != 0, "typed failure attempted cleanup")
}

func (s *ownedForkState) assertReplacement(t *testing.T) {
	t.Helper()
	if !s.copyEntered {
		t.Fatal("replacement was not attempted")
	}
	if _, err := os.Stat(s.owner.DataPath + "-old"); err != nil {
		t.Fatalf("original birth artifact lost: %v", err)
	}
	if _, err := os.Stat(s.owner.DataPath); err != nil {
		t.Fatalf("foreign replacement lost: %v", err)
	}
}

func (s *ownedForkState) assertForeignReservation(t *testing.T) {
	t.Helper()
	if !s.copyEntered {
		t.Fatal("reservation replacement was not attempted")
	}
	if _, err := os.Stat(filepath.Join(s.registry, s.owner.Target)); err != nil {
		t.Fatalf("foreign reservation removed: %v", err)
	}
}

type verifyForkStep struct {
	id     string
	result engine.CheckResult
}

func (s verifyForkStep) ID() string                   { return s.id }
func (verifyForkStep) Describe(config.Cluster) string { return "" }
func (s verifyForkStep) Inputs(config.Cluster, engine.Runtime) ([]byte, error) {
	return []byte(s.id), nil
}
func (s verifyForkStep) Check(context.Context, config.Cluster, engine.Runtime) (engine.CheckResult, error) {
	return s.result, nil
}
func (verifyForkStep) Apply(context.Context, config.Cluster, engine.Runtime, engine.Reporter) (engine.ApplyResult, error) {
	return engine.ApplyResult{}, errors.New("verification must not apply")
}

type verifyForkAccess struct{ verifyForkStep }

func (verifyForkAccess) AccessTarget() string { return "target" }

func TestForkProvisionVerificationRequiresRolesAndAccess(t *testing.T) {
	roles := verifyForkStep{id: steps.RolesStepID("target"), result: engine.CheckResult{Complete: true}}
	access := verifyForkAccess{verifyForkStep{id: engine.InstanceID("postgres-access-v1", "target"), result: engine.CheckResult{Complete: true}}}
	check := func(catalog []engine.Step) error {
		return verifyForkProvisionWith(context.Background(), config.Cluster{}, engine.Runtime{}, "target", catalog)
	}
	if err := check([]engine.Step{roles, access}); err != nil {
		t.Fatal(err)
	}
	if err := check([]engine.Step{roles}); err == nil {
		t.Fatal("missing access accepted")
	}
	access.result.Inconclusive = true
	if err := check([]engine.Step{roles, access}); err == nil {
		t.Fatal("unknown access accepted")
	}
	access.result.Inconclusive = false
	roles.result.Complete = false
	if err := check([]engine.Step{roles, access}); err == nil {
		t.Fatal("incomplete roles accepted")
	}
}

func TestForkOwnedRuntime(t *testing.T) {
	for _, mode := range []string{"success", "recovery_requirements_unmet", "recovery_parameter_mismatch", "replacement", "foreign-reservation", "foreign-marker-install", "foreign-marker-job", "foreign-marker-operation", "access", "retire"} {
		t.Run(mode, func(t *testing.T) { runOwnedForkFixture(t, mode) })
	}
}
