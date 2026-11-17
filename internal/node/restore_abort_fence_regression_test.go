package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/catalog"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/steps"
)

// preSwapRestoreAbortFixture is a ready, ordinary copy and its untouched live
// data. The aside is recorded but not created, so the swap has not started.
func preSwapRestoreAbortFixture(t *testing.T) (config.Cluster, backup.Restore, string, string) {
	t.Helper()
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Server.DataRoot = t.TempDir()
	spec := cfg.Clusters[0]
	empty := []string{}
	spec.RecoveryExtensions = &empty
	cfg.Clusters[0] = spec
	live, restoring := cfg.Server.DataDirectory("app"), cfg.Server.DataDirectory(backup.RestoringName("app"))
	for _, dir := range []string{live, restoring} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	r := backup.Restore{
		ID: "restore-abort-fence", Database: "app", Kind: backup.TargetLatest,
		Phase: backup.PhaseReady, StartedAt: "2026-01-01T00:00:00Z", Port: 6100,
		Aside:      "app__pre_restore_20260101T000000",
		InstanceID: "test-instance", OperationID: "invocation-1",
	}
	writeRestoreRecord(t, r)
	return cfg, r, live, restoring
}

func writeRestoreRecord(t *testing.T, r backup.Restore) {
	t.Helper()
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Restores[r.Database] = r
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func writePreSwapRestoreFence(t *testing.T, cfg config.Cluster, r backup.Restore) backup.ActivationFence {
	t.Helper()
	spec, ok := cfg.ClusterByName(r.Database)
	if !ok {
		t.Fatalf("fixture has no database %s", r.Database)
	}
	id := restorePromoteFenceIdentity(cfg, r, r.Aside, spec)
	if _, err := writeActivationFence(cfg, id); err != nil {
		t.Fatal(err)
	}
	f, ok := mustRecords(t).ActivationFenceFor(r.Database)
	if !ok {
		t.Fatal("the fixture wrote no activation fence")
	}
	return f
}

func writeRestoreAbortFence(t *testing.T, cfg config.Cluster, r backup.Restore) backup.ActivationFence {
	t.Helper()
	spec, ok := cfg.ClusterByName(r.Database)
	if !ok {
		t.Fatalf("fixture has no database %s", r.Database)
	}
	f := backup.ActivationFence{
		OperationID: r.ID, Kind: "restore-abort", Database: r.Database, Instance: r.InstanceID,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Aside: r.Aside, Registration: activationRegistration(spec), Ownership: spec.FollowGeneration,
		Port: spec.Port, DataDir: cfg.Server.DataDirectory(r.Database), ConfDir: steps.PGConfDir(r.Database),
		Phase: fencePhaseRouting, CreatedAt: "2026-01-01T00:00:00Z",
	}
	if err := backup.SetActivationFence(f); err != nil {
		t.Fatal(err)
	}
	return f
}

func mustRecords(t *testing.T) backup.Records {
	t.Helper()
	recs, err := backup.LoadRecords()
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func abortRestore(t *testing.T, cfg config.Cluster, dryRun bool) (Result, error) {
	t.Helper()
	params, err := json.Marshal(restoreDBParams{Database: "app"})
	if err != nil {
		t.Fatal(err)
	}
	return handleRestoreAbort(context.Background(), &Env{Cluster: cfg, Req: rpc.Request{Params: params, DryRun: dryRun}})
}

// abortRoutingStub models the poolers' runtime and records the exact order in
// which the abort publishes, verifies and releases the route.
type abortRoutingStub struct {
	spec              config.ClusterSpec
	publishErr        error
	pausedAfterResume bool
	resumed           bool
	sequence          []string
	published         []backup.ActivationFence
	onPublish         func(backup.ActivationFence)
	onResume          func()
}

func newAbortRoutingStub(t *testing.T, cfg config.Cluster) *abortRoutingStub {
	t.Helper()
	stubRestoreCredentialLifecycle(t)
	s := &abortRoutingStub{spec: cfg.Clusters[0]}
	oldApply, oldRoutes, oldResume := applyPoolerFence, poolerRuntimeRoutes, poolerResumeDatabase
	t.Cleanup(func() { applyPoolerFence, poolerRuntimeRoutes, poolerResumeDatabase = oldApply, oldRoutes, oldResume })
	applyPoolerFence = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter) error {
		s.sequence = append(s.sequence, "apply")
		if s.publishErr != nil {
			return s.publishErr
		}
		recs := mustRecords(t)
		f, ok := recs.ActivationFenceFor(s.spec.Name)
		if !ok {
			t.Error("pooler render found no abort routing fence")
			return errors.New("no abort routing fence")
		}
		s.published = append(s.published, f)
		if s.onPublish != nil {
			s.onPublish(f)
		}
		return nil
	}
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		s.sequence = append(s.sequence, "routes")
		route := steps.PoolerRoute{Name: s.spec.Name, Host: steps.PGSocketDir, Port: s.spec.Port, Database: s.spec.DatabaseName()}
		if !s.resumed || s.pausedAfterResume {
			route.Paused = true
		}
		return map[string]steps.PoolerRoute{s.spec.Name: route}, nil
	}
	poolerResumeDatabase = func(context.Context, string) {
		s.sequence = append(s.sequence, "resume")
		s.resumed = true
		if s.onResume != nil {
			s.onResume()
		}
	}
	return s
}

type restoreAbortRefusalCase struct {
	name          string
	mutateRestore func(*backup.Restore)
	mutateFence   func(*backup.ActivationFence, config.Cluster)
}

func assertAbortRoutingSequence(t *testing.T, s *abortRoutingStub) {
	t.Helper()
	want := []string{"apply", "routes", "routes", "resume", "routes", "routes"}
	if !slices.Equal(s.sequence, want) {
		t.Fatalf("routing sequence = %v, want %v", s.sequence, want)
	}
}

func assertAbortFence(t *testing.T, f backup.ActivationFence, spec config.ClusterSpec, want backup.ActivationFence, live string) {
	t.Helper()
	if f.Kind != "restore-abort" || f.Phase != fencePhaseRouting || f.OperationID != want.OperationID {
		t.Fatalf("abort fence = %+v", f)
	}
	if f.Instance != want.Instance || f.Database != spec.Name {
		t.Fatalf("abort fence provenance = %+v", f)
	}
	if f.TargetStanza != spec.Stanza || f.OldStanza != spec.Stanza || f.NewStanza != spec.Stanza {
		t.Fatalf("abort fence stanza = %+v, want %s", f, spec.Stanza)
	}
	if f.Port != spec.Port || f.DataDir != live || f.ConfDir != steps.PGConfDir(spec.Name) {
		t.Fatalf("abort fence live endpoint = %+v", f)
	}
	if f.Registration != activationRegistration(spec) || f.Ownership != spec.FollowGeneration {
		t.Fatalf("abort fence registration/ownership = %+v", f)
	}
	if f.Aside != want.Aside || f.CreatedAt != want.CreatedAt {
		t.Fatalf("abort fence did not preserve aside/created time: %+v, want %+v", f, want)
	}
	if len(f.Requirements) != 0 || len(f.Renames) != 0 {
		t.Fatalf("abort fence retained promotion facts: %+v", f)
	}
}

// TestPreSwapRestoreAbortRetiresPreparedFence is the F7-73 regression: a
// promote that fails at the pooler cut leaves its prepared fence, and abort
// converts that exact authority into a durable routing continuation, publishes
// both poolers, verifies them, releases the pause and only then clears.
func TestPreSwapRestoreAbortRetiresPreparedFence(t *testing.T) {
	cfg, r, live, restoring := preSwapRestoreAbortFixture(t)
	oldEnsure, oldFence := ensureCopyGuardAt, fenceRestorePoolers
	ensureCopyGuardAt = func(context.Context, string, *extensionCopyPlan, copyGuardEndpoint, func() error) ([]string, error) {
		return nil, nil
	}
	marker := errors.New("pooler fence failed")
	fenceRestorePoolers = func(context.Context, string) error { return marker }
	t.Cleanup(func() { ensureCopyGuardAt, fenceRestorePoolers = oldEnsure, oldFence })
	if _, err := promote(context.Background(), cfg, "app", engine.Runtime{}, nil, func() {}); !errors.Is(err, marker) {
		t.Fatalf("promote fence failure = %v", err)
	}
	prepared, ok := mustRecords(t).ActivationFenceFor("app")
	if !ok || prepared.Phase != fencePhasePrepared || prepared.OperationID != r.ID || prepared.Aside != r.Aside {
		t.Fatalf("the failed promote did not leave its prepared fence: %+v", prepared)
	}
	stub := newAbortRoutingStub(t, cfg)
	stub.onPublish = func(f backup.ActivationFence) {
		assertAbortFence(t, f, cfg.Clusters[0], prepared, live)
		recs := mustRecords(t)
		if _, ok := recs.Restores["app"]; ok {
			t.Fatal("conversion kept the restore record")
		}
	}
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort: %v", err)
	}
	assertAbortRoutingSequence(t, stub)
	if len(stub.published) != 1 {
		t.Fatalf("abort published %d times, want 1", len(stub.published))
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; ok {
		t.Fatal("abort left the restore record")
	}
	if _, ok := recs.ActivationFenceFor("app"); ok {
		t.Fatal("abort left the routing continuation")
	}
	if len(recs.Leftovers) != 0 || len(recs.Timelines) != 0 || len(recs.DroppedStanzas) != 0 || len(recs.AwaitingRecovery) != 0 {
		t.Fatalf("abort created promotion facts: %+v", recs)
	}
	if _, err := os.Stat(restoring); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abort left the temporary copy: %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatalf("abort touched the live data: %v", err)
	}
	if err := refuseActiveActivationFence(cfg, "app"); err != nil {
		t.Fatalf("the live database stayed fenced after abort: %v", err)
	}
}

// TestRestoreAbortRetainsRoutingOnRenderFailure proves a failed publish keeps
// the abort-owned continuation and never resumes. A retried abort finishes it
// without repeating cleanup.
func TestRestoreAbortRetainsRoutingOnRenderFailure(t *testing.T) {
	cfg, r, _, restoring := preSwapRestoreAbortFixture(t)
	prepared := writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	marker := errors.New("pooler render failed")
	stub.publishErr = marker
	res, err := abortRestore(t, cfg, false)
	if err != nil {
		t.Fatalf("abort render failure = %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("abort warnings = %+v", res.Warnings)
	}
	if stub.resumed || len(stub.sequence) != 1 || stub.sequence[0] != "apply" {
		t.Fatalf("render failure sequence = %v, resumed=%v", stub.sequence, stub.resumed)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; ok {
		t.Fatal("render failure lost the conversion by keeping the restore record")
	}
	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Kind != "restore-abort" || f.Phase != fencePhaseRouting || f.OperationID != prepared.OperationID {
		t.Fatalf("render failure did not retain the abort continuation: %+v", f)
	}
	if _, err := os.Stat(restoring); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("render failure left the temporary copy: %v", err)
	}
	// The retry sees no restore record and must finish only routing.
	stub.publishErr = nil
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
	if stub.resumed != true {
		t.Fatal("abort retry did not release the pause")
	}
}

// TestRestoreAbortResumeVerificationFailureRetainsContinuation proves a route
// that still reports the pause after RESUME is not cleared, and a later retry
// can finish once the runtime agrees.
func TestRestoreAbortResumeVerificationFailureRetainsContinuation(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	stub.pausedAfterResume = true
	res, err := abortRestore(t, cfg, false)
	if err != nil {
		t.Fatalf("abort pause failure = %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("abort warnings = %+v", res.Warnings)
	}
	if stub.sequence[len(stub.sequence)-1] != "routes" {
		t.Fatalf("pause failure sequence = %v", stub.sequence)
	}
	f, ok := mustRecords(t).ActivationFenceFor("app")
	if !ok || f.Kind != "restore-abort" || f.Phase != fencePhaseRouting {
		t.Fatalf("pause failure did not retain the continuation: %+v", f)
	}
	stub.pausedAfterResume = false
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
}

// TestRestoreAbortClearFailureRetainsContinuationForRetry makes the final
// records compare-and-set fail after both routes and the pause were verified.
// The continuation must survive and a retry must clear it.
func TestRestoreAbortClearFailureRetainsContinuationForRetry(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	dir := filepath.Dir(backup.RecordsPath)
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	stub := newAbortRoutingStub(t, cfg)
	stub.onResume = func() {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Error(err)
		}
	}
	res, err := abortRestore(t, cfg, false)
	if err != nil {
		t.Fatalf("abort clear failure = %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("abort warnings = %+v", res.Warnings)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, ok := mustRecords(t).ActivationFenceFor("app")
	if !ok || f.Kind != "restore-abort" || f.Phase != fencePhaseRouting {
		t.Fatalf("clear failure did not retain the continuation: %+v", f)
	}
	stub.onResume = nil
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
}

// TestRestoreAbortApplyRetryFinalizesContinuation proves the ordinary apply
// finalizer resumes the abort-owned continuation and uses the strict publish.
func TestRestoreAbortApplyRetryFinalizesContinuation(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	stub.publishErr = errors.New("render failed")
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); !ok {
		t.Fatal("abort did not leave its continuation")
	}
	stub.publishErr = nil
	if dbs := routingFencesFor(stepParams{DB: "app"}); !slices.Equal(dbs, []string{"app"}) {
		t.Fatalf("apply selected routing fences %v, want [app]", dbs)
	}
	if warnings := finalizeRoutingFences(context.Background(), &Env{Cluster: cfg}, []string{"app"}); len(warnings) != 0 {
		t.Fatalf("apply retry warnings = %+v", warnings)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("apply retry left its routing continuation")
	}
	if !stub.resumed {
		t.Fatal("apply retry did not release the pause")
	}
}

// TestRestoreAbortDryRunDoesNotConvertOrPublish checks both entry points keep
// every effect: a prepared fence and its copy stay untouched, and a durable
// continuation is not rendered or resumed.
func TestRestoreAbortDryRunDoesNotConvertOrPublish(t *testing.T) {
	cfg, r, _, restoring := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	if _, err := abortRestore(t, cfg, true); err != nil {
		t.Fatalf("dry-run abort: %v", err)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("dry-run removed the restore record")
	}
	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Kind != "restore-promote" || f.Phase != fencePhasePrepared {
		t.Fatalf("dry-run changed the prepared fence: %+v", f)
	}
	if _, err := os.Stat(restoring); err != nil {
		t.Fatalf("dry-run dropped the temporary copy: %v", err)
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("dry-run touched the poolers: %v", stub.sequence)
	}
}

// TestRestoreAbortDryRunContinuationDoesNotPublish proves a retry of a durable
// continuation is also read-only under --dry-run.
func TestRestoreAbortDryRunContinuationDoesNotPublish(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	stub.publishErr = errors.New("render failed")
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); !ok {
		t.Fatal("abort did not leave its continuation")
	}
	before := len(stub.sequence)
	if _, err := abortRestore(t, cfg, true); err != nil {
		t.Fatalf("dry-run continuation: %v", err)
	}
	if len(stub.sequence) != before {
		t.Fatalf("dry-run continuation touched the poolers: %v", stub.sequence)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); !ok {
		t.Fatal("dry-run continuation changed the fence")
	}
}

// TestRestoreAbortRefusesMismatchedOrForeignAuthority refuses before cleanup
// when the fence or record is not the exact captured pre-swap authority.
func TestRestoreAbortRefusesMismatchedOrForeignAuthority(t *testing.T) {
	cases := []restoreAbortRefusalCase{
		{"operation", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.OperationID = "another-operation" }},
		{"invocation-is-not-owner", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.OperationID = "invocation-1" }},
		{"kind", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.Kind = "fork" }},
		{"swapped-phase", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.Phase = fencePhaseSwapped }},
		{"foreign-instance", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.Instance = "another-instance" }},
		{"aside", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.Aside = "app__pre_restore_other" }},
		{"registration", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.Registration = "app:6100:" }},
		{"port", nil, func(f *backup.ActivationFence, _ config.Cluster) {
			f.Port, f.Registration = 6200, "app__restoring:6200:"
		}},
		{"live-endpoint", nil, func(f *backup.ActivationFence, cfg config.Cluster) {
			f.DataDir, f.ConfDir, f.Port = cfg.Server.DataDirectory("app"), steps.PGConfDir("app"), 5433
			f.Registration = "app:5433:"
		}},
		{"stanza", nil, func(f *backup.ActivationFence, _ config.Cluster) { f.TargetStanza = "another-stanza" }},
		{"recover", func(r *backup.Restore) { r.Recover = true }, nil},
		{"missing-instance", func(r *backup.Restore) { r.InstanceID = "" }, nil},
		{"foreign-record-instance", func(r *backup.Restore) { r.InstanceID = "another-instance" }, nil},
		{"missing-invocation", func(r *backup.Restore) { r.OperationID = "" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertRestoreAbortRefusal(t, tc) })
	}
}

func assertRestoreAbortRefusal(t *testing.T, tc restoreAbortRefusalCase) {
	t.Helper()
	cfg, r, _, restoring := preSwapRestoreAbortFixture(t)
	if tc.mutateRestore != nil {
		tc.mutateRestore(&r)
		writeRestoreRecord(t, r)
	}
	f := writePreSwapRestoreFence(t, cfg, r)
	if tc.mutateFence != nil {
		tc.mutateFence(&f, cfg)
		if err := backup.SetActivationFence(f); err != nil {
			t.Fatal(err)
		}
	}
	stub := newAbortRoutingStub(t, cfg)
	if _, err := abortRestore(t, cfg, false); err == nil {
		t.Fatal("abort accepted authority it did not own")
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("refusal touched the poolers: %v", stub.sequence)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("refusal lost the restore record")
	}
	got, ok := recs.ActivationFenceFor("app")
	if !ok || got.OperationID != f.OperationID || got.Kind != f.Kind || got.Phase != f.Phase || got.Aside != f.Aside {
		t.Fatalf("refusal changed the fence: %+v, want %+v", got, f)
	}
	if _, err := os.Stat(restoring); err != nil {
		t.Fatalf("refusal dropped the temporary copy: %v", err)
	}
}

// TestPreSwapRestoreAbortConversionRechecksExactAuthority proves the locked
// conversion rechecks the captured record, fence, swap boundary and live
// stanza, not only the earlier pre-cleanup verification.
func TestPreSwapRestoreAbortConversionRechecksExactAuthority(t *testing.T) {
	t.Run("record-changed", conversionRecheckRecordChanged)
	t.Run("fence-changed", conversionRecheckFenceChanged)
	t.Run("swap-started", conversionRecheckSwapStarted)
	t.Run("live-stanza-changed", conversionRecheckLiveStanzaChanged)
}

func capturePreSwapAbort(t *testing.T, cfg config.Cluster, r backup.Restore) preSwapRestoreAbort {
	t.Helper()
	auth, fenced, err := verifyPreSwapRestoreAbort(cfg, "app", r, r.Aside)
	if err != nil || !fenced {
		t.Fatalf("capture: %+v, %v, %v", auth, fenced, err)
	}
	return auth
}

func conversionRecheckRecordChanged(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	auth := capturePreSwapAbort(t, cfg, r)
	r.OperationID = "changed-invocation"
	writeRestoreRecord(t, r)
	if err := convertPreSwapRestoreAbort(cfg, "app", auth); err == nil {
		t.Fatal("conversion accepted a changed invocation")
	}
	assertConversionRefused(t)
}

func conversionRecheckFenceChanged(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	f := writePreSwapRestoreFence(t, cfg, r)
	auth := capturePreSwapAbort(t, cfg, r)
	f.Renames = map[string]string{"old": "app"}
	if err := backup.SetActivationFence(f); err != nil {
		t.Fatal(err)
	}
	if err := convertPreSwapRestoreAbort(cfg, "app", auth); err == nil {
		t.Fatal("conversion accepted a changed fence")
	}
	assertConversionRefused(t)
}

func conversionRecheckSwapStarted(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	auth := capturePreSwapAbort(t, cfg, r)
	if err := os.MkdirAll(cfg.Server.DataDirectory(r.Aside), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := convertPreSwapRestoreAbort(cfg, "app", auth); err == nil {
		t.Fatal("conversion accepted a started swap")
	}
	assertConversionRefused(t)
}

func conversionRecheckLiveStanzaChanged(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	auth := capturePreSwapAbort(t, cfg, r)
	cfg.Clusters[0].Stanza = "app-other"
	if err := convertPreSwapRestoreAbort(cfg, "app", auth); err == nil {
		t.Fatal("conversion accepted a changed live stanza")
	}
	assertConversionRefused(t)
}

func assertConversionRefused(t *testing.T) {
	t.Helper()
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("conversion refusal lost the restore record")
	}
	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Kind == "restore-abort" {
		t.Fatalf("conversion refusal changed the fence: %+v", f)
	}
}

// TestForkAdoptingFenceKeepsTemporaryPortNotPaths preserves the separate
// exceptions: a fork's adopting fence may carry the temporary port but never
// the restoring data/configuration paths.
func TestForkAdoptingFenceKeepsTemporaryPortNotPaths(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	base := backup.ActivationFence{
		OperationID: "fork-adopting", Kind: "fork", Database: "app", Instance: "test-instance",
		TargetStanza: spec.Stanza, OldStanza: "app-source", NewStanza: spec.Stanza,
		Registration: "app:6100:", Ownership: spec.FollowGeneration,
		Port: 6100, DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
		Phase: fencePhaseAdopting, CreatedAt: "2026-01-01T00:00:00Z",
	}
	restoringPaths := base
	restoringPaths.DataDir = cfg.Server.DataDirectory(backup.RestoringName("app"))
	restoringPaths.ConfDir = steps.PGConfDir(backup.RestoringName("app"))
	restoringPaths.Registration, restoringPaths.Port = activationRegistration(spec), spec.Port
	if err := backup.SetActivationFence(restoringPaths); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyActivationFence(cfg, "app", "fork-adopting", "fork", fencePhaseAdopting); err == nil {
		t.Fatal("fork/adopting accepted the restoring data/configuration paths")
	}
	if err := backup.SetActivationFence(base); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyActivationFence(cfg, "app", "fork-adopting", "fork", fencePhaseAdopting); err != nil {
		t.Fatalf("fork/adopting lost its temporary-port exception: %v", err)
	}
}

// TestScopedApplyRefusesRevertedDumpPromoteFence proves the explicit routing
// target/port checks survive the full-snapshot CAS: a dump promote's fence
// still naming the old/reverted stanza is never cleared by a scoped apply.
func TestScopedApplyRefusesRevertedDumpPromoteFence(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	cfg.Clusters[0].Stanza = "app-new"
	spec := cfg.Clusters[0]
	fence := backup.ActivationFence{
		OperationID: "dump-reverted", Kind: "restore-promote", Database: "app", Instance: "test-instance",
		TargetStanza: "app-old", OldStanza: "app-old", NewStanza: spec.Stanza,
		Registration: activationRegistration(spec), Ownership: spec.FollowGeneration,
		Port: spec.Port, DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
		Phase: fencePhaseRouting, CreatedAt: "2026-01-01T00:00:00Z",
	}
	if err := backup.SetActivationFence(fence); err != nil {
		t.Fatal(err)
	}
	oldRoutes, oldResume := poolerRuntimeRoutes, poolerResumeDatabase
	t.Cleanup(func() { poolerRuntimeRoutes, poolerResumeDatabase = oldRoutes, oldResume })
	resumes := 0
	poolerRuntimeRoutes = func(context.Context, string, int) (map[string]steps.PoolerRoute, error) {
		return map[string]steps.PoolerRoute{
			spec.Name: {Name: spec.Name, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()},
		}, nil
	}
	poolerResumeDatabase = func(context.Context, string) { resumes++ }
	warnings := finalizeRoutingFences(context.Background(), &Env{Cluster: cfg}, []string{"app"})
	if len(warnings) != 1 || warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("scoped apply warnings = %+v", warnings)
	}
	if resumes != 1 {
		t.Fatalf("pause releases = %d, want 1 before the target check", resumes)
	}
	f, ok := mustRecords(t).ActivationFenceFor("app")
	if !ok || f.TargetStanza != "app-old" || f.OperationID != "dump-reverted" {
		t.Fatalf("reverted dump-promote fence was cleared: %+v", f)
	}
}

// TestScopedApplyRefusesRoutingFenceWithStalePort keeps the explicit
// routing-port check alongside the snapshot CAS: a fence for another port is
// never cleared as this database's route.
func TestScopedApplyRefusesRoutingFenceWithStalePort(t *testing.T) {
	useTempExtensionRecords(t)
	useTestInstance(t)
	cfg := planCfg(t)
	spec := cfg.Clusters[0]
	fence := backup.ActivationFence{
		OperationID: "stale-port", Kind: "restore-promote", Database: "app", Instance: "test-instance",
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Registration: "app:5434:", Ownership: spec.FollowGeneration,
		Port: 5434, DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
		Phase: fencePhaseRouting, CreatedAt: "2026-01-01T00:00:00Z",
	}
	if err := backup.SetActivationFence(fence); err != nil {
		t.Fatal(err)
	}
	warnings := finalizeRoutingFences(context.Background(), &Env{Cluster: cfg}, []string{"app"})
	if len(warnings) != 1 || warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("scoped apply warnings = %+v", warnings)
	}
	f, ok := mustRecords(t).ActivationFenceFor("app")
	if !ok || f.Port != 5434 || f.OperationID != "stale-port" {
		t.Fatalf("stale-port routing fence was cleared: %+v", f)
	}
}

// TestRestoreAbortContinuationRefusesCoexistingRestoreRecord proves the common
// abort finalizer checks restore absence before publication/resume even for a
// noninspection-routable PhaseRestoring record, and both abort and apply retry
// after the ambiguous record is gone.
func TestRestoreAbortContinuationRefusesCoexistingRestoreRecord(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writeRestoreAbortFence(t, cfg, r)
	r.Phase = backup.PhaseRestoring
	writeRestoreRecord(t, r)
	stub := newAbortRoutingStub(t, cfg)
	if err := completeRestoreAbortRouting(context.Background(), &Env{Cluster: cfg}, "app", r.ID); err == nil ||
		!strings.Contains(err.Error(), "open restore record") {
		t.Fatalf("coexisting continuation = %v", err)
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("coexisting continuation touched the poolers: %v", stub.sequence)
	}
	if warnings := finalizeRoutingFences(context.Background(), &Env{Cluster: cfg}, []string{"app"}); len(warnings) != 1 {
		t.Fatalf("apply retry warnings = %+v", warnings)
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("apply retry touched the poolers: %v", stub.sequence)
	}
	if _, err := abortRestore(t, cfg, false); err == nil || !strings.Contains(err.Error(), "both an open restore") {
		t.Fatalf("abort retry = %v", err)
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("abort retry touched the poolers: %v", stub.sequence)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, "app")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry after clearing the record: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
}

// TestRestoreAbortClearRefusesRecordAppearingAtClear proves the locked clear
// refuses a restore record that appeared after publication started.
func TestRestoreAbortClearRefusesRecordAppearingAtClear(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	appeared := r
	appeared.ID, appeared.Phase = "appeared-at-clear", backup.PhaseRestoring
	stub.onResume = func() { writeRestoreRecord(t, appeared) }
	res, err := abortRestore(t, cfg, false)
	if err != nil {
		t.Fatalf("abort record-at-clear = %v", err)
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Code != "activation_routing_incomplete" {
		t.Fatalf("abort warnings = %+v", res.Warnings)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("the record that appeared at clear disappeared")
	}
	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Kind != "restore-abort" || f.Phase != fencePhaseRouting {
		t.Fatalf("clear accepted a record that appeared at clear: %+v", f)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		delete(s.Restores, "app")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stub.onResume = nil
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry after the appeared record is gone: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
}

// TestRestoreAbortFenceIsRoutingOnlyAndLive proves the new kind cannot be
// prepared or advanced, and that routing rejects a temporary endpoint,
// promotion facts or a non-live registration/ownership.
func TestRestoreAbortFenceIsRoutingOnlyAndLive(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	spec := cfg.Clusters[0]
	if _, err := writeActivationFence(cfg, activationFenceIdentity{
		Database: "app", Kind: "restore-abort", OperationID: r.ID, Spec: spec,
		TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
		Aside: r.Aside, Registration: activationRegistration(spec), Port: spec.Port,
		DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
	}); err == nil {
		t.Fatal("restore-abort was created in prepared phase")
	}
	if err := advanceActivationFence(cfg, "app", r.ID, "restore-abort", fencePhasePrepared, fencePhaseRouting, spec, nil); err == nil {
		t.Fatal("restore-abort advanced through a phase")
	}
	cases := []struct {
		name   string
		mutate func(*backup.ActivationFence)
	}{
		{"prepared", func(f *backup.ActivationFence) { f.Phase = fencePhasePrepared }},
		{"temporary-endpoint", func(f *backup.ActivationFence) {
			f.DataDir, f.ConfDir, f.Port = cfg.Server.DataDirectory(backup.RestoringName("app")), steps.PGConfDir(backup.RestoringName("app")), 6100
			f.Registration = "app__restoring:6100:"
		}},
		{"ownership", func(f *backup.ActivationFence) { f.Ownership = "other-generation" }},
		{"registration", func(f *backup.ActivationFence) { f.Registration = "app:5433:other-generation" }},
		{"requirements", func(f *backup.ActivationFence) { f.Requirements = []catalog.Extension{{Name: "custom"}} }},
		{"renames", func(f *backup.ActivationFence) { f.Renames = map[string]string{"old": "app"} }},
		{"aside", func(f *backup.ActivationFence) { f.Aside = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := backup.ActivationFence{
				OperationID: r.ID, Kind: "restore-abort", Database: "app", Instance: r.InstanceID,
				TargetStanza: spec.Stanza, OldStanza: spec.Stanza, NewStanza: spec.Stanza,
				Aside: r.Aside, Registration: activationRegistration(spec), Ownership: spec.FollowGeneration,
				Port: spec.Port, DataDir: cfg.Server.DataDirectory("app"), ConfDir: steps.PGConfDir("app"),
				Phase: fencePhaseRouting, CreatedAt: "2026-01-01T00:00:00Z",
			}
			tc.mutate(&f)
			if err := backup.SetActivationFence(f); err != nil {
				t.Fatal(err)
			}
			if _, _, err := verifyActivationFence(cfg, "app", r.ID, "restore-abort", fencePhaseRouting); err == nil {
				t.Fatal("invalid restore-abort fence was accepted")
			}
		})
	}
}

func TestRestoreAbortRefusesOpenRestoreAndAbortFence(t *testing.T) {
	cfg, r, _, restoring := preSwapRestoreAbortFixture(t)
	f := writeRestoreAbortFence(t, cfg, r)
	stub := newAbortRoutingStub(t, cfg)
	_, err := abortRestore(t, cfg, false)
	if err == nil || !strings.Contains(err.Error(), "both an open restore and a restore-abort") {
		t.Fatalf("ambiguous abort = %v", err)
	}
	if len(stub.sequence) != 0 {
		t.Fatalf("ambiguous abort touched the poolers: %v", stub.sequence)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("ambiguous abort lost the restore record")
	}
	got, ok := recs.ActivationFenceFor("app")
	if !ok || got.Kind != "restore-abort" || got.OperationID != f.OperationID {
		t.Fatalf("ambiguous abort changed the fence: %+v", got)
	}
	if _, err := os.Stat(restoring); err != nil {
		t.Fatalf("ambiguous abort dropped the temporary copy: %v", err)
	}
}

// TestReadyRestoreAbortWithoutFenceDropsCopy keeps the ordinary ready-copy
// abort working: a restore aborted before promotion never wrote a fence.
func TestReadyRestoreAbortWithoutFenceDropsCopy(t *testing.T) {
	cfg, _, _, restoring := preSwapRestoreAbortFixture(t)
	oldReload := restoreAbortReload
	reloads := 0
	restoreAbortReload = func(context.Context, *Env, config.Cluster) error { reloads++; return nil }
	t.Cleanup(func() { restoreAbortReload = oldReload })
	stub := newAbortRoutingStub(t, cfg)
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort: %v", err)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; ok {
		t.Fatal("abort left the fence-less restore record")
	}
	if _, ok := recs.ActivationFenceFor("app"); ok {
		t.Fatal("fence-less abort created routing authority")
	}
	if _, err := os.Stat(restoring); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abort left the temporary copy: %v", err)
	}
	if reloads != 1 || len(stub.sequence) != 0 {
		t.Fatalf("fence-less abort reloads=%d pooler sequence=%v", reloads, stub.sequence)
	}
}

// TestPreSwapRestoreAbortRecordUpdateFailureKeepsBothAuthorities proves the
// conversion is atomic: a failed records write leaves the restore and the
// prepared fence whole, and a retry can finish.
func TestPreSwapRestoreAbortRecordUpdateFailureKeepsBothAuthorities(t *testing.T) {
	cfg, r, _, _ := preSwapRestoreAbortFixture(t)
	writePreSwapRestoreFence(t, cfg, r)
	dir := filepath.Dir(backup.RecordsPath)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	if _, err := abortRestore(t, cfg, false); err == nil {
		t.Fatal("abort ignored a failed records update")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	recs := mustRecords(t)
	if _, ok := recs.Restores["app"]; !ok {
		t.Fatal("a failed conversion deleted the restore record")
	}
	f, ok := recs.ActivationFenceFor("app")
	if !ok || f.Kind != "restore-promote" || f.Phase != fencePhasePrepared {
		t.Fatalf("a failed conversion changed the prepared fence: %+v", f)
	}
	stub := newAbortRoutingStub(t, cfg)
	if _, err := abortRestore(t, cfg, false); err != nil {
		t.Fatalf("abort retry: %v", err)
	}
	if _, ok := mustRecords(t).ActivationFenceFor("app"); ok {
		t.Fatal("abort retry left its routing continuation")
	}
	assertAbortRoutingSequence(t, stub)
}
