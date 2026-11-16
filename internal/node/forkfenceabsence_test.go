package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

const (
	fenceOnlyJobID  = "01M7HXJK0CWK21WHJD06TB1ZJX"
	fenceOnlyJob2ID = "01M7J0ACVHQK1PSWS4AT97EQXM"
)

type fenceOnlyFixture struct {
	t           *testing.T
	cfg         config.Cluster
	root, ports string
	fence       backup.ActivationFence
	job         engine.JobRecord
	state       engine.State
	store       *forkAbsenceStore
	release     string
}

func newFenceOnlyFixture(t *testing.T) *fenceOnlyFixture {
	t.Helper()
	useTempRecords(t)
	_, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	ports := filepath.Join(filepath.Dir(root), "ports")
	mustNoErr(t, os.Mkdir(ports, 0o700))
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Stanza: "app-abcdef", Port: 5432, Size: "small"}}
	f := &fenceOnlyFixture{t: t, cfg: cfg, root: root, ports: ports}
	f.store = &forkAbsenceStore{cfg: cfg}
	oldState, oldReservation, oldStore, oldUpdate, oldRelease := forkAbsenceState, forkAbsenceReservation, dbDropStore, forkFenceUpdateRecords, releaseMaterialisationJob
	t.Cleanup(func() {
		forkAbsenceState, forkAbsenceReservation, dbDropStore, forkFenceUpdateRecords, releaseMaterialisationJob = oldState, oldReservation, oldStore, oldUpdate, oldRelease
	})
	f.job = engine.JobRecord{ID: fenceOnlyJobID, Job: "db-fork", DB: "target", State: engine.JobFailed}
	f.state = engine.State{Jobs: []engine.JobRecord{f.job}}
	f.fence = backup.ActivationFence{
		OperationID: fenceOnlyJobID, Kind: "fork", Database: "target", Instance: "install",
		TargetStanza: "target-abcdef", OldStanza: "source-abcdef", NewStanza: "target-abcdef",
		Registration: "target:5433:", Ownership: "",
		Port: 5433, DataDir: filepath.Join(root, "target"), ConfDir: filepath.Join(materialisationConfigRoot, "target"),
		Phase: fencePhasePrepared, CreatedAt: "2026-11-01T00:00:00Z",
	}
	forkAbsenceState = func() (engine.State, error) { return f.state, nil }
	forkAbsenceReservation = func(ctx context.Context, name string) (func(), error) {
		return backup.LockTemporaryReservationAbsenceWithHost(ctx, f.ports, materialisationRecordUID, name)
	}
	dbDropStore = func() state.Store { return f.store }
	releaseMaterialisationJob = func(id string) { f.release = id }
	return f
}

func (f *fenceOnlyFixture) writeRecords(t *testing.T, fences ...backup.ActivationFence) {
	t.Helper()
	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		if s.Activation == nil {
			s.Activation = map[string]backup.ActivationFence{}
		}
		for _, fence := range fences {
			s.Activation[fence.Database] = fence
		}
		return nil
	}))
}

func (f *fenceOnlyFixture) drop(t *testing.T) (Result, error) {
	t.Helper()
	return f.dropName(t, "target")
}

func (f *fenceOnlyFixture) dropName(t *testing.T, name string) (Result, error) {
	t.Helper()
	params, err := json.Marshal(map[string]string{"name": name})
	mustNoErr(t, err)
	env := &Env{Cluster: f.cfg, Req: rpc.Request{Params: params, Confirm: name}}
	return handleDBDrop(context.Background(), env)
}

func (f *fenceOnlyFixture) assertGone(t *testing.T) {
	t.Helper()
	recs, err := backup.LoadRecords()
	mustNoErr(t, err)
	if _, ok := recs.ActivationFenceFor("target"); ok {
		t.Fatal("activation fence remains")
	}
	if _, ok := f.store.cfg.ClusterByName("target"); ok {
		t.Fatal("target configured")
	}
	if f.store.saves != 0 {
		t.Fatalf("config saves = %d", f.store.saves)
	}
	for _, path := range []string{filepath.Join(f.root, "target"), filepath.Join(materialisationConfigRoot, "target"), filepath.Join(f.ports, "target")} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("path changed: %s %v", path, err)
		}
	}
}

func (f *fenceOnlyFixture) assertRefused(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("fence-only drop accepted")
	}
	recs, loadErr := backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.Activation["target"]; !ok {
		t.Fatal("refusal lost the activation fence")
	}
	if _, ok := f.store.cfg.ClusterByName("target"); ok {
		t.Fatal("refusal configured the target")
	}
	if f.store.saves != 0 {
		t.Fatalf("refusal saved configuration %d times", f.store.saves)
	}
}

func TestDropAbandonedForkFenceSuccess(t *testing.T) {
	f := newFenceOnlyFixture(t)
	f.writeRecords(t, f.fence)
	mustNoErr(t, os.WriteFile(filepath.Join(f.root, "app-sentinel"), []byte("unchanged"), 0o600))
	res, err := f.drop(t)
	mustNoErr(t, err)
	data, ok := res.Data.(map[string]any)
	if !ok || data["dropped"] != "target" || data["metadata_only"] != true {
		t.Fatalf("result = %#v", res.Data)
	}
	f.assertGone(t)
	if _, err := os.Lstat(filepath.Join(f.root, "app-sentinel")); err != nil {
		t.Fatalf("unrelated path changed: %v", err)
	}
	if f.release != fenceOnlyJobID {
		t.Fatalf("retained job released = %q", f.release)
	}
}

func TestDropAbandonedForkFencesIndependently(t *testing.T) {
	f := newFenceOnlyFixture(t)
	second := f.fence
	second.Database, second.OperationID = "target2", fenceOnlyJob2ID
	second.Registration = "target2:5433:"
	second.DataDir, second.ConfDir = filepath.Join(f.root, "target2"), filepath.Join(materialisationConfigRoot, "target2")
	secondJob := engine.JobRecord{ID: fenceOnlyJob2ID, Job: "db-fork", DB: "target2", State: engine.JobFailed}
	f.state.Jobs = append(f.state.Jobs, secondJob)
	f.writeRecords(t, f.fence, second)
	_, err := f.dropName(t, "target")
	mustNoErr(t, err)
	recs, loadErr := backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.Activation["target"]; ok {
		t.Fatal("first fence remains")
	}
	if _, ok := recs.Activation["target2"]; !ok {
		t.Fatal("second fence removed by the first drop")
	}
	_, err = f.dropName(t, "target2")
	mustNoErr(t, err)
	recs, loadErr = backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.Activation["target2"]; ok {
		t.Fatal("second fence remains")
	}
	if f.store.saves != 0 {
		t.Fatalf("fence-only drops saved configuration %d times", f.store.saves)
	}
}

func TestDropAbandonedForkFenceRetainedJobHistory(t *testing.T) {
	f := newFenceOnlyFixture(t)
	f.state = engine.State{RetainedJobs: map[string]engine.JobRecord{fenceOnlyJobID: f.job}}
	f.writeRecords(t, f.fence)
	_, err := f.drop(t)
	mustNoErr(t, err)
	f.assertGone(t)
}

func TestDropAbandonedForkFencePreservesUnrelatedRecords(t *testing.T) {
	f := newFenceOnlyFixture(t)
	other := f.fence
	other.Database, other.OperationID = "other", "other-job"
	other.DataDir, other.ConfDir = filepath.Join(f.root, "other"), filepath.Join(materialisationConfigRoot, "other")
	f.writeRecords(t, f.fence, other)
	mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
		s.DroppedStanzas = append(s.DroppedStanzas, backup.DroppedStanza{Database: "old", Stanza: "old-stanza"})
		if s.Timelines == nil {
			s.Timelines = map[string]backup.TimelineSwitch{}
		}
		s.Timelines["old"] = backup.TimelineSwitch{Stanza: "old-stanza", Timeline: "2"}
		return nil
	}))
	_, err := f.drop(t)
	mustNoErr(t, err)
	recs, loadErr := backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.ActivationFenceFor("other"); !ok {
		t.Fatal("unrelated activation fence removed")
	}
	if len(recs.DroppedStanzas) != 1 || recs.DroppedStanzas[0].Database != "old" {
		t.Fatalf("dropped stanza history changed: %+v", recs.DroppedStanzas)
	}
	if recs.Timelines["old"].Timeline != "2" {
		t.Fatalf("timeline history changed: %+v", recs.Timelines)
	}
}

func TestDropAbandonedForkFenceRecordWriteFailureAndRetry(t *testing.T) {
	f := newFenceOnlyFixture(t)
	f.writeRecords(t, f.fence)
	oldUpdate := forkFenceUpdateRecords
	forkFenceUpdateRecords = func(func(*backup.Records) error) error { return errors.New("records write failed") }
	_, err := f.drop(t)
	if err == nil {
		t.Fatal("records write failure accepted")
	}
	recs, loadErr := backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.ActivationFenceFor("target"); !ok {
		t.Fatal("write failure lost the fence")
	}
	forkFenceUpdateRecords = oldUpdate
	_, err = f.drop(t)
	mustNoErr(t, err)
	f.assertGone(t)
}

func TestDropAbandonedForkFenceChangedSnapshotIsRefused(t *testing.T) {
	f := newFenceOnlyFixture(t)
	f.writeRecords(t, f.fence)
	oldUpdate := forkFenceUpdateRecords
	forkFenceUpdateRecords = func(fn func(*backup.Records) error) error {
		mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
			changed := s.Activation["target"]
			changed.CreatedAt = "2026-11-02T00:00:00Z"
			s.Activation["target"] = changed
			return nil
		}))
		return oldUpdate(fn)
	}
	_, err := f.drop(t)
	if err == nil {
		t.Fatal("changed snapshot accepted")
	}
	recs, loadErr := backup.LoadRecords()
	mustNoErr(t, loadErr)
	if _, ok := recs.ActivationFenceFor("target"); !ok {
		t.Fatal("changed snapshot was deleted")
	}
}

func TestDropAbandonedForkFenceRefusals(t *testing.T) {
	modes := []struct {
		name   string
		mutate func(*fenceOnlyFixture)
		direct bool
	}{
		{name: "wrong-confirm", mutate: func(f *fenceOnlyFixture) { f.store.cfg = f.cfg }},
		{name: "empty-operation", mutate: func(f *fenceOnlyFixture) { f.fence.OperationID = "" }},
		{name: "foreign-instance", mutate: func(f *fenceOnlyFixture) { f.fence.Instance = "foreign" }},
		{name: "wrong-kind", mutate: func(f *fenceOnlyFixture) { f.fence.Kind = "restore-promote" }},
		{name: "adopting", mutate: func(f *fenceOnlyFixture) { f.fence.Phase = fencePhaseAdopting }},
		{name: "failed-marker", mutate: func(f *fenceOnlyFixture) { f.fence.Phase = "failed" }},
		{name: "no-old-stanza", mutate: func(f *fenceOnlyFixture) { f.fence.OldStanza = "" }},
		{name: "no-new-stanza", mutate: func(f *fenceOnlyFixture) { f.fence.NewStanza = "" }},
		{name: "new-not-target", mutate: func(f *fenceOnlyFixture) { f.fence.NewStanza = "other" }},
		{name: "same-stanza", mutate: func(f *fenceOnlyFixture) { f.fence.OldStanza = f.fence.NewStanza }},
		{name: "temporary-port", mutate: func(f *fenceOnlyFixture) { f.fence.Port = config.TemporaryPortMin }},
		{name: "ordinary-port-outside", mutate: func(f *fenceOnlyFixture) { f.fence.Port = 6000 }},
		{name: "follower-ownership", mutate: func(f *fenceOnlyFixture) { f.fence.Ownership = "generation" }},
		{name: "wrong-registration", mutate: func(f *fenceOnlyFixture) { f.fence.Registration = "target:5434:" }},
		{name: "wrong-data-dir", mutate: func(f *fenceOnlyFixture) { f.fence.DataDir = filepath.Join(f.root, "other") }},
		{name: "wrong-conf-dir", mutate: func(f *fenceOnlyFixture) { f.fence.ConfDir = filepath.Join(materialisationConfigRoot, "other") }},
		{name: "aside", mutate: func(f *fenceOnlyFixture) { f.fence.Aside = "x" }},
		{name: "foreign-follow-record", mutate: func(f *fenceOnlyFixture) {
			p, err := followOwnershipPath("target")
			mustNoErr(t, err)
			mustNoErr(t, os.WriteFile(p, []byte("{}"), 0o600))
			f.t.Cleanup(func() { _ = os.Remove(p) })
		}},
		{name: "job-missing", mutate: func(f *fenceOnlyFixture) { f.state = engine.State{} }},
		{name: "job-queued", mutate: func(f *fenceOnlyFixture) { f.job.State = engine.JobQueued; f.state.Jobs[0] = f.job }},
		{name: "job-running", mutate: func(f *fenceOnlyFixture) { f.job.State = engine.JobRunning; f.state.Jobs[0] = f.job }},
		{name: "job-succeeded", mutate: func(f *fenceOnlyFixture) { f.job.State = engine.JobSucceeded; f.state.Jobs[0] = f.job }},
		{name: "job-foreign-kind", mutate: func(f *fenceOnlyFixture) { f.job.Job = "db-follow"; f.state.Jobs[0] = f.job }},
		{name: "job-foreign-db", mutate: func(f *fenceOnlyFixture) { f.job.DB = "app"; f.state.Jobs[0] = f.job }},
		{name: "job-duplicate", mutate: func(f *fenceOnlyFixture) { f.state.Jobs = append(f.state.Jobs, f.job) }},
		{name: "job-retained-conflict", mutate: func(f *fenceOnlyFixture) {
			f.state.RetainedJobs = map[string]engine.JobRecord{fenceOnlyJobID: {ID: fenceOnlyJobID, Job: "db-fork", DB: "app", State: engine.JobFailed}}
		}},
		{name: "job-active-other", mutate: func(f *fenceOnlyFixture) {
			f.state.PutJob(engine.JobRecord{ID: "other", Job: "db-fork", DB: "target", State: engine.JobQueued})
		}},
		{name: "active-lease", mutate: func(f *fenceOnlyFixture) {
			env := &jobEnv{ID: "other", Job: "db-fork", OperationID: "operation", installID: "install", Params: json.RawMessage(`{"name":"target"}`)}
			lease := newLockedJobLease(env, []jobLockRequest{{name: "target", mode: engine.LockExclusive}}, nil, func() {})
			f.t.Cleanup(lease.finish)
		}},
		{name: "data-present", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, os.Mkdir(filepath.Join(f.root, "target"), 0o700))
		}},
		{name: "data-symlink", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, os.Symlink(filepath.Join(f.root, "dangling"), filepath.Join(f.root, "target")))
		}},
		{name: "config-present", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, os.Mkdir(filepath.Join(materialisationConfigRoot, "target"), 0o755))
		}},
		{name: "reservation-present", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, os.WriteFile(filepath.Join(f.ports, "target"), []byte("5480\n"), 0o600))
		}},
		{name: "restore-record", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
				s.Restores["target"] = backup.Restore{ID: "restore", Database: "target"}
				return nil
			}))
		}},
		{name: "undo-record", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
				s.Undoing["target"] = backup.Undo{}
				return nil
			}))
		}},
		{name: "drill-record", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
				if s.Drilling == nil {
					s.Drilling = map[string]string{}
				}
				s.Drilling["target"] = "target__drill"
				return nil
			}))
		}},
		{name: "awaiting-record", mutate: func(f *fenceOnlyFixture) {
			mustNoErr(t, backup.UpdateRecords(func(s *backup.Records) error {
				if s.AwaitingRecovery == nil {
					s.AwaitingRecovery = map[string]backup.Awaiting{}
				}
				s.AwaitingRecovery["target"] = backup.Awaiting{Since: "2026-11-01T00:00:00Z"}
				return nil
			}))
		}},
		{name: "config-present-entry", mutate: func(f *fenceOnlyFixture) {
			f.store.cfg = f.cfg
			f.store.cfg.Clusters = append(append([]config.ClusterSpec(nil), f.store.cfg.Clusters...),
				config.ClusterSpec{Name: "target", Stanza: "target-abcdef", Port: 5433, Size: "small"})
		}, direct: true},
		{name: "materialisation-marker-present", mutate: func(f *fenceOnlyFixture) {
			o := materialisationOwnership{
				Operation: fenceOnlyJobID, Job: fenceOnlyJobID, Instance: "install",
				Kind: materialisationFork, Source: "source", Target: "target", Stanza: "source-abcdef",
				IntendedPort: 5433, DataPath: filepath.Join(f.root, "target"),
				RegisteredPath: filepath.Join(materialisationConfigRoot, "target"), Phase: materialisationPrepared,
			}
			_, err := createMaterialisation(o, f.root)
			mustNoErr(t, err)
		}, direct: true},
	}
	for _, tt := range modes {
		t.Run(tt.name, func(t *testing.T) {
			runFenceOnlyRefusal(t, tt.name, tt.mutate, tt.direct)
		})
	}
}

func runFenceOnlyRefusal(t *testing.T, mode string, mutate func(*fenceOnlyFixture), direct bool) {
	t.Helper()
	f := newFenceOnlyFixture(t)
	f.writeRecords(t, f.fence)
	if mutate != nil {
		mutate(f)
	}
	// Rewrite the fence after mutation unless the mode builds another state.
	if mode != "config-present-entry" && mode != "materialisation-marker-present" {
		f.writeRecords(t, f.fence)
	}
	if mode == "wrong-confirm" {
		env := &Env{Cluster: f.store.cfg, Req: rpc.Request{Params: json.RawMessage(`{"name":"target"}`), Confirm: "other"}}
		_, err := handleDBDrop(context.Background(), env)
		f.assertRefused(t, err)
		return
	}
	if !direct {
		_, err := f.drop(t)
		f.assertRefused(t, err)
		return
	}
	_, handled, err := dropAbandonedForkFence(context.Background(), &Env{Cluster: f.store.cfg,
		Req: rpc.Request{Params: json.RawMessage(`{"name":"target"}`), Confirm: "target"}}, f.store, f.store.cfg, "target")
	if err != nil {
		f.assertRefused(t, err)
		return
	}
	if handled {
		t.Fatal("presence mode was handled by the fence-only branch")
	}
}
