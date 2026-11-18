package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/state"
)

type forkCheckingStore struct {
	state.Store
	save func([]byte) error
}

func (s forkCheckingStore) Save(raw []byte) error { return s.save(raw) }

func TestForkPreparationOrdering(t *testing.T) {
	for _, mode := range []string{"job-failure", "save-failure", "marker-exists", "dry-run", "fresh-source"} {
		t.Run(mode, func(t *testing.T) {
			runForkPreparationMode(t, mode)
		})
	}
}

func checkPreparedFork(t *testing.T, root string, n *followerNode, wantOperation, wantStanza string) {
	t.Helper()

	o, found, err := readMaterialisation("newdb", root)
	rejectf(t, err != nil || !found || o.Job != "job" || o.Operation != wantOperation || o.Instance != n.instance ||
		o.Source != "app" || o.Target != "newdb" || o.Stanza != wantStanza ||
		o.IntendedPort != 5434 || o.TemporaryPort != 0 || o.Phase != materialisationPrepared ||
		o.DataPath != filepath.Join(root, "newdb") || o.RegisteredPath != filepath.Join(materialisationConfigRoot, "newdb") ||
		ownsMaterialisation(context.Background(), o), "bad prepared marker: %+v found=%v err=%v", o, found, err)
}

func runForkPreparationMode(t *testing.T, mode string) {
	t.Helper()
	_, root := materialisationFixture(t)
	cfg := followerLifecycleCfg(t)
	cfg.Server.DataRoot = root
	n := newFollowerNode(t, cfg)
	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, true)

	oldExists, oldSpace, oldAcquire, oldJob := createClusterExists, forkSpace, forkAcquire, forkStartJob

	t.Cleanup(func() {
		createClusterExists, forkSpace, forkAcquire, forkStartJob = oldExists, oldSpace, oldAcquire, oldJob
	})

	createClusterExists = func(context.Context, string) bool { return false }
	forkSpace = func(context.Context, config.Cluster, string) ([]backup.SpaceNeed, error) { return nil, nil }
	locked, saves, jobs := false, 0, 0
	forkAcquire = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		locked = true
		return func() { locked = false }, nil
	}
	env := &Env{Cluster: cfg}
	env.Req = request("db.fork", map[string]any{"source": "app", "name": "newdb", "size": "small"})
	env.Req.RequestID = "job"
	env.Req.OperationID = "operation"
	env.Req.Secrets = copyRoleSecrets("newdb")
	env.Req.DryRun = mode == "dry-run"

	wantStanza, wantOperation := cfg.Clusters[0].Stanza, "operation"
	if mode == "fresh-source" {
		fresh := cfg
		fresh.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters...)
		wantStanza = "app-123abc"
		fresh.Clusters[0].Stanza = wantStanza
		n.save(fresh)

		env.Req.OperationID, wantOperation = "", "job"
	}

	check := func() { checkPreparedFork(t, root, n, wantOperation, wantStanza) }
	followerState = func() state.Store {
		return forkCheckingStore{Store: n.store, save: func(raw []byte) error {
			saves++

			reject(t, !locked, "save outside lock")
			check()

			if mode == "save-failure" {
				return errors.New("save failed")
			}

			return n.store.Save(raw)
		}}
	}
	forkStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		jobs++

		reject(t, locked, "job launched under preparation lock")
		check()

		return "", errors.New("start failed")
	}

	if mode == "marker-exists" {
		if err := os.WriteFile(filepath.Join(materialisationStateDir, "materialise-newdb.json"), []byte("corrupt"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, err := handleDBFork(context.Background(), env)
	assertForkPreparationOutcome(t, mode, n, root, saves, jobs, err, check)
}

func assertForkPreparationOutcome(t *testing.T, mode string, n *followerNode, root string, saves, jobs int, err error, check func()) {
	t.Helper()

	if mode == "dry-run" {
		rejectf(t, err != nil || saves != 0 || jobs != 0, "dry run: %v saves=%d jobs=%d", err, saves, jobs)

		_, found, readErr := readMaterialisation("newdb", root)
		rejectf(t, readErr != nil || found, "dry run marker: found=%v err=%v", found, readErr)

		return
	}

	reject(t, err == nil, "expected failure")

	if mode == "marker-exists" {
		rejectf(t, saves != 0 || jobs != 0, "existing marker did not block")
		return
	}

	check()

	for _, path := range []string{filepath.Join(root, "newdb"), filepath.Join(materialisationConfigRoot, "newdb")} {
		_, statErr := os.Stat(path)
		rejectf(t, !errors.Is(statErr, os.ErrNotExist), "prepared intent has an unexpected artifact at %s: %v", path, statErr)
	}

	rejectf(t, saves != 1 || (mode != "save-failure" && jobs != 1) || (mode == "save-failure" && jobs != 0), "saves=%d jobs=%d", saves, jobs)

	_, exists := n.config().ClusterByName("newdb")
	rejectf(t, exists != (mode != "save-failure"), "unexpected persisted target: %v", exists)
}

func TestForkPreparationWithoutStanza(t *testing.T) {
	o, root := materialisationFixture(t)

	o.Stanza = ""
	if _, err := createMaterialisation(o, root); err != nil {
		t.Fatal(err)
	}

	got, found, err := readMaterialisation(o.Target, root)
	rejectf(t, err != nil || !found || got != o, "empty fork stanza: %+v %v %v", got, found, err)

	o.Operation = ""
	if err := validateMaterialisation(o, root); err == nil {
		t.Fatal("empty operation accepted")
	}
}

func TestForkPreparationRejectsFreshUnsupportedSpareCPU(t *testing.T) {
	_, root := materialisationFixture(t)
	cfg := followerLifecycleCfg(t)
	cfg.Server.DataRoot = root
	n := newFollowerNode(t, cfg)
	fixedResizeHost(t, hostinfo.Info{VCPU: 8, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}},
		hostinfo.Memory{CommitLimitMiB: 16384, AvailableMiB: 8192}, true)

	oldExists, oldSpace, oldAcquire, oldJob, oldCPU := createClusterExists, forkSpace, forkAcquire, forkStartJob, checkSpareCPU

	t.Cleanup(func() {
		createClusterExists, forkSpace, forkAcquire, forkStartJob, checkSpareCPU = oldExists, oldSpace, oldAcquire, oldJob, oldCPU
	})

	createClusterExists = func(context.Context, string) bool { return false }
	forkSpace = func(context.Context, config.Cluster, string) ([]backup.SpaceNeed, error) { return nil, nil }
	saves := 0
	followerState = func() state.Store {
		return forkCheckingStore{Store: n.store, save: func(raw []byte) error {
			saves++
			return n.store.Save(raw)
		}}
	}

	var before []byte

	forkAcquire = func(context.Context, engine.LockMode, string, string, time.Duration) (engine.Release, error) {
		fresh := cfg
		fresh.Clusters = append([]config.ClusterSpec(nil), cfg.Clusters...)
		fresh.Clusters[0].CPUMode = "spare"
		fresh.Clusters[0].CPUQuota = "175%"
		fresh.Clusters[0].CPUWeight = 250
		n.save(fresh)

		var err error

		_, before, err = n.store.Load()
		reject(t, err != nil, err)

		return func() {}, nil
	}
	refused := errors.New("spare unavailable")

	var modes []string

	checkSpareCPU = func(_ context.Context, name, mode string) error {
		rejectf(t, name != "newdb", "unexpected CPU target %q", name)

		modes = append(modes, mode)
		if mode == "spare" {
			return refused
		}

		return nil
	}
	jobs := 0
	forkStartJob = func(context.Context, *Env, string, any, map[string]string) (string, error) {
		jobs++
		return "", errors.New("unexpected job")
	}
	env := &Env{Cluster: cfg}
	env.Req = request("db.fork", map[string]any{"source": "app", "name": "newdb", "size": "small"})
	env.Req.RequestID = "job"
	env.Req.Secrets = copyRoleSecrets("newdb")
	_, err := handleDBFork(context.Background(), env)
	rejectf(t, !errors.Is(err, refused) || len(modes) != 2 || modes[0] != "" || modes[1] != "spare", "err=%v modes=%v", err, modes)
	afterCfg, after, err := n.store.Load()
	rejectf(t, err != nil || string(after) != string(before) || saves != 0 || jobs != 0, "config or job changed: err=%v saves=%d jobs=%d", err, saves, jobs)

	source := afterCfg.Clusters[0]
	rejectf(t, source.CPUMode != "spare" || source.CPUQuota != "175%" || source.CPUWeight != 250, "source CPU fields changed: %+v", source)

	if _, found, err := readMaterialisation("newdb", root); err != nil || found {
		t.Fatalf("unexpected marker: found=%v err=%v", found, err)
	}
}
