package node

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
)

type forkCleanupStore struct {
	cfg   config.Cluster
	saves int
}

func (s *forkCleanupStore) Load() (config.Cluster, []byte, error) { return s.cfg, nil, nil }
func (s *forkCleanupStore) Save(raw []byte) error {
	cfg, err := config.ParseCluster(raw)
	if err == nil {
		s.cfg, s.saves = cfg, s.saves+1
	}

	return err
}
func (*forkCleanupStore) Revision() (string, error) { return "", nil }

func TestDropRetainedForkOwnedArtifacts(t *testing.T) {
	for _, mode := range []string{"recovery_requirements_unmet", "recovery_parameter_mismatch", "source-removed", "source-stanza-changed", "replace-data", "replace-config",
		"replace-reservation", "replace-marker", "replace-marker-during-registration", "registration", "drop-failure", "legacy-reservation"} {
		t.Run(mode, func(t *testing.T) {
			runDropRetainedForkOwnedArtifacts(t, mode)
		})
	}
}

// forkCleanupState carries the retained failure fixture, the target paths and
// the store observations that the cleanup assertions need.
type forkCleanupState struct {
	mode         string
	o            materialisationOwnership
	root         string
	ports        string
	cfg          config.Cluster
	m            *backup.Materialised
	p            backup.MaterialisationPaths
	store        *forkCleanupStore
	sourceBefore config.ClusterSpec
	hadSource    bool
	drops        int
}

func runDropRetainedForkOwnedArtifacts(t *testing.T, mode string) {
	t.Helper()
	s := newForkCleanupState(t, mode)
	s.applyMode(t)
	s.stubDrop(t)
	_, err := handleDBDrop(context.Background(), &Env{Cluster: s.cfg, Req: rpc.Request{Confirm: "target", Params: []byte(`{"name":"target"}`)}})
	success := mode == "recovery_requirements_unmet" || mode == "recovery_parameter_mismatch" || mode == "source-removed" || mode == "source-stanza-changed"
	rejectf(t, (err == nil) != success, "drop: %v", err)

	if success {
		assertForkCleanupSucceeded(t, s)
		return
	}

	assertForkCleanupFailed(t, s)
}

// newForkCleanupState leaves a registered, verification-failing fork marker
// behind so cleanup must either adopt the failure evidence or refuse.
func newForkCleanupState(t *testing.T, mode string) *forkCleanupState {
	t.Helper()

	o, root := materialisationFixture(t)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}

	ports := filepath.Join(materialisationAncestorBase, "ports")
	if err := os.Mkdir(ports, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCluster()
	cfg.Server.DataRoot = root
	cfg.Clusters = []config.ClusterSpec{{Name: "source", Stanza: o.Stanza, Port: 5432, Size: "small"}, {Name: "target", Port: o.IntendedPort, Size: "small"}}
	params, _ := json.Marshal(dbForkParams{Source: "source", Name: "target"})
	job := &jobEnv{ID: o.Job, Job: "db-fork", OperationID: o.Operation, installID: o.Instance, Cluster: cfg, Params: params}
	job.lease = newLockedJobLease(job, []jobLockRequest{{name: "target", mode: engine.LockExclusive}}, nil, func() {})
	t.Cleanup(job.lease.finish)
	stubForkCleanupLocks(t, ports)
	m, err := backup.NewForkMaterialisedWithHost(root, o.Target, o.Source, 5432, engine.Shell{}, job.Job, nil, backup.ForkMaterialisationHost{
		ReservationDir: ports, ConfigRoot: materialisationConfigRoot, UID: materialisationRecordUID,
		ClusterPorts: func() map[int]bool { return map[int]bool{} }, SocketTaken: func(int) bool { return false },
	})
	reject(t, err != nil, err)
	auth, err := newForkMaterialisationAuthority(job, o.Target)
	reject(t, err != nil, err)

	m.Ownership = auth

	ctx := context.Background()
	if err := auth.bindReservation(ctx, m); err != nil {
		t.Fatal(err)
	}

	if err := auth.advance(ctx, materialisationCopying, ""); err != nil {
		t.Fatal(err)
	}

	p := m.OwnershipPaths()
	if err := m.Ownership.CreateData(ctx, p); err != nil {
		t.Fatal(err)
	}

	if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRestore, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if err := m.Ownership.CreateConfig(ctx, p); err != nil {
		t.Fatal(err)
	}

	if err := m.Ownership.Guard(ctx, p, backup.MaterialisationRegister, func() error {
		return os.WriteFile(filepath.Join(p.ConfigDir, "postgresql.conf"), []byte("registered"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}

	reason := mode
	if mode != "recovery_parameter_mismatch" {
		reason = "recovery_requirements_unmet"
	}

	failure := attemptFailure(reason)
	if err := m.Ownership.Guard(ctx, p, backup.MaterialisationInspect, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("typed recovery failure: %v", err)
	}

	if err := auth.advance(ctx, materialisationFailed, recoveryFailureReason(failure)); err != nil {
		t.Fatal(err)
	}

	auth.Close()
	job.lease.finish()

	s := &forkCleanupState{mode: mode, o: o, root: root, ports: ports, cfg: cfg, m: m, p: p}
	s.applySourceMode(t)

	return s
}

func stubForkCleanupLocks(t *testing.T, ports string) {
	t.Helper()

	oldReservation, oldLock := restoreCleanupReservation, forkReservationLock

	t.Cleanup(func() { restoreCleanupReservation, forkReservationLock = oldReservation, oldLock })

	forkReservationLock = func(ctx context.Context, name string, port int, present bool) (func(), error) {
		return backup.LockForkReservationWithHost(ctx, ports, materialisationRecordUID, name, port, present)
	}
	restoreCleanupReservation = func(ctx context.Context, name string, port int, id backup.ReservationIdentity, effect func() error) error {
		return backup.CleanupRestoreReservationWithHost(ctx, ports, materialisationRecordUID, name, port, id, effect)
	}
}

// applySourceMode removes or renames the source configuration and records the
// canonical source entry so the cleanup assertion can prove it was untouched.
func (s *forkCleanupState) applySourceMode(t *testing.T) {
	t.Helper()

	if s.mode == "source-removed" || s.mode == "source-stanza-changed" {
		if err := os.Mkdir(filepath.Join(s.root, "source"), 0o700); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(s.root, "source", "sentinel"), []byte("unchanged"), 0o600); err != nil {
			t.Fatal(err)
		}

		if s.mode == "source-removed" {
			s.cfg.Clusters = s.cfg.Clusters[1:]
		} else {
			s.cfg.Clusters[0].Stanza = "source-changed"
		}
	}

	rawCfg, err := s.cfg.Encode()
	reject(t, err != nil, err)
	canonicalCfg, err := config.ParseCluster(rawCfg)
	reject(t, err != nil, err)

	s.sourceBefore, s.hadSource = canonicalCfg.ClusterByName("source")
}

// applyMode replaces an owned artifact the drop must refuse to touch.
func (s *forkCleanupState) applyMode(t *testing.T) {
	t.Helper()

	switch s.mode {
	case "replace-data", "replace-config", "replace-reservation":
		path := filepath.Join(s.root, s.o.Target)
		switch s.mode {
		case "replace-config":
			path = filepath.Join(materialisationConfigRoot, s.o.Target)
		case "replace-reservation":
			path = filepath.Join(s.ports, s.o.Target)
		}

		if err := os.Rename(path, path+"-old"); err != nil {
			t.Fatal(err)
		}

		if s.mode == "replace-reservation" {
			if err := os.WriteFile(path, []byte("6000\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	case "replace-marker", "legacy-reservation":
		path, _ := materialisationPath(s.o.Target)
		record, _, err := readMaterialisation(s.o.Target, s.root)
		reject(t, err != nil, err)

		if s.mode == "replace-marker" {
			record.Instance = "foreign"
		} else {
			record.ReservationIdentity = backup.ReservationIdentity{}
		}

		raw, _ := json.Marshal(record)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func (s *forkCleanupState) stubDrop(t *testing.T) {
	t.Helper()

	oldRegistration, oldDrop, oldStore := restoreCleanupRegistration, restoreCleanupDrop, dbDropStore
	oldLeft, oldJournal, oldOther, oldApply := dbDropLeftovers, dbDropForgetJournal, dbDropOtherConfigs, dbDropApplyOtherConfigs
	oldRouting, oldMetrics, oldClear := dbDropRouting, dbDropMetrics, dbDropClearDeferred

	t.Cleanup(func() {
		restoreCleanupRegistration, restoreCleanupDrop, dbDropStore = oldRegistration, oldDrop, oldStore
		dbDropLeftovers, dbDropForgetJournal, dbDropOtherConfigs, dbDropApplyOtherConfigs = oldLeft, oldJournal, oldOther, oldApply
		dbDropRouting, dbDropMetrics, dbDropClearDeferred = oldRouting, oldMetrics, oldClear
	})

	s.store = &forkCleanupStore{cfg: s.cfg}
	dbDropStore = func() state.Store { return s.store }
	dbDropLeftovers = func(*Env, string) error { return nil }
	dbDropForgetJournal = func(string, string) error { return nil }
	dbDropOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, string, string) []string {
		return nil
	}
	dbDropApplyOtherConfigs = func(context.Context, []engine.Step, config.Cluster, engine.Runtime, []string, engine.Options) []output.Warning {
		return nil
	}
	dbDropRouting = func(context.Context, *Env, config.Cluster) error { return nil }
	dbDropMetrics = func(context.Context, config.Cluster, engine.Runtime, engine.Reporter, string, string) *output.Warning {
		return nil
	}
	dbDropClearDeferred = func(...string) {}
	restoreCleanupRegistration = func(name, source, dataRoot string, sourcePort, port int) error {
		rejectf(t, name != s.o.Target || source != s.o.Source || dataRoot != s.root || sourcePort != 0 || port != s.p.Port, "unexpected registration identity: %s %s %s %d %d", name, source, dataRoot, sourcePort, port)

		if s.mode == "registration" {
			return errors.New("foreign registration")
		}

		if s.mode == "replace-marker-during-registration" {
			path, _ := materialisationPath(s.o.Target)
			if err := os.Rename(path, path+"-old"); err != nil {
				return err
			}

			return os.WriteFile(path, []byte("{}"), 0o600)
		}

		return nil
	}
	restoreCleanupDrop = func(context.Context, *Env, string) error {
		s.drops++
		if s.mode == "drop-failure" {
			return errors.New("drop failed")
		}

		return nil
	}
}

func assertForkCleanupSucceeded(t *testing.T, s *forkCleanupState) {
	t.Helper()

	if s.drops != 1 || s.store.saves != 1 {
		t.Fatalf("drop=%d saves=%d", s.drops, s.store.saves)
	}

	if _, ok := s.store.cfg.ClusterByName("target"); ok {
		t.Fatal("target remains configured")
	}

	sourceAfter, hasSource := s.store.cfg.ClusterByName("source")
	rejectf(t, hasSource != s.hadSource || hasSource && !reflect.DeepEqual(sourceAfter, s.sourceBefore), "source configuration changed: before=%+v after=%+v", s.sourceBefore, sourceAfter)

	if s.mode == "source-removed" || s.mode == "source-stanza-changed" {
		data, err := os.ReadFile(filepath.Join(s.root, "source", "sentinel"))
		if err != nil || string(data) != "unchanged" {
			t.Fatalf("source artifact changed: %q %v", data, err)
		}
	}

	if err := guardPublicMaterialisation(context.Background(), s.store.cfg, "target"); err != nil {
		t.Fatalf("marker remains: %v", err)
	}

	if err := guardMaterialisationApply(context.Background(), s.store.cfg, ""); err != nil {
		t.Fatalf("apply remains blocked: %v", err)
	}

	for _, path := range []string{filepath.Join(s.root, s.o.Target), filepath.Join(materialisationConfigRoot, s.o.Target), filepath.Join(s.ports, s.o.Target)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("artifact remains: %s", path)
		}
	}

	if _, err := createMaterialisation(s.o, s.root); err != nil {
		t.Fatalf("target name not reusable: %v", err)
	}
}

func assertForkCleanupFailed(t *testing.T, s *forkCleanupState) {
	t.Helper()

	if s.store.saves != 0 || s.mode != "drop-failure" && s.drops != 0 {
		t.Fatalf("failure mutated target: drops=%d saves=%d", s.drops, s.store.saves)
	}

	marker, _ := materialisationPath(s.o.Target)
	for _, path := range []string{marker, filepath.Join(s.root, s.o.Target), filepath.Join(materialisationConfigRoot, s.o.Target), filepath.Join(s.ports, s.o.Target)} {
		if _, err := os.Lstat(path); err != nil {
			t.Fatalf("failure lost evidence %s: %v", path, err)
		}
	}
}
