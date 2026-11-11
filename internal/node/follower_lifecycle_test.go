package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
	"github.com/mbvlabs/vaire/internal/hostinfo"
	"github.com/mbvlabs/vaire/internal/pg"
	"github.com/mbvlabs/vaire/internal/rpc"
	"github.com/mbvlabs/vaire/internal/state"
	"github.com/mbvlabs/vaire/internal/steps"
)

// The lifecycle boundaries of Phase 6's follow and unfollow: what each job
// leaves durable when it stops part way, and where the next run resumes. The
// jobs run for real against a scratch cluster.toml and records.json (with their
// locks); only the server work is stubbed, one boundary at a time.

// followerNode is a node's durable state for one test: cluster.toml and
// records.json in its own directory, the follower server effects replaced, and
// every cluster reported as running.
type followerNode struct {
	t           *testing.T
	dir         string
	store       state.FileStore
	instance    string
	registered  map[string]string // database -> registered data directory
	confDirRoot string            // stands in for /etc/postgresql/18
}

// followerLifecycleCfg is a valid cluster.toml with a source and the named
// followers of it.
func followerLifecycleCfg(t *testing.T, names ...string) config.Cluster {
	t.Helper()
	cfg := config.DefaultCluster()
	cfg.ClusterName, cfg.Domain = "prod", "example.com"
	cfg.Nodes = []config.NodeSpec{{Name: "db1", Role: config.RoleMaster, PublicAddress: "203.0.113.10", WGAddress: config.DefaultCentralWG}}
	cfg.Server.DataRoot = t.TempDir()
	cfg.Clusters = []config.ClusterSpec{{Name: "app", Node: "db1", Port: 5433, Stanza: "app-3f9a1c", Size: "large", StatementTimeout: "60s"}}
	for i, name := range names {
		cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
			Name: name, Node: "db1", Port: 5434 + i, Size: "small", StatementTimeout: "60s", Follows: "app",
			FollowGeneration: fmt.Sprintf("%032x", i+1),
		})
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("test configuration: %v", err)
	}
	return cfg
}

func newFollowerNode(t *testing.T, cfg config.Cluster) *followerNode {
	t.Helper()
	dir := t.TempDir()
	n := &followerNode{t: t, dir: dir, store: state.FileStore{Path: filepath.Join(dir, "cluster.toml")}, instance: "test-instance", registered: map[string]string{}, confDirRoot: filepath.Join(dir, "postgresql", "18")}
	oldRecords, oldState, oldServer, oldRunning := backup.RecordsPath, followerState, followerServerOps, clusterRunning
	oldInstance, oldRegistered, oldConf := followHostInstance, followRegisteredData, followConfRegistered
	oldConfDir, oldFloors := followConfDirectory, followerEffectiveFloors
	oldReload := applyReloadCluster
	applyReloadCluster = func() (config.Cluster, error) { return n.config(), nil }
	backup.RecordsPath = filepath.Join(dir, "records.json")
	followerState = func() state.Store { return n.store }
	clusterRunning = func(spec config.ClusterSpec) bool { return !spec.IsFollower() }
	followHostInstance = func() (string, error) { return n.instance, nil }
	followRegisteredData = func(name string) (string, error) { return n.registered[name], nil }
	followConfRegistered = func(name string) bool { _, ok := n.registered[name]; return ok }
	followConfDirectory = func(name string) (bool, error) {
		_, err := os.Stat(n.confDir(name))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	// A running source reports no floor above any proposed follower unless a
	// test supplies its own facts.
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) {
		out := map[string]int{}
		for _, name := range steps.FloorSettings {
			out[name] = 0
		}
		return out, nil
	}
	t.Cleanup(func() {
		backup.RecordsPath, followerState, followerServerOps, clusterRunning = oldRecords, oldState, oldServer, oldRunning
		followHostInstance, followRegisteredData, followConfRegistered = oldInstance, oldRegistered, oldConf
		followConfDirectory, followerEffectiveFloors = oldConfDir, oldFloors
		applyReloadCluster = oldReload
	})
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		t.Fatal(err)
	}
	return n
}

// config is cluster.toml as saved, not as a job's process loaded it.
func (n *followerNode) config() config.Cluster {
	n.t.Helper()
	cfg, _, err := n.store.Load()
	if err != nil {
		n.t.Fatal(err)
	}
	return cfg
}

// save writes cfg to cluster.toml: a change another command made while a job's
// process was starting.
func (n *followerNode) save(cfg config.Cluster) {
	n.t.Helper()
	raw, err := cfg.Encode()
	if err != nil {
		n.t.Fatal(err)
	}
	if err := n.store.Save(raw); err != nil {
		n.t.Fatal(err)
	}
}

// copyReady makes db's deterministic paths look like the adopted copy a
// recovery leaves: its configured data directory exists and postgresql-common
// registers that same directory.
func (n *followerNode) copyReady(cfg config.Cluster, db string) {
	n.t.Helper()
	data := cfg.Server.DataDirectory(db)
	if err := os.MkdirAll(data, 0o700); err != nil {
		n.t.Fatal(err)
	}
	if err := os.MkdirAll(n.confDir(db), 0o700); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.confDir(db), "postgresql.conf"), []byte("data_directory = '"+data+"'\n"), 0o600); err != nil {
		n.t.Fatal(err)
	}
	n.registered[db] = data
}

// partialRegistration leaves the configuration directory an interrupted
// registration made: files but no postgresql.conf, and no data directory.
func (n *followerNode) partialRegistration(db string) {
	n.t.Helper()
	if err := os.MkdirAll(n.confDir(db), 0o700); err != nil {
		n.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(n.confDir(db), "start.conf"), []byte("manual\n"), 0o600); err != nil {
		n.t.Fatal(err)
	}
}

func (n *followerNode) confDir(db string) string { return filepath.Join(n.confDirRoot, db) }

// recoveryOwner is the follow owner a db recover of db continues: its
// cluster.toml entry and its current awaiting record.
func recoveryOwner(n *followerNode, cfg config.Cluster, db string) followOwner {
	n.t.Helper()
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		n.t.Fatalf("%s has no cluster.toml entry", db)
	}
	return followOwner{Name: db, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec, Awaiting: n.records().AwaitingRecovery[db]}
}

// ordinaryOwner is the follow owner an ordinary db follow of db prepared.
func ordinaryOwner(n *followerNode, cfg config.Cluster, db string) followOwner {
	n.t.Helper()
	spec, ok := cfg.ClusterByName(db)
	if !ok {
		n.t.Fatalf("%s has no cluster.toml entry", db)
	}
	return followOwner{Name: db, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
}

func (n *followerNode) records() backup.Records {
	n.t.Helper()
	recs, err := backup.LoadRecords()
	if err != nil {
		n.t.Fatal(err)
	}
	return recs
}

// awaitRecovery records db as awaiting recovery, as server init --restore's
// placement and pruning do. A follower's cluster.toml entry has no stanza, so
// its real awaiting record has an empty stanza too.
func (n *followerNode) awaitRecovery(db string) {
	n.t.Helper()
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery[db] = backup.Awaiting{Stanza: "", Since: "2026-01-01T00:00:00Z"}
		return nil
	}); err != nil {
		n.t.Fatal(err)
	}
}

// followerStub replaces the follow and unfollow jobs' server effects: each call
// is recorded, and the stage named by failAt returns failErr instead of working.
// before runs at every stage, so a test reads what is durable at that boundary.
type followerStub struct {
	mu           sync.Mutex
	calls        []string
	failAt       string
	failErr      error
	before       func(stage string)
	recovering   string
	confOf       string
	slotSource   string
	finishSpec   config.ClusterSpec
	finishSource string
	resumed      bool
}

func (s *followerStub) install(t *testing.T) *followerStub {
	t.Helper()
	old := followerServerOps
	oldEnsureGuard, oldAdoptGuard, oldVerifyGuard := ensureCopyGuardAt, ensureAdoptedGuardAt, verifyStartedGuardAt
	oldRoutes, oldResumePooler := poolerRuntimeRoutes, poolerResumeDatabase
	poolerRuntimeRoutes = func(_ context.Context, _ string, _ int) (map[string]steps.PoolerRoute, error) {
		recs, err := backup.LoadRecords()
		if err != nil {
			return nil, err
		}
		cfg, _, err := followerState().Load()
		if err != nil {
			return nil, err
		}
		out := map[string]steps.PoolerRoute{}
		for db, f := range recs.Activation {
			if f.OperationID == "" {
				continue
			}
			spec, ok := cfg.ClusterByName(db)
			if !ok {
				continue
			}
			out[db] = steps.PoolerRoute{Name: db, Host: steps.PGSocketDir, Port: spec.Port, Database: spec.DatabaseName()}
		}
		return out, nil
	}
	poolerResumeDatabase = func(context.Context, string) {}
	ensureCopyGuardAt = func(context.Context, string, *extensionCopyPlan, copyGuardEndpoint, func() error) ([]string, error) {
		return nil, nil
	}
	ensureAdoptedGuardAt = func(context.Context, *jobEnv, config.Cluster, config.ClusterSpec, *extensionCopyPlan) error {
		return nil
	}
	verifyStartedGuardAt = func(context.Context, *jobEnv, config.Cluster, config.ClusterSpec) error {
		return nil
	}
	t.Cleanup(func() {
		followerServerOps = old
		ensureCopyGuardAt, ensureAdoptedGuardAt, verifyStartedGuardAt = oldEnsureGuard, oldAdoptGuard, oldVerifyGuard
		poolerRuntimeRoutes, poolerResumeDatabase = oldRoutes, oldResumePooler
	})
	followerServerOps = followerServer{
		build: func(context.Context, *jobEnv, config.Cluster, config.ClusterSpec, config.ClusterSpec, string, bool) error {
			return s.stage("build")
		},
		start: func(_ context.Context, _ *jobEnv, _ config.Cluster, _ config.ClusterSpec, _ config.ClusterSpec, recovering string) ([]output.Warning, followerReading, error) {
			s.mu.Lock()
			s.recovering = recovering
			s.mu.Unlock()
			return nil, followerReading{}, s.stage("start")
		},
		promote: func(context.Context, pg.Local, string) ([]string, error) { return nil, s.stage("promote") },
		rename: func(context.Context, sqlRunner, string, string, map[string]string, engine.Reporter, string) error {
			return s.stage("rename")
		},
		removeConf: func(cluster string) error {
			s.mu.Lock()
			s.confOf = cluster
			s.mu.Unlock()
			return s.stage("remove_conf")
		},
		restart: func(context.Context, *jobEnv, config.Cluster, config.ClusterSpec) error {
			return s.stage("restart")
		},
		dropSlot: func(_ context.Context, _ config.Cluster, source, _ string) *output.Warning {
			s.mu.Lock()
			s.slotSource = source
			s.mu.Unlock()
			_ = s.stage("drop_slot")
			return nil
		},
		finish: func(_ context.Context, _ *jobEnv, _ config.Cluster, spec config.ClusterSpec, source string, resume func()) ([]output.Warning, error) {
			s.mu.Lock()
			s.finishSpec, s.finishSource = spec, source
			s.mu.Unlock()
			if err := s.stage("finish"); err != nil {
				return nil, err
			}
			resume()
			s.mu.Lock()
			s.resumed = true
			s.mu.Unlock()
			return nil, nil
		},
		fence: func(context.Context, *jobEnv, config.Cluster, string) (func(), error) {
			if err := s.stage("fence"); err != nil {
				return nil, err
			}
			return func() {
				s.mu.Lock()
				s.resumed = true
				s.mu.Unlock()
			}, nil
		},
		settle: func(context.Context, sqlRunner, config.ClusterSpec, string, string) error {
			return s.stage("settle")
		},
		resume: func(context.Context, string) {
			s.mu.Lock()
			s.resumed = true
			s.mu.Unlock()
		},
	}
	return s
}

func (s *followerStub) stage(name string) error {
	s.mu.Lock()
	s.calls = append(s.calls, name)
	failAt, failErr, before := s.failAt, s.failErr, s.before
	s.mu.Unlock()
	if before != nil {
		before(name)
	}
	if name == failAt {
		return failErr
	}
	return nil
}

func (s *followerStub) stages() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

// failed reports whether stage was reached.
func (s *followerStub) reached(want ...string) bool {
	got := s.stages()
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// followEnv is db-follow's job for name, as RunJob builds it.
func followEnv(cfg config.Cluster, name string, recover bool) *jobEnv {
	since := ""
	if recover {
		recs, err := backup.LoadRecords()
		if err != nil {
			panic(err)
		}
		since = recs.AwaitingRecovery[name].Since
	}
	params, _ := json.Marshal(dbFollowParams{Source: "app", Name: name, From: followFromBaseBackup, Recover: recover, RecoverSince: since})
	return &jobEnv{ID: "01J0000000000000000000TEST", Job: "db-follow", Cluster: cfg, Params: params}
}

// unfollowEnv is db-unfollow's job for name, with the operator's passwords.
func unfollowEnv(cfg config.Cluster, name string) *jobEnv {
	params, _ := json.Marshal(dbUnfollowParams{Name: name})
	return &jobEnv{ID: "01J0000000000000000000TEST", Job: "db-unfollow", Cluster: cfg, Params: params, Secrets: rolePasswords(name)}
}

// rolePasswords are the passwords the operator generates for name's roles.
func rolePasswords(name string) map[string]string {
	out := map[string]string{}
	for _, role := range steps.DatabaseRoles {
		out[steps.SecretDatabaseRole(name, role)] = name + "-" + role + "-pw"
	}
	return out
}

// R2-1: db recover keeps the durable awaiting entry until the follower runs.
// The copy and its adoption are one durable boundary (nothing before the
// adoption can be resumed without a cluster), so a failure at any point in them
// leaves the entry, and db recover is admitted again; once the follower runs the
// entry is cleared, and a copy that was adopted already is started instead of
// copied again.
func TestRecoverKeepsAwaitingUntilTheFollowerRuns(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	stub := (&followerStub{}).install(t)
	installRetainingSlot(t)

	// Before the slot and the copy, on the copy itself, and before the
	// adoption: each returns through the same boundary.
	for _, stopped := range []string{"before the slot", "during the copy", "before the adoption"} {
		stub.failAt, stub.failErr = "build", errors.New("the copy stopped "+stopped)
		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
			t.Fatalf("a recovery that stopped %s returned no error", stopped)
		}
		recs := node.records()
		aw, ok := recs.AwaitingRecovery["app_follower"]
		if !ok {
			t.Fatalf("the recovery that stopped %s cleared the durable awaiting entry", stopped)
		}
		if aw.Adopted {
			t.Errorf("an entry whose copy never finished (%s) is marked adopted", stopped)
		}
		if aw.Stanza != "" {
			t.Errorf("the entry lost its stanza %s: %+v", stopped, aw)
		}
		// The retry is admitted: db recover reaches the job again (its dry run
		// checks everything the real run does before it starts it).
		res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
		if err != nil {
			t.Fatalf("the retry after %s is refused: %v", stopped, err)
		}
		if res.Data.(map[string]any)["resume"] != false {
			t.Errorf("a copy that was never adopted (%s) is resumed instead of copied: %v", stopped, res.Data)
		}
	}

	// A failure after the adoption keeps the entry and marks it adopted: the
	// retry starts the follower rather than copying over it.
	stub.failAt, stub.failErr = "start", errors.New("the start died")
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("a failed start returned no error")
	}
	if aw := node.records().AwaitingRecovery["app_follower"]; !aw.Adopted {
		t.Fatalf("the entry of an adopted copy is not marked adopted: %+v", aw)
	}
	node.copyReady(cfg, "app_follower")
	res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the retry after a failed start is refused: %v", err)
	}
	if res.Data.(map[string]any)["resume"] != true {
		t.Errorf("an adopted copy is copied again: %v", res.Data)
	}
	stub.calls, stub.failAt, stub.failErr = nil, "", nil
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the resumed recovery failed: %v", err)
	}
	if stub.recovering != "app_follower" {
		t.Errorf("the steps were given %q to keep in their view, so the entry had to be cleared to start the follower", stub.recovering)
	}
	if stub.reached("build") {
		t.Error("the resumed recovery copied the adopted follower again")
	}
	if recs := node.records(); recs.IsAwaiting("app_follower") {
		t.Error("the recovered follower still awaits recovery")
	}
}

// recoverDryRun is db.recover's dry-run request for db.
func recoverDryRun(db string) (req rpc.Request) {
	req.Method = "db.recover"
	req.Params, _ = json.Marshal(map[string]any{"database": db})
	req.DryRun = true
	return req
}

// R2-1: db recover admits the deterministic paths only when they are proven to
// be the operation's own an owned interruption, or an adopted registration that
// points at the configured data directory. Foreign or mismatched paths are
// refused untouched.
func TestFollowerRecoverInTheWay(t *testing.T) {
	for name, test := range map[string]struct {
		art            followerArtefacts
		adopted, owned bool
		refused        bool
	}{
		"nothing there":            {refused: false},
		"foreign data":             {art: followerArtefacts{Data: true}, refused: true},
		"foreign registration":     {art: followerArtefacts{Conf: true, Data: true}, refused: true},
		"foreign config directory": {art: followerArtefacts{ConfDir: true}, refused: true},
		"owned partial data":       {art: followerArtefacts{Data: true}, owned: true},
		"owned config directory":   {art: followerArtefacts{ConfDir: true}, owned: true},
		"owned mismatched":         {art: followerArtefacts{Conf: true, Data: true}, owned: true, refused: true},
		"owned registered copy":    {art: followerArtefacts{Conf: true, Data: true, Match: true}, owned: true},
		"adopted mismatched":       {art: followerArtefacts{Conf: true, Data: true}, adopted: true, refused: true},
		"adopted registered copy":  {art: followerArtefacts{Conf: true, Data: true, Match: true}, adopted: true},
	} {
		err := followerRecoverRefusal(test.art, test.adopted, test.owned, "app_follower")
		if (err != nil) != test.refused {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if err != nil && reason(t, err) != "cluster_exists" {
			t.Errorf("%s: refusal reason %q", name, reason(t, err))
		}
	}
}

// installRetainingSlot makes the source slot read as a reserved slot that
// still retains its position, without a live source cluster.
func installRetainingSlot(t *testing.T) {
	t.Helper()
	old := followerSlotOps
	followerSlotOps.inspect = func(context.Context, config.ClusterSpec, string) (followerSlotState, error) {
		return followerSlotState{Exists: true, Usable: true}, nil
	}
	t.Cleanup(func() { followerSlotOps = old })
}

// followCopyPlan replaces the physical copy of a build at its narrowest seam:
// the fresh and resumed Materialised constructors and the source-slot effects.
// The production materialiseFollower, ownership record, admission and retry
// decisions run for real; only the copy/adopt effects are scripted.
type followCopyPlan struct {
	node       *followerNode
	cfg        config.Cluster
	calls      []string
	restoreErr error
	adoptErr   error
	newErr     error
	onRestore  func()
	onAdopt    func()
	// The source slot and its retained position. reset/create move it to the
	// source's current position; the resume path must leave slotLSN alone.
	slot        followerSlotState
	slotLSN     string
	inspectErr  error
	createErr   error
	resetErr    error
	dropErr     error
	onDropOwned func(name, data string) error
}

type followCopyStub struct {
	plan *followCopyPlan
	data string
}

func (c *followCopyStub) DataDir() string { return c.data }

func (c *followCopyStub) copy() error {
	c.plan.calls = append(c.plan.calls, "copy")
	if c.plan.restoreErr != nil {
		return c.plan.restoreErr
	}
	if c.plan.onRestore != nil {
		c.plan.onRestore()
	}
	return nil
}

func (c *followCopyStub) Restore(context.Context, backup.Driver, backup.RestoreOptions) error {
	return c.copy()
}

func (c *followCopyStub) StandbyBaseBackup(context.Context, string) error { return c.copy() }

func (c *followCopyStub) Adopt(context.Context, int) error {
	c.plan.calls = append(c.plan.calls, "adopt")
	if c.plan.adoptErr != nil {
		return c.plan.adoptErr
	}
	if c.plan.onAdopt != nil {
		c.plan.onAdopt()
	}
	return nil
}

func (c *followCopyStub) Drop() error {
	c.plan.calls = append(c.plan.calls, "drop-copy")
	return os.RemoveAll(c.data)
}

func installFollowCopy(t *testing.T, plan *followCopyPlan) {
	t.Helper()
	oldNew, oldResume, oldDrop, oldSlots := newFollowerCopy, resumeFollowerCopy, dropOwnedFollowerCopy, followerSlotOps
	t.Cleanup(func() {
		newFollowerCopy, resumeFollowerCopy, dropOwnedFollowerCopy, followerSlotOps = oldNew, oldResume, oldDrop, oldSlots
	})
	followerSlotOps.inspect = func(context.Context, config.ClusterSpec, string) (followerSlotState, error) {
		plan.calls = append(plan.calls, "inspect-slot")
		if plan.inspectErr != nil {
			return followerSlotState{}, plan.inspectErr
		}
		return plan.slot, nil
	}
	followerSlotOps.create = func(context.Context, config.ClusterSpec, string) error {
		plan.calls = append(plan.calls, "create-slot")
		if plan.createErr != nil {
			return plan.createErr
		}
		plan.slot, plan.slotLSN = followerSlotState{Exists: true, Usable: true}, "current"
		return nil
	}
	followerSlotOps.reset = func(context.Context, config.ClusterSpec, string) error {
		plan.calls = append(plan.calls, "reset-slot")
		if plan.resetErr != nil {
			return plan.resetErr
		}
		plan.slot, plan.slotLSN = followerSlotState{Exists: true, Usable: true}, "current"
		return nil
	}
	followerSlotOps.drop = func(context.Context, config.ClusterSpec, string) error {
		plan.calls = append(plan.calls, "drop-slot")
		if plan.dropErr != nil {
			return plan.dropErr
		}
		plan.slot, plan.slotLSN = followerSlotState{}, ""
		return nil
	}
	fake := func(dataRoot, name string) followerCopy {
		return &followCopyStub{plan: plan, data: filepath.Join(dataRoot, name)}
	}
	newFollowerCopy = func(dataRoot, name, _ string, _ int, _ engine.Shell, _ string, _ engine.Reporter) (followerCopy, error) {
		if plan.newErr != nil {
			return nil, plan.newErr
		}
		return fake(dataRoot, name), nil
	}
	resumeFollowerCopy = func(dataRoot, name, _ string, _ int, _ engine.Shell, _ string, _ engine.Reporter) followerCopy {
		return fake(dataRoot, name)
	}
	dropOwnedFollowerCopy = func(_ engine.Shell, _ string, _ engine.Reporter, name, data string) error {
		plan.calls = append(plan.calls, "drop-owned")
		if plan.onDropOwned != nil {
			return plan.onDropOwned(name, data)
		}
		delete(plan.node.registered, name)
		return errors.Join(os.RemoveAll(data), os.RemoveAll(plan.node.confDir(name)))
	}
}

// installFollowBuildInstall runs the production build behind the job's other
// server effects, which stay scripted.
func installFollowBuildJob(t *testing.T) *followerStub {
	t.Helper()
	stub := (&followerStub{}).install(t)
	followerServerOps.build = materialiseFollower
	return stub
}

// plan holds the shared call log of the scripted copy.
func (p *followCopyPlan) has(call string) bool { return p.count(call) > 0 }

// before reports that first happened before second.
func (p *followCopyPlan) before(first, second string) bool {
	a, b := -1, -1
	for i, c := range p.calls {
		if c == first && a < 0 {
			a = i
		}
		if c == second && b < 0 {
			b = i
		}
	}
	return a >= 0 && b >= 0 && a < b
}

func (p *followCopyPlan) count(call string) int {
	n := 0
	for _, c := range p.calls {
		if c == call {
			n++
		}
	}
	return n
}

// R2-1 / FG-M4: a follow and a recover continuation both own local paths and
// roll back desired state, so their jobs take the node and both clusters'
// locks exclusively.
func TestRecoverFollowTakesExclusiveLocks(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	if got := jobLockMode(followEnv(cfg, "app_follower", true)); got != engine.LockExclusive {
		t.Fatalf("a recover follow takes %v", got)
	}
	if got := jobLockMode(followEnv(cfg, "app_follower", false)); got != engine.LockExclusive {
		t.Fatalf("a plain follow takes %v", got)
	}
}

// R2-1: a job that died while copying owns the partial data/configuration it
// left. A retry admitted by the public handler removes exactly those owned
// paths and builds again; no manual pg_dropcluster is involved.
func TestRecoverRebuildsAnOwnedInterruptedCopy(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	// The crash left the copying record and a partial data directory.
	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopying); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.Server.DataDirectory("app_follower"), 0o700); err != nil {
		t.Fatal(err)
	}

	res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the retry is refused: %v", err)
	}
	if res.Data.(map[string]any)["resume"] != false {
		t.Fatalf("an incomplete copy is resumed: %v", res.Data)
	}
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !plan.has("drop-owned") {
		t.Fatalf("the owned partial copy was not removed: %v", plan.calls)
	}
	if plan.count("copy") != 1 || plan.count("adopt") != 1 {
		t.Fatalf("the retry did not rebuild once: %v", plan.calls)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the ownership record survived a completed recovery: %v", err)
	}
	if recs := node.records(); recs.IsAwaiting("app_follower") {
		t.Fatal("the recovered follower still awaits recovery")
	}
}

// R2-1: once the copy is complete and registered, an adoption that died in any
// of its files is resumed: the retry adopts the same owned copy again instead
// of copying it or dropping it.
func TestRecoverResumesAnOwnedAdoption(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died between its files"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an adoption that died returned no error")
	}
	if aw := node.records().AwaitingRecovery["app_follower"]; aw.Adopted {
		t.Fatalf("an incomplete adoption is marked adopted: %+v", aw)
	}
	if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase != followPhaseCopied {
		t.Fatalf("the completed copy has no copied record: %+v %v %v", own, ok, err)
	}

	plan.adoptErr = nil
	res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the retry is refused: %v", err)
	}
	if res.Data.(map[string]any)["resume"] != true {
		t.Fatalf("the owned copy is not resumed: %v", res.Data)
	}
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the resumed recovery failed: %v", err)
	}
	if plan.count("copy") != 1 || plan.count("adopt") != 2 || plan.has("drop-owned") {
		t.Fatalf("the retry did not resume the same copy: %v", plan.calls)
	}
	if recs := node.records(); recs.IsAwaiting("app_follower") {
		t.Fatal("the resumed follower still awaits recovery")
	}
}

// R2-1: resuming a completed copy inspects the slot it left behind and leaves
// it exactly as it is. An inactive slot's older retained position is the WAL
// the copy still needs, so a resume must not drop or reset it.
func TestRecoverResumeKeepsTheCompletedCopysSlot(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an adoption that died returned no error")
	}
	if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase != followPhaseCopied {
		t.Fatalf("the completed copy has no copied record: %+v %v %v", own, ok, err)
	}

	// The source kept writing while the slot stayed inactive: it retains WAL
	// from before this retry.
	plan.slot, plan.slotLSN = followerSlotState{Exists: true, Usable: true}, "0/1000"
	plan.calls, plan.adoptErr = nil, nil
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the resumed recovery failed: %v", err)
	}
	if !plan.has("inspect-slot") {
		t.Fatalf("the resume did not inspect the slot: %v", plan.calls)
	}
	for _, destructive := range []string{"create-slot", "reset-slot", "drop-slot", "drop-owned"} {
		if plan.has(destructive) {
			t.Fatalf("the resume touched the completed copy's slot (%s): %v", destructive, plan.calls)
		}
	}
	if plan.slotLSN != "0/1000" {
		t.Fatalf("the slot's retained position moved to %q", plan.slotLSN)
	}
	if plan.count("copy") != 0 || plan.count("adopt") != 1 {
		t.Fatalf("the resume did not adopt the same copy: %v", plan.calls)
	}
}

// R2-1: a completed copy whose slot is missing, lost or no longer reserving
// WAL (unreserved, or a status vaire does not know) cannot expect the WAL it
// still needs, so the operation drops exactly its own copy and copies again
// with a fresh slot instead of resetting the slot beneath the old copy.
func TestRecoverRebuildsACompletedCopyWhoseSlotIsMissingOrLost(t *testing.T) {
	for name, test := range map[string]struct {
		slot  followerSlotState
		fresh string
	}{
		"missing":        {slot: followerSlotState{}, fresh: "create-slot"},
		"lost":           {slot: followerSlotState{Exists: true, Usable: false}, fresh: "reset-slot"},
		"unreserved":     {slot: followerSlotState{Exists: true, Usable: false}, fresh: "reset-slot"},
		"unknown status": {slot: followerSlotState{Exists: true, Usable: false}, fresh: "reset-slot"},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			node.awaitRecovery("app_follower")
			installFollowBuildJob(t)
			plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died"),
				onRestore: func() { node.copyReady(cfg, "app_follower") }}
			installFollowCopy(t, plan)

			if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
				t.Fatal("an adoption that died returned no error")
			}
			plan.slot, plan.slotLSN = test.slot, "0/1000"
			plan.calls, plan.adoptErr = nil, nil
			if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
				t.Fatalf("the retry failed: %v", err)
			}
			if !plan.has("drop-owned") || !plan.has(test.fresh) {
				t.Fatalf("the unusable slot did not take the owned recopy path: %v", plan.calls)
			}
			if !plan.before("drop-owned", test.fresh) {
				t.Fatalf("the fresh slot came before the owned copy was removed: %v", plan.calls)
			}
			if plan.count("copy") != 1 || plan.count("adopt") != 1 {
				t.Fatalf("the retry did not copy and adopt once: %v", plan.calls)
			}
			if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
				t.Fatalf("the ownership record survived the completed recopy: %v", err)
			}
			if recs := node.records(); recs.IsAwaiting("app_follower") {
				t.Fatal("the recopied follower still awaits recovery")
			}
		})
	}
}

// P2: db recover's dry run reads a completed copy's source slot instead of
// promising reuse: an unusable slot reports the recopy it makes, a slot the
// preview cannot read is reported as conditional, and the preview changes
// nothing. An adopted copy resumes the start, so it reads no slot, as
// execution does.
func TestRecoverDryRunReportsTheCompletedCopysSlot(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an adoption that died returned no error")
	}
	for name, test := range map[string]struct {
		slot        followerSlotState
		inspectErr  error
		resume      bool
		recopy      bool
		conditional bool
		inspected   bool
	}{
		"retaining":      {slot: followerSlotState{Exists: true, Usable: true}, resume: true, inspected: true},
		"missing":        {recopy: true, inspected: true},
		"lost":           {slot: followerSlotState{Exists: true, Usable: false}, recopy: true, inspected: true},
		"unreserved":     {slot: followerSlotState{Exists: true, Usable: false}, recopy: true, inspected: true},
		"unknown status": {slot: followerSlotState{Exists: true, Usable: false}, recopy: true, inspected: true},
		"unreadable":     {inspectErr: errors.New("the source cannot be read"), recopy: true, conditional: true, inspected: true},
	} {
		t.Run(name, func(t *testing.T) {
			plan.slot, plan.slotLSN = test.slot, "0/1000"
			plan.inspectErr = test.inspectErr
			plan.calls = nil
			res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
			if err != nil {
				t.Fatalf("the dry run is refused: %v", err)
			}
			data := res.Data.(map[string]any)
			if data["resume"] != test.resume {
				t.Fatalf("the preview reports resume=%v: %v", data["resume"], data)
			}
			if _, ok := data["recopy_reason"]; ok != test.recopy {
				t.Fatalf("the preview reports a recopy reason %v: %v", ok, data)
			}
			if test.recopy {
				if msg, _ := data["recopy_reason"].(string); msg == "" {
					t.Fatalf("the preview gives no recopy reason: %v", data)
				}
			}
			if _, ok := data["resume_conditional"]; ok != test.conditional {
				t.Fatalf("the preview reports conditional=%v: %v", ok, data)
			}
			if plan.slot != test.slot || plan.slotLSN != "0/1000" {
				t.Fatalf("the preview changed the slot: %+v %q", plan.slot, plan.slotLSN)
			}
			if got := plan.has("inspect-slot"); got != test.inspected {
				t.Fatalf("the preview inspected the slot %v: %v", got, plan.calls)
			}
			for _, effect := range []string{"create-slot", "reset-slot", "drop-slot", "drop-owned", "copy", "adopt"} {
				if plan.has(effect) {
					t.Fatalf("the preview made a change (%s): %v", effect, plan.calls)
				}
			}
			if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase != followPhaseCopied {
				t.Fatalf("the preview changed the ownership: %+v %v %v", own, ok, err)
			}
			if !node.records().IsAwaiting("app_follower") {
				t.Fatal("the preview cleared the awaiting entry")
			}
		})
	}

	// An adopted copy is still validated against its retained slot: a usable
	// slot starts it as it is, while a missing one takes the proven-owned
	// recopy the preview reports. Both read the slot.
	if err := markFollowerAdopted("app_follower"); err != nil {
		t.Fatal(err)
	}
	plan.slot, plan.slotLSN, plan.inspectErr = followerSlotState{Exists: true, Usable: true}, "0/1000", nil
	plan.calls = nil
	res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the adopted dry run is refused: %v", err)
	}
	if data := res.Data.(map[string]any); data["resume"] != true {
		t.Fatalf("an adopted copy on a retained slot is not resumed: %v", data)
	}
	if !plan.has("inspect-slot") {
		t.Fatalf("an adopted copy did not read its slot: %v", plan.calls)
	}
	plan.slot, plan.slotLSN = followerSlotState{}, ""
	plan.calls = nil
	res, err = handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the adopted dry run with a missing slot is refused: %v", err)
	}
	if data := res.Data.(map[string]any); data["resume"] != false || data["recopy_reason"] == nil {
		t.Fatalf("an adopted copy on a missing slot is not reported as recopied: %v", data)
	}
	if !plan.has("inspect-slot") {
		t.Fatalf("the adopted preview did not read the missing slot: %v", plan.calls)
	}
}

// R2-1: an owned recopy whose cleanup fails leaves the completed copy, its
// ownership and the awaiting entry for a later retry, with no fresh slot
// created beneath it.
func TestRecoverRecopyCleanupFailureKeepsTheOwnedCopy(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an adoption that died returned no error")
	}
	plan.slot, plan.slotLSN = followerSlotState{}, ""
	plan.calls, plan.adoptErr = nil, nil
	plan.onDropOwned = func(string, string) error { return errors.New("the copy could not be removed") }
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || !strings.Contains(err.Error(), "could not be removed") {
		t.Fatalf("a failed recopy cleanup returned %v", err)
	}
	if plan.has("create-slot") || plan.has("reset-slot") || plan.has("copy") {
		t.Fatalf("a failed cleanup left a fresh slot beneath the old copy: %v", plan.calls)
	}
	if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase == followPhaseStarted {
		t.Fatalf("the failed cleanup lost the copy's ownership: %+v %v %v", own, ok, err)
	}
	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err != nil {
		t.Fatalf("the retry is refused: %v", err)
	}
}

// R2-1: when the source slot cannot be inspected, the retry fails closed: the
// completed copy, its ownership and the awaiting entry stay for a later retry.
func TestRecoverResumeSlotInspectionFailureKeepsTheCopy(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, adoptErr: errors.New("the adoption died"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an adoption that died returned no error")
	}
	plan.slot, plan.slotLSN = followerSlotState{Exists: true, Usable: true}, "0/1000"
	plan.calls, plan.adoptErr = nil, nil
	plan.inspectErr = errors.New("the source cannot be read")
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("a failed slot inspection returned %v", err)
	}
	if plan.has("drop-owned") || plan.has("reset-slot") || plan.has("create-slot") || plan.has("drop-slot") || plan.has("copy") {
		t.Fatalf("a failed inspection touched the copy or its slot: %v", plan.calls)
	}
	if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase != followPhaseCopied {
		t.Fatalf("the failed inspection lost the completed copy's ownership: %+v %v %v", own, ok, err)
	}
	if recs := node.records(); !recs.IsAwaiting("app_follower") {
		t.Fatal("the failed inspection cleared the awaiting entry")
	}
	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err != nil {
		t.Fatalf("the retry is refused: %v", err)
	}

	plan.inspectErr = nil
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the retry after a failed inspection failed: %v", err)
	}
	if plan.has("reset-slot") || plan.has("drop-owned") {
		t.Fatalf("the retry did not resume the completed copy: %v", plan.calls)
	}
}

// R2-1: a registration that died before postgresql.conf leaves an owned
// configuration directory with no postgresql.conf and no data. An interruption
// after the data cleanup, before the directory cleanup, keeps ownership and the
// next retry through public admission finishes the cleanup and builds.
func TestRecoverCleansAnOwnedPartialConfigDirectory(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	if err := os.MkdirAll(cfg.Server.DataDirectory("app_follower"), 0o700); err != nil {
		t.Fatal(err)
	}
	node.partialRegistration("app_follower")
	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopying); err != nil {
		t.Fatal(err)
	}

	// The first cleanup removes the data, then stops before the directory.
	plan.onDropOwned = func(_, data string) error {
		return errors.Join(os.RemoveAll(data), errors.New("the process died before removing the configuration directory"))
	}
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("an interrupted cleanup returned no error")
	}
	if _, err := os.Stat(node.confDir("app_follower")); err != nil {
		t.Fatalf("the test did not leave the configuration directory: %v", err)
	}
	if _, err := os.Stat(cfg.Server.DataDirectory("app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the interrupted cleanup did not remove the data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(node.confDir("app_follower"), "postgresql.conf")); !os.IsNotExist(err) {
		t.Fatalf("the partial registration has a postgresql.conf: %v", err)
	}
	if own, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok || own.Phase != followPhaseCopying {
		t.Fatalf("the interrupted cleanup lost ownership: %+v %v %v", own, ok, err)
	}

	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err != nil {
		t.Fatalf("the retry is refused: %v", err)
	}
	plan.onDropOwned = nil
	plan.calls = nil
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !plan.has("drop-owned") || plan.count("copy") != 1 || plan.count("adopt") != 1 {
		t.Fatalf("the retry did not finish the cleanup and build: %v", plan.calls)
	}
	if _, err := os.Stat(filepath.Join(node.confDir("app_follower"), "postgresql.conf")); err != nil {
		t.Fatalf("the rebuilt registration is not complete: %v", err)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the ownership record survived recovery: %v", err)
	}
}

// R2-1: a configuration directory with no ownership is foreign. Public
// admission refuses it and the job does not touch it.
func TestRecoverRefusesAForeignPartialConfigDirectory(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg}
	installFollowCopy(t, plan)
	node.partialRegistration("app_follower")

	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err == nil || reason(t, err) != "cluster_exists" {
		t.Fatalf("a foreign configuration directory was admitted: %v", err)
	}
	if len(plan.calls) != 0 {
		t.Fatalf("a refused retry touched the foreign directory: %v", plan.calls)
	}
	if _, err := os.Stat(filepath.Join(node.confDir("app_follower"), "start.conf")); err != nil {
		t.Fatalf("the foreign registration was removed: %v", err)
	}
}

// R2-1: a cleanup that claims success but leaves an owned artefact must not
// clear ownership. The constructor then fails on the surviving directory, the
// record stays, and the next retry finishes removing it and builds.
func TestRecoverKeepsOwnershipWhileAnOwnedDirectoryRemains(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, newErr: errors.New("cluster 18/app_follower already exists"),
		onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	node.partialRegistration("app_follower")
	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopying); err != nil {
		t.Fatal(err)
	}
	// The cleanup removes the data it owns and reports success while the
	// configuration directory survives.
	plan.onDropOwned = func(_, data string) error { return os.RemoveAll(data) }
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("the constructor failure returned no error")
	}
	if _, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok {
		t.Fatalf("ownership was cleared while the owned directory remains: %v %v", ok, err)
	}

	plan.onDropOwned, plan.newErr = nil, nil
	plan.calls = nil
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if !plan.has("drop-owned") || plan.count("copy") != 1 {
		t.Fatalf("the retry did not remove the directory and build: %v", plan.calls)
	}
	if _, err := os.Stat(filepath.Join(node.confDir("app_follower"), "postgresql.conf")); err != nil {
		t.Fatalf("the rebuilt registration is not complete: %v", err)
	}
}

// R2-6: a fresh build reserves the source slot before it constructs its copy.
// A constructor failure drops that slot again, joins a cleanup failure, and
// leaves the awaiting entry retryable; a plain follow forgets its entry. No
// local path is touched by the cleanup.
func TestFollowConstructorFailureDropsTheFreshSlot(t *testing.T) {
	t.Run("normal follow", func(t *testing.T) {
		ctx := context.Background()
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowBuildJob(t)
		plan := &followCopyPlan{node: node, cfg: cfg, newErr: errors.New("all temporary ports are reserved")}
		installFollowCopy(t, plan)
		foreign := filepath.Join(node.dir, "foreign-copy")
		if err := os.WriteFile(foreign, []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}
		src, _ := cfg.ClusterByName("app")
		spec, _ := cfg.ClusterByName("app_follower")

		if err := materialiseFollower(ctx, followEnv(cfg, "app_follower", false), cfg, src, spec, followFromBaseBackup, false); err == nil {
			t.Fatal("the constructor failure returned no error")
		}
		if !plan.has("create-slot") || !plan.has("drop-slot") {
			t.Fatalf("the fresh slot was not dropped: %v", plan.calls)
		}
		if plan.has("drop-owned") {
			t.Fatalf("a normal follow removed local paths: %v", plan.calls)
		}
		if _, err := os.Stat(foreign); err != nil {
			t.Fatalf("a foreign local path was removed: %v", err)
		}
	})

	t.Run("recovery", func(t *testing.T) {
		ctx := context.Background()
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		installFollowBuildJob(t)
		plan := &followCopyPlan{node: node, cfg: cfg, newErr: errors.New("all temporary ports are reserved")}
		installFollowCopy(t, plan)
		foreign := filepath.Join(node.dir, "foreign-copy")
		if err := os.WriteFile(foreign, []byte("not ours"), 0o600); err != nil {
			t.Fatal(err)
		}

		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
			t.Fatal("the constructor failure returned no error")
		}
		if !plan.has("create-slot") || !plan.has("drop-slot") {
			t.Fatalf("the fresh slot was not dropped: %v", plan.calls)
		}
		if recs := node.records(); !recs.IsAwaiting("app_follower") || recs.AwaitingRecovery["app_follower"].Stanza != "" {
			t.Fatalf("the failed recovery lost its awaiting entry: %+v", recs)
		}
		if _, err := os.Stat(foreign); err != nil {
			t.Fatalf("a foreign local path was removed: %v", err)
		}
		if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err != nil {
			t.Fatalf("the retry is refused: %v", err)
		}
	})

	t.Run("cleanup error joined", func(t *testing.T) {
		ctx := context.Background()
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowBuildJob(t)
		plan := &followCopyPlan{node: node, cfg: cfg,
			newErr: errors.New("the constructor failed"), dropErr: errors.New("the slot cleanup failed")}
		installFollowCopy(t, plan)
		src, _ := cfg.ClusterByName("app")
		spec, _ := cfg.ClusterByName("app_follower")

		err := materialiseFollower(ctx, followEnv(cfg, "app_follower", false), cfg, src, spec, followFromBaseBackup, false)
		if err == nil || !strings.Contains(err.Error(), "the constructor failed") || !strings.Contains(err.Error(), "the slot cleanup failed") {
			t.Fatalf("the cleanup error was not joined with the constructor error: %v", err)
		}
	})
}

// R2-1: a crash after the adoption finished, before the adopted marker, is
// retried by adopting the same copy again and only then marking it adopted.
func TestRecoverRetriesAfterAdoptionBeforeMarker(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)
	oldMark := markFollowerAdopted
	markFollowerAdopted = func(string) error { return errors.New("the process died before the marker") }
	t.Cleanup(func() { markFollowerAdopted = oldMark })

	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("the failed marker returned no error")
	}
	if aw := node.records().AwaitingRecovery["app_follower"]; aw.Adopted {
		t.Fatalf("the copy was marked adopted without the marker: %+v", aw)
	}

	markFollowerAdopted = oldMark
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	if plan.count("copy") != 1 || plan.count("adopt") != 2 {
		t.Fatalf("the retry recopied or skipped the adoption: %v", plan.calls)
	}
	if recs := node.records(); recs.IsAwaiting("app_follower") {
		t.Fatal("the recovered follower still awaits recovery")
	}
}

// R2-1: foreign deterministic paths are refused untouched. A record from
// another instance (a rebuilt host) is stale and owns nothing. With no
// artefacts, a stale record is replaced by a fresh build.
func TestRecoverRefusesForeignPathsAndIgnoresARebuiltHost(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)

	node.copyReady(cfg, "app_follower")
	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err == nil || reason(t, err) != "cluster_exists" {
		t.Fatalf("a foreign cluster was admitted: %v", err)
	}

	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	node.instance = "the rebuilt instance"
	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err == nil || reason(t, err) != "cluster_exists" {
		t.Fatalf("progress from before a rebuild was accepted: %v", err)
	}
	if plan.has("drop-owned") || plan.has("copy") {
		t.Fatalf("a refused retry touched the foreign cluster: %v", plan.calls)
	}

	if err := os.RemoveAll(cfg.Server.DataDirectory("app_follower")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(node.confDir("app_follower")); err != nil {
		t.Fatal(err)
	}
	delete(node.registered, "app_follower")
	if _, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg}); err != nil {
		t.Fatalf("a fresh build after a rebuild is refused: %v", err)
	}
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
		t.Fatalf("the fresh build after a rebuild failed: %v", err)
	}
	if plan.count("copy") != 1 {
		t.Fatalf("the stale record was not replaced by a fresh build: %v", plan.calls)
	}
}

// R2-1: ownership belongs to one awaiting record. A later rebuild (or a
// dropped and recreated database) has a new since value, so a record left by an
// abandoned recovery owns nothing on the new one.
func TestFollowOwnershipIsBoundToTheAwaitingRecord(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	node.awaitRecovery("app_follower")
	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	if _, owned, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !owned {
		t.Fatalf("the matching awaiting record owns no copy: %v %v", owned, err)
	}
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		aw := s.AwaitingRecovery["app_follower"]
		aw.Since = "2026-02-02T00:00:00Z"
		s.AwaitingRecovery["app_follower"] = aw
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, owned, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || owned {
		t.Fatalf("a record from an older awaiting entry still owns the copy: %v %v", owned, err)
	}
}

func followOwnershipPathForTest(t *testing.T, name string) string {
	t.Helper()
	path, err := followOwnershipPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// R2-2: a database that follows nothing and has no unfinished unfollow is still
// refused, by the RPC and by the job.
func TestUnfollowRefusesWhatItCannotResume(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	(&followerStub{}).install(t)
	for name, err := range map[string]error{
		"unfollow of the source": func() error {
			_, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app", true), Cluster: cfg})
			return err
		}(),
		"job for the source": func() error {
			_, err := jobUnfollow(ctx, unfollowEnv(cfg, "app"))
			return err
		}(),
		"unknown database": handleDBUnfollowErr(ctx, cfg, "nope"),
	} {
		if err == nil {
			t.Errorf("%s is not refused", name)
			continue
		}
		if decoded := reason(t, err); decoded != "not_a_follower" && decoded != "database_not_found" {
			t.Errorf("%s: reason %q", name, decoded)
		}
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Errorf("a refused unfollow left a record: %v", recs)
	}
}

// handleDBUnfollowErr runs db.unfollow's dry run for db and returns its error.
func handleDBUnfollowErr(ctx context.Context, cfg config.Cluster, db string) error {
	_, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest(db, true), Cluster: cfg})
	return err
}

// R2-2: db unfollow records the unfollow before its first irreversible step and
// finishes it across every boundary. Whatever stops it, the record keeps the
// original source and stanza, the public command admits the retry, and the
// retry finishes with that same stanza: the configuration, slot, routing and
// backups are all done, once.
func TestUnfollowResumesAfterEveryBoundary(t *testing.T) {
	ctx := context.Background()
	for _, stop := range []string{"remove_conf", "restart", "finish"} {
		t.Run(stop, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			stub := (&followerStub{failAt: stop, failErr: errors.New("stopped at " + stop)}).install(t)
			// A crash leaves records.json as it is at the boundary itself.
			atBoundary := ""
			stub.before = func(stage string) {
				if stage != stop {
					return
				}
				recs, err := backup.LoadRecords()
				if err != nil {
					t.Error(err)
					return
				}
				if _, ok := recs.Unfollowing["app_follower"]; ok {
					atBoundary = stage
				}
			}

			if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err == nil {
				t.Fatalf("a failure at %s returned no error", stop)
			}
			if atBoundary != stop {
				t.Fatalf("the record was not on disk when the job reached %s", stop)
			}
			recs := node.records()
			unfinished, ok := recs.Unfollowing["app_follower"]
			if !ok {
				t.Fatalf("the unfollow that stopped at %s left no record", stop)
			}
			if unfinished.Source != "app" || unfinished.Stanza == "" {
				t.Fatalf("the record lost the source or stanza: %+v", unfinished)
			}
			saved := node.config()
			spec, _ := saved.ClusterByName("app_follower")
			if spec.Follows != "" || spec.Stanza != unfinished.Stanza {
				t.Fatalf("cluster.toml after %s: follows %q stanza %q, want none and %q", stop, spec.Follows, spec.Stanza, unfinished.Stanza)
			}
			// The public entry point admits the retry through the record alone.
			if _, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app_follower", true), Cluster: saved}); err != nil {
				t.Fatalf("the retry after %s is refused: %v", stop, err)
			}
			res, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app_follower", true), Cluster: saved})
			if err != nil {
				t.Fatal(err)
			}
			if data := res.Data.(map[string]any); data["resume"] != true || data["stanza"] != unfinished.Stanza {
				t.Fatalf("the dry run does not resume the record: %v", data)
			}

			stub.failAt = ""
			out, err := jobUnfollow(ctx, unfollowEnv(saved, "app_follower"))
			if err != nil {
				t.Fatalf("the retry failed: %v", err)
			}
			data := out.(map[string]any)
			if data["stanza"] != unfinished.Stanza {
				t.Errorf("the retry took a new stanza: %v, want %s", data["stanza"], unfinished.Stanza)
			}
			if data["source"] != "app" {
				t.Errorf("the retry lost the original source: %v", data["source"])
			}
			if got := node.records().Unfollowing; len(got) != 0 {
				t.Errorf("the finished unfollow left a record: %v", got)
			}
			spec, _ = node.config().ClusterByName("app_follower")
			if spec.Follows != "" || spec.Stanza != unfinished.Stanza {
				t.Errorf("cluster.toml after the retry: follows %q stanza %q", spec.Follows, spec.Stanza)
			}
			if stub.confOf != "app_follower" {
				t.Errorf("the obsolete follower configuration was not removed: %q", stub.confOf)
			}
			if stub.finishSource != "app" {
				t.Errorf("the retry finished against %q, not the original source", stub.finishSource)
			}
			if stub.slotSource != "app" {
				t.Errorf("the slot was not dropped on the original source: %q", stub.slotSource)
			}
			if stub.finishSpec.Name != "app_follower" || stub.finishSpec.Stanza != unfinished.Stanza || !stub.resumed {
				t.Errorf("the retry did not finish the routing and backups: %+v resumed %v", stub.finishSpec, stub.resumed)
			}
		})
	}
}

// R2-2: a resumed unfollow whose earlier attempt stopped at its restart finds
// the cluster down, which is what a failed restart leaves. It starts it with its
// own configuration before the promotion, which needs a running cluster, and
// then finishes as that attempt would have.
func TestUnfollowResumeStartsAStoppedCluster(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{failAt: "restart", failErr: errors.New("the restart failed")}).install(t)
	if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err == nil {
		t.Fatal("the failed unfollow returned no error")
	}
	saved := node.config()
	clusterRunning = func(config.ClusterSpec) bool { return false }
	// The public command admits the retry with the cluster down.
	if _, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app_follower", true), Cluster: saved}); err != nil {
		t.Fatalf("the retry with the cluster down is refused: %v", err)
	}

	stub.calls, stub.failAt, stub.failErr = nil, "", nil
	out, err := jobUnfollow(ctx, unfollowEnv(saved, "app_follower"))
	if err != nil {
		t.Fatalf("the retry failed: %v", err)
	}
	got := stub.stages()
	if len(got) == 0 || got[0] != "restart" {
		t.Fatalf("the retry did not start the stopped cluster first: %v", got)
	}
	promoted := false
	for _, stage := range got[1:] {
		promoted = promoted || stage == "promote"
	}
	if !promoted {
		t.Errorf("the retry never promoted the follower: %v", got)
	}
	if data := out.(map[string]any); data["source"] != "app" {
		t.Errorf("the retry lost the source: %v", data["source"])
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Errorf("the finished unfollow left a record: %v", recs)
	}
	if spec, _ := node.config().ClusterByName("app_follower"); spec.Follows != "" {
		t.Errorf("the retried follower still follows: %+v", spec)
	}
}

// R2-2: the record is written before the unfollow's first irreversible step, so
// a job that stops during the promotion or the renames still admits a
// continuation.
func TestUnfollowRecordsBeforeItActs(t *testing.T) {
	ctx := context.Background()
	for _, stop := range []string{"promote", "rename"} {
		t.Run(stop, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			recordedBefore := false
			stub := (&followerStub{failAt: stop, failErr: errors.New("stopped at " + stop)}).install(t)
			stub.before = func(stage string) {
				if stage != stop {
					return
				}
				// The record is written before the first irreversible step.
				recs, err := backup.LoadRecords()
				recordedBefore = err == nil && recs.Unfollowing["app_follower"].Stanza != ""
			}
			if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err == nil {
				t.Fatal("the failed unfollow returned no error")
			}
			if !recordedBefore {
				t.Fatalf("the unfollow reached %s without a record on disk", stop)
			}
			unfinished, ok := node.records().Unfollowing["app_follower"]
			if !ok || unfinished.Source != "app" || unfinished.Stanza == "" {
				t.Fatalf("a failure at %s left no record: %+v", stop, unfinished)
			}
			// cluster.toml still says it follows app, and the record admits a
			// continuation with the same source and stanza.
			spec, _ := node.config().ClusterByName("app_follower")
			if spec.Follows != "app" {
				t.Fatalf("cluster.toml changed before the renames finished: %+v", spec)
			}
			if _, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app_follower", true), Cluster: node.config()}); err != nil {
				t.Fatalf("the continuation is refused: %v", err)
			}
		})
	}
}

// A-R4 / RB-03: an unfollow that cannot fence both poolers must not promote.
// The durable record remains unsettled, the cluster still follows, and no
// resume is sent to a pooler that may never have been paused.
func TestUnfollowFenceFailureRefusesPromotion(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{failAt: "fence", failErr: errors.New("the session pooler did not answer")}).install(t)

	if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err == nil {
		t.Fatal("an unfollow behind a failed fence returned no error")
	}
	if stub.reached("promote", "rename", "restart", "finish") {
		t.Fatalf("a failed fence did not refuse the promotion: %v", stub.stages())
	}
	if stub.resumed {
		t.Fatal("a failed fence resumed a pooler before any promotion")
	}
	recs := node.records()
	unfinished, ok := recs.Unfollowing["app_follower"]
	if !ok || unfinished.Settled {
		t.Fatalf("the failed fence left no unsettled record: %+v", unfinished)
	}
	if spec, _ := node.config().ClusterByName("app_follower"); spec.Follows != "app" {
		t.Fatalf("the failed fence changed cluster.toml: %+v", spec)
	}
}

// A-R4 / RB-03: the privilege transition is settled after the promotion and
// again after the rename, while the alias is still fenced, and only then is
// the record marked settled for finishUnfollow (which re-renders the poolers
// and resumes them).
func TestUnfollowSettlesAccessBeforeExposingTheAlias(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters[1].Protected = true
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{}).install(t)
	var atPromote, atRestart, atFinish backup.Unfollow
	stub.before = func(stage string) {
		recs, err := backup.LoadRecords()
		if err != nil {
			t.Error(err)
			return
		}
		switch stage {
		case "promote":
			atPromote = recs.Unfollowing["app_follower"]
		case "restart":
			atRestart = recs.Unfollowing["app_follower"]
		case "finish":
			atFinish = recs.Unfollowing["app_follower"]
		}
	}

	if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err != nil {
		t.Fatalf("the unfollow failed: %v", err)
	}
	want := []string{"fence", "promote", "settle", "rename", "settle", "remove_conf", "restart", "drop_slot", "finish"}
	if got := stub.stages(); !slices.Equal(got, want) {
		t.Fatalf("stages = %v, want %v", got, want)
	}
	if atPromote.Settled || atRestart.Settled {
		t.Fatalf("the record was settled before the restart: promote %+v restart %+v", atPromote, atRestart)
	}
	if !atFinish.Settled {
		t.Fatalf("finish saw an unsettled record: %+v", atFinish)
	}
	if !stub.resumed {
		t.Fatal("the poolers were not resumed after the settled render")
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Fatalf("the finished unfollow left a record: %v", recs)
	}
}

// A-R4 / RB-03: the protected admin policy names the database and owner the
// cluster holds, whichever phase a partial rename stopped at.
func TestSettleUnfollowAdminAccessUsesActualNames(t *testing.T) {
	ctx := context.Background()
	spec := config.ClusterSpec{Name: "app_follower", Protected: true}
	for _, tc := range []struct {
		name      string
		databases map[string]bool
		roles     map[string]bool
		want      []string
	}{
		{
			name:      "before the renames",
			databases: map[string]bool{"app": true},
			roles:     map[string]bool{"app_owner": true, "app_migrate": true, "app_app": true, "app_ro": true},
			want:      []string{`ALTER ROLE "app_owner" NOLOGIN;`, `datname = 'app'`},
		},
		{
			name:      "after the renames",
			databases: map[string]bool{"app_follower": true},
			roles:     map[string]bool{"app_follower_owner": true, "app_follower_migrate": true},
			want:      []string{`ALTER ROLE "app_follower_owner" NOLOGIN;`, `datname = 'app_follower'`},
		},
		{
			name:      "database renamed first",
			databases: map[string]bool{"app_follower": true},
			roles:     map[string]bool{"app_owner": true},
			want:      []string{`ALTER ROLE "app_owner" NOLOGIN;`, `datname = 'app_follower'`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSQL{databases: tc.databases, roles: tc.roles}
			if err := settleUnfollowAdminAccess(ctx, fake, spec, "app", "app_follower"); err != nil {
				t.Fatal(err)
			}
			sql := fake.last()
			for _, want := range tc.want {
				if !strings.Contains(sql, want) {
					t.Errorf("missing %q in\n%s", want, sql)
				}
			}
		})
	}

	fake := &fakeSQL{databases: map[string]bool{"app": true, "app_follower": true}, roles: map[string]bool{"app_owner": true}}
	if err := settleUnfollowAdminAccess(ctx, fake, spec, "app", "app_follower"); err == nil {
		t.Fatal("an ambiguous database was settled instead of refused")
	}
	if fake.last() != "" {
		t.Fatalf("an ambiguous database still had its policy run:\n%s", fake.last())
	}
	if err := settleUnfollowAdminAccess(ctx, &fakeSQL{}, config.ClusterSpec{Name: "app_follower"}, "app", "app_follower"); err != nil {
		t.Fatalf("an unprotected database needs no policy: %v", err)
	}
}

// A retry whose previous attempt already settled the privilege transition
// does not fence again; finish still resumes both poolers, clearing a pause
// the earlier attempt left before it stopped.
func TestUnfollowSettledRetryResumesWithoutRefencing(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.Unfollowing["app_follower"] = backup.Unfollow{
			Source: "app", Stanza: "app_follower-abc123", StartedAt: "2026-11-10T00:00:00Z", Settled: true,
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	stub := (&followerStub{}).install(t)
	if _, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower")); err != nil {
		t.Fatalf("the settled retry failed: %v", err)
	}
	if stub.reached("fence") {
		t.Fatalf("a settled retry fenced the poolers again: %v", stub.stages())
	}
	if !stub.reached("promote", "settle", "rename", "restart", "finish") {
		t.Fatalf("the settled retry skipped work: %v", stub.stages())
	}
	if !stub.resumed {
		t.Fatal("the settled retry did not resume the poolers")
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Fatalf("the settled retry left a record: %v", recs)
	}
}

// unfollowRequest is db.unfollow's request for db: confirmed, or a dry run.
func unfollowRequest(db string, dryRun bool) (req rpc.Request) {
	req.Method = "db.unfollow"
	req.Params, _ = json.Marshal(map[string]any{"name": db})
	req.Confirm, req.DryRun = db+"/unfollow", dryRun
	if !dryRun {
		req.Secrets = rolePasswords(db)
	}
	return req
}

// R2-4: the unfollow job serializes with every other writer of the desired
// state. It takes the node lock exclusive (as db create, config set and db drop
// do) and its cluster's lock exclusive, so a second unfollow of the same target
// can't promote beside the first and a competing writer waits; nothing it runs
// reacquires the node lock, so it can't deadlock with itself.
func TestUnfollowTakesExclusiveLocks(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	var (
		mu   sync.Mutex
		got  []string
		mods = map[string]engine.LockMode{}
	)
	locks := jobLockers{
		node: func(_ context.Context, mode engine.LockMode, method, _ string, _ time.Duration) (engine.Release, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, "node")
			mods["node"] = mode
			return func() {}, nil
		},
		heavyIO: func(context.Context, string, string) (engine.Release, error) {
			t.Error("the unfollow takes the heavy-I/O lock")
			return func() {}, nil
		},
		cluster: func(_ context.Context, cluster string, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, "cluster "+cluster)
			mods["cluster "+cluster] = mode
			return func() {}, nil
		},
	}
	env := &jobEnv{ID: "01J0000000000000000000TEST", Job: "db-unfollow", Cluster: cfg,
		Params: json.RawMessage(`{"name":"app_follower"}`)}
	release, busy, err := takeJobLocks(context.Background(), env, locks)
	if err != nil || busy != nil {
		t.Fatalf("locks: %v %v", err, busy)
	}
	release()
	if mods["node"] != engine.LockExclusive {
		t.Errorf("the node lock is %q, and db create, config set and db drop take it exclusive", mods["node"])
	}
	if mods["cluster app_follower"] != engine.LockExclusive {
		t.Errorf("the follower's cluster lock is %q", mods["cluster app_follower"])
	}
	if len(got) != 2 || got[0] != "node" || got[1] != "cluster app_follower" {
		t.Errorf("lock order %v", got)
	}
}

// R2-4: two unfollows of the same follower can't hold its locks at once, so a
// second one can't promote and rename beside the first.
func TestUnfollowSameTargetIsExclusive(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	locks := fileJobLockers(t)
	env := func() *jobEnv {
		return &jobEnv{ID: "01J0000000000000000000TEST", Job: "db-unfollow", Cluster: cfg,
			Params: json.RawMessage(`{"name":"app_follower"}`)}
	}
	release, busy, err := takeJobLocks(context.Background(), env(), locks)
	if err != nil || busy != nil {
		t.Fatalf("the first unfollow's locks: %v %v", err, busy)
	}
	defer release()
	if _, busy, err = takeJobLocks(context.Background(), env(), locks); err != nil {
		t.Fatalf("the second unfollow's locks: %v", err)
	}
	if busy == nil {
		t.Fatal("a second unfollow of the same follower took its locks")
	}
}

// R2-4: with the node lock held exclusive, a competing desired-state writer is
// refused, and two unfollows of different followers both reach cluster.toml.
func TestUnfollowLocksSerializeTheDesiredState(t *testing.T) {
	ctx := context.Background()
	names := make([]string, 8)
	for i := range names {
		names[i] = fmt.Sprintf("app_follower_%d", i)
	}
	cfg := followerLifecycleCfg(t, names...)
	node := newFollowerNode(t, cfg)
	stub := (&followerStub{}).install(t)
	locks := fileJobLockers(t)
	var wg sync.WaitGroup
	for i := range 8 {
		name := names[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			env := unfollowEnv(cfg, name)
			// A second unfollow that finds the node lock busy is skipped and
			// retried by the operator; here the goroutine is that retry.
			var release engine.Release
			for range 5000 {
				var busy *jobLockBusy
				var err error
				if release, busy, err = takeJobLocks(ctx, env, locks); err == nil && busy == nil {
					break
				}
				if err != nil {
					t.Errorf("%s: locks: %v", name, err)
					return
				}
				time.Sleep(time.Millisecond)
			}
			if release == nil {
				t.Errorf("%s: never took its locks", name)
				return
			}
			defer release()
			// A competing writer (db create, config set and db drop take the
			// node lock exclusive) can't enter while this unfollow holds it.
			if _, err := locks.node(ctx, engine.LockExclusive, "db.create", "competing", 0); err == nil {
				t.Errorf("%s: a competing desired-state writer took the node lock", name)
			}
			if _, err := jobUnfollow(ctx, env); err != nil {
				t.Errorf("%s: unfollow: %v", name, err)
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		return
	}
	saved := node.config()
	stanzas := map[string]bool{}
	for _, name := range names {
		spec, ok := saved.ClusterByName(name)
		if !ok || spec.Follows != "" || spec.Stanza == "" {
			t.Fatalf("%s was lost or still follows: %+v", name, spec)
		}
		if stanzas[spec.Stanza] {
			t.Fatalf("two followers took the same stanza %q", spec.Stanza)
		}
		stanzas[spec.Stanza] = true
	}
	if recs := node.records().Unfollowing; len(recs) != 0 {
		t.Errorf("finished unfollows left records: %v", recs)
	}
	// The source is untouched, and every follower kept it.
	src, _ := saved.ClusterByName("app")
	if src.Follows != "" {
		t.Errorf("the source changed: %+v", src)
	}
	if !stub.reached("finish") {
		t.Error("no unfollow finished")
	}
}

// R2-4: the job reloads cluster.toml under its locks, so a change another
// command made while its process started is the one it acts on.
func TestUnfollowReloadsTheDesiredState(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	changed := node.config()
	for i := range changed.Clusters {
		if changed.Clusters[i].Name == "app_follower" {
			changed.Clusters[i].Follows = "other"
		}
	}
	changed.Clusters = append(changed.Clusters, config.ClusterSpec{Name: "other", Node: "db1", Port: 5450, Stanza: "other-abc123", StatementTimeout: "60s"})
	node.save(changed)
	stub := (&followerStub{}).install(t)

	out, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower"))
	if err != nil {
		t.Fatalf("unfollow: %v", err)
	}
	if data := out.(map[string]any); data["source"] != "other" {
		t.Errorf("the job acted on the configuration its process loaded, not the saved one: source %v", data["source"])
	}
	if stub.slotSource != "other" {
		t.Errorf("the slot was dropped on %q", stub.slotSource)
	}
	saved := node.config()
	if _, ok := saved.ClusterByName("other"); !ok {
		t.Error("the reloaded change was overwritten")
	}
}

// fileJobLockers are the node's lock semantics (flock on a file) on a
// directory of their own, so a test proves exclusivity without /run/lock. A
// shared lock conflicts with an exclusive one, and neither is recursive.
func fileJobLockers(t *testing.T) jobLockers {
	t.Helper()
	dir := t.TempDir()
	acquire := func(name string, mode engine.LockMode) (engine.Release, error) {
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: test directory.
		if err != nil {
			return nil, err
		}
		how := syscall.LOCK_EX
		if mode == engine.LockShared {
			how = syscall.LOCK_SH
		}
		if err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB); err != nil { //nolint:gosec // G115: test file descriptor.
			_ = f.Close()
			return nil, output.NewError(output.CodePrecondition, "another vaire operation holds the node lock", output.ExitPrecondition, "").
				WithDetails(map[string]any{"reason": "lock_held"})
		}
		return func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) //nolint:gosec // G115: test file descriptor.
			_ = f.Close()
		}, nil
	}
	return jobLockers{
		node: func(_ context.Context, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			return acquire("node.lock", mode)
		},
		heavyIO: func(context.Context, string, string) (engine.Release, error) {
			return acquire("heavy.lock", engine.LockExclusive)
		},
		cluster: func(_ context.Context, cluster string, mode engine.LockMode, _, _ string, _ time.Duration) (engine.Release, error) {
			return acquire("cluster-"+cluster+".lock", mode)
		},
	}
}

// installForgetJournal keeps a rollback test's desired-state transaction off
// the node's journal path.
func installForgetJournal(t *testing.T) {
	t.Helper()
	old := forgetClusterJournal
	forgetClusterJournal = func(string, string) error { return nil }
	t.Cleanup(func() { forgetClusterJournal = old })
}

// waitingNodeLock is a node lock that waits like engine.Acquire does, on a
// test's file locks, so a rollback test observes serialization rather than an
// immediate lock_held.
func waitingNodeLock(locks jobLockers) func(context.Context, string, string) (engine.Release, error) {
	return func(ctx context.Context, job, requestID string) (engine.Release, error) {
		for {
			release, err := locks.node(ctx, engine.LockExclusive, job, requestID, 0)
			if err == nil {
				return release, nil
			}
			if !engine.IsLockHeld(err) {
				return nil, err
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
}

// fakeSQL models a promoted follower's cluster for the renames: the databases
// and roles it holds. It records what renameFollower asked it to run.
type fakeSQL struct {
	databases map[string]bool
	roles     map[string]bool
	queries   []string
	executed  []string
}

func (f *fakeSQL) Query(_ context.Context, _ string, sql string, _ map[string]string) ([]map[string]string, error) {
	f.queries = append(f.queries, sql)
	column, names := "rolname", f.roles
	if strings.Contains(sql, "pg_database") {
		column, names = "datname", f.databases
	}
	var rows []map[string]string
	for name := range names {
		rows = append(rows, map[string]string{column: name})
	}
	return rows, nil
}

func (f *fakeSQL) Exec(_ context.Context, _ string, sql string, _ map[string]string) error {
	f.executed = append(f.executed, sql)
	return nil
}

func (f *fakeSQL) last() string { return strings.Join(f.executed, "\n") }

// R2-3: an unfollow that stopped after renaming the database but before all of
// its roles finishes the rest. The old inference (the source's database is
// gone, so every rename is done) failed on the old roles; the cluster's names
// decide now.
func TestRenameFollowerFinishesASubsetOfRoles(t *testing.T) {
	fake := &fakeSQL{
		databases: map[string]bool{"app_follower": true},
		roles:     map[string]bool{"app_follower_owner": true, "app_migrate": true, "app_app": true, "app_ro": true},
	}
	passwords := rolePasswords("app_follower")
	if err := renameFollower(context.Background(), fake, "app", "app_follower", passwords, nil, "db-unfollow"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	sql := fake.last()
	for _, want := range []string{
		"ALTER ROLE \"app_migrate\" RENAME TO \"app_follower_migrate\";",
		"ALTER ROLE \"app_app\" RENAME TO \"app_follower_app\";",
		"ALTER ROLE \"app_ro\" RENAME TO \"app_follower_ro\";",
		"ALTER ROLE \"app_follower_migrate\" SET role = 'app_follower_owner';",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("the finished renames lack %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "ALTER DATABASE") {
		t.Errorf("the database is renamed again:\n%s", sql)
	}
	if strings.Contains(sql, `"app_owner" RENAME`) || strings.Contains(sql, `"app_follower_owner" RENAME`) {
		t.Errorf("a role that is renamed already is renamed again:\n%s", sql)
	}
	for _, role := range steps.DatabaseRoles {
		if strings.Contains(sql, passwords[steps.SecretDatabaseRole("app_follower", role)]) {
			t.Errorf("the password of %s is in the SQL", role)
		}
		if !strings.Contains(sql, "ALTER ROLE \"app_follower_"+role+"\" PASSWORD '") {
			t.Errorf("no password for app_follower_%s:\n%s", role, sql)
		}
	}

	// Run again on the finished cluster: nothing is renamed, the settings and
	// passwords are set once more, and it still succeeds.
	fake.executed = nil
	fake.roles = map[string]bool{"app_follower_owner": true, "app_follower_migrate": true, "app_follower_app": true, "app_follower_ro": true}
	if err := renameFollower(context.Background(), fake, "app", "app_follower", passwords, nil, "db-unfollow"); err != nil {
		t.Fatalf("second rename: %v", err)
	}
	if sql := fake.last(); strings.Contains(sql, "RENAME") {
		t.Errorf("a finished rename runs again:\n%s", sql)
	}
}

// R2-3: the rename from the source's names happens once, with the fork's
// settings and passwords, when the cluster still has all of them.
func TestRenameFollowerRenamesEverythingOnce(t *testing.T) {
	fake := &fakeSQL{
		databases: map[string]bool{"app": true},
		roles:     map[string]bool{"app_owner": true, "app_migrate": true, "app_app": true, "app_ro": true},
	}
	if err := renameFollower(context.Background(), fake, "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	sql := fake.last()
	if !strings.Contains(sql, "ALTER DATABASE \"app\" RENAME TO \"app_follower\";") {
		t.Errorf("the database is not renamed:\n%s", sql)
	}
	if got := strings.Count(sql, " RENAME TO "); got != 5 {
		t.Errorf("%d renames, want the database and its four roles:\n%s", got, sql)
	}
}

// R2-3: a cluster the renames can't finish is refused with what is in the way,
// instead of half renaming it.
func TestRenameFollowerRefusesWhatItCannotFinish(t *testing.T) {
	both := &fakeSQL{
		databases: map[string]bool{"app": true, "app_follower": true},
		roles:     map[string]bool{"app_owner": true, "app_migrate": true, "app_app": true, "app_ro": true},
	}
	if err := renameFollower(context.Background(), both, "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err == nil {
		t.Error("both databases in one cluster are renamed")
	} else if !strings.Contains(err.Error(), "both app and app_follower") {
		t.Errorf("error %v", err)
	}
	if len(both.executed) != 0 {
		t.Error("a refused rename changed the cluster")
	}
	missing := &fakeSQL{
		databases: map[string]bool{"app_follower": true},
		roles:     map[string]bool{"app_follower_owner": true, "app_follower_migrate": true, "app_follower_app": true},
	}
	if err := renameFollower(context.Background(), missing, "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err == nil {
		t.Error("a missing role is ignored")
	} else if !strings.Contains(err.Error(), "app_follower_ro is missing") {
		t.Errorf("error %v", err)
	}
	if len(missing.executed) != 0 {
		t.Error("a refused rename changed the cluster")
	}
	// A role under both names is as ambiguous as the database: the unfollow
	// can't tell which one belongs to it.
	clash := &fakeSQL{
		databases: map[string]bool{"app_follower": true},
		roles: map[string]bool{"app_follower_owner": true, "app_follower_migrate": true,
			"app_follower_app": true, "app_ro": true, "app_follower_ro": true},
	}
	if err := renameFollower(context.Background(), clash, "app", "app_follower", rolePasswords("app_follower"), nil, "db-unfollow"); err == nil {
		t.Error("both app_ro and app_follower_ro in one cluster are ignored")
	} else if !strings.Contains(err.Error(), "both app_ro and app_follower_ro") {
		t.Errorf("error %v", err)
	}
	if len(clash.executed) != 0 {
		t.Error("a refused role clash changed the cluster")
	}
}

// R2-5: every failure of an unfollow that started says the new passwords may be
// on the database, so the CLI hands them over, and neither the error, the job's
// record nor the activity history carries one.
func TestUnfollowMarksFailuresThatMayHaveChangedPasswords(t *testing.T) {
	ctx := context.Background()
	passwords := rolePasswords("app_follower")
	for _, stop := range []string{"promote", "rename", "remove_conf", "restart", "finish"} {
		t.Run(stop, func(t *testing.T) {
			cfg := followerLifecycleCfg(t, "app_follower")
			newFollowerNode(t, cfg)
			(&followerStub{failAt: stop, failErr: errors.New("stopped at " + stop)}).install(t)
			_, err := jobUnfollow(ctx, unfollowEnv(cfg, "app_follower"))
			if err == nil {
				t.Fatalf("a failure at %s returned no error", stop)
			}
			var cliErr *output.CLIError
			if !errors.As(err, &cliErr) {
				t.Fatalf("not a CLI error: %v", err)
			}
			details, _ := cliErr.Details.(map[string]any)
			if marked, _ := details[UnfollowPasswordsDetail].(bool); !marked {
				t.Errorf("the failure at %s does not say the passwords may be set: %v", stop, cliErr.Details)
			}
			// The job's record is what activity history and job list keep.
			rec := finishedJobRecord(engine.JobRecord{ID: "01J0000000000000000000TEST", Job: "db-unfollow"}, engine.JobFailed, "", nil, err)
			raw, marshalErr := json.Marshal(rec)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			for _, password := range passwords {
				if strings.Contains(string(raw), password) || strings.Contains(cliErr.Error(), password) {
					t.Fatalf("the password of the failure at %s is in the job record or the error", stop)
				}
			}
		})
	}
}

// ordinaryFollowEnv is db-follow's job for name as handleDBFollow starts it for
// a retry: the prepared entry's generation travels in the job params. A retry
// never carries PreparedNew.
func ordinaryFollowEnv(cfg config.Cluster, name string) *jobEnv {
	spec, _ := cfg.ClusterByName(name)
	params, _ := json.Marshal(dbFollowParams{Source: spec.Follows, Name: name, From: followFromBaseBackup, Generation: spec.FollowGeneration})
	return &jobEnv{ID: "01J0000000000000000000TEST", Job: "db-follow", Cluster: cfg, Params: params}
}

// preparedFollowEnv is the job of the request that added a fresh entry.
func preparedFollowEnv(cfg config.Cluster, name string) *jobEnv {
	env := ordinaryFollowEnv(cfg, name)
	var p dbFollowParams
	_ = json.Unmarshal(env.Params, &p)
	p.PreparedNew = true
	env.Params, _ = json.Marshal(p)
	return env
}

// followDryRunRequest is db.follow's dry-run request, as the CLI sends it.
func followDryRunRequest(source, name string) (req rpc.Request) {
	req.Method = "db.follow"
	req.Params, _ = json.Marshal(map[string]any{"source": source, "name": name})
	req.DryRun = true
	return req
}

// installFollowAdmission replaces the two live facts a public follow checks:
// whether a cluster runs and how much space the copy needs.
func installFollowAdmission(t *testing.T, running func(config.ClusterSpec) bool) {
	t.Helper()
	oldRunning, oldSpace := clusterRunning, followSpace
	clusterRunning = running
	followSpace = func(context.Context, config.Cluster, string) ([]backup.SpaceNeed, error) { return nil, nil }
	t.Cleanup(func() { clusterRunning, followSpace = oldRunning, oldSpace })
}

// markAdoptedForTest writes a recovery's adopted marker without the build.
func markAdoptedForTest(t *testing.T, name string) {
	t.Helper()
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		aw := s.AwaitingRecovery[name]
		aw.Adopted = true
		s.AwaitingRecovery[name] = aw
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A-2/B-01: an adopted copy is not automatically safe. A reserved or extended
// slot with a restart position starts it as it is; a missing, lost, unreserved
// or restartless slot takes the proven-owned recopy only after the adopted
// marker is durably cleared; an unreadable slot fails closed; an unowned copy
// is never removed.
func TestRecoverAdoptedCopyRequiresRetainedSlot(t *testing.T) {
	for name, test := range map[string]struct {
		slot        followerSlotState
		owned       bool
		inspectErr  error
		recopy      bool
		fresh       string
		refused     bool
		jobFails    bool
		conditional bool
	}{
		"retained":        {slot: followerSlotState{Exists: true, Usable: true}, owned: true},
		"missing":         {owned: true, recopy: true, fresh: "create-slot"},
		"lost":            {slot: followerSlotState{Exists: true}, owned: true, recopy: true, fresh: "reset-slot"},
		"unreserved":      {slot: followerSlotState{Exists: true}, owned: true, recopy: true, fresh: "reset-slot"},
		"restartless":     {slot: followerSlotState{Exists: true}, owned: true, recopy: true, fresh: "reset-slot"},
		"unknown status":  {slot: followerSlotState{Exists: true}, owned: true, recopy: true, fresh: "reset-slot"},
		"unreadable":      {slot: followerSlotState{Exists: true, Usable: true}, owned: true, inspectErr: errors.New("the source cannot be read"), jobFails: true, conditional: true},
		"unowned missing": {refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			node.awaitRecovery("app_follower")
			installFollowBuildJob(t)
			adoptedAtDrop := true
			plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
			plan.onDropOwned = func(name, data string) error {
				recs, err := backup.LoadRecords()
				if err != nil {
					return err
				}
				adoptedAtDrop = recs.AwaitingRecovery[name].Adopted
				delete(node.registered, name)
				return errors.Join(os.RemoveAll(data), os.RemoveAll(node.confDir(name)))
			}
			installFollowCopy(t, plan)
			node.copyReady(cfg, "app_follower")
			markAdoptedForTest(t, "app_follower")
			if test.owned {
				if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
					t.Fatal(err)
				}
			}
			plan.slot, plan.slotLSN, plan.inspectErr = test.slot, "", test.inspectErr

			res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
			if test.refused {
				if err == nil {
					t.Fatalf("the dry run accepts an adopted copy it cannot reuse safely: %v", err)
				}
			} else {
				if err != nil {
					t.Fatalf("the dry run is refused: %v", err)
				}
				data := res.Data.(map[string]any)
				if test.conditional {
					if data["resume"] != false || data["resume_conditional"] != true || data["recopy_reason"] == nil {
						t.Fatalf("an unreadable adopted slot is not reported conditionally: %v", data)
					}
				}
				for _, effect := range []string{"drop-owned", "create-slot", "reset-slot", "drop-slot", "copy", "adopt"} {
					if plan.has(effect) {
						t.Fatalf("the dry run made a change (%s): %v", effect, plan.calls)
					}
				}
			}

			plan.calls = nil
			_, err = jobFollow(ctx, followEnv(cfg, "app_follower", true))
			if test.refused || test.jobFails {
				if err == nil {
					t.Fatal("the recovery accepted an adopted copy it cannot reuse safely")
				}
				if plan.has("drop-owned") || plan.has("create-slot") || plan.has("reset-slot") || plan.has("drop-slot") || plan.has("copy") || plan.has("adopt") {
					t.Fatalf("a refused adopted recovery touched the copy or slot: %v", plan.calls)
				}
				recs := node.records()
				if !recs.IsAwaiting("app_follower") || !recs.AwaitingRecovery["app_follower"].Adopted {
					t.Fatalf("a refused adopted recovery cleared its awaiting/adopted state: %+v", recs)
				}
				if test.owned {
					if _, ok, err := readFollowOwnership(recoveryOwner(node, cfg, "app_follower")); err != nil || !ok {
						t.Fatalf("a refused adopted recovery lost ownership: %v %v", ok, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("the recovery failed: %v", err)
			}
			if test.recopy {
				if !plan.has("drop-owned") || !plan.has(test.fresh) {
					t.Fatalf("an unusable adopted slot did not take the owned recopy: %v", plan.calls)
				}
				if !plan.before("drop-owned", test.fresh) {
					t.Fatalf("the fresh slot came before the owned copy was removed: %v", plan.calls)
				}
				if adoptedAtDrop {
					t.Fatalf("the adopted marker survived to the destructive recopy: %v", plan.calls)
				}
				if plan.count("copy") != 1 || plan.count("adopt") != 1 {
					t.Fatalf("the recopy did not copy and adopt once: %v", plan.calls)
				}
			} else {
				for _, effect := range []string{"drop-owned", "create-slot", "reset-slot", "drop-slot", "copy", "adopt"} {
					if plan.has(effect) {
						t.Fatalf("a retained adopted copy was touched (%s): %v", effect, plan.calls)
					}
				}
			}
			if recs := node.records(); recs.IsAwaiting("app_follower") {
				t.Fatal("the recovered follower still awaits recovery")
			}
			if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
				t.Fatalf("the ownership record survived the recovery: %v", err)
			}
		})
	}
}

// ADD: an ordinary follow interrupted after its entry and local paths were
// recorded is resumed by the same public command. Its completed copy is adopted
// again, its partial or unsafe copy is removed first, and only the operation's
// own paths are ever deleted.
func TestOrdinaryFollowRetryResumesOwnedProgress(t *testing.T) {
	for name, test := range map[string]struct {
		ready      bool
		phase      string
		slot       followerSlotState
		recopy     bool
		wantResume bool
		wantReason bool
	}{
		"partial copy":   {phase: followPhaseCopying, recopy: true},
		"completed copy": {ready: true, phase: followPhaseCopied, slot: followerSlotState{Exists: true, Usable: true}, wantResume: true},
		"unsafe slot":    {ready: true, phase: followPhaseCopied, recopy: true, wantReason: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
			installFollowBuildJob(t)
			plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
			installFollowCopy(t, plan)
			if test.ready {
				node.copyReady(cfg, "app_follower")
			} else if err := os.MkdirAll(cfg.Server.DataDirectory("app_follower"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), test.phase); err != nil {
				t.Fatal(err)
			}
			plan.slot, plan.slotLSN = test.slot, "0/1000"

			res, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
			if err != nil {
				t.Fatalf("the public retry is refused: %v", err)
			}
			data := res.Data.(map[string]any)
			if data["resume"] != test.wantResume {
				t.Fatalf("the public retry reports resume=%v: %v", data["resume"], data)
			}
			if _, ok := data["recopy_reason"]; ok != test.wantReason {
				t.Fatalf("the public retry reports a recopy reason %v: %v", ok, data)
			}

			plan.calls = nil
			if _, err := jobFollow(ctx, ordinaryFollowEnv(node.config(), "app_follower")); err != nil {
				t.Fatalf("the retried follow failed: %v", err)
			}
			if test.recopy {
				if !plan.has("drop-owned") || !plan.has("create-slot") || plan.count("copy") != 1 {
					t.Fatalf("the interrupted copy was not rebuilt: %v", plan.calls)
				}
				if plan.count("adopt") != 1 {
					t.Fatalf("the retried follow did not adopt once: %v", plan.calls)
				}
			} else {
				if plan.has("drop-owned") || plan.has("create-slot") || plan.has("reset-slot") || plan.has("copy") {
					t.Fatalf("the completed copy was not resumed: %v", plan.calls)
				}
				if plan.count("adopt") != 1 || plan.slotLSN != "0/1000" {
					t.Fatalf("the completed copy was not adopted in place: %v slot %q", plan.calls, plan.slotLSN)
				}
			}
			if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
				t.Fatalf("the ownership record survived the completed follow: %v", err)
			}
			if spec, _ := node.config().ClusterByName("app_follower"); !spec.IsFollower() || spec.Follows != "app" {
				t.Fatalf("the retried follow lost its entry: %+v", spec)
			}
		})
	}
}

// ADD: a public follow retry refuses a completed or replaced follower, and a
// job admitted for the old entry refuses to build after a writer replaced it.
func TestOrdinaryFollowRetryRefusesForeignOrReplacedProgress(t *testing.T) {
	ctx := context.Background()
	t.Run("foreign paths", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
		installFollowBuildJob(t)
		plan := &followCopyPlan{node: node, cfg: cfg}
		installFollowCopy(t, plan)
		node.copyReady(cfg, "app_follower")
		if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()}); err == nil || reason(t, err) != "database_exists" {
			t.Fatalf("foreign paths are not refused: %v", err)
		}
		if plan.has("drop-owned") || plan.has("copy") {
			t.Fatalf("a refused retry touched foreign paths: %v", plan.calls)
		}
	})
	t.Run("completed follower", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
		node.copyReady(cfg, "app_follower")
		if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()}); err == nil || reason(t, err) != "database_exists" {
			t.Fatalf("a completed follower is retried as unfinished: %v", err)
		}
	})
	t.Run("replaced entry", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
		installFollowBuildJob(t)
		plan := &followCopyPlan{node: node, cfg: cfg}
		installFollowCopy(t, plan)
		node.copyReady(cfg, "app_follower")
		if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
			t.Fatal(err)
		}
		changed := node.config()
		for i := range changed.Clusters {
			if changed.Clusters[i].Name == "app_follower" {
				changed.Clusters[i].Port = 5460
				changed.Clusters[i].FollowGeneration = "ffffffffffffffffffffffffffffffff"
			}
		}
		node.save(changed)
		if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: changed}); err == nil || reason(t, err) != "database_exists" {
			t.Fatalf("a replaced entry is treated as the old follow: %v", err)
		}
		plan.calls = nil
		if _, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower")); err == nil || reason(t, err) != "config_changed" {
			t.Fatalf("a job admitted for the old entry built under the new one: %v", err)
		}
		if plan.has("copy") || plan.has("drop-owned") {
			t.Fatalf("a replaced entry was built or removed: %v", plan.calls)
		}
	})
	t.Run("removed entry", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
		stub := (&followerStub{}).install(t)
		changed := node.config()
		kept := changed.Clusters[:0]
		for _, spec := range changed.Clusters {
			if spec.Name != "app_follower" {
				kept = append(kept, spec)
			}
		}
		changed.Clusters = kept
		node.save(changed)
		if _, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower")); err == nil || reason(t, err) != "not_a_follower" {
			t.Fatalf("a job admitted for a removed entry acted: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a removed target reached the build: %v", stub.stages())
		}
	})
}

// A-3: the job reloads cluster.toml and records.json after its locks and
// revalidates the target, source and awaiting record before any effect.
func TestRecoverFollowReloadsDesiredStateAfterLock(t *testing.T) {
	ctx := context.Background()
	t.Run("fresh entry accepted", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		stub := (&followerStub{}).install(t)
		changed := node.config()
		for i := range changed.Clusters {
			if changed.Clusters[i].Name == "app_follower" {
				changed.Clusters[i].Port = 5461
			}
		}
		node.save(changed)
		out, err := jobFollow(ctx, followEnv(cfg, "app_follower", true))
		if err != nil {
			t.Fatalf("the fresh entry was refused: %v", err)
		}
		if data := out.(map[string]any); data["port"] != 5461 {
			t.Fatalf("the job acted on its pre-lock entry: %v", data)
		}
		if !stub.reached("build", "start") {
			t.Fatalf("the recovery did not build and start: %v", stub.stages())
		}
	})
	t.Run("removed target", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		stub := (&followerStub{}).install(t)
		changed := node.config()
		changed.Clusters = changed.Clusters[:1] // app only
		node.save(changed)
		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || reason(t, err) != "not_a_follower" {
			t.Fatalf("a removed target was recovered: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a removed target reached the build: %v", stub.stages())
		}
	})
	t.Run("replaced source", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		stub := (&followerStub{}).install(t)
		changed := node.config()
		changed.Clusters = append(changed.Clusters, config.ClusterSpec{Name: "other", Node: "db1", Port: 5462, Stanza: "other-abc123", StatementTimeout: "60s"})
		for i := range changed.Clusters {
			if changed.Clusters[i].Name == "app_follower" {
				changed.Clusters[i].Follows = "other"
			}
		}
		node.save(changed)
		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || reason(t, err) != "config_changed" {
			t.Fatalf("a replaced source was recovered: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a replaced source reached the build: %v", stub.stages())
		}
	})
	t.Run("no longer awaiting", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		newFollowerNode(t, cfg)
		stub := (&followerStub{}).install(t)
		if err := backup.UpdateRecords(func(s *backup.Records) error { delete(s.AwaitingRecovery, "app_follower"); return nil }); err != nil {
			t.Fatal(err)
		}
		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || reason(t, err) != "not_awaiting_recovery" {
			t.Fatalf("a database no longer awaiting was recovered: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a database no longer awaiting reached the build: %v", stub.stages())
		}
	})
	t.Run("replaced awaiting generation", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		stub := (&followerStub{}).install(t)
		env := followEnv(cfg, "app_follower", true)
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			aw := s.AwaitingRecovery["app_follower"]
			aw.Since = "2026-02-02T00:00:00Z"
			s.AwaitingRecovery["app_follower"] = aw
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := jobFollow(ctx, env); err == nil || reason(t, err) != "config_changed" {
			t.Fatalf("a replaced awaiting generation was recovered: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a replaced awaiting generation reached the build: %v", stub.stages())
		}
	})
	t.Run("source awaiting", func(t *testing.T) {
		cfg := followerLifecycleCfg(t, "app_follower")
		node := newFollowerNode(t, cfg)
		node.awaitRecovery("app_follower")
		if err := backup.UpdateRecords(func(s *backup.Records) error {
			s.AwaitingRecovery["app"] = backup.Awaiting{Stanza: "app-abc123", Since: "2026-01-01T00:00:00Z"}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		stub := (&followerStub{}).install(t)
		if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil || reason(t, err) != "source_awaiting_recovery" {
			t.Fatalf("a follower of an awaiting source was recovered: %v", err)
		}
		if stub.reached("build") {
			t.Fatalf("a follower of an awaiting source reached the build: %v", stub.stages())
		}
	})
}

// FG-M4/B-02: a plain follow's rollback and a skipped job's no-lock cleanup
// serialize on the exclusive node lock. The second waits for the first, then
// removes only the entry its own operation prepared.
func TestFailedFollowsSerializeDesiredStateRollback(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower", "app_follower_2")
	node := newFollowerNode(t, cfg)
	installForgetJournal(t)
	stub := (&followerStub{failAt: "build", failErr: errors.New("the copy failed")}).install(t)
	locks := fileJobLockers(t)
	oldAcquire, oldBefore := followRollbackAcquire, followBeforeRollback
	followRollbackAcquire = waitingNodeLock(locks)
	reached := make(chan string, 1)
	releaseFirst := make(chan struct{})
	followBeforeRollback = func(name string) {
		if name == "app_follower" {
			reached <- name
			<-releaseFirst
		}
	}
	t.Cleanup(func() { followRollbackAcquire, followBeforeRollback = oldAcquire, oldBefore })

	first := make(chan error, 1)
	go func() {
		env := preparedFollowEnv(cfg, "app_follower")
		release, busy, err := takeJobLocks(ctx, env, locks)
		if err != nil || busy != nil {
			first <- errors.Join(err, errors.New("the first follow could not take its locks"))
			return
		}
		defer release()
		_, err = jobFollow(ctx, env)
		first <- err
	}()
	if got := <-reached; got != "app_follower" {
		t.Fatalf("the first rollback reached %q", got)
	}

	// A second follow cannot take the node lock while the first holds it.
	secondEnv := preparedFollowEnv(cfg, "app_follower_2")
	if release, busy, err := takeJobLocks(ctx, secondEnv, locks); err != nil || busy == nil {
		if release != nil {
			release()
		}
		t.Fatalf("the second follow took the node lock beside the first: %v %v", err, busy)
	}

	// Its skipped-job cleanup holds no lock itself: it waits, then removes
	// its own entry.
	second := make(chan error, 1)
	go func() {
		notRun(ctx, &jobEnv{Job: "db-follow", Params: secondEnv.Params, Report: func(output.Event) {}}, errors.New("lock busy"))
		second <- nil
	}()
	select {
	case <-second:
		t.Fatal("the skipped follow's rollback ran beside the first rollback")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	if err := <-first; err == nil || !strings.Contains(err.Error(), "the copy failed") {
		t.Fatalf("the first follow returned %v", err)
	}
	<-second
	saved := node.config()
	for _, name := range []string{"app_follower", "app_follower_2"} {
		if _, ok := saved.ClusterByName(name); ok {
			t.Errorf("the rolled-back entry %s is still in cluster.toml", name)
		}
	}
	if stub.reached("start") {
		t.Error("a failed build started the follower")
	}
}

// FG-M4/B-02: a skipped follow's cleanup waits for the desired-state writer
// and removes only the entry its own operation prepared, leaving a replaced
// entry alone.
func TestSkippedFollowRollbackWaitsForDesiredStateWriter(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	installForgetJournal(t)
	locks := fileJobLockers(t)
	old := followRollbackAcquire
	followRollbackAcquire = waitingNodeLock(locks)
	t.Cleanup(func() { followRollbackAcquire = old })
	release, err := locks.node(ctx, engine.LockExclusive, "config.set", "test", 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	prepared := mustSpec(t, cfg, "app_follower")
	go func() {
		done <- rollbackPreparedFollow(ctx, dbFollowParams{Source: "app", Name: "app_follower", Generation: prepared.FollowGeneration, PreparedNew: true})
	}()
	select {
	case <-done:
		t.Fatal("the rollback ran while a desired-state writer held the node lock")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); ok {
		t.Fatal("the skipped follow's entry was not removed")
	}

	// A replaced entry is not the operation's to remove.
	node.save(cfg)
	changed := node.config()
	for i := range changed.Clusters {
		if changed.Clusters[i].Name == "app_follower" {
			changed.Clusters[i].Port = 5463
			changed.Clusters[i].FollowGeneration = "ffffffffffffffffffffffffffffffff"
		}
	}
	node.save(changed)
	if err := rollbackPreparedFollow(ctx, dbFollowParams{Source: "app", Name: "app_follower", Generation: prepared.FollowGeneration, PreparedNew: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); !ok {
		t.Fatal("a replaced entry was removed by a stale rollback")
	}
}

func mustSpec(t *testing.T, cfg config.Cluster, name string) config.ClusterSpec {
	t.Helper()
	spec, ok := cfg.ClusterByName(name)
	if !ok {
		t.Fatalf("%s has no cluster.toml entry", name)
	}
	return spec
}

// applySuggestedFloorCommands executes every vaire config set command in a
// floor refusal through the real config setter on node's persisted pre-follow
// configuration, then saves the validated result.
func applySuggestedFloorCommands(t *testing.T, node *followerNode, err error) {
	t.Helper()
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a CLI error: %v", err)
	}
	commands := regexp.MustCompile(`vaire config set (clusters\.[^\s]+) (\d+)`).FindAllStringSubmatch(cliErr.Hint, -1)
	if len(commands) == 0 {
		t.Fatalf("the refusal has no executable config set prerequisite: %v", err)
	}
	set := map[string]string{}
	for _, command := range commands {
		set[command[1]] = command[2]
	}
	raw, err := newConfigBytes(node.store, configSetParams{Set: set})
	if err != nil {
		t.Fatalf("a suggested prerequisite is not executable: %v", err)
	}
	if err := node.store.Save(raw); err != nil {
		t.Fatal(err)
	}
}

// ADD: new-follow admission compares the proposed floors with the source's
// running values. Only a proven lower floor refuses; a missing fact fails
// closed; a pending restart that leaves equal or lower values is no refusal.
func TestFollowFloorMismatches(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	src, _ := cfg.ClusterByName("app")
	spec, _ := cfg.ClusterByName("app_follower")
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}
	floors, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		t.Fatal(err)
	}
	running := map[string]int{}
	for _, f := range floors {
		value, err := strconv.Atoi(f.Value)
		if err != nil {
			t.Fatal(err)
		}
		running[f.Name] = value
	}
	if got, err := followFloorMismatches(cfg, src, spec, host, running); err != nil || len(got) != 0 {
		t.Fatalf("equal floors are refused: %v %v", got, err)
	}
	// A pending increase keeps the source below its desired values: safe.
	if got, err := followFloorMismatches(cfg, src, spec, host, running); err != nil || len(got) != 0 {
		t.Fatalf("a pending increase is refused: %v %v", got, err)
	}
	running["max_connections"]--
	if got, err := followFloorMismatches(cfg, src, spec, host, running); err != nil || len(got) != 0 {
		t.Fatalf("a lower running source is refused: %v %v", got, err)
	}
	running["max_connections"] += 2
	got, err := followFloorMismatches(cfg, src, spec, host, running)
	if err != nil || len(got) != 1 || got[0].Setting != "max_connections" {
		t.Fatalf("an unsafe running floor is not reported: %+v %v", got, err)
	}
	absent := cfg
	absent.Clusters = absent.Clusters[:1]
	if _, err := followFloorMismatches(absent, src, spec, host, running); err == nil {
		t.Fatal("a config without the proposed target was accepted")
	}
	duplicate := cfg
	duplicate.Clusters = append(duplicate.Clusters, spec)
	if _, err := followFloorMismatches(duplicate, src, spec, host, running); err == nil {
		t.Fatal("a config with the proposed target twice was accepted")
	}
	delete(running, "max_locks_per_transaction")
	if _, err := followFloorMismatches(cfg, src, spec, host, running); err == nil {
		t.Fatal("a missing running floor was treated as safe")
	}
}

// ADD: db follow refuses a new follower when the source's running floors are
// above the follower's, and fails closed when they cannot be read, without
// writing cluster.toml.
func TestFollowRefusesUnsafeSourceFloors(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t)
	// The source's desired setting is below what it still runs: a deferred
	// shrink. The fresh proposal inherits the desired value, not the live one.
	cfg.Clusters[0].Settings = map[string]string{"max_connections": "30"}
	node := newFollowerNode(t, cfg)
	installFollowAdmission(t, func(config.ClusterSpec) bool { return true })
	oldFloors := followerEffectiveFloors
	t.Cleanup(func() { followerEffectiveFloors = oldFloors })

	running := map[string]int{}
	for _, name := range steps.FloorSettings {
		running[name] = 0
	}
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) {
		return running, nil
	}
	// A safe source admits the dry run.
	if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: cfg}); err != nil {
		t.Fatalf("a safe source was refused: %v", err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); ok {
		t.Fatal("a dry run wrote the follower entry")
	}

	running["max_connections"] = 100000
	_, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: cfg})
	if err == nil || reason(t, err) != "source_floors_above_follower" {
		t.Fatalf("an unsafe source was admitted: %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) || !strings.Contains(cliErr.Hint, "clusters.app.settings.max_connections 100000") || strings.Contains(cliErr.Hint, "restart") {
		t.Fatalf("the refusal does not name the source's executable prerequisite: %v", err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); ok {
		t.Fatal("the unsafe refusal wrote the follower entry")
	}

	// Execute exactly the suggested commands through the real config setter on
	// the true pre-follow file, then prove the retry is admitted.
	commands := regexp.MustCompile(`vaire config set (clusters\.[^\s]+) (\d+)`).FindAllStringSubmatch(cliErr.Hint, -1)
	if len(commands) != 1 {
		t.Fatalf("the refusal's executable prerequisites are not exact: %v", commands)
	}
	applySuggestedFloorCommands(t, node, err)
	if spec := mustSpec(t, node.config(), "app"); spec.Settings["max_connections"] != "100000" {
		t.Fatalf("the suggested setter did not persist the source setting: %+v", spec.Settings)
	}
	if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()}); err != nil {
		t.Fatalf("the suggested prerequisite did not permit admission: %v", err)
	}

	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) {
		return nil, errors.New("the source cannot be read")
	}
	if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: cfg}); err == nil || reason(t, err) != "source_floors_unknown" {
		t.Fatalf("an unknown running floor was treated as safe: %v", err)
	}
}

// C2-A3: a prepared ordinary follow with no local progress is not a cluster.
// General plan and apply leave it out (no initdb, start, timer or pooler
// exposure), count it in the tuning view, and still allow an unrelated
// database; --db names the real refusal. The owned follow job keeps the target
// in its own execution view, and the public retry remains admitted.
func TestApplyDoesNotInitialisePreparedFollower(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	cfg.Clusters = append(cfg.Clusters, config.ClusterSpec{
		Name: "other", Node: "db1", Port: 5460, Size: "small", StatementTimeout: "60s", Stanza: "other-1a2b3c",
	})
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	node := newFollowerNode(t, cfg)
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}
	rt := engine.Runtime{Host: host}
	journal := filepath.Join(t.TempDir(), "journal.json")
	only := []string{"postgres-cluster-v1", "postgres-config-v1", "pgbouncer-v1"}
	hasStep := func(plan engine.PlanData, id string) bool {
		for _, s := range plan.Steps {
			if s.ID == id {
				return true
			}
		}
		return false
	}

	plan, err := engine.Plan(ctx, steps.Catalog(), node.config(), rt, engine.Options{Only: only, JournalPath: journal})
	if err != nil {
		t.Fatalf("the general plan failed: %v", err)
	}
	for _, s := range plan.Steps {
		if s.Cluster == "app_follower" {
			t.Errorf("general plan includes %s for the prepared follower", s.ID)
		}
		if s.ID == "pgbouncer-v1" {
			for _, d := range s.Diffs {
				if strings.Contains(d.Diff, "app_follower") {
					t.Errorf("general plan's pooler diff exposes the prepared follower:\n%s", d.Diff)
				}
			}
		}
	}
	if !hasStep(plan, "postgres-cluster-v1@other") {
		t.Fatal("the unrelated database is not applicable in the general plan")
	}

	// Direct plan/apply --db names the unfinished follow instead of pretending
	// the entry does not exist.
	for name, handler := range map[string]func(context.Context, *Env) (Result, error){"plan": handlePlan, "apply": handleApply} {
		if _, err := handler(ctx, &Env{Req: request(name, map[string]any{"db": "app_follower"}), Cluster: node.config()}); err == nil || reason(t, err) != "follow_unfinished" {
			t.Errorf("%s --db accepted the prepared follower: %v", name, err)
		}
	}

	// The owned follow execution view keeps the prepared target after
	// materialisation; the same view is what startFollower passes.
	kept, err := engine.Plan(ctx, steps.Catalog(), node.config(), rt, engine.Options{Only: only, JournalPath: filepath.Join(t.TempDir(), "journal.json"), Recovering: "app_follower"})
	if err != nil {
		t.Fatalf("the owned follow plan failed: %v", err)
	}
	if !hasStep(kept, "postgres-cluster-v1@app_follower") {
		t.Fatal("the owned follow view lost its prepared target")
	}
	// The normal pooler renderer must not apply the direct fence's lifecycle
	// narrowing: the engine's owned view keeps the target, so its alias is
	// still rendered.
	keptView := engineView(node.config(), "app_follower")
	for _, step := range engine.Expand(steps.Catalog(), keptView) {
		renderer, ok := step.(engine.FileRenderer)
		if !ok || step.ID() != "pgbouncer-v1" {
			continue
		}
		routed := false
		for _, f := range renderer.RenderedFiles(keptView, rt) {
			if strings.Contains(string(f.Content), "app_follower =") {
				routed = true
			}
		}
		if !routed {
			t.Error("the normal pooler renderer dropped the owned follow target")
		}
	}

	// Apply the same production catalogue with every selected step complete:
	// it must not select an instance for the prepared follower, and must not
	// run anything for it. The unrelated database remains selectable/recorded.
	viewed := engineView(node.config(), "")
	expanded := map[string]engine.Step{}
	for _, s := range engine.Expand(steps.Catalog(), viewed) {
		expanded[s.ID()] = s
	}
	j := &engine.Journal{Schema: engine.JournalSchema, Steps: map[string]engine.JournalEntry{}}
	for _, s := range plan.Steps {
		step, ok := expanded[s.ID]
		if !ok {
			t.Fatalf("plan step %s is not in the viewed catalogue", s.ID)
		}
		inputs, err := step.Inputs(viewed, rt)
		if err != nil {
			t.Fatalf("inputs for %s: %v", s.ID, err)
		}
		j.Steps[s.ID] = engine.JournalEntry{Status: engine.StatusCompleted, InputsHash: engine.InputsHash(s.ID, inputs)}
	}
	if err := j.Save(journal); err != nil {
		t.Fatal(err)
	}
	applied, _, err := engine.Apply(ctx, steps.Catalog(), node.config(), rt, engine.Options{Only: only, JournalPath: journal})
	if err != nil {
		t.Fatalf("the general apply failed: %v", err)
	}
	for _, s := range applied.Steps {
		if s.Cluster == "app_follower" {
			t.Errorf("general apply selected %s for the prepared follower", s.ID)
		}
	}
	if applied.Applied != 0 {
		t.Errorf("general apply ran %d steps for a prepared follower with no local progress", applied.Applied)
	}

	// The public follow retry is still admitted for the untouched prepared
	// entry; it is the only operation that may build it.
	installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
	res, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
	if err != nil {
		t.Fatalf("the public follow retry is refused: %v", err)
	}
	if data := res.Data.(map[string]any); data["resume"] != false {
		t.Fatalf("the untouched prepared entry is not a fresh copy: %v", data)
	}
}

// ADD: an ordinary follow that died after adopting its copy, before the start
// completed, keeps its entry and ownership. The public retry resumes the same
// copy and does not copy or remove it again.
func TestOrdinaryFollowRetryAfterStartFailure(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
	stub := installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)
	stub.failAt, stub.failErr = "start", errors.New("the start died")

	if _, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower")); err == nil {
		t.Fatal("a failed start returned no error")
	}
	if _, ok := node.config().ClusterByName("app_follower"); !ok {
		t.Fatal("a failed start removed the prepared entry")
	}
	own, ok, err := readFollowOwnership(ordinaryOwner(node, cfg, "app_follower"))
	if err != nil || !ok || own.Phase != followPhaseCopied {
		t.Fatalf("a failed start lost the completed copy's ownership: %+v %v %v", own, ok, err)
	}
	if plan.count("copy") != 1 || plan.count("adopt") != 1 {
		t.Fatalf("the first attempt did not copy and adopt once: %v", plan.calls)
	}

	res, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
	if err != nil {
		t.Fatalf("the public retry after a failed start is refused: %v", err)
	}
	if data := res.Data.(map[string]any); data["resume"] != true {
		t.Fatalf("the retry after a failed start does not resume: %v", data)
	}
	stub.failAt, stub.failErr = "", nil
	plan.calls = nil
	if _, err := jobFollow(ctx, ordinaryFollowEnv(node.config(), "app_follower")); err != nil {
		t.Fatalf("the retried follow failed: %v", err)
	}
	if plan.count("copy") != 0 || plan.count("adopt") != 1 || plan.has("drop-owned") {
		t.Fatalf("the retry did not resume the same copy: %v", plan.calls)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the ownership record survived the retried follow: %v", err)
	}
}

// followRequest is db.follow's real (non-dry-run) request.
func followRequest(source, name, size string) (req rpc.Request) {
	req.Method = "db.follow"
	req.Params, _ = json.Marshal(map[string]any{"source": source, "name": name, "size": size})
	return req
}

// installPrepareTestLocks makes a handler test's preparation and cleanup locks
// local no-ops, so the real addDatabase/cleanup flow runs without /run/lock.
func installPrepareTestLocks(t *testing.T) {
	t.Helper()
	oldPrepare, oldRollback := followPrepareAcquire, followRollbackAcquire
	release := func() {}
	followPrepareAcquire = func(context.Context, string, time.Duration) (engine.Release, error) { return release, nil }
	followRollbackAcquire = func(context.Context, string, string) (engine.Release, error) { return release, nil }
	t.Cleanup(func() { followPrepareAcquire, followRollbackAcquire = oldPrepare, oldRollback })
}

// installDeterministicAddChecks skips the host CommitLimit check that would
// otherwise make a handler test depend on the runner's memory pressure.
func installDeterministicAddChecks(t *testing.T) {
	t.Helper()
	old := addDatabaseCheckStarts
	addDatabaseCheckStarts = func(config.Cluster, config.ClusterSpec, hostinfo.Info, int) error { return nil }
	t.Cleanup(func() { addDatabaseCheckStarts = old })
}

// installStartFailure fails the detachable job after the entry exists.
func installStartFailure(t *testing.T, inspect func(dbFollowParams)) {
	t.Helper()
	old := followStartJob
	followStartJob = func(_ context.Context, _ *Env, _ string, params any, _ map[string]string) (string, error) {
		p, _ := params.(dbFollowParams)
		if inspect != nil {
			inspect(p)
		}
		return "", errors.New("systemd-run failed")
	}
	t.Cleanup(func() { followStartJob = old })
}

// A-R2/RB-02: a fresh preparation's start failure removes only the untouched
// same-generation entry, after the generation was persisted under the lock. A
// retry never removes its entry.
func TestFreshFollowPreparationStartFailureCleanup(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t)
	node := newFollowerNode(t, cfg)
	installForgetJournal(t)
	installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
	installPrepareTestLocks(t)
	installDeterministicAddChecks(t)
	var started dbFollowParams
	entryDuringStart := config.ClusterSpec{}
	installStartFailure(t, func(p dbFollowParams) {
		started = p
		entryDuringStart, _ = node.config().ClusterByName(p.Name)
	})
	if _, err := handleDBFollow(ctx, &Env{Req: followRequest("app", "app_follower", "small"), Cluster: cfg}); err == nil {
		t.Fatal("the failed start returned no error")
	}
	if !started.PreparedNew || !configFollowGeneration(started.Generation) {
		t.Fatalf("the fresh preparation did not carry a new generation: %+v", started)
	}
	if entryDuringStart.FollowGeneration != started.Generation {
		t.Fatalf("the prepared entry did not persist its generation: %+v", entryDuringStart)
	}
	if _, ok := node.config().ClusterByName("app_follower"); ok {
		t.Fatal("an untouched fresh preparation survived its failed start")
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the fresh preparation left an ownership record: %v", err)
	}

	// The same target as a retry is never rolled back.
	cfg = followerLifecycleCfg(t, "app_follower")
	node = newFollowerNode(t, cfg)
	retry := ordinaryFollowEnv(cfg, "app_follower")
	if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()}); err != nil {
		t.Fatalf("the same-target public retry is refused: %v", err)
	}
	notRun(ctx, &jobEnv{Job: "db-follow", Params: retry.Params, Report: func(output.Event) {}}, errors.New("lock busy"))
	if _, ok := node.config().ClusterByName("app_follower"); !ok {
		t.Fatal("a retry invocation removed its existing entry")
	}
}

func configFollowGeneration(generation string) bool {
	if len(generation) != 32 {
		return false
	}
	for _, r := range generation {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

// A-R2/RB-02: fresh cleanup after the lock removes only an untouched entry of
// the same generation. Ownership, materialised paths, a running cluster,
// completion and uncertain records are preserved.
func TestFreshFollowCleanupChecksProgressAfterLock(t *testing.T) {
	for name, test := range map[string]struct {
		ownership bool
		artifact  bool
		running   bool
		completed bool
		garbage   bool
		removed   bool
	}{
		"untouched":        {removed: true},
		"ownership":        {ownership: true},
		"artifacts":        {artifact: true},
		"running":          {running: true},
		"completed":        {completed: true},
		"uncertain record": {garbage: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			installForgetJournal(t)
			installPrepareTestLocks(t)
			installDeterministicAddChecks(t)
			spec := mustSpec(t, cfg, "app_follower")
			if test.ownership {
				if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
					t.Fatal(err)
				}
			}
			if test.artifact {
				if err := os.MkdirAll(cfg.Server.DataDirectory("app_follower"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if test.running {
				clusterRunning = func(spec config.ClusterSpec) bool { return spec.Name == "app_follower" }
			}
			if test.completed {
				changed := node.config()
				for i := range changed.Clusters {
					if changed.Clusters[i].Name == "app_follower" {
						changed.Clusters[i].FollowGeneration = ""
					}
				}
				node.save(changed)
			}
			if test.garbage {
				path, err := followOwnershipPath("app_follower")
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			params := dbFollowParams{Source: "app", Name: "app_follower", Generation: spec.FollowGeneration, PreparedNew: true}
			if err := rollbackPreparedFollow(ctx, params); err != nil {
				t.Fatal(err)
			}
			_, present := node.config().ClusterByName("app_follower")
			if present == test.removed {
				t.Fatalf("entry present %v, want removed %v", present, test.removed)
			}
		})
	}
}

// A-R2/RB-02: a fresh cleanup that finds another attempt completed the entry
// before its lock release leaves the completed replacement alone.
func TestFreshFollowCleanupOvertakenByCompletion(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	installForgetJournal(t)
	installPrepareTestLocks(t)
	installDeterministicAddChecks(t)
	spec := mustSpec(t, cfg, "app_follower")
	oldHook := followBeforeRollback
	followBeforeRollback = func(name string) {
		completed := node.config()
		for i := range completed.Clusters {
			if completed.Clusters[i].Name == name {
				completed.Clusters[i].FollowGeneration = ""
			}
		}
		node.save(completed)
	}
	t.Cleanup(func() { followBeforeRollback = oldHook })
	if err := rollbackPreparedFollow(ctx, dbFollowParams{Source: "app", Name: "app_follower", Generation: spec.FollowGeneration, PreparedNew: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); !ok {
		t.Fatal("a completed replacement was removed by a stale fresh cleanup")
	}
}

// A-R3/RB-02: a reused name/node/port with a different generation is a
// replacement, not the old operation. Old ownership, a stale job and a stale
// rollback cannot claim or remove it.
func TestStaleGenerationCannotClaimReplacement(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	installForgetJournal(t)
	installPrepareTestLocks(t)
	installDeterministicAddChecks(t)
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg}
	installFollowCopy(t, plan)
	old := mustSpec(t, cfg, "app_follower")
	if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	changed := node.config()
	replacement := "ffffffffffffffffffffffffffffffff"
	for i := range changed.Clusters {
		if changed.Clusters[i].Name == "app_follower" {
			changed.Clusters[i].FollowGeneration = replacement
		}
	}
	node.save(changed)
	node.copyReady(changed, "app_follower")
	newSpec := mustSpec(t, changed, "app_follower")

	if _, owned, err := readFollowOwnership(ordinaryOwnerForSpec(newSpec)); err != nil || owned {
		t.Fatalf("old ownership claimed the replacement: %v %v", owned, err)
	}
	retry, err := followRetryable(changed, node.records(), newSpec)
	if err != nil {
		t.Fatal(err)
	}
	if retry {
		t.Fatal("a replacement with old ownership and materialised paths is retried as unfinished")
	}
	plan.calls = nil
	if _, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower")); err == nil || reason(t, err) != "config_changed" {
		t.Fatalf("a stale job acted on the replacement: %v", err)
	}
	if plan.has("copy") || plan.has("drop-owned") {
		t.Fatalf("a stale job changed the replacement: %v", plan.calls)
	}
	if err := rollbackPreparedFollow(ctx, dbFollowParams{Source: "app", Name: "app_follower", Generation: old.FollowGeneration, PreparedNew: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); !ok {
		t.Fatal("a stale rollback removed the replacement")
	}
}

// ordinaryOwnerForSpec is the owner record of one already-loaded entry.
func ordinaryOwnerForSpec(spec config.ClusterSpec) followOwner {
	return followOwner{Name: spec.Name, Source: spec.Follows, Generation: spec.FollowGeneration, Spec: spec}
}

// A-R3: successful completion saves the cleared generation before ownership is
// deleted; a failed completion save leaves the operation unfinished.
func TestOrdinaryFollowCompletionClearsGeneration(t *testing.T) {
	for name, test := range map[string]struct {
		failSave bool
	}{
		"completion saved": {},
		"save failed":      {failSave: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			installFollowAdmission(t, func(config.ClusterSpec) bool { return true })
			stub := installFollowBuildJob(t)
			plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
			installFollowCopy(t, plan)
			node.copyReady(cfg, "app_follower")
			if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
				t.Fatal(err)
			}
			spec := mustSpec(t, cfg, "app_follower")
			plan.slot, plan.slotLSN = followerSlotState{Exists: true, Usable: true}, "0/1000"
			if test.failSave {
				old := followerState
				followerState = func() state.Store { return failingSaveStore{Store: node.store} }
				t.Cleanup(func() { followerState = old })
			}
			_, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower"))
			if test.failSave {
				if err == nil || !strings.Contains(err.Error(), "save failed") {
					t.Fatalf("a failed completion save returned %v", err)
				}
				if _, ok := node.config().ClusterByName("app_follower"); !ok {
					t.Fatal("a failed completion save lost the entry")
				}
				if got := mustSpec(t, node.config(), "app_follower").FollowGeneration; got != spec.FollowGeneration {
					t.Fatalf("a failed completion save cleared the generation: %q", got)
				}
				if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); err != nil {
					t.Fatalf("a failed completion save removed ownership: %v", err)
				}
				res, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
				if err != nil {
					t.Fatalf("the running unfinished follow is refused after a failed completion save: %v", err)
				}
				if data := res.Data.(map[string]any); data["resume"] != true {
					t.Fatalf("the running unfinished follow is not resumed after a failed completion save: %v", data)
				}
				return
			}
			if err != nil {
				t.Fatalf("the completed follow failed: %v", err)
			}
			if got := mustSpec(t, node.config(), "app_follower").FollowGeneration; got != "" {
				t.Fatalf("the completed follower kept generation %q", got)
			}
			if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
				t.Fatalf("the completed follower kept ownership: %v", err)
			}
			if !stub.reached("start") {
				t.Fatalf("the completion test did not start: %v", stub.stages())
			}
		})
	}
}

type failingSaveStore struct{ state.Store }

func (failingSaveStore) Save([]byte) error { return errors.New("save failed") }

// A-R3: a successful drop retires only the matching old generation/ownership;
// a different generation's evidence survives.
func TestRetireFollowOwnershipAfterDrop(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	spec := mustSpec(t, cfg, "app_follower")
	if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	if err := retireFollowOwnershipAfterDrop("app_follower", "app", "ffffffffffffffffffffffffffffffff"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); err != nil {
		t.Fatalf("a mismatched generation's ownership was retired: %v", err)
	}
	if err := retireFollowOwnershipAfterDrop("app_follower", "app", spec.FollowGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the matching ownership survived a successful drop: %v", err)
	}

	// A recovery record is identified by the awaiting source, not the
	// prepared-entry generation.
	node.awaitRecovery("app_follower")
	if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	if err := retireFollowOwnershipAfterDrop("app_follower", "other", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); err != nil {
		t.Fatalf("a recovery record for another source was retired: %v", err)
	}
	if err := retireFollowOwnershipAfterDrop("app_follower", "app", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(followOwnershipPathForTest(t, "app_follower")); !os.IsNotExist(err) {
		t.Fatalf("the matching recovery record survived a successful drop: %v", err)
	}
}

// A-R1/RB-01: real pruning of a follower entry (empty stanza) supports initial
// copy, retained-slot continuation and adopted recopy.
func TestRecoveryAcceptsRealEmptyFollowerAwaitingStanza(t *testing.T) {
	for name, test := range map[string]struct {
		ready  bool
		phase  string
		adopt  bool
		slot   followerSlotState
		recopy bool
	}{
		"initial copy":          {},
		"retained continuation": {ready: true, phase: followPhaseCopied, slot: followerSlotState{Exists: true, Usable: true}},
		"adopted recopy":        {ready: true, phase: followPhaseCopied, adopt: true, recopy: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			raw, _, err := backup.PruneForRestore(nil, awaitingOf(cfg), time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(backup.RecordsPath, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			// The source was recovered first, as db recover --all orders it.
			if err := backup.UpdateRecords(func(s *backup.Records) error {
				delete(s.AwaitingRecovery, "app")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if aw := node.records().AwaitingRecovery["app_follower"]; aw.Stanza != "" || aw.Since == "" {
				t.Fatalf("real pruning produced %+v, want empty stanza and a since", aw)
			}
			installFollowBuildJob(t)
			plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
			installFollowCopy(t, plan)
			plan.slot, plan.slotLSN = test.slot, "0/1000"
			if test.ready {
				node.copyReady(cfg, "app_follower")
				if err := writeFollowOwnership(recoveryOwner(node, cfg, "app_follower"), test.phase); err != nil {
					t.Fatal(err)
				}
			}
			if test.adopt {
				markAdoptedForTest(t, "app_follower")
			}
			if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err != nil {
				t.Fatalf("the recovery failed: %v", err)
			}
			if test.recopy {
				if !plan.has("drop-owned") || !plan.has("create-slot") || plan.count("copy") != 1 {
					t.Fatalf("the adopted recopy did not use the real empty-stanza record: %v", plan.calls)
				}
			}
			if recs := node.records(); recs.IsAwaiting("app_follower") {
				t.Fatal("the recovered follower still awaits recovery")
			}
		})
	}
}

// A-R5/RB-05: a running target is not completed. Matching unfinished ownership
// admits a retry whether the target runs or stopped; completed or unowned
// running targets and unowned stopped targets are refused.
func TestRunningUnfinishedFollowIsRetried(t *testing.T) {
	for name, test := range map[string]struct {
		generation bool
		ownership  bool
		ready      bool
		wantResume bool
	}{
		"unfinished running": {generation: true, ownership: true, ready: true, wantResume: true},
		"completed running":  {ready: true},
		"unowned running":    {generation: true, ready: true},
		"untouched prepared": {generation: true, wantResume: false},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			installFollowAdmission(t, func(config.ClusterSpec) bool { return true })
			if !test.generation {
				changed := node.config()
				for i := range changed.Clusters {
					if changed.Clusters[i].Name == "app_follower" {
						changed.Clusters[i].FollowGeneration = ""
					}
				}
				node.save(changed)
			}
			if test.ready {
				node.copyReady(node.config(), "app_follower")
			}
			if test.ownership {
				plan := &followCopyPlan{node: node, cfg: node.config(), slot: followerSlotState{Exists: true, Usable: true}}
				installFollowCopy(t, plan)
				if err := writeFollowOwnership(ordinaryOwner(node, node.config(), "app_follower"), followPhaseCopied); err != nil {
					t.Fatal(err)
				}
			}
			res, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
			if !test.wantResume {
				if err == nil {
					t.Fatalf("the target was admitted as unfinished: %v", res.Data)
				}
				if reason(t, err) != "database_exists" {
					t.Fatalf("refusal reason %q, want database_exists", reason(t, err))
				}
				return
			}
			if err != nil {
				t.Fatalf("the running unfinished follow is refused: %v", err)
			}
			if data := res.Data.(map[string]any); data["resume"] != true {
				t.Fatalf("the running unfinished follow is not resumed: %v", data)
			}
		})
	}
}

// C2-A2: once an unfollow owns the target, an ordinary follow must not resume
// across it, whether the retained copy looks reusable or unsafe. The unfollow
// promoted the target and failed before independence/rename; the follow's
// public preview, public admission and locked job all refuse before touching
// the slot, copy, adoption or start, and never delete the prepared entry. The
// unfollow's own retry stays admitted.
func TestOrdinaryFollowCannotResumeAfterUnfollowBegins(t *testing.T) {
	for name, test := range map[string]struct {
		slot    followerSlotState
		settled bool
		ready   bool
	}{
		"retained copy": {slot: followerSlotState{Exists: true, Usable: true}, ready: true},
		"unsafe slot":   {slot: followerSlotState{Exists: true}, ready: true},
		"settled":       {slot: followerSlotState{Exists: true, Usable: true}, ready: true, settled: true},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			cfg := followerLifecycleCfg(t, "app_follower")
			node := newFollowerNode(t, cfg)
			installFollowAdmission(t, func(config.ClusterSpec) bool { return true })
			stub := (&followerStub{}).install(t)
			plan := &followCopyPlan{node: node, cfg: cfg, slot: test.slot}
			installFollowCopy(t, plan)
			if test.ready {
				node.copyReady(cfg, "app_follower")
			}
			if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
				t.Fatal(err)
			}

			// The unfollow promotes the running unfinished follower, then dies
			// after settling the policy (settle is also before the first rename)
			// and before the independent configuration.
			stub.failAt, stub.failErr = "rename", errors.New("stopped after promote")
			if _, err := jobUnfollow(ctx, unfollowEnv(node.config(), "app_follower")); err == nil {
				t.Fatal("the interrupted unfollow returned no error")
			}
			if !stub.reached("promote") {
				t.Fatalf("the unfollow stopped before promoting: %v", stub.stages())
			}
			if _, ok := node.records().Unfollowing["app_follower"]; !ok {
				t.Fatal("the interrupted unfollow left no durable record")
			}
			if test.settled {
				if err := backup.UpdateRecords(func(s *backup.Records) error {
					u := s.Unfollowing["app_follower"]
					u.Settled = true
					s.Unfollowing["app_follower"] = u
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before := node.config()
			gen := mustSpec(t, before, "app_follower").FollowGeneration

			// Public preview and admission refuse before any slot inspection or
			// copy decision.
			plan.calls = nil
			if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: before}); err == nil || reason(t, err) != "unfollow_in_progress" {
				t.Fatalf("the follow preview crossed the unfollow: %v", err)
			}
			if _, err := handleDBFollow(ctx, &Env{Req: followRequest("app", "app_follower", "small"), Cluster: before}); err == nil || reason(t, err) != "unfollow_in_progress" {
				t.Fatalf("the public follow crossed the unfollow: %v", err)
			}
			if len(plan.calls) != 0 {
				t.Fatalf("a refused public follow touched the copy or slot: %v", plan.calls)
			}

			// The locked job sees the durable record on its own reload and stops
			// before ownership, slot, copy, adoption or start effects.
			stub.calls = nil
			plan.calls = nil
			if _, err := jobFollow(ctx, ordinaryFollowEnv(before, "app_follower")); err == nil || reason(t, err) != "unfollow_in_progress" {
				t.Fatalf("the locked follow crossed the unfollow: %v", err)
			}
			if len(stub.stages()) != 0 || len(plan.calls) != 0 {
				t.Fatalf("the locked follow started effects: stub=%v plan=%v", stub.stages(), plan.calls)
			}

			// A stale fresh-preparation rollback must not delete the entry a
			// newer unfollow owns.
			clusterRunning = func(spec config.ClusterSpec) bool { return spec.Name != "app_follower" }
			if err := forgetPreparedFollow(dbFollowParams{Source: "app", Name: "app_follower", Generation: gen, PreparedNew: true}); err != nil {
				t.Fatalf("the stale rollback failed: %v", err)
			}
			after := node.config()
			if spec, ok := after.ClusterByName("app_follower"); !ok || spec.FollowGeneration != gen || spec.Follows != "app" {
				t.Fatalf("the follow entry was removed or replaced: %+v", spec)
			}
			if _, ok, err := readFollowOwnership(ordinaryOwner(node, after, "app_follower")); err != nil || !ok {
				t.Fatalf("the follow ownership was lost: %v %v", ok, err)
			}
			if _, ok := node.records().Unfollowing["app_follower"]; !ok {
				t.Fatal("the unfollow record was lost")
			}

			// The unfollow's own public retry remains admitted.
			res, err := handleDBUnfollow(ctx, &Env{Req: unfollowRequest("app_follower", true), Cluster: node.config()})
			if err != nil {
				t.Fatalf("the unfollow retry is refused: %v", err)
			}
			if data := res.Data.(map[string]any); data["resume"] != true {
				t.Fatalf("the unfollow retry does not resume: %v", data)
			}
		})
	}
}

// A-R6/RB-04: the exact proposed entry is floor-checked under the preparation
// lock. A source shrink after the preflight but before the lock is refused
// with the higher live values.
func TestFollowFloorsCheckedUnderPreparationLock(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t)
	cfg.Clusters[0].Settings = map[string]string{"max_connections": "100"}
	node := newFollowerNode(t, cfg)
	installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
	installPrepareTestLocks(t)
	installDeterministicAddChecks(t)
	installForgetJournal(t)
	running := map[string]int{}
	for _, name := range steps.FloorSettings {
		running[name] = 0
	}
	running["max_connections"] = 100
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) { return running, nil }
	oldHook := followBeforePrepareLock
	followBeforePrepareLock = func() {
		changed := node.config()
		for i := range changed.Clusters {
			if changed.Clusters[i].Name == "app" {
				changed.Clusters[i].Settings = map[string]string{"max_connections": "30"}
			}
		}
		node.save(changed)
	}
	t.Cleanup(func() { followBeforePrepareLock = oldHook })
	_, err := handleDBFollow(ctx, &Env{Req: followRequest("app", "app_follower", "small"), Cluster: cfg})
	if err == nil || reason(t, err) != "source_floors_above_follower" {
		t.Fatalf("the shrink between preflight and lock was admitted: %v", err)
	}
	if _, ok := node.config().ClusterByName("app_follower"); ok {
		t.Fatal("the unsafe locked preparation saved the follower entry")
	}
	if got := mustSpec(t, node.config(), "app").Settings["max_connections"]; got != "30" {
		t.Fatalf("the test did not retain the shrunken desired setting: %q", got)
	}
}

// RB-06: the proposed membership itself lowers CPU-derived floors. Restarting
// an unchanged one-cluster source cannot resolve that mismatch; the refusal
// reports actual/proposed values and the supported explicit-setting remedy.
func TestMembershipFloorMismatchNeedsExplicitSettings(t *testing.T) {
	ctx := context.Background()
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}
	cfg := followerLifecycleCfg(t)
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "app_follower", "")
	oneCluster, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		t.Fatal(err)
	}
	running := map[string]int{}
	for _, f := range oneCluster {
		value, err := strconv.Atoi(f.Value)
		if err != nil {
			t.Fatal(err)
		}
		running[f.Name] = value
	}
	twoClusters := cfg
	twoClusters.Clusters = append(twoClusters.Clusters, spec)
	mismatches, err := followFloorMismatches(twoClusters, src, spec, host, running)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range mismatches {
		if m.Setting == "max_worker_processes" && m.Source == 12 && m.Follower == 10 {
			found = true
		}
	}
	if !found {
		t.Fatalf("the membership-derived mismatch is not reported: %+v", mismatches)
	}
	// Restarting the source with its unchanged one-cluster configuration gives
	// the same live floor, so a restart-only remedy would loop.
	again, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		t.Fatal(err)
	}
	for i := range again {
		if again[i].Name == "max_worker_processes" && again[i].Value != "12" {
			t.Fatalf("the unchanged one-cluster source derives %s, want 12", again[i].Value)
		}
	}
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) { return running, nil }
	err = refuseUnsafeFollowFloors(ctx, twoClusters, src, spec, host, true)
	if err == nil || reason(t, err) != "source_floors_above_follower" {
		t.Fatalf("the membership mismatch is admitted: %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a CLI error: %v", err)
	}
	if !strings.Contains(cliErr.Hint, "clusters.app_follower.settings.max_worker_processes 12") {
		t.Fatalf("the remedy does not name the supported explicit setting: %q", cliErr.Hint)
	}
	if strings.Contains(cliErr.Hint, "restart") || strings.Contains(cliErr.Message, "restart was deferred") {
		t.Fatalf("the refusal asserts a restart remedy that cannot resolve the membership mismatch: %q / %q", cliErr.Message, cliErr.Hint)
	}
}

// C2-A4/B2-02: a fresh follow is refused before its entry exists, so its
// remedy must target the existing source. The proposed two-cluster membership
// lowers max_worker_processes from 12 to 10 on this four-vCPU host; setting
// the source's explicit value to the running 12 is executable on the persisted
// pre-follow file and makes the next admission safe. No restart is claimed.
func TestFreshMembershipFloorMismatchSuggestsSourceSetting(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t)
	node := newFollowerNode(t, cfg)
	installFollowAdmission(t, func(spec config.ClusterSpec) bool { return !spec.IsFollower() })
	host := hostinfo.Info{VCPU: 4, RAMMiB: 8192, DataDisk: hostinfo.Disk{SizeGiB: 100}}
	oldDetect := detectHost
	detectHost = func(string) (hostinfo.Info, error) { return host, nil }
	t.Cleanup(func() { detectHost = oldDetect })
	src, _ := cfg.ClusterByName("app")
	spec := followerSpec(src, "app_follower", "")
	oneCluster, err := steps.FollowerFloors(cfg, spec, src, host)
	if err != nil {
		t.Fatal(err)
	}
	running := map[string]int{}
	for _, f := range oneCluster {
		value, err := strconv.Atoi(f.Value)
		if err != nil {
			t.Fatal(err)
		}
		running[f.Name] = value
	}
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) { return running, nil }

	_, err = handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()})
	if err == nil || reason(t, err) != "source_floors_above_follower" {
		t.Fatalf("the membership mismatch was admitted: %v", err)
	}
	var cliErr *output.CLIError
	if !errors.As(err, &cliErr) {
		t.Fatalf("not a CLI error: %v", err)
	}
	if !strings.Contains(cliErr.Hint, "clusters.app.settings.max_worker_processes 12") {
		t.Fatalf("the fresh remedy does not target the existing source: %q", cliErr.Hint)
	}
	if strings.Contains(cliErr.Hint, "clusters.app_follower.") || strings.Contains(cliErr.Hint, "restart") {
		t.Fatalf("the fresh remedy names the absent follower or an unproven restart: %q", cliErr.Hint)
	}
	applySuggestedFloorCommands(t, node, err)
	if spec := mustSpec(t, node.config(), "app"); spec.Settings["max_worker_processes"] != "12" {
		t.Fatalf("the suggested source setting was not persisted: %+v", spec.Settings)
	}
	if _, err := handleDBFollow(ctx, &Env{Req: followDryRunRequest("app", "app_follower"), Cluster: node.config()}); err != nil {
		t.Fatalf("the source prerequisite did not permit admission: %v", err)
	}
}

// A-R6/RB-04: a retried follow re-reads the running source facts inside its
// locked job before ownership, slot, copy or start effects.
func TestFollowRetryFloorCheckUsesCurrentFacts(t *testing.T) {
	ctx := context.Background()
	cfg := followerLifecycleCfg(t, "app_follower")
	node := newFollowerNode(t, cfg)
	installFollowBuildJob(t)
	plan := &followCopyPlan{node: node, cfg: cfg, onRestore: func() { node.copyReady(cfg, "app_follower") }}
	installFollowCopy(t, plan)
	if err := writeFollowOwnership(ordinaryOwner(node, cfg, "app_follower"), followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	running := map[string]int{}
	for _, name := range steps.FloorSettings {
		running[name] = 0
	}
	running["max_connections"] = 100000
	followerEffectiveFloors = func(context.Context, config.ClusterSpec) (map[string]int, error) { return running, nil }
	_, err := jobFollow(ctx, ordinaryFollowEnv(cfg, "app_follower"))
	if err == nil || reason(t, err) != "source_floors_above_follower" {
		t.Fatalf("the retry did not check current floor facts: %v", err)
	}
	if plan.has("copy") || plan.has("drop-owned") || plan.has("inspect-slot") || plan.has("adopt") {
		t.Fatalf("a refused retry touched the copy or slot: %v", plan.calls)
	}
	if _, ok, err := readFollowOwnership(ordinaryOwner(node, cfg, "app_follower")); err != nil || !ok {
		t.Fatalf("a refused retry lost ownership: %v %v", ok, err)
	}
	if spec, _ := node.config().ClusterByName("app_follower"); spec.FollowGeneration == "" {
		t.Fatal("a refused retry cleared the generation")
	}
}
