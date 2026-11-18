package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

type recoveryPoolerFixture struct {
	cfg     config.Cluster
	env     *jobEnv
	calls   *[]string
	attempt backup.MaterialisationAttempt
}

func newRecoveryPoolerFixture(t *testing.T, liveOther bool) *recoveryPoolerFixture {
	t.Helper()
	useTestInstance(t)

	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldRecords })

	cfg := config.Cluster{Server: config.Server{DataRoot: t.TempDir()}, Clusters: []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-abcdef", Size: "small"},
		{Name: "other", Port: 5434, Stanza: "other-abcdef", Size: "small"},
	}}
	cfg.Backup.Provider = "r2"

	noExtensions := []string{}
	for i := range cfg.Clusters {
		cfg.Clusters[i].RecoveryExtensions = &noExtensions
	}

	f := &recoveryPoolerFixture{cfg: cfg, calls: new([]string)}
	f.env = &jobEnv{ID: "job-1", OperationID: "operation-1", installID: "test-instance", Job: "recover", DB: "app", Cluster: cfg,
		Params: json.RawMessage(`{"database":"app"}`)}
	f.env.lease = newLockedJobLease(f.env, []jobLockRequest{{name: "app", mode: engine.LockShared}, {name: "app__restoring", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(f.env.lease.finish)

	awaiting := map[string]backup.Awaiting{"app": {Stanza: "app-abcdef"}, "other": {Stanza: "other-abcdef"}}
	if liveOther {
		delete(awaiting, "other")
	}

	f.attempt = backup.MaterialisationAttempt{
		JobID: f.env.ID, OperationID: f.env.OperationID, InstanceID: f.env.installID,
		Kind: "recover", Source: "app", Target: backup.RestoringName("app"), Stanza: "app-abcdef",
		SourcePort: 5433, TemporaryPort: config.TemporaryPortMin,
	}

	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery = awaiting
		s.Restores["app"] = backup.Restore{
			ID: f.env.ID, Database: "app", Kind: backup.TargetLatest, Target: "latest",
			InstanceID: f.env.installID, OperationID: f.env.OperationID,
			Phase: backup.PhaseRestoring, StartedAt: "now", Promote: true,
			Recover: true, MaterialisationPending: true, Attempt: &f.attempt,
		}

		return nil
	}))

	return f
}

func (f *recoveryPoolerFixture) stubRecoveryHostSeams(t *testing.T) {
	t.Helper()

	oldHost, oldScaffold, oldMaterialise := jobRuntimeHostRead, recoverScaffoldApply, recoverMaterialise
	oldArm, oldPoolers, oldCheck := recoverArmArchiving, recoverCutoverPoolers, recoverCheckWritten
	oldFence, oldRender, oldRoutes := fenceRestorePoolers, applyPoolerFence, poolerRuntimeRoutes
	oldCredentials, oldSwap := restoreCredentialsCapture, swapCluster
	oldGuard, oldTarget, oldCopy := ensureCopyGuardAt, prepareTargetExtensions, prepareCopyExtensions

	t.Cleanup(func() {
		jobRuntimeHostRead, recoverScaffoldApply, recoverMaterialise = oldHost, oldScaffold, oldMaterialise
		recoverArmArchiving, recoverCutoverPoolers, recoverCheckWritten = oldArm, oldPoolers, oldCheck
		fenceRestorePoolers, applyPoolerFence, poolerRuntimeRoutes = oldFence, oldRender, oldRoutes
		restoreCredentialsCapture, swapCluster = oldCredentials, oldSwap
		ensureCopyGuardAt, prepareTargetExtensions, prepareCopyExtensions = oldGuard, oldTarget, oldCopy
	})

	call := func(name string) { *f.calls = append(*f.calls, name) }
	jobRuntimeHostRead = func(string) (hostinfo.Info, error) { return hostinfo.Info{VCPU: 2}, nil }
	recoverScaffoldApply = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, engine.Options) (engine.ApplyData, []output.Warning, error) {
		call("scaffold")
		return engine.ApplyData{}, nil, nil
	}
	recoverMaterialise = func(context.Context, *jobEnv, restoreParams) (int, *backup.MaterialisationAttempt, error) {
		call("materialise")

		attempt := f.attempt

		return config.TemporaryPortMin, &attempt, nil
	}
	recoverArmArchiving = func(context.Context, *jobEnv, config.Cluster, config.ClusterSpec, engine.Runtime) error {
		call("arm")
		return nil
	}
	recoverCutoverPoolers = func(_ context.Context, _ config.Cluster, _ engine.Runtime, recovering string, _ engine.Reporter) error {
		call("poolers")

		if recovering != "app" {
			t.Fatalf("first pooler preparation did not keep the exact target: %q", recovering)
		}

		return nil
	}
	recoverCheckWritten = func(context.Context, config.Cluster, config.ClusterSpec) error { return nil }
	fenceRestorePoolers = func(context.Context, string) error { call("fence"); return nil }
	applyPoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		call("render")
		return nil
	}
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		return map[string]steps.PoolerRoute{}, nil
	}
	restoreCredentialsCapture = func(context.Context, *restorePromotion) error { call("credentials"); return nil }
	swapCluster = func(context.Context, engine.Shell, string, engine.Reporter, string, string, string, string, int) error {
		call("swap")
		return nil
	}
	ensureCopyGuardAt = func(context.Context, string, *extensionCopyPlan, copyGuardEndpoint, func() error) ([]string, error) {
		return nil, nil
	}
	prepareTargetExtensions = func(context.Context, config.Cluster, config.ClusterSpec, engine.Runtime, engine.Reporter) error {
		return nil
	}
	prepareCopyExtensions = func(context.Context, *extensionCopyPlan, engine.Runtime, engine.Reporter) error { return nil }
}

func TestFirstRecoveryStartsBothPoolersBeforeTheStrictFence(t *testing.T) {
	f := newRecoveryPoolerFixture(t, false)
	f.stubRecoveryHostSeams(t)
	f.cfg.Clusters[0].Protected = true
	f.env.Cluster = f.cfg

	withheld := false
	recoverCutoverPoolers = func(_ context.Context, _ config.Cluster, _ engine.Runtime, recovering string, _ engine.Reporter) error {
		*f.calls = append(*f.calls, "poolers")

		recs, err := backup.LoadRecords()
		if err != nil {
			t.Fatal(err)
		}

		fence, fenced := recs.ActivationFenceFor("app")
		withheld = recovering == "app" && recs.IsAwaiting("app") && recs.Restores["app"].Recover &&
			fenced && fence.Active()

		return nil
	}
	recoverCheckWritten = func(context.Context, config.Cluster, config.ClusterSpec) error {
		*f.calls = append(*f.calls, "written")
		return nil
	}
	restoreCredentialsCapture = func(context.Context, *restorePromotion) error {
		*f.calls = append(*f.calls, "credentials")
		return errors.New("stop before swap")
	}

	if _, err := recoverDatabase(context.Background(), f.env, recoverParams{Database: "app"}); err == nil || err.Error() != "stop before swap" {
		t.Fatalf("recover error = %v, want the pre-swap stop", err)
	}

	if want := []string{"scaffold", "materialise", "arm", "poolers", "written", "fence", "render", "credentials"}; !slices.Equal(*f.calls, want) {
		t.Fatalf("cut-over order = %v, want %v", *f.calls, want)
	}

	if !withheld {
		t.Fatal("pooler preparation ran without the target and inspection aliases withheld")
	}

	if slices.Contains(*f.calls, "swap") {
		t.Fatal("the failed pre-swap step swapped the live cluster")
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if !recs.IsAwaiting("app") || recs.Restores["app"].Phase != backup.PhaseReady || !f.cfg.Clusters[0].Protected {
		t.Fatalf("failed first recovery state: awaiting=%v restore=%+v protected=%v", recs.IsAwaiting("app"), recs.Restores["app"], f.cfg.Clusters[0].Protected)
	}
}

func TestFirstRecoveryRechecksWriterAfterPoolerPreparation(t *testing.T) {
	f := newRecoveryPoolerFixture(t, false)
	f.stubRecoveryHostSeams(t)

	written := errors.New("another writer appeared during pooler startup")
	recoverCheckWritten = func(context.Context, config.Cluster, config.ClusterSpec) error {
		*f.calls = append(*f.calls, "written")
		return written
	}

	if _, err := jobRecover(context.Background(), f.env); !errors.Is(err, written) {
		t.Fatalf("recovery did not refuse the new writer: %v", err)
	}

	if !slices.Equal(*f.calls, []string{"scaffold", "materialise", "arm", "poolers", "written"}) {
		t.Fatalf("unsafe cutover order: %v", *f.calls)
	}
}

func TestFirstRecoveryDoesNotIgnoreUnfinishedNonAwaitingFollower(t *testing.T) {
	f := newRecoveryPoolerFixture(t, true)
	f.stubRecoveryHostSeams(t)
	f.cfg.Clusters[1].Follows = "app"
	f.cfg.Clusters[1].FollowGeneration = "unfinished"
	recoverCutoverPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		t.Fatal("non-awaiting follower was hidden by the routing view")
		return nil
	}

	if err := ensureRecoveryPoolers(context.Background(), f.env, f.cfg, "app", engine.Runtime{}); err != nil {
		t.Fatal(err)
	}
}

func TestFirstRecoveryPoolerFailureDoesNotSwap(t *testing.T) {
	f := newRecoveryPoolerFixture(t, false)
	f.stubRecoveryHostSeams(t)

	failure := errors.New("service auth setup failed")
	recoverCutoverPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		*f.calls = append(*f.calls, "poolers")
		return failure
	}

	if _, err := jobRecover(context.Background(), f.env); err == nil || !errors.Is(err, failure) {
		t.Fatalf("job recovery error = %v, want pooler setup failure", err)
	}

	if slices.Contains(*f.calls, "fence") || slices.Contains(*f.calls, "swap") {
		t.Fatalf("pooler failure reached the cut-over: %v", *f.calls)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	if recs.Restores["app"].Phase != backup.PhaseFailed || !recs.Restores["app"].MaterialisationPending || !recs.IsAwaiting("app") {
		t.Fatalf("failed pooler setup state: %+v", recs.Restores["app"])
	}
}

func TestLaterRecoveryKeepsTheOrdinaryStrictFenceFailure(t *testing.T) {
	f := newRecoveryPoolerFixture(t, true)
	f.stubRecoveryHostSeams(t)

	recoverCutoverPoolers = func(context.Context, config.Cluster, engine.Runtime, string, engine.Reporter) error {
		*f.calls = append(*f.calls, "unexpected poolers")
		return nil
	}
	fenceFailure := errors.New("pooler at /var/run/postgresql does not answer")
	fenceRestorePoolers = func(context.Context, string) error {
		*f.calls = append(*f.calls, "fence")
		return fenceFailure
	}

	if _, err := recoverDatabase(context.Background(), f.env, recoverParams{Database: "app"}); err == nil || !errors.Is(err, fenceFailure) {
		t.Fatalf("ordinary recovery error = %v, want the strict fence failure", err)
	}

	if !slices.Equal(*f.calls, []string{"scaffold", "materialise", "arm", "fence"}) {
		t.Fatalf("ordinary cut-over bypassed the strict fence: %v", *f.calls)
	}

	if slices.Contains(*f.calls, "swap") {
		t.Fatal("a failed ordinary strict fence swapped the database")
	}
}

type abortRecoveryFixture struct {
	cfg   config.Cluster
	env   *Env
	calls *[]string
}

func newAbortRecoveryFixture(t *testing.T, aside string) *abortRecoveryFixture {
	t.Helper()
	useTestInstance(t)

	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(t.TempDir(), "records.json")
	t.Cleanup(func() { backup.RecordsPath = oldRecords })
	cfg := config.Cluster{Server: config.Server{DataRoot: t.TempDir()}, Clusters: []config.ClusterSpec{
		{Name: "app", Port: 5433, Stanza: "app-abcdef", Size: "small", Protected: true},
	}}
	noExtensions := []string{}
	cfg.Clusters[0].RecoveryExtensions = &noExtensions
	cfg.Backup.Provider = "r2"
	r := backup.Restore{
		ID: "old-job", Database: "app", Kind: backup.TargetLatest, Target: "latest",
		InstanceID: "test-instance", OperationID: "old-operation",
		Phase: backup.PhaseFailed, StartedAt: "then", Promote: true, Recover: true,
		Port: config.TemporaryPortMin, Aside: aside, MaterialisationPending: true, Error: "poolers unavailable",
	}

	id := activationFenceIdentity{
		Database: "app", Kind: "restore-promote", OperationID: r.ID, Spec: cfg.Clusters[0],
		TargetStanza: "app-abcdef", OldStanza: "app-abcdef", NewStanza: "app-abcdef",
		Aside: aside, Registration: "app__restoring:5480:", Port: r.Port,
		DataDir: cfg.Server.DataDirectory("app__restoring"), ConfDir: steps.PGConfDir("app__restoring"),
	}
	if _, err := writeActivationFence(cfg, id); err != nil {
		t.Fatal(err)
	}

	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abcdef", Since: "then"}
		s.Restores["app"] = r

		return nil
	}))

	f := &abortRecoveryFixture{cfg: cfg, calls: new([]string)}
	f.env = &Env{Cluster: cfg, Req: rpc.Request{RequestID: "abort-job", Params: json.RawMessage(`{"database":"app"}`)}}

	return f
}

func (f *abortRecoveryFixture) stubAbortSeams(t *testing.T) {
	t.Helper()

	oldAbsence, oldDrop, oldSetting := restoreAbsenceState, restoreDropTemporary, legacyLeftoverEffectiveSetting
	oldRegistered, oldConf, oldData := retainedCopyRegistered, retainedCopyConfigDir, retainedCopyDataDirectory

	t.Cleanup(func() {
		restoreAbsenceState, restoreDropTemporary, legacyLeftoverEffectiveSetting = oldAbsence, oldDrop, oldSetting
		retainedCopyRegistered, retainedCopyConfigDir, retainedCopyDataDirectory = oldRegistered, oldConf, oldData
	})

	restoreAbsenceState = func() (engine.State, error) {
		return engine.State{RetainedJobs: map[string]engine.JobRecord{
			"old-job": {ID: "old-job", Job: "recover", DB: "app", State: engine.JobFailed},
		}}, nil
	}
	restoreDropTemporary = func(_ context.Context, _ config.Cluster, db string, _ engine.Reporter) error {
		*f.calls = append(*f.calls, "drop:"+db)
		return nil
	}
	legacyLeftoverEffectiveSetting = func(name, setting string) (string, error) {
		switch setting {
		case "data_directory":
			return f.cfg.Server.DataDirectory(name), nil
		case "cluster_name":
			return "18/" + name, nil
		case "port":
			return "5480", nil
		}

		return "", errors.New("unexpected setting")
	}
	retainedCopyRegistered = func(string) bool { return true }
	retainedCopyConfigDir = func(string) (bool, error) { return true, nil }
	retainedCopyDataDirectory = func(string) (string, error) { return f.cfg.Server.DataDirectory("app__restoring"), nil }
}

func TestAbortFailedRecoveryBeforeSwapKeepsAwaitingAndProtected(t *testing.T) {
	for _, aside := range []string{"", "app__pre_restore_20261118T000000"} {
		t.Run("aside="+aside, func(t *testing.T) {
			f := newAbortRecoveryFixture(t, aside)
			f.stubAbortSeams(t)

			if _, err := handleRestoreAbort(context.Background(), f.env); err != nil {
				t.Fatal(err)
			}

			if !slices.Equal(*f.calls, []string{"drop:app"}) {
				t.Fatalf("abort effects = %v, want only the temporary copy", *f.calls)
			}

			recs, err := backup.LoadRecords()
			if err != nil {
				t.Fatal(err)
			}

			if !recs.IsAwaiting("app") || len(recs.Restores) != 0 {
				t.Fatalf("abort did not leave the database awaiting recovery: awaiting=%v restores=%v", recs.AwaitingRecovery, recs.Restores)
			}

			if _, fenced := recs.ActivationFenceFor("app"); fenced {
				t.Fatal("abort left the recovery fence active")
			}

			spec, _ := f.cfg.ClusterByName("app")
			if !spec.Protected {
				t.Fatal("abort changed the protected state")
			}

			assertFreshRecoveryStarts(t, f)
		})
	}
}

func assertFreshRecoveryStarts(t *testing.T, f *abortRecoveryFixture) {
	t.Helper()

	oldFence, oldBackup, oldWritten := recoverCheckFence, recoverCheckBackup, recoverCheckWritten
	oldTarget, oldNamespace := restoreCheckTarget, restoreCheckNamespace

	t.Cleanup(func() {
		recoverCheckFence, recoverCheckBackup, recoverCheckWritten = oldFence, oldBackup, oldWritten
		restoreCheckTarget, restoreCheckNamespace = oldTarget, oldNamespace
	})

	recoverCheckFence = func(context.Context, config.Cluster, engine.Reporter) error { return nil }
	recoverCheckBackup = func(context.Context, config.ClusterSpec, backup.Records) error { return nil }
	recoverCheckWritten = func(context.Context, config.Cluster, config.ClusterSpec) error { return nil }
	restoreCheckTarget = func(context.Context, config.Cluster, restoreParams) (string, error) { return "latest", nil }
	restoreCheckNamespace = func(context.Context, config.Cluster, string) error { return nil }
	prepare := &Env{Cluster: f.cfg, Req: rpc.Request{RequestID: "fresh-job", OperationID: "fresh-operation"}}

	target, timeline := "", ""
	if err := prepareRecovery(context.Background(), prepare, recoverParams{Database: "app"}, &target, &timeline); err != nil {
		t.Fatal(err)
	}

	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}

	r := recs.Restores["app"]
	if r.ID != "fresh-job" || r.Phase != backup.PhaseRestoring || len(recs.Activation) != 0 || !recs.IsAwaiting("app") {
		t.Fatalf("fresh recovery did not start: %+v activation=%v awaiting=%v", r, recs.Activation, recs.AwaitingRecovery)
	}
}

func TestAbortFailedRecoveryFinishesAfterCopyWasDropped(t *testing.T) {
	f := newAbortRecoveryFixture(t, "app__pre_restore_20261118T000000")
	f.stubAbortSeams(t)

	retainedCopyRegistered = func(string) bool { return false }
	retainedCopyConfigDir = func(string) (bool, error) { return false, nil }

	if _, err := handleRestoreAbort(context.Background(), f.env); err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(*f.calls, []string{"drop:app"}) {
		t.Fatalf("retry cleanup effects = %v", *f.calls)
	}

	recs, err := backup.LoadRecords()
	if err != nil || len(recs.Restores) != 0 || len(recs.Activation) != 0 || !recs.IsAwaiting("app") {
		t.Fatalf("metadata retry state: %+v %v", recs, err)
	}
}

func TestAbortFailedRecoveryRejectsUnsafeState(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mutate     func(*backup.Records)
		nonFinal   bool
		badSetting bool
	}{
		{"foreign instance", func(s *backup.Records) {
			r := s.Restores["app"]
			r.InstanceID = "foreign"
			s.Restores["app"] = r
		}, false, false},
		{"changed stanza", func(s *backup.Records) {
			f := s.Activation["app"]
			f.TargetStanza = "other-stanza"
			s.Activation["app"] = f
		}, false, false},
		{"foreign fence", func(s *backup.Records) {
			f := s.Activation["app"]
			f.OperationID = "other-operation"
			s.Activation["app"] = f
		}, false, false},
		{"stale transition", func(s *backup.Records) {
			f := s.Activation["app"]
			f.TargetStanza, f.NewStanza = "stale-stanza", "stale-stanza"
			s.Activation["app"] = f
		}, false, false},
		{"live registration", func(s *backup.Records) {
			f := s.Activation["app"]
			f.Registration = "app:5480:"
			s.Activation["app"] = f
		}, false, false},
		{"awaiting stanza", func(s *backup.Records) {
			s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "stale-stanza"}
		}, false, false},
		{"retained attempt", func(s *backup.Records) {
			r := s.Restores["app"]
			r.Attempt = &backup.MaterialisationAttempt{}
			s.Restores["app"] = r
		}, false, false},
		{"nonfinal job", func(*backup.Records) {}, true, false},
		{"copy registration", func(*backup.Records) {}, false, true},
		{"post swap", func(*backup.Records) {}, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runUnsafeAbortRejection(t, tc.name, tc.mutate, tc.nonFinal, tc.badSetting)
		})
	}
}

func runUnsafeAbortRejection(t *testing.T, name string, mutate func(*backup.Records), nonFinal, badSetting bool) {
	t.Helper()

	aside := ""
	if name == "post swap" {
		aside = "app__pre_restore_20261118T000000"
	}

	f := newAbortRecoveryFixture(t, aside)
	f.stubAbortSeams(t)

	if name == "post swap" {
		mustNoErr(t, os.MkdirAll(filepath.Join(f.cfg.Server.DataRoot, aside), 0o700))
	}

	if nonFinal {
		restoreAbsenceState = func() (engine.State, error) {
			return engine.State{RetainedJobs: map[string]engine.JobRecord{
				"old-job": {ID: "old-job", Job: "recover", DB: "app", State: engine.JobRunning},
			}}, nil
		}
	}

	if badSetting {
		base := legacyLeftoverEffectiveSetting
		legacyLeftoverEffectiveSetting = func(name, setting string) (string, error) {
			if setting == "port" {
				return "5433", nil
			}

			return base(name, setting)
		}
	}

	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		mutate(s)
		return nil
	}))

	if _, err := handleRestoreAbort(context.Background(), f.env); err == nil {
		t.Fatal("unsafe abort was accepted")
	}

	if slices.Contains(*f.calls, "drop:app") {
		t.Fatal("unsafe abort dropped the copy")
	}

	recs, err := backup.LoadRecords()
	if err != nil || len(recs.Restores) != 1 || len(recs.Activation) != 1 {
		t.Fatalf("unsafe abort changed state: %+v %v", recs, err)
	}
}

func TestAbortFailedRecoveryRejectsEffectiveDataDirectory(t *testing.T) {
	f := newAbortRecoveryFixture(t, "")
	f.stubAbortSeams(t)

	base := legacyLeftoverEffectiveSetting
	legacyLeftoverEffectiveSetting = func(name, setting string) (string, error) {
		if setting == "data_directory" {
			return "/foreign/data", nil
		}

		return base(name, setting)
	}

	if _, err := handleRestoreAbort(context.Background(), f.env); err == nil {
		t.Fatal("effective data directory mismatch was accepted")
	}

	if len(*f.calls) != 0 {
		t.Fatalf("mismatched copy was modified: %v", *f.calls)
	}
}
