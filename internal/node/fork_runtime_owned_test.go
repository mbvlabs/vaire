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

func runOwnedForkFixture(t *testing.T, mode string) {
	t.Helper()
	owner, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := createMaterialisation(owner, root); err != nil {
		t.Fatal(err)
	}
	var foreignBytes []byte
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
		if err != nil {
			t.Fatal(err)
		}
		path, err := materialisationPath(owner.Target)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		foreignBytes = raw
	}
	registry := filepath.Join(materialisationAncestorBase, "ports")
	if err := os.Mkdir(registry, 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(materialisationConfigRoot, "source")
	if err := os.Mkdir(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "postgresql.conf"), []byte("port = 5432\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: "source", Stanza: owner.Stanza, Port: 5432, Size: "small"}, {Name: "target", Stanza: "target-abcdef", Port: 5433, Size: "medium"}}
	p := dbForkParams{Source: "source", Name: "target"}
	if strings.HasPrefix(mode, "recovery_") {
		p.At = "2026-11-01T00:00:00Z"
	}
	params, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
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
	oldLocks, oldStat, oldUID, oldRead, oldLoad := nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster
	oldUpdate, oldFence, oldActivity, oldSettle := jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord
	t.Cleanup(func() {
		nodeLockers, jobNodeStat, jobEffectiveUID, jobReadSpec, jobLoadCluster = oldLocks, oldStat, oldUID, oldRead, oldLoad
		jobUpdateState, jobFenceCheck, activityRecorderFactory, settleJobRecord = oldUpdate, oldFence, oldActivity, oldSettle
	})
	nodeLockers = lockers
	jobNodeStat = func(string) (os.FileInfo, error) { return os.Stat(root) }
	jobEffectiveUID = func() int { return 0 }
	jobReadSpec = func(string) (jobSpec, error) {
		return jobSpec{Job: "db-fork", Params: params, OperationID: owner.Operation}, nil
	}
	jobLoadCluster = func(string) (config.Cluster, error) { return cfg, nil }
	var state engine.State
	jobUpdateState = func(fn func(*engine.State) error) error { return fn(&state) }
	jobFenceCheck = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	activityRecorderFactory = func() *activityRecorder { return recorderForSettlementTest(t) }
	settleJobRecord = func(string, bool) error { return nil }
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
	provisioned, verified, exposed, copyEntered := false, false, false, false
	dropCalls, forgetCalls := 0, 0
	forkDrop = func(*backup.Materialised) error {
		dropCalls++
		return errors.New("test refuses host cleanup")
	}
	forkForgetDatabase = func(string) error {
		forgetCalls++
		return errors.New("test refuses config mutation")
	}
	uid := uint32(os.Geteuid()) //nolint:gosec // test effective uid
	forkReservationLock = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		if mode == "retire" && verified && !present {
			return nil, errors.New("reservation check refused")
		}
		return backup.LockForkReservationWithHost(ctx, registry, uid, name, port, present)
	}
	forkNewMaterialised = func(root, name, source string, port int, sh engine.Shell, step string, report engine.Reporter) (*backup.Materialised, error) {
		return backup.NewForkMaterialisedWithHost(root, name, source, port, sh, step, report, backup.ForkMaterialisationHost{
			ReservationDir: registry, ConfigRoot: materialisationConfigRoot, UID: uid,
			ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
			Running: func(context.Context, string) bool { return false },
			WriteFile: func(path string, data []byte, perm os.FileMode, _, _ string) error {
				return os.WriteFile(path, data, perm)
			}, //nolint:gosec // test host paths
		})
	}
	var seenContext context.Context
	populate := func(ctx context.Context, m *backup.Materialised, typed bool) error {
		copyEntered = true
		if m.DestinationConfig == nil || !strings.Contains(m.DestinationConfig.Tuning, "max_connections = 50") {
			t.Fatal("target settings missing")
		}
		paths := m.OwnershipPaths()
		if mode == "foreign-reservation" {
			path := filepath.Join(registry, paths.Name)
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
		if mode == "replacement" {
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
			return attemptFailure(mode)
		}
		return m.GuardOwnership(ctx, backup.MaterialisationStart, func() error { return nil })
	}
	forkBaseBackup = func(m *backup.Materialised, ctx context.Context) error { return populate(ctx, m, false) }
	forkRestore = func(ctx context.Context, _ *jobEnv, _ dbForkParams, _ config.ClusterSpec, m *backup.Materialised) error {
		return populate(ctx, m, true)
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
		o, found, err := readMaterialisation(name, root)
		if err != nil || !found || !materialisationOwner(ctx, o) || materialisationOwner(context.Background(), o) {
			t.Fatalf("owner view missing: %v", err)
		}
		if guardPublicMaterialisation(cfg, name) == nil {
			t.Fatal("public apply inherited authority")
		}
		seenContext, provisioned = ctx, true
		copied := *ctx.Value(forkViewKey{}).(*forkView).capability
		forged := &forkView{capability: &copied, target: name}
		forged.active.Store(true)
		if hasForkMaterialisationView(context.WithValue(context.Background(), forkViewKey{}, forged), name) {
			t.Fatal("copied capability accepted")
		}
		if mode == "access" {
			return errors.New("access failed")
		}
		return nil
	}
	forkVerifyProvision = func(context.Context, config.Cluster, engine.Runtime, string) error {
		if !provisioned {
			t.Fatal("verified before provisioning")
		}
		if _, found, _ := readMaterialisation("target", root); !found {
			t.Fatal("marker retired before verification")
		}
		verified = true
		return nil
	}
	forkApplyPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		if !verified {
			t.Fatal("exposed before verification")
		}
		if _, found, _ := readMaterialisation("target", root); found {
			t.Fatal("exposed before retirement")
		}
		exposed = true
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
	lockedJobs.Lock()
	initialLeases := len(lockedJobs.active)
	lockedJobs.Unlock()
	_, skipped, err := RunJob(context.Background(), "db-fork", JobOptions{ID: owner.Job}, nil)
	if skipped != "" {
		t.Fatalf("job skipped: %s", skipped)
	}
	if mode == "success" {
		if err != nil || !exposed || !verified {
			t.Fatalf("success: exposed=%v verified=%v err=%v", exposed, verified, err)
		}
	} else {
		if err == nil || exposed {
			t.Fatalf("failure exposed target: %v", err)
		}
		if foreignBytes != nil {
			path, _ := materialisationPath(owner.Target)
			raw, readErr := os.ReadFile(path) //nolint:gosec // test marker
			if readErr != nil || !bytes.Equal(raw, foreignBytes) {
				t.Fatalf("foreign marker changed: %v", readErr)
			}
			if provisioned {
				t.Fatal("foreign marker reached provisioning")
			}
			if copyEntered {
				t.Fatal("foreign marker reached materialisation")
			}
		} else {
			o, found, readErr := readMaterialisation("target", root)
			if readErr != nil || !found {
				t.Fatalf("marker lost: %v", readErr)
			}
			if strings.HasPrefix(mode, "recovery_") && (o.Phase != materialisationFailed || o.Reason != mode) {
				t.Fatalf("typed marker: %+v", o)
			}
			if strings.HasPrefix(mode, "recovery_") {
				if o.ArtifactStage != materialisationRegistered || o.TemporaryPort == 0 || o.DataIdentity.Inode == 0 || o.ConfigIdentity.Inode == 0 {
					t.Fatalf("missing immutable birth evidence: %+v", o)
				}
				if _, err := os.Stat(filepath.Join(o.DataPath, "sentinel")); err != nil {
					t.Fatalf("partial target lost: %v", err)
				}
				if _, err := os.Stat(filepath.Join(registry, o.Target)); err != nil {
					t.Fatalf("reservation lost: %v", err)
				}
				if dropCalls != 0 || forgetCalls != 0 {
					t.Fatal("typed failure attempted cleanup")
				}
			}
			if mode == "access" && !provisioned {
				t.Fatal("access callback not reached")
			}
			if mode == "retire" && (!provisioned || !verified) {
				t.Fatal("retirement failure did not follow verification")
			}
			if mode == "replacement" {
				if !copyEntered {
					t.Fatal("replacement was not attempted")
				}
				if _, err := os.Stat(owner.DataPath + "-old"); err != nil {
					t.Fatalf("original birth artifact lost: %v", err)
				}
				if _, err := os.Stat(owner.DataPath); err != nil {
					t.Fatalf("foreign replacement lost: %v", err)
				}
			}
			if mode == "foreign-reservation" {
				if !copyEntered {
					t.Fatal("reservation replacement was not attempted")
				}
				if _, err := os.Stat(filepath.Join(registry, owner.Target)); err != nil {
					t.Fatalf("foreign reservation removed: %v", err)
				}
			}
		}
	}
	if seenContext != nil && hasForkMaterialisationView(seenContext, "target") {
		t.Fatal("authority leaked")
	}
	if acquired < 3 || acquired != released {
		t.Fatalf("lease release %d/%d", released, acquired)
	}
	lockedJobs.Lock()
	finalLeases := len(lockedJobs.active)
	lockedJobs.Unlock()
	if initialLeases != finalLeases {
		t.Fatal("runner retained a job lease")
	}
	rec, found := state.Job(owner.Job)
	if !found || mode == "success" && rec.State != engine.JobSucceeded || mode != "success" && rec.State != engine.JobFailed {
		t.Fatalf("terminal job state: %+v", rec)
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
