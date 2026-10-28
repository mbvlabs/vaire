package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/vaire/internal/backup"
	"github.com/mbvlabs/vaire/internal/cli/output"
	"github.com/mbvlabs/vaire/internal/config"
	"github.com/mbvlabs/vaire/internal/engine"
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
	oldConfDir := followConfDirectory
	backup.RecordsPath = filepath.Join(dir, "records.json")
	followerState = func() state.Store { return n.store }
	clusterRunning = func(config.ClusterSpec) bool { return true }
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
	t.Cleanup(func() {
		backup.RecordsPath, followerState, followerServerOps, clusterRunning = oldRecords, oldState, oldServer, oldRunning
		followHostInstance, followRegisteredData, followConfRegistered = oldInstance, oldRegistered, oldConf
		followConfDirectory = oldConfDir
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

func (n *followerNode) records() backup.Records {
	n.t.Helper()
	recs, err := backup.LoadRecords()
	if err != nil {
		n.t.Fatal(err)
	}
	return recs
}

// awaitRecovery records db as awaiting recovery, as server init --restore's
// placement does.
func (n *followerNode) awaitRecovery(db string) {
	n.t.Helper()
	if err := backup.UpdateRecords(func(s *backup.Records) error {
		s.AwaitingRecovery[db] = backup.Awaiting{Stanza: db + "-abc123", Since: "2026-01-01T00:00:00Z"}
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
	t.Cleanup(func() { followerServerOps = old })
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
	params, _ := json.Marshal(dbFollowParams{Source: "app", Name: name, From: followFromBaseBackup, Recover: recover})
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
		if aw.Stanza != "app_follower-abc123" {
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

// R2-1: a recover continuation owns local paths, so its job takes the node and
// both clusters' locks exclusively; a plain follow keeps the shared locks.
func TestRecoverFollowTakesExclusiveLocks(t *testing.T) {
	cfg := followerLifecycleCfg(t, "app_follower")
	if got := jobLockMode(followEnv(cfg, "app_follower", true)); got != engine.LockExclusive {
		t.Fatalf("a recover follow takes %v", got)
	}
	if got := jobLockMode(followEnv(cfg, "app_follower", false)); got != engine.LockShared {
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
	if err := writeFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"], followPhaseCopying); err != nil {
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
	if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopied {
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
	if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopied {
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
			if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopied {
				t.Fatalf("the preview changed the ownership: %+v %v %v", own, ok, err)
			}
			if !node.records().IsAwaiting("app_follower") {
				t.Fatal("the preview cleared the awaiting entry")
			}
		})
	}

	// An adopted copy is started as it is: even a missing slot is no recopy.
	if err := markFollowerAdopted("app_follower"); err != nil {
		t.Fatal(err)
	}
	plan.slot, plan.slotLSN, plan.inspectErr = followerSlotState{}, "", nil
	plan.calls = nil
	res, err := handleDBRecover(ctx, &Env{Req: recoverDryRun("app_follower"), Cluster: cfg})
	if err != nil {
		t.Fatalf("the adopted dry run is refused: %v", err)
	}
	if data := res.Data.(map[string]any); data["resume"] != true {
		t.Fatalf("an adopted copy is not resumed: %v", data)
	}
	if plan.has("inspect-slot") {
		t.Fatalf("an adopted copy read the slot: %v", plan.calls)
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
	if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopied {
		t.Fatalf("the failed cleanup lost the completed copy's ownership: %+v %v %v", own, ok, err)
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
	if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopied {
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
	if err := writeFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"], followPhaseCopying); err != nil {
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
	if own, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok || own.Phase != followPhaseCopying {
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
	if err := writeFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"], followPhaseCopying); err != nil {
		t.Fatal(err)
	}
	// The cleanup removes the data it owns and reports success while the
	// configuration directory survives.
	plan.onDropOwned = func(_, data string) error { return os.RemoveAll(data) }
	if _, err := jobFollow(ctx, followEnv(cfg, "app_follower", true)); err == nil {
		t.Fatal("the constructor failure returned no error")
	}
	if _, ok, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || !ok {
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
		if recs := node.records(); !recs.IsAwaiting("app_follower") || recs.AwaitingRecovery["app_follower"].Stanza != "app_follower-abc123" {
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

	if err := writeFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"], followPhaseCopied); err != nil {
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
	awaiting := node.records().AwaitingRecovery["app_follower"]
	if err := writeFollowOwnership("app_follower", "app", awaiting, followPhaseCopied); err != nil {
		t.Fatal(err)
	}
	if _, owned, err := readFollowOwnership("app_follower", "app", awaiting); err != nil || !owned {
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
	if _, owned, err := readFollowOwnership("app_follower", "app", node.records().AwaitingRecovery["app_follower"]); err != nil || owned {
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
