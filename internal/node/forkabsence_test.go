package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

type forkAbsenceStore struct {
	cfg      config.Cluster
	saves    int
	attempts int
	saveErr  error
	onSave   func() error
}

func (s *forkAbsenceStore) Load() (config.Cluster, []byte, error) { return s.cfg, nil, nil }

func (s *forkAbsenceStore) Save(raw []byte) error {
	s.attempts++
	if s.onSave != nil {
		if err := s.onSave(); err != nil {
			return err
		}
	}
	if s.saveErr != nil {
		return s.saveErr
	}
	cfg, err := config.ParseCluster(raw)
	if err != nil {
		return err
	}
	s.cfg, s.saves = cfg, s.saves+1
	return nil
}

func (*forkAbsenceStore) Revision() (string, error) { return "", nil }

// forkAbsenceFixture is one prepared fork record, its unrelated neighbour and
// the evidence the metadata cleanup inspects.
type forkAbsenceFixture struct {
	mode         string
	t            *testing.T
	o            materialisationOwnership
	root, ports  string
	store        *forkAbsenceStore
	job          engine.JobRecord
	appBefore    config.ClusterSpec
	appMarker    string
	lease        *lockedJobLease
	probeLock    bool
	lockProbes   int
	removeCalls  int
	failRemove   bool
	recordChange bool
}

func newForkAbsenceFixture(t *testing.T, mode string) *forkAbsenceFixture {
	t.Helper()
	o, root := materialisationFixture(t)
	mustNoErr(t, os.Mkdir(root, 0o700))
	ports := filepath.Join(filepath.Dir(root), "ports")
	mustNoErr(t, os.Mkdir(ports, 0o700))
	oldRecords := backup.RecordsPath
	backup.RecordsPath = filepath.Join(materialisationStateDir, "records.json")
	oldState, oldReservation, oldStore, oldRemove := forkAbsenceState, forkAbsenceReservation, dbDropStore, materialisationRemoveRecord
	oldBeforePin := forkAbsenceBeforePin
	t.Cleanup(func() {
		backup.RecordsPath = oldRecords
		forkAbsenceState, forkAbsenceReservation, dbDropStore, materialisationRemoveRecord = oldState, oldReservation, oldStore, oldRemove
		forkAbsenceBeforePin = oldBeforePin
	})
	f := &forkAbsenceFixture{mode: mode, t: t, o: o, root: root, ports: ports}
	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{
		{Name: "app", Stanza: "app-stanza", Port: 5432, Size: "small"},
		{Name: "target", Stanza: "target-stanza", Port: o.IntendedPort, Size: "small"},
	}
	f.store = &forkAbsenceStore{cfg: cfg}
	dbDropStore = func() state.Store { return f.store }
	f.probeLock = mode == "success" || mode == "changed-spec" || mode == "unrelated" ||
		mode == "save-failure" || mode == "retire-failure"
	f.store.onSave = func() error {
		if !f.probeLock {
			return nil
		}
		return f.checkReservationHeld(context.Background())
	}
	f.job = engine.JobRecord{ID: o.Job, Job: "db-fork", DB: "target", State: engine.JobFailed}
	forkAbsenceState = f.state
	forkAbsenceReservation = f.reservation
	forkAbsenceBeforePin = f.beforePin
	materialisationRemoveRecord = f.removeRecord
	f.applyRecordMode()
	writeForkAbsenceRecord(t, f.o)
	f.applyConfigMode()
	f.applyFilesystemMode(t)
	f.applyJobMode(t)
	f.applyLeaseMode(t)
	// Compare the unrelated entry in the same canonical form the store
	// produces after a save.
	raw, err := f.store.cfg.Encode()
	mustNoErr(t, err)
	f.store.cfg, err = config.ParseCluster(raw)
	mustNoErr(t, err)
	f.appBefore, _ = f.store.cfg.ClusterByName("app")
	if mode == "retire-failure" {
		f.failRemove = true
	}
	if mode == "save-failure" {
		f.store.saveErr = errors.New("config save failed")
	}
	return f
}

func (f *forkAbsenceFixture) applyRecordMode() {
	switch f.mode {
	case "foreign-instance":
		f.o.Instance = "foreign"
	case "phase-failed", "failed-fork":
		f.o.Phase, f.o.Reason = materialisationFailed, "recovery_requirements_unmet"
	case "artifact-stage":
		f.o.ArtifactStage = materialisationDataCreated
		f.o.DataIdentity = materialisationFileID{Inode: 1, UID: materialisationRecordUID}
	case "temp-port":
		f.o.TemporaryPort = config.TemporaryPortMin
	case "reservation-identity":
		f.o.ReservationIdentity = backup.ReservationIdentity{Inode: 1, Digest: strings.Repeat("0", 64)}
	}
}

// beforePin substitutes the record between the initial eligibility read and
// the pinned reopen. The replacement must be refused without touching the
// configuration, the marker or any path it names.
func (f *forkAbsenceFixture) beforePin() error {
	switch f.mode {
	case "replace-record-job":
		o := f.o
		o.Job = "other-job"
		writeForkAbsenceRecord(f.t, o)
	case "replace-record-artifacts":
		o, err := f.boundRecord()
		if err != nil {
			return err
		}
		writeForkAbsenceRecord(f.t, o)
	}
	return nil
}

func (f *forkAbsenceFixture) boundRecord() (materialisationOwnership, error) {
	dataTarget, configTarget := filepath.Join(f.root, f.o.Target), filepath.Join(materialisationConfigRoot, f.o.Target)
	for _, dir := range []struct {
		path string
		mode os.FileMode
	}{{dataTarget, 0o700}, {configTarget, 0o755}} {
		if err := os.Mkdir(dir.path, dir.mode); err != nil {
			return materialisationOwnership{}, err
		}
		if err := os.Chmod(dir.path, dir.mode); err != nil {
			return materialisationOwnership{}, err
		}
		if err := os.WriteFile(filepath.Join(dir.path, "keep"), []byte("unchanged"), 0o600); err != nil {
			return materialisationOwnership{}, err
		}
	}
	dataInfo, err := os.Lstat(dataTarget)
	if err != nil {
		return materialisationOwnership{}, err
	}
	dataID, err := materialisationID(dataInfo)
	if err != nil {
		return materialisationOwnership{}, err
	}
	configInfo, err := os.Lstat(configTarget)
	if err != nil {
		return materialisationOwnership{}, err
	}
	configID, err := materialisationID(configInfo)
	if err != nil {
		return materialisationOwnership{}, err
	}
	o := f.o
	o.ArtifactStage, o.TemporaryPort = materialisationRegistered, config.TemporaryPortMin
	o.DataIdentity, o.ConfigIdentity = dataID, configID
	return o, nil
}

func (f *forkAbsenceFixture) applyConfigMode() {
	switch f.mode {
	case "protected":
		for i := range f.store.cfg.Clusters {
			if f.store.cfg.Clusters[i].Name == "target" {
				f.store.cfg.Clusters[i].Protected = true
			}
		}
	case "changed-spec":
		for i := range f.store.cfg.Clusters {
			if f.store.cfg.Clusters[i].Name == "target" {
				f.store.cfg.Clusters[i].Port++
			}
		}
	}
}

func (f *forkAbsenceFixture) applyFilesystemMode(t *testing.T) {
	t.Helper()
	dataTarget, configTarget := filepath.Join(f.root, f.o.Target), filepath.Join(materialisationConfigRoot, f.o.Target)
	switch f.mode {
	case "data-present":
		mustNoErr(t, os.Mkdir(dataTarget, 0o700))
		mustNoErr(t, os.WriteFile(filepath.Join(dataTarget, "keep"), []byte("unchanged"), 0o600))
	case "data-file":
		mustNoErr(t, os.WriteFile(dataTarget, []byte("unchanged"), 0o600))
	case "data-symlink":
		mustNoErr(t, os.Symlink(filepath.Join(f.root, "dangling"), dataTarget))
	case "config-present":
		mustNoErr(t, os.Mkdir(configTarget, 0o755))
		mustNoErr(t, os.WriteFile(filepath.Join(configTarget, "keep"), []byte("unchanged"), 0o600))
	case "config-file":
		mustNoErr(t, os.WriteFile(configTarget, []byte("unchanged"), 0o600))
	case "reservation-present":
		mustNoErr(t, os.WriteFile(filepath.Join(f.ports, f.o.Target), []byte("6500\n"), 0o600))
	case "unrelated":
		mustNoErr(t, os.Mkdir(filepath.Join(f.root, "app"), 0o700))
		mustNoErr(t, os.WriteFile(filepath.Join(f.root, "app", "keep"), []byte("app-unchanged"), 0o600))
		mustNoErr(t, os.WriteFile(filepath.Join(f.root, "scratch"), []byte("scratch-unchanged"), 0o600))
		other := f.o
		other.Target, other.Job, other.Operation, other.Source = "app", "app-job", "app-operation", "source"
		other.IntendedPort = 5432
		other.DataPath, other.RegisteredPath = filepath.Join(f.root, "app"), filepath.Join(materialisationConfigRoot, "app")
		writeForkAbsenceRecord(t, other)
		f.appMarker, _ = materialisationPath("app")
	}
}

func (f *forkAbsenceFixture) applyJobMode(t *testing.T) {
	t.Helper()
	switch f.mode {
	case "job-queued":
		f.job.State = engine.JobQueued
	case "job-running":
		f.job.State = engine.JobRunning
	case "job-foreign-kind":
		f.job.Job = "db-follow"
	case "job-foreign-db":
		f.job.DB = "app"
	}
}

func (f *forkAbsenceFixture) applyLeaseMode(t *testing.T) {
	t.Helper()
	if f.mode != "active-lease" {
		return
	}
	env := &jobEnv{ID: "other", Job: "db-fork", OperationID: "operation", installID: "install", Params: json.RawMessage(`{"name":"target"}`)}
	f.lease = newLockedJobLease(env, []jobLockRequest{{name: "target", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(f.lease.finish)
}

func writeForkAbsenceRecord(t *testing.T, o materialisationOwnership) {
	t.Helper()
	raw, err := json.Marshal(o)
	mustNoErr(t, err)
	p, err := materialisationPath(o.Target)
	mustNoErr(t, err)
	mustNoErr(t, os.WriteFile(p, append(raw, '\n'), 0o600))
}

func (f *forkAbsenceFixture) state() (engine.State, error) {
	switch f.mode {
	case "state-error":
		return engine.State{}, errors.New("unreadable state")
	case "job-missing":
		return engine.State{}, nil
	case "job-duplicate":
		return engine.State{Jobs: []engine.JobRecord{f.job, f.job}}, nil
	case "job-retained-conflict":
		st := engine.State{}
		st.PutJob(f.job)
		st.RetainedJobs = map[string]engine.JobRecord{
			f.job.ID: {ID: f.job.ID, Job: "db-fork", DB: "app", State: engine.JobFailed},
		}
		return st, nil
	case "job-active-other":
		st := engine.State{}
		st.PutJob(f.job)
		st.PutJob(engine.JobRecord{ID: "other", Job: "db-fork", DB: "target", State: engine.JobQueued})
		return st, nil
	}
	return engine.State{Jobs: []engine.JobRecord{f.job}}, nil
}

func (f *forkAbsenceFixture) reservation(ctx context.Context, owner string) (func(), error) {
	unlock, err := backup.LockTemporaryReservationAbsenceWithHost(ctx, f.ports, materialisationRecordUID, owner)
	if err != nil {
		return nil, err
	}
	if f.probeLock {
		if err := f.checkReservationHeld(ctx); err != nil {
			unlock()
			return nil, err
		}
	}
	switch f.mode {
	case "record-changed":
		f.recordChange = true
		o := f.o
		o.Operation = "replacement"
		raw, _ := json.Marshal(o)
		p, _ := materialisationPath(o.Target)
		if err := os.WriteFile(p, append(raw, '\n'), 0o600); err != nil {
			unlock()
			return nil, err
		}
	case "parent-changed":
		if err := os.Rename(f.root, f.root+"-old"); err != nil {
			unlock()
			return nil, err
		}
		if err := os.Mkdir(f.root, 0o700); err != nil {
			unlock()
			return nil, err
		}
	}
	return unlock, nil
}

func (f *forkAbsenceFixture) checkReservationHeld(ctx context.Context) error {
	f.lockProbes++
	probe, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := backup.LockTemporaryReservationAbsenceWithHost(probe, f.ports, materialisationRecordUID, f.o.Target); !errors.Is(err, context.DeadlineExceeded) {
		return errors.New("allocator lock was not held")
	}
	return nil
}

func (f *forkAbsenceFixture) removeRecord(p string) error {
	f.removeCalls++
	if f.probeLock {
		if err := f.checkReservationHeld(context.Background()); err != nil {
			return err
		}
	}
	if f.failRemove && f.removeCalls == 1 {
		return errors.New("marker unlink failed")
	}
	return os.Remove(p)
}

func (f *forkAbsenceFixture) drop(t *testing.T) (Result, error) {
	t.Helper()
	env := &Env{Cluster: f.store.cfg, Req: rpc.Request{Params: json.RawMessage(`{"name":"target"}`), Confirm: "target"}}
	if f.mode == "wrong-confirm" {
		env.Req.Confirm = "other"
	}
	return handleDBDrop(context.Background(), env)
}

func (f *forkAbsenceFixture) assertRefused(t *testing.T, err error, wantCode string) {
	t.Helper()
	if err == nil {
		t.Fatal("metadata cleanup accepted")
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a CLI error: %v", err)
	}
	if wantCode != "" && cliErr.Code != wantCode {
		t.Fatalf("error code = %s, want %s: %v", cliErr.Code, wantCode, err)
	}
	if f.store.saves != 0 || f.removeCalls != 0 {
		t.Fatalf("refusal mutated state: saves=%d removes=%d", f.store.saves, f.removeCalls)
	}
	marker, _ := materialisationPath("target")
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("refusal lost marker: %v", err)
	}
	if _, ok := f.store.cfg.ClusterByName("target"); !ok {
		t.Fatal("refusal removed the configured target")
	}
	f.assertArtifacts(t)
}

func (f *forkAbsenceFixture) assertArtifacts(t *testing.T) {
	t.Helper()
	dataTarget, configTarget := filepath.Join(f.root, f.o.Target), filepath.Join(materialisationConfigRoot, f.o.Target)
	switch f.mode {
	case "data-present":
		assertUnchangedFile(t, filepath.Join(dataTarget, "keep"))
	case "data-file":
		assertUnchangedFile(t, dataTarget)
	case "config-present":
		assertUnchangedFile(t, filepath.Join(configTarget, "keep"))
	case "config-file":
		assertUnchangedFile(t, configTarget)
	case "data-symlink":
		assertPathExists(t, dataTarget)
	case "reservation-present":
		assertPathExists(t, filepath.Join(f.ports, f.o.Target))
	case "replace-record-job":
		f.assertReplacementJobRecord(t)
	case "replace-record-artifacts":
		assertUnchangedFile(t, filepath.Join(dataTarget, "keep"))
		assertUnchangedFile(t, filepath.Join(configTarget, "keep"))
	}
}

func assertUnchangedFile(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("path changed: %s %q %v", path, data, err)
	}
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("path changed: %s %v", path, err)
	}
}

func (f *forkAbsenceFixture) assertReplacementJobRecord(t *testing.T) {
	t.Helper()
	got, found, err := readMaterialisation(f.o.Target, f.root)
	if err != nil || !found || got.Job != "other-job" {
		t.Fatalf("replacement marker changed: %+v %v %v", got, found, err)
	}
}

func (f *forkAbsenceFixture) assertMetadataGone(t *testing.T) {
	t.Helper()
	if _, ok := f.store.cfg.ClusterByName("target"); ok {
		t.Fatal("target remains configured")
	}
	app, ok := f.store.cfg.ClusterByName("app")
	if !ok || !reflect.DeepEqual(app, f.appBefore) {
		t.Fatalf("unrelated configuration changed: before=%+v after=%+v", f.appBefore, app)
	}
	marker, _ := materialisationPath("target")
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker remains: %v", err)
	}
	for _, path := range []string{filepath.Join(f.root, f.o.Target), filepath.Join(materialisationConfigRoot, f.o.Target)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact path changed: %s %v", path, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(f.ports, f.o.Target)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reservation remains: %v", err)
	}
}

func (f *forkAbsenceFixture) assertUnrelated(t *testing.T) {
	t.Helper()
	if f.mode != "unrelated" {
		return
	}
	for path, want := range map[string]string{
		filepath.Join(f.root, "app", "keep"): "app-unchanged",
		filepath.Join(f.root, "scratch"):     "scratch-unchanged",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("unrelated path changed: %s %q %v", path, data, err)
		}
	}
	if _, err := os.Lstat(f.appMarker); err != nil {
		t.Fatalf("unrelated marker changed: %v", err)
	}
}

func (f *forkAbsenceFixture) assertSuccess(t *testing.T, res Result) {
	t.Helper()
	data, ok := res.Data.(map[string]any)
	if !ok || data["dropped"] != "target" || data["metadata_only"] != true {
		t.Fatalf("result = %#v", res.Data)
	}
	if f.store.saves != 1 {
		t.Fatalf("config saves = %d", f.store.saves)
	}
	if f.removeCalls != 1 {
		t.Fatalf("marker removals = %d", f.removeCalls)
	}
	if f.probeLock && f.lockProbes < 3 {
		t.Fatalf("reservation lock probes = %d, want save and retire under the lock", f.lockProbes)
	}
	f.assertMetadataGone(t)
	f.assertUnrelated(t)
}

func TestLegacyPreparedForkPredicate(t *testing.T) {
	base := materialisationOwnership{Kind: materialisationFork, Phase: materialisationPrepared}
	if !legacyPreparedFork(base) {
		t.Fatal("prepared fork not recognised")
	}
	for name, mutate := range map[string]func(*materialisationOwnership){
		"kind":      func(o *materialisationOwnership) { o.Kind = materialisationRestore },
		"phase":     func(o *materialisationOwnership) { o.Phase = materialisationFailed },
		"reason":    func(o *materialisationOwnership) { o.Reason = "recovery_requirements_unmet" },
		"stage":     func(o *materialisationOwnership) { o.ArtifactStage = materialisationDataCreated },
		"temp-port": func(o *materialisationOwnership) { o.TemporaryPort = config.TemporaryPortMin },
		"data-id":   func(o *materialisationOwnership) { o.DataIdentity = materialisationFileID{Inode: 1} },
		"config-id": func(o *materialisationOwnership) { o.ConfigIdentity = materialisationFileID{Inode: 1} },
		"reservation": func(o *materialisationOwnership) {
			o.ReservationIdentity = backup.ReservationIdentity{Inode: 1}
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := base
			mutate(&o)
			rejectf(t, legacyPreparedFork(o), "accepted %s", name)
		})
	}
}

func TestDropPreparedForkMetadataRefusals(t *testing.T) {
	modes := []struct {
		mode string
		code string
	}{
		{"wrong-confirm", output.CodeUnsafeAction},
		{"protected", output.CodeDatabaseProtected},
		{"foreign-instance", output.CodePrecondition},
		{"phase-failed", output.CodePrecondition},
		{"artifact-stage", output.CodePrecondition},
		{"temp-port", output.CodePrecondition},
		{"reservation-identity", output.CodePrecondition},
		{"state-error", output.CodePrecondition},
		{"job-missing", output.CodePrecondition},
		{"job-queued", output.CodePrecondition},
		{"job-running", output.CodePrecondition},
		{"job-foreign-kind", output.CodePrecondition},
		{"job-foreign-db", output.CodePrecondition},
		{"job-duplicate", output.CodePrecondition},
		{"job-retained-conflict", output.CodePrecondition},
		{"job-active-other", output.CodePrecondition},
		{"active-lease", output.CodePrecondition},
		{"data-present", output.CodePrecondition},
		{"data-file", output.CodePrecondition},
		{"data-symlink", output.CodePrecondition},
		{"config-present", output.CodePrecondition},
		{"config-file", output.CodePrecondition},
		{"reservation-present", output.CodePrecondition},
		{"record-changed", output.CodePrecondition},
		{"parent-changed", output.CodePrecondition},
		{"replace-record-job", output.CodePrecondition},
		{"replace-record-artifacts", output.CodePrecondition},
		{"failed-fork", output.CodePrecondition},
	}
	for _, tt := range modes {
		t.Run(tt.mode, func(t *testing.T) {
			f := newForkAbsenceFixture(t, tt.mode)
			_, err := f.drop(t)
			f.assertRefused(t, err, tt.code)
		})
	}
}

func TestDropPreparedForkMetadataSuccess(t *testing.T) {
	for _, mode := range []string{"success", "changed-spec", "unrelated"} {
		t.Run(mode, func(t *testing.T) {
			f := newForkAbsenceFixture(t, mode)
			res, err := f.drop(t)
			if err != nil {
				t.Fatalf("drop: %v", err)
			}
			f.assertSuccess(t, res)
		})
	}
}

func TestDropPreparedForkMetadataReleasesRetainedJob(t *testing.T) {
	f := newForkAbsenceFixture(t, "success")
	st := engine.State{RetainedJobs: map[string]engine.JobRecord{f.job.ID: f.job}}
	forkAbsenceState = func() (engine.State, error) { return st, nil }
	oldFollower, oldRelease := followerState, releaseMaterialisationJob
	t.Cleanup(func() { followerState, releaseMaterialisationJob = oldFollower, oldRelease })
	followerState = func() state.Store { return f.store }
	released := ""
	releaseMaterialisationJob = func(id string) { released = id }
	res, err := f.drop(t)
	mustNoErr(t, err)
	f.assertSuccess(t, res)
	if released != f.job.ID {
		t.Fatalf("retained job released = %q, want %q", released, f.job.ID)
	}
}

func TestDropPreparedForkMetadataRetainsJobWhileFenceExists(t *testing.T) {
	for _, operation := range []string{"job", "foreign-operation"} {
		t.Run(operation, func(t *testing.T) {
			f := newForkAbsenceFixture(t, "success")
			st := engine.State{RetainedJobs: map[string]engine.JobRecord{f.job.ID: f.job}}
			forkAbsenceState = func() (engine.State, error) { return st, nil }
			oldFollower, oldRelease := followerState, releaseMaterialisationJob
			t.Cleanup(func() { followerState, releaseMaterialisationJob = oldFollower, oldRelease })
			followerState = func() state.Store { return f.store }
			released := ""
			releaseMaterialisationJob = func(id string) { released = id }
			mustNoErr(t, backup.SetActivationFence(backup.ActivationFence{
				OperationID: operation, Kind: "fork", Database: f.o.Target, Instance: f.o.Instance,
				TargetStanza: "target-abcdef", OldStanza: "source-abcdef", NewStanza: "target-abcdef",
				Registration: "target:5433:", Port: 5433, DataDir: f.o.DataPath, ConfDir: f.o.RegisteredPath,
				Phase: fencePhasePrepared, CreatedAt: "2026-11-01T00:00:00Z",
			}))
			res, err := f.drop(t)
			mustNoErr(t, err)
			f.assertSuccess(t, res)
			if released != "" {
				t.Fatalf("retained job released while its fence exists: %q", released)
			}
			recs, err := backup.LoadRecords()
			mustNoErr(t, err)
			if _, ok := recs.ActivationFenceFor(f.o.Target); !ok {
				t.Fatal("marker-only drop removed the pending fence")
			}
		})
	}
}

func TestDropPreparedForkMetadataSaveFailureRetry(t *testing.T) {
	f := newForkAbsenceFixture(t, "save-failure")
	if _, err := f.drop(t); err == nil {
		t.Fatal("config save failure accepted")
	}
	if f.store.saves != 0 || f.store.attempts == 0 || f.removeCalls != 0 {
		t.Fatalf("save failure state: saves=%d attempts=%d removes=%d", f.store.saves, f.store.attempts, f.removeCalls)
	}
	if _, ok := f.store.cfg.ClusterByName("target"); !ok {
		t.Fatal("save failure lost the configured target")
	}
	marker, _ := materialisationPath("target")
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("save failure lost marker: %v", err)
	}
	f.store.saveErr = nil
	res, err := f.drop(t)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	f.assertSuccess(t, res)
	if f.store.attempts != 2 {
		t.Fatalf("save attempts = %d", f.store.attempts)
	}
}

func TestDropPreparedForkMetadataMarkerRetireFailureRetry(t *testing.T) {
	f := newForkAbsenceFixture(t, "retire-failure")
	if _, err := f.drop(t); err == nil {
		t.Fatal("marker unlink failure accepted")
	}
	if f.store.saves != 1 || f.removeCalls != 1 {
		t.Fatalf("retire failure state: saves=%d removes=%d", f.store.saves, f.removeCalls)
	}
	if _, ok := f.store.cfg.ClusterByName("target"); ok {
		t.Fatal("retire failure kept the configured target")
	}
	marker, _ := materialisationPath("target")
	if _, err := os.Lstat(marker); err != nil {
		t.Fatalf("retire failure lost marker: %v", err)
	}
	f.failRemove = false
	res, err := f.drop(t)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if data, ok := res.Data.(map[string]any); !ok || data["metadata_only"] != true {
		t.Fatalf("retry result = %#v", res.Data)
	}
	if f.store.saves != 1 || f.store.attempts != 1 {
		t.Fatalf("retry saved config again: saves=%d attempts=%d", f.store.saves, f.store.attempts)
	}
	if f.removeCalls != 2 {
		t.Fatalf("marker removals = %d", f.removeCalls)
	}
	f.assertMetadataGone(t)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker remains after retry: %v", err)
	}
}

// The metadata branch must not claim a failed fork, whose ordinary recovery
// cleanup still requires its recorded reservation.
func TestPreparedForkMetadataLeavesFailedForkAlone(t *testing.T) {
	f := newForkAbsenceFixture(t, "failed-fork")
	_, err := f.drop(t)
	f.assertRefused(t, err, output.CodePrecondition)
	if f.recordChange {
		t.Fatal("failed record was treated as a prepared fork")
	}
}
