package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

type publicForkContinuation struct {
	t              *testing.T
	cfg            config.Cluster
	owner          materialisationOwnership
	root           string
	registry       string
	artifacts      *materialisationArtifacts
	state          engine.State
	started        bool
	adopted        bool
	provisioned    bool
	verified       bool
	published      bool
	guardSeen      bool
	sourceBefore   string
	startCalls     int
	adoptCalls     int
	provisionCalls int
	verifyCalls    int
	publishCalls   int
	resumeCalls    int
	engineCalls    int
}

func (f *publicForkContinuation) sourceConfig() string {
	return filepath.Join(materialisationConfigRoot, f.owner.Source, "postgresql.conf")
}

func (f *publicForkContinuation) targetConfig() string {
	return filepath.Join(materialisationConfigRoot, f.owner.Target, "postgresql.conf")
}

func (f *publicForkContinuation) dataDir() string {
	return filepath.Join(f.root, f.owner.Target)
}

// newPublicForkContinuation builds the real retained record, birth-bound
// artifacts and reservation, a fork activation fence and a terminal db-fork
// job. stage is the artifact stage; recordPhase is the materialisation phase.
func newPublicForkContinuation(t *testing.T, stage, recordPhase, fencePhase string) *publicForkContinuation {
	t.Helper()
	useTempExtensionRecords(t)
	useExtensionFileFixture(t)
	f := &publicForkContinuation{t: t}
	owner, root := materialisationFixture(t)
	f.owner, f.root = owner, root
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	f.prepareConfig()
	f.prepareArtifacts(stage, recordPhase)
	f.prepareFence(fencePhase)
	f.stub(t)
	return f
}

// prepareConfig writes the source cluster's configuration and makes both the
// steps and backup package read the same test configuration root.
func (f *publicForkContinuation) prepareConfig() {
	f.cfg = config.DefaultCluster()
	f.cfg.ClusterName, f.cfg.Domain = "prod", "example.com"
	f.cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: "10.42.0.1"}}
	f.cfg.Server.DataRoot = f.root
	f.cfg.Clusters = []config.ClusterSpec{
		{Name: "source", Node: "db1", Stanza: f.owner.Stanza, Port: 5440, Size: "small", StatementTimeout: "60s"},
		{Name: "target", Node: "db1", Stanza: "target-abcdef", Port: 5433, Size: "medium", StatementTimeout: "60s"},
	}
	f.cfg.Extensions = map[string]config.ExtensionDefinition{
		"custom": {
			Source: catalog.SourcePreinstalled, ExtName: "custom",
			CopySafety: stringPtr(catalog.CopySafetyDisabled), StandbySafe: boolPtr(true),
			CopySettings: map[string]string{"custom.worker": "off"},
		},
	}
	if err := os.Mkdir(filepath.Join(materialisationConfigRoot, f.owner.Source), 0o700); err != nil {
		f.t.Fatal(err)
	}
	sourceConf := []byte("port = 5440\n")
	if err := os.WriteFile(f.sourceConfig(), sourceConf, 0o600); err != nil {
		f.t.Fatal(err)
	}
	f.sourceBefore = string(sourceConf)
	restoreSteps, restoreBackup := steps.PGConfRootForTest(materialisationConfigRoot), backup.PGConfRootForTest(materialisationConfigRoot)
	f.t.Cleanup(func() { restoreSteps(); restoreBackup() })
}

// prepareArtifacts creates the retained materialisation record, its birth
// reservation and the artifact handles the continuation must honour.
func (f *publicForkContinuation) prepareArtifacts(stage, recordPhase string) {
	if _, err := createMaterialisation(f.owner, f.root); err != nil {
		f.t.Fatal(err)
	}
	identity := f.prepareArtifactReservation()
	artifacts := f.bindArtifactReservation(identity)
	f.writeTempRegistration(config.TemporaryPortMin)
	if recordPhase == materialisationProvisioning || recordPhase == materialisationProvisioned || stage == materialisationProvisioned {
		f.writeFinalRegistration()
	}
	f.advanceArtifactPhase(artifacts, stage, recordPhase)
	f.owner = artifacts.owner
}

// prepareArtifactReservation records and pins the temporary port reservation
// the retained materialisation was created with.
func (f *publicForkContinuation) prepareArtifactReservation() backup.ReservationIdentity {
	f.registry = filepath.Join(materialisationAncestorBase, "ports")
	if err := os.Mkdir(f.registry, 0o700); err != nil {
		f.t.Fatal(err)
	}
	reservation := []byte(fmt.Sprintf("%d\n", config.TemporaryPortMin))
	reservationPath := filepath.Join(f.registry, f.owner.Target)
	if err := os.WriteFile(reservationPath, reservation, 0o600); err != nil {
		f.t.Fatal(err)
	}
	info, err := os.Lstat(reservationPath)
	if err != nil {
		f.t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		f.t.Fatal("reservation stat unavailable")
	}
	sum := sha256.Sum256(reservation)
	return backup.ReservationIdentity{Device: st.Dev, Inode: st.Ino, UID: st.Uid, Digest: hex.EncodeToString(sum[:])}
}

// bindArtifactReservation opens the birth-bound artifact handles and binds the
// reservation whose identity the continuation must retain.
func (f *publicForkContinuation) bindArtifactReservation(identity backup.ReservationIdentity) *materialisationArtifacts {
	artifacts, err := openMaterialisationArtifacts(f.owner.Target, f.root, materialisationRecordUID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.artifacts = artifacts
	f.t.Cleanup(func() { artifacts.close() })
	if err := artifacts.bindMaterialisationReservation(config.TemporaryPortMin, func(_ materialisationOwnership, _ int, finish func() error) error {
		return finish()
	}, identity); err != nil {
		f.t.Fatal(err)
	}
	if err := artifacts.bindCreated(false); err != nil {
		f.t.Fatal(err)
	}
	if err := artifacts.bindCreated(true); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(materialisationConfigRoot, f.owner.Target, "conf.d"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return artifacts
}

// advanceArtifactPhase moves the retained record to the stage and phase the
// fixture models, in the same order the real continuation persists them.
func (f *publicForkContinuation) advanceArtifactPhase(artifacts *materialisationArtifacts, stage, recordPhase string) {
	if recordPhase == materialisationProvisioning || recordPhase == materialisationProvisioned || stage == materialisationProvisioned {
		next := artifacts.owner
		next.Phase = materialisationCopying
		if err := artifacts.persistMaterialisation(next); err != nil {
			f.t.Fatal(err)
		}
		next = artifacts.owner
		next.Phase = materialisationProvisioning
		if err := artifacts.persistMaterialisation(next); err != nil {
			f.t.Fatal(err)
		}
	}
	if recordPhase == materialisationCopying {
		next := artifacts.owner
		next.Phase = materialisationCopying
		if err := artifacts.persistMaterialisation(next); err != nil {
			f.t.Fatal(err)
		}
	}
	if stage == materialisationProvisioned {
		if err := artifacts.markMaterialisationProvisioned(func() error { return nil }); err != nil {
			f.t.Fatal(err)
		}
	}
}

// prepareFence records the fork activation fence and the terminal db-fork job
// the public apply must resume from exactly that durable checkpoint.
func (f *publicForkContinuation) prepareFence(fencePhase string) {
	oldLock, oldDir := forkReservationLock, forkContinuationReservationDir
	forkReservationLock = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		return backup.LockForkReservationWithHost(ctx, f.registry, uint32(os.Geteuid()), name, port, present)
	}
	forkContinuationReservationDir = func() string { return f.registry }
	f.t.Cleanup(func() { forkReservationLock, forkContinuationReservationDir = oldLock, oldDir })

	spec, _ := f.cfg.ClusterByName(f.owner.Target)
	registration, port := fmt.Sprintf("%s:%d:", f.owner.Target, config.TemporaryPortMin), config.TemporaryPortMin
	requirements := []catalog.Extension{{
		Name: "custom", ExtName: "custom", Source: catalog.SourcePreinstalled,
		CopySafety: catalog.CopySafetyDisabled, StandbySafe: true,
		CopySettings: map[string]string{"custom.worker": "off"},
	}}
	_, err := writeActivationFence(f.cfg, activationFenceIdentity{
		Database: f.owner.Target, Kind: "fork", OperationID: f.owner.Job, Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: f.owner.Stanza, NewStanza: spec.Stanza,
		Registration: registration, Port: port,
		DataDir: f.dataDir(), ConfDir: steps.PGConfDir(f.owner.Target),
		Requirements: requirements, Renames: map[string]string{f.owner.Source: f.owner.Target},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if fencePhase != fencePhasePrepared {
		if err := backup.UpdateActivationFence(f.owner.Target, f.owner.Job, func(cur *backup.ActivationFence) error {
			cur.Phase = fencePhase
			if fencePhase == fencePhaseAdopted || fencePhase == fencePhaseRouting {
				cur.Registration = activationRegistration(spec)
				cur.Port = spec.Port
				cur.DataDir = f.cfg.Server.DataDirectory(f.owner.Target)
				cur.ConfDir = steps.PGConfDir(f.owner.Target)
			}
			return nil
		}); err != nil {
			f.t.Fatal(err)
		}
	}
	if fencePhase == fencePhaseAdopted || fencePhase == fencePhaseRouting {
		f.writeFinalRegistration()
	}
	f.state = engine.State{Jobs: []engine.JobRecord{{
		ID: f.owner.Job, Job: "db-fork", DB: f.owner.Target, State: engine.JobFailed,
	}}}
	if fencePhase == fencePhaseRouting {
		// The routing checkpoint means adoption, provisioning and the first
		// start finished and the temporary reservation is gone.
		if err := os.Remove(filepath.Join(f.registry, f.owner.Target)); err != nil && !errors.Is(err, os.ErrNotExist) {
			f.t.Fatal(err)
		}
		f.started, f.provisioned = true, true
	}
}

func (f *publicForkContinuation) writeTempRegistration(port int) {
	raw := fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", f.dataDir(), f.owner.Target, port)
	if err := os.WriteFile(f.targetConfig(), []byte(raw), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *publicForkContinuation) writeFinalRegistration() {
	f.writeRegistration(5433, f.owner.Target)
}

func (f *publicForkContinuation) writeRegistration(port int, name string) {
	raw := fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", f.dataDir(), name, port)
	if err := os.WriteFile(f.targetConfig(), []byte(raw), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *publicForkContinuation) stub(t *testing.T) {
	t.Helper()
	oldState, oldReap := forkAbsenceState, forkContinuationReapJob
	oldAdopt, oldHost := forkContinuationAdopt, forkContinuationResumeHost
	oldStart, oldProvision, oldVerify := forkStartCluster, forkProvisionOwned, forkVerifyProvision
	oldPrepare, oldSQL, oldCheck, oldPrune := prepareCopyExtensions, reconcileCopySQL, checkCopyExtensions, pruneCopyGuard
	oldPublish, oldRoutes, oldResume := publishRouting, poolerRuntimeRoutes, poolerResumeDatabase
	oldEngine := applyEngine
	oldRunning := clusterRunning
	oldFollowReg, oldFollowData := followConfRegistered, followRegisteredData
	oldLoad, oldReload := loadGuardCluster, applyReloadCluster
	oldFollower := followerState
	oldAcquire, oldAcquireCluster, oldRunLog := serveAcquire, serveAcquireCluster, openServeRunLog
	t.Cleanup(func() {
		forkAbsenceState, forkContinuationReapJob = oldState, oldReap
		forkContinuationAdopt, forkContinuationResumeHost = oldAdopt, oldHost
		forkStartCluster, forkProvisionOwned, forkVerifyProvision = oldStart, oldProvision, oldVerify
		prepareCopyExtensions, reconcileCopySQL, checkCopyExtensions, pruneCopyGuard = oldPrepare, oldSQL, oldCheck, oldPrune
		publishRouting, poolerRuntimeRoutes, poolerResumeDatabase = oldPublish, oldRoutes, oldResume
		applyEngine = oldEngine
		clusterRunning = oldRunning
		followConfRegistered, followRegisteredData = oldFollowReg, oldFollowData
		loadGuardCluster, applyReloadCluster = oldLoad, oldReload
		followerState = oldFollower
		serveAcquire, serveAcquireCluster, openServeRunLog = oldAcquire, oldAcquireCluster, oldRunLog
	})
	forkAbsenceState = func() (engine.State, error) { return f.state, nil }
	forkContinuationReapJob = func(context.Context, string) error { return errors.New("test reaper unavailable") }
	forkContinuationAdopt = f.adoptHook()
	forkStartCluster = f.startHook()
	forkProvisionOwned = f.provisionHook()
	forkVerifyProvision = f.verifyHook()
	prepareCopyExtensions = func(context.Context, *extensionCopyPlan, engine.Runtime, engine.Reporter) error { return nil }
	reconcileCopySQL = func(context.Context, config.Cluster, config.ClusterSpec, sqlRunner, string) ([]string, error) {
		return nil, nil
	}
	checkCopyExtensions = func(context.Context, config.Cluster, config.ClusterSpec, *backup.CopyPlan, sqlRunner, string) ([]output.Warning, error) {
		return nil, nil
	}
	pruneCopyGuard = func(context.Context, config.Cluster, config.ClusterSpec, string, *backup.CopyPlan, bool, string, func() error) ([]output.Warning, error) {
		return nil, nil
	}
	publishRouting = f.publishHook()
	poolerResumeDatabase = func(context.Context, string) { f.resumeCalls++ }
	applyEngine = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		f.engineCalls++
		return engine.ApplyData{}, nil, nil
	}
	poolerRuntimeRoutes = f.routesHook()
	clusterRunning = func(config.ClusterSpec) bool { return false }
	followConfRegistered = func(name string) bool {
		_, err := os.Stat(filepath.Join(materialisationConfigRoot, name, "postgresql.conf"))
		return err == nil
	}
	followRegisteredData = func(name string) (string, error) {
		return f.cfg.Server.DataDirectory(name), nil
	}
	loadGuardCluster = func() (config.Cluster, error) { return f.cfg.Clone(), nil }
	applyReloadCluster = func() (config.Cluster, error) { return f.cfg.Clone(), nil }
	followerState = func() state.Store { return ownedForkConfigStore{cfg: &f.cfg} }
	serveAcquire = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	serveAcquireCluster = func(context.Context, string, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		return func() {}, nil
	}
	openServeRunLog = func(method, id string) (*engine.RunLog, error) {
		return engine.OpenRunLogFile(filepath.Join(t.TempDir(), method+"-"+id+".log"))
	}
}

func (f *publicForkContinuation) adoptHook() func(context.Context, config.Cluster, *Env, materialisationOwnership, config.ClusterSpec, config.ClusterSpec, backup.FollowerResumeGuard) error {
	return func(ctx context.Context, cfg config.Cluster, env *Env, o materialisationOwnership, src, spec config.ClusterSpec, guard backup.FollowerResumeGuard) error {
		f.adoptCalls++
		m := backup.ResumeMaterialisedWithHost(cfg.Server.DataRoot, o.Target, o.Source, src.Port,
			runtimeFor(env, false).Shell, "db-fork", env.Report, materialisationConfigRoot, followerResumeHostForTest{f})
		_, err := m.AdoptResumedFollower(ctx, spec.Port, guard)
		if err == nil {
			f.adopted = true
		}
		return err
	}
}

func (f *publicForkContinuation) startHook() func(context.Context, config.Cluster, engine.Runtime, config.ClusterSpec, engine.Reporter) error {
	return func(context.Context, config.Cluster, engine.Runtime, config.ClusterSpec, engine.Reporter) error {
		f.startCalls++
		guard, present, err := backup.ReadCopyGuard(f.dataDir())
		if err != nil {
			return err
		}
		if present && guard.Settings["custom.worker"] == "off" {
			f.guardSeen = true
		}
		if !present {
			return errors.New("retry start ran before the complete guard was installed")
		}
		f.started = true
		return nil
	}
}

func (f *publicForkContinuation) provisionHook() func(context.Context, config.Cluster, engine.Runtime, string, string, engine.Reporter) error {
	return func(ctx context.Context, cfg config.Cluster, _ engine.Runtime, name, _ string, _ engine.Reporter) error {
		scope := forkContinuationScopeFrom(ctx)
		if name != f.owner.Target || scope == nil || !scope.hasOwner || !materialisationContinuationOwner(ctx, scope.owner) {
			return errors.New("owned provisioning lost its exact continuation scope")
		}
		if hasForkMaterialisationView(ctx, name) {
			return errors.New("public continuation minted a job authority")
		}
		f.provisionCalls++
		f.provisioned = true
		return nil
	}
}

func (f *publicForkContinuation) verifyHook() func(context.Context, config.Cluster, engine.Runtime, string) error {
	return func(context.Context, config.Cluster, engine.Runtime, string) error {
		f.verifyCalls++
		if !f.provisioned || !f.started {
			return errors.New("provisioning was verified before it ran")
		}
		if _, found, _ := readMaterialisation(f.owner.Target, f.root); !found {
			return errors.New("marker retired before provisioning verification")
		}
		f.verified = true
		return nil
	}
}

func (f *publicForkContinuation) publishHook() func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
	return func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		f.publishCalls++
		if _, found, _ := readMaterialisation(f.owner.Target, f.root); found {
			return errors.New("routes published before marker retirement")
		}
		f.published = true
		return nil
	}
}

func (f *publicForkContinuation) routesHook() func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
	return func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		if !f.published {
			return map[string]steps.PoolerRoute{}, nil
		}
		return map[string]steps.PoolerRoute{
			f.owner.Target: {Name: f.owner.Target, Host: steps.PGSocketDir, Port: 5433, Database: f.owner.Target},
		}, nil
	}
}

type followerResumeHostForTest struct{ f *publicForkContinuation }

func (h followerResumeHostForTest) RegistrationDirectory(name string) string {
	return filepath.Join(materialisationConfigRoot, name)
}
func (h followerResumeHostForTest) ReservationStore() (string, uint32, error) {
	return h.f.registry, uint32(os.Geteuid()), nil
}
func (h followerResumeHostForTest) Stop(context.Context) error { return nil }
func (h followerResumeHostForTest) AdoptRegistration(final int) error {
	raw := fmt.Sprintf("data_directory = '%s'\ncluster_name = '18/%s'\nport = %d\n", h.f.dataDir(), h.f.owner.Target, final)
	return os.WriteFile(h.f.targetConfig(), []byte(raw), 0o600)
}

func (f *publicForkContinuation) apply() (string, int) {
	return f.applyParams(fmt.Sprintf(`{"db":%q}`, f.owner.Target))
}

func (f *publicForkContinuation) applyParams(params string) (string, int) {
	withServeActivity(f.t, &fakeActivityStore{})
	req := matchingBuildRequest("apply")
	req.Params = json.RawMessage(params)
	return serveRequest(f.t, req)
}

func (f *publicForkContinuation) assertRetired() {
	f.t.Helper()
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || found {
		f.t.Fatalf("materialisation record remains: found=%v err=%v", found, err)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		f.t.Fatal(err)
	}
	if _, ok := recs.ActivationFenceFor(f.owner.Target); ok {
		f.t.Fatal("activation fence remains")
	}
	if _, err := os.Stat(filepath.Join(f.registry, f.owner.Target)); !errors.Is(err, os.ErrNotExist) {
		f.t.Fatalf("temporary reservation remains: %v", err)
	}
	if got, err := os.ReadFile(f.sourceConfig()); err != nil || string(got) != f.sourceBefore {
		f.t.Fatalf("source configuration changed: %q %v", got, err)
	}
}

func TestPublicApplyContinuesAdoptedFork(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationRegistered, materialisationCopying, fencePhaseAdopting)
	if !forkContinuationCandidate(f.cfg, f.owner.Target) {
		recs, _ := backup.LoadRecords()
		t.Fatalf("not a candidate: owner=%+v fence=%+v", f.owner, recs.Activation[f.owner.Target])
	}
	out, code := f.apply()
	if code != 0 {
		t.Fatalf("public apply = %d\n%s", code, out)
	}
	if !f.adopted || !f.started || !f.provisioned || !f.verified || !f.published || !f.guardSeen {
		t.Fatalf("continuation effects: adopted=%v started=%v provisioned=%v verified=%v published=%v guardSeen=%v",
			f.adopted, f.started, f.provisioned, f.verified, f.published, f.guardSeen)
	}
	f.assertRetired()
}

func TestPublicApplyFinishesAdoptingProvisioningGap(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationRegistered, materialisationProvisioning, fencePhaseAdopting)
	// The adoption completed and removed its reservation, but the durable
	// record still says provisioning and the fence still says adopting.
	if err := os.Remove(filepath.Join(f.registry, f.owner.Target)); err != nil {
		t.Fatal(err)
	}
	f.published = false
	out, code := f.apply()
	if code != 0 {
		t.Fatalf("public apply = %d\n%s", code, out)
	}
	if f.adoptCalls != 0 {
		t.Fatal("an already-adopted checkpoint replayed adoption")
	}
	if !f.started || !f.provisioned || !f.verified || !f.published {
		t.Fatalf("adopting gap did not settle: started=%v provisioned=%v verified=%v published=%v",
			f.started, f.provisioned, f.verified, f.published)
	}
	f.assertRetired()
}

func TestPublicApplyResumesAdoptedProvisioning(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationRegistered, materialisationProvisioning, fencePhaseAdopted)
	if err := os.Remove(filepath.Join(f.registry, f.owner.Target)); err != nil {
		t.Fatal(err)
	}
	out, code := f.apply()
	if code != 0 {
		t.Fatalf("public apply = %d\n%s", code, out)
	}
	if f.adoptCalls != 0 {
		t.Fatal("an adopted checkpoint replayed adoption")
	}
	if !f.provisioned || !f.started || !f.verified || !f.published {
		t.Fatalf("adopted continuation did not settle: provisioned=%v started=%v verified=%v published=%v",
			f.provisioned, f.started, f.verified, f.published)
	}
	f.assertRetired()
}

func TestPublicApplyRoutingOnlyRetiresProvisionedMarker(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationProvisioned, materialisationProvisioning, fencePhaseRouting)
	out, code := f.apply()
	if code != 0 {
		t.Fatalf("public apply = %d\n%s", code, out)
	}
	if f.adoptCalls != 0 || f.startCalls != 0 || f.provisionCalls != 0 {
		t.Fatalf("routing retry replayed effects: adopt=%d start=%d provision=%d", f.adoptCalls, f.startCalls, f.provisionCalls)
	}
	if f.verifyCalls != 1 || !f.published {
		t.Fatalf("routing retry verify=%d published=%v", f.verifyCalls, f.published)
	}
	f.assertRetired()
}

func TestPublicApplyRoutingRetryAfterUnlinkSyncFailure(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationProvisioned, materialisationProvisioning, fencePhaseRouting)
	oldSync := materialisationSyncStateDir
	fail := true
	materialisationSyncStateDir = func() error {
		if fail {
			return errors.New("injected state directory sync failure")
		}
		return oldSync()
	}
	t.Cleanup(func() { materialisationSyncStateDir = oldSync })
	out, code := f.apply()
	if code == 0 {
		t.Fatalf("sync failure was reported as success:\n%s", out)
	}
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || found {
		t.Fatalf("marker state after sync failure: found=%v err=%v", found, err)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if fence, ok := recs.ActivationFenceFor(f.owner.Target); !ok || fence.Phase != fencePhaseRouting {
		t.Fatalf("sync failure did not retain the routing fence: %+v", fence)
	}
	fail = false
	out, code = f.apply()
	if code != 0 {
		t.Fatalf("routing retry = %d\n%s", code, out)
	}
	f.assertRetired()
}

func TestPublicApplyRoutingPublicationFailureRetries(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationProvisioned, materialisationProvisioning, fencePhaseRouting)
	fail := true
	oldPublish := publishRouting
	publishRouting = func(ctx context.Context, cfg config.Cluster, rt engine.Runtime, report engine.Reporter) error {
		if fail {
			fail = false
			return errors.New("injected publication failure")
		}
		return oldPublish(ctx, cfg, rt, report)
	}
	out, code := f.apply()
	if code != 0 || !strings.Contains(out, "activation_routing_incomplete") {
		t.Fatalf("publication failure = %d\n%s", code, out)
	}
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	if fence, ok := recs.ActivationFenceFor(f.owner.Target); !ok || fence.Phase != fencePhaseRouting {
		t.Fatalf("publication failure did not retain routing: %+v", fence)
	}
	out, code = f.apply()
	if code != 0 || !f.published {
		t.Fatalf("publication retry = %d published=%v\n%s", code, f.published, out)
	}
	f.assertRetired()
}

// assertNoContinuationEffects proves a refusal reached no continuation stage.
func (f *publicForkContinuation) assertNoContinuationEffects() {
	f.t.Helper()
	if f.adoptCalls != 0 || f.startCalls != 0 || f.provisionCalls != 0 || f.verifyCalls != 0 || f.publishCalls != 0 || f.engineCalls != 0 {
		f.t.Fatalf("refusal had effects: %+v", f)
	}
	if f.published || f.resumeCalls != 0 {
		f.t.Fatalf("refusal published or resumed: %+v", f)
	}
}

// assertRefusedBeforeEffects proves the durable record, fence and guard were
// left untouched by a refused continuation.
func (f *publicForkContinuation) assertRefusedBeforeEffects(fencePhase string) {
	f.t.Helper()
	f.assertNoContinuationEffects()
	recs, err := backup.LoadRecords()
	if err != nil {
		f.t.Fatal(err)
	}
	if len(recs.ExtensionRequirements) != 0 {
		f.t.Fatalf("refusal wrote extension history: %+v", recs.ExtensionRequirements)
	}
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || !found {
		f.t.Fatalf("refusal lost the marker: found=%v err=%v", found, err)
	}
	if fence, ok := recs.ActivationFenceFor(f.owner.Target); !ok || fence.Phase != fencePhase {
		f.t.Fatalf("refusal changed the fence: %+v", fence)
	}
	if _, present, err := backup.ReadCopyGuard(f.dataDir()); err != nil || present {
		f.t.Fatalf("refusal installed a guard: present=%v err=%v", present, err)
	}
}

func (f *publicForkContinuation) assertNoUnprovenEffects() {
	f.t.Helper()
	if f.adoptCalls != 0 || f.startCalls != 0 || f.provisionCalls != 0 || f.publishCalls != 0 {
		f.t.Fatalf("refusal had effects: %+v", f)
	}
}

func (f *publicForkContinuation) updateFence(change func(*backup.ActivationFence)) {
	f.t.Helper()
	if err := backup.UpdateActivationFence(f.owner.Target, f.owner.Job, func(cur *backup.ActivationFence) error {
		change(cur)
		return nil
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *publicForkContinuation) corruptJob() {
	f.owner.Job = "other-job"
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptInstance() {
	f.owner.Instance = "other-instance"
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptOperation() {
	f.updateFence(func(cur *backup.ActivationFence) { cur.OperationID = "other-operation" })
}

func (f *publicForkContinuation) corruptStanza() {
	f.owner.Stanza = "other-stanza"
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptPort() {
	f.owner.IntendedPort = 5999
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptDataPath() {
	f.owner.DataPath = filepath.Join(f.root, "elsewhere")
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptReservation() {
	f.owner.ReservationIdentity.Inode++
	f.rewriteRecord()
}

func (f *publicForkContinuation) corruptPreparedFence() {
	f.updateFence(func(cur *backup.ActivationFence) { cur.Phase = fencePhasePrepared })
}

func (f *publicForkContinuation) corruptRegistration() {
	f.updateFence(func(cur *backup.ActivationFence) { cur.Registration = "other:5480:" })
}

func (f *publicForkContinuation) replaceDataDir() {
	if err := os.Rename(f.dataDir(), f.dataDir()+"-old"); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Mkdir(f.dataDir(), 0o700); err != nil {
		f.t.Fatal(err)
	}
}

func (f *publicForkContinuation) makeAncestryUnsafe() {
	if err := os.Chmod(materialisationConfigRoot, 0o777); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = os.Chmod(materialisationConfigRoot, 0o700) })
}

func (f *publicForkContinuation) duplicateJobHistory() {
	f.state.Jobs = append(f.state.Jobs, f.state.Jobs[0])
}

func (f *publicForkContinuation) activateOriginalJob() {
	f.state.Jobs[0].State = engine.JobRunning
}

func TestPublicApplyRefusesWrongActualRegistrationBeforeEffects(t *testing.T) {
	cases := []struct {
		name   string
		stage  string
		fence  string
		mutate func(*publicForkContinuation)
	}{
		{"adopted-wrong-port", materialisationRegistered, fencePhaseAdopted, func(f *publicForkContinuation) { f.writeRegistration(5490, f.owner.Target) }},
		{"adopted-wrong-cluster-name", materialisationRegistered, fencePhaseAdopted, func(f *publicForkContinuation) { f.writeRegistration(5433, "other") }},
		{"routing-wrong-port", materialisationProvisioned, fencePhaseRouting, func(f *publicForkContinuation) { f.writeRegistration(5490, f.owner.Target) }},
		{"routing-wrong-cluster-name", materialisationProvisioned, fencePhaseRouting, func(f *publicForkContinuation) { f.writeRegistration(5433, "other") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicForkContinuation(t, tc.stage, materialisationProvisioning, tc.fence)
			if tc.fence == fencePhaseAdopted {
				if err := os.Remove(filepath.Join(f.registry, f.owner.Target)); err != nil {
					t.Fatal(err)
				}
			}
			tc.mutate(f)
			out, code := f.apply()
			if code == 0 || strings.Contains(out, `"ok":true`) {
				t.Fatalf("wrong actual registration accepted: %d\n%s", code, out)
			}
			f.assertRefusedBeforeEffects(tc.fence)
		})
	}
}

func TestForkRoutingDurableAbsenceBoundaryBeforeEngine(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationProvisioned, materialisationProvisioning, fencePhaseRouting)
	oldSync := materialisationSyncStateDir
	syncAllowed := false
	materialisationSyncStateDir = func() error {
		if !syncAllowed {
			return errors.New("injected boundary sync failure")
		}
		return oldSync()
	}
	t.Cleanup(func() { materialisationSyncStateDir = oldSync })
	// The scoped retry unlinks the marker but fails its durable parent sync.
	if out, code := f.apply(); code == 0 {
		t.Fatalf("scoped sync failure = %d\n%s", code, out)
	}
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || found {
		t.Fatalf("scoped sync failure marker state: found=%v err=%v", found, err)
	}
	fenceStillRouting := func() bool {
		recs, err := backup.LoadRecords()
		if err != nil {
			t.Fatal(err)
		}
		fence, ok := recs.ActivationFenceFor(f.owner.Target)
		return ok && fence.Phase == fencePhaseRouting
	}
	if !fenceStillRouting() {
		t.Fatal("scoped sync failure cleared the routing fence")
	}
	// A full apply and a step-selected apply must refuse before the engine's
	// shared render/publication/resume while the boundary sync still fails.
	for _, params := range []string{
		`{"skip_doctor":true}`,
		`{"db":"target","steps":["postgres-config-v1@target"],"skip_doctor":true}`,
	} {
		out, code := f.applyParams(params)
		if code == 0 || strings.Contains(out, `"ok":true`) {
			t.Fatalf("apply %s ran with a non-durable marker absence: %d\n%s", params, code, out)
		}
		if f.engineCalls != 0 || f.published || f.resumeCalls != 0 || !fenceStillRouting() {
			t.Fatalf("apply %s touched routing: engine=%d published=%v resume=%d", params, f.engineCalls, f.published, f.resumeCalls)
		}
	}
	// The normal engine publishes shared routes; model that once the boundary
	// proof is allowed, then the strict finalizer verifies, resumes and clears.
	applyEngine = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		f.engineCalls++
		f.published = true
		return engine.ApplyData{}, nil, nil
	}
	syncAllowed = true
	out, code := f.applyParams(`{"db":"target","steps":["postgres-config-v1@target"],"skip_doctor":true}`)
	if code != 0 {
		t.Fatalf("allowed step-selected retry = %d\n%s", code, out)
	}
	if f.engineCalls != 1 || !f.published || f.resumeCalls != 1 || fenceStillRouting() {
		t.Fatalf("allowed retry did not finalize: engine=%d published=%v resume=%d", f.engineCalls, f.published, f.resumeCalls)
	}
	f.assertRetired()
}

func TestPublicApplyRefusesUnprovenForkContinuations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*publicForkContinuation)
	}{
		{"wrong-job", (*publicForkContinuation).corruptJob},
		{"wrong-instance", (*publicForkContinuation).corruptInstance},
		{"wrong-fence-operation", (*publicForkContinuation).corruptOperation},
		{"wrong-stanza", (*publicForkContinuation).corruptStanza},
		{"wrong-port", (*publicForkContinuation).corruptPort},
		{"wrong-data-path", (*publicForkContinuation).corruptDataPath},
		{"bad-reservation", (*publicForkContinuation).corruptReservation},
		{"prepared-fence", (*publicForkContinuation).corruptPreparedFence},
		{"wrong-registration", (*publicForkContinuation).corruptRegistration},
		{"replaced-inode", (*publicForkContinuation).replaceDataDir},
		{"unsafe-ancestry", (*publicForkContinuation).makeAncestryUnsafe},
		{"ambiguous-history", (*publicForkContinuation).duplicateJobHistory},
		{"active-original-job", (*publicForkContinuation).activateOriginalJob},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newPublicForkContinuation(t, materialisationRegistered, materialisationCopying, fencePhaseAdopting)
			tc.mutate(f)
			out, code := f.apply()
			if code == 0 || strings.Contains(out, `"ok":true`) {
				t.Fatalf("unproven continuation accepted: %d\n%s", code, out)
			}
			f.assertNoUnprovenEffects()
		})
	}
}

func TestCopyPlanFromFenceUsesCompleteDurableUnion(t *testing.T) {
	useTempExtensionRecords(t)
	useExtensionFileFixture(t)
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "target", Stanza: "target-abcdef", Port: 5433, Size: "small", StatementTimeout: "60s", Extensions: []string{"current"}}}
	cfg.Extensions = map[string]config.ExtensionDefinition{
		"current":  {Source: catalog.SourcePreinstalled, ExtName: "current_ext", CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(true)},
		"history":  {Source: catalog.SourcePreinstalled, ExtName: "history_ext", CopySafety: stringPtr(catalog.CopySafetyDisabled), StandbySafe: boolPtr(true), CopySettings: map[string]string{"history.worker": "off"}},
		"retained": {Source: catalog.SourcePreinstalled, ExtName: "retained_ext", CopySafety: stringPtr(catalog.CopySafetySafe), StandbySafe: boolPtr(false)},
	}
	history := catalog.Extension{Name: "history", ExtName: "history_ext", Source: catalog.SourcePreinstalled, CopySafety: catalog.CopySafetyDisabled, StandbySafe: true, CopySettings: map[string]string{"history.worker": "off"}}
	retained := catalog.Extension{Name: "retained", ExtName: "retained_ext", Source: catalog.SourcePreinstalled, CopySafety: catalog.CopySafetySafe, StandbySafe: false}
	if err := backup.RecordExtensionRequirements("target-abcdef", "target", []catalog.Extension{retained}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	spec, _ := cfg.ClusterByName("target")
	f := backup.ActivationFence{
		Database: "target", Kind: "fork", Phase: fencePhaseAdopted,
		TargetStanza: spec.Stanza, OldStanza: "source-abc", NewStanza: spec.Stanza,
		Requirements: []catalog.Extension{history},
	}
	plan, err := copyPlanFromFence(cfg, spec, f)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, ext := range plan.Plan.Requirements {
		names[ext.Name] = true
	}
	for _, name := range []string{"current", "history", "retained"} {
		if !names[name] {
			t.Fatalf("the plan lost %s from the durable/current union: %+v", name, plan.Plan.Requirements)
		}
	}
	if plan.Plan.Guard.Settings["history.worker"] != "off" {
		t.Fatalf("mandatory copied settings were not enforced: %+v", plan.Plan.Guard)
	}
	if plan.Plan.Cron || plan.Plan.ValidateGuard == nil || len(plan.Merged) != len(plan.Plan.Requirements) {
		t.Fatalf("incomplete derived plan: %+v", plan.Plan)
	}
}

func TestCopyPlanFromFenceRefusesUnknownAndConflictingHistory(t *testing.T) {
	useTempExtensionRecords(t)
	useExtensionFileFixture(t)
	base := config.DefaultCluster()
	base.Clusters = []config.ClusterSpec{{Name: "target", Stanza: "target-abcdef", Port: 5433, Size: "small", StatementTimeout: "60s", Extensions: []string{"sqlonly"}}}
	base.Extensions = map[string]config.ExtensionDefinition{
		"sqlonly": {Source: catalog.SourcePreinstalled, CopySafety: stringPtr(catalog.CopySafetyUnknown), StandbySafe: boolPtr(true)},
	}
	spec, _ := base.ClusterByName("target")
	f := backup.ActivationFence{Database: "target", Kind: "fork", Phase: fencePhaseAdopted, TargetStanza: spec.Stanza, OldStanza: "source-abc", NewStanza: spec.Stanza}
	if _, err := copyPlanFromFence(base, spec, f); err == nil {
		t.Fatal("a current SQL-only descriptor without copy safety was accepted")
	}
	// A retained conflicting provider of the same extension is refused, not
	// silently deduplicated by name.
	conflict := base
	conflict.Clusters[0].Extensions = nil
	conflict.Extensions = map[string]config.ExtensionDefinition{}
	retained := catalog.Extension{Name: "history", ExtName: "history_ext", Source: catalog.SourceAPT, Package: "one", CopySafety: catalog.CopySafetySafe, StandbySafe: true}
	if err := backup.RecordExtensionRequirements("target-abcdef", "target", []catalog.Extension{retained}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	other := catalog.Extension{Name: "history", ExtName: "history_ext", Source: catalog.SourceAPT, Package: "two", CopySafety: catalog.CopySafetySafe, StandbySafe: true}
	f.Requirements = []catalog.Extension{other}
	if _, err := copyPlanFromFence(conflict, spec, f); err == nil {
		t.Fatal("conflicting historical providers were not refused")
	}
}

func TestForkContinuationLeaseRejectsCopiesAndRevocation(t *testing.T) {
	env := &Env{}
	l := &materialisationContinuationLease{env: env, target: "target", global: engine.LockExclusive, cluster: engine.LockExclusive, dispatcher: true}
	l.self = l
	l.active.Store(true)
	if !l.valid() {
		t.Fatal("a fresh dispatcher lease is invalid")
	}
	copied := &materialisationContinuationLease{self: l.self, env: l.env, target: l.target, global: l.global, cluster: l.cluster, dispatcher: l.dispatcher}
	copied.active.Store(true)
	if copied.valid() {
		t.Fatal("a copied lease was accepted")
	}
	l.revoke()
	if l.valid() || materialisationContinuationLeaseFrom(withMaterialisationContinuationLease(context.Background(), l)) != nil {
		t.Fatal("a revoked lease was accepted")
	}
}

func TestPublicApplyVerifyFailureRetainsRoutingMarker(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationRegistered, materialisationProvisioning, fencePhaseRouting)
	oldVerify := forkVerifyProvision
	fail := true
	forkVerifyProvision = func(ctx context.Context, cfg config.Cluster, rt engine.Runtime, target string) error {
		if fail {
			fail = false
			return errors.New("injected provisioning verification failure")
		}
		return oldVerify(ctx, cfg, rt, target)
	}
	out, code := f.apply()
	if code == 0 || !strings.Contains(out, "injected provisioning verification failure") {
		t.Fatalf("verification failure = %d\n%s", code, out)
	}
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || !found {
		t.Fatalf("verification failure retired the marker: found=%v err=%v", found, err)
	}
	if f.published {
		t.Fatal("verification failure published routes")
	}
	out, code = f.apply()
	if code != 0 || !f.verified || !f.published {
		t.Fatalf("verification retry = %d verified=%v published=%v\n%s", code, f.verified, f.published, out)
	}
	f.assertRetired()
}

func TestPublicApplyUnlinkFailureRetainsRoutingMarker(t *testing.T) {
	f := newPublicForkContinuation(t, materialisationProvisioned, materialisationProvisioning, fencePhaseRouting)
	oldRemove := materialisationRemoveRecord
	fail := true
	materialisationRemoveRecord = func(path string) error {
		if fail {
			fail = false
			return errors.New("injected unlink failure")
		}
		return oldRemove(path)
	}
	t.Cleanup(func() { materialisationRemoveRecord = oldRemove })
	out, code := f.apply()
	if code == 0 || !strings.Contains(out, "injected unlink failure") {
		t.Fatalf("unlink failure = %d\n%s", code, out)
	}
	if _, found, err := readMaterialisation(f.owner.Target, f.root); err != nil || !found {
		t.Fatalf("unlink failure lost the marker: found=%v err=%v", found, err)
	}
	if f.published {
		t.Fatal("unlink failure published routes")
	}
	out, code = f.apply()
	if code != 0 || !f.published {
		t.Fatalf("unlink retry = %d published=%v\n%s", code, f.published, out)
	}
	f.assertRetired()
}

func (f *publicForkContinuation) rewriteRecord() {
	raw, err := json.Marshal(f.owner)
	if err != nil {
		f.t.Fatal(err)
	}
	p, err := materialisationPath(f.owner.Target)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		f.t.Fatal(err)
	}
}
